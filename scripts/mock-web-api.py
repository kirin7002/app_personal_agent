#!/usr/bin/env python3
"""Standalone zero-dependency mock server implementing the frozen Web Demo SSE
contract (doc/design/web-demo-interface-design.md) for frontend development and
automated verification of W04-W06 before/alongside Go backend integration.

Usage:
    python3 scripts/mock-web-api.py --port 18088 --sim-mode accepted_only
"""

from __future__ import annotations

import argparse
import datetime
import http.cookies
import json
import mimetypes
import pathlib
import queue
import threading
import time
import urllib.parse
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

REPO_ROOT = pathlib.Path(__file__).resolve().parent.parent
WEB_DIR = REPO_ROOT / "cmd" / "server" / "web"

CAPABILITIES = [
    {
        "device": "window",
        "tool": "close",
        "mcp_tool": "dispatch_vehicle_command",
        "description": "关闭指定位置的车窗",
        "aliases": ["关窗", "关闭车窗", "升窗", "左前窗", "右前窗", "左后窗", "右后窗"],
        "utterances": ["把车窗关一下", "把左后窗关上"],
        "params_schema": {
            "type": "object",
            "properties": {
                "position": {"type": "string", "enum": ["front_left", "front_right", "rear_left", "rear_right"]}
            },
            "required": ["position"],
            "additionalProperties": False,
        },
    },
    {
        "device": "window",
        "tool": "open",
        "mcp_tool": "dispatch_vehicle_command",
        "description": "打开指定位置的车窗",
        "aliases": ["开窗", "打开车窗", "降窗"],
        "utterances": ["把左后窗打开"],
        "params_schema": {
            "type": "object",
            "properties": {
                "position": {"type": "string", "enum": ["front_left", "front_right", "rear_left", "rear_right"]}
            },
            "required": ["position"],
            "additionalProperties": False,
        },
    },
    {
        "device": "window",
        "tool": "get_status",
        "mcp_tool": "get_vehicle_status",
        "description": "查询一扇或多扇车窗的开关状态",
        "aliases": ["查车窗", "车窗状态", "车窗关了吗"],
        "utterances": ["左后窗关了吗"],
        "params_schema": {
            "type": "object",
            "properties": {
                "positions": {
                    "type": "array",
                    "items": {"type": "string", "enum": ["front_left", "front_right", "rear_left", "rear_right"]},
                    "minItems": 1,
                }
            },
            "required": ["positions"],
            "additionalProperties": False,
        },
    },
    {
        "device": "hvac",
        "tool": "set_climate",
        "mcp_tool": "dispatch_vehicle_command",
        "description": "同时控制空调开关状态与目标温度",
        "aliases": ["空调", "开空调", "关空调", "制冷", "制热"],
        "utterances": ["打开空调调到24度"],
        "params_schema": {
            "type": "object",
            "properties": {
                "power": {"type": "string", "enum": ["on", "off"]},
                "temperature_c": {"type": "number", "minimum": 16, "maximum": 32},
            },
            "required": ["power", "temperature_c"],
            "additionalProperties": False,
        },
    },
]


def utc_now() -> str:
    return datetime.datetime.now(datetime.timezone.utc).isoformat().replace("+00:00", "Z")


class MockState:
    def __init__(self, sim_mode: str = "auto_complete", demo_token: str = "demo-token"):
        self.sim_mode = sim_mode
        self.demo_token = demo_token
        self.lock = threading.RLock()
        self.seq = 0
        self.ring: list[tuple[int, str, dict]] = []
        self.subscribers: list[queue.Queue] = []
        now = utc_now()
        self.telemetry = {
            "window.front_left": {"Value": "closed", "SampledAt": now, "Available": True},
            "window.front_right": {"Value": "closed", "SampledAt": now, "Available": True},
            "window.rear_left": {"Value": "open", "SampledAt": now, "Available": True},
            "window.rear_right": {"Value": "closed", "SampledAt": now, "Available": True},
            "hvac.power": {"Value": "off", "SampledAt": now, "Available": True},
            "hvac.temperature_c": {"Value": 24, "SampledAt": now, "Available": True},
        }
        self.pending_tasks: list[dict] = []
        self.active_commands: list[dict] = []
        self.vehicle_results: list[dict] = []
        self.messages: list[dict] = []
        self.process_steps: list[dict] = []
        self.sessions: dict[str, dict] = {}
        self.cmd_seq = 0

    def publish(self, event_name: str, payload: dict) -> dict:
        with self.lock:
            self.seq += 1
            entry = dict(payload)
            entry["seq"] = self.seq
            entry.setdefault("ts", utc_now())
            self.ring.append((self.seq, event_name, entry))
            if len(self.ring) > 500:
                self.ring = self.ring[-500:]
            if event_name == "process.step":
                self.process_steps.append(entry)
                if len(self.process_steps) > 200:
                    self.process_steps = self.process_steps[-200:]
            elif event_name == "message.upsert":
                self.messages.append(entry.get("message", entry))
                if len(self.messages) > 100:
                    self.messages = self.messages[-100:]
            for q in list(self.subscribers):
                try:
                    q.put_nowait((self.seq, event_name, entry))
                except queue.Full:
                    pass
            return entry

    def replay_since(self, after: int) -> tuple[bool, list[tuple[int, str, dict]]]:
        with self.lock:
            if after > 0 and self.ring and after < self.ring[0][0] - 1:
                return True, []
            return False, [item for item in self.ring if item[0] > after]

    def apply_receipt(self, command_id: str, status: str, reason: str = "") -> dict | None:
        with self.lock:
            cmd = next((c for c in self.active_commands if c["command_id"] == command_id), None)
            if not cmd:
                return None
            self.active_commands = [c for c in self.active_commands if c["command_id"] != command_id]
            now = utc_now()
            if status == "completed":
                if cmd["device"] == "window":
                    pos = cmd["params"].get("position", "rear_left")
                    self.telemetry[f"window.{pos}"] = {
                        "Value": "closed" if cmd["tool"] == "close" else "open",
                        "SampledAt": now,
                        "Available": True,
                    }
                elif cmd["device"] == "hvac":
                    if "power" in cmd["params"]:
                        self.telemetry["hvac.power"] = {"Value": cmd["params"]["power"], "SampledAt": now, "Available": True}
                    if "temperature_c" in cmd["params"]:
                        self.telemetry["hvac.temperature_c"] = {"Value": cmd["params"]["temperature_c"], "SampledAt": now, "Available": True}
            event = {
                "scope": cmd["scope"],
                "device": cmd["device"],
                "tool": cmd["tool"],
                "event_id": f"{command_id}:{status}",
                "request_id": cmd["request_id"],
                "task_id": cmd["task_id"],
                "command_id": command_id,
                "status": status,
                "occurred_at": now,
            }
            self.vehicle_results.insert(0, event)
        self.publish("process.step", {
            "scope": cmd["scope"],
            "turn_id": cmd["turn_id"],
            "request_id": cmd["request_id"],
            "task_id": cmd["task_id"],
            "command_id": command_id,
            "channel": "executor",
            "step": {
                "step_id": f"executor:receipt:{command_id}",
                "stage": "executor.receipt",
                "title": "车辆执行回执确认",
                "status": "done" if status == "completed" else "error",
                "started_at": now,
                "finished_at": now,
                "duration_ms": 18,
                "duration_kind": "measured_local",
                "detail": f"回执状态：{status}" + (f" ({reason})" if reason else ""),
                "observation": {"action": "handle_receipt", "output": event},
            },
        })
        self.publish("telemetry.updated", {"scope": cmd["scope"], "telemetry": self.telemetry})
        self.publish("task.updated", {"scope": cmd["scope"], "pending_tasks": self.pending_tasks, "active_commands": self.active_commands})
        self.publish("vehicle.result", event)
        return event


def make_handler(store: MockState):
    class Handler(BaseHTTPRequestHandler):
        protocol_version = "HTTP/1.1"

        def log_message(self, fmt, *args):
            pass

        def _scope(self) -> dict:
            cookie_hdr = self.headers.get("Cookie", "")
            c = http.cookies.SimpleCookie()
            c.load(cookie_hdr)
            if "vehicle_demo_session" in c:
                sid = c["vehicle_demo_session"].value
                if sid in store.sessions:
                    return store.sessions[sid]
            return {"user_id": "demo-user", "session_id": "web-demo", "vehicle_id": "sim-car"}

        def _send_json(self, code: int, obj: dict, headers: dict | None = None):
            raw = json.dumps(obj, ensure_ascii=False).encode("utf-8")
            self.send_response(code)
            self.send_header("Content-Type", "application/json; charset=utf-8")
            self.send_header("Content-Length", str(len(raw)))
            if headers:
                for k, v in headers.items():
                    self.send_header(k, v)
            self.end_headers()
            self.wfile.write(raw)

        def _read_json(self) -> dict:
            length = int(self.headers.get("Content-Length", "0") or "0")
            if length <= 0:
                return {}
            return json.loads(self.rfile.read(length).decode("utf-8"))

        def do_GET(self):
            parsed = urllib.parse.urlparse(self.path)
            path = parsed.path
            qs = urllib.parse.parse_qs(parsed.query)

            if path == "/" or path == "/index.html":
                self._serve_file(WEB_DIR / "index.html")
                return
            if path.startswith("/static/"):
                rel = path[len("/static/"):]
                target = (WEB_DIR / rel).resolve()
                if WEB_DIR.resolve() in target.parents and target.is_file():
                    self._serve_file(target)
                    return
                self.send_error(404)
                return
            if path == "/healthz":
                self._send_json(200, {"status": "ok", "storage": "mock", "sim_mode": store.sim_mode})
                return
            if path == "/api/config":
                self._send_json(200, {
                    "storage": "sqlite (mock)",
                    "hermes_url": "http://127.0.0.1:8642",
                    "hermes_ready": True,
                    "voice_enabled": False,
                    "tts_backend": "tone",
                    "sim_mode": store.sim_mode,
                    "debug_enabled": True,
                    "default_vehicle": {
                        "vehicle_id": "sim-car",
                        "model": "EV-SUV-2026",
                        "manual_version": "2026.1",
                        "min_temp_c": 16,
                        "max_temp_c": 32,
                    },
                    "suggestions": ["把车窗关一下", "打开空调调到24度", "左后窗关了吗", "儿童锁怎么用"],
                })
                return
            if path == "/api/capabilities":
                self._send_json(200, {"capabilities": CAPABILITIES})
                return
            if path == "/api/state":
                with store.lock:
                    self._send_json(200, {
                        "cursor": store.seq,
                        "scope": self._scope(),
                        "telemetry": store.telemetry,
                        "pending_tasks": store.pending_tasks,
                        "active_commands": store.active_commands,
                        "vehicle_results": store.vehicle_results,
                        "messages": store.messages,
                        "process_steps": store.process_steps,
                    })
                return
            if path == "/api/events/stream":
                self._serve_sse(qs)
                return
            self.send_error(404)

        def do_POST(self):
            parsed = urllib.parse.urlparse(self.path)
            path = parsed.path
            if path == "/api/session":
                body = self._read_json()
                token = body.get("token", "")
                if token and token != store.demo_token:
                    self._send_json(401, {"message": "invalid demo token"})
                    return
                scope = {
                    "user_id": "demo-user",
                    "session_id": body.get("session_id") or "web-demo",
                    "vehicle_id": body.get("vehicle_id") or "sim-car",
                }
                cookie_id = f"sess-{int(time.time()*1000)}"
                store.sessions[cookie_id] = scope
                self._send_json(
                    200,
                    {**scope, "cursor": store.seq},
                    headers={"Set-Cookie": f"vehicle_demo_session={cookie_id}; Path=/; HttpOnly; SameSite=Strict"},
                )
                return
            if path == "/voice/utterances":
                self._handle_utterance()
                return
            if path == "/debug/receipt":
                body = self._read_json()
                ev = store.apply_receipt(body.get("command_id", ""), body.get("status", "completed"), body.get("reason", ""))
                if not ev:
                    self._send_json(404, {"message": "command not found"})
                    return
                self._send_json(200, {"ok": True, "command_id": ev["command_id"], "status": ev["status"]})
                return
            self.send_error(404)

        def _serve_file(self, file_path: pathlib.Path):
            data = file_path.read_bytes()
            ctype, _ = mimetypes.guess_type(str(file_path))
            if file_path.suffix == ".js":
                ctype = "application/javascript; charset=utf-8"
            elif file_path.suffix == ".css":
                ctype = "text/css; charset=utf-8"
            elif file_path.suffix == ".html":
                ctype = "text/html; charset=utf-8"
            self.send_response(200)
            self.send_header("Content-Type", ctype or "application/octet-stream")
            self.send_header("Content-Length", str(len(data)))
            self.end_headers()
            self.wfile.write(data)

        def _serve_sse(self, qs):
            after = int(self.headers.get("Last-Event-ID") or (qs.get("after", ["0"])[0]) or "0")
            self.close_connection = True
            self.send_response(200)
            self.send_header("Content-Type", "text/event-stream; charset=utf-8")
            self.send_header("Cache-Control", "no-store")
            self.send_header("Connection", "close")
            self.send_header("X-Accel-Buffering", "no")
            self.end_headers()

            q: queue.Queue = queue.Queue(maxsize=200)
            with store.lock:
                store.subscribers.append(q)
            try:
                reset, backlog = store.replay_since(after)
                if reset:
                    self._write_sse(store.seq, "stream.reset", {"cursor": store.seq})
                else:
                    for seq, ev_name, payload in backlog:
                        self._write_sse(seq, ev_name, payload)
                while True:
                    try:
                        seq, ev_name, payload = q.get(timeout=15)
                        self._write_sse(seq, ev_name, payload)
                    except queue.Empty:
                        self._write_sse(None, "ping", {"ts": utc_now()})
            except (BrokenPipeError, ConnectionResetError):
                pass
            finally:
                with store.lock:
                    if q in store.subscribers:
                        store.subscribers.remove(q)

        def _write_sse(self, event_id, event_name: str, payload: dict):
            lines = []
            if event_id is not None:
                lines.append(f"id: {event_id}")
            lines.append(f"event: {event_name}")
            lines.append("data: " + json.dumps(payload, ensure_ascii=False))
            chunk = ("\n".join(lines) + "\n\n").encode("utf-8")
            self.wfile.write(chunk)
            self.wfile.flush()

        def _handle_utterance(self):
            body = self._read_json()
            text = (body.get("text") or "").strip()
            req_id = body.get("request_id") or f"req-{int(time.time()*1000)}"
            scope = self._scope()
            scope = {**scope, "session_id": body.get("session_id") or scope["session_id"], "vehicle_id": body.get("vehicle_id") or scope["vehicle_id"]}
            turn_id = f"text-{req_id}"
            run_id = f"run_{int(time.time()*1000)}"

            self.close_connection = True
            self.send_response(200)
            self.send_header("Content-Type", "text/event-stream; charset=utf-8")
            self.send_header("Cache-Control", "no-store")
            self.send_header("Connection", "close")
            self.send_header("X-Accel-Buffering", "no")
            self.end_headers()

            def emit_step(step_dict: dict, task_id="", command_id=""):
                env = {
                    "scope": scope,
                    "turn_id": turn_id,
                    "request_id": req_id,
                    "run_id": run_id,
                    "task_id": task_id,
                    "command_id": command_id,
                    "channel": "text",
                    "step": step_dict,
                }
                published = store.publish("process.step", env)
                self._write_sse(published["seq"], "process.step", published)

            t0 = utc_now()
            emit_step({
                "step_id": "catalog:recall",
                "stage": "catalog.recall",
                "title": "BM25 能力预筛与待办加载",
                "status": "done",
                "started_at": t0,
                "finished_at": utc_now(),
                "duration_ms": 1,
                "duration_kind": "measured_local",
                "detail": f"召回候选：window.close, hvac.set_climate · 待办 {len(store.pending_tasks)} 条",
                "observation": {"action": "bm25_recall", "output": {"candidate_tools": ["window.close", "window.get_status"], "pending_count": len(store.pending_tasks)}},
            })

            t1 = utc_now()
            emit_step({
                "step_id": "hermes:run",
                "stage": "hermes.run",
                "title": "Hermes 意图理解与编排",
                "status": "running",
                "started_at": t1,
                "duration_kind": "boundary_wait",
                "detail": f"已提交 {run_id}",
            })
            time.sleep(0.12)

            reply: dict
            if "关" in text and "窗" in text and not any(k in text for k in ("左前", "右前", "左后", "右后")):
                task_id = f"T{int(time.time()*1000)%1000000:06d}"
                expires = (datetime.datetime.now(datetime.timezone.utc) + datetime.timedelta(minutes=10)).isoformat().replace("+00:00", "Z")
                pending = {
                    "task_id": task_id,
                    "version": 1,
                    "device": "window",
                    "tool": "close",
                    "known_params": {},
                    "missing_fields": ["position"],
                    "question": "要关哪扇车窗？",
                    "created_at": utc_now(),
                    "expires_at": expires,
                }
                with store.lock:
                    store.pending_tasks.insert(0, pending)
                emit_step({
                    "step_id": "mcp:create_vehicle_task",
                    "stage": "mcp.tool",
                    "title": "调用车控工具 create_vehicle_task",
                    "status": "done",
                    "started_at": utc_now(),
                    "finished_at": utc_now(),
                    "duration_ms": 14,
                    "duration_kind": "measured_tool",
                    "detail": "window.close 缺少必填参数 position，进入 pending_reply",
                    "observation": {"action": "create_vehicle_task", "input": {"device": "window", "tool": "close", "params": {}}, "output": pending},
                }, task_id=task_id)
                store.publish("task.updated", {"scope": scope, "pending_tasks": store.pending_tasks, "active_commands": store.active_commands})
                reply = {"voice_turn_id": turn_id, "request_id": req_id, "run_id": run_id, "task_id": task_id, "status": "pending_reply", "text": "要关哪扇车窗？"}
            elif any(k in text for k in ("左后", "左前", "右后", "右前")) and store.pending_tasks:
                pos = "rear_left" if "左后" in text else "front_left" if "左前" in text else "rear_right" if "右后" in text else "front_right"
                with store.lock:
                    task = store.pending_tasks.pop(0)
                    store.cmd_seq += 1
                    cmd_id = f"C{store.cmd_seq:08d}"
                    cmd = {
                        "command_id": cmd_id,
                        "downstream_id": f"VEH-{store.cmd_seq:04d}",
                        "task_id": task["task_id"],
                        "request_id": req_id,
                        "turn_id": turn_id,
                        "scope": scope,
                        "device": task["device"],
                        "tool": task["tool"],
                        "params": {"position": pos},
                        "status": "executing",
                    }
                    store.active_commands.insert(0, cmd)
                emit_step({
                    "step_id": "mcp:dispatch_vehicle_command",
                    "stage": "mcp.tool",
                    "title": "调用车控工具 dispatch_vehicle_command",
                    "status": "done",
                    "started_at": utc_now(),
                    "finished_at": utc_now(),
                    "duration_ms": 19,
                    "duration_kind": "measured_tool",
                    "detail": f"续办 {task['task_id']}，已登记命令 {cmd_id}",
                    "observation": {"action": "dispatch_vehicle_command", "input": {"task_id": task["task_id"], "device": "window", "tool": "close", "params": {"position": pos}}, "output": {"command_id": cmd_id, "status": "pending_dispatch"}},
                }, task_id=task["task_id"], command_id=cmd_id)
                emit_step({
                    "step_id": f"executor:dispatch:{cmd_id}",
                    "stage": "executor.dispatch",
                    "title": "车辆指令下发（Outbox Worker）",
                    "status": "done",
                    "started_at": utc_now(),
                    "finished_at": utc_now(),
                    "duration_ms": 22,
                    "duration_kind": "boundary_wait",
                    "detail": f"模拟通道已受理 ({cmd['downstream_id']})",
                    "observation": {"action": "send_command", "output": {"command_id": cmd_id, "downstream_id": cmd["downstream_id"], "status": "executing"}},
                }, task_id=task["task_id"], command_id=cmd_id)
                store.publish("task.updated", {"scope": scope, "pending_tasks": store.pending_tasks, "active_commands": store.active_commands})
                reply = {"voice_turn_id": turn_id, "request_id": req_id, "run_id": run_id, "task_id": task["task_id"], "command_id": cmd_id, "status": "executing", "text": "正在处理车辆指令。"}
                if store.sim_mode == "auto_complete":
                    threading.Timer(0.4, lambda: store.apply_receipt(cmd_id, "completed")).start()
            else:
                emit_step({
                    "step_id": "mcp:get_vehicle_status",
                    "stage": "mcp.tool",
                    "title": "调用只读查询工具",
                    "status": "done",
                    "started_at": utc_now(),
                    "finished_at": utc_now(),
                    "duration_ms": 9,
                    "duration_kind": "measured_tool",
                    "detail": "只读查询无状态写入",
                    "observation": {"action": "get_vehicle_status", "output": {"window.rear_left": store.telemetry["window.rear_left"]["Value"]}},
                })
                reply = {"voice_turn_id": turn_id, "request_id": req_id, "run_id": run_id, "status": "completed", "text": f"已收到「{text}」，当前左后窗状态为 {store.telemetry['window.rear_left']['Value']}。"}

            emit_step({
                "step_id": "hermes:run",
                "stage": "hermes.run",
                "title": "Hermes 意图理解与编排",
                "status": "done",
                "started_at": t1,
                "finished_at": utc_now(),
                "duration_ms": 150,
                "duration_kind": "boundary_wait",
                "detail": f"{run_id} 已完成",
            }, task_id=reply.get("task_id", ""), command_id=reply.get("command_id", ""))

            self._write_sse(req_id, "speech.reply", reply)

    return Handler


def main():
    parser = argparse.ArgumentParser(description="Mock Web Demo SSE API Server")
    parser.add_argument("--host", default="127.0.0.1")
    parser.add_argument("--port", type=int, default=18088)
    parser.add_argument("--sim-mode", default="auto_complete", choices=["auto_complete", "accepted_only"])
    parser.add_argument("--token", default="demo-token")
    args = parser.parse_args()

    store = MockState(sim_mode=args.sim_mode, demo_token=args.token)
    srv = ThreadingHTTPServer((args.host, args.port), make_handler(store))
    print(f"Mock Web Demo server running at http://{args.host}:{args.port} (sim_mode={args.sim_mode})", flush=True)
    try:
        srv.serve_forever()
    except KeyboardInterrupt:
        pass
    finally:
        srv.server_close()


if __name__ == "__main__":
    main()
