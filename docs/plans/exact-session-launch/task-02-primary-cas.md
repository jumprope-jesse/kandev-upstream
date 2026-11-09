---
id: "02-primary-cas"
title: "Baseline-fenced primary promotion"
status: done
wave: 1
depends_on: []
plan: "plan.md"
requirements:
  - REQ-TASKS-EXACT-LAUNCH-005
acceptance_criteria:
  - AC-TASKS-EXACT-LAUNCH-005.2
  - AC-TASKS-EXACT-LAUNCH-005.3
  - AC-TASKS-EXACT-LAUNCH-005.5
  - AC-TASKS-EXACT-LAUNCH-005.6
  - AC-TASKS-EXACT-LAUNCH-005.7
system_design:
  - ../../specs/tasks/system-design/exact-session-launch.md
---

# Task 02: Baseline-fenced primary promotion

## Summary

Add a repository method that promotes one session to primary only if the card's
current primary still equals a stated baseline, and changes only the rows whose
flag changes.

## In scope

- `PromoteSessionPrimaryIfBaseline(taskID, sessionID, baseline)` returning
  `applied`, `already_primary`, or `conflict_not_applied`, on SQLite and Postgres
  (task-row lock on Postgres, as `setSessionPrimary` does).
- Nonterminal-session requirement.
- Verification from the committed state that the session is the only primary
  before returning `applied`.
- Concurrency tests that race two promotions and a concurrent primary change.

## Out of scope

- Changing `setSessionPrimary`, `SetSessionPrimaryIfNonterminal`, or the election at
  insert. Their behavior and the `updated_at` rewrite stay as they are.

## Acceptance

- A changed primary is never overwritten; the result is `conflict_not_applied`.
- Sessions whose flag does not change keep their `updated_at`.
- A terminal session is not promoted.

## Verification

```bash
(cd apps/backend && go test -race -trimpath ./internal/task/repository/sqlite -run 'Primary' -count=1)
(cd apps/backend && go test -race ./internal/persistence/storeconformance -count=1)
(cd apps/backend && go run ./cmd/sqlguard ./internal)
bash scripts/lint-go-changed <changed-go-files>
git diff --check
```

## Files likely touched

- `apps/backend/internal/task/repository/sqlite/session_primary.go`
- `apps/backend/internal/task/repository/sqlite/session_primary_race_contract_test.go`
- `apps/backend/internal/task/repository/sqlite/session_primary_write_test.go`
- `apps/backend/internal/task/repository/interface.go`

## Dependencies

None.

## Risks

- The existing path bumps `updated_at` on every session; copying it would violate
  AC-005.5.
- Postgres lock ordering must match the existing primary writers to avoid deadlock.

## Parallelism

`parallel-safe with 01`

## Inputs

- [Requirements](../../specs/tasks/requirements/exact-session-launch.md), [design](../../specs/tasks/system-design/exact-session-launch.md), [plan](plan.md).
- `session_primary_race_contract_test.go` for the existing race contract.

## Results

Done. Added the compare-and-set primary promotion with activity-marker revalidation at commit; a changed primary is reported as `conflict_not_applied` and never displaced.
