package sqlite

import (
	"context"
	"strings"
	"time"

	"github.com/jmoiron/sqlx"

	"github.com/kandev/kandev/internal/db/dialect"
	"github.com/kandev/kandev/internal/task/models"
)

func exactLaunchPrimaryResult(status models.ExactSessionLaunchPrimaryStatus, reason models.ExactSessionLaunchPrimaryReason) models.PromoteSessionPrimaryResult {
	return models.PromoteSessionPrimaryResult{Status: status, Reason: reason}
}

// PromoteSessionPrimaryIfBaseline makes the operation's session primary only
// when the card's primary set is still the admitted baseline and every
// nonterminal baseline session is provably idle with the admitted activity
// marker. The task lock is taken first, matching every other primary writer.
func (r *Repository) PromoteSessionPrimaryIfBaseline(
	ctx context.Context,
	op *models.ExactSessionLaunchOperation,
) (models.PromoteSessionPrimaryResult, error) {
	tx, err := r.db.BeginTxx(ctx, nil)
	if err != nil {
		return models.PromoteSessionPrimaryResult{}, err
	}
	defer func() { _ = tx.Rollback() }()
	if err := r.managedDeletionBarrierTx(ctx, tx, op.TaskID); err != nil {
		return models.PromoteSessionPrimaryResult{}, err
	}
	result, err := r.promoteIfBaselineTx(ctx, tx, op)
	if err != nil {
		return models.PromoteSessionPrimaryResult{}, err
	}
	if err := tx.Commit(); err != nil {
		return models.PromoteSessionPrimaryResult{}, err
	}
	return result, nil
}

func (r *Repository) promoteIfBaselineTx(ctx context.Context, tx *sqlx.Tx, op *models.ExactSessionLaunchOperation) (models.PromoteSessionPrimaryResult, error) {
	live, err := r.lockNonterminalPrimarySession(ctx, tx, op.SessionID)
	if err != nil {
		return models.PromoteSessionPrimaryResult{}, err
	}
	if !live {
		return exactLaunchPrimaryResult(models.ExactLaunchPrimaryConflict, models.ExactLaunchReasonSessionEnded), nil
	}
	current, err := r.exactLaunchPrimaryIDs(ctx, tx, op.TaskID)
	if err != nil {
		return models.PromoteSessionPrimaryResult{}, err
	}
	if len(current) == 1 && current[0] == op.SessionID {
		return exactLaunchPrimaryResult(models.ExactLaunchPrimaryAlready, models.ExactLaunchReasonNone), nil
	}
	if strings.Join(current, ",") != op.BaselinePrimarySessionID {
		return exactLaunchPrimaryResult(models.ExactLaunchPrimaryConflict, models.ExactLaunchReasonPrimaryChanged), nil
	}
	if idle, err := r.exactLaunchBaselineIdle(ctx, tx, op); err != nil {
		return models.PromoteSessionPrimaryResult{}, err
	} else if !idle {
		return exactLaunchPrimaryResult(models.ExactLaunchPrimaryConflict, models.ExactLaunchReasonBaselineActive), nil
	}
	if err := r.applyExactLaunchPrimary(ctx, tx, op); err != nil {
		return models.PromoteSessionPrimaryResult{}, err
	}
	return exactLaunchPrimaryResult(models.ExactLaunchPrimaryApplied, models.ExactLaunchReasonNone), nil
}

// exactLaunchBaselineIdle reports whether displacing the baseline primary is
// safe: it ended, or it kept the admitted marker and has no open work. A new
// turn since admission changes the marker, so primary identity alone never
// proves idleness.
func (r *Repository) exactLaunchBaselineIdle(ctx context.Context, tx *sqlx.Tx, op *models.ExactSessionLaunchOperation) (bool, error) {
	if op.BaselinePrimarySessionID == "" {
		return true, nil
	}
	ids := strings.Split(op.BaselinePrimarySessionID, ",")
	nonterminal, err := r.exactLaunchNonterminalCount(ctx, tx, ids)
	if err != nil {
		return false, err
	}
	if nonterminal == 0 {
		return true, nil
	}
	marker, err := r.exactLaunchActivityMarker(ctx, tx, ids)
	if err != nil {
		return false, err
	}
	if marker != op.BaselineActivityMarker {
		return false, nil
	}
	query, args, err := sqlx.In(`s.id IN (?)`, ids)
	if err != nil {
		return false, err
	}
	busy, err := r.exactLaunchSessionsHaveOpenWork(ctx, tx, query, args...)
	return !busy, err
}

func (r *Repository) exactLaunchNonterminalCount(ctx context.Context, tx *sqlx.Tx, ids []string) (int, error) {
	query, args, err := sqlx.In(`SELECT COUNT(*) FROM task_sessions WHERE id IN (?) AND state IN `+exactLaunchNonterminalSessionStates, ids)
	if err != nil {
		return 0, err
	}
	var n int
	err = tx.QueryRowContext(ctx, r.db.Rebind(query), args...).Scan(&n)
	return n, err
}

// applyExactLaunchPrimary changes only rows whose flag changes so concurrent
// snapshot-fenced session writers observe a version bump exactly when their
// primary state changed.
func (r *Repository) applyExactLaunchPrimary(ctx context.Context, tx *sqlx.Tx, op *models.ExactSessionLaunchOperation) error {
	now := time.Now().UTC()
	if _, err := tx.ExecContext(ctx, r.db.Rebind(
		`UPDATE task_sessions SET is_primary = ?, updated_at = ? WHERE task_id = ? AND is_primary = 1 AND id <> ?`),
		dialect.BoolToInt(false), now, op.TaskID, op.SessionID); err != nil {
		return err
	}
	_, err := tx.ExecContext(ctx, r.db.Rebind(
		`UPDATE task_sessions SET is_primary = ?, updated_at = ? WHERE id = ? AND COALESCE(is_primary, 0) <> 1`),
		dialect.BoolToInt(true), now, op.SessionID)
	return err
}
