// Package taskruntime 声明 Task Runtime 领域拥有的 PostgreSQL Migration。
package taskruntime

import "github.com/zhaohaip/agentops-go/internal/adapter/postgres/migration"

const sharedErrorCodeCheck = `
    'TaskCancelled',
    'TaskTimeout',
    'CONFIG_VERSION_MISMATCH',
    'DATA_INCONSISTENT',
    'CheckpointInvalid',
    'PlanGenerationFailed',
    'PlanValidationFailed',
    'InputResolutionFailed',
    'ModelCallFailed',
    'ModelOutputInvalid',
    'ResultSanitizationFailed',
    'StepOutputInvalid',
    'StepOutputTooLarge',
    'ToolNotFound',
    'ToolDisabled',
    'ToolNotAuthorized',
    'ToolInputInvalid',
    'ToolAccessDenied',
    'ToolTimeout',
    'ToolConnectionLost',
    'ToolCallFailed',
    'ApprovalContextChanged',
    'ApprovalRejected',
    'WRITE_TOOL_INTERRUPTED',
    'WORKER_INTERRUPTED',
    'RESULT_PERSISTENCE_FAILED',
    'ReportGenerationFailed',
    'STEP_EXECUTOR_CONTRACT_BROKEN',
    'RUNTIME_STATIC_TOOL_SNAPSHOT_INCONSISTENT',
    'PersistenceInvariantViolation'
`

// Migrations 返回 Task Runtime 已发布的完整 Migration 历史。
func Migrations() []migration.Migration {
	return []migration.Migration{createTaskRuntimeTables()}
}

func createTaskRuntimeTables() migration.Migration {
	return migration.Migration{
		Version: 1,
		Name:    "create_task_runtime_tables",
		Statements: []string{
			createTaskTableSQL,
			createRunTableSQL,
			createTaskExecutionTableSQL,
			addTaskCurrentRunForeignKeySQL,
			addTaskCurrentExecutionForeignKeySQL,
			createCommandReceiptTableSQL,
			createTaskLogTableSQL,
			createTaskQueueIndexSQL,
			createTaskExecutionStatusIndexSQL,
			createTaskLogTaskIndexSQL,
		},
	}
}

const createTaskTableSQL = `
CREATE TABLE task (
    task_id TEXT PRIMARY KEY CHECK (task_id <> ''),
    agent_id TEXT NOT NULL CHECK (agent_id <> ''),
    created_by TEXT NOT NULL CHECK (created_by <> ''),
    input TEXT NOT NULL,
    status TEXT NOT NULL CHECK (status IN (
        'Pending', 'Running', 'WaitingApproval', 'INTERRUPTED',
        'Completed', 'Failed', 'Cancelled'
    )),
    current_run_id TEXT NOT NULL CHECK (current_run_id <> ''),
    current_execution_version BIGINT NOT NULL CHECK (current_execution_version > 0),
    result_summary TEXT,
    error_code TEXT CHECK (error_code IS NULL OR error_code IN (` + sharedErrorCodeCheck + `)),
    deadline_at TIMESTAMPTZ NOT NULL,
    queued_at TIMESTAMPTZ,
    created_at TIMESTAMPTZ NOT NULL,
    started_at TIMESTAMPTZ,
    ended_at TIMESTAMPTZ,
    CONSTRAINT task_time_order_check CHECK (
        deadline_at >= created_at
        AND (started_at IS NULL OR started_at >= created_at)
        AND (ended_at IS NULL OR ended_at >= created_at)
        AND (started_at IS NULL OR ended_at IS NULL OR ended_at >= started_at)
    ),
    CONSTRAINT task_terminal_time_check CHECK (
        (status IN ('Completed', 'Failed', 'Cancelled')) = (ended_at IS NOT NULL)
    ),
    CONSTRAINT task_error_check CHECK (
        (status = 'Completed' AND error_code IS NULL)
        OR (status IN ('Failed', 'Cancelled', 'INTERRUPTED') AND error_code IS NOT NULL)
        OR (status IN ('Pending', 'Running', 'WaitingApproval') AND error_code IS NULL)
    )
)`

const createRunTableSQL = `
CREATE TABLE run (
    run_id TEXT PRIMARY KEY CHECK (run_id <> ''),
    task_id TEXT NOT NULL CHECK (task_id <> ''),
    status TEXT NOT NULL CHECK (status IN (
        'Pending', 'Running', 'WaitingApproval', 'Completed', 'Failed'
    )),
    plan_id TEXT,
    current_step_id TEXT,
    context JSONB NOT NULL DEFAULT '{}'::jsonb CHECK (jsonb_typeof(context) = 'object'),
    error_code TEXT CHECK (error_code IS NULL OR error_code IN (` + sharedErrorCodeCheck + `)),
    started_at TIMESTAMPTZ,
    ended_at TIMESTAMPTZ,
    CONSTRAINT run_task_unique UNIQUE (task_id),
    CONSTRAINT run_task_identity_unique UNIQUE (task_id, run_id),
    CONSTRAINT run_task_foreign_key FOREIGN KEY (task_id) REFERENCES task (task_id),
    CONSTRAINT run_terminal_time_check CHECK (
        (status IN ('Completed', 'Failed')) = (ended_at IS NOT NULL)
    ),
    CONSTRAINT run_error_check CHECK (
        (status = 'Completed' AND error_code IS NULL)
        OR (status = 'Failed' AND error_code IS NOT NULL)
        OR (status IN ('Pending', 'Running', 'WaitingApproval') AND error_code IS NULL)
    )
)`

const createTaskExecutionTableSQL = `
CREATE TABLE task_execution (
    task_execution_id TEXT PRIMARY KEY CHECK (task_execution_id <> ''),
    task_id TEXT NOT NULL CHECK (task_id <> ''),
    execution_version BIGINT NOT NULL CHECK (execution_version > 0),
    worker_id TEXT CHECK (worker_id IS NULL OR worker_id <> ''),
    status TEXT NOT NULL CHECK (status IN (
        'QUEUED', 'RUNNING', 'WAITING_APPROVAL', 'COMPLETED', 'FAILED', 'INTERRUPTED'
    )),
    execution_config_hash TEXT NOT NULL CHECK (execution_config_hash ~ '^[0-9a-f]{64}$'),
    observed_config_hash TEXT CHECK (observed_config_hash ~ '^[0-9a-f]{64}$'),
    error_code TEXT CHECK (error_code IS NULL OR error_code IN (` + sharedErrorCodeCheck + `)),
    invariant_code TEXT CHECK (invariant_code IS NULL OR invariant_code IN (
        'CURRENT_EXECUTION_INVALID',
        'QUEUE_STATE_INVALID',
        'CLAIM_SOURCE_AMBIGUOUS',
        'CROSS_OBJECT_STATE_INVALID'
    )),
    termination_reason TEXT CHECK (termination_reason IS NULL OR termination_reason IN ('CANCELLED', 'TIMED_OUT')),
    created_at TIMESTAMPTZ NOT NULL,
    started_at TIMESTAMPTZ,
    ended_at TIMESTAMPTZ,
    CONSTRAINT task_execution_task_version_unique UNIQUE (task_id, execution_version),
    CONSTRAINT task_execution_task_foreign_key FOREIGN KEY (task_id) REFERENCES task (task_id),
    CONSTRAINT task_execution_time_order_check CHECK (
        (started_at IS NULL OR started_at >= created_at)
        AND (ended_at IS NULL OR ended_at >= created_at)
        AND (started_at IS NULL OR ended_at IS NULL OR ended_at >= started_at)
    ),
    CONSTRAINT task_execution_status_fields_check CHECK (
        (status = 'QUEUED' AND worker_id IS NULL AND ended_at IS NULL)
        OR (status = 'RUNNING' AND worker_id IS NOT NULL AND started_at IS NOT NULL AND ended_at IS NULL)
        OR (status = 'WAITING_APPROVAL' AND worker_id IS NULL AND started_at IS NOT NULL AND ended_at IS NULL)
        OR (status IN ('COMPLETED', 'FAILED', 'INTERRUPTED') AND ended_at IS NOT NULL)
    ),
    CONSTRAINT task_execution_observed_config_check CHECK (
        observed_config_hash IS NULL
        OR (
            error_code IS NOT NULL
            AND error_code = 'CONFIG_VERSION_MISMATCH'
            AND status IN ('INTERRUPTED', 'FAILED')
        )
    ),
    CONSTRAINT task_execution_invariant_check CHECK (
        invariant_code IS NULL
        OR (error_code IS NOT NULL AND error_code = 'DATA_INCONSISTENT')
    ),
    CONSTRAINT task_execution_termination_check CHECK (
        termination_reason IS NULL OR status = 'FAILED'
    )
)`

const addTaskCurrentRunForeignKeySQL = `
ALTER TABLE task
ADD CONSTRAINT task_current_run_foreign_key
FOREIGN KEY (task_id, current_run_id)
REFERENCES run (task_id, run_id)
DEFERRABLE INITIALLY DEFERRED`

const addTaskCurrentExecutionForeignKeySQL = `
ALTER TABLE task
ADD CONSTRAINT task_current_execution_foreign_key
FOREIGN KEY (task_id, current_execution_version)
REFERENCES task_execution (task_id, execution_version)
DEFERRABLE INITIALLY DEFERRED`

const createCommandReceiptTableSQL = `
CREATE TABLE command_receipt (
    command_id TEXT PRIMARY KEY CHECK (command_id <> ''),
    command_type TEXT NOT NULL CHECK (command_type IN ('Create', 'Approve', 'Reject', 'Cancel', 'Recover')),
    target_id TEXT NOT NULL CHECK (target_id <> ''),
    request_fingerprint TEXT NOT NULL CHECK (request_fingerprint ~ '^[0-9a-f]{64}$'),
    response JSONB NOT NULL CHECK (jsonb_typeof(response) = 'object'),
    created_at TIMESTAMPTZ NOT NULL
)`

const createTaskLogTableSQL = `
CREATE TABLE task_log (
    log_id TEXT PRIMARY KEY CHECK (log_id <> ''),
    task_id TEXT NOT NULL CHECK (task_id <> ''),
    run_id TEXT NOT NULL CHECK (run_id <> ''),
    step_id TEXT,
    execution_version BIGINT CHECK (execution_version > 0),
    level TEXT NOT NULL CHECK (level IN ('Info', 'Error')),
    event TEXT NOT NULL CHECK (event <> ''),
    message TEXT NOT NULL,
    operator TEXT NOT NULL CHECK (operator <> ''),
    created_at TIMESTAMPTZ NOT NULL,
    CONSTRAINT task_log_run_foreign_key FOREIGN KEY (task_id, run_id) REFERENCES run (task_id, run_id),
    CONSTRAINT task_log_execution_foreign_key FOREIGN KEY (task_id, execution_version)
        REFERENCES task_execution (task_id, execution_version)
)`

const createTaskQueueIndexSQL = `
CREATE INDEX task_queue_fifo_index
ON task (queued_at, created_at, task_id)
WHERE queued_at IS NOT NULL`

const createTaskExecutionStatusIndexSQL = `
CREATE INDEX task_execution_status_worker_index
ON task_execution (status, worker_id, task_id)`

const createTaskLogTaskIndexSQL = `
CREATE INDEX task_log_task_created_index
ON task_log (task_id, created_at, log_id)`
