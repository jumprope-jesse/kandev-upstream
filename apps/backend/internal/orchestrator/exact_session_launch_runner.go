package orchestrator

import (
	"context"
	"errors"
	"sync"
	"time"

	"go.uber.org/zap"

	"github.com/kandev/kandev/internal/task/models"
	"github.com/kandev/kandev/internal/task/repository"
	"github.com/kandev/kandev/internal/task/repository/repoerrors"
)

const (
	exactLaunchLease         = 30 * time.Second
	exactLaunchSweepInterval = 15 * time.Second
	exactLaunchSweepBatch    = 100
	exactLaunchPollInterval  = 50 * time.Millisecond
)

// exactLaunchRunner owns every goroutine that drives an exact session launch:
// the per-operation workers and the periodic reconcile sweep. Workers run
// under the runner context, never the caller's request context.
type exactLaunchRunner struct {
	mu      sync.Mutex
	ctx     context.Context
	cancel  context.CancelFunc
	stopped bool
	workers sync.WaitGroup
	active  map[string]struct{}
	cooling map[string]time.Time
	sweeper *ceilingSweeper
}

// reset reopens the runner for a new service generation.
func (r *exactLaunchRunner) reset() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.stopped = false
	r.ctx, r.cancel = context.WithCancel(context.Background())
	r.active = make(map[string]struct{})
	r.cooling = make(map[string]time.Time)
}

// cooldown keeps an operation that just failed transiently from being
// respawned in a tight loop by waiting callers and the sweep.
func (r *exactLaunchRunner) cooldown(operationID string, d time.Duration) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.cooling == nil {
		r.cooling = make(map[string]time.Time)
	}
	r.cooling[operationID] = time.Now().Add(d)
}

// begin registers a worker for operationID and returns the context it must
// run under. It reports false when the runner is stopped or this process is
// already driving the operation.
func (r *exactLaunchRunner) begin(operationID string) (context.Context, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.stopped {
		return nil, false
	}
	if r.ctx == nil {
		r.ctx, r.cancel = context.WithCancel(context.Background())
	}
	if r.active == nil {
		r.active = make(map[string]struct{})
	}
	if _, running := r.active[operationID]; running {
		return nil, false
	}
	if until, cooling := r.cooling[operationID]; cooling {
		if time.Now().Before(until) {
			return nil, false
		}
		delete(r.cooling, operationID)
	}
	r.active[operationID] = struct{}{}
	r.workers.Add(1)
	return r.ctx, true
}

func (r *exactLaunchRunner) end(operationID string) {
	r.mu.Lock()
	delete(r.active, operationID)
	r.mu.Unlock()
	r.workers.Done()
}

func (r *exactLaunchRunner) isActive(operationID string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	_, running := r.active[operationID]
	return running
}

func (r *exactLaunchRunner) isStopped() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.stopped
}

// stop cancels every worker and waits for them within a bound.
func (r *exactLaunchRunner) stop() bool {
	r.mu.Lock()
	r.stopped = true
	cancel := r.cancel
	r.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	done := make(chan struct{})
	go func() {
		r.workers.Wait()
		close(done)
	}()
	timer := time.NewTimer(sendNowClaimRecoveryTimeout)
	defer timer.Stop()
	select {
	case <-done:
		return true
	case <-timer.C:
		return false
	}
}

func (s *Service) exactLaunchStore() (repository.ExactSessionLaunchRepository, error) {
	store, ok := s.repo.(repository.ExactSessionLaunchRepository)
	if !ok {
		return nil, ErrExactLaunchUnavailable
	}
	return store, nil
}

// spawnExactLaunchWorker starts a worker for operationID unless one already
// runs in this process or the service is stopped.
func (s *Service) spawnExactLaunchWorker(operationID string) bool {
	ctx, ok := s.exactLaunch.begin(operationID)
	if !ok {
		return false
	}
	go func() {
		defer s.exactLaunch.end(operationID)
		s.runExactLaunch(ctx, operationID)
	}()
	return true
}

// awaitExactLaunch returns once the operation is terminal or ctx ends. A
// caller whose context ends first receives the in-progress operation; the
// worker keeps running under the service context.
func (s *Service) awaitExactLaunch(
	ctx context.Context, store repository.ExactSessionLaunchRepository, op *models.ExactSessionLaunchOperation, replayed bool,
) (*ExactSessionLaunchResult, error) {
	s.spawnExactLaunchWorker(op.OperationID)
	timer := time.NewTimer(exactLaunchPollInterval)
	defer timer.Stop()
	for !op.Phase.IsTerminal() {
		select {
		case <-ctx.Done():
			return &ExactSessionLaunchResult{Operation: op, Replayed: replayed}, nil
		case <-timer.C:
		}
		fresh, err := store.GetExactSessionLaunch(ctx, op.OperationID)
		if err != nil {
			if ctx.Err() != nil {
				return &ExactSessionLaunchResult{Operation: op, Replayed: replayed}, nil
			}
			return nil, err
		}
		op = fresh
		if !op.Phase.IsTerminal() && !s.exactLaunch.isActive(op.OperationID) && exactLaunchClaimable(op, time.Now()) {
			s.spawnExactLaunchWorker(op.OperationID)
		}
		timer.Reset(exactLaunchPollInterval)
	}
	return &ExactSessionLaunchResult{Operation: op, Replayed: replayed}, nil
}

func exactLaunchClaimable(op *models.ExactSessionLaunchOperation, now time.Time) bool {
	return op.OwnerToken == "" || op.LeaseExpiresAt == nil || !op.LeaseExpiresAt.After(now)
}

// startExactLaunchRecovery resets the runner and starts the reconcile sweep.
// The first pass runs immediately so operations interrupted by a restart
// resume without waiting for the interval.
func (s *Service) startExactLaunchRecovery(ctx context.Context) {
	s.exactLaunch.reset()
	if _, err := s.exactLaunchStore(); err != nil {
		return
	}
	s.exactLaunch.mu.Lock()
	sweeper := newCeilingSweeper()
	sweeper.interval = exactLaunchSweepInterval
	s.exactLaunch.sweeper = sweeper
	s.exactLaunch.mu.Unlock()
	if sweeper.start(ctx, s.exactLaunchSweepTick) {
		sweeper.signalNow()
	}
}

func (s *Service) stopExactLaunchRecovery() {
	s.exactLaunch.mu.Lock()
	sweeper := s.exactLaunch.sweeper
	s.exactLaunch.sweeper = nil
	s.exactLaunch.mu.Unlock()
	sweeper.stop()
	if !s.exactLaunch.stop() {
		s.logger.Zap().Warn("timed out waiting for exact session launch workers during shutdown")
	}
}

func (s *Service) exactLaunchSweepTick(ctx context.Context) {
	store, err := s.exactLaunchStore()
	if err != nil {
		return
	}
	ops, err := store.ListOpenExactSessionLaunches(ctx, exactLaunchSweepBatch)
	if err != nil {
		if ctx.Err() == nil {
			s.logger.Zap().Warn("could not list open exact session launches", zap.Error(err))
			exactSessionLaunchReconcileTotal.Add(exactLaunchReconcileListFail, 1)
		}
		return
	}
	now := time.Now()
	for _, op := range ops {
		if s.exactLaunch.isActive(op.OperationID) || !exactLaunchClaimable(op, now) {
			continue
		}
		if s.spawnExactLaunchWorker(op.OperationID) {
			exactSessionLaunchReconcileTotal.Add(exactLaunchReconcileResumed, 1)
		}
	}
}

func isExactLaunchOwnershipLoss(err error) bool {
	return errors.Is(err, repoerrors.ErrExactLaunchFenced) || errors.Is(err, repoerrors.ErrExactLaunchOwned)
}
