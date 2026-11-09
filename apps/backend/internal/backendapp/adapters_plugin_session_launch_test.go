package backendapp

import (
	"context"
	"errors"
	"testing"

	"github.com/kandev/kandev/internal/orchestrator"
	"github.com/kandev/kandev/internal/task/models"
	"github.com/kandev/kandev/internal/task/repository/repoerrors"
	"github.com/kdlbs/kandev/pluginsdk"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

type sessionLaunchOrchestratorFake struct {
	exactExecutionOrchestratorFake
	calls  int
	scope  string
	req    models.ExactSessionLaunchRequest
	fence  *models.TaskManagementClaimFence
	result *orchestrator.ExactSessionLaunchResult
	err    error
}

func (f *sessionLaunchOrchestratorFake) LaunchFreshSessionExact(_ context.Context, scope string, req models.ExactSessionLaunchRequest, fence *models.TaskManagementClaimFence) (*orchestrator.ExactSessionLaunchResult, error) {
	f.calls++
	f.scope, f.req, f.fence = scope, req, fence
	return f.result, f.err
}

func sessionLaunchController(torch *sessionLaunchOrchestratorFake, tasks exactExecutionTaskFake) pluginsExactExecutionController {
	return pluginsExactExecutionController{tasks: tasks, orchestrator: torch}
}

func sessionLaunchCommand() pluginsdk.ExactSessionLaunchCommand {
	return pluginsdk.ExactSessionLaunchCommand{
		RequestID: "request-1", WorkspaceID: "workspace-1", TaskID: "task-1", IdempotencyKey: "key-1",
		ExpectedTaskResourceVersion: "2026-09-26T09:00:00Z", ManagementInstanceKey: "lead", ExpectedClaimGeneration: 3,
		Prompt: "Investigate.", MakePrimary: true,
	}
}

func claimedTasks() exactExecutionTaskFake {
	return exactExecutionTaskFake{
		task:           &models.Task{ID: "task-1", WorkspaceID: "workspace-1"},
		session:        &models.TaskSession{ID: "session-new", TaskID: "task-1", State: models.TaskSessionStateWaitingForInput},
		installationID: "installation-1", instanceKey: "lead", claimGeneration: 3,
	}
}

func completedOperation() *orchestrator.ExactSessionLaunchResult {
	return &orchestrator.ExactSessionLaunchResult{Operation: &models.ExactSessionLaunchOperation{
		OperationID: "op-1", SessionID: "session-new", MakePrimary: true, Phase: models.ExactLaunchPhaseCompleted,
		PromptStatus: models.ExactLaunchPromptAccepted, PrimaryStatus: models.ExactLaunchPrimaryApplied,
	}}
}

func TestLaunchFreshSessionMapsTheNativeOperationAndScopesTheKeyToTheInstallation(t *testing.T) {
	torch := &sessionLaunchOrchestratorFake{result: completedOperation()}
	outcome, err := sessionLaunchController(torch, claimedTasks()).LaunchFreshSession(context.Background(), "installation-1", sessionLaunchCommand())
	if err != nil {
		t.Fatalf("LaunchFreshSession: %v", err)
	}
	if outcome.OperationID != "op-1" || outcome.SessionID != "session-new" || outcome.SessionState != "WAITING_FOR_INPUT" ||
		outcome.Outcome != "completed" || outcome.PromptStatus != "accepted" || outcome.PrimaryStatus != "applied" {
		t.Fatalf("outcome = %+v", outcome)
	}
	if torch.scope != "plugin:installation-1" || torch.req.Prompt != "Investigate." || !torch.req.MakePrimary ||
		torch.req.IdempotencyKey != "key-1" || torch.req.ExpectedTaskResourceVersion != "2026-09-26T09:00:00Z" ||
		torch.fence == nil || torch.fence.InstallationID != "installation-1" || torch.fence.InstanceKey != "lead" || torch.fence.Generation != 3 {
		t.Fatalf("native call scope=%q req=%+v fence=%+v", torch.scope, torch.req, torch.fence)
	}
}

func TestLaunchFreshSessionReportsAnUnappliedPrimaryAsPartial(t *testing.T) {
	result := completedOperation()
	result.Operation.PrimaryStatus = models.ExactLaunchPrimaryConflict
	result.Operation.PrimaryReason = models.ExactSessionLaunchPrimaryReason("primary_changed")
	torch := &sessionLaunchOrchestratorFake{result: result}
	outcome, err := sessionLaunchController(torch, claimedTasks()).LaunchFreshSession(context.Background(), "installation-1", sessionLaunchCommand())
	if err != nil || outcome.Outcome != "partial" || outcome.PrimaryStatus != "conflict_not_applied" || outcome.PrimaryReason != "primary_changed" {
		t.Fatalf("outcome = %+v err=%v", outcome, err)
	}
}

func TestLaunchFreshSessionRechecksTheClaimBeforeTheNativeCall(t *testing.T) {
	torch := &sessionLaunchOrchestratorFake{result: completedOperation()}
	command := sessionLaunchCommand()
	command.ExpectedClaimGeneration = 2
	_, err := sessionLaunchController(torch, claimedTasks()).LaunchFreshSession(context.Background(), "installation-1", command)
	if status.Code(err) != codes.Aborted || status.Convert(err).Message() != "claim_changed" {
		t.Fatalf("stale claim error = %v, want Aborted claim_changed", err)
	}
	_, err = sessionLaunchController(torch, claimedTasks()).LaunchFreshSession(context.Background(), "installation-2", sessionLaunchCommand())
	if status.Code(err) != codes.Aborted {
		t.Fatalf("competing installation error = %v, want Aborted", err)
	}
	if torch.calls != 0 {
		t.Fatalf("native launch calls = %d, want none under a changed claim", torch.calls)
	}
}

func TestLaunchFreshSessionMapsNativeRefusalsToStableReasonCodes(t *testing.T) {
	cases := []struct {
		err    error
		code   codes.Code
		reason string
	}{
		{repoerrors.ErrExactLaunchBusy, codes.FailedPrecondition, "task_busy"},
		{repoerrors.ErrExactLaunchTaskArchived, codes.FailedPrecondition, "task_archived"},
		{orchestrator.ErrExactLaunchPendingMove, codes.FailedPrecondition, "pending_move"},
		{repoerrors.ErrTaskVersionConflict, codes.Aborted, "task_version_changed"},
		{repoerrors.ErrTaskOperationConflict, codes.Aborted, "idempotency_payload_mismatch"},
		{repoerrors.ErrExactLaunchBaselineMismatch, codes.Aborted, "primary_changed"},
		{repoerrors.ErrTaskNotFound, codes.NotFound, "target_not_found"},
		{models.ErrInvalidExactSessionLaunch, codes.InvalidArgument, "invalid_request"},
		{orchestrator.ErrExactLaunchUnavailable, codes.Unavailable, "launch_unavailable"},
	}
	for _, tc := range cases {
		torch := &sessionLaunchOrchestratorFake{err: tc.err}
		_, err := sessionLaunchController(torch, claimedTasks()).LaunchFreshSession(context.Background(), "installation-1", sessionLaunchCommand())
		if status.Code(err) != tc.code || status.Convert(err).Message() != tc.reason {
			t.Fatalf("%v mapped to %v, want %v %s", tc.err, err, tc.code, tc.reason)
		}
	}
}

func TestLaunchFreshSessionDoesNotLeakUnrecognisedErrors(t *testing.T) {
	torch := &sessionLaunchOrchestratorFake{err: errors.New("sql: connection refused at /var/lib/kandev.db")}
	_, err := sessionLaunchController(torch, claimedTasks()).LaunchFreshSession(context.Background(), "installation-1", sessionLaunchCommand())
	if status.Code(err) != codes.FailedPrecondition || status.Convert(err).Message() == "" ||
		status.Convert(err).Message() == torch.err.Error() {
		t.Fatalf("unrecognised error = %v, want a sanitized status", err)
	}
}

func TestLaunchFreshSessionIsUnavailableWithoutTheNativeLauncher(t *testing.T) {
	controller := pluginsExactExecutionController{tasks: claimedTasks(), orchestrator: &exactExecutionOrchestratorFake{}}
	_, err := controller.LaunchFreshSession(context.Background(), "installation-1", sessionLaunchCommand())
	if status.Code(err) != codes.Unavailable {
		t.Fatalf("error = %v, want Unavailable", err)
	}
}
