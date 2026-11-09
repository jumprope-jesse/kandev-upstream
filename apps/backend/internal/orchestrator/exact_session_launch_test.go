package orchestrator

import (
	"testing"

	"github.com/kandev/kandev/internal/task/models"
	"github.com/stretchr/testify/require"
)

func TestExactLaunchNeverStartedCardRecordsPromptAsFirstMessage(t *testing.T) {
	h := newExactLaunchHarness(t)
	res := h.mustLaunch(h.request("key-1"))
	require.False(t, res.Replayed)
	require.Equal(t, models.ExactLaunchOutcomeCompleted, res.Outcome())
	require.Equal(t, models.ExactLaunchPromptAccepted, res.Operation.PromptStatus)
	require.Equal(t, 1, h.dispatches())

	session, err := h.repo.GetTaskSession(t.Context(), res.Operation.SessionID)
	require.NoError(t, err)
	require.Equal(t, exactTestTask, session.TaskID)

	msgs := h.userMessages()
	require.Len(t, msgs, 1)
	require.Equal(t, exactTestPrompt, msgs[0].content, "the stored first message is the raw prompt")
	require.Equal(t, res.Operation.SessionID, msgs[0].sessionID)
}
