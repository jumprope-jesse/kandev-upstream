package orchestrator

import (
	"context"

	"go.uber.org/zap"

	"github.com/kandev/kandev/internal/task/models"
)

const clarificationAgentDisconnectedMetadataKey = "agent_disconnected"

// sessionHasPendingClarification reports whether the session still has durable
// clarification_request rows awaiting user input. Used to fail closed on
// workflow on_turn_complete while the user can still answer.
func (s *Service) sessionHasPendingClarification(ctx context.Context, sessionID string) bool {
	if sessionID == "" {
		return false
	}
	msgs, err := s.repo.FindActiveClarificationMessagesBySessionID(ctx, sessionID)
	if err != nil {
		s.logger.Warn("failed to check pending clarifications; blocking turn-complete transition",
			zap.String("session_id", sessionID),
			zap.Error(err))
		return true
	}
	return len(msgs) > 0
}

// sessionHasLiveClarification reports whether a current-turn clarification
// still owns the agent turn. A detached bundle remains pending and answerable,
// but it no longer owns a live turn, so a parked session may dispatch a newer
// queued prompt. Repository failures fail closed because the caller cannot
// safely distinguish a detached bundle from a live clarification.
func (s *Service) sessionHasLiveClarification(ctx context.Context, sessionID string) bool {
	if sessionID == "" {
		return false
	}
	msgs, err := s.repo.FindActiveClarificationMessagesBySessionID(ctx, sessionID)
	if err != nil {
		s.logger.Warn("failed to check live clarifications; blocking queue drain",
			zap.String("session_id", sessionID),
			zap.Error(err))
		return true
	}
	for _, msg := range msgs {
		if msg == nil || msg.Metadata == nil {
			return true
		}
		detached, ok := msg.Metadata[clarificationAgentDisconnectedMetadataKey].(bool)
		if !ok || !detached {
			return true
		}
	}
	return false
}

// SessionHasLiveClarification reports whether a current-turn clarification
// still owns the session's agent turn. Callers outside the queue use it to hold
// a prompt behind the question instead of superseding it.
func (s *Service) SessionHasLiveClarification(ctx context.Context, sessionID string) bool {
	return s.sessionHasLiveClarification(ctx, sessionID)
}

// turnCompleteBlockedByUserInput reports and applies the workflow barrier for
// durable user-input waits. A pending clarification is a platform pause, not a
// step-completion signal, so all turn-complete transition entrypoints fail
// closed while it exists.
func (s *Service) turnCompleteBlockedByUserInput(ctx context.Context, taskID, sessionID string, session *models.TaskSession) bool {
	if !s.sessionHasPendingClarification(ctx, sessionID) {
		return false
	}
	s.logger.Info("deferring on_turn_complete while clarification is pending",
		zap.String("task_id", taskID),
		zap.String("session_id", sessionID))
	if session != nil {
		if _, has := models.LoadPendingStepSignal(session.Metadata); has {
			s.clearPendingStepSignal(ctx, session)
		}
	}
	s.setSessionWaitingForInput(ctx, taskID, sessionID, session)
	return true
}

type pendingActionReader interface {
	GetPendingActionsBySessionIDs(ctx context.Context, sessionIDs []string) (map[string]models.TaskPendingAction, error)
}

// SessionHasOpenQuestion reports whether the session's current turn is parked
// on a user question: a live clarification_request or a pending
// permission_request. A plain message must be queued behind it rather than
// cancel the turn that owns the question. Lookup failures fail closed.
func (s *Service) SessionHasOpenQuestion(ctx context.Context, sessionID string) bool {
	if sessionID == "" {
		return false
	}
	if s.sessionHasLiveClarification(ctx, sessionID) {
		return true
	}
	reader, ok := s.repo.(pendingActionReader)
	if !ok {
		return false
	}
	actions, err := reader.GetPendingActionsBySessionIDs(ctx, []string{sessionID})
	if err != nil {
		s.logger.Warn("failed to check pending permission requests; holding prompt",
			zap.String("session_id", sessionID), zap.Error(err))
		return true
	}
	return actions[sessionID] == models.TaskPendingActionPermission
}
