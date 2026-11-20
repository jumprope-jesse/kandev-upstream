# PR draft: fix: recover sessions after restart without another restart

Local draft only. Not opened upstream (that is Jesse's decision).
Branch: feature/jb-111-restart-recov-w9m

## Changes
1. Recovery guard release is proof-based, never time-based.
   `Manager.releaseGuardIfOwnerProvenGone` releases the retryable guard (keeping the
   executor record) only when the runtime probe returns `no_matching_instance` for a
   runtime where that proves the process is gone (remote Docker, Sprites), the exact
   owner snapshot still matches, no stop is pending, and no execution is tracked for the
   session. It runs from the normal retry loop. `Manager.ReleaseRemoteRecoveryGuard(ctx,
   sessionID)` is the explicit operator release; it runs the same probe and returns
   `ErrRemoteOwnerNotProvenGone` while the old agent is alive or absence is unprovable
   (including SSH/Kubernetes, where no-match proves nothing; their guard clears through
   attach or the stop path). Not wired to an API or command yet. Dual-agent risk: a
   release without proof could start a second agent; hence no timer.
2. A FAILED session with a resumable executor row (resume token) may now reattach its
   live remote agent (`validateRemoteRecoverySnapshotSession`). Before, it needed a
   pending delivery proof, so the guard stayed pending forever. A later resume of the
   FAILED session goes through the existing stale-execution cleanup, then relaunches with
   the resume token; the guard still refuses it first while the old owner is held.
   Boot cleanup (task IN_PROGRESS to REVIEW repair, open-turn abandon) is unchanged for
   FAILED sessions.
3. Refused recover keeps state: `executor.transitionLaunchFailure` and
   `Service.handleSessionLaunchFailure` skip FAILED and the recovery block for a
   retryable `SessionRecoveryGuardError`. The unstoppable-agent refusal (Retryable=false)
   deliberately still surfaces as FAILED: it needs another restart and an existing test
   (`TestPromptTask_NonRetryableRecoveryGuardRemainsVisible`) pins that.
4. OWNER_MISMATCH on a detached turn: `runSessionDeliveryRecovery` now flags
   `ownerMismatch` when the lifecycle result carries OWNER_MISMATCH, ErrDeliveryOwnerMismatch,
   or a 409 `DeliveryHTTPError` whose body has OWNER_MISMATCH (the evidenced chain is
   `Stale + Blocked + DeliveryHTTPError`). `reconcileSilentRestoreSource` returns
   `errDetachedDeliveryOwnerMismatch`, and the pass settles dispatching submissions to
   interrupted_unknown and abandons open turns. A bare stale-evidence error or a 413
   conflict does not settle. The candidate is still re-listed each minute; the settle is
   idempotent, so the repeat changes nothing.
5. `message_task` into a session refused by the guard (either refusal kind) stays queued
   (start path and prepare path) instead of rolling back.

## Tests
New: `remote_recovery_guard_release_test` (refused while owner alive, released on proof,
unprovable outcomes and SSH refused, retries never release without proof),
`remote_recovery_failed_session_test`, `restart_owner_mismatch_settle_test`,
`restart_owner_mismatch_pass_test` (pass-level, evidenced error, transient variant),
`restart_recovery_sequence_test` (restart with turn in flight, refused manual recover on a
WAITING_FOR_INPUT session with queued messages, peer message; fails without the fix with
state FAILED), `message_task_recovery_guard_test`.

## Verification (Go 1.26.10, golangci-lint 2.9.0, cgo via `zig cc` as CC)
From `apps/backend` with `CGO_ENABLED=1 CC=<zig cc wrapper>`:
- `gofmt -l internal` : clean
- `go vet ./internal/orchestrator/... ./internal/agent/runtime/lifecycle/ ./internal/mcp/handlers/` : clean
- `golangci-lint run ./internal/orchestrator/... ./internal/agent/runtime/lifecycle/... ./internal/mcp/handlers/...` : 0 issues
- `go test ./internal/orchestrator/executor ./internal/orchestrator -count=1` : ok (final commit)
- `go test ./internal/agent/runtime/lifecycle -count=1` : ok (before the last small commit; no lifecycle files changed after)
- `go test ./internal/mcp/handlers -count=1`, `go test ./internal/backendapp -count=1` : ok
  (run before the last commit, which only narrowed the executor/orchestrator state-skip back
  to retryable guard errors; the handlers' targeted `RecoveryGuard` tests were rerun after it)
Not run: `./internal/agent/...` beyond lifecycle, the rest of `./internal/mcp/...`.

## Open
Nothing calls `ReleaseRemoteRecoveryGuard` yet. The Jira upstream-gap note link is not
added (no Jira access from the card).
