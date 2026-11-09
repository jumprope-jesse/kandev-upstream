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
	"github.com/kandev/kandev/internal/testutil"
)

// The PostgreSQL exact-launch tests skip unless KANDEV_TEST_POSTGRES_DSN names
// a disposable PostgreSQL. They reuse the SQLite fixtures so both dialects
// assert the same behavior against the real dialect SQL.

const exactLaunchPostgresBlockWindow = 300 * time.Millisecond

func newExactLaunchPostgresFixture(t *testing.T, conns int) *exactLaunchFixture {
	t.Helper()
	db := openIsolatedPostgresMultiConn(t, testutil.PostgresDSNFromEnv(t), conns)
	repo, err := NewWithDB(db, db, nil)
	if err != nil {
		t.Fatalf("init postgres schema: %v", err)
	}
	f := &exactLaunchFixture{repo: repo, ctx: context.Background()}
	if err := repo.CreateWorkspace(f.ctx, &models.Workspace{ID: exactLaunchTestWorkspace, Name: "Exact launch"}); err != nil {
		t.Fatalf("seed workspace: %v", err)
	}
	return f
}

func (f *exactLaunchFixture) operationCount(t *testing.T) int {
	t.Helper()
	var n int
	if err := f.repo.db.GetContext(f.ctx, &n, `SELECT COUNT(*) FROM exact_session_launch_operations`); err != nil {
		t.Fatalf("count operations: %v", err)
	}
	return n
}

// requireBlockedUntilReleased runs op while a competing transaction holds a
// lock, proves op cannot finish inside the block window, then releases the
// lock and returns op's result. A false failure is impossible: an operation
// that really waits on the lock cannot complete early.
func requireBlockedUntilReleased(t *testing.T, release func(), op func() error) error {
	t.Helper()
	var once sync.Once
	releaseOnce := func() { once.Do(release) }
	t.Cleanup(releaseOnce)
	done := make(chan error, 1)
	go func() { done <- op() }()
	select {
	case err := <-done:
		t.Fatalf("operation finished while the competing lock was held (err=%v)", err)
	case <-time.After(exactLaunchPostgresBlockWindow):
	}
	releaseOnce()
	select {
	case err := <-done:
		return err
	case <-time.After(15 * time.Second):
		t.Fatal("operation did not finish after the competing lock was released")
		return nil
	}
}

func TestPostgresAdmitExactSessionLaunchPersistsFrozenInputAndBaseline(t *testing.T) {
	f := newExactLaunchPostgresFixture(t, 4)
	f.task(t, "task-a")
	f.session(t, "task-a", "old", models.TaskSessionStateWaitingForInput, true)

	op, replayed, err := f.repo.AdmitExactSessionLaunch(f.ctx, exactLaunchAdmission("task-a", "k1", "new-1"))
	if err != nil || replayed {
		t.Fatalf("admit: replayed=%v err=%v", replayed, err)
	}
	if op.Phase != models.ExactLaunchPhaseAdmitted || op.PrimaryStatus != models.ExactLaunchPrimaryPending ||
		op.PromptStatus != models.ExactLaunchPromptPending || op.OwnerEpoch != 0 || op.OwnerToken != "" {
		t.Fatalf("initial state = %+v", op)
	}
	if op.BaselinePrimarySessionID != "old" || op.BaselineActivityMarker != "old:WAITING_FOR_INPUT:0" {
		t.Fatalf("baseline = %q marker %q", op.BaselinePrimarySessionID, op.BaselineActivityMarker)
	}
	want := exactLaunchAdmission("task-a", "k1", "new-1").Operation
	got, err := f.repo.GetExactSessionLaunch(f.ctx, op.OperationID)
	if err != nil || got.FrozenInput != want.FrozenInput || !got.MakePrimary || got.RequestDigest != want.RequestDigest {
		t.Fatalf("round trip = %+v err=%v", got, err)
	}
	byKey, err := f.repo.GetExactSessionLaunchByKey(f.ctx, "plugin:one", exactLaunchTestWorkspace, "k1")
	if err != nil || byKey.OperationID != op.OperationID {
		t.Fatalf("get by key: %v %v", byKey, err)
	}
	if _, err := f.repo.GetExactSessionLaunch(f.ctx, "missing"); !errors.Is(err, repoerrors.ErrExactLaunchNotFound) {
		t.Fatalf("missing op err = %v", err)
	}
}

func TestPostgresAdmitExactSessionLaunchReplayAndConflict(t *testing.T) {
	f := newExactLaunchPostgresFixture(t, 4)
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
	if n := f.operationCount(t); n != 1 {
		t.Fatalf("operation rows = %d, want 1", n)
	}
}

func TestPostgresAdmitExactSessionLaunchReplaySkipsAdmissionPreconditions(t *testing.T) {
	f := newExactLaunchPostgresFixture(t, 4)
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

func TestPostgresAdmitExactSessionLaunchScopesKeysByPrincipal(t *testing.T) {
	f := newExactLaunchPostgresFixture(t, 4)
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

func TestPostgresAdmitExactSessionLaunchRefusals(t *testing.T) {
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
			f := newExactLaunchPostgresFixture(t, 4)
			f.task(t, "task-a")
			if tc.setup != nil {
				tc.setup(t, f)
			}
			adm := exactLaunchAdmission("task-a", "k1", "new-1")
			if tc.tweak != nil {
				tc.tweak(&adm)
			}
			if _, _, err := f.repo.AdmitExactSessionLaunch(f.ctx, adm); !errors.Is(err, tc.want) {
				t.Fatalf("err = %v, want %v", err, tc.want)
			}
			if n := f.operationCount(t); n != 0 {
				t.Fatalf("a refused admission left %d operation rows", n)
			}
		})
	}
}

func TestPostgresAdmitExactSessionLaunchAllowsIdleAndEndedSessions(t *testing.T) {
	f := newExactLaunchPostgresFixture(t, 4)
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

func TestPostgresAdmitExactSessionLaunchRefusesSecondOperationForSameCard(t *testing.T) {
	f := newExactLaunchPostgresFixture(t, 4)
	f.task(t, "task-a")
	if _, _, err := f.repo.AdmitExactSessionLaunch(f.ctx, exactLaunchAdmission("task-a", "k1", "new-1")); err != nil {
		t.Fatalf("first admit: %v", err)
	}
	if _, _, err := f.repo.AdmitExactSessionLaunch(f.ctx, exactLaunchAdmission("task-a", "k2", "new-2")); !errors.Is(err, repoerrors.ErrExactLaunchBusy) {
		t.Fatalf("second operation err = %v, want busy", err)
	}
}

func TestPostgresAdmitExactSessionLaunchHonorsClaimFence(t *testing.T) {
	f := newExactLaunchPostgresFixture(t, 4)
	f.task(t, "task-a")
	adm := exactLaunchAdmission("task-a", "k1", "new-1")
	adm.ClaimFence = &models.TaskManagementClaimFence{InstanceKey: "stale", Generation: 9}
	if _, _, err := f.repo.AdmitExactSessionLaunch(f.ctx, adm); !errors.Is(err, repoerrors.ErrTaskManagementClaimConflict) {
		t.Fatalf("stale claim fence err = %v", err)
	}
	if n := f.operationCount(t); n != 0 {
		t.Fatalf("a fenced admission left %d operation rows", n)
	}
}

func TestPostgresAdmitExactSessionLaunchConcurrentDuplicatesCreateOneOperation(t *testing.T) {
	const workers = 12
	f := newExactLaunchPostgresFixture(t, workers)
	f.task(t, "task-a")
	ids := make([]string, workers)
	sessions := make([]string, workers)
	replays := make([]bool, workers)
	errs := make([]error, workers)
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			adm := exactLaunchAdmission("task-a", "dup", fmt.Sprintf("session-%d", i))
			adm.Operation.OperationID = fmt.Sprintf("op-%d", i)
			<-start
			op, replayed, err := f.repo.AdmitExactSessionLaunch(f.ctx, adm)
			errs[i], replays[i] = err, replayed
			if op != nil {
				ids[i], sessions[i] = op.OperationID, op.SessionID
			}
		}(i)
	}
	close(start)
	wg.Wait()
	created := 0
	for i := range ids {
		if errs[i] != nil || ids[i] != ids[0] || sessions[i] != sessions[0] {
			t.Fatalf("worker %d: id=%q session=%q err=%v (first %q/%q)", i, ids[i], sessions[i], errs[i], ids[0], sessions[0])
		}
		if !replays[i] {
			created++
		}
	}
	if created != 1 {
		t.Fatalf("%d workers reported creating the operation, want exactly 1", created)
	}
	if n := f.operationCount(t); n != 1 {
		t.Fatalf("operation rows = %d", n)
	}
}

func TestPostgresAdmitExactSessionLaunchSameKeyOnDifferentCardsYieldsOneOperation(t *testing.T) {
	const workers = 8
	f := newExactLaunchPostgresFixture(t, workers)
	for i := 0; i < workers; i++ {
		f.task(t, fmt.Sprintf("task-%d", i))
	}
	errs := make([]error, workers)
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			adm := exactLaunchAdmission(fmt.Sprintf("task-%d", i), "shared", fmt.Sprintf("session-%d", i))
			adm.Operation.OperationID = fmt.Sprintf("op-%d", i)
			<-start
			_, _, errs[i] = f.repo.AdmitExactSessionLaunch(f.ctx, adm)
		}(i)
	}
	close(start)
	wg.Wait()
	won, conflicted := 0, 0
	for i, err := range errs {
		switch {
		case err == nil:
			won++
		case errors.Is(err, repoerrors.ErrTaskOperationConflict):
			conflicted++
		default:
			t.Fatalf("worker %d: unexpected error %v", i, err)
		}
	}
	if won != 1 || conflicted != workers-1 {
		t.Fatalf("winners=%d conflicts=%d, want 1/%d", won, conflicted, workers-1)
	}
	if n := f.operationCount(t); n != 1 {
		t.Fatalf("operation rows = %d, want 1", n)
	}
}

func TestPostgresAdmitExactSessionLaunchDifferentKeysOnOneCardAdmitExactlyOne(t *testing.T) {
	const workers = 8
	f := newExactLaunchPostgresFixture(t, workers)
	f.task(t, "task-a")
	errs := make([]error, workers)
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			adm := exactLaunchAdmission("task-a", fmt.Sprintf("key-%d", i), fmt.Sprintf("session-%d", i))
			<-start
			_, _, errs[i] = f.repo.AdmitExactSessionLaunch(f.ctx, adm)
		}(i)
	}
	close(start)
	wg.Wait()
	won, busy := 0, 0
	for i, err := range errs {
		switch {
		case err == nil:
			won++
		case errors.Is(err, repoerrors.ErrExactLaunchBusy):
			busy++
		default:
			t.Fatalf("worker %d: unexpected error %v", i, err)
		}
	}
	if won != 1 || busy != workers-1 {
		t.Fatalf("winners=%d busy=%d, want 1/%d", won, busy, workers-1)
	}
	if n := f.operationCount(t); n != 1 {
		t.Fatalf("operation rows = %d, want 1", n)
	}
}

func TestPostgresAdmitExactSessionLaunchWaitsForKeyAdvisoryLock(t *testing.T) {
	f := newExactLaunchPostgresFixture(t, 4)
	f.task(t, "task-a")
	adm := exactLaunchAdmission("task-a", "k1", "new-1")
	holder, err := f.repo.db.BeginTxx(f.ctx, nil)
	if err != nil {
		t.Fatalf("begin holder: %v", err)
	}
	t.Cleanup(func() { _ = holder.Rollback() })
	lockKey := exactLaunchKeyLockNamespace + adm.Operation.PrincipalScope + "\x00" + adm.Operation.WorkspaceID + "\x00" + adm.Operation.IdempotencyKey
	if _, err := holder.ExecContext(f.ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1, 0))`, lockKey); err != nil {
		t.Fatalf("hold key lock: %v", err)
	}
	err = requireBlockedUntilReleased(t, func() { _ = holder.Rollback() }, func() error {
		_, _, err := f.repo.AdmitExactSessionLaunch(f.ctx, adm)
		return err
	})
	if err != nil {
		t.Fatalf("admit after release: %v", err)
	}
	if n := f.operationCount(t); n != 1 {
		t.Fatalf("operation rows = %d", n)
	}
}

func TestPostgresInsertExactLaunchConflictReturnsStoredOperation(t *testing.T) {
	f := newExactLaunchPostgresFixture(t, 4)
	f.task(t, "task-a")
	first, _, err := f.repo.AdmitExactSessionLaunch(f.ctx, exactLaunchAdmission("task-a", "k1", "new-1"))
	if err != nil {
		t.Fatalf("admit: %v", err)
	}
	tx, err := f.repo.db.BeginTxx(f.ctx, nil)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer func() { _ = tx.Rollback() }()
	dup := exactLaunchAdmission("task-a", "k1", "new-dup").Operation
	dup.OperationID = "op-dup"
	stored, replayed, err := f.repo.insertExactLaunch(f.ctx, tx, dup)
	if err != nil || !replayed || stored.OperationID != first.OperationID || stored.SessionID != "new-1" {
		t.Fatalf("conflicting insert = %+v replayed=%v err=%v", stored, replayed, err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}
	if n := f.operationCount(t); n != 1 {
		t.Fatalf("operation rows = %d, want 1", n)
	}
}

func TestPostgresAdmitExactSessionLaunchWaitsForTaskRowLock(t *testing.T) {
	f := newExactLaunchPostgresFixture(t, 4)
	f.task(t, "task-a")
	holder, err := f.repo.db.BeginTxx(f.ctx, nil)
	if err != nil {
		t.Fatalf("begin holder: %v", err)
	}
	t.Cleanup(func() { _ = holder.Rollback() })
	if _, err := holder.ExecContext(f.ctx, `SELECT id FROM tasks WHERE id = 'task-a' FOR UPDATE`); err != nil {
		t.Fatalf("hold task lock: %v", err)
	}
	err = requireBlockedUntilReleased(t, func() { _ = holder.Rollback() }, func() error {
		_, _, err := f.repo.AdmitExactSessionLaunch(f.ctx, exactLaunchAdmission("task-a", "k1", "new-1"))
		return err
	})
	if err != nil {
		t.Fatalf("admit after release: %v", err)
	}
}
