package plugins

import (
	"context"

	"github.com/kdlbs/kandev/pluginsdk"
)

// exactExecutionController is satisfied by backendapp adapters over the native
// task, orchestrator, queue, and lifecycle services.
type ExactExecutionController interface {
	EnsureTaskRun(context.Context, string, pluginsdk.ExactTaskRunCommand) (pluginsdk.ExactTaskRunResult, error)
	StopTaskRun(context.Context, string, pluginsdk.ExactTaskExecutionCommand) (bool, error)
	RecoverSession(context.Context, string, pluginsdk.ExactSessionRecoveryCommand) (pluginsdk.ExactTaskRunResult, error)
	CancelPendingTaskTransition(context.Context, string, pluginsdk.ExactPendingTransitionCommand) (bool, error)
	GetSessionModeContext(context.Context, string, pluginsdk.ExactTaskExecutionCommand) (pluginsdk.SessionModeContext, error)
	SetSessionMode(context.Context, string, pluginsdk.ExactSessionModeCommand) (string, error)
}

type exactExecutionController = ExactExecutionController

// ExactSessionLaunchOutcome is the controller's report of one native exact
// launch operation. FailureCode is the stable native error code of a failed or
// uncertain operation.
type ExactSessionLaunchOutcome struct {
	OperationID   string
	SessionID     string
	SessionState  string
	Outcome       string
	PromptStatus  string
	PrimaryStatus string
	PrimaryReason string
	FailureCode   string
}

// ExactSessionLaunchController is the optional extension of an
// ExactExecutionController that can start a fresh session with an initial
// prompt. A controller that does not implement it leaves the method
// unavailable. Native refusals are returned as gRPC status errors whose
// message is a stable reason code.
type ExactSessionLaunchController interface {
	LaunchFreshSession(context.Context, string, pluginsdk.ExactSessionLaunchCommand) (ExactSessionLaunchOutcome, error)
}
