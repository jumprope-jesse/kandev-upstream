package orchestrator

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/kandev/kandev/internal/task/models"
)

func (h *exactLaunchHarness) receiptState(sessionID string) string {
	h.t.Helper()
	session, err := h.repo.GetTaskSession(context.Background(), sessionID)
	require.NoError(h.t, err)
	submission, found, err := models.LoadInitialPromptSubmission(session.Metadata)
	require.NoError(h.t, err)
	require.True(h.t, found)
	return submission.State
}

// ownedDriver claims op as a dead owner so a test can drive single steps.
func (h *exactLaunchHarness) ownedDriver(op *models.ExactSessionLaunchOperation) *exactLaunchDriver {
	h.t.Helper()
	claimed, err := h.repo.ClaimExactSessionLaunch(context.Background(), op.OperationID, "dead-owner", time.Now().Add(-time.Hour), exactLaunchLease)
	require.NoError(h.t, err)
	return &exactLaunchDriver{
		s: h.svc, store: h.repo, op: claimed,
		fence: models.ExactSessionLaunchFence{OperationID: op.OperationID, OwnerToken: "dead-owner", OwnerEpoch: claimed.OwnerEpoch},
	}
}

func TestExactLaunchRecordsFirstInstructionBeforeTheProviderAcceptsIt(t *testing.T) {
	h := newExactLaunchHarness(t)
	var atAdmission []mockUserMessage
	var receiptAtAdmission string
	h.mu.Lock()
	h.afterAdmit = func() {
		atAdmission = h.userMessages()
		ops, err := h.repo.ListOpenExactSessionLaunches(context.Background(), 10)
		require.NoError(t, err)
		require.Len(t, ops, 1)
		receiptAtAdmission = h.receiptState(ops[0].SessionID)
	}
	h.mu.Unlock()

	res := h.mustLaunch(h.request("key-order"))
	done := h.awaitPhase(res.Operation.OperationID, exactTerminal)

	require.Equal(t, models.ExactLaunchOutcomeCompleted, done.Outcome())
	require.Len(t, atAdmission, 1, "the instruction is on record before the provider reports acceptance")
	require.Equal(t, exactTestPrompt, atAdmission[0].content)
	require.Equal(t, models.InitialPromptSubmissionDispatching, receiptAtAdmission)
	require.Len(t, h.userMessages(), 1, "completion does not record a second instruction")
	require.Equal(t, models.InitialPromptSubmissionAccepted, h.receiptState(done.SessionID))
}

func TestExactLaunchRefusesDispatchWhenTheFirstInstructionCannotBeRecorded(t *testing.T) {
	h := newExactLaunchHarness(t)
	h.svc.stopExactLaunchRecovery()
	h.messages.mu.Lock()
	h.messages.userMessageErr = errors.New("transcript store unavailable")
	h.messages.mu.Unlock()
	op := h.admitDirect(h.svc, h.request("key-no-message"))

	h.svc.startExactLaunchRecovery(t.Context())
	done := h.awaitPhase(op.OperationID, exactTerminal)
	require.Equal(t, models.ExactLaunchOutcomeFailed, done.Outcome())
	require.Equal(t, models.ExactLaunchPromptNotDelivered, done.PromptStatus)
	require.Zero(t, h.dispatches(), "a prompt is never dispatched ahead of its transcript record")
	require.Equal(t, models.InitialPromptSubmissionPending, h.receiptState(op.SessionID))
}

func TestExactLaunchFlagsTheRecordedInstructionWhenDeliveryIsRefused(t *testing.T) {
	h := newExactLaunchHarness(t)
	h.svc.stopExactLaunchRecovery()
	op := h.admitDirect(h.svc, h.request("key-refused"))
	ctx := context.Background()
	d := h.ownedDriver(op)
	require.NoError(t, d.prepare(ctx))
	require.NoError(t, d.advance(ctx, d.update(models.ExactLaunchPhasePromptDispatched, models.ExactLaunchPromptPending)))
	require.NoError(t, d.ensureFirstMessage(ctx, d.snapshot()))

	require.NoError(t, d.settlePrompt(ctx, exactPromptNotDelivered, exactCodeAdmissionRejected))

	done := h.operation(op.OperationID)
	require.Equal(t, models.ExactLaunchOutcomeFailed, done.Outcome())
	require.Equal(t, models.ExactLaunchPromptNotDelivered, done.PromptStatus)
	messageID := initialSubmissionUserMessageID(exactTestTask, op.SessionID)
	require.Equal(t, models.MessageDeliveryStatusNotDelivered, h.messages.userMessageStatuses[messageID])
}

func TestExactLaunchDoesNotReportNotDeliveredWhenTheMessageFlagFails(t *testing.T) {
	h := newExactLaunchHarness(t)
	h.svc.stopExactLaunchRecovery()
	op := h.admitDirect(h.svc, h.request("key-flag-fails"))
	ctx := context.Background()
	d := h.ownedDriver(op)
	require.NoError(t, d.prepare(ctx))
	require.NoError(t, d.advance(ctx, d.update(models.ExactLaunchPhasePromptDispatched, models.ExactLaunchPromptPending)))
	require.NoError(t, d.ensureFirstMessage(ctx, d.snapshot()))
	h.messages.mu.Lock()
	h.messages.userMessageStatusFn = func(string, string, string) (bool, error) { return false, errors.New("flag write failed") }
	h.messages.mu.Unlock()

	err := d.settlePrompt(ctx, exactPromptNotDelivered, exactCodeAdmissionRejected)

	require.Error(t, err)
	require.False(t, h.operation(op.OperationID).Phase.IsTerminal(), "the operation is not closed over an unflagged instruction")
}

func TestExactLaunchReassertsAcceptedReceiptAfterAFailedAcceptanceWrite(t *testing.T) {
	h := newExactLaunchHarness(t)
	h.svc.stopExactLaunchRecovery()
	op := h.admitDirect(h.svc, h.request("key-reassert"))
	ctx := context.Background()
	d := h.ownedDriver(op)
	require.NoError(t, d.prepare(ctx))
	require.NoError(t, d.advance(ctx, d.update(models.ExactLaunchPhasePromptDispatched, models.ExactLaunchPromptPending)))
	require.NoError(t, d.ensureFirstMessage(ctx, d.snapshot()))
	require.NoError(t, h.svc.transitionInitialSubmissionToDispatching(ctx, op.SessionID, "exec-1", ""))
	// The provider accepted and the operation advanced, but the receipt write
	// failed, so the session receipt still says dispatching.
	require.NoError(t, d.advance(ctx, d.update(models.ExactLaunchPhasePromptAccepted, models.ExactLaunchPromptAccepted)))
	require.Equal(t, models.InitialPromptSubmissionDispatching, h.receiptState(op.SessionID))

	restarted, _ := h.newNode(newExactTaskRepo())
	restarted.startExactLaunchRecovery(t.Context())
	t.Cleanup(restarted.stopExactLaunchRecovery)

	done := h.awaitPhase(op.OperationID, exactTerminal)
	require.Equal(t, models.ExactLaunchOutcomeCompleted, done.Outcome())
	require.Equal(t, models.InitialPromptSubmissionAccepted, h.receiptState(op.SessionID), "operation and session receipts agree")
	require.Zero(t, h.dispatches(), "nothing is resent")
	require.Len(t, h.userMessages(), 1)
}

func TestExactLaunchDoesNotCompleteOverAnInconsistentReceipt(t *testing.T) {
	h := newExactLaunchHarness(t)
	h.svc.stopExactLaunchRecovery()
	op := h.admitDirect(h.svc, h.request("key-inconsistent"))
	ctx := context.Background()
	d := h.ownedDriver(op)
	require.NoError(t, d.prepare(ctx))
	require.NoError(t, d.advance(ctx, d.update(models.ExactLaunchPhasePromptAccepted, models.ExactLaunchPromptAccepted)))
	require.Equal(t, models.InitialPromptSubmissionPending, h.receiptState(op.SessionID))

	err := d.finish(ctx)

	require.Error(t, err, "a receipt that never reached dispatching cannot be completed as accepted")
	require.Equal(t, models.ExactLaunchPhasePromptAccepted, h.operation(op.OperationID).Phase)
	require.Equal(t, models.InitialPromptSubmissionPending, h.receiptState(op.SessionID), "the receipt is reported, never overwritten")
}
