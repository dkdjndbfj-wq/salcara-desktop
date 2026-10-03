import { createHostPolicy } from "./host-policy.mjs";
import { createNativeCatalog } from "./native-catalog.mjs";
import { createNativeControl } from "./native-control.mjs";
import { createDesktopConnect } from "./gateway.mjs";
import { createProbe } from "./probe.mjs";
import { createMcpServer } from "./server.mjs";
import { runStdio } from "./stdio.mjs";

// This server has no command-line path or caller identity overrides. Only the
// current host's inherited signals + genuine MCP metadata and explicit approval
// can enable the separately scoped experimental active control call.
const inspectHost = createHostPolicy({ env: {
  CODEX_ELECTRON_RESOURCES_PATH: process.env.CODEX_ELECTRON_RESOURCES_PATH,
  CODEX_APP_TOOLS_PIPE_PATH: process.env.CODEX_APP_TOOLS_PIPE_PATH,
} });
const catalog = createNativeCatalog();
const server = createMcpServer({ probe: createProbe({ inspectHost, catalog }),
  connect: createDesktopConnect({ inspectHost, catalog, nativeCall: createNativeControl() }) });
process.once("SIGINT", () => server.close());
process.once("SIGTERM", () => server.close());

if (process.argv.length !== 2) {
  process.exitCode = 1;
} else {
  process.stdout.on("error", () => { process.exitCode = 1; });
  try { await runStdio({ input: process.stdin, output: process.stdout, server }); }
  catch { process.exitCode = 1; }
}
