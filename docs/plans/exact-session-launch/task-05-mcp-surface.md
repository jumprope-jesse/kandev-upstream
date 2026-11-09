---
id: "05-mcp-surface"
title: "MCP spawn_session exact path"
status: done
wave: 3
depends_on:
  - "03-orchestrator-driver"
plan: "plan.md"
requirements:
  - REQ-TASKS-EXACT-LAUNCH-006
  - REQ-TASKS-EXACT-LAUNCH-007
acceptance_criteria:
  - AC-TASKS-EXACT-LAUNCH-006.5
  - AC-TASKS-EXACT-LAUNCH-006.7
  - AC-TASKS-EXACT-LAUNCH-007.1
  - AC-TASKS-EXACT-LAUNCH-007.2
  - AC-TASKS-EXACT-LAUNCH-007.3
system_design:
  - ../../specs/tasks/system-design/exact-session-launch.md
---

# Task 05: MCP spawn_session exact path

## Summary

Extend `spawn_session_kandev` with optional `idempotency_key`, `make_primary`, and
`expected_primary_session_id`. The key selects the exact path.

## In scope

- Tool schema and handler changes in `internal/mcp/handlers/spawn_session.go` and
  `internal/mcp/server/server.go`; the WebSocket action `mcp.spawn_session`.
- Build the shared `ExactSessionLaunchRequest`; idempotency scope is the
  authenticated agent identity and the sender comes from the stream, never the
  payload. Keep `SpawnOrigin` and `PreserveWorkflowSourceBinding` attribution.
- Reject, not ignore, fields the exact path does not permit (for example `name`).
- Keyless calls keep their current behavior and tests.

## Out of scope

- Any change to the keyless spawn path. New tools.

## Acceptance

- The same logical request through MCP and the plugin command yields the same
  digest and result fields.
- A keyless call is byte-for-byte unchanged in behavior.
- Unsupported fields with a key return an error before any side effect.

## Verification

```bash
(cd apps/backend && go test -race -trimpath ./internal/mcp/... -count=1)
bash scripts/lint-go-changed <changed-go-files>
git diff --check
```

## Files likely touched

- `apps/backend/internal/mcp/handlers/spawn_session.go` and tests
- `apps/backend/internal/mcp/server/server.go` and tests

## Dependencies

Work order 03.

## Risks

- The MCP tool description is user-facing to agents; keep it short and accurate
  about the primary rule and crash limits.

## Parallelism

`parallel-safe with 04`

## Inputs

- [Requirements](../../specs/tasks/requirements/exact-session-launch.md), [design](../../specs/tasks/system-design/exact-session-launch.md), [plan](plan.md).

## Results

Done. `spawn_session_kandev` accepts `idempotency_key`, `make_primary` and `expected_primary_session_id`; the keyless path is unchanged. Outcomes and refusal reasons match the plugin surface.
