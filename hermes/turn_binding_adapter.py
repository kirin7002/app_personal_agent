"""Minimal Hermes runtime adapter for trusted per-turn MCP metadata propagation (H01).

Target Hermes version: vgit.72c8803 (2026.9.24), MCP Python SDK 2.3.0.

Why this adapter exists:
  Upstream Hermes vgit.72c8803 only supports static per-connection MCP HTTP headers
  configured in config.yaml and invokes `ClientSession.call_tool(tool_name, arguments=args)`
  without per-run headers or `meta=`. It also dispatches MCP tool calls onto a shared
  background event loop (`mcp-event-loop`) that does not inherit the calling turn's
  contextvars. Therefore stock Hermes cannot prove to Go MCP which authenticated user,
  session, vehicle, and candidate snapshot originated a `tools/call` without trusting
  model-controlled inputs.

How this adapter solves it without modifying upstream Hermes source files:
  1. Inbound `POST /v1/runs`: reads `X-Vehicle-Turn-Handle` and
     `X-Vehicle-Hermes-Session-ID` from trusted HTTP request headers (never from `input`,
     `instructions`, or model-visible arguments) and registers them keyed by `run_id`
     and session-authority `admission_id` before execution starts.
  2. Execution binding: binds a run-isolated ContextVar on both the direct executor
     path (`_run_agent_sync`) and the durable `session_authority` path
     (`_admission_scope`).
  3. Outbound MCP `tools/call`: captures the active turn metadata on the agent thread
     BEFORE crossing onto `mcp-event-loop`, then attaches it to:
       - Outgoing HTTP request headers (`X-Vehicle-Turn-Handle`,
         `X-Vehicle-Hermes-Session-ID`) via an `httpx` async request hook (explicitly
         stripping those headers when no turn metadata is bound to prevent bleed across
         pooled HTTP connections), AND
       - MCP JSON-RPC `params._meta` (`vehicle.turn_handle`,
         `vehicle.hermes_session_id`) via `ClientSession.call_tool(..., meta=...)`.
"""

from __future__ import annotations

import contextlib
import contextvars
import functools
import inspect
import logging
import threading
from dataclasses import dataclass
from typing import Any, Optional

logger = logging.getLogger("vehicle.turn_binding_adapter")

TURN_HANDLE_HEADER = "X-Vehicle-Turn-Handle"
HERMES_SESSION_HEADER = "X-Vehicle-Hermes-Session-ID"

META_TURN_HANDLE_KEY = "vehicle.turn_handle"
META_HERMES_SESSION_KEY = "vehicle.hermes_session_id"

TARGET_HERMES_COMMIT = "72c88037"


@dataclass(frozen=True, slots=True)
class TurnMetadata:
    """Trusted server-to-server turn metadata; never exposed to the LLM."""

    turn_handle: str
    hermes_session_id: str

    def valid(self) -> bool:
        return bool(self.turn_handle.strip() and self.hermes_session_id.strip())

    def to_mcp_meta(self) -> dict[str, str]:
        return {
            META_TURN_HANDLE_KEY: self.turn_handle,
            META_HERMES_SESSION_KEY: self.hermes_session_id,
        }


# Inbound HTTP request -> _handle_runs -> _admit_run_to_authority / _execute_run.
_INBOUND_TURN_META: contextvars.ContextVar[Optional[TurnMetadata]] = contextvars.ContextVar(
    "vehicle_inbound_turn_meta", default=None
)

# Active turn on the agent execution thread (both direct and session_authority modes).
_ACTIVE_TURN_META: contextvars.ContextVar[Optional[TurnMetadata]] = contextvars.ContextVar(
    "vehicle_active_turn_meta", default=None
)

# Outbound MCP call task-local ContextVar on the background mcp-event-loop.
_MCP_OUTBOUND_TURN_META: contextvars.ContextVar[Optional[TurnMetadata]] = contextvars.ContextVar(
    "vehicle_mcp_outbound_turn_meta", default=None
)

_LOCK = threading.RLock()
_RUN_METADATA: dict[str, TurnMetadata] = {}
_ADMISSION_METADATA: dict[str, TurnMetadata] = {}
_INSTALLED = False


def register_run_metadata(
    run_id: str,
    meta: TurnMetadata,
    *,
    admission_id: Optional[str] = None,
) -> None:
    """Record trusted metadata for an admitted run before execution begins."""
    if not run_id or not meta.valid():
        return
    with _LOCK:
        _RUN_METADATA[run_id] = meta
        if admission_id:
            _ADMISSION_METADATA[admission_id] = meta


def bind_admission_metadata(run_id: str, admission_id: str) -> None:
    """Associate a session-authority admission_id with an already-registered run_id."""
    if not run_id or not admission_id:
        return
    with _LOCK:
        meta = _RUN_METADATA.get(run_id)
        if meta is not None:
            _ADMISSION_METADATA[admission_id] = meta


def release_run_metadata(run_id: str, *, admission_id: Optional[str] = None) -> None:
    """Remove run metadata once execution finishes to avoid retaining stale handles."""
    with _LOCK:
        if run_id:
            _RUN_METADATA.pop(run_id, None)
        if admission_id:
            _ADMISSION_METADATA.pop(admission_id, None)


def lookup_turn_metadata(
    *,
    run_id: Optional[str] = None,
    admission_id: Optional[str] = None,
) -> Optional[TurnMetadata]:
    """Resolve trusted metadata for the currently executing turn."""
    active = _ACTIVE_TURN_META.get()
    if active is not None:
        return active
    with _LOCK:
        if admission_id and admission_id in _ADMISSION_METADATA:
            return _ADMISSION_METADATA[admission_id]
        if run_id and run_id in _RUN_METADATA:
            return _RUN_METADATA[run_id]
    try:
        from tools.approval_context import get_current_session_key

        session_key = get_current_session_key()
        if session_key:
            with _LOCK:
                return _RUN_METADATA.get(session_key)
    except Exception:
        pass
    return None


def extract_turn_metadata_from_headers(
    headers: Any,
    fallback_session_id: str = "",
) -> Optional[TurnMetadata]:
    """Parse trusted turn metadata from inbound HTTP headers only."""
    if headers is None:
        return None
    handle = (headers.get(TURN_HANDLE_HEADER) or "").strip()
    if not handle:
        return None
    hermes_session_id = (headers.get(HERMES_SESSION_HEADER) or fallback_session_id or "").strip()
    meta = TurnMetadata(turn_handle=handle, hermes_session_id=hermes_session_id)
    return meta if meta.valid() else None


async def _httpx_inject_turn_headers(request: Any) -> None:
    """httpx async request hook: attach current task's trusted headers or strip stale ones."""
    meta = _MCP_OUTBOUND_TURN_META.get()
    if meta is not None and meta.valid():
        request.headers[TURN_HANDLE_HEADER] = meta.turn_handle
        request.headers[HERMES_SESSION_HEADER] = meta.hermes_session_id
    else:
        request.headers.pop(TURN_HANDLE_HEADER, None)
        request.headers.pop(HERMES_SESSION_HEADER, None)


def _patch_api_server_runs() -> None:
    from gateway.platforms import api_server_runs as runs_mod

    if getattr(runs_mod, "_vehicle_turn_binding_patched", False):
        return

    orig_handle_runs = runs_mod._handle_runs
    orig_admit = runs_mod._admit_run_to_authority
    orig_execute_run = runs_mod._execute_run
    orig_run_agent_sync = runs_mod._run_agent_sync

    @functools.wraps(orig_handle_runs)
    async def wrapped_handle_runs(self, request, *, _api_server):
        raw_handle = (request.headers.get(TURN_HANDLE_HEADER) or "").strip()
        raw_sid = (request.headers.get(HERMES_SESSION_HEADER) or "").strip()
        inbound = TurnMetadata(turn_handle=raw_handle, hermes_session_id=raw_sid) if raw_handle else None
        token = _INBOUND_TURN_META.set(inbound)
        try:
            return await orig_handle_runs(self, request, _api_server=_api_server)
        finally:
            _INBOUND_TURN_META.reset(token)

    @functools.wraps(orig_admit)
    async def wrapped_admit(
        self, launch, run_id, session_history_delivery, _declared_selected,
        idempotency_scope, idempotency_key, _openai_error,
    ):
        inbound = _INBOUND_TURN_META.get()
        if inbound is not None and inbound.turn_handle:
            effective = TurnMetadata(
                turn_handle=inbound.turn_handle,
                hermes_session_id=inbound.hermes_session_id or launch.session_id,
            )
            if effective.valid():
                register_run_metadata(run_id, effective)
        refusal = await orig_admit(
            self, launch, run_id, session_history_delivery, _declared_selected,
            idempotency_scope, idempotency_key, _openai_error,
        )
        if refusal is None and launch.admission is not None:
            row = launch.admission[2]
            admission_id = row.get("admission_id") if isinstance(row, dict) else None
            if admission_id:
                bind_admission_metadata(run_id, str(admission_id))
        elif refusal is not None:
            release_run_metadata(run_id)
        return refusal

    @functools.wraps(orig_execute_run)
    async def wrapped_execute_run(self, run, *, _api_server):
        inbound = _INBOUND_TURN_META.get()
        admission_id = None
        if run.admission is not None and len(run.admission) >= 3 and isinstance(run.admission[2], dict):
            admission_id = run.admission[2].get("admission_id")
        if inbound is not None and inbound.turn_handle:
            effective = TurnMetadata(
                turn_handle=inbound.turn_handle,
                hermes_session_id=inbound.hermes_session_id or run.session_id,
            )
            if effective.valid():
                register_run_metadata(run.run_id, effective, admission_id=admission_id)
        try:
            return await orig_execute_run(self, run, _api_server=_api_server)
        finally:
            release_run_metadata(run.run_id, admission_id=admission_id)

    @functools.wraps(orig_run_agent_sync)
    def wrapped_run_agent_sync(self, run, agent, approval_notify, *, _api_server):
        meta = lookup_turn_metadata(run_id=run.run_id)
        token = _ACTIVE_TURN_META.set(meta)
        try:
            return orig_run_agent_sync(self, run, agent, approval_notify, _api_server=_api_server)
        finally:
            _ACTIVE_TURN_META.reset(token)

    runs_mod._handle_runs = wrapped_handle_runs
    runs_mod._admit_run_to_authority = wrapped_admit
    runs_mod._execute_run = wrapped_execute_run
    runs_mod._run_agent_sync = wrapped_run_agent_sync
    runs_mod._vehicle_turn_binding_patched = True


def _patch_session_ingress() -> None:
    """Bind turn metadata during session_authority admission execution."""
    try:
        from gateway import session_ingress as ingress_mod
    except ImportError:
        return
    if getattr(ingress_mod, "_vehicle_turn_binding_patched", False):
        return
    orig_scope = getattr(ingress_mod, "_admission_scope", None)
    if orig_scope is None:
        return

    @contextlib.asynccontextmanager
    async def wrapped_admission_scope(authority, ref, home, row, event, captured):
        req_id = row.get("request_id") if isinstance(row, dict) else None
        adm_id = row.get("admission_id") if isinstance(row, dict) else None
        meta = lookup_turn_metadata(run_id=req_id, admission_id=adm_id)
        token = _ACTIVE_TURN_META.set(meta)
        try:
            async with orig_scope(authority, ref, home, row, event, captured):
                yield
        finally:
            _ACTIVE_TURN_META.reset(token)

    ingress_mod._admission_scope = wrapped_admission_scope
    ingress_mod._vehicle_turn_binding_patched = True


def _patch_mcp_handlers() -> None:
    """Carry active turn metadata across mcp-event-loop and into call_tool(..., meta=...)."""
    from tools import mcp_tool_handlers as handlers_mod

    if getattr(handlers_mod, "_vehicle_turn_binding_patched", False):
        return

    orig_make_tool_handler = handlers_mod._make_tool_handler
    orig_call_tool_racing = handlers_mod._call_tool_racing_stdio_death

    @functools.wraps(orig_call_tool_racing)
    async def wrapped_call_tool_racing(server, server_name: str, tool_name: str, args: dict, progress_callback=None):
        meta = _MCP_OUTBOUND_TURN_META.get()
        if meta is None or not meta.valid():
            return await orig_call_tool_racing(server, server_name, tool_name, args, progress_callback)

        orig_session_call = server.session.call_tool
        sig = inspect.signature(orig_session_call)
        accepts_meta = "meta" in sig.parameters

        if not accepts_meta:
            return await orig_call_tool_racing(server, server_name, tool_name, args, progress_callback)

        @functools.wraps(orig_session_call)
        async def call_tool_with_meta(*c_args, **c_kwargs):
            existing = dict(c_kwargs.get("meta") or {})
            existing.update(meta.to_mcp_meta())
            c_kwargs["meta"] = existing
            return await orig_session_call(*c_args, **c_kwargs)

        # Temporarily bind per-call under server._rpc_lock (already held by caller).
        server.session.call_tool = call_tool_with_meta
        try:
            return await orig_call_tool_racing(server, server_name, tool_name, args, progress_callback)
        finally:
            server.session.call_tool = orig_session_call

    @functools.wraps(orig_make_tool_handler)
    def wrapped_make_tool_handler(server_name: str, tool_name: str, tool_timeout: float, *, native_images: bool = False):
        inner = orig_make_tool_handler(server_name, tool_name, tool_timeout, native_images=native_images)

        @functools.wraps(inner)
        def wrapped_handler(args: dict, **kwargs):
            # Capture on the agent thread before _run_on_mcp_loop jumps to mcp-event-loop.
            meta = lookup_turn_metadata()
            token = _ACTIVE_TURN_META.set(meta)
            try:
                return inner(args, **kwargs)
            finally:
                _ACTIVE_TURN_META.reset(token)

        return wrapped_handler

    handlers_mod._call_tool_racing_stdio_death = wrapped_call_tool_racing
    handlers_mod._make_tool_handler = wrapped_make_tool_handler
    handlers_mod._vehicle_turn_binding_patched = True


def _patch_mcp_loop() -> None:
    """Propagate _ACTIVE_TURN_META into _MCP_OUTBOUND_TURN_META on the MCP background loop."""
    from tools import mcp_tool_loop as loop_mod

    if getattr(loop_mod, "_vehicle_turn_binding_patched", False):
        return

    orig_run_on_mcp_loop = loop_mod._run_on_mcp_loop

    @functools.wraps(orig_run_on_mcp_loop)
    def wrapped_run_on_mcp_loop(coro_or_factory, timeout: float = 30):
        meta = lookup_turn_metadata()

        def _wrapped_factory():
            coro = coro_or_factory() if callable(coro_or_factory) else coro_or_factory

            async def _scoped():
                token = _MCP_OUTBOUND_TURN_META.set(meta)
                try:
                    return await coro
                finally:
                    _MCP_OUTBOUND_TURN_META.reset(token)

            return _scoped()

        return orig_run_on_mcp_loop(_wrapped_factory, timeout=timeout)

    loop_mod._run_on_mcp_loop = wrapped_run_on_mcp_loop
    loop_mod._vehicle_turn_binding_patched = True


def _patch_mcp_http_transport() -> None:
    """Attach the per-request httpx header hook to Streamable HTTP MCP clients."""
    from tools import mcp_tool_transport as transport_mod

    if getattr(transport_mod, "_vehicle_turn_binding_patched", False):
        return

    orig_make_stripper = transport_mod._make_redirect_header_stripper

    @functools.wraps(orig_make_stripper)
    def wrapped_make_stripper(httpx_mod, initial_url, *, strict: bool, configured_header_names: set):
        # Also protect trusted turn headers from ever leaking on cross-origin redirects.
        protected = set(configured_header_names or ()) | {
            TURN_HANDLE_HEADER.lower(),
            HERMES_SESSION_HEADER.lower(),
        }
        build_client = orig_make_stripper(
            httpx_mod,
            initial_url,
            strict=strict,
            configured_header_names=protected,
        )

        def build_client_with_turn_hook(**client_kwargs):
            hooks = dict(client_kwargs.get("event_hooks") or {})
            req_hooks = list(hooks.get("request") or [])
            if _httpx_inject_turn_headers not in req_hooks:
                req_hooks.append(_httpx_inject_turn_headers)
            hooks["request"] = req_hooks
            client_kwargs["event_hooks"] = hooks
            return build_client(**client_kwargs)

        return build_client_with_turn_hook

    transport_mod._make_redirect_header_stripper = wrapped_make_stripper
    transport_mod._vehicle_turn_binding_patched = True


def install() -> bool:
    """Install all Hermes turn-binding hooks idempotently."""
    global _INSTALLED
    with _LOCK:
        if _INSTALLED:
            return False
        _patch_api_server_runs()
        _patch_session_ingress()
        _patch_mcp_loop()
        _patch_mcp_handlers()
        _patch_mcp_http_transport()
        _INSTALLED = True
        logger.info("Installed vehicle trusted turn-binding adapter for Hermes %s", TARGET_HERMES_COMMIT)
        return True
