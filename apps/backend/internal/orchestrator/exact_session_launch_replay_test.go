package orchestrator

import (
	"context"
	"sync"
	"testing"

	"github.com/kandev/kandev/internal/task/models"
	"github.com/kandev/kandev/internal/task/repository/repoerrors"
	"github.com/stretchr/testify/require"
)

func TestExactLaunchEndedSessionLaunchesFreshWithoutPrimaryByDefault(t *testing.T) {
	h := newExactLaunchHarness(t)
	h.seedSessionState("old", models.TaskSessionStateCompleted, true)

	res := h.mustLaunch(h.request("key-ended"))
	require.Equal(t, models.ExactLaunchOutcomeCompleted, res.Outcome())
	require.Equal(t, models.ExactLaunchPrimaryNotRequested, res.Operation.PrimaryStatus)
	require.NotEqual(t, "old", res.Operation.SessionID)

	old, err := h.repo.GetTaskSession(t.Context(), "old")
	require.NoError(t, err)
	require.True(t, old.IsPrimary, "an omitted make_primary preserves the established selection")
}

func TestExactLaunchEndedSessionMakePrimaryReplacesBaseline(t *testing.T) {
	h := newExactLaunchHarness(t)
	h.seedSessionState("old", models.TaskSessionStateFailed, true)
	req := h.request("key-primary")
	req.MakePrimary = true
	baseline := "old"
	req.ExpectedPrimarySessionID = &baseline

	res := h.mustLaunch(req)
	require.Equal(t, models.ExactLaunchOutcomeCompleted, res.Outcome())
	require.Equal(t, models.ExactLaunchPrimaryApplied, res.Operation.PrimaryStatus)

	fresh, err := h.repo.GetTaskSession(t.Context(), res.Operation.SessionID)
	require.NoError(t, err)
	require.True(t, fresh.IsPrimary)
	old, err := h.repo.GetTaskSession(t.Context(), "old")
	require.NoError(t, err)
	require.False(t, old.IsPrimary)
}

func TestExactLaunchIdenticalRetryReturnsTheSameOutcome(t *testing.T) {
	h := newExactLaunchHarness(t)
	first := h.mustLaunch(h.request("key-retry"))
	second := h.mustLaunch(h.request("key-retry"))

	require.True(t, second.Replayed)
	require.Equal(t, first.Operation.OperationID, second.Operation.OperationID)
	require.Equal(t, first.Operation.SessionID, second.Operation.SessionID)
	require.Equal(t, 1, h.dispatches())
	require.Len(t, h.userMessages(), 1)
}

func TestExactLaunchChangedPayloadUnderSameKeyConflicts(t *testing.T) {
	h := newExactLaunchHarness(t)
	h.mustLaunch(h.request("key-conflict"))

	changed := h.request("key-conflict")
	changed.Prompt = "a different instruction"
	_, err := h.launch(changed)
	require.ErrorIs(t, err, repoerrors.ErrTaskOperationConflict)

	primary := h.request("key-conflict")
	primary.MakePrimary = true
	_, err = h.launch(primary)
	require.ErrorIs(t, err, repoerrors.ErrTaskOperationConflict, "make_primary is part of the digest")
	require.Equal(t, 1, h.dispatches())
}

func TestExactLaunchConcurrentDuplicatesCreateOneSession(t *testing.T) {
	h := newExactLaunchHarness(t)
	const callers = 8
	results := make([]*ExactSessionLaunchResult, callers)
	errs := make([]error, callers)
	var wg sync.WaitGroup
	for i := range callers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			results[i], errs[i] = h.launch(h.request("key-concurrent"))
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

func TestExactLaunchLostAckReplayReturnsTheCompletedOutcome(t *testing.T) {
	h := newExactLaunchHarness(t)
	ctx, cancel := context.WithCancel(t.Context())
	h.afterAdmit = cancel
	res, err := h.svc.LaunchFreshSessionExact(ctx, exactTestPrincipal, h.request("key-ack"), nil)
	require.NoError(t, err)
	require.False(t, res.Operation.Phase.IsTerminal(), "the caller's context ended before the outcome")

	replay := h.mustLaunch(h.request("key-ack"))
	require.Equal(t, res.Operation.OperationID, replay.Operation.OperationID)
	require.Equal(t, models.ExactLaunchOutcomeCompleted, replay.Outcome())
	require.Equal(t, 1, h.dispatches())
	require.Len(t, h.userMessages(), 1)
}

func TestExactLaunchKeysAreScopedToThePrincipal(t *testing.T) {
	h := newExactLaunchHarness(t)
	first := h.mustLaunch(h.request("shared-key"))

	own, err := h.repo.GetExactSessionLaunchByKey(t.Context(), exactTestPrincipal, exactTestWorkspace, "shared-key")
	require.NoError(t, err)
	require.Equal(t, first.Operation.OperationID, own.OperationID)
	_, err = h.repo.GetExactSessionLaunchByKey(t.Context(), "user:bob", exactTestWorkspace, "shared-key")
	require.ErrorIs(t, err, repoerrors.ErrExactLaunchNotFound)

	// The fresh session is live, so a second principal is refused as busy
	// rather than served the first principal's operation.
	_, err = h.svc.LaunchFreshSessionExact(t.Context(), "user:bob", h.request("shared-key"), nil)
	require.ErrorIs(t, err, repoerrors.ErrExactLaunchBusy)
}
