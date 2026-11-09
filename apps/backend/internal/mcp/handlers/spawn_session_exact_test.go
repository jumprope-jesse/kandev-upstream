package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/kandev/kandev/internal/orchestrator"
	"github.com/kandev/kandev/internal/plugins"
	"github.com/kandev/kandev/internal/task/models"
	"github.com/kandev/kandev/internal/task/repository/repoerrors"
	ws "github.com/kandev/kandev/pkg/websocket"
	"github.com/kdlbs/kandev/pluginsdk"
)

type exactSpawnOrchestrator struct {
	*fakeOrchestrator
	calls  int
	scope  string
	req    models.ExactSessionLaunchRequest
	claim  *models.TaskManagementClaimFence
	result *orchestrator.ExactSessionLaunchResult
	err    error
}

func (f *exactSpawnOrchestrator) LaunchFreshSessionExact(_ context.Context, scope string, req models.ExactSessionLaunchRequest, claim *models.TaskManagementClaimFence) (*orchestrator.ExactSessionLaunchResult, error) {
	f.calls++
	f.scope, f.req, f.claim = scope, req, claim
	return f.result, f.err
}

func exactSpawnOperation(mutate func(*models.ExactSessionLaunchOperation)) *orchestrator.ExactSessionLaunchResult {
	op := &models.ExactSessionLaunchOperation{
		OperationID: "op-1", SessionID: "session-new", Phase: models.ExactLaunchPhaseCompleted,
		PromptStatus: models.ExactLaunchPromptAccepted, PrimaryStatus: models.ExactLaunchPrimaryNotRequested,
	}
	if mutate != nil {
		mutate(op)
	}
	return &orchestrator.ExactSessionLaunchResult{Operation: op}
}

func newExactSpawnFixture(t *testing.T) (*Handlers, *exactSpawnOrchestrator, *models.Task, string) {
	t.Helper()
	svc, repo := newTestTaskService(t)
	_, target, sess := seedTaskWithSession(t, svc, repo, models.TaskSessionStateRunning)
	h, orch := newMessageTaskHandler(t, svc)
	exact := &exactSpawnOrchestrator{fakeOrchestrator: orch, result: exactSpawnOperation(nil)}
	h.sessionLauncher = exact
	return h, exact, target, sess.ID
}

func spawnExact(t *testing.T, h *Handlers, payload map[string]interface{}) *ws.Message {
	t.Helper()
	resp, err := h.handleSpawnSession(context.Background(), makeWSMessage(t, ws.ActionMCPSpawnSession, payload))
	require.NoError(t, err)
	return resp
}

func TestHandleSpawnSessionExactPathCallsTheNativeLaunchWithoutTheKeylessPath(t *testing.T) {
	h, exact, target, senderSession := newExactSpawnFixture(t)
	exact.result = exactSpawnOperation(func(op *models.ExactSessionLaunchOperation) {
		op.PrimaryStatus = models.ExactLaunchPrimaryApplied
	})
	payload := spawnPayload(target.ID, "first instruction", target.ID, senderSession)
	payload["idempotency_key"] = "key-1"
	payload["make_primary"] = true
	payload["expected_primary_session_id"] = "baseline"
	payload["agent_profile_id"] = "profile-x"

	resp := spawnExact(t, h, payload)
	require.Equal(t, ws.MessageTypeResponse, resp.Type)
	var out map[string]interface{}
	require.NoError(t, json.Unmarshal(resp.Payload, &out))
	assert.Equal(t, "completed", out["outcome"])
	assert.Equal(t, "session-new", out["session_id"])
	assert.Equal(t, "applied", out["primary_status"])
	assert.Equal(t, "accepted", out["prompt_status"])

	require.Equal(t, 1, exact.calls)
	assert.Equal(t, "mcp::"+target.ID, exact.scope)
	assert.Equal(t, "ws-1", exact.req.WorkspaceID)
	assert.Equal(t, target.ID, exact.req.TaskID)
	assert.Equal(t, "first instruction", exact.req.Prompt)
	assert.Equal(t, "key-1", exact.req.IdempotencyKey)
	assert.Equal(t, "profile-x", exact.req.AgentProfileID)
	assert.True(t, exact.req.MakePrimary)
	require.NotNil(t, exact.req.ExpectedPrimarySessionID)
	assert.Equal(t, "baseline", *exact.req.ExpectedPrimarySessionID)
	assert.Nil(t, exact.claim)
	assert.Empty(t, exact.launchCalls, "the exact path must not compose the keyless launch")
	assert.Empty(t, exact.renameCalls)
}

func TestHandleSpawnSessionExactPathDoesNotInheritTheSpawnerProfile(t *testing.T) {
	h, exact, target, senderSession := newExactSpawnFixture(t)
	payload := spawnPayload(target.ID, "go", target.ID, senderSession)
	payload["idempotency_key"] = "key-1"
	resp := spawnExact(t, h, payload)
	require.Equal(t, ws.MessageTypeResponse, resp.Type)
	assert.Empty(t, exact.req.AgentProfileID)
	assert.Nil(t, exact.req.ExpectedPrimarySessionID)
	assert.False(t, exact.req.MakePrimary)
}

func TestHandleSpawnSessionRejectsUnsupportedFieldsBeforeAnySideEffect(t *testing.T) {
	h, exact, target, senderSession := newExactSpawnFixture(t)
	named := spawnPayload(target.ID, "go", target.ID, senderSession)
	named["idempotency_key"] = "key-1"
	named["name"] = "reviewer"
	assertWSError(t, spawnExact(t, h, named), ws.ErrorCodeValidation)

	for field, value := range map[string]interface{}{"make_primary": true, "expected_primary_session_id": "s"} {
		keyless := spawnPayload(target.ID, "go", target.ID, senderSession)
		keyless[field] = value
		assertWSError(t, spawnExact(t, h, keyless), ws.ErrorCodeValidation)
	}
	assert.Zero(t, exact.calls)
	assert.Empty(t, exact.launchCalls)
}

func TestHandleSpawnSessionExactPathEnforcesTheSameWorkspaceScope(t *testing.T) {
	h, exact, _, senderSession := newExactSpawnFixture(t)
	payload := spawnPayload("no-such-task", "go", "no-such-task", senderSession)
	payload["idempotency_key"] = "key-1"
	assertWSError(t, spawnExact(t, h, payload), ws.ErrorCodeNotFound)
	assert.Zero(t, exact.calls)
}

func TestHandleSpawnSessionExactPathReportsPartialPrimaryAsAnExplicitOutcome(t *testing.T) {
	h, exact, target, senderSession := newExactSpawnFixture(t)
	exact.result = exactSpawnOperation(func(op *models.ExactSessionLaunchOperation) {
		op.MakePrimary = true
		op.PrimaryStatus = models.ExactLaunchPrimaryConflict
		op.PrimaryReason = models.ExactSessionLaunchPrimaryReason("primary_changed")
	})
	exact.result.Replayed = true
	payload := spawnPayload(target.ID, "go", target.ID, senderSession)
	payload["idempotency_key"] = "key-1"
	payload["make_primary"] = true
	resp := spawnExact(t, h, payload)
	require.Equal(t, ws.MessageTypeResponse, resp.Type)
	var out map[string]interface{}
	require.NoError(t, json.Unmarshal(resp.Payload, &out))
	assert.Equal(t, "partial", out["outcome"])
	assert.Equal(t, "conflict_not_applied", out["primary_status"])
	assert.Equal(t, "primary_changed", out["primary_reason"])
	assert.Equal(t, true, out["replayed"])
}

func TestHandleSpawnSessionExactPathNeverReportsFailedOrUncertainAsSuccess(t *testing.T) {
	h, exact, target, senderSession := newExactSpawnFixture(t)
	payload := spawnPayload(target.ID, "go", target.ID, senderSession)
	payload["idempotency_key"] = "key-1"

	exact.result = exactSpawnOperation(func(op *models.ExactSessionLaunchOperation) {
		op.Phase = models.ExactLaunchPhaseUncertain
		op.PromptStatus = models.ExactLaunchPromptUncertain
		op.ErrorCode = "prompt_delivery_uncertain"
	})
	resp := spawnExact(t, h, payload)
	assertWSError(t, resp, ws.ErrorCodeConflict)
	assert.Equal(t, "uncertain", wsErrorDetails(t, resp)["outcome"])
	assert.Equal(t, "session-new", wsErrorDetails(t, resp)["session_id"])

	exact.result = exactSpawnOperation(func(op *models.ExactSessionLaunchOperation) {
		op.Phase = models.ExactLaunchPhaseFailed
		op.PromptStatus = models.ExactLaunchPromptNotDelivered
		op.ErrorCode = "session_not_startable"
	})
	resp = spawnExact(t, h, payload)
	assertWSError(t, resp, ws.ErrorCodeConflict)
	assert.Equal(t, "failed", wsErrorDetails(t, resp)["outcome"])
}

func TestHandleSpawnSessionExactPathMapsRefusalsToStableReasons(t *testing.T) {
	h, exact, target, senderSession := newExactSpawnFixture(t)
	payload := spawnPayload(target.ID, "go", target.ID, senderSession)
	payload["idempotency_key"] = "key-1"
	cases := []struct {
		err    error
		code   string
		reason string
	}{
		{repoerrors.ErrExactLaunchBusy, ws.ErrorCodeConflict, "task_busy"},
		{repoerrors.ErrTaskOperationConflict, ws.ErrorCodeConflict, "idempotency_payload_mismatch"},
		{repoerrors.ErrExactLaunchBaselineMismatch, ws.ErrorCodeConflict, "primary_changed"},
		{repoerrors.ErrTaskNotFound, ws.ErrorCodeNotFound, "target_not_found"},
		{models.ErrInvalidExactSessionLaunch, ws.ErrorCodeValidation, "invalid_request"},
		{orchestrator.ErrExactLaunchUnavailable, ws.ErrorCodeUnavailable, "launch_unavailable"},
	}
	for _, tc := range cases {
		exact.err = tc.err
		resp := spawnExact(t, h, payload)
		assertWSError(t, resp, tc.code)
		assert.Equal(t, tc.reason, wsErrorDetails(t, resp)["reason"], tc.err.Error())
	}
	exact.err = errors.New("sql: connection refused at /var/lib/kandev.db")
	resp := spawnExact(t, h, payload)
	assertWSError(t, resp, ws.ErrorCodeInternalError)
	assert.NotContains(t, string(resp.Payload), "/var/lib")
}

func TestHandleSpawnSessionExactPathIsUnavailableWithoutTheNativeLauncher(t *testing.T) {
	svc, repo := newTestTaskService(t)
	_, target, sess := seedTaskWithSession(t, svc, repo, models.TaskSessionStateRunning)
	h, orch := newMessageTaskHandler(t, svc)
	payload := spawnPayload(target.ID, "go", target.ID, sess.ID)
	payload["idempotency_key"] = "key-1"
	assertWSError(t, spawnExact(t, h, payload), ws.ErrorCodeUnavailable)
	assert.Empty(t, orch.launchCalls)
}

func TestHandleSpawnSessionExactRequestMatchesThePluginCommandDigest(t *testing.T) {
	h, exact, target, senderSession := newExactSpawnFixture(t)
	baseline := "baseline"
	payload := spawnPayload(target.ID, "first instruction", target.ID, senderSession)
	payload["idempotency_key"] = "key-1"
	payload["make_primary"] = true
	payload["expected_primary_session_id"] = baseline
	payload["agent_profile_id"] = "profile-x"
	require.Equal(t, ws.MessageTypeResponse, spawnExact(t, h, payload).Type)

	viaPlugin := plugins.SessionLaunchRequest(pluginsdk.ExactSessionLaunchCommand{
		RequestID: "request-1", WorkspaceID: "ws-1", TaskID: target.ID, IdempotencyKey: "key-1",
		ExpectedTaskResourceVersion: "2026-09-26T09:00:00Z", ManagementInstanceKey: "lead", ExpectedClaimGeneration: 9,
		ApprovalRevision: 4, ManifestDigest: "d", Prompt: "first instruction", AgentProfileID: "profile-x",
		MakePrimary: true, ExpectedPrimarySessionID: &baseline,
	})
	assert.Equal(t, viaPlugin.Digest(), exact.req.Digest(), "the same logical launch must have one digest on both surfaces")

	changed := viaPlugin
	changed.Prompt = "other"
	assert.NotEqual(t, changed.Digest(), exact.req.Digest())
}
