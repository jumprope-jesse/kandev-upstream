package orchestrator

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	agentruntime "github.com/kandev/kandev/internal/agent/runtime"
	"github.com/kandev/kandev/internal/task/models"
)

func TestSettleDetachedTurnAfterOwnerMismatchFreesSession(t *testing.T) {
	repo := setupTestRepo(t)
	seedSession(t, repo, "task1", "session1", "step1", "session1")
	svc := createTestService(repo, newMockStepGetter(), newMockTaskRepo())
	svc.turnService = &repoTurnService{repo: repo}
	ctx := context.Background()
	if _, err := svc.turnService.StartTurn(ctx, "session1"); err != nil {
		t.Fatalf("seed turn: %v", err)
	}
	now := time.Now().UTC()
	submission := &models.AgentDeliverySubmission{
		ID: "prompt:detached", SessionID: "session1", IncarnationID: "session1", HarnessGeneration: 1,
		OwnerGeneration: 1, DispatchAttemptID: "detached", PayloadHash: "hash", Payload: []byte("prompt"),
		State: models.DeliverySubmissionDispatching, CreatedAt: now, UpdatedAt: now,
	}
	if _, err := repo.PrepareAgentDeliverySubmission(ctx, submission); err != nil {
		t.Fatal(err)
	}

	if err := svc.settleDetachedTurnAfterOwnerMismatch(ctx, "session1"); err != nil {
		t.Fatalf("settle: %v", err)
	}

	stored, err := repo.GetAgentDeliverySubmission(ctx, submission.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.State != models.DeliverySubmissionInterruptedUnknown {
		t.Fatalf("submission state = %s, want interrupted_unknown", stored.State)
	}
	if open := openTurnCount(t, repo, "session1"); open != 0 {
		t.Fatalf("open turns = %d, want 0", open)
	}
}

func TestIsDetachedDeliveryOwnerMismatch(t *testing.T) {
	if !isDetachedDeliveryOwnerMismatch(fmt.Errorf("x: %w", agentruntime.ErrDeliveryOwnerMismatch)) {
		t.Fatal("owner mismatch must be recognized")
	}
	if isDetachedDeliveryOwnerMismatch(errors.New("transient")) {
		t.Fatal("transient errors keep retrying")
	}
}
