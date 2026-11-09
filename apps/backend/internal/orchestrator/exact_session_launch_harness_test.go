package orchestrator

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/kandev/kandev/internal/orchestrator/executor"
	"github.com/kandev/kandev/internal/task/models"
	sqliterepo "github.com/kandev/kandev/internal/task/repository/sqlite"
	v1 "github.com/kandev/kandev/pkg/api/v1"
	"github.com/stretchr/testify/require"
)

const (
	exactTestWorkspace = "ws1"
	exactTestTask      = "task-exact"
	exactTestProfile   = "profile-exact"
	exactTestPrompt    = "implement the exact launch"
	exactTestPrincipal = "user:alice"
)

// exactProviderMode decides what the fake provider does when the launch
// reaches the point where the initial prompt would be dispatched.
type exactProviderMode int

const (
	exactProviderAccept exactProviderMode = iota
	exactProviderDispatchThenHang
	exactProviderFailBeforeDispatch
)

// exactLaunchHarness drives LaunchFreshSessionExact against a real SQLite
// repository, the real executor and a fake agent manager.
type exactLaunchHarness struct {
	t        *testing.T
	repo     *sqliterepo.Repository
	svc      *Service
	manager  *mockAgentManager
	messages *mockMessageCreator

	mu       sync.Mutex
	mode     exactProviderMode
	dispatch int
	// afterAdmit runs after the prompt is admitted and before the provider
	// reports acceptance.
	afterAdmit       func()
	launchedProfiles []string
	taskRepo         *mockTaskRepo
	// hangRelease frees providers parked in the dispatch-then-hang mode.
	hangRelease chan struct{}
}

func newExactLaunchHarness(t *testing.T) *exactLaunchHarness {
	t.Helper()
	repo := setupTestRepo(t)
	h := &exactLaunchHarness{t: t, repo: repo, hangRelease: make(chan struct{})}
	t.Cleanup(func() { close(h.hangRelease) })
	h.messages = &mockMessageCreator{}
	h.seedWorkspace()
	h.taskRepo = newExactTaskRepo()
	h.svc, h.manager = h.newNode(h.taskRepo)
	h.svc.startExactLaunchRecovery(t.Context())
	t.Cleanup(h.svc.stopExactLaunchRecovery)
	return h
}

func newExactTaskRepo() *mockTaskRepo {
	taskRepo := newMockTaskRepo()
	taskRepo.tasks[exactTestTask] = &v1.Task{ID: exactTestTask, WorkspaceID: exactTestWorkspace, State: v1.TaskStateInProgress}
	return taskRepo
}

// newNode builds one more orchestrator service over the shared repository. It
// plays a second backend process: its recovery is not started.
func (h *exactLaunchHarness) newNode(taskRepo *mockTaskRepo) (*Service, *mockAgentManager) {
	manager := &mockAgentManager{repoForExecutionLookup: h.repo}
	manager.startAgentProcessFunc = h.providerFor(manager)
	manager.launchAgentFunc = func(_ context.Context, req *executor.LaunchAgentRequest) (*executor.LaunchAgentResponse, error) {
		h.mu.Lock()
		h.launchedProfiles = append(h.launchedProfiles, req.AgentProfileID)
		h.mu.Unlock()
		return &executor.LaunchAgentResponse{AgentExecutionID: "mock-launch-" + req.SessionID}, nil
	}
	svc := createTestServiceWithScheduler(h.repo, newMockStepGetter(), taskRepo, manager)
	svc.messageCreator = h.messages
	return svc, manager
}

func (h *exactLaunchHarness) seedWorkspace() {
	h.t.Helper()
	ctx := context.Background()
	now := time.Now().UTC()
	require.NoError(h.t, h.repo.CreateWorkspace(ctx, &models.Workspace{ID: exactTestWorkspace, Name: "Test", CreatedAt: now, UpdatedAt: now}))
	require.NoError(h.t, h.repo.CreateWorkflow(ctx, &models.Workflow{ID: "wf1", WorkspaceID: exactTestWorkspace, Name: "WF", CreatedAt: now, UpdatedAt: now}))
	require.NoError(h.t, h.repo.CreateTask(ctx, &models.Task{
		ID: exactTestTask, WorkspaceID: exactTestWorkspace, WorkflowID: "wf1", Title: "Card",
		Description: "the card description", State: v1.TaskStateInProgress, CreatedAt: now, UpdatedAt: now,
	}))
}

func (h *exactLaunchHarness) setMode(mode exactProviderMode) {
	h.mu.Lock()
	h.mode = mode
	h.mu.Unlock()
}

func (h *exactLaunchHarness) dispatches() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.dispatch
}

// provider plays the agent manager's detached initial-prompt sender.
func (h *exactLaunchHarness) providerFor(manager *mockAgentManager) func(context.Context, string) error {
	return func(ctx context.Context, _ string) error { return h.provide(ctx, manager) }
}

func (h *exactLaunchHarness) provide(ctx context.Context, manager *mockAgentManager) error {
	manager.mu.Lock()
	admit := manager.initialPromptAdmissionCallback
	accepted := manager.initialPromptDispatchCallback
	failed := manager.initialPromptFailureCallback
	manager.mu.Unlock()
	h.mu.Lock()
	mode := h.mode
	h.mu.Unlock()
	if mode == exactProviderFailBeforeDispatch {
		failed()
		return nil
	}
	if err := admit(ctx, "prepared prompt", nil); err != nil {
		failed()
		return nil
	}
	h.mu.Lock()
	h.dispatch++
	hook := h.afterAdmit
	h.mu.Unlock()
	if hook != nil {
		hook()
	}
	switch mode {
	case exactProviderDispatchThenHang:
		select {
		case <-ctx.Done():
		case <-h.hangRelease:
		}
		return ctx.Err()
	default:
		accepted()
		return nil
	}
}

func (h *exactLaunchHarness) request(key string) models.ExactSessionLaunchRequest {
	return models.ExactSessionLaunchRequest{
		WorkspaceID: exactTestWorkspace, TaskID: exactTestTask, IdempotencyKey: key,
		Prompt: exactTestPrompt, AgentProfileID: exactTestProfile,
	}
}

func (h *exactLaunchHarness) launch(req models.ExactSessionLaunchRequest) (*ExactSessionLaunchResult, error) {
	ctx, cancel := context.WithTimeout(h.t.Context(), 20*time.Second)
	defer cancel()
	return h.svc.LaunchFreshSessionExact(ctx, exactTestPrincipal, req, nil)
}

func (h *exactLaunchHarness) mustLaunch(req models.ExactSessionLaunchRequest) *ExactSessionLaunchResult {
	h.t.Helper()
	res, err := h.launch(req)
	require.NoError(h.t, err)
	return res
}

// seedSessionState inserts a prior session on the card.
func (h *exactLaunchHarness) seedSessionState(id string, state models.TaskSessionState, primary bool) {
	h.t.Helper()
	now := time.Now().UTC()
	require.NoError(h.t, h.repo.CreateTaskSession(context.Background(), &models.TaskSession{
		ID: id, TaskID: exactTestTask, State: state, StartedAt: now, UpdatedAt: now, IsPrimary: primary,
	}))
	if primary {
		require.NoError(h.t, h.repo.SetSessionPrimary(context.Background(), id))
	}
}

func (h *exactLaunchHarness) userMessages() []mockUserMessage {
	h.messages.mu.Lock()
	defer h.messages.mu.Unlock()
	return append([]mockUserMessage(nil), h.messages.userMessages...)
}

func (h *exactLaunchHarness) operation(id string) *models.ExactSessionLaunchOperation {
	h.t.Helper()
	op, err := h.repo.GetExactSessionLaunch(context.Background(), id)
	require.NoError(h.t, err)
	return op
}

func (h *exactLaunchHarness) profilesLaunched() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]string(nil), h.launchedProfiles...)
}

// admitDirect records an operation exactly as admission would, without
// starting any driver, so a test can place the operation in any durable state.
func (h *exactLaunchHarness) admitDirect(svc *Service, req models.ExactSessionLaunchRequest) *models.ExactSessionLaunchOperation {
	h.t.Helper()
	frozen, err := svc.freezeExactLaunchInput(context.Background(), req)
	require.NoError(h.t, err)
	op, _, err := h.repo.AdmitExactSessionLaunch(context.Background(), models.ExactSessionLaunchAdmission{
		Operation: newExactLaunchOperation(exactTestPrincipal, req, frozen),
	})
	require.NoError(h.t, err)
	return op
}

func (h *exactLaunchHarness) awaitPhase(operationID string, want func(*models.ExactSessionLaunchOperation) bool) *models.ExactSessionLaunchOperation {
	h.t.Helper()
	var got *models.ExactSessionLaunchOperation
	require.Eventually(h.t, func() bool {
		op, err := h.repo.GetExactSessionLaunch(context.Background(), operationID)
		if err != nil {
			return false
		}
		got = op
		return want(op)
	}, 10*time.Second, 10*time.Millisecond)
	return got
}

func exactTerminal(op *models.ExactSessionLaunchOperation) bool { return op.Phase.IsTerminal() }
