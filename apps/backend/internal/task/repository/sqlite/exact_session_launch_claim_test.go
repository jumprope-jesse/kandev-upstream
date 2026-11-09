package sqlite

import (
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kandev/kandev/internal/task/models"
	"github.com/kandev/kandev/internal/task/repository/repoerrors"
)

func admitExactLaunchForClaim(t *testing.T, f *exactLaunchFixture) *models.ExactSessionLaunchOperation {
	t.Helper()
	f.task(t, "task-a")
	op, _, err := f.repo.AdmitExactSessionLaunch(f.ctx, exactLaunchAdmission("task-a", "k1", "new-1"))
	if err != nil {
		t.Fatalf("admit: %v", err)
	}
	return op
}

func fenceOf(op *models.ExactSessionLaunchOperation) models.ExactSessionLaunchFence {
	return models.ExactSessionLaunchFence{OperationID: op.OperationID, OwnerToken: op.OwnerToken, OwnerEpoch: op.OwnerEpoch}
}

func TestClaimExactSessionLaunchIsSingleOwnerUntilLeaseExpires(t *testing.T) {
	f := newExactLaunchFixture(t)
	op := admitExactLaunchForClaim(t, f)
	now := time.Date(2026, 11, 9, 10, 0, 0, 0, time.UTC)

	first, err := f.repo.ClaimExactSessionLaunch(f.ctx, op.OperationID, "worker-a", now, time.Minute)
	if err != nil || first.OwnerToken != "worker-a" || first.OwnerEpoch != 1 {
		t.Fatalf("first claim = %+v err=%v", first, err)
	}
	if _, err := f.repo.ClaimExactSessionLaunch(f.ctx, op.OperationID, "worker-b", now.Add(30*time.Second), time.Minute); !errors.Is(err, repoerrors.ErrExactLaunchOwned) {
		t.Fatalf("live lease must refuse a second owner, err = %v", err)
	}
	second, err := f.repo.ClaimExactSessionLaunch(f.ctx, op.OperationID, "worker-b", now.Add(2*time.Minute), time.Minute)
	if err != nil || second.OwnerToken != "worker-b" || second.OwnerEpoch != 2 {
		t.Fatalf("takeover = %+v err=%v", second, err)
	}
}

func TestExactSessionLaunchOldOwnerIsFencedAfterTakeover(t *testing.T) {
	f := newExactLaunchFixture(t)
	op := admitExactLaunchForClaim(t, f)
	now := time.Date(2026, 11, 9, 10, 0, 0, 0, time.UTC)
	old, err := f.repo.ClaimExactSessionLaunch(f.ctx, op.OperationID, "old", now, time.Minute)
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	newer, err := f.repo.ClaimExactSessionLaunch(f.ctx, op.OperationID, "new", now.Add(time.Hour), time.Minute)
	if err != nil {
		t.Fatalf("takeover: %v", err)
	}

	update := models.ExactSessionLaunchUpdate{
		Phase: models.ExactLaunchPhasePrepared, PrimaryStatus: models.ExactLaunchPrimaryPending, PromptStatus: models.ExactLaunchPromptPending,
	}
	if _, err := f.repo.AdvanceExactSessionLaunch(f.ctx, fenceOf(old), models.ExactLaunchPhaseAdmitted, update, now); !errors.Is(err, repoerrors.ErrExactLaunchFenced) {
		t.Fatalf("old owner advance err = %v, want fenced", err)
	}
	if err := f.repo.RenewExactSessionLaunch(f.ctx, fenceOf(old), now, time.Minute); !errors.Is(err, repoerrors.ErrExactLaunchFenced) {
		t.Fatalf("old owner renew err = %v, want fenced", err)
	}
	got, err := f.repo.AdvanceExactSessionLaunch(f.ctx, fenceOf(newer), models.ExactLaunchPhaseAdmitted, update, now)
	if err != nil || got.Phase != models.ExactLaunchPhasePrepared {
		t.Fatalf("new owner advance = %+v err=%v", got, err)
	}
}

func TestAdvanceExactSessionLaunchRequiresExpectedPhase(t *testing.T) {
	f := newExactLaunchFixture(t)
	op := admitExactLaunchForClaim(t, f)
	claimed, err := f.repo.ClaimExactSessionLaunch(f.ctx, op.OperationID, "w", time.Now(), time.Minute)
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	update := models.ExactSessionLaunchUpdate{Phase: models.ExactLaunchPhasePromptAccepted, PrimaryStatus: models.ExactLaunchPrimaryPending, PromptStatus: models.ExactLaunchPromptAccepted}
	if _, err := f.repo.AdvanceExactSessionLaunch(f.ctx, fenceOf(claimed), models.ExactLaunchPhasePrepared, update, time.Now()); !errors.Is(err, repoerrors.ErrExactLaunchFenced) {
		t.Fatalf("phase skip err = %v, want fenced", err)
	}
}

func TestExactSessionLaunchTerminalAdvanceReleasesOwnerAndRefusesClaims(t *testing.T) {
	f := newExactLaunchFixture(t)
	op := admitExactLaunchForClaim(t, f)
	claimed, err := f.repo.ClaimExactSessionLaunch(f.ctx, op.OperationID, "w", time.Now(), time.Minute)
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	done, err := f.repo.AdvanceExactSessionLaunch(f.ctx, fenceOf(claimed), models.ExactLaunchPhaseAdmitted, models.ExactSessionLaunchUpdate{
		Phase: models.ExactLaunchPhaseFailed, PrimaryStatus: models.ExactLaunchPrimaryPending, PromptStatus: models.ExactLaunchPromptNotDelivered,
		ErrorCode: "gate_refused", ErrorMessage: "profile unavailable",
	}, time.Now())
	if err != nil {
		t.Fatalf("terminal advance: %v", err)
	}
	if done.OwnerToken != "" || done.CompletedAt == nil || done.Outcome() != models.ExactLaunchOutcomeFailed {
		t.Fatalf("terminal op = %+v", done)
	}
	again, err := f.repo.ClaimExactSessionLaunch(f.ctx, op.OperationID, "other", time.Now().Add(time.Hour), time.Minute)
	if err != nil || again.Phase != models.ExactLaunchPhaseFailed || again.OwnerToken != "" {
		t.Fatalf("claim of a terminal op must return it unowned: %+v err=%v", again, err)
	}
	open, err := f.repo.ListOpenExactSessionLaunches(f.ctx, 10)
	if err != nil || len(open) != 0 {
		t.Fatalf("terminal op listed as open: %v err=%v", open, err)
	}
}

func TestListOpenExactSessionLaunchesSurvivesReopenedRepositoryHandle(t *testing.T) {
	f := newExactLaunchFixture(t)
	op := admitExactLaunchForClaim(t, f)
	if _, err := f.repo.ClaimExactSessionLaunch(f.ctx, op.OperationID, "dead-worker", time.Now().Add(-time.Hour), time.Minute); err != nil {
		t.Fatalf("claim: %v", err)
	}
	open, err := f.repo.ListOpenExactSessionLaunches(f.ctx, 10)
	if err != nil || len(open) != 1 {
		t.Fatalf("open = %v err=%v", open, err)
	}
	want := exactLaunchAdmission("task-a", "k1", "new-1").Operation.FrozenInput
	if open[0].FrozenInput != want || open[0].SessionID != "new-1" {
		t.Fatalf("recovery data lost: %+v", open[0])
	}
	revived, err := f.repo.ClaimExactSessionLaunch(f.ctx, open[0].OperationID, "reconciler", time.Now(), time.Minute)
	if err != nil || revived.OwnerEpoch != 2 {
		t.Fatalf("reconciler takeover = %+v err=%v", revived, err)
	}
}

func TestClaimExactSessionLaunchConcurrentClaimersYieldOneOwner(t *testing.T) {
	f := newExactLaunchFixture(t)
	op := admitExactLaunchForClaim(t, f)
	var won, owned atomic.Int32
	var wg sync.WaitGroup
	now := time.Now()
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, err := f.repo.ClaimExactSessionLaunch(f.ctx, op.OperationID, string(rune('a'+i)), now, time.Hour)
			switch {
			case err == nil:
				won.Add(1)
			case errors.Is(err, repoerrors.ErrExactLaunchOwned):
				owned.Add(1)
			default:
				t.Errorf("claimer %d: %v", i, err)
			}
		}(i)
	}
	wg.Wait()
	if won.Load() != 1 || owned.Load() != 15 {
		t.Fatalf("winners=%d refused=%d, want 1/15", won.Load(), owned.Load())
	}
}

func TestReleaseExactSessionLaunchLetsAnotherWorkerClaimImmediately(t *testing.T) {
	f := newExactLaunchFixture(t)
	op := admitExactLaunchForClaim(t, f)
	now := time.Now()
	claimed, err := f.repo.ClaimExactSessionLaunch(f.ctx, op.OperationID, "a", now, time.Hour)
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	if err := f.repo.ReleaseExactSessionLaunch(f.ctx, fenceOf(claimed)); err != nil {
		t.Fatalf("release: %v", err)
	}
	next, err := f.repo.ClaimExactSessionLaunch(f.ctx, op.OperationID, "b", now, time.Hour)
	if err != nil || next.OwnerToken != "b" || next.OwnerEpoch != claimed.OwnerEpoch+1 {
		t.Fatalf("claim after release = %+v err=%v", next, err)
	}
}
