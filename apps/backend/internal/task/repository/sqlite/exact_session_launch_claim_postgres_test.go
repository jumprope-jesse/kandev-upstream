package sqlite

import (
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kandev/kandev/internal/task/models"
	"github.com/kandev/kandev/internal/task/repository/repoerrors"
)

func preparedUpdate() models.ExactSessionLaunchUpdate {
	return models.ExactSessionLaunchUpdate{
		Phase: models.ExactLaunchPhasePrepared, PrimaryStatus: models.ExactLaunchPrimaryPending, PromptStatus: models.ExactLaunchPromptPending,
	}
}

func TestPostgresClaimExactSessionLaunchIsSingleOwnerUntilLeaseExpires(t *testing.T) {
	f := newExactLaunchPostgresFixture(t, 4)
	op := admitExactLaunchForClaim(t, f)
	now := time.Date(2026, 11, 9, 10, 0, 0, 0, time.UTC)

	first, err := f.repo.ClaimExactSessionLaunch(f.ctx, op.OperationID, "worker-a", now, time.Minute)
	if err != nil || first.OwnerToken != "worker-a" || first.OwnerEpoch != 1 || first.LeaseExpiresAt == nil || !first.LeaseExpiresAt.Equal(now.Add(time.Minute)) {
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

func TestPostgresClaimExactSessionLaunchTakesOverAtTheLeaseBoundary(t *testing.T) {
	f := newExactLaunchPostgresFixture(t, 4)
	op := admitExactLaunchForClaim(t, f)
	now := time.Date(2026, 11, 9, 10, 0, 0, 0, time.UTC)
	if _, err := f.repo.ClaimExactSessionLaunch(f.ctx, op.OperationID, "a", now, time.Minute); err != nil {
		t.Fatalf("claim: %v", err)
	}
	if _, err := f.repo.ClaimExactSessionLaunch(f.ctx, op.OperationID, "b", now.Add(time.Minute-time.Microsecond), time.Minute); !errors.Is(err, repoerrors.ErrExactLaunchOwned) {
		t.Fatalf("a lease that has not expired must hold, err = %v", err)
	}
	if got, err := f.repo.ClaimExactSessionLaunch(f.ctx, op.OperationID, "b", now.Add(time.Minute), time.Minute); err != nil || got.OwnerToken != "b" {
		t.Fatalf("a lease expiring exactly now must be claimable: %+v err=%v", got, err)
	}
}

func TestPostgresClaimExactSessionLaunchRejectsIncompleteAndUnknown(t *testing.T) {
	f := newExactLaunchPostgresFixture(t, 4)
	op := admitExactLaunchForClaim(t, f)
	if _, err := f.repo.ClaimExactSessionLaunch(f.ctx, op.OperationID, "", time.Now(), time.Minute); err == nil {
		t.Fatal("an empty owner token must be refused")
	}
	if _, err := f.repo.ClaimExactSessionLaunch(f.ctx, op.OperationID, "w", time.Now(), 0); err == nil {
		t.Fatal("a zero lease must be refused")
	}
	if _, err := f.repo.ClaimExactSessionLaunch(f.ctx, "missing", "w", time.Now(), time.Minute); !errors.Is(err, repoerrors.ErrExactLaunchNotFound) {
		t.Fatalf("unknown operation err = %v", err)
	}
}

func TestPostgresExactSessionLaunchOldOwnerIsFencedAfterTakeover(t *testing.T) {
	f := newExactLaunchPostgresFixture(t, 4)
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
	if _, err := f.repo.AdvanceExactSessionLaunch(f.ctx, fenceOf(old), models.ExactLaunchPhaseAdmitted, preparedUpdate(), now); !errors.Is(err, repoerrors.ErrExactLaunchFenced) {
		t.Fatalf("old owner advance err = %v, want fenced", err)
	}
	if err := f.repo.RenewExactSessionLaunch(f.ctx, fenceOf(old), now, time.Minute); !errors.Is(err, repoerrors.ErrExactLaunchFenced) {
		t.Fatalf("old owner renew err = %v, want fenced", err)
	}
	if err := f.repo.ReleaseExactSessionLaunch(f.ctx, fenceOf(old)); err != nil {
		t.Fatalf("old owner release must be a harmless no-op: %v", err)
	}
	got, err := f.repo.AdvanceExactSessionLaunch(f.ctx, fenceOf(newer), models.ExactLaunchPhaseAdmitted, preparedUpdate(), now)
	if err != nil || got.Phase != models.ExactLaunchPhasePrepared || got.OwnerToken != "new" {
		t.Fatalf("new owner advance = %+v err=%v (a stale release must not clear the new owner)", got, err)
	}
}

func TestPostgresRenewExactSessionLaunchExtendsOnlyTheCurrentLease(t *testing.T) {
	f := newExactLaunchPostgresFixture(t, 4)
	op := admitExactLaunchForClaim(t, f)
	now := time.Date(2026, 11, 9, 10, 0, 0, 0, time.UTC)
	claimed, err := f.repo.ClaimExactSessionLaunch(f.ctx, op.OperationID, "w", now, time.Minute)
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	if err := f.repo.RenewExactSessionLaunch(f.ctx, fenceOf(claimed), now.Add(30*time.Second), time.Hour); err != nil {
		t.Fatalf("renew: %v", err)
	}
	got, err := f.repo.GetExactSessionLaunch(f.ctx, op.OperationID)
	if err != nil || got.LeaseExpiresAt == nil || !got.LeaseExpiresAt.Equal(now.Add(30*time.Second+time.Hour)) {
		t.Fatalf("lease after renew = %+v err=%v", got, err)
	}
	if _, err := f.repo.ClaimExactSessionLaunch(f.ctx, op.OperationID, "other", now.Add(2*time.Minute), time.Minute); !errors.Is(err, repoerrors.ErrExactLaunchOwned) {
		t.Fatalf("a renewed lease must keep refusing claimers, err = %v", err)
	}
	wrongEpoch := fenceOf(claimed)
	wrongEpoch.OwnerEpoch++
	if err := f.repo.RenewExactSessionLaunch(f.ctx, wrongEpoch, now, time.Minute); !errors.Is(err, repoerrors.ErrExactLaunchFenced) {
		t.Fatalf("a wrong epoch must be fenced, err = %v", err)
	}
}

func TestPostgresAdvanceExactSessionLaunchRequiresExpectedPhase(t *testing.T) {
	f := newExactLaunchPostgresFixture(t, 4)
	op := admitExactLaunchForClaim(t, f)
	claimed, err := f.repo.ClaimExactSessionLaunch(f.ctx, op.OperationID, "w", time.Now(), time.Minute)
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	update := models.ExactSessionLaunchUpdate{Phase: models.ExactLaunchPhasePromptAccepted, PrimaryStatus: models.ExactLaunchPrimaryPending, PromptStatus: models.ExactLaunchPromptAccepted}
	if _, err := f.repo.AdvanceExactSessionLaunch(f.ctx, fenceOf(claimed), models.ExactLaunchPhasePrepared, update, time.Now()); !errors.Is(err, repoerrors.ErrExactLaunchFenced) {
		t.Fatalf("phase skip err = %v, want fenced", err)
	}
	got, err := f.repo.GetExactSessionLaunch(f.ctx, op.OperationID)
	if err != nil || got.Phase != models.ExactLaunchPhaseAdmitted {
		t.Fatalf("a refused advance must leave the phase alone: %+v err=%v", got, err)
	}
}

func TestPostgresExactSessionLaunchAdvancePersistsEveryStatusColumn(t *testing.T) {
	f := newExactLaunchPostgresFixture(t, 4)
	op := admitExactLaunchForClaim(t, f)
	claimed, err := f.repo.ClaimExactSessionLaunch(f.ctx, op.OperationID, "w", time.Now(), time.Minute)
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	got, err := f.repo.AdvanceExactSessionLaunch(f.ctx, fenceOf(claimed), models.ExactLaunchPhaseAdmitted, models.ExactSessionLaunchUpdate{
		Phase: models.ExactLaunchPhasePromptDispatched, PrimaryStatus: models.ExactLaunchPrimaryPending,
		PromptStatus: models.ExactLaunchPromptUncertain, ErrorCode: "code", ErrorMessage: "message",
	}, time.Now())
	if err != nil {
		t.Fatalf("advance: %v", err)
	}
	if got.Phase != models.ExactLaunchPhasePromptDispatched || got.PromptStatus != models.ExactLaunchPromptUncertain ||
		got.ErrorCode != "code" || got.ErrorMessage != "message" || got.OwnerToken != "w" || got.CompletedAt != nil {
		t.Fatalf("advanced op = %+v", got)
	}
}

func TestPostgresExactSessionLaunchTerminalAdvanceReleasesOwnerAndRefusesClaims(t *testing.T) {
	f := newExactLaunchPostgresFixture(t, 4)
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
	if done.OwnerToken != "" || done.LeaseExpiresAt != nil || done.CompletedAt == nil || done.Outcome() != models.ExactLaunchOutcomeFailed {
		t.Fatalf("terminal op = %+v", done)
	}
	again, err := f.repo.ClaimExactSessionLaunch(f.ctx, op.OperationID, "other", time.Now().Add(time.Hour), time.Minute)
	if err != nil || again.Phase != models.ExactLaunchPhaseFailed || again.OwnerToken != "" {
		t.Fatalf("claim of a terminal op must return it unowned: %+v err=%v", again, err)
	}
	if err := f.repo.RenewExactSessionLaunch(f.ctx, fenceOf(claimed), time.Now(), time.Minute); !errors.Is(err, repoerrors.ErrExactLaunchFenced) {
		t.Fatalf("renew of a terminal op err = %v, want fenced", err)
	}
	if open, err := f.repo.ListOpenExactSessionLaunches(f.ctx, 10); err != nil || len(open) != 0 {
		t.Fatalf("terminal op listed as open: %v err=%v", open, err)
	}
}

func TestPostgresListOpenExactSessionLaunchesRecoversFrozenInputAfterReopen(t *testing.T) {
	f := newExactLaunchPostgresFixture(t, 4)
	op := admitExactLaunchForClaim(t, f)
	if _, err := f.repo.ClaimExactSessionLaunch(f.ctx, op.OperationID, "dead-worker", time.Now().Add(-time.Hour), time.Minute); err != nil {
		t.Fatalf("claim: %v", err)
	}
	reopened, err := NewWithDB(f.repo.db, f.repo.ro, nil)
	if err != nil {
		t.Fatalf("reopen repository: %v", err)
	}
	open, err := reopened.ListOpenExactSessionLaunches(f.ctx, 10)
	if err != nil || len(open) != 1 {
		t.Fatalf("open = %v err=%v", open, err)
	}
	want := exactLaunchAdmission("task-a", "k1", "new-1").Operation.FrozenInput
	if open[0].FrozenInput != want || open[0].SessionID != "new-1" || open[0].BaselineActivityMarker != op.BaselineActivityMarker {
		t.Fatalf("recovery data lost: %+v", open[0])
	}
	revived, err := reopened.ClaimExactSessionLaunch(f.ctx, open[0].OperationID, "reconciler", time.Now(), time.Minute)
	if err != nil || revived.OwnerEpoch != 2 {
		t.Fatalf("reconciler takeover = %+v err=%v", revived, err)
	}
}

func TestPostgresListOpenExactSessionLaunchesOrdersOldestFirstAndHonorsLimit(t *testing.T) {
	f := newExactLaunchPostgresFixture(t, 4)
	for i := 0; i < 3; i++ {
		f.task(t, fmt.Sprintf("task-%d", i))
		adm := exactLaunchAdmission(fmt.Sprintf("task-%d", i), fmt.Sprintf("k%d", i), fmt.Sprintf("s%d", i))
		if _, _, err := f.repo.AdmitExactSessionLaunch(f.ctx, adm); err != nil {
			t.Fatalf("admit %d: %v", i, err)
		}
		time.Sleep(2 * time.Millisecond)
	}
	open, err := f.repo.ListOpenExactSessionLaunches(f.ctx, 2)
	if err != nil || len(open) != 2 || open[0].OperationID != "op-k0" || open[1].OperationID != "op-k1" {
		t.Fatalf("open = %v err=%v", open, err)
	}
}

func TestPostgresClaimExactSessionLaunchConcurrentClaimersYieldOneOwner(t *testing.T) {
	const claimers = 16
	f := newExactLaunchPostgresFixture(t, claimers)
	op := admitExactLaunchForClaim(t, f)
	var won, owned atomic.Int32
	start := make(chan struct{})
	var wg sync.WaitGroup
	now := time.Now()
	for i := 0; i < claimers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			_, err := f.repo.ClaimExactSessionLaunch(f.ctx, op.OperationID, fmt.Sprintf("claimer-%d", i), now, time.Hour)
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
	close(start)
	wg.Wait()
	if won.Load() != 1 || owned.Load() != claimers-1 {
		t.Fatalf("winners=%d refused=%d, want 1/%d", won.Load(), owned.Load(), claimers-1)
	}
	got, err := f.repo.GetExactSessionLaunch(f.ctx, op.OperationID)
	if err != nil || got.OwnerEpoch != 1 {
		t.Fatalf("epoch after the race = %+v err=%v, want 1", got, err)
	}
}

func TestPostgresAdvanceExactSessionLaunchConcurrentSameFenceAppliesOnce(t *testing.T) {
	const workers = 8
	f := newExactLaunchPostgresFixture(t, workers)
	op := admitExactLaunchForClaim(t, f)
	claimed, err := f.repo.ClaimExactSessionLaunch(f.ctx, op.OperationID, "w", time.Now(), time.Hour)
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	var applied, fenced atomic.Int32
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			_, err := f.repo.AdvanceExactSessionLaunch(f.ctx, fenceOf(claimed), models.ExactLaunchPhaseAdmitted, preparedUpdate(), time.Now())
			switch {
			case err == nil:
				applied.Add(1)
			case errors.Is(err, repoerrors.ErrExactLaunchFenced):
				fenced.Add(1)
			default:
				t.Errorf("advance: %v", err)
			}
		}()
	}
	close(start)
	wg.Wait()
	if applied.Load() != 1 || fenced.Load() != workers-1 {
		t.Fatalf("applied=%d fenced=%d, want 1/%d", applied.Load(), fenced.Load(), workers-1)
	}
}

func TestPostgresExactSessionLaunchTakeoverRacesStaleOwnerAdvance(t *testing.T) {
	const rounds = 10
	f := newExactLaunchPostgresFixture(t, 4)
	op := admitExactLaunchForClaim(t, f)
	base := time.Date(2026, 11, 9, 10, 0, 0, 0, time.UTC)
	owner, err := f.repo.ClaimExactSessionLaunch(f.ctx, op.OperationID, "owner-0", base, time.Minute)
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	for round := 1; round <= rounds; round++ {
		takeoverAt := base.Add(time.Duration(round) * time.Hour)
		var advanceErr, claimErr error
		var taker *models.ExactSessionLaunchOperation
		start := make(chan struct{})
		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			<-start
			// The stale owner re-applies the same phase: a no-op transition keeps the race about ownership.
			_, advanceErr = f.repo.AdvanceExactSessionLaunch(f.ctx, fenceOf(owner), models.ExactLaunchPhaseAdmitted, models.ExactSessionLaunchUpdate{
				Phase: models.ExactLaunchPhaseAdmitted, PrimaryStatus: models.ExactLaunchPrimaryPending, PromptStatus: models.ExactLaunchPromptPending,
			}, takeoverAt)
		}()
		go func() {
			defer wg.Done()
			<-start
			taker, claimErr = f.repo.ClaimExactSessionLaunch(f.ctx, op.OperationID, fmt.Sprintf("owner-%d", round), takeoverAt, time.Minute)
		}()
		close(start)
		wg.Wait()
		if claimErr != nil {
			t.Fatalf("round %d takeover: %v", round, claimErr)
		}
		if advanceErr != nil && !errors.Is(advanceErr, repoerrors.ErrExactLaunchFenced) {
			t.Fatalf("round %d stale advance: %v", round, advanceErr)
		}
		if _, err := f.repo.AdvanceExactSessionLaunch(f.ctx, fenceOf(owner), models.ExactLaunchPhaseAdmitted, preparedUpdate(), takeoverAt); !errors.Is(err, repoerrors.ErrExactLaunchFenced) {
			t.Fatalf("round %d: the displaced owner must be fenced afterwards, err = %v", round, err)
		}
		owner = taker
	}
	got, err := f.repo.GetExactSessionLaunch(f.ctx, op.OperationID)
	if err != nil || got.OwnerEpoch != int64(rounds+1) || got.Phase != models.ExactLaunchPhaseAdmitted {
		t.Fatalf("final op = %+v err=%v", got, err)
	}
}

func TestPostgresReleaseExactSessionLaunchLetsAnotherWorkerClaimImmediately(t *testing.T) {
	f := newExactLaunchPostgresFixture(t, 4)
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
