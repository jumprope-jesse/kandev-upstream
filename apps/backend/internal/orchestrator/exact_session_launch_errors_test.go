package orchestrator

import (
	"errors"
	"fmt"
	"testing"

	"github.com/kandev/kandev/internal/task/models"
	"github.com/kandev/kandev/internal/task/repository/repoerrors"
)

func TestClassifyExactLaunchErrorReturnsStableKindsAndReasons(t *testing.T) {
	cases := []struct {
		err    error
		kind   ExactLaunchRefusalKind
		reason string
	}{
		{models.ErrInvalidExactSessionLaunch, ExactLaunchRefusalInvalid, "invalid_request"},
		{repoerrors.ErrTaskNotFound, ExactLaunchRefusalNotFound, "target_not_found"},
		{repoerrors.ErrTaskVersionConflict, ExactLaunchRefusalStale, "task_version_changed"},
		{repoerrors.ErrTaskManagementClaimConflict, ExactLaunchRefusalStale, "claim_changed"},
		{repoerrors.ErrExactLaunchBaselineMismatch, ExactLaunchRefusalStale, "primary_changed"},
		{repoerrors.ErrTaskOperationConflict, ExactLaunchRefusalStale, "idempotency_payload_mismatch"},
		{repoerrors.ErrExactLaunchBusy, ExactLaunchRefusalPrecondition, "task_busy"},
		{repoerrors.ErrExactLaunchTaskArchived, ExactLaunchRefusalPrecondition, "task_archived"},
		{ErrExactLaunchPendingMove, ExactLaunchRefusalPrecondition, "pending_move"},
		{ErrExactLaunchOfficeTask, ExactLaunchRefusalPrecondition, "office_task"},
		{ErrExactLaunchPassthrough, ExactLaunchRefusalPrecondition, "passthrough_profile"},
		{errExactLaunchCeilingRefused, ExactLaunchRefusalPrecondition, "launch_ceiling_refused"},
		{&sessionRecoveryRequiredError{}, ExactLaunchRefusalPrecondition, "recovery_required"},
		{ErrExactLaunchUnavailable, ExactLaunchRefusalUnavailable, "launch_unavailable"},
		{ErrServiceNotRunning, ExactLaunchRefusalUnavailable, "launch_unavailable"},
	}
	for _, tc := range cases {
		got, ok := ClassifyExactLaunchError(fmt.Errorf("wrapped: %w", tc.err))
		if !ok || got.Kind != tc.kind || got.Reason != tc.reason {
			t.Fatalf("%v classified as %+v ok=%v, want %s/%s", tc.err, got, ok, tc.kind, tc.reason)
		}
	}
	if _, ok := ClassifyExactLaunchError(errors.New("database is locked")); ok {
		t.Fatal("an unrecognised error must not be classified")
	}
	if _, ok := ClassifyExactLaunchError(nil); ok {
		t.Fatal("nil must not be classified")
	}
}
