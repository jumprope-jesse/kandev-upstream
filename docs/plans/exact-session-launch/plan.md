---
created: 2026-11-09
status: implemented
requirements:
  - REQ-TASKS-EXACT-LAUNCH-001
  - REQ-TASKS-EXACT-LAUNCH-002
  - REQ-TASKS-EXACT-LAUNCH-003
  - REQ-TASKS-EXACT-LAUNCH-004
  - REQ-TASKS-EXACT-LAUNCH-005
  - REQ-TASKS-EXACT-LAUNCH-006
  - REQ-TASKS-EXACT-LAUNCH-007
system_design:
  - ../../specs/tasks/system-design/exact-session-launch.md
legacy_specs: []
---

# Implementation Plan: Exact Fresh-Session Launch

## Overview

Add one native task-domain operation that launches a fresh session on an existing
card with an initial prompt, a caller idempotency key, and optional primary
election. Expose it through the plugin SDK (`LaunchFreshSessionExact`) and MCP
(`spawn_session_kandev` with `idempotency_key`). Follow the
[requirements](../../specs/tasks/requirements/exact-session-launch.md) and the
[system design](../../specs/tasks/system-design/exact-session-launch.md).

## Starting point

- Repository: `jumprope-jesse/kandev-upstream` (fork of `kdlbs/kandev`). All work
  stays in the fork; nothing is opened against upstream until Review and QA pass.
- Base: upstream `main` at `1529e72ed5b8ccd3bc6fd79498a12d57492ec120`
  (tree `08d6b43a011529a01e7e63dfb8f8a027a490d62b`), fast-forwarded into the local
  branch `feature/upstream-exact-launc-2st`.
- The fork's own `origin/main` was 256 commits behind upstream when this package
  was written, and a plain `git fetch origin` does not show that. Re-fetch the
  upstream `main` read-only and re-check the base before implementation.
- The plugin SDK is the separate Go module `pluginsdk/` at the repository root,
  with a `replace` from `apps/backend`. Proto regeneration is `make proto` run
  from `pluginsdk/`.

## Duplicate and prior-art check

Searched 2026-11-09 with the unauthenticated GitHub search API (read-only) and
local code search against `kdlbs/kandev` main. The fork has issues disabled.

- No issue or pull request implements a keyed, prompt-bearing, fresh-session
  launch with primary selection through MCP and the plugin SDK.
- Related context only: pull request #4764 and issue #4477. Neither provides the
  contract here.
- In-tree prior art that this package reuses: `EnsureTaskRunExact` (no prompt, may
  reuse a session, no primary), `spawn_session_kandev` (prompt, no durable key, no
  primary), the exact message client-ID queue, and the initial-prompt submission
  recovery.

Recheck both searches immediately before the upstream PR.

## Scope

### In scope

- The task-domain operation, its table, admission, driver, and restart
  reconciliation.
- Preallocated session identity on the server-only launch request.
- A primary compare-and-swap repository method on SQLite and Postgres.
- The plugin Host command, SDK interface, proto, and adapter.
- The MCP extension.
- Documentation and a disposable-backend QA recipe.

### Out of scope

- The observe hook, any provider implementation, host or live installation, and
  release timer or workflow changes.
- Attachments, session naming, executor, model, or permission overrides.
- Any change to `EnsureTaskRunExact`, live-idle exact messaging, or keyless
  `spawn_session_kandev`.
- A provider-side delivery deduplication handle. Crash window W5 stays an
  explicit, documented gap.
- Merge, release, deploy, or an upstream pull request.

## Work orders and order

| Order | Work order | Depends on |
| --- | --- | --- |
| 1 | [01 Operation store](task-01-operation-store.md) | none |
| 1 | [02 Primary CAS](task-02-primary-cas.md) | none |
| 2 | [03 Orchestrator driver](task-03-orchestrator-driver.md) | 01, 02 |
| 3 | [04 Plugin surface](task-04-plugin-surface.md) | 03 |
| 3 | [05 MCP surface](task-05-mcp-surface.md) | 03 |
| 4 | [06 Docs and QA](task-06-docs-and-qa.md) | 04, 05 |

Work orders 01 and 02 can proceed in parallel. 04 and 05 can proceed in parallel
after 03. Parallel work still needs the user's explicit authorization for any
subagent.

## Verification strategy

Every work order owns its targeted commands. The package-level gate, run in the
foreground with real exit codes after work order 05:

```bash
(cd pluginsdk && make proto && go vet ./... && go test -race ./...)
(cd apps/backend && go test -trimpath ./internal/orchestrator/... ./internal/plugins/... ./internal/backendapp/... ./internal/mcp/... ./internal/task/... ./internal/persistence/...)
(cd apps/backend && go run ./cmd/sqlguard ./internal)
(cd apps/backend && go test -race ./internal/persistence/storeconformance -count=1)
bash scripts/lint-go-changed <changed-go-files>
python3 scripts/list-docs.py validate
python3 scripts/lint-spec-files.py --all
git diff --check
```

Postgres-gated repository tests need a disposable Postgres. Record whether they
ran. If they cannot run, the handoff says so and does not claim dual-dialect
coverage.

## Test matrix

| Criteria | Required evidence | Work order |
| --- | --- | --- |
| 001.1, 001.2 | never-started and ended-session launches create one fresh session | 03 |
| 001.3 to 001.6 | busy, archived, not-found, no side effects | 03 |
| 002.1 to 002.5 | prompt bound, first message, at-most-once, uncertain, trusted context | 03 |
| 003.1 to 003.7 | digest, replay, conflict, concurrent duplicate, scope, lost ACK, config | 01, 03 |
| 004.1 to 004.7 | persisted before session, restart reconcile, retry reconcile, terminal failure, metrics | 01, 03 |
| 005.1 to 005.7 | omitted rule, CAS, race, baseline mismatch, no version bump, already primary | 02, 03 |
| 006.1 to 006.7 | unauthorized, wrong workspace, stale version or claim, archive, pending move, continuity, unchanged commands | 03, 04, 05 |
| 007.1 to 007.6 | MCP and SDK wire equivalence, unsupported host, docs | 04, 05, 06 |

User-facing end-to-end evidence is the disposable-backend QA in work order 06:
a never-started card, an ended session to fresh and primary, and an existing idle
message that stays on the separate path. No production writes and no model spend.
If QA needs authority it does not have, the handoff reports the exact need and
does not substitute fake proof.

## Risks

- W5 crash window: the provider might have received a `dispatching` prompt. The
  system reports uncertainty and does not resend. Not exactly-once.
- Postgres coverage depends on a disposable database being available.
- New table wiring is easy to miss: transfer projection catalog, workspace-delete
  and e2e-reset cleanup, `requiredstores`, and conformance tests.
- The existing `setSessionPrimary` rewrites every session's `updated_at`. The CAS
  must not, and must not alter the established behavior of the old path.
- Two design assumptions (A-3 busy, A-4 activity fencing) need reviewer
  confirmation before they harden into public documentation.
- Adding to the public SDK must not break existing `ExactExecutionCommandManager`
  implementers, hence the separate optional interface.
- The asynchronous start must not be tied to the request context, or a dropped
  connection strands a prepared session.

## Delivery

After all work orders pass, return a source handoff with the exact commit, tree,
and base, the files, the behavior and crash limits, every command with its real
exit code, and the QA evidence. Independent Review and QA follow on the same card
before any upstream pull request. Do not commit, push, or open a pull request
until the user authorizes it.
