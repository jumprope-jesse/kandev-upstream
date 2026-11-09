package orchestrator

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/kandev/kandev/internal/task/models"
)

func TestExactLaunchProviderFailureBeforeDispatchIsNotDelivered(t *testing.T) {
	h := newExactLaunchHarness(t)
	h.seedSessionState("old", models.TaskSessionStateCompleted, true)
	h.setMode(exactProviderFailBeforeDispatch)
	req := h.request("key-fail")
	req.MakePrimary = true
	baseline := "old"
	req.ExpectedPrimarySessionID = &baseline

	res := h.mustLaunch(req)
	require.Equal(t, models.ExactLaunchOutcomeFailed, res.Outcome())
	require.Equal(t, models.ExactLaunchPromptNotDelivered, res.Operation.PromptStatus)
	require.Equal(t, models.ExactLaunchPrimaryConflict, res.Operation.PrimaryStatus, "a requested primary is never reported as pending or applied")
	require.Zero(t, h.dispatches())
	require.Empty(t, h.userMessages(), "no first message is recorded for a prompt that was not delivered")
	old, err := h.repo.GetTaskSession(t.Context(), "old")
	require.NoError(t, err)
	require.True(t, old.IsPrimary)
}

func TestExactPromptSignalClassifiesFailuresByDispatchEvidence(t *testing.T) {
	cases := []struct {
		name string
		run  func(g *exactPromptSignal)
		want exactPromptOutcome
		code string
	}{
		{"failure before dispatch", func(g *exactPromptSignal) { g.failed(exactCodeLaunchFailed) }, exactPromptNotDelivered, exactCodeLaunchFailed},
		{"failure after dispatch", func(g *exactPromptSignal) {
			require.NoError(t, g.enterDispatch())
			g.failed(exactCodeLaunchFailed)
		}, exactPromptUncertain, exactCodeDispatchUncertain},
		{"provider rejected the admission", func(g *exactPromptSignal) {
			require.NoError(t, g.enterDispatch())
			g.rejectAdmission()
			g.failed(exactCodeLaunchFailed)
		}, exactPromptNotDelivered, exactCodeAdmissionRejected},
		{"acceptance wins over a later failure", func(g *exactPromptSignal) {
			require.NoError(t, g.enterDispatch())
			g.accepted()
			g.failed(exactCodeLaunchFailed)
		}, exactPromptAccepted, ""},
		{"failure wins over a later acceptance", func(g *exactPromptSignal) {
			g.failed(exactCodeLaunchFailed)
			g.accepted()
		}, exactPromptNotDelivered, exactCodeLaunchFailed},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			g := newExactPromptSignal()
			tc.run(g)
			outcome, code := g.result()
			require.Equal(t, tc.want, outcome)
			require.Equal(t, tc.code, code)
		})
	}
}

func TestExactPromptSignalRefusesDispatchAfterTheGateCloses(t *testing.T) {
	g := newExactPromptSignal()
	outcome, _ := g.closeGate(exactCodeShutdown, false)
	require.Equal(t, exactPromptOpen, outcome, "an undispatched operation stays open for a restart")
	require.ErrorIs(t, g.enterDispatch(), errExactPromptGateClosed)
}

func TestExactLaunchShutdownAfterDispatchIsUncertainAndNeverResent(t *testing.T) {
	h := newExactLaunchHarness(t)
	h.setMode(exactProviderDispatchThenHang)

	callerCtx, cancelCaller := context.WithCancel(t.Context())
	type launched struct {
		res *ExactSessionLaunchResult
		err error
	}
	done := make(chan launched, 1)
	go func() {
		res, err := h.svc.LaunchFreshSessionExact(callerCtx, exactTestPrincipal, h.request("key-hang"), nil)
		done <- launched{res, err}
	}()
	require.Eventually(t, func() bool { return h.dispatches() == 1 }, 10*time.Second, 5*time.Millisecond)
	cancelCaller()
	first := <-done
	require.NoError(t, first.err)
	require.False(t, first.res.Operation.Phase.IsTerminal())

	h.svc.stopExactLaunchRecovery()
	op := h.operation(first.res.Operation.OperationID)
	require.Equal(t, models.ExactLaunchOutcomeUncertain, op.Outcome())
	require.Equal(t, models.ExactLaunchPromptUncertain, op.PromptStatus)

	h.setMode(exactProviderAccept)
	h.svc.startExactLaunchRecovery(t.Context())
	replay := h.mustLaunch(h.request("key-hang"))
	require.Equal(t, models.ExactLaunchOutcomeUncertain, replay.Outcome(), "the uncertainty is reported, not retried into success")
	require.Equal(t, 1, h.dispatches(), "the prompt is never resent")
	require.Empty(t, h.userMessages())
}

func TestExactLaunchRecoversFromDurableFrozenInputAfterRestart(t *testing.T) {
	h := newExactLaunchHarness(t)
	h.svc.stopExactLaunchRecovery()
	h.taskRepo.tasks[exactTestTask].Metadata = map[string]interface{}{models.MetaKeyAgentProfileID: "profile-frozen"}
	req := h.request("key-restart")
	req.AgentProfileID = ""
	op := h.admitDirect(h.svc, req)
	require.Equal(t, models.ExactLaunchPhaseAdmitted, op.Phase)
	require.Equal(t, "profile-frozen", op.FrozenInput.AgentProfileID)

	// A new process: the request is gone and the defaults have changed.
	changed := newExactTaskRepo()
	changed.tasks[exactTestTask].Metadata = map[string]interface{}{models.MetaKeyAgentProfileID: "profile-changed"}
	restarted, _ := h.newNode(changed)
	restarted.startExactLaunchRecovery(t.Context())
	t.Cleanup(restarted.stopExactLaunchRecovery)

	done := h.awaitPhase(op.OperationID, exactTerminal)
	require.Equal(t, models.ExactLaunchOutcomeCompleted, done.Outcome())
	require.Equal(t, []string{"profile-frozen"}, h.profilesLaunched())
	require.Equal(t, 1, h.dispatches())
	msgs := h.userMessages()
	require.Len(t, msgs, 1)
	require.Equal(t, exactTestPrompt, msgs[0].content)
	session, err := h.repo.GetTaskSession(t.Context(), op.SessionID)
	require.NoError(t, err)
	require.Equal(t, "profile-frozen", session.AgentProfileID)
}

func TestExactLaunchCrashAfterDispatchIsUncertainAfterRestart(t *testing.T) {
	h := newExactLaunchHarness(t)
	h.svc.stopExactLaunchRecovery()
	op := h.admitDirect(h.svc, h.request("key-crash"))

	// The previous process claimed the operation, created the session and
	// fenced the prompt as dispatching, then died: its lease is long expired.
	ctx := context.Background()
	claimed, err := h.repo.ClaimExactSessionLaunch(ctx, op.OperationID, "dead-owner", time.Now().Add(-time.Hour), exactLaunchLease)
	require.NoError(t, err)
	d := &exactLaunchDriver{
		s: h.svc, store: h.repo, op: claimed,
		fence: models.ExactSessionLaunchFence{OperationID: op.OperationID, OwnerToken: "dead-owner", OwnerEpoch: claimed.OwnerEpoch},
	}
	require.NoError(t, d.prepare(ctx))
	require.NoError(t, d.advance(ctx, d.update(models.ExactLaunchPhasePromptDispatched, models.ExactLaunchPromptPending)))
	require.NoError(t, h.svc.transitionInitialSubmissionToDispatching(ctx, op.SessionID, "exec-dead", ""))

	restarted, _ := h.newNode(newExactTaskRepo())
	restarted.startExactLaunchRecovery(t.Context())
	t.Cleanup(restarted.stopExactLaunchRecovery)

	done := h.awaitPhase(op.OperationID, exactTerminal)
	require.Equal(t, models.ExactLaunchOutcomeUncertain, done.Outcome())
	require.Equal(t, models.ExactLaunchPromptUncertain, done.PromptStatus)
	require.Equal(t, exactCodeDispatchUncertain, done.ErrorCode)
	require.Zero(t, h.dispatches(), "recovery never resends a prompt that may have been delivered")
	require.Empty(t, h.userMessages())
}

func TestExactLaunchStopThenStartResumesAnUndispatchedOperation(t *testing.T) {
	h := newExactLaunchHarness(t)
	h.svc.stopExactLaunchRecovery()
	op := h.admitDirect(h.svc, h.request("key-resume"))

	h.svc.startExactLaunchRecovery(t.Context())
	done := h.awaitPhase(op.OperationID, exactTerminal)
	require.Equal(t, models.ExactLaunchOutcomeCompleted, done.Outcome())
	require.Equal(t, 1, h.dispatches())
}
