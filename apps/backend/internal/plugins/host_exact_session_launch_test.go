package plugins

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/kandev/kandev/internal/task/models"
	"github.com/kdlbs/kandev/pluginsdk"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

type fakeSessionLaunchController struct {
	*fakeExactExecutionController
	calls     int
	inputs    []pluginsdk.ExactSessionLaunchCommand
	callers   []string
	deadlines []bool
	outcome   ExactSessionLaunchOutcome
	err       error
	errs      []error
}

func (f *fakeSessionLaunchController) LaunchFreshSession(ctx context.Context, installationID string, in pluginsdk.ExactSessionLaunchCommand) (ExactSessionLaunchOutcome, error) {
	_, hasDeadline := ctx.Deadline()
	f.calls++
	f.inputs = append(f.inputs, in)
	f.callers = append(f.callers, installationID)
	f.deadlines = append(f.deadlines, hasDeadline)
	if len(f.errs) > 0 {
		err := f.errs[0]
		f.errs = f.errs[1:]
		if err != nil {
			return ExactSessionLaunchOutcome{}, err
		}
	} else if f.err != nil {
		return ExactSessionLaunchOutcome{}, f.err
	}
	return f.outcome, nil
}

func completedLaunchOutcome() ExactSessionLaunchOutcome {
	return ExactSessionLaunchOutcome{
		OperationID: "op-1", SessionID: "session-new", SessionState: "WAITING_FOR_INPUT",
		Outcome: launchOutcomeCompleted, PromptStatus: "accepted", PrimaryStatus: "applied",
	}
}

func newSessionLaunchHost(t *testing.T, controller *fakeSessionLaunchController) (*pluginHost, pluginsdk.ExactSessionLaunchCommandManager, pluginsdk.ExactSessionLaunchCommand) {
	t.Helper()
	host, digest := newExactExecutionHost(t, controller)
	manager, ok := pluginsdk.HostExactSessionLaunchCommands(host)
	if !ok {
		t.Fatal("exact session launch is not exposed")
	}
	command := pluginsdk.ExactSessionLaunchCommand{
		RequestID: "request-1", WorkspaceID: "workspace-execution", TaskID: "task-1", IdempotencyKey: "launch-1",
		ExpectedTaskResourceVersion: "2026-09-26T09:00:00Z", ManagementInstanceKey: "delivery-lead", ExpectedClaimGeneration: 4,
		ApprovalRevision: 1, ManifestDigest: digest, Prompt: "Investigate the failing build.", MakePrimary: true,
	}
	return host, manager, command
}

func newLaunchController() *fakeSessionLaunchController {
	return &fakeSessionLaunchController{fakeExactExecutionController: &fakeExactExecutionController{}, outcome: completedLaunchOutcome()}
}

func TestExactSessionLaunchMethodUsesTheExistingExecutionGrant(t *testing.T) {
	for _, method := range exactHostMethods {
		if method.method == exactLaunchFreshSessionMethod {
			if method.capability != "host.v2.write:execution" {
				t.Fatalf("launch capability = %q, want the existing execution write grant", method.capability)
			}
			return
		}
	}
	t.Fatal("exact session launch is not in the method registry")
}

func TestExactSessionLaunchAppliesAndReplaysFromTheReceipt(t *testing.T) {
	controller := newLaunchController()
	_, manager, command := newSessionLaunchHost(t, controller)
	ctx := context.Background()

	first, result, err := manager.LaunchFreshSession(ctx, command)
	if err != nil || first.Status != pluginsdk.CommandApplied || result.SessionID != "session-new" ||
		result.Outcome != launchOutcomeCompleted || result.PrimaryStatus != "applied" || result.PromptStatus != "accepted" {
		t.Fatalf("launch = %+v result=%+v err=%v", first, result, err)
	}
	got := controller.inputs[0]
	if got.Prompt != command.Prompt || !got.MakePrimary || got.ManagementInstanceKey != "delivery-lead" || got.ExpectedClaimGeneration != 4 ||
		controller.callers[0] != "execution-installation" || !controller.deadlines[0] {
		t.Fatalf("controller input = %+v caller=%q deadline=%v", got, controller.callers[0], controller.deadlines[0])
	}

	retry := command
	retry.RequestID = "request-lost-ack"
	retry.ExpectedTaskResourceVersion = "2026-09-26T09:30:00Z"
	replayed, replayResult, err := manager.LaunchFreshSession(ctx, retry)
	if err != nil || replayed.Status != pluginsdk.CommandAlreadyApplied || replayResult.SessionID != result.SessionID ||
		replayResult.OperationID != result.OperationID || replayResult.Outcome != launchOutcomeCompleted || replayResult.PrimaryStatus != "applied" {
		t.Fatalf("replay = %+v result=%+v err=%v", replayed, replayResult, err)
	}
	if controller.calls != 2 {
		t.Fatalf("controller calls = %d, want the claim and policy checks to run on the replay", controller.calls)
	}
}

func TestExactSessionLaunchChangedPayloadUnderTheSameKeyConflicts(t *testing.T) {
	controller := newLaunchController()
	_, manager, command := newSessionLaunchHost(t, controller)
	ctx := context.Background()
	if first, _, err := manager.LaunchFreshSession(ctx, command); err != nil || first.Status != pluginsdk.CommandApplied {
		t.Fatalf("first launch = %+v err=%v", first, err)
	}
	mutations := map[string]func(*pluginsdk.ExactSessionLaunchCommand){
		"prompt":       func(c *pluginsdk.ExactSessionLaunchCommand) { c.Prompt = "A different instruction." },
		"make primary": func(c *pluginsdk.ExactSessionLaunchCommand) { c.MakePrimary = false },
		"profile":      func(c *pluginsdk.ExactSessionLaunchCommand) { c.AgentProfileID = "profile-other" },
		"task":         func(c *pluginsdk.ExactSessionLaunchCommand) { c.TaskID = "task-2" },
		"expected prim": func(c *pluginsdk.ExactSessionLaunchCommand) {
			baseline := "session-old"
			c.ExpectedPrimarySessionID = &baseline
		},
	}
	for name, mutate := range mutations {
		changed := command
		changed.RequestID = "request-" + name
		mutate(&changed)
		result, _, err := manager.LaunchFreshSession(ctx, changed)
		if err != nil || result.Status != pluginsdk.CommandConflict || result.Reason != "idempotency_payload_mismatch" {
			t.Fatalf("%s change = %+v err=%v, want idempotency conflict", name, result, err)
		}
	}
	if controller.calls != 1 {
		t.Fatalf("controller calls = %d, want a conflicting payload never to reach the Host", controller.calls)
	}
}

func TestExactSessionLaunchRequestedPrimaryNotAppliedIsPartialAndStaysPartial(t *testing.T) {
	controller := newLaunchController()
	controller.outcome = ExactSessionLaunchOutcome{
		OperationID: "op-2", SessionID: "session-new", SessionState: "WAITING_FOR_INPUT", Outcome: launchOutcomePartial,
		PromptStatus: "accepted", PrimaryStatus: "conflict_not_applied", PrimaryReason: "primary_changed",
	}
	_, manager, command := newSessionLaunchHost(t, controller)
	first, result, err := manager.LaunchFreshSession(context.Background(), command)
	if err != nil || first.Status != pluginsdk.CommandPartial || first.Reason != "primary_changed" ||
		result.Outcome != launchOutcomePartial || result.PrimaryStatus != "conflict_not_applied" || result.SessionID != "session-new" {
		t.Fatalf("partial launch = %+v result=%+v err=%v", first, result, err)
	}
	retry := command
	retry.RequestID = "request-retry"
	replayed, replayResult, err := manager.LaunchFreshSession(context.Background(), retry)
	if err != nil || replayed.Status != pluginsdk.CommandPartial || replayResult.PrimaryReason != "primary_changed" {
		t.Fatalf("partial replay = %+v result=%+v err=%v", replayed, replayResult, err)
	}
}

func TestExactSessionLaunchUncertainPromptIsNeverReportedAsSuccess(t *testing.T) {
	controller := newLaunchController()
	controller.outcome = ExactSessionLaunchOutcome{
		OperationID: "op-3", SessionID: "session-new", SessionState: "RUNNING", Outcome: launchOutcomeUncertain,
		PromptStatus: "uncertain", PrimaryStatus: "pending", FailureCode: "prompt_delivery_uncertain",
	}
	_, manager, command := newSessionLaunchHost(t, controller)
	first, result, err := manager.LaunchFreshSession(context.Background(), command)
	if err != nil || first.Status != pluginsdk.CommandUncertain || result.PromptStatus != "uncertain" || result.Outcome != launchOutcomeUncertain {
		t.Fatalf("uncertain launch = %+v result=%+v err=%v", first, result, err)
	}
	retry := command
	retry.RequestID = "request-retry"
	replayed, replayResult, err := manager.LaunchFreshSession(context.Background(), retry)
	if err != nil || replayed.Status != pluginsdk.CommandUncertain || replayResult.PromptStatus != "uncertain" {
		t.Fatalf("uncertain replay = %+v result=%+v err=%v", replayed, replayResult, err)
	}
}

func TestExactSessionLaunchFailedOperationKeepsItsKeyBurned(t *testing.T) {
	controller := newLaunchController()
	controller.outcome = ExactSessionLaunchOutcome{
		OperationID: "op-4", SessionID: "session-new", SessionState: "FAILED", Outcome: launchOutcomeFailed,
		PromptStatus: "not_delivered", PrimaryStatus: "pending", FailureCode: "session_not_startable",
	}
	_, manager, command := newSessionLaunchHost(t, controller)
	first, result, err := manager.LaunchFreshSession(context.Background(), command)
	if err != nil || first.Status != pluginsdk.CommandConflict || first.Reason != "session_not_startable" || result.PromptStatus != "not_delivered" {
		t.Fatalf("failed launch = %+v result=%+v err=%v", first, result, err)
	}
}

func TestExactSessionLaunchInProgressLeavesTheReceiptOpenForTheSameKey(t *testing.T) {
	controller := newLaunchController()
	controller.outcome = ExactSessionLaunchOutcome{OperationID: "op-5", SessionID: "session-new", SessionState: "STARTING", Outcome: launchOutcomeInProgress, PromptStatus: "pending", PrimaryStatus: "pending"}
	_, manager, command := newSessionLaunchHost(t, controller)
	ctx := context.Background()
	pending, result, err := manager.LaunchFreshSession(ctx, command)
	if err != nil || pending.Status != pluginsdk.CommandUnavailable || pending.Reason != "launch_in_progress" ||
		result.Outcome != launchOutcomeInProgress || result.SessionID != "session-new" || pending.Receipt != nil {
		t.Fatalf("in-progress launch = %+v result=%+v err=%v", pending, result, err)
	}
	controller.outcome = completedLaunchOutcome()
	retry := command
	retry.RequestID = "request-retry"
	done, doneResult, err := manager.LaunchFreshSession(ctx, retry)
	if err != nil || done.Status != pluginsdk.CommandApplied || doneResult.Outcome != launchOutcomeCompleted || done.Receipt == nil {
		t.Fatalf("completed retry = %+v result=%+v err=%v", done, doneResult, err)
	}
}

func TestExactSessionLaunchRefusalsLeaveTheKeyReusable(t *testing.T) {
	cases := []struct {
		name   string
		err    error
		status pluginsdk.CommandStatus
		reason string
	}{
		{"busy", status.Error(codes.FailedPrecondition, "task_busy"), pluginsdk.CommandConflict, "task_busy"},
		{"archived", status.Error(codes.FailedPrecondition, "task_archived"), pluginsdk.CommandConflict, "task_archived"},
		{"pending move", status.Error(codes.FailedPrecondition, "pending_move"), pluginsdk.CommandConflict, "pending_move"},
		{"stale version", status.Error(codes.Aborted, "task_version_changed"), pluginsdk.CommandConflict, "task_version_changed"},
		{"claim changed", status.Error(codes.Aborted, "claim_changed"), pluginsdk.CommandConflict, "claim_changed"},
		{"primary changed", status.Error(codes.Aborted, "primary_changed"), pluginsdk.CommandConflict, "primary_changed"},
		{"wrong workspace", status.Error(codes.NotFound, "target_not_found"), pluginsdk.CommandNotFound, "target_not_found"},
		{"invalid", status.Error(codes.InvalidArgument, "invalid_request"), pluginsdk.CommandInvalid, "invalid_request"},
		{"unexpected", status.Error(codes.Internal, "database exploded at /var/lib"), pluginsdk.CommandUnavailable, "execution_control_unavailable"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			controller := newLaunchController()
			controller.errs = []error{tc.err}
			_, manager, command := newSessionLaunchHost(t, controller)
			ctx := context.Background()
			refused, result, err := manager.LaunchFreshSession(ctx, command)
			if err != nil || refused.Status != tc.status || refused.Reason != tc.reason || result.SessionID != "" {
				t.Fatalf("refusal = %+v result=%+v err=%v, want %s/%s", refused, result, err, tc.status, tc.reason)
			}
			retry := command
			retry.RequestID = "request-after-refusal"
			applied, appliedResult, err := manager.LaunchFreshSession(ctx, retry)
			if err != nil || applied.Status != pluginsdk.CommandApplied || appliedResult.SessionID != "session-new" {
				t.Fatalf("retry after refusal = %+v result=%+v err=%v", applied, appliedResult, err)
			}
		})
	}
}

func TestExactSessionLaunchReplayStillHonoursAChangedClaim(t *testing.T) {
	controller := newLaunchController()
	_, manager, command := newSessionLaunchHost(t, controller)
	ctx := context.Background()
	if first, _, err := manager.LaunchFreshSession(ctx, command); err != nil || first.Status != pluginsdk.CommandApplied {
		t.Fatalf("first launch = %+v err=%v", first, err)
	}
	controller.errs = []error{status.Error(codes.Aborted, "claim_changed")}
	retry := command
	retry.RequestID = "request-after-claim-change"
	result, launch, err := manager.LaunchFreshSession(ctx, retry)
	if err != nil || result.Status != pluginsdk.CommandConflict || result.Reason != "claim_changed" || launch.SessionID != "" {
		t.Fatalf("replay under a changed claim = %+v launch=%+v err=%v", result, launch, err)
	}
	controller.errs = nil
	again, againResult, err := manager.LaunchFreshSession(ctx, command)
	if err != nil || again.Status != pluginsdk.CommandAlreadyApplied || againResult.SessionID != "session-new" {
		t.Fatalf("replay under the original claim = %+v result=%+v err=%v", again, againResult, err)
	}
}

func TestExactSessionLaunchDeniedAndInvalidRequestsNeverReachTheHost(t *testing.T) {
	controller := newLaunchController()
	_, manager, command := newSessionLaunchHost(t, controller)
	ctx := context.Background()

	otherWorkspace := command
	otherWorkspace.WorkspaceID = "workspace-without-grant"
	staleRevision := command
	staleRevision.ApprovalRevision = 7
	wrongManifest := command
	wrongManifest.ManifestDigest = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	for name, denied := range map[string]pluginsdk.ExactSessionLaunchCommand{
		"workspace without approval": otherWorkspace, "stale approval revision": staleRevision, "foreign manifest": wrongManifest,
	} {
		result, _, err := manager.LaunchFreshSession(ctx, denied)
		if err != nil || result.Status != pluginsdk.CommandDenied {
			t.Fatalf("%s = %+v err=%v, want denied", name, result, err)
		}
	}

	empty := command
	empty.Prompt = "   "
	noKey := command
	noKey.IdempotencyKey = ""
	badVersion := command
	badVersion.ExpectedTaskResourceVersion = "yesterday"
	oversized := command
	oversized.Prompt = strings.Repeat("a", models.MaxInitialPromptSubmissionBytes+1)
	badFence := command
	badFence.ManagementInstanceKey, badFence.ExpectedClaimGeneration = "delivery-lead", 0
	for name, invalid := range map[string]pluginsdk.ExactSessionLaunchCommand{
		"blank prompt": empty, "missing key": noKey, "malformed version": badVersion, "oversized prompt": oversized, "bad claim fence": badFence,
	} {
		result, _, err := manager.LaunchFreshSession(ctx, invalid)
		if err != nil || result.Status != pluginsdk.CommandInvalid {
			t.Fatalf("%s = %+v err=%v, want invalid", name, result, err)
		}
	}
	if controller.calls != 0 {
		t.Fatalf("controller calls = %d, want no call for a denied or invalid request", controller.calls)
	}
}

func TestExactSessionLaunchIsUnavailableWithoutAnOptedInController(t *testing.T) {
	plain := &fakeExactExecutionController{}
	host, digest := newExactExecutionHost(t, plain)
	manager, _ := pluginsdk.HostExactSessionLaunchCommands(host)
	result, _, err := manager.LaunchFreshSession(context.Background(), pluginsdk.ExactSessionLaunchCommand{
		RequestID: "r", WorkspaceID: "workspace-execution", TaskID: "task-1", IdempotencyKey: "k",
		ExpectedTaskResourceVersion: time.Now().UTC().Format(time.RFC3339Nano), ApprovalRevision: 1, ManifestDigest: digest, Prompt: "go",
	})
	if err != nil || result.Status != pluginsdk.CommandUnavailable {
		t.Fatalf("launch without controller support = %+v err=%v", result, err)
	}
	capability, err := host.GetCapabilityContext(context.Background(), "workspace-execution")
	if err != nil {
		t.Fatalf("GetCapabilityContext: %v", err)
	}
	if operation := findCapabilityOperation(capability, exactLaunchFreshSessionMethod); operation == nil || operation.Supported ||
		operation.UnavailableReason != "exact_session_launch_unavailable" {
		t.Fatalf("capability without controller support = %+v", operation)
	}

	supported := newLaunchController()
	supportedHost, _ := newExactExecutionHost(t, supported)
	capability, err = supportedHost.GetCapabilityContext(context.Background(), "workspace-execution")
	if err != nil {
		t.Fatalf("GetCapabilityContext: %v", err)
	}
	if operation := findCapabilityOperation(capability, exactLaunchFreshSessionMethod); operation == nil || !operation.Supported || !operation.Authorized {
		t.Fatalf("capability with controller support = %+v", operation)
	}
}

func findCapabilityOperation(capability *pluginsdk.CapabilityContext, method string) *pluginsdk.CapabilityOperation {
	for index := range capability.Operations {
		if capability.Operations[index].Method == method {
			return &capability.Operations[index]
		}
	}
	return nil
}
