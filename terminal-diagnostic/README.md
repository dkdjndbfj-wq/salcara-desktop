# Windows PTY isolation probe

Copy this folder to `terminal-diagnostic/` on the temporary diagnostic branch. No production source is modified. Run from the repository root:

```powershell
./terminal-diagnostic/terminal-diagnostic.ps1 -RepositoryRoot $PWD -OutputDirectory "$PWD/terminal-diagnostic-output"
```

`workflow-job.yml` is a job snippet for the main thread's temporary workflow. It needs Go 1.27.1 and Windows 2025; no release secrets, publishing or signing steps are used.

The script copies the exact production Windows ConPTY implementation and environment builder into an isolated module, records source SHA256, and compares five variants: baseline, removal of STARTF_USESTDHANDLES suppression, removal of PSReadLine, detached parent console, and normal standard handles with detached parent. Each tests raw CR, raw LF, CRLF, separately written CR, and Win32 Enter sequences.

Each command creates a unique execution marker file and prints the same assembled marker. The marker never appears verbatim in the input. Logs distinguish execution from terminal output and record Windows/PowerShell/PSReadLine versions, language mode, parent handle modes, cursor queries and complete fixture output. The reader continuously drains ConPTY and replies to cursor queries. Every case closes its own terminal Job Object.

Some encodings, especially LF alone, are intentionally expected to fail. The script collects per-variant test exit codes in `summary.json` and exits successfully after all observations are collected. Inspect individual logs; the real Bridge PTY tests remain the release gate.

Optional subset: append `-Variants baseline,normal-std-handles`.
