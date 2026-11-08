# Remote context references

`AgentCapabilities.contextReferences` is independent of `features`. Only adapters with explicit support and both optional turn methods advertise it. Old or read-only adapters reject a selected reference; dispatch never silently strips it.

`AgentCapabilities.contextAnnotations` additionally gates true message-selection annotations. Codex/Claude implement it; it is published only together with working reference transport. A reference-only Bridge is not sufficient for displaying the true-selection menu. Annotation requests to an adapter without this explicit capability fail before execution, even when no comment was entered.

`session.start`, `session.send`, and `session.continue` accept `contextReferences`. Entries are immutable user-selected snapshots, not filesystem/session capabilities. Bridge never opens `path` or reads/navigates `sessionKey` because it appears in a reference. Cross-tool source labels are rejected.

Each entry has `kind` (`file` or `message`), `label`, `text`, and positive safe-integer `capturedAt`:

- `file`: exactly one source, either the existing native `sessionKey` or `projectPath` for a new task with no native session. `projectPath` is a bounded display source, not filesystem permission. Canonical relative slash-separated `path` and positive `startLine`/`endLine` identify the selected lines. Range length equals the number of snapshot lines. Absolute relative-file paths, traversal, ambiguous segments and Windows path metacharacters are rejected.
- `message`: native source `sessionKey`, exact `messageId`, nonnegative `startOffset`/`endOffset`; `projectPath` is forbidden. The offset difference equals snapshot UTF-16 length, matching JavaScript selection offsets. Task selection is represented by selected message snapshots, not an unbounded task-history import.

True message selections add `textBasis: "source" | "rendered"` and `sourceRevision`, the 64-character lowercase SHA-256 of the canonical source text. `source` offsets locate the original text and forbid `blockIndex`. `rendered` offsets locate text within the rendered block, with required `blockIndex` from 0 through 100000. They must never be reported as raw Markdown offsets. These fields are location/stale-highlight metadata, not authority to read a session. `annotation` is an optional UTF-8 user comment; omitted and explicit empty comments are both supported, including round-trip persistence. Presence of any new field requires the basis and source revision. File references reject all four fields, including empty values. Legacy message references that omit all four fields remain compatible.

Limits: 8 entries, 32 KiB UTF-8 per snapshot, 8 KiB UTF-8 per optional comment, 64 KiB combined snapshot/comment bytes; source/id 256 bytes, label 200 bytes, path 1024 bytes. Unknown fields, null fields, and fields belonging to the other kind are rejected. Snapshot/comment control characters other than tab/CR/LF are rejected. Labels and identifiers reject Unicode control and format characters. No truncation is used.

The original request and the JSON-quoted reference data are separate native text blocks. Within reference JSON, `text` holds the source snapshot and `annotation` holds the user's separate optional comment. The block explains the distinction and that source/location metadata does not grant access or task control. Claude's native skill slash prefix remains in its original first block. Valid reference-only/annotation-only sends are allowed; an ordinary empty message remains invalid.

Selected skills/references require a body at most 64 KiB. All three send commands have a 240 KiB serialized JSON limit, retaining space within the Hub's unchanged 256 KiB command request cap. The phone also checks the complete authenticated envelope, including a potentially large source grant. Oversized input is rejected before execution; references are never shortened to fit.

Codex persists the original body, exact skills and context snapshots before `turn/start`. A random or phone-supplied UUID is sent as `clientUserMessageId`; the exact native echo binds the native item ID for later history reads that omit clientId. Claude sends an SDK user UUID and extends its existing receipt with references. History uses only exact session/UUID or bound native item identity, never equal text or timestamps. Metadata survives a Bridge restart while the local receipt store remains intact. Historical records without a receipt are not guessed or promoted.

Events carry the same `contextReferences`; Hub preserves them. Bridge queue accounting includes all metadata and permits bounded referenced user events up to 1.5 MiB conservative weight, below the 2 MiB upload-body limit. The total queue budget remains unchanged. Hub production body/resource limits are unchanged.

Verification includes strict wire shape, UTF-16 selection, traversal/control characters, byte/count budgets, quote-heavy request rejection, all three send routes without feature support, missing capability rejection, native mock transport, exact restart/history receipts and cross-scope refusal, plus Hub forwarding. Real model execution is not part of these fixtures.

## Project-only file reads before creating a task

`workspace.project.files.v1` is an independent process capability on supported Windows/Linux handle implementations. `workspace.project.files.list` and `.read` accept only string fields `tool`, `cwd`, `expectedCwd`, `controlSurface: "cli"`, `path` and optional `cursor`. The tool must declare CLI read support. The Bridge selects the root from its explicitly saved `Projects` before any filesystem operation; phone paths only assert equality. Subdirectories, history cwd discovery and desktop/read-only fallbacks cannot create permission.

The project reader reuses the existing `os.OpenRoot`, visible-file filters, 128 KiB UTF-8 text cap, paged listings, final-handle checks, bounded concurrency and cancellation. It rechecks pairing, station identity, configured project and exact root handle after IO. It does not list, open or create a native session. Hub treats only these exact methods as paired reads and refuses durable request IDs so private file text does not enter operation receipts.
