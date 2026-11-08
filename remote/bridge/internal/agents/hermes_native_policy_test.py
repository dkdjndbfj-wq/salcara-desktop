"""Execute the official frozen launch-key function from a supplied checkout.

Set HERMES_POLICY_SOURCE to the upstream gateway/session_policy.py. The function
is parsed from that file, not reimplemented as a permissive fixture. This test
does not start Hermes or contact a model provider.
"""
import ast
import hashlib
import os
from pathlib import Path
import sys
import types
import unittest
from unittest.mock import patch

sys.dont_write_bytecode = True


class NativePolicyContractTest(unittest.TestCase):
    def test_official_resume_accepts_only_original_launch_key(self):
        path = os.environ.get("HERMES_POLICY_SOURCE")
        if not path:
            self.skipTest("set HERMES_POLICY_SOURCE to the official policy source")
        source = Path(path).read_text(encoding="utf-8")
        parsed = ast.parse(source, filename=path)
        names = {"rebind_launch_key", "_key_slot"}
        functions = [n for n in parsed.body if isinstance(n, ast.FunctionDef) and n.name in names]
        self.assertEqual({n.name for n in functions}, names)
        digest = lambda key: hashlib.sha256(key.encode()).hexdigest()
        ref = "owner:7:native-session#" + digest("original-lease")
        modules = {}
        for name in ("agent.credential_persistence", "gateway.config", "hermes_state_local"):
            modules[name] = types.ModuleType(name)
        modules["agent.credential_persistence"].fingerprint_secret_value = digest
        modules["gateway.config"].Platform = types.SimpleNamespace(LOCAL="local")
        modules["hermes_state_local"].local_receipt = lambda db, sid: {"policy": ref}
        scope = {"RuntimeStoreError": RuntimeError, "restore_policy": lambda value: types.SimpleNamespace(credential_ref=value)}
        code = compile(ast.Module(body=functions, type_ignores=[]), path, "exec")
        exec(code, scope)
        session = types.SimpleNamespace(session_id="native-session")
        with patch.dict(sys.modules, modules):
            for held in (None, "original-lease"):
                with self.subTest(owner_restarted=held is None):
                    authority = types.SimpleNamespace(db=None, sessions={session.session_id: types.SimpleNamespace(source=types.SimpleNamespace(platform="local"))})
                    if held is not None:
                        authority._local_launch_keys = {"owner:7:native-session": held}
                    with self.assertRaisesRegex(RuntimeError, "admission_conflict"):
                        scope["rebind_launch_key"](authority, session, "renewed-bridge-lease")
                    scope["rebind_launch_key"](authority, session, "original-lease")
                    self.assertEqual(authority._local_launch_keys["owner:7:native-session"], "original-lease")


if __name__ == "__main__":
    unittest.main()
