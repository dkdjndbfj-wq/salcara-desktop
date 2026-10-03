import test from "node:test";
import assert from "node:assert/strict";
import { createHistoryCursors } from "../src/history-cursor.mjs";

test("opaque native history cursors are bound to exact target and live gateway", () => {
  const cursors = createHistoryCursors(), token = cursors.encode("codex:target", '{"anchor":"fixture"}');
  assert.equal(cursors.decode("codex:target", token), '{"anchor":"fixture"}');
  assert.equal(cursors.decode("codex:target", undefined), undefined);
  assert.equal(cursors.encode("codex:target", null), null);
  for (const [scope, value] of [["codex:other", token], ["codex:target", token.slice(0, -2) + "--"], ["codex:target", "native-raw"], ["codex:target", ""], ["codex:target", null], ["codex:target", "x".repeat(4097)]]) {
    assert.throws(() => cursors.decode(scope, value), { code: "REQUEST_INVALID" });
  }
  assert.throws(() => createHistoryCursors().decode("codex:target", token), { code: "REQUEST_INVALID" });
  for (const value of ["", "x".repeat(2049), "中".repeat(700), "bad\n", {}, 1]) {
    assert.throws(() => cursors.encode("codex:target", value), { code: "NATIVE_RESPONSE_INVALID" });
  }
});
