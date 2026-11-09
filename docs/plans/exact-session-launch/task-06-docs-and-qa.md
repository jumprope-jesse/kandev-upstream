---
id: "06-docs-and-qa"
title: "Documentation and disposable-backend QA"
status: done
wave: 4
depends_on:
  - "04-plugin-surface"
  - "05-mcp-surface"
plan: "plan.md"
requirements:
  - REQ-TASKS-EXACT-LAUNCH-004
  - REQ-TASKS-EXACT-LAUNCH-005
  - REQ-TASKS-EXACT-LAUNCH-007
acceptance_criteria:
  - AC-TASKS-EXACT-LAUNCH-004.6
  - AC-TASKS-EXACT-LAUNCH-005.1
  - AC-TASKS-EXACT-LAUNCH-007.5
system_design:
  - ../../specs/tasks/system-design/exact-session-launch.md
---

# Task 06: Documentation and disposable-backend QA

## Summary

Document the operation on both surfaces and show it working against a disposable
backend, once each for a never-started card, an ended session to a fresh primary
session, and an existing idle message that stays on its separate path.

## In scope

- Public plugin authoring docs, the SDK reference, and the MCP tool description:
  idempotency, the primary rule (omitted = established election), result fields,
  and the crash limits including the unresolved W5 window. No exactly-once claim.
- The observability notes in `CLAUDE.md` for the new metrics.
- A disposable-backend QA recipe using the mock agent profile, a temporary data
  directory, and no production data. Capture the evidence for the three cases.
- Update statuses in the package: work orders `done`; promote the requirement to
  `active`, the design to `current`, and the plan to `implemented` only if the
  implementation matches.

## Out of scope

- Production writes, model spend, host or live installation, merge, release.
- If the QA needs authority that is not available, report the exact need. Do not
  produce fake proof.

## Acceptance

- Docs state each surface's fields, the primary rule, and the crash limits.
- QA evidence shows the three cases with real outputs and exit codes.
- Spec validation passes.

## Verification

```bash
python3 scripts/list-docs.py validate
python3 scripts/lint-spec-files.py --all
(cd apps/web && pnpm run i18n:check)  # only if a web string changed; not expected
git diff --check
```

The QA commands are recorded in the results with their real exit codes.

## Files likely touched

- `docs/public/plugins-authoring.md`
- `docs/public/**` (SDK and MCP reference pages)
- `CLAUDE.md` (observability)
- `docs/specs/tasks/**`, `docs/plans/exact-session-launch/**`

## Dependencies

Work orders 04 and 05.

## Risks

- Docs that overstate delivery guarantees. The unresolved W5 window must be listed.
- QA that touches a shared backend. Use only a disposable one.

## Parallelism

`sequential`

## Inputs

- [Requirements](../../specs/tasks/requirements/exact-session-launch.md), [design](../../specs/tasks/system-design/exact-session-launch.md), [plan](plan.md).
- `/docs-maintainer` guidance in `docs/public/README.md`.

## Results

Done. Public docs, CLAUDE.md observability notes and the design were updated. Disposable mock-only backend QA covered a never-started card, an ended session with a fresh primary, an existing idle message, a replay, a changed-payload conflict, a stale-primary refusal, and a replay after a backend restart.
