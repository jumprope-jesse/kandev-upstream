package orchestrator

import (
	"context"
	"errors"
	"fmt"
	"time"

	"go.uber.org/zap"

	"github.com/kandev/kandev/internal/common/constants"
	"github.com/kandev/kandev/internal/orchestrator/executor"
	"github.com/kandev/kandev/internal/task/models"
	"github.com/kandev/kandev/internal/task/repository"
)

// prepare creates the CREATED session row from the frozen input. The session
// ID was allocated at admission, so a retry after a crash adopts the row an
// earlier attempt may already have committed.
func (d *exactLaunchDriver) prepare(ctx context.Context) error {
	op := d.snapshot()
	session, err := d.s.repo.GetTaskSession(ctx, op.SessionID)
	switch {
	case err == nil && session != nil:
		if session.TaskID != op.TaskID {
			return d.failNotDelivered(ctx, exactCodePrepareFailed, "the preallocated session belongs to another card")
		}
	case err == nil || errors.Is(err, models.ErrTaskSessionNotFound):
		if err := d.createSession(ctx, op); err != nil {
			return err
		}
	default:
		return err
	}
	return d.advance(ctx, d.update(models.ExactLaunchPhasePrepared, models.ExactLaunchPromptPending))
}

func (d *exactLaunchDriver) createSession(ctx context.Context, op models.ExactSessionLaunchOperation) error {
	s := d.s
	task, err := s.scheduler.GetTask(ctx, op.TaskID)
	if err != nil {
		return d.prepareFailed(ctx, op, err)
	}
	submission, err := models.NewInitialPromptSubmission(op.FrozenInput.Prompt, false, nil)
	if err != nil {
		return d.prepareFailed(ctx, op, err)
	}
	pctx := executor.WithTaskRunnerProfileExplicit(ctx, true)
	pctx = models.WithPreallocatedSessionID(pctx, op.TaskID, op.SessionID)
	pctx = withInitialPromptSubmission(pctx, submission)
	pctx = withInitialPromptPreview(pctx, models.NewInitialPromptPreview(op.FrozenInput.Prompt, nil))
	in := &resolvedPrepareInputs{
		task:              task,
		agentProfileID:    op.FrozenInput.AgentProfileID,
		executorID:        op.FrozenInput.ExecutorID,
		executorProfileID: op.FrozenInput.ExecutorProfileID,
		workflowStepID:    op.FrozenInput.WorkflowStepID,
	}
	created, err := s.prepareTaskSessionResolved(pctx, in)
	if err == nil && created != op.SessionID {
		err = fmt.Errorf("prepared session %s differs from the admitted session", created)
	}
	if err != nil {
		return d.prepareFailed(ctx, op, err)
	}
	return nil
}

// prepareFailed ends the operation unless the session row exists after all
// (the error came after the commit) or the service is shutting down.
func (d *exactLaunchDriver) prepareFailed(ctx context.Context, op models.ExactSessionLaunchOperation, cause error) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if session, err := d.s.repo.GetTaskSession(ctx, op.SessionID); err == nil && session != nil && session.TaskID == op.TaskID {
		return nil
	}
	return d.failNotDelivered(ctx, exactCodePrepareFailed, cause.Error())
}

// launch starts the prepared session once. The only recoverable state is a
// still-pending initial submission; anything the provider may have seen is
// reported uncertain and never resent.
func (d *exactLaunchDriver) launch(ctx context.Context) error {
	op := d.snapshot()
	session, err := d.s.repo.GetTaskSession(ctx, op.SessionID)
	if err != nil || session == nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return d.failNotDelivered(ctx, exactCodeSubmissionLost, "the prepared session is unavailable")
	}
	submission, found, err := models.LoadInitialPromptSubmission(session.Metadata)
	if err != nil || !found {
		return d.failNotDelivered(ctx, exactCodeSubmissionLost, "the initial prompt receipt is unavailable")
	}
	switch submission.State {
	case models.InitialPromptSubmissionAccepted:
		return d.settlePrompt(ctx, exactPromptAccepted, "")
	case models.InitialPromptSubmissionPending:
	default:
		return d.settlePrompt(ctx, exactPromptUncertain, exactCodeDispatchUncertain)
	}
	if session.State != models.TaskSessionStateCreated && session.State != models.TaskSessionStateWaitingForInput {
		return d.failNotDelivered(ctx, exactCodeNotStartable, "the prepared session left its startable state before the prompt was sent")
	}
	return d.startAndWait(ctx, op)
}

func (d *exactLaunchDriver) startAndWait(ctx context.Context, op models.ExactSessionLaunchOperation) error {
	sig := newExactPromptSignal()
	defer sig.closeGate(exactCodeShutdown, false)
	_, err := d.s.startCreatedSession(
		withExactLaunchCeilingRefusal(ctx), op.TaskID, op.SessionID, op.FrozenInput.AgentProfileID,
		op.FrozenInput.Prompt, true, false, false, nil, nil, "", d.launchOptions(ctx, op, sig),
	)
	if err != nil && !sig.isDispatching() {
		if ctx.Err() != nil {
			sig.closeGate(exactCodeShutdown, false)
			return ctx.Err()
		}
		outcome, code := sig.closeGate(exactLaunchErrorCode(err), true)
		return d.settlePrompt(ctx, outcome, code)
	}
	return d.awaitPrompt(ctx, sig)
}

func (d *exactLaunchDriver) awaitPrompt(ctx context.Context, sig *exactPromptSignal) error {
	timer := time.NewTimer(constants.AgentLaunchTimeout)
	defer timer.Stop()
	select {
	case <-sig.done:
	case <-ctx.Done():
		outcome, code := sig.closeGate(exactCodeShutdown, false)
		if outcome == exactPromptOpen {
			return ctx.Err()
		}
		return d.settlePrompt(ctx, outcome, code)
	case <-timer.C:
		outcome, code := sig.closeGate(exactCodeLaunchTimeout, true)
		return d.settlePrompt(ctx, outcome, code)
	}
	outcome, code := sig.result()
	return d.settlePrompt(ctx, outcome, code)
}

func exactLaunchErrorCode(err error) string {
	switch {
	case errors.Is(err, executor.ErrExecutionAlreadyRunning):
		return exactCodeSessionBusy
	case errors.Is(err, errExactLaunchCeilingRefused):
		return exactCodeCeilingRefused
	default:
		return exactCodeLaunchFailed
	}
}

// launchOptions wires the initial-prompt callbacks to the durable operation.
// beforeProviderAdmission is the last point before the prompt can reach the
// provider: it fences the operation into prompt_dispatching and marks the
// session's own receipt dispatching, so any later crash is uncertain.
func (d *exactLaunchDriver) launchOptions(ctx context.Context, op models.ExactSessionLaunchOperation, sig *exactPromptSignal) startCreatedSessionOptions {
	return startCreatedSessionOptions{
		skipTaskDescriptionFallback: true,
		promptAlreadyComposed:       true,
		profileFrozen:               true,
		retryPrompt:                 op.FrozenInput.Prompt,
		promptReferencesPrepared:    true,
		beforeProviderAdmission: func(executionID string) error {
			return d.beforeDispatch(ctx, op, sig, executionID)
		},
		onInitialPromptAdmissionFailed: func(error) { sig.rejectAdmission() },
		onInitialPromptFailed:          func() { sig.failed(exactCodeLaunchFailed) },
		onInitialPromptAccepted: func(executionID string) {
			d.onAccepted(ctx, op, sig, executionID)
		},
	}
}

func (d *exactLaunchDriver) beforeDispatch(ctx context.Context, op models.ExactSessionLaunchOperation, sig *exactPromptSignal, executionID string) error {
	if err := sig.enterDispatch(); err != nil {
		sig.rejectAdmission()
		return err
	}
	if err := d.advance(ctx, d.update(models.ExactLaunchPhasePromptDispatched, models.ExactLaunchPromptPending)); err != nil {
		sig.rejectAdmission()
		return err
	}
	if err := d.s.transitionInitialSubmissionToDispatching(ctx, op.SessionID, executionID, ""); err != nil {
		sig.rejectAdmission()
		return err
	}
	return nil
}

func (d *exactLaunchDriver) onAccepted(ctx context.Context, op models.ExactSessionLaunchOperation, sig *exactPromptSignal, executionID string) {
	wctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), exactLaunchWriteTimeout)
	defer cancel()
	if err := d.s.transitionInitialSubmissionToAccepted(wctx, op.SessionID, executionID, ""); err != nil {
		d.s.logger.Zap().Warn("could not persist exact launch prompt acceptance",
			zap.String("operation_id", op.OperationID), zap.Error(err))
	}
	sig.accepted()
}

// recoverDispatching resolves an operation whose previous owner stopped after
// the prompt may have reached the provider. Only a durable acceptance proves
// delivery; otherwise the outcome stays uncertain and nothing is resent.
func (d *exactLaunchDriver) recoverDispatching(ctx context.Context) error {
	op := d.snapshot()
	session, err := d.s.repo.GetTaskSession(ctx, op.SessionID)
	if err != nil || session == nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return d.settlePrompt(ctx, exactPromptUncertain, exactCodeDispatchUncertain)
	}
	if submission, found, loadErr := models.LoadInitialPromptSubmission(session.Metadata); loadErr == nil && found &&
		submission.State == models.InitialPromptSubmissionAccepted {
		return d.settlePrompt(ctx, exactPromptAccepted, "")
	}
	return d.settlePrompt(ctx, exactPromptUncertain, exactCodeDispatchUncertain)
}

// finish records the raw first user message, applies the requested primary
// under the baseline and activity fence, and completes the operation.
func (d *exactLaunchDriver) finish(ctx context.Context) error {
	op := d.snapshot()
	if err := d.ensureFirstMessage(ctx, op); err != nil {
		return err
	}
	up := d.update(models.ExactLaunchPhaseCompleted, models.ExactLaunchPromptAccepted)
	up.PrimaryStatus, up.PrimaryReason = models.ExactLaunchPrimaryNotRequested, models.ExactLaunchReasonNone
	if op.MakePrimary {
		result, err := d.s.exactLaunchPromote(ctx, d.store, &op)
		if err != nil {
			return err
		}
		up.PrimaryStatus, up.PrimaryReason = result.Status, result.Reason
	}
	return d.advance(ctx, up)
}

func (s *Service) exactLaunchPromote(
	ctx context.Context, store repository.ExactSessionLaunchRepository, op *models.ExactSessionLaunchOperation,
) (models.PromoteSessionPrimaryResult, error) {
	result, err := store.PromoteSessionPrimaryIfBaseline(ctx, op)
	if err != nil {
		return result, err
	}
	if result.Status == models.ExactLaunchPrimaryApplied {
		s.publishPrimarySessionUpdate(ctx, op.TaskID, op.SessionID)
	}
	return result, nil
}

func (d *exactLaunchDriver) ensureFirstMessage(ctx context.Context, op models.ExactSessionLaunchOperation) error {
	if d.s.messageCreator == nil {
		return nil
	}
	turnID, _ := d.s.peekActiveTurnID(ctx, op.SessionID)
	meta := NewUserMessageMeta().WithPlanMode(false).WithAutoStart(false)
	return d.s.messageCreator.CreateUserMessageIdempotent(
		ctx, initialSubmissionUserMessageID(op.TaskID, op.SessionID), op.TaskID, op.FrozenInput.Prompt,
		op.SessionID, turnID, meta.ToMap(),
	)
}
