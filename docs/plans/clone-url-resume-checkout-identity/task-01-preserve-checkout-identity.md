---
id: "01-preserve-checkout-identity"
title: "Preserve checkout identity on resume"
status: done
wave: 1
depends_on: []
plan: "plan.md"
requirements:
  - REQ-TASKS-ADDITIONAL-SESSION-WORKSPACE-REUSE-001
acceptance_criteria:
  - AC-TASKS-ADDITIONAL-SESSION-WORKSPACE-REUSE-001.1
  - AC-TASKS-ADDITIONAL-SESSION-WORKSPACE-REUSE-001.2
system_design:
  - ../../specs/tasks/system-design/additional-session-workspace-reuse.md
---

# Task 01: Preserve checkout identity on resume

## Outcome

Select the existing checkout inventory during clone-URL resume when base and
checkout branches differ. Preserve attach-only preparation.

## Scope

- Carry checkout identity only for clone-URL executors and one matching attachment.
- Keep missing, ambiguous, and non-clone attachment handling unchanged.
- Test resume preparation through canonical inventory admission and reuse.
- Keep the production comment limited to the invariant it preserves.

## Owned files

- `apps/backend/internal/orchestrator/executor/executor_resume.go`
- `apps/backend/internal/orchestrator/executor/executor_resume_checkout_identity_test.go`
- `apps/backend/internal/orchestrator/executor/executor_resume_checkout_reuse_test.go`
- This work order and its sibling plan.

## Acceptance

1. Docker, remote Docker, and Sprites resume preparation succeeds against the
   existing checkout row when the base is `release/x` and checkout is `feature/y`.
2. The selected environment, workspace path, and checkout inventory remain
   unchanged. Resume retains required reuse and requests neither clone nor checkout.
3. Existing tests retain the no-checkout, ambiguous-attachment, unmatched-repository,
   and local-executor behavior.

## Dependencies and exclusions

No dependency. No UI, persistence, configuration, executor provisioning, or
workspace-repair policy changes are in scope.

## Verification

From `apps/backend`:

```bash
go test -trimpath -tags fts5 -run 'Resume|Reuse|Inventory' ./internal/orchestrator/executor/ -count=1
go test -trimpath -race -tags fts5 -run '^TestPrepareResumeRepositorySettings_ReusesCheckoutBranchInventory$' ./internal/orchestrator/executor/ -count=1
```

Disable only the new identity assignment through a temporary Go overlay. Run
the new regression and require the original inventory mismatch. Restore the
normal source inputs and require a pass.

From the repository root, validate this changed work order and its references
with `.github/scripts/pr-docs.cjs`'s `validateCoverage`, check diff whitespace,
and run normal commit hooks.

## Risks

The branch identifier is selection input. It must not weaken the canonical
inventory guard or request a new checkout.

## Results

- GREEN: the focused resume, reuse, and inventory suite passed 371 tests and
  subtests, with one existing case-insensitive-filesystem skip.
- GREEN: the new resume regression passed with the race detector for all three
  executor types.
- RED mutation proof: removing only the identity assignment through a temporary
  Go overlay made all three cases fail with the original `release-x` inventory
  mismatch. No production behavior changed during review remediation.
- GREEN: local documentation coverage and diff whitespace checks passed.
- Hosted CI and current-head reviews remain externally pending until the
  delivered commit completes them.
