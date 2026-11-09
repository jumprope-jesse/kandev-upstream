---
status: active
system: tasks
created: 2026-11-09
owners:
  - kandev
---

# Exact Fresh-Session Launch Requirements

## Overview

A trusted caller sometimes needs to start one fresh agent session on a card that
already exists, deliver a specific first instruction, and optionally make that
session the card's primary session, with a caller-supplied key that makes the
whole occurrence safe to retry. Today no single supported operation provides
that.

- The plugin `EnsureTaskRunExact` command cannot carry an initial prompt, cannot
  guarantee a fresh session (it reuses an existing session), and has no primary
  option.
- The MCP `spawn_session_kandev` tool carries a prompt but has no durable
  occurrence key and no primary option.
- Composing launch, message, and set-primary calls leaves crash windows in
  which a retry duplicates the session or the prompt.

This capability adds one first-class launch operation, offered through both the
plugin SDK and MCP with equivalent semantics. It reuses the existing launch
gates and the existing exact-command receipt model. It does not weaken either.

No existing upstream issue or pull request implements this capability
(searched 2026-11-09 against `kdlbs/kandev` main at `1529e72ed`). Related
context is pull request #4764 and issue #4477. The implementation plan records
the search.

## Terminology

- **Occurrence:** one intended launch, identified by the caller principal, the
  workspace, and the caller idempotency key.
- **Launch operation:** the durable native record of one occurrence. It exists
  before any session is created.
- **Fresh session:** a session created by this launch operation. It is never an
  existing session reused, resumed, or recovered.
- **Initial prompt:** the caller-authored instruction text supplied with the
  request.
- **Baseline primary:** the card's primary session as observed at admission, or
  as asserted by the caller when the caller supplies one.
- **Make-primary:** an optional request that the fresh session become the
  card's primary session.
- **Live-idle messaging:** the existing exact message command that targets one
  existing live session. It is a separate operation.

## Requirements

### REQ-TASKS-EXACT-LAUNCH-001: Fresh session on an existing card

**Intent:** Start a new session on a card without reusing or disturbing any
existing session.

#### Acceptance criteria

- **AC-TASKS-EXACT-LAUNCH-001.1:** When the card has never had a session, the system shall create one fresh session on that card.
- **AC-TASKS-EXACT-LAUNCH-001.2:** When every earlier session on the card is failed, cancelled, or otherwise ended, the system shall create one fresh session and shall not resume, recover, or reopen an earlier session.
- **AC-TASKS-EXACT-LAUNCH-001.3:** When the card has an in-flight launch, a running turn, known live background or subagent work, or activity whose liveness cannot be established, the system shall refuse the request with a retryable busy error before it creates any session. A session that is idle with no active work shall not block the launch, and the operation shall never interrupt active work.
- **AC-TASKS-EXACT-LAUNCH-001.4:** When the card is archived, the system shall refuse the request before it creates any session.
- **AC-TASKS-EXACT-LAUNCH-001.5:** When the target card does not exist in the stated workspace, the system shall return a not-found error that does not reveal whether the card exists elsewhere.
- **AC-TASKS-EXACT-LAUNCH-001.6:** The operation shall not create a card, change the card's workflow step, or send a message to any existing session.

### REQ-TASKS-EXACT-LAUNCH-002: Initial prompt is the first user instruction

**Intent:** The agent's first user-authored instruction is exactly what the caller supplied.

#### Acceptance criteria

- **AC-TASKS-EXACT-LAUNCH-002.1:** The system shall require a non-empty initial prompt and shall reject a prompt above the existing initial-submission size bound before it creates any session.
- **AC-TASKS-EXACT-LAUNCH-002.2:** When the session starts, the first user message recorded in its transcript shall equal the supplied prompt, and the user-authored text delivered to the provider shall be that prompt only. No card description or other user-authored text shall precede or replace it. The server-composed trusted context that every launch adds may accompany it.
- **AC-TASKS-EXACT-LAUNCH-002.3:** The system shall deliver the initial prompt at most once per launch operation, including after a retry, a lost response, or a restart.
- **AC-TASKS-EXACT-LAUNCH-002.4:** When the system cannot determine whether the prompt reached the provider, it shall record the delivery as uncertain, shall not resend it, and shall report the session identity with an explicit uncertain-delivery status.
- **AC-TASKS-EXACT-LAUNCH-002.5:** The system shall keep server-composed trusted context separate from the caller prompt and shall not let the caller prompt forge it.

### REQ-TASKS-EXACT-LAUNCH-003: Canonical request identity and idempotency

**Intent:** One key names one occurrence, and the same request always yields the same outcome.

#### Acceptance criteria

- **AC-TASKS-EXACT-LAUNCH-003.1:** The system shall bind each request to a canonical digest of the workspace, card, initial prompt, permitted configuration, make-primary flag, and any caller-asserted baseline primary.
- **AC-TASKS-EXACT-LAUNCH-003.2:** When a request repeats a key with an identical digest, the system shall return the same session, initial-prompt, execution, and primary outcome and shall create no new session or prompt.
- **AC-TASKS-EXACT-LAUNCH-003.3:** When a request repeats a key with a different digest, the system shall return a conflict and shall perform no side effect.
- **AC-TASKS-EXACT-LAUNCH-003.4:** When concurrent requests carry the same key and digest, exactly one shall run the launch and the others shall observe its outcome or a retryable in-progress status with the same operation identity.
- **AC-TASKS-EXACT-LAUNCH-003.5:** The idempotency scope shall include the authenticated caller principal, so one caller's key cannot observe or collide with another caller's occurrence.
- **AC-TASKS-EXACT-LAUNCH-003.6:** When the original response was lost, a retry shall return the outcome the first attempt produced without repeating any side effect.
- **AC-TASKS-EXACT-LAUNCH-003.7:** The permitted configuration shall be limited to an optional explicit agent profile. When it is omitted, the system shall resolve the profile exactly as an ordinary launch of that card does, and no model, permission, or executor override shall be accepted.

### REQ-TASKS-EXACT-LAUNCH-004: Durable operation and reconciliation

**Intent:** An interrupted launch is completed or reported honestly, never repeated blindly.

#### Acceptance criteria

- **AC-TASKS-EXACT-LAUNCH-004.1:** The system shall persist the launch operation, including the fresh session identity, before it creates the session.
- **AC-TASKS-EXACT-LAUNCH-004.2:** When the process restarts with a launch operation that has not finished, the system shall reconcile it from durable facts and shall drive it to a terminal outcome or to an explicit uncertain outcome.
- **AC-TASKS-EXACT-LAUNCH-004.3:** When a retry arrives while an earlier attempt is unfinished, the system shall reconcile that operation instead of returning a generic uncertain error or launching again.
- **AC-TASKS-EXACT-LAUNCH-004.4:** The system shall report success only when the session exists and the initial prompt is accepted by the provider. A result for any other state shall name that state.
- **AC-TASKS-EXACT-LAUNCH-004.5:** When a gate refuses the launch after the session exists, the system shall record a terminal failure for the operation, shall leave no session that can later start without the operation, and shall return the same failure on retry.
- **AC-TASKS-EXACT-LAUNCH-004.6:** The documentation shall state each crash window that reconciliation cannot resolve. It shall not describe the operation as exactly-once for provider delivery.
- **AC-TASKS-EXACT-LAUNCH-004.7:** The system shall expose operation status transitions and reconciliation outcomes through metrics with closed label sets and through structured logs, with no task, session, prompt, or key value as a label.
- **AC-TASKS-EXACT-LAUNCH-004.8:** The system shall persist the validated launch input, including the initial prompt and the resolved launch configuration, durably before it creates the session, and shall complete an interrupted launch from that stored input alone, without the original request and without resolving profile or defaults again.
- **AC-TASKS-EXACT-LAUNCH-004.9:** At most one worker shall drive an operation at a time. A worker that loses ownership shall stop before its next effect, and a concurrent retry and a restart reconciliation shall not both dispatch the same step.

### REQ-TASKS-EXACT-LAUNCH-005: Primary selection

**Intent:** The caller chooses whether the fresh session becomes primary, and a concurrent change is never overwritten.

#### Acceptance criteria

- **AC-TASKS-EXACT-LAUNCH-005.1:** When make-primary is omitted, the system shall preserve the established selection: the fresh session becomes primary only when the card has no primary session at creation, and otherwise the existing primary is unchanged. The documentation shall state this rule.
- **AC-TASKS-EXACT-LAUNCH-005.2:** When make-primary is requested and the baseline primary is still primary, the system shall make the fresh session primary in one atomic step that clears the baseline.
- **AC-TASKS-EXACT-LAUNCH-005.3:** When make-primary is requested and the primary changed since the baseline, the system shall leave the current primary unchanged and shall report an explicit partial outcome with a not-applied primary status and reason, with the session and prompt outcomes intact. It shall not report undifferentiated success.
- **AC-TASKS-EXACT-LAUNCH-005.4:** When the caller asserts a baseline primary that differs from the stored primary at admission, the system shall refuse the request before it creates any session.
- **AC-TASKS-EXACT-LAUNCH-005.5:** The system shall promote the fresh session only while it is nonterminal and shall not change the version of any session whose primary flag does not change.
- **AC-TASKS-EXACT-LAUNCH-005.6:** When the fresh session is already primary, a retry shall report it as already primary and shall not change any session.
- **AC-TASKS-EXACT-LAUNCH-005.7:** The system shall never report a requested primary as applied unless the committed state shows the fresh session as the card's only primary.
- **AC-TASKS-EXACT-LAUNCH-005.8:** The system shall not displace a baseline primary that has active work or whose activity changed since admission. It shall revalidate the baseline's lifecycle and activity at the primary commit, and primary-ID equality alone shall not establish idleness.
- **AC-TASKS-EXACT-LAUNCH-005.9:** The result shall always carry the primary status and, when the primary was not applied, the reason, so a partial outcome is distinguishable from complete success.

### REQ-TASKS-EXACT-LAUNCH-006: Existing gates and authority are preserved

**Intent:** The new operation is not a way around any existing control.

#### Acceptance criteria

- **AC-TASKS-EXACT-LAUNCH-006.1:** The system shall enforce the existing caller authentication, per-user workspace scoping, and capability approval, and shall add no capability, permission, or broad grant.
- **AC-TASKS-EXACT-LAUNCH-006.2:** When a plugin caller lacks the execution capability, targets another workspace, presents a stale approval revision or manifest digest, or presents a stale task management claim fence, the system shall refuse the request before it creates any session.
- **AC-TASKS-EXACT-LAUNCH-006.3:** When the observed task resource version is stale, the system shall refuse the request before it creates any session.
- **AC-TASKS-EXACT-LAUNCH-006.4:** When the card has a pending workflow move, a continuity or recovery block, a workflow-session ceiling deferral, a profile or repository gate failure, or host admission refusal, the system shall refuse or fail the launch through the same decision the ordinary launch makes and shall not bypass or weaken it.
- **AC-TASKS-EXACT-LAUNCH-006.5:** The system shall take the acting identity from the authenticated principal and the connected installation or agent stream, and shall not accept an actor, sender, or origin value from the request payload.
- **AC-TASKS-EXACT-LAUNCH-006.6:** The system shall use the existing execution-ownership and host-admission paths and shall not start an agent outside them.
- **AC-TASKS-EXACT-LAUNCH-006.7:** The operation shall not change the behavior of live-idle exact messaging, `EnsureTaskRunExact`, or `spawn_session_kandev` without a key.
- **AC-TASKS-EXACT-LAUNCH-006.8:** The system shall evaluate authentication, workspace scoping, capability, claim fence, approval, and manifest checks on every request, including a retry of a known operation, and shall apply the observed resource-version precondition only when creating a new operation.

### REQ-TASKS-EXACT-LAUNCH-007: MCP and plugin SDK equivalence

**Intent:** The two surfaces expose one supported operation with the same meaning.

#### Acceptance criteria

- **AC-TASKS-EXACT-LAUNCH-007.1:** The plugin SDK and MCP shall each expose the operation with the same request fields, the same result fields, and the same error classes.
- **AC-TASKS-EXACT-LAUNCH-007.2:** For equal logical requests, the two surfaces shall produce the same canonical digest.
- **AC-TASKS-EXACT-LAUNCH-007.3:** When the MCP request carries no idempotency key, `spawn_session_kandev` shall behave exactly as before. When the request carries the key, it shall use the exact path, and fields the exact path does not permit shall be rejected rather than ignored.
- **AC-TASKS-EXACT-LAUNCH-007.4:** When the connected host does not support the operation, the SDK shall fail closed with an unsupported error and shall not fall back to another command.
- **AC-TASKS-EXACT-LAUNCH-007.5:** The authoring documentation, the SDK reference, and the MCP tool description shall describe the operation, its idempotency, its primary rule, and its crash limits.
- **AC-TASKS-EXACT-LAUNCH-007.6:** The plugin surface shall return the standard command result with the operation identity so a caller can correlate a retry.

## Out of scope

- The observe hook, any provider implementation, host or live installation, and release timer or workflow changes.
- Attachments on the initial prompt, session naming, and executor, model, or permission overrides.
- A general exactly-once guarantee for provider delivery.
- Changing live-idle exact messaging, `EnsureTaskRunExact`, or the keyless `spawn_session_kandev` behavior.
- A new receipt system. The existing exact command receipts remain the plugin receipt, and a task-domain operation record owns launch truth.
- Rendered UI. Existing surfaces already display sessions and the primary star. The system does not change them.
