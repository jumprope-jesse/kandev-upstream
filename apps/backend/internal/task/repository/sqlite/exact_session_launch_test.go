package sqlite

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/kandev/kandev/internal/task/models"
	"github.com/kandev/kandev/internal/task/repository/repoerrors"
)

const exactLaunchTestWorkspace = "ws-exact"

type exactLaunchFixture struct {
	repo *Repository
	ctx  context.Context
}

func newExactLaunchFixture(t *testing.T) *exactLaunchFixture {
	t.Helper()
	return &exactLaunchFixture{repo: newRepoForSessionTests(t), ctx: context.Background()}
}

func (f *exactLaunchFixture) task(t *testing.T, id string) *models.Task {
	t.Helper()
	task := &models.Task{ID: id, WorkspaceID: exactLaunchTestWorkspace, Title: id}
	if err := f.repo.CreateTask(f.ctx, task); err != nil {
		t.Fatalf("CreateTask %s: %v", id, err)
	}
	return task
}

func (f *exactLaunchFixture) session(t *testing.T, taskID, id string, state models.TaskSessionState, primary bool) {
	t.Helper()
	if err := f.repo.CreateTaskSession(f.ctx, &models.TaskSession{ID: id, TaskID: taskID, State: state}); err != nil {
		t.Fatalf("CreateTaskSession %s: %v", id, err)
	}
	if primary {
		if err := f.repo.SetSessionPrimary(f.ctx, id); err != nil {
			t.Fatalf("SetSessionPrimary %s: %v", id, err)
		}
	}
}

func (f *exactLaunchFixture) exec(t *testing.T, query string, args ...any) {
	t.Helper()
	if _, err := f.repo.db.ExecContext(f.ctx, f.repo.db.Rebind(query), args...); err != nil {
		t.Fatalf("exec %q: %v", query, err)
	}
}

func (f *exactLaunchFixture) turn(t *testing.T, taskID, sessionID, turnID string, open bool) {
	t.Helper()
	now := time.Now().UTC()
	var completed any
	if !open {
		completed = now
	}
	f.exec(t, `INSERT INTO task_session_turns (id, task_session_id, task_id, started_at, completed_at, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?)`, turnID, sessionID, taskID, now, completed, now, now)
}

func exactLaunchAdmission(taskID, key, sessionID string) models.ExactSessionLaunchAdmission {
	req := models.ExactSessionLaunchRequest{
		WorkspaceID: exactLaunchTestWorkspace, TaskID: taskID, IdempotencyKey: key,
		Prompt: "do the thing", AgentProfileID: "profile-1", MakePrimary: true,
	}
	return models.ExactSessionLaunchAdmission{Operation: models.ExactSessionLaunchOperation{
		OperationID: "op-" + key, PrincipalScope: "plugin:one", WorkspaceID: exactLaunchTestWorkspace,
		IdempotencyKey: key, RequestDigest: req.Digest(), TaskID: taskID, SessionID: sessionID,
		MakePrimary: true,
		FrozenInput: models.ExactSessionLaunchFrozenInput{
			Version: models.ExactSessionLaunchFrozenInputVersion, Prompt: "do the thing", AgentProfileID: "profile-1",
			ExecutorID: "exec-1", ExecutorProfileID: "execprof-1", WorkflowStepID: "step-1",
		},
	}}
}

func TestAdmitExactSessionLaunchPersistsFrozenInputAndBaseline(t *testing.T) {
	f := newExactLaunchFixture(t)
	f.task(t, "task-a")
	f.session(t, "task-a", "old", models.TaskSessionStateWaitingForInput, true)

	op, replayed, err := f.repo.AdmitExactSessionLaunch(f.ctx, exactLaunchAdmission("task-a", "k1", "new-1"))
	if err != nil || replayed {
		t.Fatalf("admit: replayed=%v err=%v", replayed, err)
	}
	if op.Phase != models.ExactLaunchPhaseAdmitted || op.PrimaryStatus != models.ExactLaunchPrimaryPending ||
		op.PromptStatus != models.ExactLaunchPromptPending {
		t.Fatalf("initial state = %s/%s/%s", op.Phase, op.PrimaryStatus, op.PromptStatus)
	}
	if op.BaselinePrimarySessionID != "old" || op.BaselineActivityMarker != "old:WAITING_FOR_INPUT:0" {
		t.Fatalf("baseline = %q marker %q", op.BaselinePrimarySessionID, op.BaselineActivityMarker)
	}
	got, err := f.repo.GetExactSessionLaunch(f.ctx, op.OperationID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.FrozenInput != exactLaunchAdmission("task-a", "k1", "new-1").Operation.FrozenInput {
		t.Fatalf("frozen input did not round trip: %+v", got.FrozenInput)
	}
	byKey, err := f.repo.GetExactSessionLaunchByKey(f.ctx, "plugin:one", exactLaunchTestWorkspace, "k1")
	if err != nil || byKey.OperationID != op.OperationID {
		t.Fatalf("get by key: %v %v", byKey, err)
	}
}

func TestAdmitExactSessionLaunchReplayAndConflict(t *testing.T) {
	f := newExactLaunchFixture(t)
	f.task(t, "task-a")
	first, _, err := f.repo.AdmitExactSessionLaunch(f.ctx, exactLaunchAdmission("task-a", "k1", "new-1"))
	if err != nil {
		t.Fatalf("admit: %v", err)
	}
	retry := exactLaunchAdmission("task-a", "k1", "new-other")
	retry.Operation.OperationID = "op-other"
	again, replayed, err := f.repo.AdmitExactSessionLaunch(f.ctx, retry)
	if err != nil || !replayed || again.OperationID != first.OperationID || again.SessionID != "new-1" {
		t.Fatalf("replay = %+v replayed=%v err=%v", again, replayed, err)
	}
	changed := exactLaunchAdmission("task-a", "k1", "new-2")
	changed.Operation.RequestDigest = "sha256:different"
	if _, _, err := f.repo.AdmitExactSessionLaunch(f.ctx, changed); !errors.Is(err, repoerrors.ErrTaskOperationConflict) {
		t.Fatalf("changed payload err = %v, want conflict", err)
	}
}

func TestAdmitExactSessionLaunchReplaySkipsAdmissionPreconditions(t *testing.T) {
	f := newExactLaunchFixture(t)
	f.task(t, "task-a")
	adm := exactLaunchAdmission("task-a", "k1", "new-1")
	if _, _, err := f.repo.AdmitExactSessionLaunch(f.ctx, adm); err != nil {
		t.Fatalf("admit: %v", err)
	}
	f.session(t, "task-a", "new-1", models.TaskSessionStateRunning, false)
	adm.ExpectedResourceVersion = "2000-01-01T00:00:00Z"
	if _, replayed, err := f.repo.AdmitExactSessionLaunch(f.ctx, adm); err != nil || !replayed {
		t.Fatalf("replay with stale version and a running session: replayed=%v err=%v", replayed, err)
	}
}

func TestAdmitExactSessionLaunchScopesKeysByPrincipal(t *testing.T) {
	f := newExactLaunchFixture(t)
	f.task(t, "task-a")
	f.task(t, "task-b")
	one := exactLaunchAdmission("task-a", "same", "s-a")
	two := exactLaunchAdmission("task-b", "same", "s-b")
	two.Operation.OperationID, two.Operation.PrincipalScope = "op-two", "agent:other"
	a, _, errA := f.repo.AdmitExactSessionLaunch(f.ctx, one)
	b, replayed, errB := f.repo.AdmitExactSessionLaunch(f.ctx, two)
	if errA != nil || errB != nil || replayed || a.OperationID == b.OperationID {
		t.Fatalf("scopes must not collide: %v %v replayed=%v", errA, errB, replayed)
	}
}

func TestAdmitExactSessionLaunchConcurrentDuplicatesCreateOneOperation(t *testing.T) {
	f := newExactLaunchFixture(t)
	f.task(t, "task-a")
	const workers = 12
	ids := make([]string, workers)
	errs := make([]error, workers)
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			adm := exactLaunchAdmission("task-a", "dup", fmt.Sprintf("session-%d", i))
			adm.Operation.OperationID = fmt.Sprintf("op-%d", i)
			op, _, err := f.repo.AdmitExactSessionLaunch(f.ctx, adm)
			errs[i] = err
			if op != nil {
				ids[i] = op.OperationID
			}
		}(i)
	}
	wg.Wait()
	for i := range ids {
		if errs[i] != nil || ids[i] != ids[0] {
			t.Fatalf("worker %d: id=%q err=%v (first %q)", i, ids[i], errs[i], ids[0])
		}
	}
	var n int
	if err := f.repo.db.GetContext(f.ctx, &n, `SELECT COUNT(*) FROM exact_session_launch_operations`); err != nil || n != 1 {
		t.Fatalf("operation rows = %d err=%v", n, err)
	}
}

func TestAdmitExactSessionLaunchRefusals(t *testing.T) {
	cases := []struct {
		name  string
		setup func(t *testing.T, f *exactLaunchFixture)
		tweak func(adm *models.ExactSessionLaunchAdmission)
		want  error
	}{
		{name: "unknown task", tweak: func(a *models.ExactSessionLaunchAdmission) { a.Operation.TaskID = "missing" }, want: repoerrors.ErrTaskNotFound},
		{name: "wrong workspace", tweak: func(a *models.ExactSessionLaunchAdmission) { a.Operation.WorkspaceID = "ws-other" }, want: repoerrors.ErrTaskNotFound},
		{name: "stale resource version", tweak: func(a *models.ExactSessionLaunchAdmission) { a.ExpectedResourceVersion = "2000-01-01T00:00:00Z" }, want: repoerrors.ErrTaskVersionConflict},
		{name: "archived", setup: func(t *testing.T, f *exactLaunchFixture) {
			f.exec(t, `UPDATE tasks SET archived_at = ? WHERE id = ?`, time.Now().UTC(), "task-a")
		}, want: repoerrors.ErrExactLaunchTaskArchived},
		{name: "running session", setup: func(t *testing.T, f *exactLaunchFixture) {
			f.session(t, "task-a", "s", models.TaskSessionStateRunning, true)
		}, want: repoerrors.ErrExactLaunchBusy},
		{name: "starting session", setup: func(t *testing.T, f *exactLaunchFixture) {
			f.session(t, "task-a", "s", models.TaskSessionStateStarting, false)
		}, want: repoerrors.ErrExactLaunchBusy},
		{name: "created session", setup: func(t *testing.T, f *exactLaunchFixture) {
			f.session(t, "task-a", "s", models.TaskSessionStateCreated, false)
		}, want: repoerrors.ErrExactLaunchBusy},
		{name: "idle with open turn", setup: func(t *testing.T, f *exactLaunchFixture) {
			f.session(t, "task-a", "s", models.TaskSessionStateWaitingForInput, true)
			f.turn(t, "task-a", "s", "t1", true)
		}, want: repoerrors.ErrExactLaunchBusy},
		{name: "idle with open background work", setup: func(t *testing.T, f *exactLaunchFixture) {
			f.session(t, "task-a", "s", models.TaskSessionStateWaitingForInput, true)
			f.exec(t, `INSERT INTO task_session_background_work (id, task_id, session_id, state) VALUES ('w1', 'task-a', 's', 'running')`)
		}, want: repoerrors.ErrExactLaunchBusy},
		{name: "idle with open background run", setup: func(t *testing.T, f *exactLaunchFixture) {
			f.session(t, "task-a", "s", models.TaskSessionStateWaitingForInput, true)
			f.exec(t, `INSERT INTO task_session_background_runs (id, workload_id, session_id, state) VALUES ('r1', 'w1', 's', 'running')`)
		}, want: repoerrors.ErrExactLaunchBusy},
		{name: "baseline mismatch", setup: func(t *testing.T, f *exactLaunchFixture) {
			f.session(t, "task-a", "s", models.TaskSessionStateWaitingForInput, true)
		}, tweak: func(a *models.ExactSessionLaunchAdmission) {
			a.Operation.BaselineAsserted, a.Operation.BaselinePrimarySessionID = true, "someone-else"
		}, want: repoerrors.ErrExactLaunchBaselineMismatch},
		{name: "asserted no primary but one exists", setup: func(t *testing.T, f *exactLaunchFixture) {
			f.session(t, "task-a", "s", models.TaskSessionStateWaitingForInput, true)
		}, tweak: func(a *models.ExactSessionLaunchAdmission) {
			a.Operation.BaselineAsserted, a.Operation.BaselinePrimarySessionID = true, ""
		}, want: repoerrors.ErrExactLaunchBaselineMismatch},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newExactLaunchFixture(t)
			f.task(t, "task-a")
			if tc.setup != nil {
				tc.setup(t, f)
			}
			adm := exactLaunchAdmission("task-a", "k1", "new-1")
			if tc.tweak != nil {
				tc.tweak(&adm)
			}
			_, _, err := f.repo.AdmitExactSessionLaunch(f.ctx, adm)
			if !errors.Is(err, tc.want) {
				t.Fatalf("err = %v, want %v", err, tc.want)
			}
			var n int
			_ = f.repo.db.GetContext(f.ctx, &n, `SELECT COUNT(*) FROM exact_session_launch_operations`)
			if n != 0 {
				t.Fatalf("a refused admission left %d operation rows", n)
			}
		})
	}
}

func TestAdmitExactSessionLaunchAllowsIdleAndEndedSessions(t *testing.T) {
	f := newExactLaunchFixture(t)
	f.task(t, "task-a")
	f.session(t, "task-a", "ended", models.TaskSessionStateFailed, true)
	f.turn(t, "task-a", "ended", "stale-open-turn", true)
	f.exec(t, `INSERT INTO task_session_background_work (id, task_id, session_id, state) VALUES ('w1', 'task-a', 'ended', 'running')`)
	f.session(t, "task-a", "idle", models.TaskSessionStateWaitingForInput, false)
	f.turn(t, "task-a", "idle", "done-turn", false)
	adm := exactLaunchAdmission("task-a", "k1", "new-1")
	adm.Operation.BaselineAsserted, adm.Operation.BaselinePrimarySessionID = true, "ended"
	op, _, err := f.repo.AdmitExactSessionLaunch(f.ctx, adm)
	if err != nil {
		t.Fatalf("an idle card with only ended-session residue must admit: %v", err)
	}
	if op.BaselinePrimarySessionID != "ended" {
		t.Fatalf("baseline = %q", op.BaselinePrimarySessionID)
	}
}

func TestAdmitExactSessionLaunchRefusesSecondOperationForSameCard(t *testing.T) {
	f := newExactLaunchFixture(t)
	f.task(t, "task-a")
	if _, _, err := f.repo.AdmitExactSessionLaunch(f.ctx, exactLaunchAdmission("task-a", "k1", "new-1")); err != nil {
		t.Fatalf("first admit: %v", err)
	}
	second := exactLaunchAdmission("task-a", "k2", "new-2")
	if _, _, err := f.repo.AdmitExactSessionLaunch(f.ctx, second); !errors.Is(err, repoerrors.ErrExactLaunchBusy) {
		t.Fatalf("second operation err = %v, want busy", err)
	}
}

func TestAdmitExactSessionLaunchHonorsClaimFence(t *testing.T) {
	f := newExactLaunchFixture(t)
	f.task(t, "task-a")
	adm := exactLaunchAdmission("task-a", "k1", "new-1")
	adm.ClaimFence = &models.TaskManagementClaimFence{InstanceKey: "stale", Generation: 9}
	if _, _, err := f.repo.AdmitExactSessionLaunch(f.ctx, adm); !errors.Is(err, repoerrors.ErrTaskManagementClaimConflict) {
		t.Fatalf("stale claim fence err = %v", err)
	}
}
