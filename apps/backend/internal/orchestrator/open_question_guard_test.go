package orchestrator

import (
	"context"
	"testing"
	"time"

	"github.com/kandev/kandev/internal/task/models"
)

func TestSessionHasOpenQuestionCoversClarificationAndPermission(t *testing.T) {
	ctx := context.Background()
	repo := setupTestRepo(t)
	seedSession(t, repo, "t1", "s1", "step1")
	svc := createTestService(repo, newMockStepGetter(), newMockTaskRepo())
	if svc.SessionHasOpenQuestion(ctx, "s1") {
		t.Fatal("no question is open yet")
	}
	now := time.Now().UTC()
	requireNoError(t, repo.CreateTurn(ctx, &models.Turn{ID: "turn-1", TaskSessionID: "s1", TaskID: "t1", StartedAt: now}))
	requireNoError(t, repo.CreateMessage(ctx, &models.Message{
		ID: "perm-1", TaskSessionID: "s1", TaskID: "t1", TurnID: "turn-1",
		AuthorType: models.MessageAuthorAgent, Type: "permission_request", Content: "run?", CreatedAt: now,
		Metadata: map[string]interface{}{"pending_id": "p1", "status": "pending"},
	}))
	if !svc.SessionHasOpenQuestion(ctx, "s1") {
		t.Fatal("a pending permission request must hold plain messages")
	}
	perm, err := repo.GetMessage(ctx, "perm-1")
	requireNoError(t, err)
	perm.Metadata["status"] = "approved"
	requireNoError(t, repo.UpdateMessage(ctx, perm))
	if svc.SessionHasOpenQuestion(ctx, "s1") {
		t.Fatal("a resolved permission request must release the hold")
	}
}

// A cancelled turn settles its delivery submission as terminal. Its prompt
// error must not then open an unknown-outcome recovery block, which would
// leave every later message blocked under the dead turn.
func TestSettledBeforeInterruptSkipsTerminalSubmission(t *testing.T) {
	ctx := context.Background()
	service, repo := newServiceWithRealRepo(t)
	seedSession(t, repo, "task-cancel", "session-cancel", "step-1")
	session, err := repo.GetTaskSession(ctx, "session-cancel")
	requireNoError(t, err)
	requireNoError(t, repo.CreateHarnessSessionGeneration(ctx, &models.HarnessSessionGeneration{
		SessionID: session.ID, IncarnationID: session.QueueIncarnationID, Generation: 1,
		NativeSessionID: "native-cancel", CreationReason: "test",
	}))
	for _, state := range []models.DeliverySubmissionState{
		models.DeliverySubmissionDispatching, models.DeliverySubmissionCancelled,
	} {
		id := "sub-" + string(state)
		_, err := repo.PrepareAgentDeliverySubmission(ctx, &models.AgentDeliverySubmission{
			ID: id, SessionID: session.ID, IncarnationID: session.QueueIncarnationID,
			HarnessGeneration: 1, OwnerGeneration: 1, PayloadHash: "h", Payload: []byte("p"), State: state,
		})
		requireNoError(t, err)
		runtime := &agentDeliverySubmissionRuntime{store: repo, id: id, sessionID: session.ID}
		want := state == models.DeliverySubmissionCancelled
		if got := runtime.settledBeforeInterrupt(ctx); got != want {
			t.Fatalf("state %s: settledBeforeInterrupt = %v, want %v", state, got, want)
		}
	}
	if err := service.checkSessionRecoveryBlock(ctx, session.ID); err != nil {
		t.Fatalf("a cancelled turn must leave the session promptable, got %v", err)
	}
}
