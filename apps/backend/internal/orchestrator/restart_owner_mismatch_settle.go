package orchestrator

import (
	"context"
	"errors"
	"time"

	agentruntime "github.com/kandev/kandev/internal/agent/runtime"
	"github.com/kandev/kandev/internal/task/models"
	taskrepo "github.com/kandev/kandev/internal/task/repository"
	"go.uber.org/zap"
)

// isDetachedDeliveryOwnerMismatch reports an error proving the delivery
// evidence for a restored session belongs to a different owner than the one
// the restart recovery pinned. Retrying cannot succeed.
func isDetachedDeliveryOwnerMismatch(err error) bool {
	return errors.Is(err, agentruntime.ErrDeliveryOwnerMismatch) ||
		errors.Is(err, agentruntime.ErrDeliveryRecordEvidenceConflict)
}

// settleDetachedTurnAfterOwnerMismatch settles a detached turn whose delivery
// can no longer be reconciled: still-dispatching submissions become
// interrupted_unknown and open turns are abandoned, which frees the session
// for new input instead of retrying the same mismatching delivery.
func (s *Service) settleDetachedTurnAfterOwnerMismatch(ctx context.Context, sessionID string) error {
	lister, ok := s.repo.(nativeResumeDeliverySubmissionReader)
	store, storeOK := s.repo.(taskrepo.AgentDeliveryRepository)
	if !ok || !storeOK {
		return errors.New("delivery submission store is unavailable")
	}
	rows, err := lister.ListAgentDeliverySubmissions(ctx, sessionID)
	if err != nil {
		return err
	}
	for _, row := range rows {
		if row.State != models.DeliverySubmissionDispatching {
			continue
		}
		if _, err := store.TransitionAgentDeliverySubmission(ctx, row.ID, row.State,
			models.DeliverySubmissionInterruptedUnknown, unknownPromptOutcome, time.Now().UTC()); err != nil {
			return err
		}
	}
	s.abandonOpenTurnsOnStartup(ctx, sessionID, "delivery owner mismatch on detached turn")
	s.logger.Info("settled detached turn as interrupted after delivery owner mismatch",
		zap.String("session_id", sessionID))
	return nil
}
