package handlers

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"

	"github.com/kandev/kandev/internal/orchestrator"
	"github.com/kandev/kandev/internal/task/models"
)

type openQuestionOrchestrator struct {
	*resumeRetryOrchestrator
	open bool
}

func (o *openQuestionOrchestrator) SessionHasOpenQuestion(context.Context, string) bool {
	return o.open
}

func dispatchAndWait(h *MessageHandlers, orch *openQuestionOrchestrator, created bool) {
	h.dispatchPromptAsync(context.Background(), wsAddMessageRequest{
		TaskID: "task-1", TaskSessionID: "session-1", Content: "plain message",
	}, "profile-1", created, false, "")
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if orch.promptCalls+orch.queuePromptCalls > 0 {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func newOpenQuestionHandlers(t *testing.T, open bool) (*MessageHandlers, *openQuestionOrchestrator) {
	repo := &resumeRetryRepo{
		sessionStateSequencer: sessionStateSequencer{
			states: []models.TaskSessionState{models.TaskSessionStateWaitingForInput},
		},
	}
	orch := &openQuestionOrchestrator{resumeRetryOrchestrator: &resumeRetryOrchestrator{}, open: open}
	return newTestMessageHandlersWithOrchestrator(t, repo, orch), orch
}

// A plain message sent while a question is open is queued, never prompted:
// prompting would cancel the turn that owns the question.
func TestDispatchPromptAsync_OpenQuestionQueuesInsteadOfPrompting(t *testing.T) {
	h, orch := newOpenQuestionHandlers(t, true)
	dispatchAndWait(h, orch, false)
	assert.Equal(t, 0, orch.promptCalls, "the open question's turn must not be cancelled")
	assert.Equal(t, 1, orch.queuePromptCalls)
	assert.Equal(t, true, orch.queueMetadata[orchestrator.MetaKeyTurnStartAlreadyProcessed])
}

// Without an open question (for example after a cancelled turn) the message
// starts a new turn directly.
func TestDispatchPromptAsync_NoOpenQuestionPromptsDirectly(t *testing.T) {
	h, orch := newOpenQuestionHandlers(t, false)
	dispatchAndWait(h, orch, false)
	assert.Equal(t, 1, orch.promptCalls)
	assert.Equal(t, 0, orch.queuePromptCalls)
}
