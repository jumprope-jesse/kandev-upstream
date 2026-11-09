package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/jmoiron/sqlx"

	"github.com/kandev/kandev/internal/agentctl/types/streams"
	"github.com/kandev/kandev/internal/db/dialect"
	"github.com/kandev/kandev/internal/task/models"
	"github.com/kandev/kandev/internal/task/repository/repoerrors"
)

const exactLaunchColumns = `operation_id, principal_scope, workspace_id, idempotency_key, request_digest,
	task_id, session_id, frozen_input, baseline_primary_session_id, baseline_asserted,
	baseline_activity_marker, make_primary, phase, primary_status, primary_reason,
	prompt_status, error_code, error_message, owner_token, owner_epoch, lease_expires_at,
	created_at, updated_at, completed_at`

const exactLaunchOpenPhases = `('completed', 'failed', 'uncertain')`

const exactLaunchNonterminalSessionStates = `('CREATED', 'STARTING', 'RUNNING', 'IDLE', 'WAITING_FOR_INPUT')`

type exactLaunchQuerier interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
	ExecContext(context.Context, string, ...any) (sql.Result, error)
}

type exactLaunchScanner interface {
	Scan(dest ...any) error
}

func scanExactLaunch(row exactLaunchScanner) (*models.ExactSessionLaunchOperation, error) {
	var (
		op                      models.ExactSessionLaunchOperation
		frozen                  string
		baselineAsserted, makes int
		lease, completed        sql.NullTime
		phase, pStatus, pReason string
		promptStatus            string
	)
	err := row.Scan(&op.OperationID, &op.PrincipalScope, &op.WorkspaceID, &op.IdempotencyKey,
		&op.RequestDigest, &op.TaskID, &op.SessionID, &frozen, &op.BaselinePrimarySessionID,
		&baselineAsserted, &op.BaselineActivityMarker, &makes, &phase, &pStatus, &pReason,
		&promptStatus, &op.ErrorCode, &op.ErrorMessage, &op.OwnerToken, &op.OwnerEpoch, &lease,
		&op.CreatedAt, &op.UpdatedAt, &completed)
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal([]byte(frozen), &op.FrozenInput); err != nil {
		return nil, fmt.Errorf("decode frozen exact launch input: %w", err)
	}
	op.BaselineAsserted, op.MakePrimary = baselineAsserted == 1, makes == 1
	op.Phase = models.ExactSessionLaunchPhase(phase)
	op.PrimaryStatus = models.ExactSessionLaunchPrimaryStatus(pStatus)
	op.PrimaryReason = models.ExactSessionLaunchPrimaryReason(pReason)
	op.PromptStatus = models.ExactSessionLaunchPromptStatus(promptStatus)
	if lease.Valid {
		t := lease.Time.UTC()
		op.LeaseExpiresAt = &t
	}
	if completed.Valid {
		t := completed.Time.UTC()
		op.CompletedAt = &t
	}
	return &op, nil
}

func (r *Repository) getExactLaunch(ctx context.Context, q exactLaunchQuerier, where string, args ...any) (*models.ExactSessionLaunchOperation, error) {
	row := q.QueryRowContext(ctx, r.db.Rebind(`SELECT `+exactLaunchColumns+` FROM exact_session_launch_operations WHERE `+where), args...)
	op, err := scanExactLaunch(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, repoerrors.ErrExactLaunchNotFound
	}
	return op, err
}

// GetExactSessionLaunch returns one operation by ID.
func (r *Repository) GetExactSessionLaunch(ctx context.Context, operationID string) (*models.ExactSessionLaunchOperation, error) {
	return r.getExactLaunch(ctx, r.ro, `operation_id = ?`, operationID)
}

// GetExactSessionLaunchByKey returns one operation by its idempotency scope.
func (r *Repository) GetExactSessionLaunchByKey(ctx context.Context, principalScope, workspaceID, idempotencyKey string) (*models.ExactSessionLaunchOperation, error) {
	return r.getExactLaunch(ctx, r.ro, `principal_scope = ? AND workspace_id = ? AND idempotency_key = ?`,
		principalScope, workspaceID, idempotencyKey)
}

// ListOpenExactSessionLaunches returns unfinished operations, oldest first.
func (r *Repository) ListOpenExactSessionLaunches(ctx context.Context, limit int) ([]*models.ExactSessionLaunchOperation, error) {
	if limit <= 0 {
		limit = 100
	}
	rows, err := r.ro.QueryContext(ctx, r.ro.Rebind(`SELECT `+exactLaunchColumns+
		` FROM exact_session_launch_operations WHERE phase NOT IN `+exactLaunchOpenPhases+
		` ORDER BY created_at, operation_id LIMIT ?`), limit)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []*models.ExactSessionLaunchOperation
	for rows.Next() {
		op, err := scanExactLaunch(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, op)
	}
	return out, rows.Err()
}

// AdmitExactSessionLaunch validates the card and persists the operation with
// its frozen input in one transaction, before any session exists.
func (r *Repository) AdmitExactSessionLaunch(
	ctx context.Context,
	adm models.ExactSessionLaunchAdmission,
) (*models.ExactSessionLaunchOperation, bool, error) {
	op := adm.Operation
	if err := validateExactLaunchAdmission(op); err != nil {
		return nil, false, err
	}
	tx, err := r.db.BeginTxx(ctx, nil)
	if err != nil {
		return nil, false, err
	}
	defer func() { _ = tx.Rollback() }()
	if err := r.managedDeletionBarrierTx(ctx, tx, op.TaskID); err != nil {
		return nil, false, err
	}
	existing, err := r.getExactLaunch(ctx, tx, `principal_scope = ? AND workspace_id = ? AND idempotency_key = ?`,
		op.PrincipalScope, op.WorkspaceID, op.IdempotencyKey)
	if err == nil {
		return r.finishExactLaunchReplay(tx, existing, op.RequestDigest)
	}
	if !errors.Is(err, repoerrors.ErrExactLaunchNotFound) {
		return nil, false, err
	}
	if err := r.checkExactLaunchAdmissible(ctx, tx, adm); err != nil {
		return nil, false, err
	}
	if err := r.captureExactLaunchBaseline(ctx, tx, &op); err != nil {
		return nil, false, err
	}
	stored, replayed, err := r.insertExactLaunch(ctx, tx, op)
	if err != nil {
		return nil, false, err
	}
	if replayed {
		return r.finishExactLaunchReplay(tx, stored, op.RequestDigest)
	}
	if err := tx.Commit(); err != nil {
		return nil, false, err
	}
	return stored, false, nil
}

func validateExactLaunchAdmission(op models.ExactSessionLaunchOperation) error {
	for _, field := range []string{
		op.OperationID, op.PrincipalScope, op.WorkspaceID, op.TaskID,
		op.IdempotencyKey, op.RequestDigest, op.SessionID,
	} {
		if field == "" {
			return errors.New("exact session launch identity is incomplete")
		}
	}
	return op.FrozenInput.Validate()
}

func (r *Repository) finishExactLaunchReplay(tx *sqlx.Tx, existing *models.ExactSessionLaunchOperation, digest string) (*models.ExactSessionLaunchOperation, bool, error) {
	if existing.RequestDigest != digest {
		return nil, false, repoerrors.ErrTaskOperationConflict
	}
	if err := tx.Commit(); err != nil {
		return nil, false, err
	}
	return existing, true, nil
}

func (r *Repository) insertExactLaunch(ctx context.Context, tx *sqlx.Tx, op models.ExactSessionLaunchOperation) (*models.ExactSessionLaunchOperation, bool, error) {
	frozen, err := json.Marshal(op.FrozenInput)
	if err != nil {
		return nil, false, err
	}
	now := time.Now().UTC()
	primaryStatus := models.ExactLaunchPrimaryNotRequested
	if op.MakePrimary {
		primaryStatus = models.ExactLaunchPrimaryPending
	}
	res, err := tx.ExecContext(ctx, r.db.Rebind(`
		INSERT INTO exact_session_launch_operations (
			operation_id, principal_scope, workspace_id, idempotency_key, request_digest,
			task_id, session_id, frozen_input, baseline_primary_session_id, baseline_asserted,
			baseline_activity_marker, make_primary, phase, primary_status, primary_reason,
			prompt_status, error_code, error_message, owner_token, owner_epoch, lease_expires_at,
			created_at, updated_at, completed_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, '', ?, '', '', '', 0, NULL, ?, ?, NULL)
		ON CONFLICT (principal_scope, workspace_id, idempotency_key) DO NOTHING`),
		op.OperationID, op.PrincipalScope, op.WorkspaceID, op.IdempotencyKey, op.RequestDigest,
		op.TaskID, op.SessionID, string(frozen), op.BaselinePrimarySessionID, dialect.BoolToInt(op.BaselineAsserted),
		op.BaselineActivityMarker, dialect.BoolToInt(op.MakePrimary), models.ExactLaunchPhaseAdmitted, primaryStatus,
		models.ExactLaunchPromptPending, now, now)
	if err != nil {
		return nil, false, err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		existing, err := r.getExactLaunch(ctx, tx, `principal_scope = ? AND workspace_id = ? AND idempotency_key = ?`,
			op.PrincipalScope, op.WorkspaceID, op.IdempotencyKey)
		return existing, true, err
	}
	stored, err := r.getExactLaunch(ctx, tx, `operation_id = ?`, op.OperationID)
	return stored, false, err
}

func (r *Repository) checkExactLaunchAdmissible(ctx context.Context, tx *sqlx.Tx, adm models.ExactSessionLaunchAdmission) error {
	op := adm.Operation
	var workspaceID string
	var version time.Time
	var archivedAt sql.NullTime
	query := `SELECT workspace_id, updated_at, archived_at FROM tasks WHERE id = ?`
	if dialect.IsPostgres(r.db.DriverName()) {
		query += forUpdateClause
	}
	err := tx.QueryRowContext(ctx, r.db.Rebind(query), op.TaskID).Scan(&workspaceID, &version, &archivedAt)
	if errors.Is(err, sql.ErrNoRows) || (err == nil && workspaceID != op.WorkspaceID) {
		return fmt.Errorf("%w: %s", repoerrors.ErrTaskNotFound, op.TaskID)
	}
	if err != nil {
		return err
	}
	if archivedAt.Valid {
		return repoerrors.ErrExactLaunchTaskArchived
	}
	if adm.ExpectedResourceVersion != "" {
		expected, err := time.Parse(time.RFC3339Nano, adm.ExpectedResourceVersion)
		if err != nil {
			return fmt.Errorf("invalid expected task resource version: %w", err)
		}
		if !version.Equal(expected) {
			return repoerrors.ErrTaskVersionConflict
		}
	}
	if adm.ClaimFence != nil {
		if err := r.checkTaskManagementClaimFence(ctx, tx, op.TaskID, *adm.ClaimFence); err != nil {
			return err
		}
	}
	return r.checkExactLaunchQuiescent(ctx, tx, op.TaskID)
}

// checkExactLaunchQuiescent refuses a card with any session that may be doing
// work. An idle session with no open turn, background work, or background run
// does not block; an unreadable signal is treated as busy by the caller.
func (r *Repository) checkExactLaunchQuiescent(ctx context.Context, tx exactLaunchQuerier, taskID string) error {
	busy, err := r.exactLaunchTaskBusy(ctx, tx, taskID)
	if err != nil {
		return err
	}
	if busy {
		return repoerrors.ErrExactLaunchBusy
	}
	var open bool
	if err := tx.QueryRowContext(ctx, r.db.Rebind(`SELECT EXISTS (SELECT 1 FROM exact_session_launch_operations
		WHERE task_id = ? AND phase NOT IN `+exactLaunchOpenPhases+`)`), taskID).Scan(&open); err != nil {
		return err
	}
	if open {
		return repoerrors.ErrExactLaunchBusy
	}
	return nil
}

func (r *Repository) exactLaunchTaskBusy(ctx context.Context, tx exactLaunchQuerier, taskID string) (bool, error) {
	var foreground bool
	if err := tx.QueryRowContext(ctx, r.db.Rebind(`SELECT EXISTS (SELECT 1 FROM task_sessions
		WHERE task_id = ? AND state IN ('CREATED', 'STARTING', 'RUNNING'))`), taskID).Scan(&foreground); err != nil || foreground {
		return foreground, err
	}
	return r.exactLaunchSessionsHaveOpenWork(ctx, tx, `s.task_id = ?`, taskID)
}

// exactLaunchSessionsHaveOpenWork reports an open turn, background workload, or
// background run on a nonterminal session selected by the session predicate.
func (r *Repository) exactLaunchSessionsHaveOpenWork(ctx context.Context, tx exactLaunchQuerier, predicate string, args ...any) (bool, error) {
	var openTurn bool
	if err := tx.QueryRowContext(ctx, r.db.Rebind(`SELECT EXISTS (SELECT 1 FROM task_session_turns t
		WHERE t.completed_at IS NULL AND t.task_session_id IN (SELECT s.id FROM task_sessions s WHERE `+predicate+
		` AND s.state IN `+exactLaunchNonterminalSessionStates+`))`), args...).Scan(&openTurn); err != nil || openTurn {
		return openTurn, err
	}
	for _, table := range []string{"task_session_background_work", "task_session_background_runs"} {
		open, err := r.exactLaunchOpenWorkIn(ctx, tx, table, predicate, args)
		if err != nil || open {
			return open, err
		}
	}
	return false, nil
}

func (r *Repository) exactLaunchOpenWorkIn(ctx context.Context, tx exactLaunchQuerier, table, predicate string, args []any) (bool, error) {
	var query string
	switch table {
	case "task_session_background_work":
		query = `SELECT EXISTS (SELECT 1 FROM task_session_background_work w WHERE w.state NOT IN (?) AND w.session_id IN (SELECT s.id FROM task_sessions s WHERE ` +
			predicate + ` AND s.state IN ` + exactLaunchNonterminalSessionStates + `))`
	default:
		query = `SELECT EXISTS (SELECT 1 FROM task_session_background_runs w WHERE w.state NOT IN (?) AND w.session_id IN (SELECT s.id FROM task_sessions s WHERE ` +
			predicate + ` AND s.state IN ` + exactLaunchNonterminalSessionStates + `))`
	}
	expanded, expandedArgs, err := sqlx.In(query, append([]any{streams.TerminalRunStates}, args...)...)
	if err != nil {
		return false, err
	}
	var open bool
	err = tx.QueryRowContext(ctx, r.db.Rebind(expanded), expandedArgs...).Scan(&open)
	return open, err
}

func (r *Repository) exactLaunchPrimaryIDs(ctx context.Context, tx exactLaunchQuerier, taskID string) ([]string, error) {
	rows, err := tx.QueryContext(ctx, r.db.Rebind(`SELECT id FROM task_sessions WHERE task_id = ? AND is_primary = 1 ORDER BY id`), taskID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// exactLaunchActivityMarker fingerprints each session's lifecycle state and
// completed-or-open turn count so a turn begun after admission changes it.
func (r *Repository) exactLaunchActivityMarker(ctx context.Context, tx exactLaunchQuerier, ids []string) (string, error) {
	parts := make([]string, 0, len(ids))
	for _, id := range ids {
		var state string
		err := tx.QueryRowContext(ctx, r.db.Rebind(`SELECT state FROM task_sessions WHERE id = ?`), id).Scan(&state)
		if errors.Is(err, sql.ErrNoRows) {
			parts = append(parts, id+":gone:0")
			continue
		}
		if err != nil {
			return "", err
		}
		var turns int
		if err := tx.QueryRowContext(ctx, r.db.Rebind(`SELECT COUNT(*) FROM task_session_turns WHERE task_session_id = ?`), id).Scan(&turns); err != nil {
			return "", err
		}
		parts = append(parts, fmt.Sprintf("%s:%s:%d", id, state, turns))
	}
	sort.Strings(parts)
	return strings.Join(parts, ","), nil
}

func (r *Repository) captureExactLaunchBaseline(ctx context.Context, tx *sqlx.Tx, op *models.ExactSessionLaunchOperation) error {
	ids, err := r.exactLaunchPrimaryIDs(ctx, tx, op.TaskID)
	if err != nil {
		return err
	}
	actual := strings.Join(ids, ",")
	if op.BaselineAsserted && op.BaselinePrimarySessionID != actual {
		return repoerrors.ErrExactLaunchBaselineMismatch
	}
	marker, err := r.exactLaunchActivityMarker(ctx, tx, ids)
	if err != nil {
		return err
	}
	op.BaselinePrimarySessionID, op.BaselineActivityMarker = actual, marker
	return nil
}

// ClaimExactSessionLaunch takes ownership of an open operation whose owner is
// absent or whose lease expired.
func (r *Repository) ClaimExactSessionLaunch(
	ctx context.Context,
	operationID, ownerToken string,
	now time.Time,
	lease time.Duration,
) (*models.ExactSessionLaunchOperation, error) {
	if operationID == "" || ownerToken == "" || lease <= 0 {
		return nil, errors.New("exact session launch claim is incomplete")
	}
	now = now.UTC()
	tx, err := r.db.BeginTxx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()
	res, err := tx.ExecContext(ctx, r.db.Rebind(`
		UPDATE exact_session_launch_operations
		SET owner_token = ?, owner_epoch = owner_epoch + 1, lease_expires_at = ?, updated_at = ?
		WHERE operation_id = ? AND phase NOT IN `+exactLaunchOpenPhases+`
			AND (owner_token = '' OR lease_expires_at IS NULL OR lease_expires_at <= ?)`),
		ownerToken, now.Add(lease), now, operationID, now)
	if err != nil {
		return nil, err
	}
	claimed, err := res.RowsAffected()
	if err != nil {
		return nil, err
	}
	op, err := r.getExactLaunch(ctx, tx, `operation_id = ?`, operationID)
	if err != nil {
		return nil, err
	}
	if claimed == 0 {
		if op.Phase.IsTerminal() {
			return op, nil
		}
		return nil, repoerrors.ErrExactLaunchOwned
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return op, nil
}

// RenewExactSessionLaunch extends the lease of the current owner.
func (r *Repository) RenewExactSessionLaunch(ctx context.Context, fence models.ExactSessionLaunchFence, now time.Time, lease time.Duration) error {
	now = now.UTC()
	res, err := r.db.ExecContext(ctx, r.db.Rebind(`
		UPDATE exact_session_launch_operations SET lease_expires_at = ?, updated_at = ?
		WHERE operation_id = ? AND owner_token = ? AND owner_epoch = ? AND phase NOT IN `+exactLaunchOpenPhases),
		now.Add(lease), now, fence.OperationID, fence.OwnerToken, fence.OwnerEpoch)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return repoerrors.ErrExactLaunchFenced
	}
	return nil
}

// AdvanceExactSessionLaunch applies one fenced phase transition.
func (r *Repository) AdvanceExactSessionLaunch(
	ctx context.Context,
	fence models.ExactSessionLaunchFence,
	from models.ExactSessionLaunchPhase,
	update models.ExactSessionLaunchUpdate,
	now time.Time,
) (*models.ExactSessionLaunchOperation, error) {
	now = now.UTC()
	tx, err := r.db.BeginTxx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()
	res, err := r.execExactLaunchAdvance(ctx, tx, fence, from, update, now)
	if err != nil {
		return nil, err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return nil, repoerrors.ErrExactLaunchFenced
	}
	op, err := r.getExactLaunch(ctx, tx, `operation_id = ?`, fence.OperationID)
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return op, nil
}

const (
	exactLaunchAdvanceHead = `UPDATE exact_session_launch_operations SET
			phase = ?, primary_status = ?, primary_reason = ?, prompt_status = ?,
			error_code = ?, error_message = ?, updated_at = ?, `
	exactLaunchAdvanceTail = ` WHERE operation_id = ? AND owner_token = ? AND owner_epoch = ? AND phase = ?`
)

func (r *Repository) execExactLaunchAdvance(
	ctx context.Context,
	tx *sqlx.Tx,
	fence models.ExactSessionLaunchFence,
	from models.ExactSessionLaunchPhase,
	update models.ExactSessionLaunchUpdate,
	now time.Time,
) (sql.Result, error) {
	args := []any{update.Phase, update.PrimaryStatus, update.PrimaryReason, update.PromptStatus,
		update.ErrorCode, update.ErrorMessage, now}
	tail := []any{fence.OperationID, fence.OwnerToken, fence.OwnerEpoch, from}
	if update.Phase.IsTerminal() {
		return tx.ExecContext(ctx, r.db.Rebind(exactLaunchAdvanceHead+`completed_at = ?, owner_token = '', lease_expires_at = NULL`+exactLaunchAdvanceTail),
			append(append(args, now), tail...)...)
	}
	return tx.ExecContext(ctx, r.db.Rebind(exactLaunchAdvanceHead+`owner_token = owner_token`+exactLaunchAdvanceTail),
		append(args, tail...)...)
}

// ReleaseExactSessionLaunch clears ownership so another worker may claim.
func (r *Repository) ReleaseExactSessionLaunch(ctx context.Context, fence models.ExactSessionLaunchFence) error {
	_, err := r.db.ExecContext(ctx, r.db.Rebind(`
		UPDATE exact_session_launch_operations SET owner_token = '', lease_expires_at = NULL
		WHERE operation_id = ? AND owner_token = ? AND owner_epoch = ?`),
		fence.OperationID, fence.OwnerToken, fence.OwnerEpoch)
	return err
}
