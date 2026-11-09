package backendapp

import (
	"context"

	mcphandlers "github.com/kandev/kandev/internal/mcp/handlers"
	"github.com/kandev/kandev/internal/orchestrator"
	"github.com/kandev/kandev/internal/plugins"
	taskmodels "github.com/kandev/kandev/internal/task/models"
	"github.com/kdlbs/kandev/pluginsdk"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// exactSessionLauncher is the native exact fresh-session launch the
// orchestrator exposes to both the MCP and plugin surfaces.
type exactSessionLauncher interface {
	LaunchFreshSessionExact(context.Context, string, taskmodels.ExactSessionLaunchRequest, *taskmodels.TaskManagementClaimFence) (*orchestrator.ExactSessionLaunchResult, error)
}

// pluginPrincipalScope is the stable idempotency-key scope of a plugin
// installation; it is derived from the host-authenticated installation and
// never from a request field.
func pluginPrincipalScope(installationID string) string {
	return "plugin:" + installationID
}

// LaunchFreshSession runs the native exact launch. The management claim is
// re-checked on every call, including a replay, because the native layer
// verifies the claim only when it admits a new operation.
func (a pluginsExactExecutionController) LaunchFreshSession(ctx context.Context, installationID string, in pluginsdk.ExactSessionLaunchCommand) (plugins.ExactSessionLaunchOutcome, error) {
	launcher, ok := a.orchestrator.(exactSessionLauncher)
	if !ok || a.tasks == nil {
		return plugins.ExactSessionLaunchOutcome{}, status.Error(codes.Unavailable, "launch_unavailable")
	}
	if err := a.withManagementClaimFence(ctx, installationID, in.TaskID, in.ManagementInstanceKey, in.ExpectedClaimGeneration, func() error { return nil }); err != nil {
		return plugins.ExactSessionLaunchOutcome{}, claimFenceRefusal(err)
	}
	request := plugins.SessionLaunchRequest(in)
	fence := &taskmodels.TaskManagementClaimFence{
		InstallationID: installationID, InstanceKey: in.ManagementInstanceKey, Generation: in.ExpectedClaimGeneration,
	}
	result, err := launcher.LaunchFreshSessionExact(ctx, pluginPrincipalScope(installationID), request, fence)
	if err != nil {
		return plugins.ExactSessionLaunchOutcome{}, exactLaunchStatusError(err)
	}
	if result == nil || result.Operation == nil {
		return plugins.ExactSessionLaunchOutcome{}, status.Error(codes.Unavailable, "launch_unavailable")
	}
	return a.exactLaunchOutcome(ctx, result), nil
}

func claimFenceRefusal(err error) error {
	if status.Code(err) == codes.Aborted {
		return status.Error(codes.Aborted, "claim_changed")
	}
	return err
}

func (a pluginsExactExecutionController) exactLaunchOutcome(ctx context.Context, result *orchestrator.ExactSessionLaunchResult) plugins.ExactSessionLaunchOutcome {
	op := result.Operation
	outcome := plugins.ExactSessionLaunchOutcome{
		OperationID: op.OperationID, SessionID: op.SessionID, Outcome: string(result.Outcome()),
		PromptStatus: string(op.PromptStatus), PrimaryStatus: string(op.PrimaryStatus),
		PrimaryReason: string(op.PrimaryReason), FailureCode: op.ErrorCode,
	}
	if op.SessionID != "" {
		if session, err := a.tasks.GetTaskSession(ctx, op.SessionID); err == nil && session != nil {
			outcome.SessionState = string(session.State)
		}
	}
	return outcome
}

// exactLaunchStatusError maps a native refusal to a gRPC status whose message
// is the stable reason code.
func exactLaunchStatusError(err error) error {
	refusal, ok := orchestrator.ClassifyExactLaunchError(err)
	if !ok {
		return normalizeExecutionControlError(err)
	}
	code := codes.FailedPrecondition
	switch refusal.Kind {
	case orchestrator.ExactLaunchRefusalInvalid:
		code = codes.InvalidArgument
	case orchestrator.ExactLaunchRefusalNotFound:
		code = codes.NotFound
	case orchestrator.ExactLaunchRefusalStale:
		code = codes.Aborted
	case orchestrator.ExactLaunchRefusalUnavailable:
		code = codes.Unavailable
	}
	return status.Error(code, refusal.Reason)
}

var _ plugins.ExactSessionLaunchController = pluginsExactExecutionController{}

var (
	_ exactSessionLauncher             = (*orchestrator.Service)(nil)
	_ mcphandlers.ExactSessionLauncher = (*orchestrator.Service)(nil)
)
