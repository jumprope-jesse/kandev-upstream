---
id: "04-plugin-surface"
title: "Plugin Host command and SDK"
status: done
wave: 3
depends_on:
  - "03-orchestrator-driver"
plan: "plan.md"
requirements:
  - REQ-TASKS-EXACT-LAUNCH-003
  - REQ-TASKS-EXACT-LAUNCH-006
  - REQ-TASKS-EXACT-LAUNCH-007
acceptance_criteria:
  - AC-TASKS-EXACT-LAUNCH-003.5
  - AC-TASKS-EXACT-LAUNCH-006.1
  - AC-TASKS-EXACT-LAUNCH-006.2
  - AC-TASKS-EXACT-LAUNCH-006.3
  - AC-TASKS-EXACT-LAUNCH-006.7
  - AC-TASKS-EXACT-LAUNCH-007.1
  - AC-TASKS-EXACT-LAUNCH-007.2
  - AC-TASKS-EXACT-LAUNCH-007.4
  - AC-TASKS-EXACT-LAUNCH-007.6
system_design:
  - ../../specs/tasks/system-design/exact-session-launch.md
---

# Task 04: Plugin Host command and SDK

## Summary

Expose the operation as the exact Host command `LaunchFreshSessionExact` with the
existing `host.v2.write:execution` capability, an optional SDK interface, new proto
messages, and a backend adapter.

## In scope

- Method constant and registry entry in `host_exact.go`; plugin receipt Admit and
  Complete around the native operation; a retry reconciles through the operation
  and not through the uncertain-outcome path.
- `ExactSessionLaunchController` in `exact_execution_contract.go` and its adapter
  in `backendapp/adapters_plugin_execution.go`.
- `pluginsdk` optional interface `ExactSessionLaunchCommandManager` with
  `HostExactSessionLaunchCommands(host)`, gRPC wiring, and proto messages. Run
  `make proto`; never hand edit generated files. An older host returns
  Unimplemented and the SDK fails closed.
- Approval revision, manifest digest, claim fence, workspace, capability, and
  resource version checks identical to the other exact write commands.

## Out of scope

- A new capability or grant. Changes to `EnsureTaskRunExact` or any existing
  interface method set.

## Acceptance

- Missing capability, wrong workspace, stale approval or manifest, stale claim
  fence, and stale resource version each refuse before any session exists.
- Equal logical requests produce the same digest as the MCP surface.
- A plugin built against the SDK fails with an unsupported error against a host
  without the method, with no fallback command.

## Verification

```bash
(cd pluginsdk && make proto && go vet ./... && go test -race ./...)
(cd apps/backend && go test -race -trimpath ./internal/plugins/... ./internal/backendapp/... -count=1)
bash scripts/lint-go-changed <changed-go-files>
git diff --check
```

## Files likely touched

- `pluginsdk/proto/kandev/plugin/v1/plugin.proto` and generated `*.pb.go`
- `pluginsdk/exact_execution.go`, `pluginsdk/exact_execution_grpc.go`, and tests
- `apps/backend/internal/plugins/host_exact.go`
- `apps/backend/internal/plugins/host_exact_execution.go`
- `apps/backend/internal/plugins/exact_execution_contract.go`
- `apps/backend/internal/backendapp/adapters_plugin_execution.go`

## Dependencies

Work order 03.

## Risks

- Adding to a public interface breaks implementers; the separate optional interface
  avoids that.
- Generated proto drift; regenerate with the pinned toolchain only.

## Parallelism

`parallel-safe with 05`

## Inputs

- [Requirements](../../specs/tasks/requirements/exact-session-launch.md), [design](../../specs/tasks/system-design/exact-session-launch.md), [plan](plan.md).
- The `EnsureTaskRunExact` wiring as the template.

## Results

Done. Added the `LaunchFreshSession` RPC, the optional `ExactSessionLaunchCommandManager` SDK interface, host classification, and the plugin adapter that reuses the existing exact command receipts and the `host.v2.write:execution` grant.
