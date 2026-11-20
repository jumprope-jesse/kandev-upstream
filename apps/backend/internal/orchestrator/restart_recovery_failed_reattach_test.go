package orchestrator

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/kandev/kandev/internal/orchestrator/executor"
	"github.com/kandev/kandev/internal/task/models"
)

func seedFailedResumableSession(t *testing.T, svc *Service, repo interface {
	UpdateTaskSessionState(context.Context, string, models.TaskSessionState, string) error
	UpsertExecutorRunning(context.Context, *models.ExecutorRunning) error
}) {
	t.Helper()
	ctx := context.Background()
	if err := repo.UpdateTaskSessionState(ctx, "session1", models.TaskSessionStateFailed, "boom"); err != nil {
		t.Fatalf("mark session failed: %v", err)
	}
	if err := repo.UpsertExecutorRunning(ctx, &models.ExecutorRunning{
		ID: "session1", SessionID: "session1", TaskID: "task1", AgentExecutionID: "exec-before-restart",
		Status: models.ExecutorRunningStatusFailed, Resumable: true, ResumeToken: "resume-token",
	}); err != nil {
		t.Fatalf("seed executors_running: %v", err)
	}
}

func TestReconcileFailedSessionOnStartup_PendingRemoteOwnerIsPreserved(t *testing.T) {
	svc, repo := newTurnLifecycleTestService(t)
	ctx := context.Background()
	turn, err := svc.turnService.StartTurn(ctx, "session1")
	if err != nil {
		t.Fatalf("seed turn: %v", err)
	}
	seedFailedResumableSession(t, svc, repo)
	svc.SetPendingRemoteRecoveryChecker(func(sessionID, executionID string) bool {
		return sessionID == "session1" && executionID == "exec-before-restart"
	})

	svc.reconcileSessionsOnStartup(ctx)

	if open := openTurnCount(t, repo, "session1"); open != 1 {
		t.Fatalf("open turns = %d, want the live turn left alone (%s)", open, turn.ID)
	}
	if _, err := repo.GetExecutorRunningBySessionID(ctx, "session1"); err != nil {
		t.Fatalf("executor row must be preserved: %v", err)
	}
}

func TestReconcileFailedSessionOnStartup_RetrackedAgentIsPreserved(t *testing.T) {
	svc, repo := newTurnLifecycleTestService(t)
	ctx := context.Background()
	if _, err := svc.turnService.StartTurn(ctx, "session1"); err != nil {
		t.Fatalf("seed turn: %v", err)
	}
	seedFailedResumableSession(t, svc, repo)
	svc.SetRetrackedSessionChecker(func(sessionID string) bool { return sessionID == "session1" })

	svc.reconcileSessionsOnStartup(ctx)

	if open := openTurnCount(t, repo, "session1"); open != 1 {
		t.Fatalf("open turns = %d, want the live turn left alone", open)
	}
}

func TestRetryableGuardRefusalLeavesSessionState(t *testing.T) {
	refusal := fmt.Errorf("resume: %w", &SessionRecoveryGuardError{Retryable: true, SessionID: "session1"})
	if !executor.IsRetryableRecoveryGuardRefusal(refusal) {
		t.Fatal("retryable guard error must be recognized by the executor")
	}
	if executor.IsRetryableRecoveryGuardRefusal(&SessionRecoveryGuardError{Retryable: false}) {
		t.Fatal("unstoppable guard error is not a retryable refusal")
	}
	if executor.IsRetryableRecoveryGuardRefusal(errors.New("other")) {
		t.Fatal("unrelated errors are not guard refusals")
	}

	svc, repo := newTurnLifecycleTestService(t)
	ctx := context.Background()
	if err := repo.UpdateTaskSessionState(ctx, "session1", models.TaskSessionStateWaitingForInput, ""); err != nil {
		t.Fatalf("seed state: %v", err)
	}
	if err := svc.handleSessionLaunchFailure(ctx, "task1", "session1", refusal); !errors.Is(err, refusal) {
		t.Fatalf("handleSessionLaunchFailure must return the refusal unchanged, got %v", err)
	}
	session, err := repo.GetTaskSession(ctx, "session1")
	if err != nil {
		t.Fatalf("GetTaskSession: %v", err)
	}
	if session.State != models.TaskSessionStateWaitingForInput {
		t.Fatalf("refused recover changed session state to %s", session.State)
	}
}
