# PR draft: fix: recover sessions after restart without another restart

Local draft only. Not opened upstream (that is Jesse's decision).
Branch: feature/jb-111-restart-recov-w9m (commit 862c67e5f)

## Changes
1. Guard lifetime: remote-pending recovery guard is released after 10 min
   (`remoteRecoveryGuardMaxAge`), keeping the executor record. A pending stop
   keeps its guard. New `Manager.ReleaseRemoteRecoveryGuard(sessionID)` is the
   explicit operator release. Wiring it to an API/command is not done.
2. Refused recover keeps state: `executor.transitionLaunchFailure` and
   `Service.handleSessionLaunchFailure` skip FAILED + recovery block when the
   error is a retryable `SessionRecoveryGuardError`.
3. Boot: FAILED resumable sessions whose remote agent is pending recovery or
   re-tracked skip failed-session cleanup (open turn, executor row kept).
   They stay FAILED; terminal states are sticky, so a later resume finds the
   live agent.
4. OWNER_MISMATCH / evidence conflict in the silent-restore pass settles
   dispatching submissions to interrupted_unknown and abandons open turns.
5. `message_task` into a session refused by the guard stays queued
   (start path and prepare path) instead of rolling back.

## Tests
New: remote_recovery_guard_lifetime_test, restart_recovery_failed_reattach_test,
restart_owner_mismatch_settle_test, message_task_recovery_guard_test.
Targeted runs of lifecycle, orchestrator, mcp/handlers pass.
