package orchestrator

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/google/uuid"

	"github.com/kandev/kandev/internal/task/models"
	"github.com/kandev/kandev/internal/task/repository"
	"github.com/kandev/kandev/internal/task/repository/repoerrors"
)

var (
	// ErrExactLaunchUnavailable reports that the configured repository cannot
	// persist exact launch operations.
	ErrExactLaunchUnavailable = errors.New("exact session launch is not available")
	// ErrExactLaunchOfficeTask reports an Office card: its sessions belong to
	// the scheduler.
	ErrExactLaunchOfficeTask = errors.New("exact session launch refused: office tasks are started by the scheduler")
	// ErrExactLaunchPassthrough reports a CLI passthrough profile, whose
	// initial prompt has no delivery acceptance signal to bind an exact launch to.
	ErrExactLaunchPassthrough = errors.New("exact session launch refused: passthrough profiles are not supported")
	// ErrExactLaunchPendingMove reports a deferred workflow move queued for a
	// live session of the card.
	ErrExactLaunchPendingMove = errors.New("exact session launch refused: a workflow move is pending")
)

// ExactSessionLaunchResult is the caller-visible state of one operation.
type ExactSessionLaunchResult struct {
	Operation *models.ExactSessionLaunchOperation
	Replayed  bool
}

// Outcome is the derived outcome class of the result.
func (r *ExactSessionLaunchResult) Outcome() models.ExactSessionLaunchOutcome {
	return r.Operation.Outcome()
}

// LaunchFreshSessionExact starts one fresh session on an existing card with
// prompt as its first user instruction. principalScope is the stable
// authenticated identity the idempotency key is scoped to. Authorization runs
// on every call, including replays, before any dependency is used.
func (s *Service) LaunchFreshSessionExact(
	ctx context.Context, principalScope string, req models.ExactSessionLaunchRequest, claim *models.TaskManagementClaimFence,
) (*ExactSessionLaunchResult, error) {
	if err := s.authorizeTaskPrompt(ctx, req.TaskID); err != nil {
		return nil, err
	}
	if err := s.authorizeTask(ctx, req.TaskID); err != nil {
		return nil, err
	}
	if strings.TrimSpace(principalScope) == "" {
		return nil, fmt.Errorf("%w: principal scope is required", models.ErrInvalidExactSessionLaunch)
	}
	if err := req.Validate(); err != nil {
		return nil, err
	}
	store, err := s.exactLaunchStore()
	if err != nil {
		return nil, err
	}
	if s.exactLaunch.isStopped() {
		return nil, ErrServiceNotRunning
	}
	if res, err := s.replayExactLaunch(ctx, store, principalScope, req); res != nil || err != nil {
		return res, err
	}
	frozen, err := s.admitExactLaunchGates(ctx, req)
	if err != nil {
		// A concurrent duplicate may have admitted the key between the lookup
		// and the gates; that caller owns the card, so this one replays it.
		if res, replayErr := s.replayExactLaunch(ctx, store, principalScope, req); res != nil || replayErr != nil {
			return res, replayErr
		}
		return nil, err
	}
	stored, replayed, err := store.AdmitExactSessionLaunch(ctx, models.ExactSessionLaunchAdmission{
		Operation:               newExactLaunchOperation(principalScope, req, frozen),
		ExpectedResourceVersion: req.ExpectedTaskResourceVersion,
		ClaimFence:              claim,
	})
	if err != nil {
		return nil, err
	}
	return s.awaitExactLaunch(ctx, store, stored, replayed)
}

// replayExactLaunch serves an operation already admitted under the request's
// key. It returns nil, nil when the key is unused.
func (s *Service) replayExactLaunch(
	ctx context.Context, store repository.ExactSessionLaunchRepository, principalScope string, req models.ExactSessionLaunchRequest,
) (*ExactSessionLaunchResult, error) {
	existing, err := store.GetExactSessionLaunchByKey(ctx, principalScope, req.WorkspaceID, req.IdempotencyKey)
	if errors.Is(err, repoerrors.ErrExactLaunchNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if existing.RequestDigest != req.Digest() {
		return nil, repoerrors.ErrTaskOperationConflict
	}
	return s.awaitExactLaunch(ctx, store, existing, true)
}

func newExactLaunchOperation(
	principalScope string, req models.ExactSessionLaunchRequest, frozen models.ExactSessionLaunchFrozenInput,
) models.ExactSessionLaunchOperation {
	op := models.ExactSessionLaunchOperation{
		OperationID:    uuid.NewString(),
		PrincipalScope: principalScope,
		WorkspaceID:    req.WorkspaceID,
		IdempotencyKey: req.IdempotencyKey,
		RequestDigest:  req.Digest(),
		TaskID:         req.TaskID,
		SessionID:      uuid.NewString(),
		FrozenInput:    frozen,
		MakePrimary:    req.MakePrimary,
		PrimaryStatus:  models.ExactLaunchPrimaryNotRequested,
		PromptStatus:   models.ExactLaunchPromptPending,
		Phase:          models.ExactLaunchPhaseAdmitted,
	}
	if req.MakePrimary {
		op.PrimaryStatus = models.ExactLaunchPrimaryPending
	}
	if req.ExpectedPrimarySessionID != nil {
		op.BaselineAsserted = true
		op.BaselinePrimarySessionID = *req.ExpectedPrimarySessionID
	}
	return op
}

// admitExactLaunchGates applies the orchestrator-owned refusals that must hold
// before an operation is recorded, and freezes the launch input. Durable busy,
// archive, workspace, version, claim and baseline checks run inside the
// repository admission transaction.
func (s *Service) admitExactLaunchGates(
	ctx context.Context, req models.ExactSessionLaunchRequest,
) (models.ExactSessionLaunchFrozenInput, error) {
	var zero models.ExactSessionLaunchFrozenInput
	isOffice, err := s.lookupOfficeTask(ctx, req.TaskID)
	if err != nil {
		return zero, fmt.Errorf("determine office task status: %w", err)
	}
	if isOffice {
		return zero, ErrExactLaunchOfficeTask
	}
	if err := s.refuseExactLaunchOnLiveSessions(ctx, req.TaskID); err != nil {
		return zero, err
	}
	frozen, err := s.freezeExactLaunchInput(ctx, req)
	if err != nil {
		return zero, err
	}
	if s.isPassthroughProfile(ctx, frozen.AgentProfileID) {
		return zero, ErrExactLaunchPassthrough
	}
	return frozen, nil
}

// refuseExactLaunchOnLiveSessions refuses while any non-terminal session of the
// card has a pending workflow move, an open recovery block, or in-memory
// foreground, background or subagent activity. It never interrupts work.
func (s *Service) refuseExactLaunchOnLiveSessions(ctx context.Context, taskID string) error {
	sessions, err := s.repo.ListActiveTaskSessionsByTaskID(ctx, taskID)
	if err != nil {
		return fmt.Errorf("list active sessions: %w", err)
	}
	for _, session := range sessions {
		if s.messageQueue != nil {
			if _, pending, moveErr := s.messageQueue.GetPendingMoveWithError(ctx, session.ID); moveErr != nil {
				return fmt.Errorf("read pending workflow move: %w", moveErr)
			} else if pending {
				return ErrExactLaunchPendingMove
			}
		}
		if err := s.checkSessionRecoveryBlock(ctx, session.ID); err != nil {
			return err
		}
		if s.exactLaunchSessionBusyInMemory(session.ID) {
			return repoerrors.ErrExactLaunchBusy
		}
	}
	return nil
}

// exactLaunchSessionBusyInMemory reports live activity the durable admission
// cannot see: subagents, background work, and an in-flight prompt.
func (s *Service) exactLaunchSessionBusyInMemory(sessionID string) bool {
	if s.ActiveSubagentCount(sessionID) > 0 {
		return true
	}
	ta := s.lockTurnActivity(sessionID, false)
	if ta == nil {
		return false
	}
	defer ta.mu.Unlock()
	return ta.promptInFlight || ta.dispatchInFlightGeneration != 0 || len(ta.background) > 0
}

// freezeExactLaunchInput resolves every default once so a driver recovering
// after a restart launches exactly what was admitted. An explicit profile is
// kept as given; an omitted one is resolved through the same chain as an
// ordinary start.
func (s *Service) freezeExactLaunchInput(
	ctx context.Context, req models.ExactSessionLaunchRequest,
) (models.ExactSessionLaunchFrozenInput, error) {
	var zero models.ExactSessionLaunchFrozenInput
	in, err := s.resolvePrepareInputs(ctx, req.TaskID, req.AgentProfileID, "", "", "")
	if err != nil {
		return zero, err
	}
	if in.task.WorkspaceID != req.WorkspaceID {
		return zero, repoerrors.ErrTaskNotFound
	}
	profileID := in.agentProfileID
	if req.AgentProfileID == "" {
		if profileID, err = s.resolveEffectiveAgentProfile(ctx, req.TaskID, in.workflowStepID, profileID); err != nil {
			return zero, err
		}
	}
	if strings.TrimSpace(profileID) == "" {
		return zero, fmt.Errorf("%w: no agent profile resolves for this card", models.ErrInvalidExactSessionLaunch)
	}
	if s.profileExecutionResolver != nil {
		if err := s.profileExecutionResolver.ValidateProfile(ctx, profileID); err != nil {
			return zero, err
		}
	}
	executorID := in.executorID
	if mode, _ := workspacePolicyMode(in.task.Metadata); executorID == "" && in.executorProfileID == "" && mode == "" && s.executor != nil {
		executorID = s.executor.DefaultExecutorID(ctx, in.task.WorkspaceID)
	}
	return models.ExactSessionLaunchFrozenInput{
		Version:           models.ExactSessionLaunchFrozenInputVersion,
		Prompt:            req.Prompt,
		AgentProfileID:    profileID,
		ExecutorID:        executorID,
		ExecutorProfileID: in.executorProfileID,
		WorkflowStepID:    in.workflowStepID,
	}, nil
}
