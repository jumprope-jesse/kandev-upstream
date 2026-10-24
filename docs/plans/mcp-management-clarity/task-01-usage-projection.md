---
id: "01-usage-projection"
title: "Authorized catalog usage projection"
status: pending
wave: 1
depends_on: []
plan: "plan.md"
requirements:
  - REQ-AGENTS-MCP-MANAGEMENT-001
acceptance_criteria:
  - AC-AGENTS-MCP-MANAGEMENT-001.1
  - AC-AGENTS-MCP-MANAGEMENT-001.2
  - AC-AGENTS-MCP-MANAGEMENT-001.3
  - AC-AGENTS-MCP-MANAGEMENT-001.4
system_design:
  - ../../specs/agents/system-design/workspace-mcp-management.md
---

# Task 01: Authorized catalog usage projection

## Summary

Expose named direct selections through one bounded, authorized catalog usage
read. Return typed owner identities for navigation without leaking inaccessible
records or reading runtime configuration.

## In scope

- Settings-store enumeration of existing association references with stable
  paging and workspace predicates. No schema migration.
- `mcpconfig` usage service and backendapp owner callback. Validate stored owner
  workspace, caller access, target identity, disabled/archived states, and names.
- Catalog `/usage` route, safe DTOs, frontend client, and failure handling.
  Keep missing names distinct from missing/denied owners. Bound requests and
  cursors as specified by the design. Do not return unfiltered totals.

## Out of scope

Rendered UI, selection writes, delete behavior changes, inheritance expansion,
runtime resolution, probes, secret reads, and sibling features.

## Acceptance

1. Typed responses list each visible direct owner once, including disabled
   definitions, with correct navigation identities and no configuration fields.
2. Cross-workspace, inaccessible, removed, and malformed targets disclose no
   identity. Empty pages, continuation, refresh, and failures follow the design.
3. Focused service/store/handler/client tests pass. Reads cause no writes,
   imports, materialization, provider calls, or new session activity.

## Verification

Run from repository root. Use `/tdd`: first run new tests against the missing
projection, then implement. Existing test names below are supplemented by the
new `CatalogUsage`/`MCPCatalogUsage` tests in the owned files.

```bash
(cd apps && pnpm install --frozen-lockfile)
(cd apps/backend && go test -trimpath ./internal/agent/mcpconfig ./internal/agent/settings/store ./internal/agent/settings/handlers ./internal/backendapp -run 'Test.*(CatalogUsage|MCPCatalogUsage)' -count=1)
(cd apps/backend && go test -trimpath ./internal/agent/mcpconfig ./internal/agent/settings/store ./internal/agent/settings/handlers -run 'Test.*(Selection|MCPCatalog)' -count=1)
(cd apps/web && pnpm exec vitest run lib/api/domains/mcp-api.test.ts)
(cd apps/web && pnpm run typecheck)
bash scripts/lint-go-changed apps/backend/internal/agent/mcpconfig/catalog_usage.go apps/backend/internal/agent/mcpconfig/catalog_usage_test.go apps/backend/internal/agent/settings/store/mcp_usage.go apps/backend/internal/agent/settings/store/mcp_usage_test.go apps/backend/internal/agent/settings/handlers/mcp_catalog_usage_handlers.go apps/backend/internal/agent/settings/handlers/mcp_catalog_usage_handlers_test.go apps/backend/internal/agent/settings/handlers/handlers.go apps/backend/internal/backendapp/mcp_usage.go apps/backend/internal/backendapp/mcp_usage_test.go apps/backend/internal/backendapp/helpers.go
git diff --check
```

Use the existing isolated PostgreSQL fixture for the new usage store cases.
The following command requires an owned fixture's DSN in
`KANDEV_TEST_POSTGRES_DSN`; skipped tests do not count as PostgreSQL evidence.

```bash
(cd apps/backend && go test -trimpath -race ./internal/agent/settings/store -run 'TestPostgresMCPCatalogUsage' -count=1)
```

If implementation changes the file split, update the explicit lint command to
the actual changed Go files before recording results. Do not run broad suites.

## Files likely touched

- `apps/backend/internal/agent/mcpconfig/catalog_usage.go` and `_test.go` (new).
- `apps/backend/internal/agent/settings/store/mcp_usage.go`, `mcp_usage_test.go`
  (new), using `mcp_selections.go` and `postgres_mcp_consistency_test.go` patterns.
- `apps/backend/internal/agent/settings/handlers/mcp_catalog_usage_handlers.go`
  and `_test.go` (new), plus `handlers.go` route/dependency wiring.
- `apps/backend/internal/backendapp/mcp_usage.go` and `_test.go` (new),
  `helpers.go` wiring beside `mcp_selection.go`.
- `apps/web/lib/types/http-mcp.ts`, `lib/api/domains/mcp-api.ts`, and
  `lib/api/domains/mcp-api.test.ts` (new client tests).

## Dependencies

The reconciled main baseline recorded in the plan and a later explicit
implementation request. No sibling
implementation is needed. Scope and API identifiers must match the design.

## Risks

Cross-store lookup failures must not masquerade as an empty authorized list.
Pagination must not expose skipped owner IDs or skip visible entries under a
stable dataset. Deletion impact remains separate from visible usage totals.

## Parallelism

`sequential`

## Inputs

- [Usage requirements](../../specs/agents/requirements/workspace-mcp-management.md).
- [Usage projection and destinations](../../specs/agents/system-design/workspace-mcp-management.md#usage-projection-and-destinations).
- Existing `SelectionImpact`, `mcpSelectionOwnerValidator`, and catalog handlers.
- Root/backend scoped guidance and `/tdd` backend test reference.

## Results

Pending. No implementation tests ran during design.
