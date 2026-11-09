package sqlite

import (
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/kandev/kandev/internal/task/models"
)

func requirePromotion(t *testing.T, f *exactLaunchFixture, op *models.ExactSessionLaunchOperation, status models.ExactSessionLaunchPrimaryStatus, reason models.ExactSessionLaunchPrimaryReason) {
	t.Helper()
	res, err := f.repo.PromoteSessionPrimaryIfBaseline(f.ctx, op)
	if err != nil || res.Status != status || res.Reason != reason {
		t.Fatalf("result = %+v err=%v, want %s/%s", res, err, status, reason)
	}
}

func requirePrimaries(t *testing.T, f *exactLaunchFixture, want ...string) {
	t.Helper()
	got := f.primaries(t)
	if len(got) != len(want) {
		t.Fatalf("primaries = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("primaries = %v, want %v", got, want)
		}
	}
}

func TestPostgresPromoteSessionPrimaryIfBaselineAppliesOverIdleBaseline(t *testing.T) {
	f := newExactLaunchPostgresFixture(t, 4)
	op := admittedExactLaunch(t, f, func() { f.session(t, "task-a", "old", models.TaskSessionStateWaitingForInput, true) })
	requirePromotion(t, f, op, models.ExactLaunchPrimaryApplied, models.ExactLaunchReasonNone)
	requirePrimaries(t, f, "new-1")
}

func TestPostgresPromoteSessionPrimaryIfBaselineAppliesWhenCardHadNoPrimary(t *testing.T) {
	f := newExactLaunchPostgresFixture(t, 4)
	op := admittedExactLaunch(t, f, nil)
	requirePromotion(t, f, op, models.ExactLaunchPrimaryApplied, models.ExactLaunchReasonNone)
	requirePrimaries(t, f, "new-1")
}

func TestPostgresPromoteSessionPrimaryIfBaselineAppliesOverEndedBaseline(t *testing.T) {
	f := newExactLaunchPostgresFixture(t, 4)
	op := admittedExactLaunch(t, f, func() { f.session(t, "task-a", "old", models.TaskSessionStateFailed, true) })
	requirePromotion(t, f, op, models.ExactLaunchPrimaryApplied, models.ExactLaunchReasonNone)
	requirePrimaries(t, f, "new-1")
}

func TestPostgresPromoteSessionPrimaryIfBaselineReportsAlreadyPrimaryWithoutWrites(t *testing.T) {
	f := newExactLaunchPostgresFixture(t, 4)
	op := admittedExactLaunch(t, f, func() { f.session(t, "task-a", "old", models.TaskSessionStateWaitingForInput, true) })
	requirePromotion(t, f, op, models.ExactLaunchPrimaryApplied, models.ExactLaunchReasonNone)
	before := f.updatedAt(t, "new-1")
	requirePromotion(t, f, op, models.ExactLaunchPrimaryAlready, models.ExactLaunchReasonNone)
	if !f.updatedAt(t, "new-1").Equal(before) {
		t.Fatal("an already-primary replay must not bump the session version")
	}
}

func TestPostgresPromoteSessionPrimaryIfBaselineDoesNotStealChangedPrimary(t *testing.T) {
	f := newExactLaunchPostgresFixture(t, 4)
	op := admittedExactLaunch(t, f, func() { f.session(t, "task-a", "old", models.TaskSessionStateWaitingForInput, true) })
	f.session(t, "task-a", "other", models.TaskSessionStateWaitingForInput, true)
	requirePromotion(t, f, op, models.ExactLaunchPrimaryConflict, models.ExactLaunchReasonPrimaryChanged)
	requirePrimaries(t, f, "other")
}

func TestPostgresPromoteSessionPrimaryIfBaselineRefusesBaselineThatStartedNewTurn(t *testing.T) {
	f := newExactLaunchPostgresFixture(t, 4)
	op := admittedExactLaunch(t, f, func() { f.session(t, "task-a", "old", models.TaskSessionStateWaitingForInput, true) })
	f.exec(t, `UPDATE task_sessions SET state = ? WHERE id = 'old'`, string(models.TaskSessionStateRunning))
	f.turn(t, "task-a", "old", "t-new", true)
	requirePromotion(t, f, op, models.ExactLaunchPrimaryConflict, models.ExactLaunchReasonBaselineActive)
	requirePrimaries(t, f, "old")
}

func TestPostgresPromoteSessionPrimaryIfBaselineRefusesCompletedTurnSinceAdmission(t *testing.T) {
	f := newExactLaunchPostgresFixture(t, 4)
	op := admittedExactLaunch(t, f, func() { f.session(t, "task-a", "old", models.TaskSessionStateWaitingForInput, true) })
	f.turn(t, "task-a", "old", "t-done", false)
	requirePromotion(t, f, op, models.ExactLaunchPrimaryConflict, models.ExactLaunchReasonBaselineActive)
	requirePrimaries(t, f, "old")
}

func TestPostgresPromoteSessionPrimaryIfBaselineRefusesOpenBackgroundWork(t *testing.T) {
	cases := map[string]string{
		"work": `INSERT INTO task_session_background_work (id, task_id, session_id, state) VALUES ('w1', 'task-a', 'old', 'running')`,
		"run":  `INSERT INTO task_session_background_runs (id, workload_id, session_id, state) VALUES ('r1', 'w1', 'old', 'running')`,
	}
	for name, insert := range cases {
		t.Run(name, func(t *testing.T) {
			f := newExactLaunchPostgresFixture(t, 4)
			op := admittedExactLaunch(t, f, func() { f.session(t, "task-a", "old", models.TaskSessionStateWaitingForInput, true) })
			f.exec(t, insert)
			requirePromotion(t, f, op, models.ExactLaunchPrimaryConflict, models.ExactLaunchReasonBaselineActive)
			requirePrimaries(t, f, "old")
		})
	}
}

func TestPostgresPromoteSessionPrimaryIfBaselineRefusesEndedLaunchSession(t *testing.T) {
	f := newExactLaunchPostgresFixture(t, 4)
	op := admittedExactLaunch(t, f, func() { f.session(t, "task-a", "old", models.TaskSessionStateWaitingForInput, true) })
	f.exec(t, `UPDATE task_sessions SET state = ? WHERE id = 'new-1'`, string(models.TaskSessionStateCancelled))
	requirePromotion(t, f, op, models.ExactLaunchPrimaryConflict, models.ExactLaunchReasonSessionEnded)
	requirePrimaries(t, f, "old")
}

func TestPostgresPromoteSessionPrimaryIfBaselineBumpsOnlyChangedRows(t *testing.T) {
	f := newExactLaunchPostgresFixture(t, 4)
	op := admittedExactLaunch(t, f, func() {
		f.session(t, "task-a", "old", models.TaskSessionStateWaitingForInput, true)
		f.session(t, "task-a", "bystander", models.TaskSessionStateCompleted, false)
	})
	oldBefore, bystanderBefore := f.updatedAt(t, "old"), f.updatedAt(t, "bystander")
	time.Sleep(5 * time.Millisecond)
	requirePromotion(t, f, op, models.ExactLaunchPrimaryApplied, models.ExactLaunchReasonNone)
	if !f.updatedAt(t, "old").After(oldBefore) {
		t.Fatal("displaced primary must observe a version bump")
	}
	if !f.updatedAt(t, "bystander").Equal(bystanderBefore) {
		t.Fatal("a session whose flag did not change must keep its version")
	}
}

func TestPostgresPromoteSessionPrimaryIfBaselineConcurrentPromotersApplyOnce(t *testing.T) {
	const workers = 8
	f := newExactLaunchPostgresFixture(t, workers)
	op := admittedExactLaunch(t, f, func() { f.session(t, "task-a", "old", models.TaskSessionStateWaitingForInput, true) })
	results := make([]models.PromoteSessionPrimaryResult, workers)
	errs := make([]error, workers)
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			results[i], errs[i] = f.repo.PromoteSessionPrimaryIfBaseline(f.ctx, op)
		}(i)
	}
	close(start)
	wg.Wait()
	applied, already := 0, 0
	for i, res := range results {
		if errs[i] != nil {
			t.Fatalf("promoter %d: %v", i, errs[i])
		}
		switch res.Status {
		case models.ExactLaunchPrimaryApplied:
			applied++
		case models.ExactLaunchPrimaryAlready:
			already++
		default:
			t.Fatalf("promoter %d: unexpected result %+v", i, res)
		}
	}
	if applied != 1 || already != workers-1 {
		t.Fatalf("applied=%d already=%d, want 1/%d", applied, already, workers-1)
	}
	requirePrimaries(t, f, "new-1")
}

func TestPostgresPromoteSessionPrimaryIfBaselineRacesAnotherPrimaryWriter(t *testing.T) {
	const rounds = 8
	for round := 0; round < rounds; round++ {
		t.Run(fmt.Sprintf("round-%d", round), func(t *testing.T) {
			f := newExactLaunchPostgresFixture(t, 4)
			op := admittedExactLaunch(t, f, func() { f.session(t, "task-a", "old", models.TaskSessionStateWaitingForInput, true) })
			f.session(t, "task-a", "other", models.TaskSessionStateWaitingForInput, false)
			var promote models.PromoteSessionPrimaryResult
			var promoteErr, setErr error
			start := make(chan struct{})
			var wg sync.WaitGroup
			wg.Add(2)
			go func() {
				defer wg.Done()
				<-start
				promote, promoteErr = f.repo.PromoteSessionPrimaryIfBaseline(f.ctx, op)
			}()
			go func() {
				defer wg.Done()
				<-start
				setErr = f.repo.SetSessionPrimary(f.ctx, "other")
			}()
			close(start)
			wg.Wait()
			if promoteErr != nil || setErr != nil {
				t.Fatalf("promote=%v set=%v", promoteErr, setErr)
			}
			if len(f.primaries(t)) != 1 {
				t.Fatalf("a race left %v primaries", f.primaries(t))
			}
			// Whichever writer is serialized second wins the flag; the launch must
			// report Applied only when it ran first and was then displaced, and
			// primary_changed when it ran second.
			switch promote.Status {
			case models.ExactLaunchPrimaryApplied:
			case models.ExactLaunchPrimaryConflict:
				if promote.Reason != models.ExactLaunchReasonPrimaryChanged {
					t.Fatalf("conflict reason = %s", promote.Reason)
				}
			default:
				t.Fatalf("promote result = %+v", promote)
			}
			requirePrimaries(t, f, "other")
		})
	}
}

func TestPostgresPromoteSessionPrimaryIfBaselineWaitsForTaskRowLock(t *testing.T) {
	f := newExactLaunchPostgresFixture(t, 4)
	op := admittedExactLaunch(t, f, func() { f.session(t, "task-a", "old", models.TaskSessionStateWaitingForInput, true) })
	holder, err := f.repo.db.BeginTxx(f.ctx, nil)
	if err != nil {
		t.Fatalf("begin holder: %v", err)
	}
	t.Cleanup(func() { _ = holder.Rollback() })
	if _, err := holder.ExecContext(f.ctx, `SELECT id FROM tasks WHERE id = 'task-a' FOR UPDATE`); err != nil {
		t.Fatalf("hold task lock: %v", err)
	}
	var res models.PromoteSessionPrimaryResult
	err = requireBlockedUntilReleased(t, func() { _ = holder.Rollback() }, func() error {
		var err error
		res, err = f.repo.PromoteSessionPrimaryIfBaseline(f.ctx, op)
		return err
	})
	if err != nil || res.Status != models.ExactLaunchPrimaryApplied {
		t.Fatalf("promote after release = %+v err=%v", res, err)
	}
	requirePrimaries(t, f, "new-1")
}

func TestPostgresPromoteSessionPrimaryIfBaselineSeesActivityCommittedWhileWaiting(t *testing.T) {
	f := newExactLaunchPostgresFixture(t, 4)
	op := admittedExactLaunch(t, f, func() { f.session(t, "task-a", "old", models.TaskSessionStateWaitingForInput, true) })
	holder, err := f.repo.db.BeginTxx(f.ctx, nil)
	if err != nil {
		t.Fatalf("begin holder: %v", err)
	}
	t.Cleanup(func() { _ = holder.Rollback() })
	if _, err := holder.ExecContext(f.ctx, `SELECT id FROM tasks WHERE id = 'task-a' FOR UPDATE`); err != nil {
		t.Fatalf("hold task lock: %v", err)
	}
	var res models.PromoteSessionPrimaryResult
	var competing error
	err = requireBlockedUntilReleased(t, func() {
		// The competing writer starts a turn on the baseline and commits; the
		// waiting promotion must evaluate against that committed state.
		now := time.Now().UTC()
		if _, competing = holder.ExecContext(f.ctx, holder.Rebind(`UPDATE task_sessions SET state = 'RUNNING' WHERE id = 'old'`)); competing != nil {
			return
		}
		if _, competing = holder.ExecContext(f.ctx, holder.Rebind(`INSERT INTO task_session_turns (id, task_session_id, task_id, started_at, completed_at, created_at, updated_at)
			VALUES ('t-race', 'old', 'task-a', ?, NULL, ?, ?)`), now, now, now); competing != nil {
			return
		}
		competing = holder.Commit()
	}, func() error {
		var err error
		res, err = f.repo.PromoteSessionPrimaryIfBaseline(f.ctx, op)
		return err
	})
	if competing != nil {
		t.Fatalf("competing writer: %v", competing)
	}
	if err != nil || res.Status != models.ExactLaunchPrimaryConflict || res.Reason != models.ExactLaunchReasonBaselineActive {
		t.Fatalf("promote after a concurrent turn start = %+v err=%v", res, err)
	}
	requirePrimaries(t, f, "old")
}
