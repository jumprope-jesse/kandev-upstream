package pluginsdk

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

type exactSessionLaunchHostFixture struct {
	exactHostFixture
	manager *exactSessionLaunchManagerFixture
}

func (h *exactSessionLaunchHostFixture) ExactSessionLaunchCommands() ExactSessionLaunchCommandManager {
	return h.manager
}

type exactSessionLaunchManagerFixture struct{ got ExactSessionLaunchCommand }

func (f *exactSessionLaunchManagerFixture) LaunchFreshSession(_ context.Context, in ExactSessionLaunchCommand) (*CommandResult, ExactSessionLaunchResult, error) {
	f.got = in
	return &CommandResult{Status: CommandApplied}, ExactSessionLaunchResult{
		OperationID: "op-1", SessionID: "session-1", SessionState: "RUNNING", Outcome: "partial",
		PromptStatus: "accepted", PrimaryStatus: "conflict_not_applied", PrimaryReason: "primary_changed",
	}, nil
}

func launchCommandFixture() ExactSessionLaunchCommand {
	return ExactSessionLaunchCommand{
		RequestID: "request-1", WorkspaceID: "workspace-1", TaskID: "task-1", IdempotencyKey: "launch-1",
		ExpectedTaskResourceVersion: "task-version", ManagementInstanceKey: "delivery-lead", ExpectedClaimGeneration: 7,
		ApprovalRevision: 7, ManifestDigest: "manifest-digest", Prompt: "first instruction",
		AgentProfileID: "profile-1", MakePrimary: true,
	}
}

func TestExactSessionLaunchRoundTripPreservesPrimaryAssertionPresence(t *testing.T) {
	empty := ""
	for name, expected := range map[string]*string{"absent": nil, "asserts none": &empty} {
		t.Run(name, func(t *testing.T) {
			fixture := &exactSessionLaunchHostFixture{manager: &exactSessionLaunchManagerFixture{}}
			host := dialHostOverBufconn(t, fixture)
			manager, ok := HostExactSessionLaunchCommands(host)
			require.True(t, ok)

			command := launchCommandFixture()
			command.ExpectedPrimarySessionID = expected
			result, launch, err := manager.LaunchFreshSession(context.Background(), command)
			require.NoError(t, err)
			require.Equal(t, CommandApplied, result.Status)
			require.Equal(t, "partial", launch.Outcome)
			require.Equal(t, "conflict_not_applied", launch.PrimaryStatus)
			require.Equal(t, "primary_changed", launch.PrimaryReason)
			require.Equal(t, "session-1", launch.SessionID)
			require.Equal(t, command, fixture.manager.got)
		})
	}
}

func TestExactSessionLaunchFailsClosedAgainstAHostWithoutTheExtension(t *testing.T) {
	host := dialHostOverBufconn(t, &exactHostFixture{})
	manager, ok := HostExactSessionLaunchCommands(host)
	require.True(t, ok, "the client always exposes the optional interface; the Host decides support")

	_, _, err := manager.LaunchFreshSession(context.Background(), launchCommandFixture())
	require.Equal(t, codes.Unimplemented, status.Code(err))
}
