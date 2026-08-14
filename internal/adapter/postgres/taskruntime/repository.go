// Package taskruntime 实现 Task Runtime 的 PostgreSQL Repository Port。
package taskruntime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	postgresruntime "github.com/zhaohaip/agentops-go/internal/adapter/postgres/runtime"
	"github.com/zhaohaip/agentops-go/internal/contracts"
	domain "github.com/zhaohaip/agentops-go/internal/taskruntime"
)

// Repositories 聚合共享同一个只读连接入口的 Task Runtime Repository 实现。
type Repositories struct {
	Tasks      *TaskRepository
	Runs       *RunRepository
	Executions *TaskExecutionRepository
	Receipts   *CommandReceiptRepository
	TaskLogs   *TaskLogRepository
	Clock      *DatabaseClock
	Recovery   *RecoveryRepository
}

// New 创建 Task Runtime PostgreSQL Repository 集合。
func New(reader *postgresruntime.ReadPool) *Repositories {
	return &Repositories{
		Tasks:      &TaskRepository{reader: reader},
		Runs:       &RunRepository{reader: reader},
		Executions: &TaskExecutionRepository{reader: reader},
		Receipts:   &CommandReceiptRepository{reader: reader},
		TaskLogs:   &TaskLogRepository{},
		Clock:      &DatabaseClock{},
		Recovery:   &RecoveryRepository{},
	}
}

// RecoveryRepository 实现 P2 Recover 的锁定事实和 GENERATE_PLAN 失败终态 Port。
type RecoveryRepository struct{}

// LockRecoveryFacts 按 Task、Run、当前 Execution 的依赖顺序取得行锁。
func (*RecoveryRepository) LockRecoveryFacts(ctx context.Context, token contracts.RuntimeWriteTx,
	taskID contracts.TaskID) (domain.TerminationFacts, error) {
	var result domain.TerminationFacts
	err := withWriteTx(token, func(tx pgx.Tx) error {
		task, err := scanTask(tx.QueryRow(ctx, taskSelectSQL+" WHERE task_id = $1 FOR UPDATE", taskID))
		if err != nil {
			return err
		}
		run, err := scanRun(tx.QueryRow(ctx, runSelectSQL+" WHERE task_id = $1 FOR UPDATE", taskID))
		if err != nil {
			return err
		}
		execution, err := scanTaskExecution(tx.QueryRow(ctx, taskExecutionSelectSQL+
			" WHERE task_id = $1 AND execution_version = $2 FOR UPDATE", taskID, task.CurrentExecutionVersion))
		if err != nil {
			return err
		}
		result = domain.TerminationFacts{Task: task, Run: run, Execution: execution}
		return nil
	})
	return result, err
}

// ApplyRecoveryFailure 原子关闭无 Step/Tool 的 P2 GENERATE_PLAN 恢复现场。
func (*RecoveryRepository) ApplyRecoveryFailure(ctx context.Context, token contracts.RuntimeWriteTx,
	request domain.ApplyRecoveryFailureRequest) (bool, error) {
	var updated bool
	err := withWriteTx(token, func(tx pgx.Tx) error {
		var terminationReason any
		if request.TerminationReason != nil {
			terminationReason = *request.TerminationReason
		}
		tag, err := tx.Exec(ctx, `
WITH guarded AS (
    SELECT t.task_id, r.run_id, e.task_execution_id
    FROM task AS t
    JOIN run AS r ON r.task_id=t.task_id AND r.run_id=t.current_run_id
    JOIN task_execution AS e ON e.task_id=t.task_id AND e.execution_version=t.current_execution_version
    WHERE t.task_id=$1 AND t.current_execution_version=$2
      AND t.status=$3 AND r.status=$4 AND e.status=$5
      AND t.status IN ('Pending', 'Running', 'WaitingApproval', 'INTERRUPTED')
      AND r.status IN ('Pending', 'Running', 'WaitingApproval')
      AND e.status IN ('QUEUED', 'RUNNING', 'WAITING_APPROVAL', 'INTERRUPTED')
    FOR UPDATE OF t, r, e
), task_updated AS (
    UPDATE task AS t SET status='Failed', error_code=$6, queued_at=NULL, ended_at=$8
    FROM guarded AS g WHERE t.task_id=g.task_id RETURNING t.task_id
), run_updated AS (
    UPDATE run AS r SET status='Failed', error_code=$6, ended_at=$8
    FROM guarded AS g WHERE r.run_id=g.run_id RETURNING r.run_id
)
UPDATE task_execution AS e
SET status='FAILED',
    error_code=CASE WHEN e.error_code='CONFIG_VERSION_MISMATCH' THEN e.error_code ELSE $6 END,
    termination_reason=$7, ended_at=COALESCE(e.ended_at, $8)
FROM guarded AS g, task_updated, run_updated
WHERE e.task_execution_id=g.task_execution_id`, request.TaskID, request.ExpectedExecutionVersion,
			request.ExpectedTaskStatus, request.ExpectedRunStatus, request.ExpectedExecutionStatus,
			request.ErrorCode, terminationReason, request.EndedAt)
		if err != nil {
			return err
		}
		updated = tag.RowsAffected() == 1
		return nil
	})
	return updated, err
}

var _ domain.RecoveryRepository = (*RecoveryRepository)(nil)

// TaskRepository 实现 Task 持久化 Port。
type TaskRepository struct {
	reader *postgresruntime.ReadPool
}

// Insert 在调用方事务内创建 Task。
func (*TaskRepository) Insert(ctx context.Context, token contracts.RuntimeWriteTx, task domain.Task) error {
	return withWriteTx(token, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `
INSERT INTO task (
    task_id, agent_id, created_by, input, status, current_run_id,
    current_execution_version, result_summary, error_code, deadline_at,
    queued_at, created_at, started_at, ended_at
) VALUES (
    $1, $2, $3, $4, $5, $6,
    $7, $8, $9, $10,
    $11, $12, $13, $14
)`,
			task.TaskID, task.AgentID, task.CreatedBy, task.Input, task.Status, task.CurrentRunID,
			task.CurrentExecutionVersion, task.ResultSummary, task.ErrorCode, task.DeadlineAt,
			task.QueuedAt, task.CreatedAt, task.StartedAt, task.EndedAt,
		)
		return err
	})
}

// Find 通过只读连接池加载 Task。
func (r *TaskRepository) Find(ctx context.Context, taskID contracts.TaskID) (domain.Task, error) {
	if r == nil || r.reader == nil {
		return domain.Task{}, errors.New("find Task: read pool is not initialized")
	}
	return scanTask(r.reader.QueryRow(ctx, taskSelectSQL+" WHERE task_id = $1", taskID))
}

// List 按可选状态过滤，并按创建时间倒序、Task ID 升序返回 Task。
func (r *TaskRepository) List(ctx context.Context, status *contracts.TaskStatus) ([]domain.Task, error) {
	if r == nil || r.reader == nil {
		return nil, errors.New("list Tasks: read pool is not initialized")
	}
	query := taskSelectSQL + " ORDER BY created_at DESC, task_id ASC"
	var args []any
	if status != nil {
		query = taskSelectSQL + " WHERE status = $1 ORDER BY created_at DESC, task_id ASC"
		args = append(args, *status)
	}
	rows, err := r.reader.Query(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := make([]domain.Task, 0)
	for rows.Next() {
		task, err := scanTask(rows)
		if err != nil {
			return nil, err
		}
		result = append(result, task)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return result, nil
}

// Lock 在调用方事务内锁定 Task。
func (*TaskRepository) Lock(ctx context.Context, token contracts.RuntimeWriteTx, taskID contracts.TaskID) (domain.Task, error) {
	var result domain.Task
	err := withWriteTx(token, func(tx pgx.Tx) error {
		var err error
		result, err = scanTask(tx.QueryRow(ctx, taskSelectSQL+" WHERE task_id = $1 FOR UPDATE", taskID))
		return err
	})
	return result, err
}

// Update 按状态与 current_execution_version Guard 条件更新 Task，并返回是否命中一行。
func (*TaskRepository) Update(ctx context.Context, token contracts.RuntimeWriteTx, update domain.TaskUpdate) (bool, error) {
	var updated bool
	err := withWriteTx(token, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `
UPDATE task
SET status = $4,
    current_execution_version = $5,
    result_summary = $6,
    error_code = $7,
    queued_at = $8,
    started_at = $9,
    ended_at = $10
WHERE task_id = $1
  AND status = $2
  AND current_execution_version = $3`,
			update.TaskID, update.ExpectedStatus, update.ExpectedCurrentExecutionVersion,
			update.Status, update.CurrentExecutionVersion, update.ResultSummary, update.ErrorCode,
			update.QueuedAt, update.StartedAt, update.EndedAt,
		)
		if err != nil {
			return err
		}
		updated = tag.RowsAffected() == 1
		return nil
	})
	return updated, err
}

// LockNextQueueCandidate 在调用方事务内严格锁定 FIFO 队首；不跳过已被其他事务锁定的队首。
func (*TaskRepository) LockNextQueueCandidate(
	ctx context.Context,
	token contracts.RuntimeWriteTx,
) (domain.QueueCandidate, error) {
	var candidate domain.QueueCandidate
	err := withWriteTx(token, func(tx pgx.Tx) error {
		var err error
		candidate, err = scanQueueCandidate(tx.QueryRow(ctx, `
SELECT t.task_id, t.current_run_id, t.current_execution_version,
       t.status, e.status, t.queued_at, t.created_at
FROM task AS t
JOIN task_execution AS e
  ON e.task_id = t.task_id
 AND e.execution_version = t.current_execution_version
WHERE t.queued_at IS NOT NULL
ORDER BY t.queued_at ASC, t.created_at ASC, t.task_id ASC
LIMIT 1
FOR UPDATE OF t`))
		return err
	})
	return candidate, err
}

// RunRepository 实现 Run 持久化 Port。
type RunRepository struct {
	reader *postgresruntime.ReadPool
}

// Insert 在调用方事务内创建 Run。
func (*RunRepository) Insert(ctx context.Context, token contracts.RuntimeWriteTx, run domain.Run) error {
	return withWriteTx(token, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `
INSERT INTO run (
    run_id, task_id, status, plan_id, current_step_id,
    context, error_code, started_at, ended_at
) VALUES ($1, $2, $3, $4, $5, $6::jsonb, $7, $8, $9)`,
			run.RunID, run.TaskID, run.Status, run.PlanID, run.CurrentStepID,
			jsonText(run.Context), run.ErrorCode, run.StartedAt, run.EndedAt,
		)
		return err
	})
}

// FindByTask 通过只读连接池加载唯一 Run。
func (r *RunRepository) FindByTask(ctx context.Context, taskID contracts.TaskID) (domain.Run, error) {
	if r == nil || r.reader == nil {
		return domain.Run{}, errors.New("find Run: read pool is not initialized")
	}
	return scanRun(r.reader.QueryRow(ctx, runSelectSQL+" WHERE task_id = $1", taskID))
}

// LockByTask 在调用方事务内锁定唯一 Run。
func (*RunRepository) LockByTask(ctx context.Context, token contracts.RuntimeWriteTx, taskID contracts.TaskID) (domain.Run, error) {
	var result domain.Run
	err := withWriteTx(token, func(tx pgx.Tx) error {
		var err error
		result, err = scanRun(tx.QueryRow(ctx, runSelectSQL+" WHERE task_id = $1 FOR UPDATE", taskID))
		return err
	})
	return result, err
}

// Update 按 Task 当前执行版本和预期状态条件更新 Run，并返回是否命中一行。
func (*RunRepository) Update(ctx context.Context, token contracts.RuntimeWriteTx, update domain.RunUpdate) (bool, error) {
	var updated bool
	err := withWriteTx(token, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `
WITH guarded_task AS (
    SELECT task_id, current_run_id
    FROM task
    WHERE task_id = $2
      AND current_run_id = $1
      AND current_execution_version = $3
    FOR UPDATE
)
UPDATE run AS r
SET status = $5,
    plan_id = $6,
    current_step_id = $7,
    context = $8::jsonb,
    error_code = $9,
    started_at = $10,
    ended_at = $11
FROM guarded_task AS t
WHERE r.run_id = $1
  AND r.task_id = $2
  AND r.status = $4
  AND t.task_id = r.task_id
  AND t.current_run_id = r.run_id`,
			update.RunID, update.TaskID, update.ExecutionVersion, update.ExpectedStatus,
			update.Status, update.PlanID, update.CurrentStepID,
			jsonText(update.Context), update.ErrorCode, update.StartedAt, update.EndedAt,
		)
		if err != nil {
			return err
		}
		updated = tag.RowsAffected() == 1
		return nil
	})
	return updated, err
}

// TaskExecutionRepository 实现 TaskExecution 持久化 Port。
type TaskExecutionRepository struct {
	reader *postgresruntime.ReadPool
}

// Insert 在调用方事务内创建 TaskExecution。
func (*TaskExecutionRepository) Insert(ctx context.Context, token contracts.RuntimeWriteTx, execution domain.TaskExecution) error {
	return withWriteTx(token, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `
INSERT INTO task_execution (
    task_execution_id, task_id, execution_version, worker_id, status,
    execution_config_hash, observed_config_hash, error_code, invariant_code,
    termination_reason, created_at, started_at, ended_at
) VALUES (
    $1, $2, $3, $4, $5,
    $6, $7, $8, $9,
    $10, $11, $12, $13
)`,
			execution.TaskExecutionID, execution.TaskID, execution.ExecutionVersion,
			execution.WorkerID, execution.Status, execution.ExecutionConfigHash,
			execution.ObservedConfigHash, execution.ErrorCode, execution.InvariantCode,
			execution.TerminationReason, execution.CreatedAt, execution.StartedAt, execution.EndedAt,
		)
		return err
	})
}

// FindByTaskVersion 通过只读连接池加载明确版本，不推断最大版本。
func (r *TaskExecutionRepository) FindByTaskVersion(
	ctx context.Context,
	taskID contracts.TaskID,
	version contracts.ExecutionVersion,
) (domain.TaskExecution, error) {
	if r == nil || r.reader == nil {
		return domain.TaskExecution{}, errors.New("find TaskExecution: read pool is not initialized")
	}
	return scanTaskExecution(r.reader.QueryRow(
		ctx,
		taskExecutionSelectSQL+" WHERE task_id = $1 AND execution_version = $2",
		taskID,
		version,
	))
}

// LockByTaskVersion 在调用方事务内锁定明确版本。
func (*TaskExecutionRepository) LockByTaskVersion(
	ctx context.Context,
	token contracts.RuntimeWriteTx,
	taskID contracts.TaskID,
	version contracts.ExecutionVersion,
) (domain.TaskExecution, error) {
	var result domain.TaskExecution
	err := withWriteTx(token, func(tx pgx.Tx) error {
		var err error
		result, err = scanTaskExecution(tx.QueryRow(
			ctx,
			taskExecutionSelectSQL+" WHERE task_id = $1 AND execution_version = $2 FOR UPDATE",
			taskID,
			version,
		))
		return err
	})
	return result, err
}

// Update 按当前版本、状态和 worker_id 条件更新；observed_config_hash 仅允许首次配置失配写入，nil 保留原值。
func (*TaskExecutionRepository) Update(
	ctx context.Context,
	token contracts.RuntimeWriteTx,
	update domain.TaskExecutionUpdate,
) (bool, error) {
	var updated bool
	err := withWriteTx(token, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `
WITH guarded_task AS (
    SELECT task_id
    FROM task
    WHERE task_id = $1
      AND current_execution_version = $2
    FOR UPDATE
)
UPDATE task_execution AS e
SET status = $5,
    worker_id = $6,
    observed_config_hash = COALESCE(e.observed_config_hash, $7),
    error_code = $8,
    invariant_code = $9,
    termination_reason = $10,
    started_at = $11,
    ended_at = $12
FROM guarded_task AS t
WHERE e.task_id = $1
  AND e.execution_version = $2
  AND e.status = $3
  AND e.worker_id IS NOT DISTINCT FROM $4
  AND t.task_id = e.task_id
  AND (
      $7::text IS NULL
      OR (
          e.observed_config_hash IS NULL
          AND $5 = 'INTERRUPTED'
          AND $8 = 'CONFIG_VERSION_MISMATCH'
      )
  )`,
			update.TaskID, update.ExecutionVersion, update.ExpectedStatus, update.ExpectedWorkerID,
			update.Status, update.WorkerID, update.ObservedConfigHash, update.ErrorCode,
			update.InvariantCode, update.TerminationReason, update.StartedAt, update.EndedAt,
		)
		if err != nil {
			return err
		}
		updated = tag.RowsAffected() == 1
		return nil
	})
	return updated, err
}

// CommandReceiptRepository 实现不可变 Receipt 持久化 Port。
type CommandReceiptRepository struct {
	reader *postgresruntime.ReadPool
}

// Insert 在调用方事务内创建 Receipt；唯一冲突保留 PostgreSQL 原始错误链。
func (*CommandReceiptRepository) Insert(ctx context.Context, token contracts.RuntimeWriteTx, receipt domain.CommandReceipt) error {
	return withWriteTx(token, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `
INSERT INTO command_receipt (
    command_id, command_type, target_id, request_fingerprint, response, created_at
) VALUES ($1, $2, $3, $4, $5::jsonb, $6)`,
			receipt.CommandID, receipt.CommandType, receipt.TargetID,
			receipt.RequestFingerprint, jsonText(receipt.Response), receipt.CreatedAt,
		)
		return err
	})
}

// Find 通过只读连接池加载 Receipt。
func (r *CommandReceiptRepository) Find(ctx context.Context, commandID domain.CommandID) (domain.CommandReceipt, error) {
	if r == nil || r.reader == nil {
		return domain.CommandReceipt{}, errors.New("find Command Receipt: read pool is not initialized")
	}
	return scanCommandReceipt(r.reader.QueryRow(ctx, commandReceiptSelectSQL+" WHERE command_id = $1", commandID))
}

// Lock 在调用方事务内锁定 Receipt。
func (*CommandReceiptRepository) Lock(
	ctx context.Context,
	token contracts.RuntimeWriteTx,
	commandID domain.CommandID,
) (domain.CommandReceipt, error) {
	var result domain.CommandReceipt
	err := withWriteTx(token, func(tx pgx.Tx) error {
		var err error
		result, err = scanCommandReceipt(tx.QueryRow(
			ctx,
			commandReceiptSelectSQL+" WHERE command_id = $1 FOR UPDATE",
			commandID,
		))
		return err
	})
	return result, err
}

// TaskLogRepository 实现 append-only TaskLog Port。
type TaskLogRepository struct{}

// Append 在调用方提供的事务内追加一条日志，不读取或更新历史日志。
func (*TaskLogRepository) Append(ctx context.Context, token contracts.RuntimeWriteTx, log domain.TaskLog) error {
	return withWriteTx(token, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `
INSERT INTO task_log (
    log_id, task_id, run_id, step_id, execution_version,
    level, event, message, operator, created_at
) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)`,
			log.LogID, log.TaskID, log.RunID, log.StepID, log.ExecutionVersion,
			log.Level, log.Event, log.Message, log.Operator, log.CreatedAt,
		)
		return err
	})
}

// DatabaseClock 实现事务内 PostgreSQL 权威时间 Port。
type DatabaseClock struct{}

// Now 返回调用方事务内的 PostgreSQL clock_timestamp UTC 值。
func (*DatabaseClock) Now(ctx context.Context, token contracts.RuntimeWriteTx) (time.Time, error) {
	var now time.Time
	err := withWriteTx(token, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, "SELECT clock_timestamp()").Scan(&now)
	})
	return now.UTC(), err
}

const taskSelectSQL = `
SELECT task_id, agent_id, created_by, input, status, current_run_id,
       current_execution_version, result_summary, error_code, deadline_at,
       queued_at, created_at, started_at, ended_at
FROM task`

const runSelectSQL = `
SELECT run_id, task_id, status, plan_id, current_step_id,
       context, error_code, started_at, ended_at
FROM run`

const taskExecutionSelectSQL = `
SELECT task_execution_id, task_id, execution_version, worker_id, status,
       execution_config_hash, observed_config_hash, error_code, invariant_code,
       termination_reason, created_at, started_at, ended_at
FROM task_execution`

const commandReceiptSelectSQL = `
SELECT command_id, command_type, target_id, request_fingerprint, response, created_at
FROM command_receipt`

type rowScanner interface {
	Scan(...any) error
}

func scanTask(row rowScanner) (domain.Task, error) {
	var (
		result        domain.Task
		version       int64
		resultSummary *string
		errorCode     *string
	)
	err := row.Scan(
		&result.TaskID, &result.AgentID, &result.CreatedBy, &result.Input, &result.Status,
		&result.CurrentRunID, &version, &resultSummary, &errorCode, &result.DeadlineAt,
		&result.QueuedAt, &result.CreatedAt, &result.StartedAt, &result.EndedAt,
	)
	if err != nil {
		return domain.Task{}, mapNotFound(err)
	}
	parsed, err := executionVersion(version)
	if err != nil {
		return domain.Task{}, err
	}
	result.CurrentExecutionVersion = parsed
	result.ResultSummary = resultSummary
	result.ErrorCode = errorCodeValue(errorCode)
	result.DeadlineAt = result.DeadlineAt.UTC()
	result.CreatedAt = result.CreatedAt.UTC()
	result.QueuedAt = utcTime(result.QueuedAt)
	result.StartedAt = utcTime(result.StartedAt)
	result.EndedAt = utcTime(result.EndedAt)
	return result, nil
}

func scanQueueCandidate(row rowScanner) (domain.QueueCandidate, error) {
	var (
		candidate domain.QueueCandidate
		version   int64
	)
	if err := row.Scan(
		&candidate.TaskID,
		&candidate.RunID,
		&version,
		&candidate.TaskStatus,
		&candidate.ExecutionStatus,
		&candidate.QueuedAt,
		&candidate.CreatedAt,
	); err != nil {
		return domain.QueueCandidate{}, mapNotFound(err)
	}
	parsed, err := executionVersion(version)
	if err != nil {
		return domain.QueueCandidate{}, err
	}
	candidate.ExecutionVersion = parsed
	candidate.QueuedAt = candidate.QueuedAt.UTC()
	candidate.CreatedAt = candidate.CreatedAt.UTC()
	return candidate, nil
}

func scanRun(row rowScanner) (domain.Run, error) {
	var (
		result        domain.Run
		planID        *string
		currentStepID *string
		contextBytes  []byte
		errorCode     *string
	)
	if err := row.Scan(
		&result.RunID, &result.TaskID, &result.Status, &planID, &currentStepID,
		&contextBytes, &errorCode, &result.StartedAt, &result.EndedAt,
	); err != nil {
		return domain.Run{}, mapNotFound(err)
	}
	result.PlanID = planIDValue(planID)
	result.CurrentStepID = stepIDValue(currentStepID)
	result.Context = append(json.RawMessage(nil), contextBytes...)
	result.ErrorCode = errorCodeValue(errorCode)
	result.StartedAt = utcTime(result.StartedAt)
	result.EndedAt = utcTime(result.EndedAt)
	return result, nil
}

func scanTaskExecution(row rowScanner) (domain.TaskExecution, error) {
	var (
		result             domain.TaskExecution
		version            int64
		workerID           *string
		observedConfigHash *string
		errorCode          *string
		invariantCode      *string
		terminationReason  *string
	)
	if err := row.Scan(
		&result.TaskExecutionID, &result.TaskID, &version, &workerID, &result.Status,
		&result.ExecutionConfigHash, &observedConfigHash, &errorCode, &invariantCode,
		&terminationReason, &result.CreatedAt, &result.StartedAt, &result.EndedAt,
	); err != nil {
		return domain.TaskExecution{}, mapNotFound(err)
	}
	parsed, err := executionVersion(version)
	if err != nil {
		return domain.TaskExecution{}, err
	}
	result.ExecutionVersion = parsed
	result.WorkerID = workerIDValue(workerID)
	result.ObservedConfigHash = configHashValue(observedConfigHash)
	result.ErrorCode = errorCodeValue(errorCode)
	result.InvariantCode = invariantCodeValue(invariantCode)
	result.TerminationReason = terminationReasonValue(terminationReason)
	result.CreatedAt = result.CreatedAt.UTC()
	result.StartedAt = utcTime(result.StartedAt)
	result.EndedAt = utcTime(result.EndedAt)
	return result, nil
}

func scanCommandReceipt(row rowScanner) (domain.CommandReceipt, error) {
	var (
		result        domain.CommandReceipt
		responseBytes []byte
	)
	if err := row.Scan(
		&result.CommandID, &result.CommandType, &result.TargetID,
		&result.RequestFingerprint, &responseBytes, &result.CreatedAt,
	); err != nil {
		return domain.CommandReceipt{}, mapNotFound(err)
	}
	result.Response = append(json.RawMessage(nil), responseBytes...)
	result.CreatedAt = result.CreatedAt.UTC()
	return result, nil
}

func withWriteTx(token contracts.RuntimeWriteTx, work func(pgx.Tx) error) error {
	return postgresruntime.WithPostgreSQLWriteTx(token, work)
}

func mapNotFound(err error) error {
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.ErrRepositoryNotFound
	}
	return err
}

func executionVersion(value int64) (contracts.ExecutionVersion, error) {
	version := contracts.ExecutionVersion(value)
	if !version.Valid() {
		return 0, fmt.Errorf("scan Task Runtime execution version %d: %w", value, domain.ErrPersistenceInvariantViolation)
	}
	return version, nil
}

func jsonText(value []byte) string {
	return string(value)
}

func utcTime(value *time.Time) *time.Time {
	if value == nil {
		return nil
	}
	converted := value.UTC()
	return &converted
}

func errorCodeValue(value *string) *contracts.ErrorCode {
	if value == nil {
		return nil
	}
	converted := contracts.ErrorCode(*value)
	return &converted
}

func invariantCodeValue(value *string) *contracts.InvariantCode {
	if value == nil {
		return nil
	}
	converted := contracts.InvariantCode(*value)
	return &converted
}

func terminationReasonValue(value *string) *contracts.TerminationReason {
	if value == nil {
		return nil
	}
	converted := contracts.TerminationReason(*value)
	return &converted
}

func workerIDValue(value *string) *contracts.WorkerID {
	if value == nil {
		return nil
	}
	converted := contracts.WorkerID(*value)
	return &converted
}

func configHashValue(value *string) *contracts.ExecutionConfigHash {
	if value == nil {
		return nil
	}
	converted := contracts.ExecutionConfigHash(*value)
	return &converted
}

func planIDValue(value *string) *contracts.PlanID {
	if value == nil {
		return nil
	}
	converted := contracts.PlanID(*value)
	return &converted
}

func stepIDValue(value *string) *contracts.StepID {
	if value == nil {
		return nil
	}
	converted := contracts.StepID(*value)
	return &converted
}

var (
	_ domain.TaskRepository           = (*TaskRepository)(nil)
	_ domain.RunRepository            = (*RunRepository)(nil)
	_ domain.TaskExecutionRepository  = (*TaskExecutionRepository)(nil)
	_ domain.CommandReceiptRepository = (*CommandReceiptRepository)(nil)
	_ domain.TaskLogRepository        = (*TaskLogRepository)(nil)
	_ domain.DatabaseClock            = (*DatabaseClock)(nil)
)
