package orchestrator

import (
	"errors"

	"github.com/kandev/kandev/internal/task/models"
	"github.com/kandev/kandev/internal/task/repository/repoerrors"
)

// ExactLaunchRefusalKind groups the refusals of an exact launch admission so a
// surface can map them to its own status vocabulary.
type ExactLaunchRefusalKind string

const (
	ExactLaunchRefusalInvalid      ExactLaunchRefusalKind = "invalid"
	ExactLaunchRefusalNotFound     ExactLaunchRefusalKind = "not_found"
	ExactLaunchRefusalStale        ExactLaunchRefusalKind = "stale"
	ExactLaunchRefusalPrecondition ExactLaunchRefusalKind = "precondition"
	ExactLaunchRefusalUnavailable  ExactLaunchRefusalKind = "unavailable"
)

// ExactLaunchRefusal is a stable, caller-visible classification of an admission
// error. Reason is a closed code, never free text.
type ExactLaunchRefusal struct {
	Kind   ExactLaunchRefusalKind
	Reason string
}

// ClassifyExactLaunchError returns the refusal class of err, or false when err
// is not a recognised admission refusal.
func ClassifyExactLaunchError(err error) (ExactLaunchRefusal, bool) {
	switch {
	case err == nil:
		return ExactLaunchRefusal{}, false
	case errors.Is(err, models.ErrInvalidExactSessionLaunch):
		return ExactLaunchRefusal{ExactLaunchRefusalInvalid, "invalid_request"}, true
	case errors.Is(err, repoerrors.ErrTaskNotFound):
		return ExactLaunchRefusal{ExactLaunchRefusalNotFound, "target_not_found"}, true
	case errors.Is(err, repoerrors.ErrTaskVersionConflict):
		return ExactLaunchRefusal{ExactLaunchRefusalStale, "task_version_changed"}, true
	case errors.Is(err, repoerrors.ErrTaskManagementClaimConflict):
		return ExactLaunchRefusal{ExactLaunchRefusalStale, "claim_changed"}, true
	case errors.Is(err, repoerrors.ErrExactLaunchBaselineMismatch):
		return ExactLaunchRefusal{ExactLaunchRefusalStale, "primary_changed"}, true
	case errors.Is(err, repoerrors.ErrTaskOperationConflict):
		return ExactLaunchRefusal{ExactLaunchRefusalStale, "idempotency_payload_mismatch"}, true
	}
	return classifyExactLaunchPrecondition(err)
}

func classifyExactLaunchPrecondition(err error) (ExactLaunchRefusal, bool) {
	var recovery *sessionRecoveryRequiredError
	switch {
	case errors.Is(err, repoerrors.ErrExactLaunchBusy):
		return ExactLaunchRefusal{ExactLaunchRefusalPrecondition, "task_busy"}, true
	case errors.Is(err, repoerrors.ErrExactLaunchTaskArchived):
		return ExactLaunchRefusal{ExactLaunchRefusalPrecondition, "task_archived"}, true
	case errors.Is(err, ErrExactLaunchPendingMove):
		return ExactLaunchRefusal{ExactLaunchRefusalPrecondition, "pending_move"}, true
	case errors.Is(err, ErrExactLaunchOfficeTask):
		return ExactLaunchRefusal{ExactLaunchRefusalPrecondition, "office_task"}, true
	case errors.Is(err, ErrExactLaunchPassthrough):
		return ExactLaunchRefusal{ExactLaunchRefusalPrecondition, "passthrough_profile"}, true
	case errors.Is(err, errExactLaunchCeilingRefused):
		return ExactLaunchRefusal{ExactLaunchRefusalPrecondition, "launch_ceiling_refused"}, true
	case errors.As(err, &recovery):
		return ExactLaunchRefusal{ExactLaunchRefusalPrecondition, "recovery_required"}, true
	case errors.Is(err, ErrExactLaunchUnavailable), errors.Is(err, ErrServiceNotRunning):
		return ExactLaunchRefusal{ExactLaunchRefusalUnavailable, "launch_unavailable"}, true
	}
	return ExactLaunchRefusal{}, false
}
