package orchestrator

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/google/uuid"
	"go.uber.org/zap"

	"github.com/kandev/kandev/internal/task/models"
	"github.com/kandev/kandev/internal/task/repository"
	"github.com/kandev/kandev/internal/task/repository/repoerrors"
)

const (
	exactLaunchWriteTimeout = 5 * time.Second
	exactLaunchCooldown     = 2 * time.Second
	exactLaunchMaxMessage   = 500
)

// exactLaunchDriver advances one claimed operation. Every durable write is
// fenced by the claim, so a driver that lost ownership can no longer change
// the operation.
type exactLaunchDriver struct {
	s     *Service
	store repository.ExactSessionLaunchRepository
	fence models.ExactSessionLaunchFence
	mu    sync.Mutex
	op    *models.ExactSessionLaunchOperation
}

// runExactLaunch claims operationID and drives it until it is terminal, the
// claim is lost, or ctx ends. It reads nothing from the original request: the
// frozen input stored with the operation is the only source.
func (s *Service) runExactLaunch(ctx context.Context, operationID string) {
	store, err := s.exactLaunchStore()
	if err != nil {
		return
	}
	token := uuid.NewString()
	op, err := store.ClaimExactSessionLaunch(ctx, operationID, token, time.Now(), exactLaunchLease)
	if err != nil {
		if isExactLaunchOwnershipLoss(err) || errors.Is(err, repoerrors.ErrExactLaunchNotFound) {
			exactSessionLaunchReconcileTotal.Add(exactLaunchReconcileOwned, 1)
		} else if ctx.Err() == nil {
			s.logger.Zap().Warn("could not claim exact session launch", zap.Error(err))
		}
		return
	}
	if op.Phase.IsTerminal() {
		return
	}
	d := &exactLaunchDriver{
		s: s, store: store, op: op,
		fence: models.ExactSessionLaunchFence{OperationID: op.OperationID, OwnerToken: token, OwnerEpoch: op.OwnerEpoch},
	}
	d.run(ctx)
}

func (d *exactLaunchDriver) run(ctx context.Context) {
	driveCtx, cancel := context.WithCancel(ctx)
	beatDone := make(chan struct{})
	go d.heartbeat(driveCtx, cancel, beatDone)
	err := d.drive(driveCtx)
	cancel()
	<-beatDone
	d.releaseIfOpen()
	if err == nil || ctx.Err() != nil || isExactLaunchOwnershipLoss(err) {
		return
	}
	exactSessionLaunchReconcileTotal.Add(exactLaunchReconcileDriveErr, 1)
	d.s.exactLaunch.cooldown(d.fence.OperationID, exactLaunchCooldown)
	d.s.logger.Zap().Warn("exact session launch paused after an error",
		zap.String("operation_id", d.fence.OperationID), zap.Error(err))
}

func (d *exactLaunchDriver) heartbeat(ctx context.Context, cancel context.CancelFunc, done chan<- struct{}) {
	defer close(done)
	timer := time.NewTimer(exactLaunchLease / 3)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
		}
		if err := d.store.RenewExactSessionLaunch(ctx, d.fence, time.Now(), exactLaunchLease); err != nil && isExactLaunchOwnershipLoss(err) {
			cancel()
			return
		}
		timer.Reset(exactLaunchLease / 3)
	}
}

func (d *exactLaunchDriver) releaseIfOpen() {
	if d.snapshot().Phase.IsTerminal() {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), exactLaunchWriteTimeout)
	defer cancel()
	_ = d.store.ReleaseExactSessionLaunch(ctx, d.fence)
}

func (d *exactLaunchDriver) snapshot() models.ExactSessionLaunchOperation {
	d.mu.Lock()
	defer d.mu.Unlock()
	return *d.op
}

// advance writes update from the operation's current phase under the fence.
func (d *exactLaunchDriver) advance(ctx context.Context, update models.ExactSessionLaunchUpdate) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	op, err := d.store.AdvanceExactSessionLaunch(ctx, d.fence, d.op.Phase, update, time.Now())
	if err != nil {
		return err
	}
	d.op = op
	if op.Phase.IsTerminal() {
		exactSessionLaunchOperationsTotal.Add(string(op.Outcome()), 1)
		exactSessionLaunchPrimaryTotal.Add(string(op.PrimaryStatus), 1)
	}
	return nil
}

func (d *exactLaunchDriver) drive(ctx context.Context) error {
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		var err error
		switch d.snapshot().Phase {
		case models.ExactLaunchPhaseAdmitted:
			err = d.prepare(ctx)
		case models.ExactLaunchPhasePrepared:
			err = d.launch(ctx)
		case models.ExactLaunchPhasePromptDispatched:
			err = d.recoverDispatching(ctx)
		case models.ExactLaunchPhasePromptAccepted:
			err = d.finish(ctx)
		default:
			return nil
		}
		if err != nil {
			return err
		}
	}
}

// update builds a transition that keeps the operation's current primary state.
func (d *exactLaunchDriver) update(phase models.ExactSessionLaunchPhase, prompt models.ExactSessionLaunchPromptStatus) models.ExactSessionLaunchUpdate {
	op := d.snapshot()
	return models.ExactSessionLaunchUpdate{
		Phase: phase, PrimaryStatus: op.PrimaryStatus, PrimaryReason: op.PrimaryReason, PromptStatus: prompt,
	}
}

// terminate ends the operation without a prompt acceptance. A requested
// primary is reported as not applied rather than left pending.
func (d *exactLaunchDriver) terminate(ctx context.Context, phase models.ExactSessionLaunchPhase, prompt models.ExactSessionLaunchPromptStatus, code, message string) error {
	up := d.update(phase, prompt)
	if d.snapshot().MakePrimary {
		up.PrimaryStatus, up.PrimaryReason = models.ExactLaunchPrimaryConflict, models.ExactLaunchReasonUnverified
	}
	up.ErrorCode, up.ErrorMessage = code, truncateExactMessage(message)
	return d.advance(ctx, up)
}

func (d *exactLaunchDriver) failNotDelivered(ctx context.Context, code, message string) error {
	return d.terminate(ctx, models.ExactLaunchPhaseFailed, models.ExactLaunchPromptNotDelivered, code, message)
}

func (d *exactLaunchDriver) markUncertain(ctx context.Context, code, message string) error {
	return d.terminate(ctx, models.ExactLaunchPhaseUncertain, models.ExactLaunchPromptUncertain, code, message)
}

// settlePrompt records the prompt outcome using a context that survives
// shutdown, so a decided outcome is never lost to cancellation.
func (d *exactLaunchDriver) settlePrompt(ctx context.Context, outcome exactPromptOutcome, code string) error {
	wctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), exactLaunchWriteTimeout)
	defer cancel()
	switch outcome {
	case exactPromptAccepted:
		return d.advance(wctx, d.update(models.ExactLaunchPhasePromptAccepted, models.ExactLaunchPromptAccepted))
	case exactPromptUncertain:
		return d.markUncertain(wctx, code, "prompt delivery could not be confirmed and was not resent")
	default:
		return d.failNotDelivered(wctx, code, "the initial prompt was not delivered")
	}
}

func truncateExactMessage(message string) string {
	if len(message) <= exactLaunchMaxMessage {
		return message
	}
	return message[:exactLaunchMaxMessage]
}
