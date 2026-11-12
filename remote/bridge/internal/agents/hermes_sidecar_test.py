"""Contract tests use only a fake installed client; no model/network credentials."""
import asyncio
from contextlib import asynccontextmanager
import importlib.util
import io
import json
from pathlib import Path
import sys
import types
import unittest
sys.dont_write_bytecode = True


class Refusal(Exception):
    code = -32000


current = None


@asynccontextmanager
async def connect_gateway(**kwargs):
    assert kwargs["max_size"] == 8 * 1024 * 1024
    yield current


client_module = types.ModuleType("hermes_cli.gateway_client")
client_module.connect_gateway = connect_gateway
client_module.GatewayRPCError = Refusal
parent = types.ModuleType("hermes_cli")
parent.__path__ = []
sys.modules["hermes_cli"] = parent
sys.modules["hermes_cli.gateway_client"] = client_module
spec = importlib.util.spec_from_file_location("salcara_sidecar", Path(__file__).with_name("hermes_sidecar.py"))
sidecar = importlib.util.module_from_spec(spec)
original_stdout = sys.stdout
spec.loader.exec_module(sidecar)
sys.stdout = original_stdout

DESCRIPTION = {"capabilities": ["durable-admission-v1", "event-replay-v1", "local-cli-create-v1"],
               "session_create": {"sources": ["cli"], "parameters": ["request_id", "source", "cwd", "model", "provider", "base_url", "api_key", "yolo"]}}


class Client:
    def __init__(self):
        self.events = asyncio.Queue()
        self.calls = []
        self.failure = None

    async def rpc(self, method, **params):
        self.calls.append((method, params))
        if self.failure:
            raise self.failure
        if method == "runtime.describe":
            return DESCRIPTION
        return {"accepted": True}


class SidecarContractTest(unittest.IsolatedAsyncioTestCase):
    async def invoke(self, client, requests):
        global current
        current = client
        previous_stdin = sys.stdin
        sys.stdin = io.TextIOWrapper(io.BytesIO(b"".join(json.dumps(row).encode() + b"\n" for row in requests)))
        sidecar.wire = io.StringIO()
        try:
            await sidecar.serve()
            return [json.loads(line) for line in sidecar.wire.getvalue().splitlines()]
        finally:
            sys.stdin = previous_stdin

    async def test_installed_client_contract_and_exact_generation_forwarding(self):
        client = Client()
        requests = [{"id": 1, "method": "salcara.initialize"},
                    {"id": 2, "method": "session.create", "params": {"api_key": "fixture-secret", "model": "model"}},
                    {"id": 3, "method": "session.interrupt", "params": {"session_id": "native", "execution_generation": 8}},
                    {"id": 4, "method": "approval.respond", "params": {"session_id": "native", "execution_generation": 8, "prompt_id": "prompt", "choice": "once"}}]
        answers = await self.invoke(client, requests)
        self.assertEqual(answers[0]["result"], {"protocol": "salcara-hermes-gateway-v1", "questions": False})
        self.assertEqual(client.calls[2], ("session.interrupt", requests[2]["params"]))
        self.assertEqual(client.calls[3], ("approval.respond", requests[3]["params"]))
        self.assertNotIn("fixture-secret", json.dumps(answers))

    async def test_transport_failure_is_unknown_without_diagnostics_or_retry(self):
        client = Client()
        client.failure = RuntimeError("untrusted diagnostic fixture-secret")
        answers = await self.invoke(client, [{"id": 1, "method": "prompt.submit", "params": {"session_id": "native", "text": "task"}}])
        self.assertEqual(answers[0]["error"]["message"], "command_delivery_uncertain")
        self.assertNotIn("fixture-secret", json.dumps(answers))
        self.assertEqual(len(client.calls), 1)

    async def test_received_refusal_remains_definitive_and_unknown_methods_never_dispatch(self):
        client = Client()
        client.failure = Refusal("session_busy")
        answers = await self.invoke(client, [{"id": 1, "method": "prompt.submit"}, {"id": 2, "method": "shell.execute"}])
        by_id = {answer["id"]: answer for answer in answers}
        self.assertEqual(by_id[1]["error"]["message"], "session_busy")
        self.assertEqual(by_id[2]["error"]["code"], -32601)
        self.assertEqual(len(client.calls), 1)

    async def test_slow_history_cannot_block_other_session_control(self):
        released = asyncio.Event()
        class SlowClient(Client):
            async def rpc(self, method, **params):
                self.calls.append((method, params))
                if method == "session.resume":
                    await released.wait()
                if method == "session.interrupt":
                    released.set()
                return {"accepted": True}
        client = SlowClient()
        answers = await self.invoke(client, [
            {"id": 1, "method": "session.resume", "params": {"session_id": "a"}},
            {"id": 2, "method": "session.interrupt", "params": {"session_id": "b", "execution_generation": 8}}])
        self.assertTrue(released.is_set())
        self.assertEqual({row["id"] for row in answers}, {1, 2})
        self.assertEqual(client.calls[1][0], "session.interrupt")

    async def test_read_capacity_is_bounded_and_reserves_control_slots(self):
        released = asyncio.Event()
        class BusyClient(Client):
            async def rpc(self, method, **params):
                self.calls.append((method, params))
                if method == "session.resume":
                    await released.wait()
                if method == "approval.respond":
                    released.set()
                return {"accepted": True}
        client = BusyClient()
        requests = [{"id": i, "method": "session.resume"} for i in range(40)]
        requests.append({"id": 41, "method": "approval.respond"})
        answers = await self.invoke(client, requests)
        self.assertTrue(released.is_set())
        self.assertEqual(sum(method == "session.resume" for method, _ in client.calls), sidecar.MAX_READS)
        self.assertEqual(sum(row.get("error", {}).get("message") == "gateway_busy" for row in answers), 40-sidecar.MAX_READS)

    async def test_clarify_reply_preserves_exact_generation_and_answer(self):
        client = Client()
        request = {"id": 1, "method": "clarify.respond", "params": {"session_id": "native", "execution_generation": 8, "prompt_id": "q", "answer": '["a,b","c"]'}}
        answers = await self.invoke(client, [request])
        self.assertEqual(client.calls, [("clarify.respond", request["params"])])
        self.assertEqual(answers[0]["id"], 1)

    async def test_control_resume_uses_reserved_slot_and_native_method(self):
        released = asyncio.Event()
        class BusyClient(Client):
            async def rpc(self, method, **params):
                self.calls.append((method, params))
                if params.get("session_id") == "stop-target":
                    released.set()
                else:
                    await released.wait()
                return {"accepted": True}
        client = BusyClient()
        requests = [{"id": i, "method": "session.resume"} for i in range(sidecar.MAX_READS)]
        requests.append({"id": 100, "method": "salcara.control.resume", "params": {"session_id": "stop-target"}})
        answers = await self.invoke(client, requests)
        self.assertTrue(released.is_set())
        self.assertEqual(client.calls[-1], ("session.resume", {"session_id": "stop-target"}))
        self.assertEqual(len(answers), sidecar.MAX_READS + 1)

    def test_capability_gate_refuses_older_or_incomplete_owners(self):
        self.assertTrue(sidecar.contract(DESCRIPTION))
        for value in (None, [], {}, {**DESCRIPTION, "capabilities": []}, {**DESCRIPTION, "session_create": {"sources": ["cli"], "parameters": []}}):
            self.assertFalse(sidecar.contract(value))


if __name__ == "__main__":
    unittest.main()
