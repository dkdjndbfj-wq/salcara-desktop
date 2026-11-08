# Windows Agent shutdown audit — 2026-10-14

## Scope and constraints

This audit covers owned Codex / Claude worker shutdown only. It does not change
the UI, source authorization, account pools, station routing, or model execution.
All runtime checks use synthetic local executables, temporary profiles and dummy
configuration. No real Agent, chat history, credential, or paid provider is used.
Nothing was committed, published or deployed.

## Reproduced failures and cause

- An isolated pre-fix run reproduced
  `TestCodexCloseBoundedWithOwnedNotifySinkBarrier` failing after 18.90 seconds
  total fixture time. Its unchanged critical assertion permits 12 seconds for
  `Close`, not including fixture construction.
- `Close` was giving the same non-cooperative event consumer **two independent
  five-second drain budgets**: first the transport, then the startup/request
  wait group. Windows tree termination can consume another three seconds, so
  the intended bounded close exceeded its own 12-second acceptance test.
- The same isolated run timed out at 100 seconds after progressing through most
  of the 32 provider-guard subtests. The current subtest had only been running
  two seconds and the terminator's context watcher was still waiting for its
  deadline. This trace does **not** prove an individual `taskkill` ignored its
  three-second cancellation deadline. A separately owned harmless process also
  measured a normal system terminator exit at approximately 1.96 seconds.
- Thus the earlier six-minute package timeout and the genuine close-budget
  regression must not be conflated. Provider-guard tests repeatedly launch and
  terminate real synthetic Windows processes, and their cumulative time matters.

## Changes

1. `internal/agents/codex.go` and `codex_start_lifecycle.go`: all post-termination
   transport and worker drainage shares one five-second deadline. Late events do
   not regain a connection, and an unconfirmed OS exit is not labelled confirmed.
   Existing 12-second / six-second close assertions have not been increased.
2. `internal/agents/proc_windows.go`: locate `taskkill.exe` using the Windows
   system-directory API. Neither user `PATH` nor an overridden `SystemRoot`
   decides which executable receives the PID. The normal `/T /F /PID` tree
   operation, three-second cancellation, `WaitDelay`, synchronous reaping, and
   exact-owned-process fallback remain in place.
   Additionally, `Process.WithHandle` pins the original process object throughout
   terminator dispatch and reaping, including concurrent `Wait`. Cleanup after
   `Wait`/`Release` never reopens or dispatches by a stale numeric PID. Fallback
   termination uses the pinned original handle. This API was introduced in
   Go 1.26, matching the existing module minimum; `go.mod` is not changed.
3. Replace the old test's PATH-shadowed fake terminator with an explicit local
   command-factory parameter. No mutable package-level command hook is added.
4. Add synthetic regressions covering a shared close deadline, expired deadline,
   trusted terminator resolution, the real cancellation deadline, reaping the
   slow terminator before return, discovery-failure fallback, descendant cleanup,
   preservation of an unrelated synthetic worker, refusal after reaping, and a
   concurrently completed `Wait` without deadlock or premature handle release.
   Existing inherited-pipe
   and unpublished-startup tests remain exercised.

## Verification

- Final focused Windows lifecycle / close / terminator run: **14 tests passed**,
  `49.998s`, including the handle-lifetime regression. The previously failing
  owned-notification-sink case passed at `9.30s` total fixture time; the critical
  12-second assertion is unchanged.
- Pre-handle-pin full agents package: **passed, `279.553s`**; 318 top-level tests passed,
  681 pass records including subtests, two explicit opt-in native-host probes
  skipped, zero failures. The 32-case provider-guard test finished in `81.45s`.
- Pre-handle-pin full agents race run: **passed, `473.889s`**, the same test
  counts and no race report. These earlier full runs do not cover the subsequent
  handle pin or the independently coordinated execution-settings change.
- Final full agents ordinary run: passed in `209.375s` after the coordinated
  settings freeze; agents and test-fixture source hashes remained unchanged.
  The corresponding full race run also **passed, `258.341s`**, with the same
  unchanged source fingerprint and no race report. Each final run has 322
  top-level passes, 687 pass records including subtests, two explicit opt-in
  native-host skips, and zero failures.
  The full package is allotted 15 minutes
  because of the measured cumulative synthetic process-launch/termination cost,
  not by changing any operation's production timeout or its test assertion.

## Boundaries

- No claim is made that arbitrary Windows kernel/driver failures can always be
  interrupted. The current evidence validates ordinary system termination and
  the intentionally sleeping synthetic terminator, not a hostile kernel driver.
- A failed system tree termination retains the existing exact-root fallback;
  the original process-exit fence remains responsible for denying unsafe restart
  when real OS exit cannot be confirmed. This patch does not replace Windows
  process management with a new Job Object implementation.
- Android devices, real official desktop hosts, weak-network dual-station
  handovers and long-running multi-user capacity tests are outside this audit.

## Final verification results

- Final ordinary: `go test ./internal/agents -count=1 -json -timeout=15m`,
  **passed, `209.375s`**. It includes the handle pin and coordinated local/remote
  execution-settings fix. No native-host opt-in was enabled.
- Agents and synthetic-fixture Go source SHA-256 before/after the final ordinary
  and race runs:
  `7017d3639f7d9cf532c8adf6ec7281028aa7081f23221d08cf91cf280e1c1d39`.
- Final race: `go test -race ./internal/agents -count=1 -json -timeout=15m`,
  **passed, `258.341s`**, with 322 top-level / 687 including-subtest passes,
  two native-host opt-in skips, zero failures, zero race warnings.
- Final ordinary has the same 322 / 687 passes and two opt-in skips. The critical
  non-cooperative sink test passed in both runs without increasing its original
  12-second close assertion. The explicit five-second shared-drain tests also
  passed. Timings include no claim about real upstream or device performance.
- All changed files passed the scoped `git diff --check` review; any reported
  LF-to-CRLF notices describe the repository's Windows line-ending conversion,
  not whitespace errors.
