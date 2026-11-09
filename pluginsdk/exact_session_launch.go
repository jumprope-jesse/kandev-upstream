package pluginsdk

import "context"

// ExactSessionLaunchCommand starts one fresh session on an existing task with
// Prompt as its first user instruction. IdempotencyKey names the occurrence:
// an identical retry returns the same outcome, and a changed payload under the
// same key conflicts. The management key and generation are the claim
// assertions described by ExactTaskRunCommand.
type ExactSessionLaunchCommand struct {
	RequestID, WorkspaceID, TaskID, IdempotencyKey string
	ExpectedTaskResourceVersion                    string
	ManagementInstanceKey                          string
	ExpectedClaimGeneration                        int64
	ApprovalRevision                               uint64
	ManifestDigest                                 string
	// Prompt is the first user instruction of the new session.
	Prompt string
	// AgentProfileID names the profile to launch with. Empty selects the
	// card's configured default.
	AgentProfileID string
	// MakePrimary asks the Host to promote the new session to primary when the
	// baseline is unchanged and idle.
	MakePrimary bool
	// ExpectedPrimarySessionID, when non-nil, asserts the primary session the
	// caller observed; an empty string asserts that the task has none.
	ExpectedPrimarySessionID *string
}

// ExactSessionLaunchResult reports the durable operation. Outcome is one of
// in_progress, completed, partial, failed, or uncertain. A requested primary
// that was not applied is partial, never completed. PromptStatus is pending,
// accepted, uncertain, or not_delivered; uncertain means the Host cannot prove
// whether the provider received the prompt and will not resend it.
type ExactSessionLaunchResult struct {
	Result        *CommandResult
	OperationID   string
	SessionID     string
	SessionState  string
	Outcome       string
	PromptStatus  string
	PrimaryStatus string
	PrimaryReason string
}

// ExactSessionLaunchCommandManager exposes the exact fresh-session launch.
type ExactSessionLaunchCommandManager interface {
	LaunchFreshSession(context.Context, ExactSessionLaunchCommand) (*CommandResult, ExactSessionLaunchResult, error)
}

// ExactSessionLaunchCommandHost is the optional Host extension for the exact
// fresh-session launch. A Host that does not implement it, or that predates
// the RPC, fails the call closed with an Unimplemented error.
type ExactSessionLaunchCommandHost interface {
	ExactSessionLaunchCommands() ExactSessionLaunchCommandManager
}

// HostExactSessionLaunchCommands returns the optional exact launch extension.
func HostExactSessionLaunchCommands(host Host) (ExactSessionLaunchCommandManager, bool) {
	provider, ok := host.(ExactSessionLaunchCommandHost)
	if !ok {
		return nil, false
	}
	return provider.ExactSessionLaunchCommands(), true
}
