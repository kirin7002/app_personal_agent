"""Unit tests for hermes.turn_binding_adapter (H01)."""

from __future__ import annotations

import asyncio
import unittest
from types import SimpleNamespace

from hermes import turn_binding_adapter as adapter


class TurnBindingAdapterTests(unittest.IsolatedAsyncioTestCase):
    def tearDown(self) -> None:
        with adapter._LOCK:
            adapter._RUN_METADATA.clear()
            adapter._ADMISSION_METADATA.clear()

    async def test_concurrent_httpx_hook_isolation_and_strip(self) -> None:
        async def run_one(handle: str, session_id: str, delay: float) -> tuple[str | None, str | None]:
            token = adapter._MCP_OUTBOUND_TURN_META.set(
                adapter.TurnMetadata(turn_handle=handle, hermes_session_id=session_id)
            )
            try:
                await asyncio.sleep(delay)
                req = SimpleNamespace(headers={"Authorization": "Bearer svc-token"})
                await adapter._httpx_inject_turn_headers(req)
                return (
                    req.headers.get(adapter.TURN_HANDLE_HEADER),
                    req.headers.get(adapter.HERMES_SESSION_HEADER),
                )
            finally:
                adapter._MCP_OUTBOUND_TURN_META.reset(token)

        res_a, res_b = await asyncio.gather(
            run_one("TH-USER-A", "hermes-S1-VCL1", 0.02),
            run_one("TH-USER-B", "hermes-S2-VCL2", 0.005),
        )
        self.assertEqual(res_a, ("TH-USER-A", "hermes-S1-VCL1"))
        self.assertEqual(res_b, ("TH-USER-B", "hermes-S2-VCL2"))

        # Stale headers on a pooled request must be stripped when no turn metadata is active.
        stale_req = SimpleNamespace(
            headers={
                "Authorization": "Bearer svc-token",
                adapter.TURN_HANDLE_HEADER: "TH-STALE",
                adapter.HERMES_SESSION_HEADER: "S-STALE",
            }
        )
        await adapter._httpx_inject_turn_headers(stale_req)
        self.assertNotIn(adapter.TURN_HANDLE_HEADER, stale_req.headers)
        self.assertNotIn(adapter.HERMES_SESSION_HEADER, stale_req.headers)
        self.assertEqual(stale_req.headers["Authorization"], "Bearer svc-token")

    def test_register_lookup_and_release_by_run_and_admission(self) -> None:
        meta = adapter.TurnMetadata(turn_handle="TH-101", hermes_session_id="S1-VCL1")
        adapter.register_run_metadata("run_abc", meta)
        adapter.bind_admission_metadata("run_abc", "adm_xyz")

        self.assertEqual(adapter.lookup_turn_metadata(run_id="run_abc"), meta)
        self.assertEqual(adapter.lookup_turn_metadata(admission_id="adm_xyz"), meta)

        adapter.release_run_metadata("run_abc", admission_id="adm_xyz")
        self.assertIsNone(adapter.lookup_turn_metadata(run_id="run_abc"))
        self.assertIsNone(adapter.lookup_turn_metadata(admission_id="adm_xyz"))

    def test_extract_headers_requires_non_empty_handle_and_session(self) -> None:
        self.assertIsNone(adapter.extract_turn_metadata_from_headers({}))
        self.assertIsNone(
            adapter.extract_turn_metadata_from_headers({adapter.TURN_HANDLE_HEADER: "TH-1"})
        )
        parsed = adapter.extract_turn_metadata_from_headers(
            {adapter.TURN_HANDLE_HEADER: "TH-1"},
            fallback_session_id="S1",
        )
        self.assertEqual(
            parsed,
            adapter.TurnMetadata(turn_handle="TH-1", hermes_session_id="S1"),
        )
        self.assertEqual(
            parsed.to_mcp_meta(),
            {
                adapter.META_TURN_HANDLE_KEY: "TH-1",
                adapter.META_HERMES_SESSION_KEY: "S1",
            },
        )


if __name__ == "__main__":
    unittest.main()
