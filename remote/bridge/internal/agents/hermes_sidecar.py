"""Salcara's bounded stdio transport to Hermes' installed canonical client.

Protocol reference: NousResearch/hermes-agent, hermes_cli/gateway_client.py,
gateway/session_controls.py. This file does not implement authentication or
execute model work itself. Closing stdin detaches this viewer only.
"""
import asyncio
import contextlib
import json
import sys

wire = sys.stdout
sys.stdout = sys.stderr  # Imported Hermes diagnostics cannot corrupt JSONL.
from hermes_cli.gateway_client import connect_gateway, GatewayRPCError

METHODS = {"session.create", "session.resume", "prompt.submit", "prompt.receipt",
           "session.interrupt", "approval.respond", "clarify.respond", "salcara.control.resume"}
READS = {"session.resume", "prompt.receipt", "salcara.initialize"}
EVENTS = {"message.delta", "message.complete", "tool.start", "tool.complete",
          "approval.request", "approval.settled", "clarify.request", "clarify.settled",
          "session.info", "session.replay_gap", "status.update", "notification.show"}
LIMIT = 8 * 1024 * 1024
MAX_REQUESTS = 32
MAX_READS = 24  # reserve slots so history reads cannot starve stop/approval


def emit(frame):
    wire.write(json.dumps(frame, ensure_ascii=True, separators=(",", ":")) + "\n")
    wire.flush()


def contract(description):
    if not isinstance(description, dict):
        return False
    create = description.get("session_create", {})
    if not isinstance(create, dict):
        return False
    required = {"request_id", "source", "cwd", "model", "provider", "base_url", "api_key", "yolo"}
    return (isinstance(description, dict)
            and {"durable-admission-v1", "event-replay-v1", "local-cli-create-v1"}.issubset(description.get("capabilities", []))
            and "cli" in create.get("sources", [])
            and required.issubset(create.get("parameters", [])))


async def events(client):
    while True:
        frame = await client.events.get()
        if isinstance(frame, Exception):
            emit({"method": "gateway.disconnected", "params": {}})
            return
        if not isinstance(frame, dict):
            continue
        if "id" in frame and "method" in frame:
            # sudo/server requests are not native approval prompts. Never answer
            # them with a broad grant or expose a password entry on the phone.
            await client.websocket.send(json.dumps({"jsonrpc": "2.0", "id": frame["id"],
                "error": {"code": -32601, "message": "unsupported_client_request"}}))
            continue
        params = frame.get("params", {})
        if frame.get("method") == "event" and params.get("type") in EVENTS:
            emit({"method": "event", "params": params})


async def serve():
    async with connect_gateway(max_size=LIMIT) as client:
        pump = asyncio.create_task(events(client))
        pending = set()
        reads = set()

        async def request_one(rid, method, params):
            # The stop path needs a fresh generation before interrupting. Its
            # read uses the reserved slots too; never forward this local alias.
            if method == "salcara.control.resume":
                method = "session.resume"
            try:
                if method == "salcara.initialize":
                    description = await client.rpc("runtime.describe")
                    if not contract(description):
                        raise ValueError("unsupported_gateway_contract")
                    result = {"protocol": "salcara-hermes-gateway-v1",
                              "questions": "clarify-batch-v1" in description.get("capabilities", [])}
                else:
                    result = await client.rpc(method, **params)
                emit({"id": rid, "result": result})
            except GatewayRPCError as exc:
                emit({"id": rid, "error": {"code": exc.code or -32000, "message": str(exc),
                    "data": {"reason": str(exc)}}})
            except Exception:
                reason = "gateway_read_failed" if method in READS else "command_delivery_uncertain"
                emit({"id": rid, "error": {"code": -32001, "message": reason,
                    "data": {"code": reason}}})

        try:
            while True:
                line = await asyncio.to_thread(sys.stdin.buffer.readline, LIMIT + 1)
                if not line:
                    # Let already-completed acknowledgements drain, then detach
                    # this viewer without waiting on an unresponsive RPC/owner.
                    if pending:
                        await asyncio.wait(pending, timeout=1)
                    return
                if len(line) > LIMIT or not line.endswith(b"\n"):
                    return  # no guessed request identity for an oversized frame
                request = json.loads(line)
                rid, method = request.get("id"), request.get("method")
                params = request.get("params") or {}
                if isinstance(rid, bool) or not isinstance(rid, int) or not isinstance(params, dict):
                    continue
                if method not in METHODS and method != "salcara.initialize":
                    emit({"id": rid, "error": {"code": -32601, "message": "unsupported_method"}})
                    continue
                if len(pending) >= MAX_REQUESTS or method in READS and len(reads) >= MAX_READS:
                    # Definitive pre-dispatch refusal, never an attempted write.
                    emit({"id": rid, "error": {"code": -32002, "message": "gateway_busy"}})
                    continue
                task = asyncio.create_task(request_one(rid, method, params))
                pending.add(task)
                task.add_done_callback(pending.discard)
                if method in READS:
                    reads.add(task)
                    task.add_done_callback(reads.discard)
        finally:
            remaining = list(pending)
            for task in remaining:
                task.cancel()
            await asyncio.gather(*remaining, return_exceptions=True)
            pump.cancel()
            with contextlib.suppress(asyncio.CancelledError):
                await pump


if __name__ == "__main__":
    try:
        asyncio.run(serve())
    except Exception:
        # Bootstrap failures are deliberately diagnostic-only, without raw paths/keys.
        print("Hermes Gateway unavailable; update Hermes and check its local gateway.", file=sys.stderr)
        sys.exit(1)
