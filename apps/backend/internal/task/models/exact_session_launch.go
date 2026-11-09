package models

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"
)

const (
	// ExactSessionLaunchDigestVersion versions the canonical digest encoding.
	ExactSessionLaunchDigestVersion = 1
	// ExactSessionLaunchFrozenInputVersion versions the stored launch input.
	ExactSessionLaunchFrozenInputVersion = 1
	// MaxExactSessionLaunchKeyBytes bounds a caller idempotency key.
	MaxExactSessionLaunchKeyBytes = 200
)

// ExactSessionLaunchPhase is the durable position of one launch operation.
type ExactSessionLaunchPhase string

const (
	ExactLaunchPhaseAdmitted         ExactSessionLaunchPhase = "admitted"
	ExactLaunchPhasePrepared         ExactSessionLaunchPhase = "prepared"
	ExactLaunchPhasePromptDispatched ExactSessionLaunchPhase = "prompt_dispatching"
	ExactLaunchPhasePromptAccepted   ExactSessionLaunchPhase = "prompt_accepted"
	ExactLaunchPhaseCompleted        ExactSessionLaunchPhase = "completed"
	ExactLaunchPhaseFailed           ExactSessionLaunchPhase = "failed"
	ExactLaunchPhaseUncertain        ExactSessionLaunchPhase = "uncertain"
)

// IsTerminal reports whether no driver may advance the operation further.
func (p ExactSessionLaunchPhase) IsTerminal() bool {
	return p == ExactLaunchPhaseCompleted || p == ExactLaunchPhaseFailed || p == ExactLaunchPhaseUncertain
}

// ExactSessionLaunchPrimaryStatus is the make-primary result.
type ExactSessionLaunchPrimaryStatus string

const (
	ExactLaunchPrimaryNotRequested ExactSessionLaunchPrimaryStatus = "not_requested"
	ExactLaunchPrimaryPending      ExactSessionLaunchPrimaryStatus = "pending"
	ExactLaunchPrimaryApplied      ExactSessionLaunchPrimaryStatus = "applied"
	ExactLaunchPrimaryAlready      ExactSessionLaunchPrimaryStatus = "already_primary"
	ExactLaunchPrimaryConflict     ExactSessionLaunchPrimaryStatus = "conflict_not_applied"
)

// ExactSessionLaunchPrimaryReason explains a not-applied primary.
type ExactSessionLaunchPrimaryReason string

const (
	ExactLaunchReasonNone           ExactSessionLaunchPrimaryReason = ""
	ExactLaunchReasonPrimaryChanged ExactSessionLaunchPrimaryReason = "primary_changed"
	ExactLaunchReasonBaselineActive ExactSessionLaunchPrimaryReason = "baseline_active"
	ExactLaunchReasonSessionEnded   ExactSessionLaunchPrimaryReason = "session_terminal"
	ExactLaunchReasonUnverified     ExactSessionLaunchPrimaryReason = "unverified"
)

// ExactSessionLaunchPromptStatus is the cached initial-prompt delivery state.
type ExactSessionLaunchPromptStatus string

const (
	ExactLaunchPromptPending      ExactSessionLaunchPromptStatus = "pending"
	ExactLaunchPromptAccepted     ExactSessionLaunchPromptStatus = "accepted"
	ExactLaunchPromptUncertain    ExactSessionLaunchPromptStatus = "uncertain"
	ExactLaunchPromptNotDelivered ExactSessionLaunchPromptStatus = "not_delivered"
)

// ExactSessionLaunchOutcome is the derived caller-visible result class.
type ExactSessionLaunchOutcome string

const (
	ExactLaunchOutcomeInProgress ExactSessionLaunchOutcome = "in_progress"
	ExactLaunchOutcomeCompleted  ExactSessionLaunchOutcome = "completed"
	ExactLaunchOutcomePartial    ExactSessionLaunchOutcome = "partial"
	ExactLaunchOutcomeFailed     ExactSessionLaunchOutcome = "failed"
	ExactLaunchOutcomeUncertain  ExactSessionLaunchOutcome = "uncertain"
)

var (
	// ErrInvalidExactSessionLaunch reports a request the operation refuses
	// before it creates any state.
	ErrInvalidExactSessionLaunch = errors.New("invalid exact session launch request")
)

// ExactSessionLaunchRequest is the logical request both the plugin Host
// command and the MCP tool build. Neither surface computes its own digest.
type ExactSessionLaunchRequest struct {
	WorkspaceID    string
	TaskID         string
	IdempotencyKey string
	Prompt         string
	AgentProfileID string
	MakePrimary    bool
	// ExpectedPrimarySessionID, when non-nil, asserts the baseline primary. An
	// empty value asserts that the card has no primary.
	ExpectedPrimarySessionID *string
	// ExpectedTaskResourceVersion is an admission precondition on the plugin
	// surface. It is not part of the occurrence identity.
	ExpectedTaskResourceVersion string
}

// Validate refuses a request that cannot name one bounded occurrence.
func (r ExactSessionLaunchRequest) Validate() error {
	switch {
	case strings.TrimSpace(r.WorkspaceID) == "":
		return fmt.Errorf("%w: workspace_id is required", ErrInvalidExactSessionLaunch)
	case strings.TrimSpace(r.TaskID) == "":
		return fmt.Errorf("%w: task_id is required", ErrInvalidExactSessionLaunch)
	case strings.TrimSpace(r.IdempotencyKey) == "":
		return fmt.Errorf("%w: idempotency_key is required", ErrInvalidExactSessionLaunch)
	case len(r.IdempotencyKey) > MaxExactSessionLaunchKeyBytes || !utf8.ValidString(r.IdempotencyKey):
		return fmt.Errorf("%w: idempotency_key is invalid or too long", ErrInvalidExactSessionLaunch)
	case strings.TrimSpace(r.Prompt) == "":
		return fmt.Errorf("%w: prompt is required", ErrInvalidExactSessionLaunch)
	case len(r.Prompt) > MaxInitialPromptSubmissionBytes || !utf8.ValidString(r.Prompt):
		return fmt.Errorf("%w: prompt is invalid or exceeds the size limit", ErrInvalidExactSessionLaunch)
	}
	return nil
}

// Digest is the versioned canonical digest of the occurrence identity. The
// resource version, claim fence, and approval data are admission preconditions
// and are excluded so a retry after a legitimate version bump replays.
func (r ExactSessionLaunchRequest) Digest() string {
	expectedAsserted := r.ExpectedPrimarySessionID != nil
	expected := ""
	if expectedAsserted {
		expected = *r.ExpectedPrimarySessionID
	}
	encoded, _ := json.Marshal(struct {
		Version          int    `json:"v"`
		WorkspaceID      string `json:"workspace_id"`
		TaskID           string `json:"task_id"`
		Prompt           string `json:"prompt"`
		AgentProfileID   string `json:"agent_profile_id"`
		MakePrimary      bool   `json:"make_primary"`
		ExpectedAsserted bool   `json:"expected_primary_asserted"`
		ExpectedPrimary  string `json:"expected_primary_session_id"`
	}{
		ExactSessionLaunchDigestVersion, r.WorkspaceID, r.TaskID, r.Prompt,
		r.AgentProfileID, r.MakePrimary, expectedAsserted, expected,
	})
	sum := sha256.Sum256(encoded)
	return "sha256:" + hex.EncodeToString(sum[:])
}

// ExactSessionLaunchFrozenInput is the validated launch input persisted with
// the operation so any driver can finish the launch without the original
// request and without resolving profile or defaults again.
type ExactSessionLaunchFrozenInput struct {
	Version           int    `json:"version"`
	Prompt            string `json:"prompt"`
	AgentProfileID    string `json:"agent_profile_id"`
	ExecutorID        string `json:"executor_id,omitempty"`
	ExecutorProfileID string `json:"executor_profile_id,omitempty"`
	WorkflowStepID    string `json:"workflow_step_id,omitempty"`
}

// Validate refuses a frozen input that cannot drive a launch.
func (f ExactSessionLaunchFrozenInput) Validate() error {
	if f.Version != ExactSessionLaunchFrozenInputVersion {
		return fmt.Errorf("%w: unsupported frozen input version", ErrInvalidExactSessionLaunch)
	}
	if strings.TrimSpace(f.Prompt) == "" || len(f.Prompt) > MaxInitialPromptSubmissionBytes {
		return fmt.Errorf("%w: frozen prompt is invalid", ErrInvalidExactSessionLaunch)
	}
	if strings.TrimSpace(f.AgentProfileID) == "" {
		return fmt.Errorf("%w: frozen agent profile is required", ErrInvalidExactSessionLaunch)
	}
	return nil
}

// ExactSessionLaunchOperation is the durable native record of one occurrence.
type ExactSessionLaunchOperation struct {
	OperationID    string
	PrincipalScope string
	WorkspaceID    string
	IdempotencyKey string
	RequestDigest  string
	TaskID         string
	SessionID      string

	FrozenInput ExactSessionLaunchFrozenInput

	BaselinePrimarySessionID string
	BaselineAsserted         bool
	BaselineActivityMarker   string
	MakePrimary              bool

	Phase         ExactSessionLaunchPhase
	PrimaryStatus ExactSessionLaunchPrimaryStatus
	PrimaryReason ExactSessionLaunchPrimaryReason
	PromptStatus  ExactSessionLaunchPromptStatus
	ErrorCode     string
	ErrorMessage  string

	OwnerToken     string
	OwnerEpoch     int64
	LeaseExpiresAt *time.Time

	CreatedAt   time.Time
	UpdatedAt   time.Time
	CompletedAt *time.Time
}

// Outcome derives the caller-visible class. A requested primary that was not
// applied is partial, never plain success.
func (o *ExactSessionLaunchOperation) Outcome() ExactSessionLaunchOutcome {
	switch o.Phase {
	case ExactLaunchPhaseFailed:
		return ExactLaunchOutcomeFailed
	case ExactLaunchPhaseUncertain:
		return ExactLaunchOutcomeUncertain
	case ExactLaunchPhaseCompleted:
		if o.PrimaryStatus == ExactLaunchPrimaryConflict {
			return ExactLaunchOutcomePartial
		}
		return ExactLaunchOutcomeCompleted
	default:
		return ExactLaunchOutcomeInProgress
	}
}

// ExactSessionLaunchFence names the single owner that may write an operation.
type ExactSessionLaunchFence struct {
	OperationID string
	OwnerToken  string
	OwnerEpoch  int64
}

// ExactSessionLaunchUpdate is the full mutable state one fenced transition
// writes. Only the fenced owner writes, so it carries every status column.
type ExactSessionLaunchUpdate struct {
	Phase         ExactSessionLaunchPhase
	PrimaryStatus ExactSessionLaunchPrimaryStatus
	PrimaryReason ExactSessionLaunchPrimaryReason
	PromptStatus  ExactSessionLaunchPromptStatus
	ErrorCode     string
	ErrorMessage  string
}

// ExactSessionLaunchAdmission is the repository input for admitting one
// operation. Operation carries the identity, digest, and frozen input;
// BaselineAsserted and BaselinePrimarySessionID carry the caller's expected
// primary, and the repository captures the actual baseline and activity marker.
// ExpectedResourceVersion and ClaimFence are checked only for a new operation.
type ExactSessionLaunchAdmission struct {
	Operation               ExactSessionLaunchOperation
	ExpectedResourceVersion string
	ClaimFence              *TaskManagementClaimFence
}

// PromoteSessionPrimaryResult is the outcome of a baseline-fenced promotion.
type PromoteSessionPrimaryResult struct {
	Status ExactSessionLaunchPrimaryStatus
	Reason ExactSessionLaunchPrimaryReason
}

type preallocatedSessionIDKey struct{}

// WithPreallocatedSessionID pins the session row an exact launch will create so
// a retry after a crash adopts or fails on the same primary key.
func WithPreallocatedSessionID(ctx context.Context, taskID, sessionID string) context.Context {
	return context.WithValue(ctx, preallocatedSessionIDKey{}, [2]string{taskID, sessionID})
}

// PreallocatedSessionIDFromContext returns the pinned session ID for taskID.
func PreallocatedSessionIDFromContext(ctx context.Context, taskID string) (string, bool) {
	pair, ok := ctx.Value(preallocatedSessionIDKey{}).([2]string)
	if !ok || pair[0] == "" || pair[1] == "" || pair[0] != taskID {
		return "", false
	}
	return pair[1], true
}
