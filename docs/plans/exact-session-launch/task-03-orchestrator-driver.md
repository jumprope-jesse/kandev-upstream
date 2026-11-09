---
id: "03-orchestrator-driver"
title: "Exact launch driver and reconciliation"
status: done
wave: 2
depends_on:
  - "01-operation-store"
  - "02-primary-cas"
plan: "plan.md"
requirements:
  - REQ-TASKS-EXACT-LAUNCH-001
  - REQ-TASKS-EXACT-LAUNCH-002
  - REQ-TASKS-EXACT-LAUNCH-003
  - REQ-TASKS-EXACT-LAUNCH-004
  - REQ-TASKS-EXACT-LAUNCH-005
  - REQ-TASKS-EXACT-LAUNCH-006
acceptance_criteria:
  - AC-TASKS-EXACT-LAUNCH-001.1
  - AC-TASKS-EXACT-LAUNCH-001.2
  - AC-TASKS-EXACT-LAUNCH-001.3
  - AC-TASKS-EXACT-LAUNCH-001.4
  - AC-TASKS-EXACT-LAUNCH-001.5
  - AC-TASKS-EXACT-LAUNCH-001.6
  - AC-TASKS-EXACT-LAUNCH-002.1
  - AC-TASKS-EXACT-LAUNCH-002.2
  - AC-TASKS-EXACT-LAUNCH-002.3
  - AC-TASKS-EXACT-LAUNCH-002.4
  - AC-TASKS-EXACT-LAUNCH-002.5
  - AC-TASKS-EXACT-LAUNCH-003.2
  - AC-TASKS-EXACT-LAUNCH-003.3
  - AC-TASKS-EXACT-LAUNCH-003.4
  - AC-TASKS-EXACT-LAUNCH-003.6
  - AC-TASKS-EXACT-LAUNCH-003.7
  - AC-TASKS-EXACT-LAUNCH-004.2
  - AC-TASKS-EXACT-LAUNCH-004.3
  - AC-TASKS-EXACT-LAUNCH-004.4
  - AC-TASKS-EXACT-LAUNCH-004.5
  - AC-TASKS-EXACT-LAUNCH-004.7
  - AC-TASKS-EXACT-LAUNCH-005.1
  - AC-TASKS-EXACT-LAUNCH-005.4
  - AC-TASKS-EXACT-LAUNCH-006.3
  - AC-TASKS-EXACT-LAUNCH-006.4
  - AC-TASKS-EXACT-LAUNCH-006.5
  - AC-TASKS-EXACT-LAUNCH-006.6
system_design:
  - ../../specs/tasks/system-design/exact-session-launch.md
---

# Task 03: Exact launch driver and reconciliation

## Summary

Implement the task-domain operation: admission, the S1 to S5 driver over the
existing two-phase launch and initial-prompt submission, the primary step, failure
handling, restart reconciliation, and metrics.

## In scope

- A server-only `PreallocatedSessionID` on `LaunchSessionRequest` (`json:"-"`) that
  the session insert honors idempotently by primary key.
- Admission order from the design, calling the existing archive, pending-move,
  continuity, ceiling, and busy checks rather than copying them.
- One driver that serves the first attempt, a retry, and restart reconciliation,
  taking the operation row as its only input. It must run under a context that is
  independent of the caller's request.
- Prompt status derived from the submission state. Crash window W5 ends in a
  terminal uncertain status and is never resent.
- Terminal failure after a gate refusal: no session that can start later.
- Primary step using the work order 02 method; omitted make-primary leaves the
  existing election alone.
- Metrics with closed labels and structured logs; startup reconciliation of
  non-terminal operations.
- A test of how an ordinary created-session start composes the first message,
  pinning AC-002.2.

## Out of scope

- Plugin and MCP surfaces. Changing any existing launch gate or the keyless paths.

## Acceptance

- Never-started and ended-session cards each get one fresh session with the prompt
  delivered once and, when requested, the primary elected.
- Concurrent duplicates and a lost-ACK replay return the same session and operation
  with one prompt; a changed digest conflicts.
- Injected failures at each crash window W1 to W8 reconcile as the design states,
  and W5 reports uncertain without resending.

## Verification

```bash
(cd apps/backend && go test -race -trimpath ./internal/orchestrator/... ./internal/task/... -count=1)
(cd apps/backend && go run ./cmd/sqlguard ./internal)
bash scripts/lint-go-changed <changed-go-files>
git diff --check
```

## Files likely touched

- `apps/backend/internal/orchestrator/session_launch.go`
- `apps/backend/internal/orchestrator/initial_submission_recovery.go`
- `apps/backend/internal/orchestrator/task_operations.go`
- `apps/backend/internal/orchestrator/` (new `exact_session_launch*.go` and tests)
- `apps/backend/internal/task/repository/sqlite/session.go` (preallocated insert)
- `apps/backend/internal/task/service/`

## Dependencies

Work orders 01 and 02.

## Risks

- The 80-line function limit pushes the driver into small phase handlers; keep one
  entry point so the three callers cannot diverge.
- A start run under the request context strands a prepared session when the
  connection drops.
- The busy definition (assumption A-3) is a reviewer-confirmed interpretation.

## Parallelism

`sequential`

## Inputs

- [Requirements](../../specs/tasks/requirements/exact-session-launch.md), [design](../../specs/tasks/system-design/exact-session-launch.md), [plan](plan.md).
- [Task launch failure recovery](../../specs/tasks/system-design/task-launch-failure-recovery.md).

## Results

Done. Added `LaunchFreshSessionExact`, the restart reconciler, the ceiling reservation, and the prompt-dispatch driver. A crash during prompt dispatch resolves to `uncertain` and the prompt is never resent.
