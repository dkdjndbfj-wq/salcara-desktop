# Agent browser navigation on the phone

The Bridge adds optional `browser` metadata to an existing completed `tool`
timeline event. Its normal ID, session, output, tool kind and ordering remain
unchanged. The Hub preserves the metadata in live events and buffered replay.

```json
{
  "type": "tool",
  "sessionKey": "codex:example",
  "id": "native-tool-call-id",
  "status": "done",
  "browser": {
    "action": "navigate",
    "url": "https://example.test/docs?q=api#setup",
    "pageId": "playwright:tab-1"
  }
}
```

`action` is `open` or `navigate`. `url` is the explicit destination from the
successful tool's input, preserved with its query and fragment. It is limited
to 4096 UTF-8 bytes and absolute HTTP(S); credentials, whitespace, control and
format characters, backslashes, malformed escapes, hosts and ports are rejected.
The adapter does not fetch the URL. It does not transfer the desktop browser's
cookies, login, DOM, local server or visual state to the phone.

Supported native interfaces:

| Tool | Mapping |
| --- | --- |
| Playwright `browser_navigate` | `navigate` with input `url` |
| Puppeteer `puppeteer_navigate` | `navigate` with input `url` |
| `chrome-devtools` / `chrome_devtools` `new_page` | `open` with input `url` |
| `chrome-devtools` / `chrome_devtools` `navigate_page` | `navigate` for URL navigation only |
| `codex_app.open_in_codex` | `open` for `target.type = browser`, or `navigate` when `target.tabId` is supplied |

Codex `mcpToolCall` must be completed successfully. `dynamicToolCall` also needs
native `success: true`. The same pure projection runs on app-server adapter
events and on explicitly authorized `desktop.session.read` history items; it
does not expand the desktop lease or discover additional tasks. Claude
`tool_result` must match a recorded `tool_use`
and must not report `is_error`. MCP result `isError: true`, nested structured
error results and JSON result envelopes serialized as text veto the metadata.
Running, rejected, cancelled, failed, unowned and unknown calls remain normal
activities. Claude names `mcp__server__tool` and Codex native server/namespace
fields are recognized. No URL is inferred from chat text, web search, WebFetch,
shell commands, arbitrary JavaScript, page-click tools or human-readable output.

An explicit `threadId` on `open_in_codex` is conservatively excluded, including
when it could refer to the current chat. Non-browser targets and existing tabs
opened without a URL are excluded. Browser back/forward/reload actions have no
explicit new destination and are not mapped.

The optional `pageId` is an opaque identity, constructed as
`PathEscape(server) + ":" + PathEscape(nativeId)` and limited to 256 bytes.
Native `tab_id`, `page_id`, `tabId` and `pageId` strings or nonnegative safe
integers are accepted from the input target or structured result. Conflicting,
invalid or oversized IDs suppress the metadata. Server namespaces are retained
so two browser servers' tab 1 cannot overwrite each other. Without a server,
the exact supported tool name is the namespace. If the native tool exposes no
page identity, `pageId` is omitted; page lists and prose are never scraped to
invent one. Consumers should scope these opaque identities by computer and
conversation and deduplicate event replay by the existing tool event identity.

This is additive display metadata. Older clients ignore it. A phone renders
the link inside its own browser UI and keeps its ordinary external-link fallback
when a site disallows embedding; it must not present this as a mirrored or
authenticated desktop browser session.
