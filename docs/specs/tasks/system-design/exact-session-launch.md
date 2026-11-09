---
status: current
system: tasks
requirements:
  - REQ-TASKS-EXACT-LAUNCH-001
  - REQ-TASKS-EXACT-LAUNCH-002
  - REQ-TASKS-EXACT-LAUNCH-003
  - REQ-TASKS-EXACT-LAUNCH-004
  - REQ-TASKS-EXACT-LAUNCH-005
  - REQ-TASKS-EXACT-LAUNCH-006
  - REQ-TASKS-EXACT-LAUNCH-007
created: 2026-11-09
owners:
  - kandev
---

# Exact Fresh-Session Launch System Design

## Context and boundaries

The tasks system owns one native operation: launch one fresh session on an
existing card, deliver one initial prompt, and optionally elect that session as
primary, all under a caller idempotency key. The plugin Host command and the MCP
tool are two thin surfaces over that operation. Neither surface owns launch
truth.

The design reuses, and does not replace, these existing mechanisms:

- The two-phase launch in `internal/orchestrator/session_launch.go`
  (`LaunchSession` with `IntentPrepare` and `DeferredStart`, then
  `IntentStartCreated`) and its launch gates.
- The initial-prompt submission state machine in
  `internal/task/models/initial_prompt_submission.go` and
  `internal/orchestrator/initial_submission_recovery.go`
  (`pending`, `dispatching`, `accepted`, `replay_blocked`, and
  `ErrInitialSubmissionAcceptanceUncertain`).
- The plugin exact command receipts in `internal/plugins/state/command_store.go`
  (admit, effect, complete) and the method registry in
  `internal/plugins/host_exact.go`. These remain the plugin-side receipt.
- The native transactional idempotency precedent `exact_task_command_operations`.
- The install-wide host-resource activity coordinator for host admission.

Boundary decisions:

- Task-domain operation record, not a new receipt system. A receipt that is
  `accepted` but not `completed` yields only `operation_outcome_uncertain` today,
  with no way to learn what happened. A launch cannot be recovered from that.
  The native operation row owns launch truth, so a plugin retry reconciles from
  it.
- No new capability. The plugin method reuses `host.v2.write:execution`.
- No payload actor. The principal comes from the authenticated plugin
  installation or the connected agent stream.

## Requirement mapping

| Requirement | Design section |
| --- | --- |
| REQ-TASKS-EXACT-LAUNCH-001 | Admission, Control flow |
| REQ-TASKS-EXACT-LAUNCH-002 | Initial prompt, Control flow |
| REQ-TASKS-EXACT-LAUNCH-003 | Canonical request, Operation record |
| REQ-TASKS-EXACT-LAUNCH-004 | Operation record, Control flow, Crash windows, Observability |
| REQ-TASKS-EXACT-LAUNCH-005 | Primary selection |
| REQ-TASKS-EXACT-LAUNCH-006 | Admission, Authority and fences |
| REQ-TASKS-EXACT-LAUNCH-007 | Plugin surface, MCP surface, Canonical request |

## Canonical request

One Go type in the task domain, `ExactSessionLaunchRequest`, carries the logical
request. Both surfaces build it and neither computes its own digest.

| Field | Notes |
| --- | --- |
| `WorkspaceID`, `TaskID` | Required. A mismatch is not-found. |
| `IdempotencyKey` | Required, bounded length, opaque. |
| `Prompt` | Required, non-empty after the existing validation, at most `MaxInitialPromptSubmissionBytes`. |
| `AgentProfileID` | Optional. Empty selects the ordinary resolution for the card. |
| `MakePrimary` | Boolean, default false. |
| `ExpectedPrimarySessionID` | Optional pointer. Absent means the host captures the baseline at admission. Present and empty means the caller expects no primary. |
| `ExpectedTaskResourceVersion` | Plugin surface only. Checked at admission, not part of the digest. |

The digest is the sha256 of a versioned canonical JSON encoding of the digest
fields: workspace, task, prompt, profile, make-primary, and the caller-asserted
baseline when present. The resource version, the claim fence, and the approval
data are admission preconditions. They are not part of the occurrence identity,
so a retry after a legitimate version bump still replays the same occurrence.
A retry with a different digest conflicts (AC-003.3).

The plugin `CommandStore` digest wraps this value with the method name, as for
other exact commands. The native digest is computed from the same canonical
encoding, so equal logical requests agree across surfaces (AC-007.2).

## Operation record

New table `exact_session_launch_operations`, task domain, SQLite and Postgres.
New tables use `CREATE TABLE IF NOT EXISTS` plus an idempotent migration entry.
No foreign key, matching the `exact_task_command_operations` precedent.

| Column | Purpose |
| --- | --- |
| `operation_id` | Primary key, generated at admission. |
| `principal_scope`, `workspace_id`, `idempotency_key` | Unique together. The scope is the authenticated principal: a plugin installation identity, or an agent identity for MCP. |
| `request_digest` | Canonical digest. |
| `task_id` | Target card. |
| `session_id` | Preallocated at admission. |
| `frozen_input` | Versioned JSON of the validated launch input: the prompt, and the resolved agent profile, executor, executor profile, and workflow step the launch will use. Written in the admission transaction. |
| `baseline_primary_session_id`, `baseline_asserted`, `baseline_activity_marker` | Baseline primary and the activity marker recorded for it at admission. |
| `make_primary` | Request flag. |
| `phase` | `admitted`, `prepared`, `prompt_dispatching`, `prompt_accepted`, `completed`, `failed`, `uncertain`. The last three are terminal. |
| `primary_status`, `primary_reason` | `not_requested`, `pending`, `applied`, `already_primary`, `conflict_not_applied`; reason `primary_changed`, `baseline_active`, `session_terminal`, or `unverified`. |
| `prompt_status` | Cached: `pending`, `accepted`, `uncertain`, `not_delivered`. |
| `error_code`, `error_message` | Terminal failure class and a bounded message. |
| `owner_token`, `owner_epoch`, `lease_expires_at` | Single-owner claim, see Ownership and fencing. |
| `created_at`, `updated_at`, `completed_at` | Timestamps. |

**Frozen input.** The row holds the digest and also everything a driver needs to
finish the launch without the original HTTP payload or the in-process request:
the canonical validated prompt and the final effective launch configuration.
Admission resolves the configuration once, with the same resolution an ordinary
prepare performs, including the workflow step override. It then stores the result.
The driver never re-resolves profile, executor, or defaults after admission, so a
default that changes between a crash and the restart cannot change the launch
(AC-004.8). The prompt is stored in the same plaintext form as the session
metadata `initial_prompt_submission` that already carries it, so no new
sensitivity class is introduced. Logs and metrics never carry it. The row is
deleted with the task through the transfer catalog and workspace delete.

The session identifier is not duplicated as truth for the prompt state. The
session row holds its initial-prompt submission, which stays authoritative.
`prompt_status` is a cache refreshed from the submission at each transition and
corrected at reconciliation.

Admission is one transaction that inserts the row if the unique key is absent. On
PostgreSQL it first takes `pg_advisory_xact_lock(hashtextextended(namespace +
principal scope + workspace + key, 0))`, so concurrent callers with one key
serialize before the first read; the unique constraint remains defense in depth.
When the key exists, the digest is compared: equal returns the stored operation
and different returns a conflict. The row, with its frozen input, exists before
any session is created (AC-004.1).

Required wiring for a new table: an entry in both lists of the task-transfer
projection catalog (`task_transfer_projection_catalog.go`), workspace-delete and
e2e-reset cleanup where completeness tests require it, a `requiredstores`
descriptor, and Postgres-gated repository tests.

## Admission

Admission runs the refusals before the operation row can lead to a session.

1. Authenticate and scope. Plugin: capability, workspace, approval revision,
   manifest digest, and the management claim fence (`withManagementClaimFence`).
   MCP: the sender comes from the agent stream and workspace scoping applies as
   for other task tools. These checks run on every request, including a replay.
2. Resolve the card in the stated workspace. Absent returns not-found with no
   cross-workspace signal (AC-001.5).
3. Refuse archived cards (AC-001.4).
4. Check the observed resource version on the plugin surface for a new
   operation only (AC-006.3). A replay of a known operation does not reapply the
   historical precondition, but steps 1 to 3 still run (AC-003.2, AC-006.8).
5. Check pending workflow moves, continuity or recovery blocks, Office-task
   ownership, and the workflow-session ceiling through the same functions the
   ordinary launch calls (AC-006.4). The ceiling is consulted as an automatic
   launch, and a refusal fails the operation. It never leaves a deferred launch
   record that could start the session later.
6. Resolve and freeze the launch configuration (see Operation record).
7. In one repository transaction, on Postgres after locking the task row:
   a. Refuse when another non-terminal operation exists for the same card.
   b. Refuse when the card is busy (AC-001.3, below).
   c. Capture the baseline primary, or compare the caller-asserted baseline and
      refuse on mismatch (AC-005.4), and record the baseline activity marker.
   d. Insert the operation row with the preallocated session ID.

**Busy definition.** The card is busy when any of its sessions satisfies any of:

- state `CREATED`, `STARTING`, or `RUNNING`;
- an open turn (`task_session_turns.completed_at IS NULL`), read fail-closed;
- a non-terminal background work or background run row;
- known live foreground or subagent activity in the orchestrator's in-memory
  `turnActivity`, or a liveness lookup that fails or cannot establish absence.

The durable conditions are evaluated inside the admission transaction. The
in-memory signal can only add a refusal, never remove one, because it is best
effort and never proves absence. A lookup error is treated as busy and is
retryable. A session in `IDLE` or `WAITING_FOR_INPUT` with none of the above is
idle and does not block a fresh launch. Terminal sessions are not considered.
This authorizes only safe fresh launches: the operation never interrupts,
stops, or messages active work.

Host-resource admission remains inside the ordinary prepare and start path and
is not re-implemented. A refusal there surfaces as a terminal failure for the
operation.

## Initial prompt

The launch uses the existing submission machinery. The preparation creates the
session with a `pending` `InitialPromptSubmission` holding the caller prompt.
Start callbacks (`beforeProviderAdmission`, `onInitialPromptAccepted`) update
the operation phase. The caller prompt is the only user-authored text.
`wrapCreatedSessionPrompt` still adds server-composed trusted context around it,
and the design does not change that. The recorded first user message is the
caller prompt (AC-002.2, AC-002.5).

The exact path does not append the card description or any other user text.
Implementation must verify, in a test, how an ordinary created-session start
composes the first message, and keep the exact path's behavior to the AC wording.

The caller prompt is recorded as the first user message in
`beforeProviderAdmission`, after the operation is durably `prompt_dispatched`
and before the session receipt becomes `dispatching`. The write is idempotent by
the deterministic initial-submission message ID. If it fails, admission is
refused and nothing is sent. The instruction is therefore on record ahead of any
agent output, and an uncertain dispatch still has it. Recovery and completion
never write the message. When provider admission later refuses the prompt, the
message is flagged `not_delivered` before the operation closes as failed. A
failed flag write keeps the operation open, and a later reconciliation settles
it as `uncertain`.

The session receipt becomes `accepted` in `onInitialPromptAccepted`. If that
write fails, the operation still advances to `prompt_accepted`, because the
provider acceptance is the fact. Completion re-asserts the receipt (`dispatching`
becomes `accepted` under the recorded execution) and refuses to complete over any
other state, so the two receipts cannot disagree silently. An `uncertain`
operation leaves the receipt `dispatching`; no accepted receipt is manufactured.

Delivery is at most once per operation. A `dispatching` submission that was not
resolved when the process died is never resent (AC-002.3, AC-002.4). See
Crash windows.

## Control flow

```text
S1 admit      -> operation(admitted), session_id reserved
S2 prepare    -> LaunchSession{IntentPrepare, DeferredStart, session_id, pending submission}
                 operation(prepared)
S3 start      -> LaunchSession{IntentStartCreated, session_id, prompt}
                 operation(prompt_dispatching) -> operation(prompt_accepted)
S4 primary    -> CAS promote, only if make_primary
S5 complete   -> operation(completed) -> plugin receipt Complete
```

Every step reads only the operation row (including the frozen input), the session
row, and the submission. A step never reads the original request. The prepare
step is `PrepareTaskSession` split into resolve and prepare-from-resolved: the
ordinary entry point composes both and is unchanged, and the exact path calls
only the second half with the frozen values. The start step passes the frozen
profile so the start path does not re-run workflow-step profile resolution
(AC-004.8).

Preallocation needs a server-only field on `LaunchSessionRequest`
(`PreallocatedSessionID`, tagged `json:"-"`) that the session insert honors. The
insert is idempotent by primary key, so a repeated prepare cannot create a second
session. No surface populates this field from external input.

Execution of S2 to S5 runs under the operation, not under the HTTP request, so a
dropped connection does not abandon a prepared session. S3 follows the existing
asynchronous pattern in `prepareStartAgentSession`
(`internal/task/handlers/task_http_handlers.go`). The calling surface waits for a
bounded time and returns either the completed outcome or an in-progress status
with the operation ID (AC-003.4). A caller retry with the same key observes the
same operation.

A gate refusal after S2 ends in terminal failure (AC-004.5). The failure path
reuses the semantics of
[Task launch failure recovery](task-launch-failure-recovery.md): the prepared
session is moved to a terminal state so that nothing can start it later, and the
operation records `failed` with the error class. A retry returns that same
failure and does not create a second session.

Success is reported only when the session exists and the submission is
`accepted` (AC-004.4). The result always names the prompt status and the primary
status, so a partial result is distinguishable.

## Primary selection

Omitted make-primary keeps the established rule: the session becomes primary at
creation only when the card has no primary, through the existing election in the
insert path (`electInitialSessionPrimaryTx`). The exact path adds nothing there
(AC-005.1). The documentation states the rule.

When make-primary is requested, S4 runs after the prompt is accepted. A new
repository method, `PromoteSessionPrimaryIfBaseline`, takes the card, the fresh
session, the baseline session, and the baseline activity marker. It runs in one
transaction (with the task-row lock on Postgres, as `setSessionPrimary` does):

1. Load the fresh session. It must be nonterminal, else `conflict_not_applied`
   with reason `session_terminal` (AC-005.5).
2. Read the current primaries.
3. If the fresh session is already the only primary, return `already_primary`
   (AC-005.6).
4. If the primaries are not exactly the baseline, change nothing and return
   `conflict_not_applied` with reason `primary_changed` (AC-005.3).
5. If a baseline exists, lock it and recompute its activity with the busy
   definition's durable conditions. If it is in `CREATED`, `STARTING`, or
   `RUNNING`, has an open turn, has open background work, or its marker differs
   from the recorded one, change nothing and return `conflict_not_applied` with
   reason `baseline_active` (AC-005.8).
6. Otherwise clear the baseline's flag and set the fresh session's flag. Update
   only rows whose flag changes, so no `updated_at` or version change reaches
   other sessions (AC-005.5). This differs from `setSessionPrimary`, which
   rewrites every session of the task.
7. Return `applied` only after the committed read shows the session as the sole
   primary (AC-005.7).

Primary-ID equality alone does not establish idleness. The marker (the baseline
session's turn count and background row count at admission) detects a turn that
began and ended, or work that began and ended, between admission and commit.
The orchestrator also checks the in-memory activity signal immediately before
S4. Two races are documented:

- Admission race: two operations for the same card cannot both pass step 7,
  because the non-terminal-operation check and the busy check share a transaction
  with the insert, and Postgres locks the task row first. A session launched by
  an unrelated path between the orchestrator pre-checks and the transaction is
  seen by the durable busy check.
- Commit race: a baseline turn that starts after the in-memory check and before
  the promote transaction is visible to the transaction as `RUNNING`, an open
  turn, or a changed marker, and yields `baseline_active`. The remaining
  window is a turn begun in memory but not yet persisted when the transaction
  reads. The design accepts that residual and reports it in the documentation.
  The cost is a primary change next to a just-started turn, not lost work or an
  interrupted turn.

The result of S4 is stored in `primary_status` and `primary_reason`. A conflict
does not fail the session or prompt, and is never reported as plain success: the
overall outcome is `partial` (AC-005.3, AC-005.9). Callers read
`primary_status`.

## Authority and fences

- Authentication, per-user workspace scoping, and capability approval are the
  existing code paths and are unchanged (AC-006.1, AC-006.2).
- The actor is the authenticated principal. The plugin installation identity or
  the agent stream sets it. The request payload has no actor, sender, or origin
  field. The MCP exact path does not apply the keyless `SpawnOrigin` spawner
  attribution: the initial prompt is delivered verbatim as the first user
  instruction (AC-002.2), and attribution text prepended to it would change the
  digest-bound prompt and make the first turn differ between a first attempt and
  a retry from another session (AC-006.5).
- Execution ownership, `QueueIncarnationID` and message queue
  `LifecycleGeneration` checks, and host admission stay in the ordinary start
  path (AC-006.6).
- Primary changes are fenced by the baseline compare, the baseline activity
  revalidation, and the nonterminal check in the same transaction as the write.
  A replay still runs authentication, workspace scoping, capability, claim
  fence, approval, and manifest checks even though a historical resource version
  is not reapplied (AC-006.8).
- `EnsureTaskRunExact`, live-idle exact messaging, and keyless
  `spawn_session_kandev` keep their code paths and tests (AC-006.7).

## Ownership and fencing

Phase compare-and-set alone cannot stop two workers from both dispatching the
same step, so each operation has a single-owner lease.

- `owner_token` names a worker, `owner_epoch` increases on every takeover, and
  `lease_expires_at` bounds a claim. The lease uses a monotonically increasing
  epoch and not a wall clock for ordering.
- Claim: read the row. Refuse when it is terminal, or held by another owner with
  an unexpired lease. Otherwise update `owner_token`, increment `owner_epoch`, and
  set the lease, with `WHERE operation_id = ? AND owner_epoch = <observed>`.
  Exactly one concurrent claimant wins.
- Every phase transition and status write carries
  `WHERE operation_id = ? AND owner_token = ? AND owner_epoch = ? AND phase = <from>`.
  An old owner that lost its lease after a takeover matches zero rows and stops.
- Write-ahead: the transition that precedes an effect commits before the effect
  runs. The effect itself keeps its own backstops: the session insert is
  idempotent by primary key, the submission `pending` to `dispatching` change is
  a single-winner compare-and-set, and the session lifecycle lock still
  serializes starts.
- The owning worker renews the lease while it works. Work runs under a context
  derived from a shutdown-owned root, bounded by the existing launch timeout,
  not under the caller's request context. Shutdown cancels it and waits.
- Takeover is safe only for a worker whose lease expired. A takeover resumes by
  the Crash windows table, so it never repeats an effect whose durable mark
  already exists.
- Callers that find a live owner in the same process wait on that operation's
  done channel for a bounded time and then return `in_progress` with the
  operation ID. Callers in another process do the same by polling the row.
- The startup reconciler and a periodic sweep claim through the same function, so
  a restart reconciler and a replaying retry cannot both drive an operation.

## Crash windows

The operation is advanced by a driver that takes the operation row as its only
input. The same driver serves the first attempt, a retry, and restart
reconciliation, so the three cannot diverge.

| Window | Durable state | Recovery |
| --- | --- | --- |
| W1 | Plugin receipt `accepted`, no operation row | Safe. Admission is the first effect, so the retry admits normally. The retry no longer meets a dead-end uncertain receipt for this method. |
| W2 | Operation `admitted` with frozen input, no session | Continue to S2 from the frozen input. The original request and fresh defaults are not read. |
| W3 | Session exists, phase still `admitted` | Continue. The insert is idempotent by primary key, so S2 finds the session and sets `prepared`. |
| W4 | `prepared`, not started | Continue S3. If the submission is `pending` and the session is still `CREATED`, the prompt was provably not delivered and the start proceeds. If the session is no longer `CREATED` while the submission is `pending`, the operation fails with `prompt_status=not_delivered`, the session is settled terminal, and nothing is resent. |
| W5 | Submission `dispatching` | Unresolvable. The provider might have received the prompt. The driver marks the operation `uncertain` with `prompt_status=uncertain`, records the session ID, and never resends (AC-002.4). |
| W6 | Submission `accepted`, phase still `prompt_dispatching` | Derive from the submission, set `prompt_accepted`, continue to S4. |
| W6b | Phase `prompt_accepted`, submission still `dispatching` | Completion re-asserts the receipt as `accepted` from the execution recorded at dispatch, then continues to S4. Any other receipt state blocks completion and is reported. |
| W7 | Primary applied, phase not updated | The CAS reports `already_primary`. Continue to complete. |
| W8 | Operation `completed`, plugin receipt not completed | Complete the receipt from the operation row. |

W5 is a real limit of the current orchestration. The existing submission
machine has no provider-side deduplication handle, so the system cannot tell
whether a `dispatching` prompt was received. The design reports this and does not
claim exactly-once provider delivery. The documentation lists W5 as the one
unresolved window (AC-004.6). What the system does guarantee is at-most-once
delivery of the prompt, no duplicate session, and an honest status.

Reconciliation runs at backend start for every non-terminal operation, in a
periodic sweep, and on any replay of the key, each through the claim in Ownership
and fencing. It does not start a session whose operation is terminal.

## Plugin surface

- Method constant `exactLaunchFreshSessionMethod = "LaunchFreshSessionExact"` in
  `internal/plugins/host_exact.go`, registered with `host.v2.write:execution`.
- New optional SDK interface in `pluginsdk/exact_execution.go`, separate from
  `ExactExecutionCommandManager` so existing implementations still compile:
  `ExactSessionLaunchCommandManager` with `HostExactSessionLaunchCommands(host)`.
- New RPC and messages in `pluginsdk/proto/kandev/plugin/v1/plugin.proto`,
  generated with `make proto` from `pluginsdk/`. Generated files are never hand
  edited.
- The command carries `RequestID`, `WorkspaceID`, `TaskID`, `IdempotencyKey`,
  `ExpectedTaskResourceVersion`, `ManagementInstanceKey`,
  `ExpectedClaimGeneration`, `ApprovalRevision`, `ManifestDigest`, `Prompt`,
  `AgentProfileID`, `MakePrimary`, and optional `ExpectedPrimarySessionID`.
- The result is the standard `CommandResult` plus `SessionID`, `ExecutionID`,
  `SessionState`, `PrimaryStatus`, `PromptStatus`, and `OperationID` (AC-007.6).
- An older host returns Unimplemented and the SDK surfaces an unsupported error
  without falling back to another command (AC-007.4).
- The adapter in `internal/backendapp/adapters_plugin_execution.go` implements a
  new `ExactSessionLaunchController` in `exact_execution_contract.go`. The plugin
  layer admits the plugin receipt, calls the task-domain operation, and completes
  the receipt from the operation's terminal state. When the operation is still in
  progress, the receipt stays `accepted` and the retry reconciles through the
  operation, not through the uncertain-outcome path.

## MCP surface

`spawn_session_kandev` (`internal/mcp/handlers/spawn_session.go` and the tool
registration in `internal/mcp/server/server.go`) gains optional
`idempotency_key`, `make_primary`, and `expected_primary_session_id`. The
presence of `idempotency_key` selects the exact path. On the exact path,
fields that it does not permit, such as `name`, are rejected, not ignored
(AC-007.3). Without the key, the tool behaves as it does now. The idempotency
scope is the authenticated agent identity, and the sender comes from the stream,
not the payload. The handler builds the same `ExactSessionLaunchRequest` and calls
the same task-domain operation as the plugin adapter.

Implemented behavior of the MCP exact path
(`internal/mcp/handlers/spawn_session_exact.go`):

- The principal scope is `mcp:<authenticated user id>:<sender task id>`. With
  authentication disabled the user id is empty, so the scope is the sender task.
  The plugin scope is `plugin:<installation id>`. The two scopes never share a
  key space.
- `make_primary` and `expected_primary_session_id` are rejected without a key.
  `name` is rejected with a key.
- The profile is the explicit `agent_profile_id` or the native default. It is
  not inherited from the spawning session, so one key has one digest regardless
  of which session retries it.
- The workspace is read from the target task after the existing same-workspace
  check. The management claim fence is nil because MCP has no claim.
- The MCP path has no resource-version precondition. `expected_primary_session_id`
  is the only optimistic assertion.
- `completed`, `partial`, and `in_progress` return a response whose `outcome`
  field names the result, with `replayed`, `operation_id`, `session_id`,
  `primary_status`, `primary_reason`, and `prompt_status`. `failed` and
  `uncertain` return a Conflict error that carries the same details. A refusal
  returns an error whose message carries a stable `reason`
  (`task_busy`, `task_archived`, `pending_move`, `primary_changed`,
  `idempotency_payload_mismatch`, and the other classifier reasons). An
  unrecognised error is returned sanitized.
- A stale `expected_primary_session_id` observed before the launch is a refusal
  (`primary_changed`) and creates no session. A primary that changes after the
  session exists yields `partial` with `primary_status=conflict_not_applied`.

## Observability

Metrics, with closed label sets, no task, session, prompt, or key labels:

- `exact_session_launch_operations_total{outcome}` where outcome is one of
  `in_progress`, `completed`, `partial`, `failed`, `uncertain`.
- `exact_session_launch_reconcile_total{result}` where result is one of
  `resumed`, `skipped_owned`, `list_failed`, `drive_error`.
- `exact_session_launch_primary_total{status}` where status is one of
  `not_requested`, `pending`, `applied`, `already_primary`,
  `conflict_not_applied`.

Structured zap logs carry the operation ID, phase transitions, and reconciliation
decisions. They never contain the prompt. The metrics are exposed through expvar and documented in the observability
section of `CLAUDE.md`.

## Persistence and migration

- One new table, additive, with a migration entry and dual-dialect SQL that
  passes `go run ./cmd/sqlguard ./internal`.
- No change to existing columns. The `PreallocatedSessionID` is an in-memory
  request field.
- The new CAS method is a repository method with SQLite and Postgres
  implementations, covered by the store conformance suite.
- Rollback is an unused table. The new plugin method and MCP fields are ignored by
  older callers.

## Assumptions for review

- A-1: Idempotency scope is the authenticated principal, not the workspace
  alone.
- A-2: The configuration surface is the optional agent profile only. Session
  `name` is rejected on the exact path because it would be an extra post-step
  side effect with its own crash window.
- A-3: "Busy" is the fail-closed definition in Admission: in-flight launch states,
  an open turn, open background work, known live foreground or subagent
  activity, or uncertain liveness. A session that is idle with none of those
  does not block a fresh launch.
- A-4: "Activity fencing" is that definition at admission, the baseline activity
  revalidation at the primary commit, the baseline compare, the nonterminal
  check, and host admission.
- A-5: A primary conflict that arises after the session exists does not fail
  the session or prompt outcome, and is reported as `partial` with
  `primary_status=conflict_not_applied`.
- A-6: The MCP exact path delivers the prompt verbatim and does not apply
  `SpawnOrigin` attribution or spawner profile inheritance (see MCP surface).

## Known limits

- W5 is unresolved by design. A crash with a `dispatching` submission yields
  `uncertain`, and the prompt is never resent. The system does not claim
  exactly-once provider delivery.
- A stale-but-alive owner that outlives its lease can cause a false `uncertain`,
  because the takeover cannot prove the old owner's dispatch did not happen.
- A failed or uncertain operation keeps its key. A caller needs a new key to
  try again, and the same key replays the recorded outcome.
- When the profile is omitted, the native default resolves at admission and is
  frozen into the operation. A later change to the default does not alter a
  retry.
- A retry after a crash between the primary promotion and the completion write
  reports `already_primary` instead of `applied`.
- A session that moved beyond `CREATED` while its submission is still `pending`
  is reported as failed with `session_not_startable`.
- A provider admission rejection is covered at the prompt-signal unit level and
  by a driver-level settlement test, not by an end-to-end provider fake.
- When provider admission refuses the prompt after the session receipt became
  `dispatching`, the operation reports `failed` with `not_delivered` while the
  session receipt stays `dispatching`. This is retained on purpose: returning
  the receipt to `pending` would let an ordinary start deliver the prompt later,
  outside the operation. The recorded first message carries `not_delivered`.
- The idempotency key is scoped to the authenticated principal and the sender
  task (`mcp:<user>:<sender task>`, `plugin:<installation>`). The same key used
  from a different sender task is a different operation and creates its own
  session. This is the intended per-sender scope, not a broadened grant.
- The plugin call is bounded to 30 seconds. An `in_progress` result means the
  caller retries the same key.
- The environment-gated PostgreSQL repository tests
  (`exact_session_launch*_postgres_test.go`) skip without
  `KANDEV_TEST_POSTGRES_DSN`. They have not been run against a live PostgreSQL
  in this change, so PostgreSQL behavior is not proven by a run.

## Related

- [Exact fresh-session launch requirements](../requirements/exact-session-launch.md)
- [Task launch failure recovery](task-launch-failure-recovery.md)
- [Implementation plan](../../../plans/exact-session-launch/plan.md)
