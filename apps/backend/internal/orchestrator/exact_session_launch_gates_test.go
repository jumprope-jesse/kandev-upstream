package orchestrator

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/kandev/kandev/internal/orchestrator/executor"
	"github.com/kandev/kandev/internal/orchestrator/messagequeue"
	"github.com/kandev/kandev/internal/task/models"
	"github.com/kandev/kandev/internal/task/repository/repoerrors"
	v1 "github.com/kandev/kandev/pkg/api/v1"
)

var errExactDenied = errors.New("denied")

func (h *exactLaunchHarness) requireNoOperation(key string) {
	h.t.Helper()
	_, err := h.repo.GetExactSessionLaunchByKey(context.Background(), exactTestPrincipal, exactTestWorkspace, key)
	require.ErrorIs(h.t, err, repoerrors.ErrExactLaunchNotFound)
	require.Zero(h.t, h.dispatches())
}

func (h *exactLaunchHarness) requireRefused(key string, want error) {
	h.t.Helper()
	_, err := h.launch(h.request(key))
	require.ErrorIs(h.t, err, want)
	h.requireNoOperation(key)
}

func TestExactLaunchUnauthorizedCallerCreatesNothing(t *testing.T) {
	h := newExactLaunchHarness(t)
	h.svc.SetTaskAccessChecker(func(context.Context, string) error { return errExactDenied })
	h.svc.SetTaskPromptChecker(func(context.Context, string) error { return nil })
	h.requireRefused("key-read-denied", errExactDenied)

	h.svc.SetTaskAccessChecker(func(context.Context, string) error { return nil })
	h.svc.SetTaskPromptChecker(func(context.Context, string) error { return errExactDenied })
	h.requireRefused("key-prompt-denied", errExactDenied)
}

func TestExactLaunchReplayStillRunsAuthorization(t *testing.T) {
	h := newExactLaunchHarness(t)
	h.mustLaunch(h.request("key-auth-replay"))

	h.svc.SetTaskPromptChecker(func(context.Context, string) error { return errExactDenied })
	_, err := h.launch(h.request("key-auth-replay"))
	require.ErrorIs(t, err, errExactDenied, "a retry is authorized against the current grant")
}

func TestExactLaunchWrongWorkspaceIsNotFound(t *testing.T) {
	h := newExactLaunchHarness(t)
	req := h.request("key-wrong-ws")
	req.WorkspaceID = "ws-other"
	_, err := h.launch(req)
	require.ErrorIs(t, err, repoerrors.ErrTaskNotFound)
	require.Zero(t, h.dispatches())
}

func TestExactLaunchStaleResourceVersionConflicts(t *testing.T) {
	h := newExactLaunchHarness(t)
	req := h.request("key-stale")
	req.ExpectedTaskResourceVersion = time.Date(2001, 1, 1, 0, 0, 0, 0, time.UTC).Format(time.RFC3339Nano)
	_, err := h.launch(req)
	require.ErrorIs(t, err, repoerrors.ErrTaskVersionConflict)
	h.requireNoOperation("key-stale")
}

func TestExactLaunchArchivedCardIsRefused(t *testing.T) {
	h := newExactLaunchHarness(t)
	require.NoError(t, h.repo.ArchiveTask(t.Context(), exactTestTask))
	h.requireRefused("key-archived", repoerrors.ErrExactLaunchTaskArchived)
}

func TestExactLaunchRefusesWhileTheCardIsBusy(t *testing.T) {
	for _, state := range []models.TaskSessionState{
		models.TaskSessionStateCreated, models.TaskSessionStateStarting, models.TaskSessionStateRunning,
	} {
		t.Run(string(state), func(t *testing.T) {
			h := newExactLaunchHarness(t)
			h.seedSessionState("live", state, true)
			h.requireRefused("key-busy", repoerrors.ErrExactLaunchBusy)
		})
	}
}

func TestExactLaunchRefusesOpenTurn(t *testing.T) {
	h := newExactLaunchHarness(t)
	h.seedSessionState("idle", models.TaskSessionStateWaitingForInput, true)
	now := time.Now().UTC()
	require.NoError(t, h.repo.CreateTurn(t.Context(), &models.Turn{
		ID: "turn-open", TaskSessionID: "idle", TaskID: exactTestTask, StartedAt: now, CreatedAt: now, UpdatedAt: now,
	}))
	h.requireRefused("key-turn", repoerrors.ErrExactLaunchBusy)
}

func TestExactLaunchRefusesInMemoryActivity(t *testing.T) {
	cases := map[string]func(ta *turnActivity){
		"prompt in flight": func(ta *turnActivity) { ta.promptInFlight = true },
		"background work":  func(ta *turnActivity) { ta.background["bg"] = backgroundWork{} },
	}
	for name, mark := range cases {
		t.Run(name, func(t *testing.T) {
			h := newExactLaunchHarness(t)
			h.seedSessionState("idle", models.TaskSessionStateWaitingForInput, true)
			ta := h.svc.lockTurnActivity("idle", true)
			mark(ta)
			ta.mu.Unlock()
			h.requireRefused("key-memory", repoerrors.ErrExactLaunchBusy)
		})
	}
}

func TestExactLaunchRefusesPendingMove(t *testing.T) {
	h := newExactLaunchHarness(t)
	h.seedSessionState("idle", models.TaskSessionStateWaitingForInput, true)
	require.NoError(t, h.svc.messageQueue.SetPendingMove(t.Context(), "idle", &messagequeue.PendingMove{
		TaskID: exactTestTask, WorkflowID: "wf1", WorkflowStepID: "step-next",
	}))
	h.requireRefused("key-move", ErrExactLaunchPendingMove)
}

func TestExactLaunchRefusesOpenRecoveryBlock(t *testing.T) {
	h := newExactLaunchHarness(t)
	h.seedSessionState("idle", models.TaskSessionStateWaitingForInput, true)
	session, err := h.repo.GetTaskSession(t.Context(), "idle")
	require.NoError(t, err)
	incarnation := session.QueueIncarnationID
	if incarnation == "" {
		incarnation = session.ID
	}
	require.NoError(t, h.repo.UpsertSessionRecoveryBlock(t.Context(), &models.SessionRecoveryBlock{
		SessionID: "idle", IncarnationID: incarnation, Reason: "continuity_unverified", State: models.RecoveryBlockOpen,
	}))
	_, err = h.launch(h.request("key-block"))
	var blocked *sessionRecoveryRequiredError
	require.ErrorAs(t, err, &blocked)
	h.requireNoOperation("key-block")
}

func TestExactLaunchRefusesOfficeCards(t *testing.T) {
	h := newExactLaunchHarness(t)
	now := time.Now().UTC()
	require.NoError(t, h.repo.CreateTask(t.Context(), &models.Task{
		ID: "task-office", WorkspaceID: exactTestWorkspace, WorkflowID: "wf1", Title: "Office",
		State: v1.TaskStateInProgress, ProjectID: "project-office", CreatedAt: now, UpdatedAt: now,
	}))
	req := h.request("key-office")
	req.TaskID = "task-office"
	_, err := h.launch(req)
	require.ErrorIs(t, err, ErrExactLaunchOfficeTask)
}

func TestExactLaunchRefusesPassthroughProfiles(t *testing.T) {
	h := newExactLaunchHarness(t)
	h.manager.resolveProfileInfo = &executor.AgentProfileInfo{ProfileID: exactTestProfile, CLIPassthrough: true}
	h.requireRefused("key-passthrough", ErrExactLaunchPassthrough)
}

func TestExactLaunchIdleWaitingSessionWithNoWorkLaunchesFresh(t *testing.T) {
	h := newExactLaunchHarness(t)
	h.seedSessionState("idle", models.TaskSessionStateWaitingForInput, true)
	res := h.mustLaunch(h.request("key-idle"))
	require.Equal(t, models.ExactLaunchOutcomeCompleted, res.Outcome())
	require.NotEqual(t, "idle", res.Operation.SessionID)
	idle, err := h.repo.GetTaskSession(t.Context(), "idle")
	require.NoError(t, err)
	require.Equal(t, models.TaskSessionStateWaitingForInput, idle.State, "the idle session is never interrupted")
}

func TestExactLaunchCeilingRefusalDoesNotDefer(t *testing.T) {
	ctx := withExactLaunchCeilingRefusal(t.Context())
	require.True(t, exactLaunchCeilingRefusal(ctx))
	require.False(t, exactLaunchCeilingRefusal(t.Context()))
	require.Equal(t, launchOriginAutomatic, launchOriginForStart(ctx, false))
	require.Equal(t, launchOriginAutomatic, launchOriginForStart(ctx, true))
}
