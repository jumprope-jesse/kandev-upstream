---
id: "01-operation-store"
title: "Durable launch operation store"
status: done
wave: 1
depends_on: []
plan: "plan.md"
requirements:
  - REQ-TASKS-EXACT-LAUNCH-003
  - REQ-TASKS-EXACT-LAUNCH-004
acceptance_criteria:
  - AC-TASKS-EXACT-LAUNCH-003.1
  - AC-TASKS-EXACT-LAUNCH-003.2
  - AC-TASKS-EXACT-LAUNCH-003.3
  - AC-TASKS-EXACT-LAUNCH-003.4
  - AC-TASKS-EXACT-LAUNCH-003.5
  - AC-TASKS-EXACT-LAUNCH-004.1
system_design:
  - ../../specs/tasks/system-design/exact-session-launch.md
---

# Task 01: Durable launch operation store

## Summary

Add the `exact_session_launch_operations` table, its repository methods on SQLite
and Postgres, and the canonical request type and digest. Nothing launches yet.

## In scope

- The canonical `ExactSessionLaunchRequest` type, validation (prompt non-empty and
  within `MaxInitialPromptSubmissionBytes`, bounded key length), and the versioned
  canonical digest.
- `CREATE TABLE IF NOT EXISTS` plus an idempotent migration entry. Unique key on
  principal scope, workspace, and idempotency key.
- Repository methods: admit (insert or return existing with digest comparison),
  load by key, load by operation ID, advance phase with compare on the current
  phase, record primary and prompt status, record terminal failure, and list
  non-terminal operations for reconciliation.
- The task-transfer projection catalog entry, workspace-delete and e2e-reset
  cleanup, the `requiredstores` descriptor, and the storeconformance coverage.

## Out of scope

- Any orchestration, plugin, or MCP code. Any change to existing tables.

## Acceptance

- Admission is atomic: concurrent admits with the same key and digest yield one row
  and one operation ID; a different digest yields a conflict and no row change.
- Different principal scopes with the same key do not collide.
- The suite passes on SQLite and, where a disposable Postgres is available, Postgres.

## Verification

Use TDD. Run from the repository root.

```bash
(cd apps/backend && go test -trimpath ./internal/task/repository/... ./internal/persistence/...)
(cd apps/backend && go test -race ./internal/persistence/storeconformance -count=1)
(cd apps/backend && go run ./cmd/sqlguard ./internal)
bash scripts/lint-go-changed <changed-go-files>
git diff --check
```

## Files likely touched

- `apps/backend/internal/task/models/` (new operation model and digest)
- `apps/backend/internal/task/repository/sqlite/base_migrations.go`
- `apps/backend/internal/task/repository/sqlite/` (new `exact_session_launch.go` and tests)
- `apps/backend/internal/task/repository/sqlite/task_transfer_projection_catalog.go`
- `apps/backend/internal/task/repository/interface.go`
- `apps/backend/internal/persistence/requiredstores/`

## Dependencies

None.

## Risks

- Missing one of the new-table registrations fails a completeness test or leaves
  data behind on workspace delete.
- Postgres tests may not be runnable locally. Record that in the results.

## Parallelism

`parallel-safe with 02`

## Inputs

- [Requirements](../../specs/tasks/requirements/exact-session-launch.md), [design](../../specs/tasks/system-design/exact-session-launch.md), [plan](plan.md).
- The `exact_task_command_operations` migration and repository code as the precedent.

## Results

Done. Added the `exact_session_launch_operations` table and repository (admit with frozen input, lease/claim fencing, phase transitions, open-operation listing) with SQLite tests and sqlguard/storeconformance coverage. PostgreSQL behavior is not proven.
