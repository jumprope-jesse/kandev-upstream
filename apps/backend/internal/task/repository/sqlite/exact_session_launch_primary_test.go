package sqlite

import (
	"testing"
	"time"

	"github.com/kandev/kandev/internal/task/models"
)

// admittedExactLaunch admits an operation for task-a with the given baseline
// sessions seeded, then creates the new launch session in WAITING_FOR_INPUT.
func admittedExactLaunch(t *testing.T, f *exactLaunchFixture, seed func()) *models.ExactSessionLaunchOperation {
	t.Helper()
	f.task(t, "task-a")
	if seed != nil {
		seed()
	}
	op, _, err := f.repo.AdmitExactSessionLaunch(f.ctx, exactLaunchAdmission("task-a", "k1", "new-1"))
	if err != nil {
		t.Fatalf("admit: %v", err)
	}
	f.session(t, "task-a", "new-1", models.TaskSessionStateWaitingForInput, false)
	return op
}

func (f *exactLaunchFixture) primaries(t *testing.T) []string {
	t.Helper()
	ids, err := f.repo.exactLaunchPrimaryIDs(f.ctx, f.repo.db, "task-a")
	if err != nil {
		t.Fatalf("primaries: %v", err)
	}
	return ids
}

func (f *exactLaunchFixture) updatedAt(t *testing.T, sessionID string) time.Time {
	t.Helper()
	var ts time.Time
	if err := f.repo.db.GetContext(f.ctx, &ts, f.repo.db.Rebind(`SELECT updated_at FROM task_sessions WHERE id = ?`), sessionID); err != nil {
		t.Fatalf("updated_at: %v", err)
	}
	return ts
}

func TestPromoteSessionPrimaryIfBaselineAppliesOverIdleBaseline(t *testing.T) {
	f := newExactLaunchFixture(t)
	op := admittedExactLaunch(t, f, func() { f.session(t, "task-a", "old", models.TaskSessionStateWaitingForInput, true) })

	res, err := f.repo.PromoteSessionPrimaryIfBaseline(f.ctx, op)
	if err != nil || res.Status != models.ExactLaunchPrimaryApplied {
		t.Fatalf("result = %+v err=%v", res, err)
	}
	if got := f.primaries(t); len(got) != 1 || got[0] != "new-1" {
		t.Fatalf("primaries = %v", got)
	}
}

func TestPromoteSessionPrimaryIfBaselineAppliesWhenCardHadNoPrimary(t *testing.T) {
	f := newExactLaunchFixture(t)
	op := admittedExactLaunch(t, f, nil)
	res, err := f.repo.PromoteSessionPrimaryIfBaseline(f.ctx, op)
	if err != nil || res.Status != models.ExactLaunchPrimaryApplied {
		t.Fatalf("result = %+v err=%v", res, err)
	}
}

func TestPromoteSessionPrimaryIfBaselineAppliesOverEndedBaseline(t *testing.T) {
	f := newExactLaunchFixture(t)
	op := admittedExactLaunch(t, f, func() { f.session(t, "task-a", "old", models.TaskSessionStateFailed, true) })
	res, err := f.repo.PromoteSessionPrimaryIfBaseline(f.ctx, op)
	if err != nil || res.Status != models.ExactLaunchPrimaryApplied {
		t.Fatalf("result = %+v err=%v", res, err)
	}
}

func TestPromoteSessionPrimaryIfBaselineReportsAlreadyPrimaryWithoutWrites(t *testing.T) {
	f := newExactLaunchFixture(t)
	op := admittedExactLaunch(t, f, func() { f.session(t, "task-a", "old", models.TaskSessionStateWaitingForInput, true) })
	if _, err := f.repo.PromoteSessionPrimaryIfBaseline(f.ctx, op); err != nil {
		t.Fatalf("first promote: %v", err)
	}
	before := f.updatedAt(t, "new-1")
	res, err := f.repo.PromoteSessionPrimaryIfBaseline(f.ctx, op)
	if err != nil || res.Status != models.ExactLaunchPrimaryAlready {
		t.Fatalf("replay result = %+v err=%v", res, err)
	}
	if !f.updatedAt(t, "new-1").Equal(before) {
		t.Fatal("an already-primary replay must not bump the session version")
	}
}

func TestPromoteSessionPrimaryIfBaselineDoesNotStealChangedPrimary(t *testing.T) {
	f := newExactLaunchFixture(t)
	op := admittedExactLaunch(t, f, func() { f.session(t, "task-a", "old", models.TaskSessionStateWaitingForInput, true) })
	f.session(t, "task-a", "other", models.TaskSessionStateWaitingForInput, true)

	res, err := f.repo.PromoteSessionPrimaryIfBaseline(f.ctx, op)
	if err != nil || res.Status != models.ExactLaunchPrimaryConflict || res.Reason != models.ExactLaunchReasonPrimaryChanged {
		t.Fatalf("result = %+v err=%v", res, err)
	}
	if got := f.primaries(t); len(got) != 1 || got[0] != "other" {
		t.Fatalf("concurrent primary was displaced: %v", got)
	}
}

func TestPromoteSessionPrimaryIfBaselineRefusesBaselineThatStartedNewTurn(t *testing.T) {
	f := newExactLaunchFixture(t)
	op := admittedExactLaunch(t, f, func() { f.session(t, "task-a", "old", models.TaskSessionStateWaitingForInput, true) })
	f.exec(t, `UPDATE task_sessions SET state = ? WHERE id = 'old'`, string(models.TaskSessionStateRunning))
	f.turn(t, "task-a", "old", "t-new", true)

	res, err := f.repo.PromoteSessionPrimaryIfBaseline(f.ctx, op)
	if err != nil || res.Status != models.ExactLaunchPrimaryConflict || res.Reason != models.ExactLaunchReasonBaselineActive {
		t.Fatalf("result = %+v err=%v", res, err)
	}
	if got := f.primaries(t); len(got) != 1 || got[0] != "old" {
		t.Fatalf("active baseline was displaced: %v", got)
	}
}

func TestPromoteSessionPrimaryIfBaselineRefusesCompletedTurnSinceAdmission(t *testing.T) {
	f := newExactLaunchFixture(t)
	op := admittedExactLaunch(t, f, func() { f.session(t, "task-a", "old", models.TaskSessionStateWaitingForInput, true) })
	// The baseline ran and finished a whole turn between admission and commit:
	// it is idle again, but its activity marker moved, so it is not the
	// session the caller admitted against.
	f.turn(t, "task-a", "old", "t-done", false)

	res, err := f.repo.PromoteSessionPrimaryIfBaseline(f.ctx, op)
	if err != nil || res.Status != models.ExactLaunchPrimaryConflict || res.Reason != models.ExactLaunchReasonBaselineActive {
		t.Fatalf("result = %+v err=%v", res, err)
	}
}

func TestPromoteSessionPrimaryIfBaselineRefusesBaselineWithBackgroundWork(t *testing.T) {
	f := newExactLaunchFixture(t)
	op := admittedExactLaunch(t, f, func() { f.session(t, "task-a", "old", models.TaskSessionStateWaitingForInput, true) })
	f.exec(t, `INSERT INTO task_session_background_work (id, task_id, session_id, state) VALUES ('w1', 'task-a', 'old', 'running')`)

	res, err := f.repo.PromoteSessionPrimaryIfBaseline(f.ctx, op)
	if err != nil || res.Status != models.ExactLaunchPrimaryConflict || res.Reason != models.ExactLaunchReasonBaselineActive {
		t.Fatalf("result = %+v err=%v", res, err)
	}
}

func TestPromoteSessionPrimaryIfBaselineRefusesEndedLaunchSession(t *testing.T) {
	f := newExactLaunchFixture(t)
	op := admittedExactLaunch(t, f, func() { f.session(t, "task-a", "old", models.TaskSessionStateWaitingForInput, true) })
	f.exec(t, `UPDATE task_sessions SET state = ? WHERE id = 'new-1'`, string(models.TaskSessionStateCancelled))

	res, err := f.repo.PromoteSessionPrimaryIfBaseline(f.ctx, op)
	if err != nil || res.Status != models.ExactLaunchPrimaryConflict || res.Reason != models.ExactLaunchReasonSessionEnded {
		t.Fatalf("result = %+v err=%v", res, err)
	}
	if got := f.primaries(t); len(got) != 1 || got[0] != "old" {
		t.Fatalf("primary moved to an ended session: %v", got)
	}
}

func TestPromoteSessionPrimaryIfBaselineBumpsOnlyChangedRows(t *testing.T) {
	f := newExactLaunchFixture(t)
	op := admittedExactLaunch(t, f, func() {
		f.session(t, "task-a", "old", models.TaskSessionStateWaitingForInput, true)
		f.session(t, "task-a", "bystander", models.TaskSessionStateCompleted, false)
	})
	oldBefore, bystanderBefore := f.updatedAt(t, "old"), f.updatedAt(t, "bystander")
	time.Sleep(5 * time.Millisecond)

	if res, err := f.repo.PromoteSessionPrimaryIfBaseline(f.ctx, op); err != nil || res.Status != models.ExactLaunchPrimaryApplied {
		t.Fatalf("result = %+v err=%v", res, err)
	}
	if !f.updatedAt(t, "old").After(oldBefore) {
		t.Fatal("displaced primary must observe a version bump")
	}
	if !f.updatedAt(t, "bystander").Equal(bystanderBefore) {
		t.Fatal("a session whose flag did not change must keep its version")
	}
}
