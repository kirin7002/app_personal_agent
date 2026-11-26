# H01 Hermes 运行时与可信轮次绑定验证报告

验证日期：2026-11-26。本报告记录 H01 对真实 Hermes 运行时的版本锁定、可信轮次传递边界、Runs API 协议行为以及可复现探针的实测结论。

## 1. 锁定版本与运行时配置

| 维度 | 锁定值 |
| --- | --- |
| Hermes 版本 / Commit | `vgit.72c8803 (2026.9.24)` / `72c88037` |
| Python / 依赖 | Python `3.14.9`、`mcp==2.3.0`、`httpx2`（SDK 内置） |
| 模型与提供方 | `deepseek-v4.1-flash-1017`（`custom:vehicle-bailian`，`anthropic_messages`） |
| MCP 传输与协议 | Streamable HTTP，`MCP-Protocol-Version: 2025-11-25` |
| 配置样例 | [config.vehicle.yaml.example](../hermes/config.vehicle.yaml.example) |
| 车控提示词 | [vehicle_system_prompt.md](../hermes/prompts/vehicle_system_prompt.md) |
| 最小适配层 | [turn_binding_adapter.py](../hermes/turn_binding_adapter.py)、[gateway_launcher.py](../hermes/gateway_launcher.py) |

## 2. 上游 Hermes 评估与最小适配结论

### 2.1 原生 Hermes 结论（不可直接开启真实车辆写入）

检查 Hermes `72c88037` 源码与实测结果确认：
1. `gateway/platforms/api_server_runs.py` 不读取也不保存自定义入站轮次头（`X-Vehicle-Turn-Handle`、`X-Vehicle-Hermes-Session-ID`）。
2. `tools/mcp_tool_transport.py` 仅在建立 HTTP 客户端时注入 `config.yaml` 中的静态连接头。
3. `tools/mcp_tool_handlers.py` 调用 `server.session.call_tool(tool_name, arguments=args, ...)` 时仅传 `progress_callback`，不传 `meta=`。
4. `tools/mcp_tool_loop.py` 通过 `run_coroutine_threadsafe` 将 MCP 调用切换到独立后台事件循环 `mcp-event-loop`，不会继承调用线程的普通 `ContextVar`。

**结论**：未经适配的原版 Hermes `72c88037` **禁止开启真实车辆写入**，因为 Go MCP 无法在不信任模型参数的前提下绑定本轮认证用户、车辆、会话与候选快照。

### 2.2 已验证的最小非侵入适配层

[turn_binding_adapter.py](../hermes/turn_binding_adapter.py) 不修改上游 Hermes 源码，在启动 [gateway_launcher.py](../hermes/gateway_launcher.py) 时安装 5 个最小挂钩：
1. **入站接收**：在 `POST /v1/runs` 仅从受信任 HTTP 头读取 `X-Vehicle-Turn-Handle` 与 `X-Vehicle-Hermes-Session-ID`（绝不进入 `input`、`instructions` 或工具参数），按 `run_id` 和 `session_authority` 的 `admission_id` 注册，并在 Run 结束/取消时清理。
2. **执行绑定**：同时覆盖直接执行（`_run_agent_sync`）与持久化会话权威（`session_ingress._admission_scope`），为当前 Run 绑定隔离的 `ContextVar`。
3. **跨事件循环透传**：在 Agent 执行线程捕获本轮元数据，显式携入 `mcp-event-loop` 的任务局部 `ContextVar`。
4. **双载体出站传递**：
   - HTTP 头：`httpx` async `request` hook 为每次出站请求写入 `X-Vehicle-Turn-Handle` 与 `X-Vehicle-Hermes-Session-ID`；当无有效轮次时主动删除这两个头，防止连接池复用导致跨轮残留；同时加入 `strict_redirect_headers` 保护集合。
   - MCP JSON-RPC `params._meta`：通过 `ClientSession.call_tool(..., meta={"vehicle.turn_handle": ..., "vehicle.hermes_session_id": ...})` 注入。
5. **模型隔离**：六个工具的 JSON Schema 均设置 `additionalProperties: false` 且不含任何身份/轮次字段；Go MCP 侧同时校验 HTTP 头与 `params._meta` 一致，并拒绝参数中夹带 `turn_handle` / `user_id` / `session_id` / `vehicle_id`。

## 3. 真实 Hermes 运行时行为实测记录

### 3.1 MCP 工具注册名

实测 Hermes `72c88037` 配置 `mcp_servers.vehicle` 后，SSE `tool.started` / `tool.completed` 中观测到的注册名为**双下划线前缀** `mcp__vehicle__<tool>`：
- `mcp__vehicle__create_vehicle_task`
- `mcp__vehicle__list_vehicle_tasks`
- `mcp__vehicle__dispatch_vehicle_command`
- `mcp__vehicle__get_vehicle_status`
- `mcp__vehicle__search_vehicle_manual`
- `mcp__vehicle__search_vehicle_capabilities`

（原始 MCP JSON-RPC `tools/call` 仍发送裸工具名，如 `create_vehicle_task`。）

### 3.2 写工具审批配置

Hermes `72c88037` 的 `tools/mcp_tool_handlers.py::_trust_gate_check` 对 `trust: untrusted` 服务器上未声明 `readOnlyHint=true` 的工具会暂停执行并发送 `approval.request`，导致语音轮次阻塞。
因此车辆 MCP 在 Hermes 侧必须配置：
- `trust: full`、`sampling.enabled: false`
- 只读工具（`list_vehicle_tasks`、`get_vehicle_status`、`search_vehicle_manual`、`search_vehicle_capabilities`）在 `tools/list` 标注 `annotations.readOnlyHint: true`，写工具标注 `false`
- 所有安全边界（服务鉴权、可信轮次锁、候选版本、意图校验、参数 Schema、单轮单写幂等）由 Go MCP（H04）强制执行。

### 3.3 Runs API 幂等、SSE 断线重订阅、终态轮询与回调竞态

- **`Idempotency-Key`**：Hermes `72c88037` 原生支持持久化 SQLite 幂等表（`runs_idempotency.db`）。同键同体返回 `HTTP 202` + `Idempotency-Replayed: true` + 同一 `run_id`；同键异体返回 `HTTP 409` + `code: "idempotency_key_conflict"`。
- **SSE 断线重订阅**：`GET /v1/runs/{run_id}/events` 支持 `Last-Event-ID` 头（以及 `?last_seq=`），断线后按已收 `id` 重订阅可无缝续收后续事件直至 `run.completed`。
- **终态轮询**：`GET /v1/runs/{run_id}` 返回 `status: "completed"` 及完整 `output`，可供 SSE 断流或终态早于建连时恢复。
- **MCP 回调早于 HTTP 202**：H02 在发出 `POST /v1/runs` 前即先持久化 `TurnContext`（含 `turn_handle` 和候选快照），Hermes 适配层在创建执行任务前完成 `turn_handle` 绑定，因此即便 MCP `tools/call` 先于 Go 读完 HTTP 202 响应到达，Go MCP 仍可通过 `turn_handle` 正确解析并加锁。

## 4. 实测验证命令与结果

### 单元测试（Go + Python）
```bash
go test -race ./... && go vet ./...
/Users/kirin/Documents/ChatGPT/personal_agent/.runtime/hermes-home/installs/b59ccf3b57118ba2/environments/caa4495d4a1446e8821e88cb9961e91b/venv/bin/python -m unittest hermes/test_turn_binding_adapter.py -v
```

### 真实 Hermes 端到端探针
```bash
go run ./cmd/h01probe -out hermes/testdata/h01-live-probe-report.json
```

脱敏实测结果保存在 [h01-live-probe-report.json](../hermes/testdata/h01-live-probe-report.json)，全部 10 项探针通过：
- `scenario_a_run1_create_task`: `true`（「把车窗关一下」→ `create_vehicle_task` → `T101`，追问车窗位置）
- `scenario_a_run2_resume_dispatch`: `true`（「左后窗」→ `dispatch_vehicle_command(task_id=T101, position=rear_left)` → `C88` `pending_dispatch`，播报「正在关闭左后车窗。」）
- `scenario_b_dual_candidate_match`: `true`（`T202` 空调 + `T201` 车窗并存时，「左后窗」唯一续办 `T201`，保留 `T202` 为 `pending_reply`）
- `parallel_multi_user_isolation`: `true`（`U_PAR_1`/`VCL_PAR_1` 与 `U_PAR_2`/`VCL_PAR_2` 并行发 Run，无任何句柄或作用域串扰）
- `cancelled_late_call_rejected`: `true`（已取消轮次的迟到调用被 Go MCP 以 `TURN_CONTEXT_INVALID` 拒绝）
- `missing_handle_rejected`: `true`（无句柄请求不继承历史头，被 Go MCP 以 `TURN_CONTEXT_INVALID` 拒绝）
- `idempotency_replay`: `true`、`idempotency_conflict_409`: `true`
- `sse_disconnect_resubscribe`: `true`、`terminal_polling`: `true`

## 5. 与 Agent A（H00/H02）共享契约对齐确认

- 入站/出站 HTTP 头完全对齐 `contract/vehicle.go`：`X-Vehicle-Turn-Handle`、`X-Vehicle-Hermes-Session-ID`。
- MCP `params._meta` 键：`vehicle.turn_handle`、`vehicle.hermes_session_id`（与 HTTP 头双校验一致）。
- H04 写事务将直接复用 H02 的 `vehicle/turns.LockWritableTurn(ctx, tx, contract.TurnMetadata{...}, now)`，读工具调用 `contract.TurnRegistry.ResolveTurn`。
- H03 将实现 `contract.ProgressReader.ReadTurnProgress(ctx, scope, requestID)`（无写返回 `contract.ErrTurnHasNoWrite`）。

## 6. Hermes 版本升级验证清单

升级 Hermes commit/tag 前必须重跑 `cmd/h01probe` 并核对：
1. `gateway/platforms/api_server_runs.py` 中 `_handle_runs`、`_admit_run_to_authority`、`_execute_run`、`_run_agent_sync` 签名未变。
2. `gateway/session_ingress.py::_admission_scope` 仍包含 `row['request_id']` / `row['admission_id']`。
3. `tools/mcp_tool_loop.py::_run_on_mcp_loop`、`tools/mcp_tool_handlers.py::_call_tool_racing_stdio_death`、`tools/mcp_tool_transport.py::_make_redirect_header_stripper` 钩子仍然生效。
4. 实测工具注册名前缀（当前 `mcp__vehicle__`）与 `Idempotency-Key` / `Last-Event-ID` 语义未退化。
