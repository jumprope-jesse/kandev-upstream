package orchestrator

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/kandev/kandev/internal/task/models"
	"github.com/kandev/kandev/internal/task/repository/repoerrors"
)

func TestExactLaunchForeignLiveOwnerBlocksDispatchUntilReleased(t *testing.T) {
	h := newExactLaunchHarness(t)
	h.svc.stopExactLaunchRecovery()
	op := h.admitDirect(h.svc, h.request("key-foreign"))
	ctx := context.Background()
	claimed, err := h.repo.ClaimExactSessionLaunch(ctx, op.OperationID, "foreign", time.Now(), exactLaunchLease)
	require.NoError(t, err)

	h.svc.startExactLaunchRecovery(t.Context())
	require.Never(t, func() bool {
		h.svc.exactLaunchSweepTick(ctx)
		cur := h.operation(op.OperationID)
		return h.dispatches() > 0 || cur.Phase != models.ExactLaunchPhaseAdmitted
	}, 300*time.Millisecond, 20*time.Millisecond, "a live foreign owner is never preempted")

	fence := models.ExactSessionLaunchFence{OperationID: op.OperationID, OwnerToken: "foreign", OwnerEpoch: claimed.OwnerEpoch}
	require.NoError(t, h.repo.ReleaseExactSessionLaunch(ctx, fence))
	h.svc.exactLaunchSweepTick(ctx)
	done := h.awaitPhase(op.OperationID, exactTerminal)
	require.Equal(t, models.ExactLaunchOutcomeCompleted, done.Outcome())
	require.Equal(t, 1, h.dispatches())
}

func TestExactLaunchOldOwnerIsFencedAfterTakeover(t *testing.T) {
	h := newExactLaunchHarness(t)
	h.svc.stopExactLaunchRecovery()
	op := h.admitDirect(h.svc, h.request("key-takeover"))
	ctx := context.Background()
	stale, err := h.repo.ClaimExactSessionLaunch(ctx, op.OperationID, "stale", time.Now().Add(-time.Hour), exactLaunchLease)
	require.NoError(t, err)
	staleFence := models.ExactSessionLaunchFence{OperationID: op.OperationID, OwnerToken: "stale", OwnerEpoch: stale.OwnerEpoch}

	h.svc.startExactLaunchRecovery(t.Context())
	done := h.awaitPhase(op.OperationID, exactTerminal)
	require.Equal(t, models.ExactLaunchOutcomeCompleted, done.Outcome())

	_, err = h.repo.AdvanceExactSessionLaunch(ctx, staleFence, models.ExactLaunchPhaseAdmitted, models.ExactSessionLaunchUpdate{
		Phase: models.ExactLaunchPhaseFailed, PrimaryStatus: models.ExactLaunchPrimaryNotRequested, PromptStatus: models.ExactLaunchPromptNotDelivered,
	}, time.Now())
	require.Error(t, err, "a fenced owner cannot rewrite the operation")
	require.Equal(t, models.ExactLaunchPhaseCompleted, h.operation(op.OperationID).Phase)
	require.Equal(t, 1, h.dispatches())
}

func TestExactLaunchTwoProcessesDispatchOnce(t *testing.T) {
	h := newExactLaunchHarness(t)
	other, _ := h.newNode(newExactTaskRepo())
	other.startExactLaunchRecovery(t.Context())
	t.Cleanup(other.stopExactLaunchRecovery)

	const callers = 8
	results := make([]*ExactSessionLaunchResult, callers)
	errs := make([]error, callers)
	var wg sync.WaitGroup
	for i := range callers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			svc := h.svc
			if i%2 == 1 {
				svc = other
			}
			ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
			defer cancel()
			results[i], errs[i] = svc.LaunchFreshSessionExact(ctx, exactTestPrincipal, h.request("key-two-nodes"), nil)
		}()
	}
	wg.Wait()

	for i := range callers {
		require.NoError(t, errs[i])
		require.Equal(t, results[0].Operation.OperationID, results[i].Operation.OperationID)
		require.Equal(t, models.ExactLaunchOutcomeCompleted, results[i].Outcome())
	}
	sessions, err := h.repo.ListTaskSessions(t.Context(), exactTestTask)
	require.NoError(t, err)
	require.Len(t, sessions, 1)
	require.Equal(t, 1, h.dispatches())
	require.Len(t, h.userMessages(), 1)
}

func TestExactLaunchCompetingReconcilerDuringDispatchDoesNotDispatchAgain(t *testing.T) {
	h := newExactLaunchHarness(t)
	other, _ := h.newNode(newExactTaskRepo())
	other.startExactLaunchRecovery(t.Context())
	t.Cleanup(other.stopExactLaunchRecovery)
	h.afterAdmit = func() {
		ops, err := h.repo.ListOpenExactSessionLaunches(context.Background(), 10)
		if err != nil {
			return
		}
		for _, op := range ops {
			other.runExactLaunch(context.Background(), op.OperationID)
		}
	}

	res := h.mustLaunch(h.request("key-competing"))
	require.Equal(t, models.ExactLaunchOutcomeCompleted, res.Outcome())
	require.Equal(t, 1, h.dispatches())
	require.Len(t, h.userMessages(), 1)
}

func TestExactLaunchPrimaryChangedConcurrentlyIsNotStolen(t *testing.T) {
	h := newExactLaunchHarness(t)
	h.seedSessionState("old", models.TaskSessionStateCompleted, true)
	h.seedSessionState("rival", models.TaskSessionStateCompleted, false)
	h.afterAdmit = func() { require.NoError(t, h.repo.SetSessionPrimary(context.Background(), "rival")) }
	req := h.request("key-race")
	req.MakePrimary = true
	baseline := "old"
	req.ExpectedPrimarySessionID = &baseline

	res := h.mustLaunch(req)
	require.Equal(t, models.ExactLaunchOutcomePartial, res.Outcome())
	require.Equal(t, models.ExactLaunchPrimaryConflict, res.Operation.PrimaryStatus)
	require.Equal(t, models.ExactLaunchReasonPrimaryChanged, res.Operation.PrimaryReason)
	require.Equal(t, models.ExactLaunchPromptAccepted, res.Operation.PromptStatus, "the prompt is still delivered")
	rival, err := h.repo.GetTaskSession(t.Context(), "rival")
	require.NoError(t, err)
	require.True(t, rival.IsPrimary)
}

func TestExactLaunchBaselineBecomingActiveBeforeCommitIsNotDisplaced(t *testing.T) {
	cases := map[string]func(h *exactLaunchHarness){
		"state change": func(h *exactLaunchHarness) {
			require.NoError(h.t, h.repo.UpdateTaskSessionState(context.Background(), "idle", models.TaskSessionStateRunning, ""))
		},
		"new turn": func(h *exactLaunchHarness) {
			now := time.Now().UTC()
			require.NoError(h.t, h.repo.CreateTurn(context.Background(), &models.Turn{
				ID: "turn-late", TaskSessionID: "idle", TaskID: exactTestTask, StartedAt: now, CreatedAt: now, UpdatedAt: now,
			}))
		},
	}
	for name, activate := range cases {
		t.Run(name, func(t *testing.T) {
			h := newExactLaunchHarness(t)
			h.seedSessionState("idle", models.TaskSessionStateWaitingForInput, true)
			h.afterAdmit = func() { activate(h) }
			req := h.request("key-active")
			req.MakePrimary = true
			baseline := "idle"
			req.ExpectedPrimarySessionID = &baseline

			res := h.mustLaunch(req)
			require.Equal(t, models.ExactLaunchOutcomePartial, res.Outcome())
			require.Equal(t, models.ExactLaunchReasonBaselineActive, res.Operation.PrimaryReason)
			idle, err := h.repo.GetTaskSession(t.Context(), "idle")
			require.NoError(t, err)
			require.True(t, idle.IsPrimary, "a session that began work is not displaced")
		})
	}
}

func TestExactLaunchExpectedPrimaryMismatchIsRefusedAtAdmission(t *testing.T) {
	h := newExactLaunchHarness(t)
	h.seedSessionState("old", models.TaskSessionStateCompleted, true)
	req := h.request("key-mismatch")
	req.MakePrimary = true
	wrong := "someone-else"
	req.ExpectedPrimarySessionID = &wrong
	_, err := h.launch(req)
	require.ErrorIs(t, err, repoerrors.ErrExactLaunchBaselineMismatch)
	h.requireNoOperation("key-mismatch")
}
