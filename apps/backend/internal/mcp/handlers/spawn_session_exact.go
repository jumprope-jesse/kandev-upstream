package handlers

import (
	"context"
	"strings"

	"github.com/kandev/kandev/internal/auth/authn"
	"github.com/kandev/kandev/internal/orchestrator"
	"github.com/kandev/kandev/internal/task/models"
	ws "github.com/kandev/kandev/pkg/websocket"
)

// ExactSessionLauncher is the native exact fresh-session launch. A launcher
// that does not implement it leaves the idempotent spawn path unavailable.
type ExactSessionLauncher interface {
	LaunchFreshSessionExact(ctx context.Context, principalScope string, req models.ExactSessionLaunchRequest, claim *models.TaskManagementClaimFence) (*orchestrator.ExactSessionLaunchResult, error)
}

// mcpPrincipalScope scopes an idempotency key to the authenticated user and the
// calling agent's task. Both come from server state, never the request body.
func mcpPrincipalScope(ctx context.Context, senderTaskID string) string {
	userID := ""
	if identity, ok := authn.IdentityFromContext(ctx); ok {
		userID = identity.UserID
	}
	return "mcp:" + userID + ":" + senderTaskID
}

// handleExactSpawnSession starts a fresh session whose first user instruction is
// the prompt, under a caller idempotency key, through the same native operation
// the plugin surface uses. The agent profile is the explicit request value or
// the native default; it is not inherited from the spawner, so a retry from a
// different session of the same task keeps one request digest.
func (h *Handlers) handleExactSpawnSession(ctx context.Context, req *spawnSessionRequest, msg *ws.Message) (*ws.Message, error) {
	launcher, ok := h.sessionLauncher.(ExactSessionLauncher)
	if !ok {
		return wsError(msg.ID, msg.Action, ws.ErrorCodeUnavailable, "idempotent session launch is not available"), nil
	}
	if errResp := h.authorizeSpawnTarget(ctx, req, msg); errResp != nil {
		return errResp, nil
	}
	target, err := h.taskSvc.GetTask(ctx, req.TaskID)
	if err != nil || target == nil {
		return wsError(msg.ID, msg.Action, ws.ErrorCodeNotFound, "task not found: "+req.TaskID), nil
	}
	result, err := launcher.LaunchFreshSessionExact(ctx, mcpPrincipalScope(ctx, req.SenderTaskID), models.ExactSessionLaunchRequest{
		WorkspaceID: target.WorkspaceID, TaskID: req.TaskID, IdempotencyKey: req.IdempotencyKey,
		Prompt: req.Prompt, AgentProfileID: req.AgentProfileID, MakePrimary: req.MakePrimary,
		ExpectedPrimarySessionID: req.ExpectedPrimarySessionID,
	}, nil)
	if err != nil {
		return exactSpawnRefusal(msg, err), nil
	}
	return exactSpawnResponse(msg, req.TaskID, result), nil
}

func exactSpawnRefusal(msg *ws.Message, err error) *ws.Message {
	refusal, ok := orchestrator.ClassifyExactLaunchError(err)
	if !ok {
		return wsError(msg.ID, msg.Action, ws.ErrorCodeInternalError, "failed to spawn session")
	}
	code := ws.ErrorCodeConflict
	switch refusal.Kind {
	case orchestrator.ExactLaunchRefusalInvalid:
		code = ws.ErrorCodeValidation
	case orchestrator.ExactLaunchRefusalNotFound:
		code = ws.ErrorCodeNotFound
	case orchestrator.ExactLaunchRefusalUnavailable:
		code = ws.ErrorCodeUnavailable
	}
	resp, _ := ws.NewError(msg.ID, msg.Action, code, "session launch refused: "+refusal.Reason,
		map[string]interface{}{"reason": refusal.Reason, "recoverable": true})
	return resp
}

// exactSpawnResponse reports the operation's outcome class. Failed and
// uncertain operations are errors so a caller cannot read them as success; a
// partial or in-progress operation is a response whose outcome field says so.
func exactSpawnResponse(msg *ws.Message, taskID string, result *orchestrator.ExactSessionLaunchResult) *ws.Message {
	op := result.Operation
	outcome := result.Outcome()
	details := map[string]interface{}{
		"task_id": taskID, "operation_id": op.OperationID, "session_id": op.SessionID,
		"outcome": string(outcome), "prompt_status": string(op.PromptStatus),
		"primary_status": string(op.PrimaryStatus), "primary_reason": string(op.PrimaryReason),
		"replayed": result.Replayed,
	}
	switch outcome {
	case models.ExactLaunchOutcomeFailed, models.ExactLaunchOutcomeUncertain:
		details["reason"] = op.ErrorCode
		resp, _ := ws.NewError(msg.ID, msg.Action, ws.ErrorCodeConflict,
			"session launch "+string(outcome)+": "+strings.TrimSpace(op.ErrorCode), details)
		return resp
	}
	resp, _ := ws.NewResponse(msg.ID, msg.Action, details)
	return resp
}
