# Codex account core

This package is independently implemented for Salcara. It uses the MIT licensed CLIProxyAPI/v7 public SDK at v7.2.157; production browser and device connectors follow that SDK's legacy protocol and public CLI client identity. No application code is copied from Cockpit. It manages **legacy Codex CLI credentials**, plus explicitly marked web sessions for quota lookup. These are distinct from OpenAI's dynamic Sign in with ChatGPT (SIWC) agent OAuth flow and must not be relabelled or sent to a different API backend.

## Public browser contract

`Account`, `ImportPreview`, `LoginRequest`, and `LoginSession` contain no access, refresh, or ID tokens. Their account name, plan, and expiry are display hints, including any decoded JWT claims. JWT signatures are not validated here, and this metadata must never establish identity, entitlement, or account routing. Routing account IDs are copied only from an explicit credential field, never an unverified JWT.

`ParseImport(bytes)` accepts:

- standard Codex `auth.json` with a `tokens` object;
- a CLIProxyAPI `type: "codex"` token record;
- a CLIProxyAPI `provider: "codex"` record with `metadata`;
- a versioned bundle: `{"version":1,"provider":"codex","accounts":[<credential records>]}`;
- a credential array, an `accounts` wrapper, Cockpit managed-account exports, and Sub2API `platform: "openai", type: "oauth"` credential records;
- JSONL / NDJSON and UTF-8 BOM text;
- web Session JSON, a `session` object, or a `session_json` escaped JSON string;
- explicit token aliases (`accessToken`, `at_token`, `atToken`, `personal_access_token`, `token`), plus `Authorization: "Bearer …"` and `ChatGPT-Account-Id` in `headers`, `custom_headers`, or `customHeaders`, with ambiguity checks;
- explicit account/workspace, plan, subscription, and RFC3339 / Unix-second / Unix-millisecond expiry hints.

`ParseTokenImport(bytes, kind)` accepts multiline text or JSON strings/string arrays with an explicit `access_token` or `refresh_token` kind. It never guesses an opaque token's kind. `ParseImportFiles(inputs, format)` applies the same parser to each file while sharing the byte/account budget across the whole selection. The console preserves original file text so duplicate keys cannot disappear in a frontend JSON parse. Preview is local; confirmation saves the entire valid batch atomically, then refresh-only credentials use the existing refresh owner.

Web-session imports are persisted as `chatgpt_web_session` and must contain an access token. They remain available for quota lookup, but are excluded from SDK exports, API execution, Codex refresh-token exchange, and quota-reset consumption. Replacing an API credential with a web session also retires its API associations. Browser/device OAuth replacement can make the account API-eligible again. Agent Identity, password-only drafts, API keys, and other-provider records are rejected by this OAuth importer; it does not silently reinterpret them or drop invalid rows from a mixed batch.

Portable header imports preserve the explicitly selected workspace. Only the two supported credential headers are extracted; arbitrary custom headers never become execution configuration. Repeated credential headers across containers or casing variants are rejected, as are conflicts with canonical fields. OAuth exports may retain null or blank `OPENAI_API_KEY` / `openai_api_key` / `agent_identity` / `agentIdentity` placeholders, but populated or malformed markers reject the record even when OAuth tokens are also present. The same rule covers flat `agent_runtime_id` / `agentRuntimeId` and `agent_private_key` / `agentPrivateKey` fields.

Sub2API exports may repeat `expires_at` as a root Unix timestamp and an inner ISO string. Only this field permits semantic equality across types; the instants must agree exactly, including milliseconds. Identity, plan, email and secret fields retain their strict conflict checks. A `personal_access_token` declaration in any supported `auth_mode`, `authMode`, `openai_auth_mode` or `openaiAuthMode` field requires a nonempty access token and no nonempty refresh or ID token; blank aliases cannot hide a conflicting declaration.

After import submission, the console clears raw credentials and retains only a safe receipt. A saved account with a failed list refresh remains visibly saved; recovery only re-reads the API and vault lists. Malformed or superseded vault responses do not count as a successful refresh. An ambiguous import response remains unconfirmed until the user checks the list, and is never automatically re-submitted.

The caller explicitly supplies the bytes. The package never discovers or reads user auth files. `PreparedImport.Preview()` returns safe display data. The prepared import is opaque to JSON serialization. All input has a 1 MiB bound, token fields have a 64 KiB bound, bundles have at most 64 accounts, JSON nesting/entry counts are bounded, and duplicate keys anywhere in the document are rejected. API key-only auth.json is not an OAuth credential import.

## Service wiring

Construct `New(Options{Path: dedicatedVaultPath, Refresher: backendRefresher, Login: browserConnector, DeviceLogin: deviceConnector, ExclusiveWriter: true})`. The parent directory must be dedicated to the account vault: the store restricts its permissions. Production callers enable `ExclusiveWriter`; this holds an OS-owned sibling lock until `Close`, so a second Bridge process receives `ErrVaultBusy` instead of racing refresh-token rotation. Leave it disabled only for deliberately read-only/in-process fixtures; it does not coordinate with unrelated software that directly reuses the same provider refresh token.

Safe UI operations are `List`, `Get`, `Import`, `Delete`, `Refresh`, `RetrySave`, `StartLogin`, `LoginStatus`, and `CancelLogin`. `Import` commits a bundle as one transaction; replacing an account requires one record and its existing ID. Delete stores a monotonic epoch tombstone. Starting replacement login, replacing credentials, and deletion fence prior login/refresh results. `Close` cancels active sessions and refresh work.

`SetUsageRefreshTargets` maintains quota for explicitly active routes that need quota reserves or quota/plan ordering. The console supplies only current authorized local, remote and named API client sources, deduplicated by vault identity. Importing an unused account or retaining an enabled service setting across restart does not start provider requests. Refreshes share `RefreshUsage`, run at most two background recovery chains, follow the actual snapshot/reset deadline, and back off on failure. Token rotation and cache reset wake maintenance; stale snapshots remain ineligible for quota reserves.

Removing a target cancels its background-only quota read while preserving a coalesced manual request. An already-issued credential rotation finishes under its independent timeout so a provider-consumed refresh token is not lost; it starts no new quota request after target removal. Deletion, identity replacement and service close retain the credential owner's existing fences. Cancelled transports count toward the background limit until they unwind.

Official reset-credit consumption is deliberately separate from `ResetUsage`: `PrepareResetCredit` creates a durable UUID handle, `ResetCreditStatus` is read-only, and `ConsumeResetCredit` is the only operation that calls the pinned upstream consume endpoint. The handle is bound to `IdentityEpoch`, concurrent callers share one provider request, a 401 refresh reuses the same UUID once, and an interrupted or ambiguous request becomes `unknown` so callers must refresh usage before deciding whether to retry. The default consumer is production-only; tests inject a `ResetCreditConsumer` or `http.RoundTripper` and never contact the provider.

`Epoch` increments on every saved credential mutation. `IdentityEpoch` changes only on import/replacement/successful login and stays fixed during refresh-token rotation. The execution gateway must match its configured identity epoch to the backend SDK snapshot's `Attributes["salcara_identity_epoch"]` immediately before execution. This rejects a replaced account without rejecting normal token refresh. Older vaults missing identity epoch migrate it from their saved credential epoch.

`SDKRecord(ctx, id)` and `SDKStore().List(ctx)` are **trusted backend-only** snapshots containing secrets. They must never be serialized, logged, or sent to the UI. The snapshot store rejects `Save`/`Delete`, so the SDK cannot become a competing mutation owner. The execution gateway must pin the selected account, disable the SDK's independent refresh/persistence paths, and obtain a new snapshot after this service refreshes; execution/gateway policy is outside this package.

`NewSDKRefresher(manager)` calls `manager.Executor("codex").Refresh` directly. The caller installs the SDK executor first. Tests inject a `RefreshFunc` and use a local httptest endpoint; there are no real paid provider requests.

## Protection and refresh semantics

Windows production builds use current-user DPAPI with interactive prompts disabled and a protected ACL allowing only the current user and SYSTEM. Portable builds use directory mode 0700 and file mode 0600; they reject a vault readable by other users. Portable data is protected by filesystem permissions, not encryption. Inject `Protector` when encryption/keychain protection is needed on other systems or when testing.

All mutations encode and protect the next vault, write a private temporary file, sync it, and atomically replace the current vault. Windows uses MoveFileEx with replacement and write-through; other systems rename and sync the parent directory. Memory changes only after successful persistence. No plaintext temporary credential files are written on Windows.

Refreshes are coalesced per account. Cancellation of one waiter leaves other waiters active. `CancelRefresh`, replacement, deletion, and `Close` cancel the shared work and fence a connector that ignores cancellation. Provider refresh failure keeps the previous access token, surfaces `refresh_failed` or `needs_login`, and returns only fixed sanitized errors. The upstream remains responsible for validating an access token. A valid previous access token can still be obtained while refresh needs login. When the provider rejects a refresh token as invalid, or rotation resolves to another provider account, the service atomically clears that refresh token, records `needs_login`, and advances `Epoch` without changing `IdentityEpoch`. A normal restart therefore cannot retry the rejected credential.

If an upstream rotates its refresh token but local persistence fails, the new credential stays private in memory, status becomes `save_failed`, and further refresh calls are refused. The same fail-closed state applies when persisting invalid-token retirement fails: the current process immediately removes the rejected refresh token from SDK snapshots and refuses another refresh. `RetrySave` atomically saves either pending mutation. Restart/close loses an unsaved pending mutation; a failed disk write cannot make its retirement durable. The UI should keep the process open and retry the save, or require login again after a forced restart rather than claiming the on-disk credential was repaired.

## Login and cancellation limits

Production registers two independently implemented connectors following the pinned MIT CLIProxyAPI protocol. `StartLogin` accepts `mode: "browser"` (also the empty default) or `mode: "device"`; unknown or unavailable modes fail without silently substituting another flow. The connector is fixed for each session. Supply a session lifetime context, not an HTTP request context that ends after the response.

`NewBrowserConnector` uses the pinned legacy Codex CLI client, PKCE S256, a random state, and a loopback-only callback listener. It publishes the same authorization URL used to open the browser. `SubmitLoginCallback` accepts a complete callback URL for only that pending browser session and applies the listener's exact redirect/state/one-shot checks without requesting the pasted URL. It never accepts a bare code or another session's callback.

Production binds `127.0.0.1:1455`, falling back to `127.0.0.1:1457` if unavailable. It retains the bound listener and uses its matching localhost redirect for authorization, manual callback validation, and token exchange. If neither port is available, the session exposes only `errorCode: "callback_unavailable"`; the UI offers device authorization. Fixtures never fall back to production ports implicitly. Accepted HTTP callbacks receive up to one second, bounded by the login context, to finish their response before connection closure.

`NewLegacyDeviceConnector` uses the same pinned client identity and fixed OpenAI device-code, polling, and exchange endpoints. Progress contains only the verification URL, user code, and expiry; device session IDs and provider tokens remain private. The console can copy/open the URL and copy the code. A device session cannot accept a browser callback. `NewDeviceConnector` remains available for trusted synthetic endpoint/client fixtures; it requires an explicit client ID.

Both production connectors use private HTTP transports without environment proxies or shared cookies, bounded timeouts and response sizes, strict successful JSON parsing, and no redirects. Provider-supplied account IDs, including IDs in the authenticated token exchange's ID token, can identify the upstream route; imported JWT hints do not establish entitlement. Raw provider errors never reach the UI.

Cancellation clears the visible challenge immediately and fences credentials from late work; `CancelLogin` then allows up to two seconds for the connector to drain. Browser sessions serialize listener ownership, so retry cannot overlap a still-closing callback port. `Close` includes all live login workers in its bounded shutdown join before releasing the vault lease. Cancelled workers still draining count toward the bounded session limit. The retained `SDKLoginConnector` compatibility adapter is not the production login path.

The browser authorization timeout includes waiting for listener ownership, and the displayed expiry uses the actual context deadline. Explicit cancellation remains `cancelled`; a connector or parent deadline produces `failed` with the fixed `errorCode: "expired"`, including deadlines while reading a token response. The browser checks its context even when a late response ends with clean EOF. Terminal sessions clear the visible challenge and never save credentials returned after cancellation or expiry.

The console follows cancellation with a status read: completion may already have committed an account, so a successful cancel request alone does not imply `cancelled`. The status endpoint retains its identity check and account association. Login completion refreshes are scoped to the poll revision and panel lifetime; failed list reads retry without a new authorization, and old refreshes cannot close or cancel a newer session.

Closing the dialog and receiving a late start response use a detached cancel/status reconciliation as well. Only a completed status whose account passes the existing identity check can trigger a list refresh. Temporary failures receive at most two retries; missing or replaced identities stop immediately. Detached reconciliation never writes a new dialog's state or timer, and list revision checks prevent a late response from restoring a deleted card.

## Verification

Tests use synthetic credentials only and cover import formats, duplicate keys and bounds, unverified metadata separation, token-free DTOs, protected atomic replacement, persistent deletion epochs, local HTTP refresh coalescing, rotation before publication, failed-save recovery, durable invalid-token retirement across restart, old access retention, and cancellation/delete/relogin fencing. No actual user auth files are used.
