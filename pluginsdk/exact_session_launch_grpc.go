package pluginsdk

import (
	"context"

	pluginv1 "github.com/kdlbs/kandev/pluginsdk/proto/kandev/plugin/v1"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

type grpcExactSessionLaunchCommandManager struct{ client pluginv1.HostClient }

func (h *grpcHostClient) ExactSessionLaunchCommands() ExactSessionLaunchCommandManager {
	return grpcExactSessionLaunchCommandManager{client: h.client}
}

func (m grpcExactSessionLaunchCommandManager) LaunchFreshSession(ctx context.Context, in ExactSessionLaunchCommand) (*CommandResult, ExactSessionLaunchResult, error) {
	resp, err := m.client.LaunchFreshSessionExact(ctx, &pluginv1.LaunchFreshSessionExactRequest{
		RequestId: in.RequestID, WorkspaceId: in.WorkspaceID, TaskId: in.TaskID,
		IdempotencyKey: in.IdempotencyKey, ExpectedTaskResourceVersion: in.ExpectedTaskResourceVersion,
		ApprovalRevision: in.ApprovalRevision, ManifestDigest: in.ManifestDigest,
		ManagementInstanceKey: in.ManagementInstanceKey, ExpectedClaimGeneration: in.ExpectedClaimGeneration,
		Prompt: in.Prompt, AgentProfileId: in.AgentProfileID, MakePrimary: in.MakePrimary,
		ExpectedPrimarySessionId: in.ExpectedPrimarySessionID,
	})
	if err != nil {
		return nil, ExactSessionLaunchResult{}, err
	}
	result := commandResultFromProto(resp.GetResult())
	return result, ExactSessionLaunchResult{
		Result: result, OperationID: resp.GetOperationId(), SessionID: resp.GetSessionId(),
		SessionState: resp.GetSessionState(), Outcome: resp.GetOutcome(), PromptStatus: resp.GetPromptStatus(),
		PrimaryStatus: resp.GetPrimaryStatus(), PrimaryReason: resp.GetPrimaryReason(),
	}, nil
}

func (s *grpcHostServer) LaunchFreshSessionExact(ctx context.Context, req *pluginv1.LaunchFreshSessionExactRequest) (*pluginv1.LaunchFreshSessionExactResponse, error) {
	if s.sessionLaunchCommands == nil {
		return nil, status.Error(codes.Unimplemented, "exact session launch is unavailable")
	}
	var expectedPrimary *string
	if req.ExpectedPrimarySessionId != nil {
		value := req.GetExpectedPrimarySessionId()
		expectedPrimary = &value
	}
	result, launch, err := s.sessionLaunchCommands.ExactSessionLaunchCommands().LaunchFreshSession(ctx, ExactSessionLaunchCommand{
		RequestID: req.GetRequestId(), WorkspaceID: req.GetWorkspaceId(), TaskID: req.GetTaskId(),
		IdempotencyKey: req.GetIdempotencyKey(), ExpectedTaskResourceVersion: req.GetExpectedTaskResourceVersion(),
		ManagementInstanceKey: req.GetManagementInstanceKey(), ExpectedClaimGeneration: req.GetExpectedClaimGeneration(),
		ApprovalRevision: req.GetApprovalRevision(), ManifestDigest: req.GetManifestDigest(),
		Prompt: req.GetPrompt(), AgentProfileID: req.GetAgentProfileId(), MakePrimary: req.GetMakePrimary(),
		ExpectedPrimarySessionID: expectedPrimary,
	})
	if err != nil {
		return nil, err
	}
	return &pluginv1.LaunchFreshSessionExactResponse{
		Result: commandResultToProto(result), OperationId: launch.OperationID, SessionId: launch.SessionID,
		SessionState: launch.SessionState, Outcome: launch.Outcome, PromptStatus: launch.PromptStatus,
		PrimaryStatus: launch.PrimaryStatus, PrimaryReason: launch.PrimaryReason,
	}, nil
}

var _ ExactSessionLaunchCommandManager = grpcExactSessionLaunchCommandManager{}
