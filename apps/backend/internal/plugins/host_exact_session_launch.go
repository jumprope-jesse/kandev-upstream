package plugins

import (
	"context"
	"encoding/json"
	"time"

	"github.com/kandev/kandev/internal/task/models"
	"github.com/kdlbs/kandev/pluginsdk"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

const (
	exactLaunchFreshSessionMethod = "LaunchFreshSessionExact"
	// exactSessionLaunchWait bounds one call. A launch still in progress when it
	// elapses is reported as in progress; the caller retries the same key.
	exactSessionLaunchWait = 30 * time.Second

	launchOutcomeInProgress = "in_progress"
	launchOutcomeCompleted  = "completed"
	launchOutcomePartial    = "partial"
	launchOutcomeFailed     = "failed"
	launchOutcomeUncertain  = "uncertain"
)

type exactSessionLaunchManager struct{ host *pluginHost }

func (h *pluginHost) ExactSessionLaunchCommands() pluginsdk.ExactSessionLaunchCommandManager {
	return exactSessionLaunchManager{host: h}
}

func exactSessionLaunchUnavailableReason(method string, host *pluginHost, commandStoreAvailable bool) string {
	if method != exactLaunchFreshSessionMethod {
		return ""
	}
	switch {
	case host.sessionLaunchController() == nil:
		return "exact_session_launch_unavailable"
	case !commandStoreAvailable:
		return exactReasonCommandStoreUnavailable
	}
	return ""
}

func (h *pluginHost) sessionLaunchController() ExactSessionLaunchController {
	controller := h.executionController()
	if controller == nil {
		return nil
	}
	launcher, _ := controller.(ExactSessionLaunchController)
	return launcher
}

func (m exactSessionLaunchManager) LaunchFreshSession(ctx context.Context, in pluginsdk.ExactSessionLaunchCommand) (*pluginsdk.CommandResult, pluginsdk.ExactSessionLaunchResult, error) {
	if !validSessionLaunchCommand(in) {
		return exactInvalidResult(), pluginsdk.ExactSessionLaunchResult{}, nil
	}
	digest := SessionLaunchRequest(in).Digest()
	admission, result := m.host.admitExecutionCommand(ctx, exactLaunchFreshSessionMethod, "host.v2.write:execution", in.RequestID, in.WorkspaceID, in.TaskID, in.IdempotencyKey, in.ExpectedTaskResourceVersion, in.ApprovalRevision, in.ManifestDigest, digest)
	if result != nil {
		return result, pluginsdk.ExactSessionLaunchResult{}, nil
	}
	defer admission.unlock()
	controller := m.host.sessionLaunchController()
	if controller == nil {
		return exactUnavailableResult("execution_control_unavailable"), pluginsdk.ExactSessionLaunchResult{}, nil
	}
	// The controller runs on every call, including a replay of a completed
	// receipt, so current claim and native policy checks are never skipped. The
	// native operation record, not this receipt, is the source of the outcome.
	callCtx, cancel := context.WithTimeout(ctx, exactSessionLaunchWait)
	defer cancel()
	outcome, err := controller.LaunchFreshSession(callCtx, m.host.installationID, in)
	if err != nil {
		return launchErrorResult(err), pluginsdk.ExactSessionLaunchResult{}, nil
	}
	return completeSessionLaunch(ctx, admission, outcome)
}

func completeSessionLaunch(ctx context.Context, admission *exactExecutionAdmission, outcome ExactSessionLaunchOutcome) (*pluginsdk.CommandResult, pluginsdk.ExactSessionLaunchResult, error) {
	statusValue, reason, terminal := sessionLaunchStatus(outcome)
	if !terminal {
		result := exactUnavailableResult("launch_in_progress")
		return result, sessionLaunchResult(outcome, result), nil
	}
	if admission.replay && admission.record.Receipt.State == exactReceiptStateCompleted {
		replay, _ := replayedExactCommand(admission)
		return replay, replayedSessionLaunchResult(admission, replay), nil
	}
	data, err := json.Marshal(outcome)
	if err != nil {
		return exactUnavailableResult("command_receipt_unavailable"), pluginsdk.ExactSessionLaunchResult{}, nil
	}
	completed, err := admission.store.CompleteWithResultData(ctx, admission.record.Intent.OperationID,
		string(statusValue), reason, outcome.SessionID, outcome.SessionState, string(data))
	if err != nil {
		return exactUnavailableResult("command_receipt_unavailable"), pluginsdk.ExactSessionLaunchResult{}, nil
	}
	result := commandResultFromRecord(completed)
	return result, sessionLaunchResult(outcome, result), nil
}

// sessionLaunchStatus maps the native outcome to a command status. Only a
// terminal native outcome completes the receipt; a requested primary that was
// not applied is PARTIAL, and an unprovable prompt delivery is UNCERTAIN.
func sessionLaunchStatus(outcome ExactSessionLaunchOutcome) (pluginsdk.CommandStatus, string, bool) {
	switch outcome.Outcome {
	case launchOutcomeCompleted:
		return pluginsdk.CommandApplied, "", true
	case launchOutcomePartial:
		return pluginsdk.CommandPartial, firstNonEmpty(outcome.PrimaryReason, "primary_not_applied"), true
	case launchOutcomeFailed:
		return pluginsdk.CommandConflict, firstNonEmpty(outcome.FailureCode, "launch_failed"), true
	case launchOutcomeUncertain:
		return pluginsdk.CommandUncertain, firstNonEmpty(outcome.FailureCode, "prompt_delivery_uncertain"), true
	default:
		return "", "", false
	}
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}

func sessionLaunchResult(outcome ExactSessionLaunchOutcome, result *pluginsdk.CommandResult) pluginsdk.ExactSessionLaunchResult {
	return pluginsdk.ExactSessionLaunchResult{
		Result: result, OperationID: outcome.OperationID, SessionID: outcome.SessionID,
		SessionState: outcome.SessionState, Outcome: outcome.Outcome, PromptStatus: outcome.PromptStatus,
		PrimaryStatus: outcome.PrimaryStatus, PrimaryReason: outcome.PrimaryReason,
	}
}

func replayedSessionLaunchResult(admission *exactExecutionAdmission, result *pluginsdk.CommandResult) pluginsdk.ExactSessionLaunchResult {
	var stored ExactSessionLaunchOutcome
	if data := admission.record.Receipt.ResultData; data == "" || json.Unmarshal([]byte(data), &stored) != nil {
		return pluginsdk.ExactSessionLaunchResult{Result: result}
	}
	return sessionLaunchResult(stored, result)
}

// launchErrorResult maps a controller error to a command result. The receipt is
// left open: a refusal or stale precondition created no native operation, so a
// retry of the same key must be evaluated afresh.
func launchErrorResult(err error) *pluginsdk.CommandResult {
	result := exactExecutionErrorResult(err)
	if reason := status.Convert(err).Message(); isSessionLaunchReason(reason) {
		result.Reason = reason
	}
	if status.Code(err) == codes.Unavailable && result.Reason == "execution_control_unavailable" {
		result.Reason = "launch_unavailable"
	}
	return result
}

var sessionLaunchReasons = map[string]struct{}{
	"invalid_request": {}, "target_not_found": {}, "task_version_changed": {}, "claim_changed": {},
	"primary_changed": {}, "idempotency_payload_mismatch": {}, "task_busy": {}, "task_archived": {},
	"pending_move": {}, "office_task": {}, "passthrough_profile": {}, "launch_ceiling_refused": {},
	"recovery_required": {}, "launch_unavailable": {},
}

func isSessionLaunchReason(reason string) bool {
	_, ok := sessionLaunchReasons[reason]
	return ok
}

// SessionLaunchRequest is the canonical native request of a plugin launch
// command; its Digest is the occurrence identity every surface shares.
func SessionLaunchRequest(in pluginsdk.ExactSessionLaunchCommand) models.ExactSessionLaunchRequest {
	return models.ExactSessionLaunchRequest{
		WorkspaceID: in.WorkspaceID, TaskID: in.TaskID, IdempotencyKey: in.IdempotencyKey,
		Prompt: in.Prompt, AgentProfileID: in.AgentProfileID, MakePrimary: in.MakePrimary,
		ExpectedPrimarySessionID:    in.ExpectedPrimarySessionID,
		ExpectedTaskResourceVersion: in.ExpectedTaskResourceVersion,
	}
}

func validSessionLaunchCommand(in pluginsdk.ExactSessionLaunchCommand) bool {
	if !isBoundedApprovalIdentifier(in.RequestID) || !isBoundedApprovalIdentifier(in.WorkspaceID) ||
		!isBoundedApprovalIdentifier(in.TaskID) || !isBoundedApprovalIdentifier(in.IdempotencyKey) ||
		!isRFC3339ResourceVersion(in.ExpectedTaskResourceVersion) ||
		!validManagementClaimFence(in.ManagementInstanceKey, in.ExpectedClaimGeneration) ||
		in.ApprovalRevision == 0 || len(in.ManifestDigest) != 64 {
		return false
	}
	if in.AgentProfileID != "" && !isBoundedApprovalIdentifier(in.AgentProfileID) {
		return false
	}
	if in.ExpectedPrimarySessionID != nil && *in.ExpectedPrimarySessionID != "" && !isBoundedApprovalIdentifier(*in.ExpectedPrimarySessionID) {
		return false
	}
	return SessionLaunchRequest(in).Validate() == nil
}
