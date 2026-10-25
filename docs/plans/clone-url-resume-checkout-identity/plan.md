---
created: 2026-10-25
status: implemented
requirements:
  - REQ-TASKS-ADDITIONAL-SESSION-WORKSPACE-REUSE-001
system_design:
  - ../../specs/tasks/system-design/additional-session-workspace-reuse.md
legacy_specs: []
---

# Clone-URL resume checkout identity

## Outcome

Resume a clone-URL task against the canonical checkout inventory when its
checkout branch differs from its comparison base. Preserve the existing
workspace and all inventory admission checks. This repairs issue #4786.

## Scope and approach

The executor carries the sanitized checkout identity from one unambiguous
task-repository attachment into the single-repository resume request. It
leaves `CheckoutBranch` unset so resume does not request another checkout.
Missing or ambiguous attachments retain the existing behavior. Local executor
requests retain their current branch handling.

The task system owns the canonical workspace identity contract. This repair
implements its existing requirements and design without changing public APIs,
configuration, recovery policy, or user instructions.

## Work orders

- [x] [Task 01: Preserve checkout identity on resume](task-01-preserve-checkout-identity.md)

## Verification

Run focused resume, reuse, and inventory tests. Prove that the regression fails
with the identity assignment disabled. Validate documentation coverage and run
normal commit hooks before pushing.

## Risks

A branch identity must select the existing inventory row without authorizing
new materialization. Tests must retain reuse admission and leave checkout and
clone requests unset. Hosted CI and current-head bot reviews remain delivery
gates after local validation.

## Documentation impact

No public docs change is needed. This repair restores documented workspace
continuity and introduces no new operator action or public contract.

## Results

The focused resume, reuse, and inventory suite passed 371 tests and subtests.
One existing test skipped because it requires a case-insensitive filesystem.
The new regression passed with the race detector for all three executor types.
Disabling the identity assignment reproduced the original mismatch in each case.
Documentation coverage and diff whitespace checks passed. Normal commit hooks
and hosted CI/reviews remain delivery gates for the final commit.
