// Package stepexecutor 声明 Step Executor 模块拥有的 PostgreSQL Migration。
package stepexecutor

import "github.com/zhaohaip/agentops-go/internal/adapter/postgres/migration"

const stepErrorCodeCheck = `
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

// Migrations 返回 Step Executor 已发布的完整 Migration 历史。
func Migrations() []migration.Migration {
	return []migration.Migration{{
		Version: 4,
		Name:    "create_step_table",
		Statements: []string{
			createStepTableSQL,
			addRunCurrentStepForeignKeySQL,
			createStepPlanSequenceIndexSQL,
		},
	}}
}

const createStepTableSQL = `
CREATE TABLE step (
    step_id TEXT PRIMARY KEY CHECK (step_id <> ''),
    run_id TEXT NOT NULL CHECK (run_id <> ''),
    plan_id TEXT NOT NULL CHECK (plan_id <> ''),
    sequence BIGINT NOT NULL CHECK (sequence > 0 AND sequence <= 4294967295),
    type TEXT NOT NULL CHECK (type IN ('Analysis', 'ModelCall', 'ToolCall', 'Verification')),
    name TEXT NOT NULL CHECK (name <> ''),
    input JSONB NOT NULL CHECK (jsonb_typeof(input) = 'object'),
    output_schema JSONB NOT NULL CHECK (
        jsonb_typeof(output_schema) = 'object' AND output_schema <> '{}'::jsonb
    ),
    output JSONB CHECK (output IS NULL OR jsonb_typeof(output) = 'object'),
    status TEXT NOT NULL CHECK (status IN (
        'Pending', 'Running', 'WaitingApproval', 'Completed', 'Failed'
    )),
    tool_name TEXT NOT NULL DEFAULT '',
    error_code TEXT CHECK (error_code IS NULL OR error_code IN (` + stepErrorCodeCheck + `)),
    started_at TIMESTAMPTZ,
    ended_at TIMESTAMPTZ,
    CONSTRAINT step_run_sequence_unique UNIQUE (run_id, sequence),
    CONSTRAINT step_run_identity_unique UNIQUE (run_id, step_id),
    CONSTRAINT step_plan_foreign_key FOREIGN KEY (run_id, plan_id)
        REFERENCES plan (run_id, plan_id),
    CONSTRAINT step_tool_name_check CHECK (
        (type = 'ToolCall' AND tool_name <> '')
        OR (type <> 'ToolCall' AND tool_name = '')
    ),
    CONSTRAINT step_time_order_check CHECK (
        started_at IS NULL OR ended_at IS NULL OR ended_at >= started_at
    ),
    CONSTRAINT step_status_fields_check CHECK (
        (status = 'Pending' AND output IS NULL AND error_code IS NULL AND started_at IS NULL AND ended_at IS NULL)
        OR (status IN ('Running', 'WaitingApproval') AND output IS NULL AND error_code IS NULL AND started_at IS NOT NULL AND ended_at IS NULL)
        OR (status = 'Completed' AND output IS NOT NULL AND error_code IS NULL AND started_at IS NOT NULL AND ended_at IS NOT NULL)
        OR (status = 'Failed' AND output IS NULL AND error_code IS NOT NULL AND ended_at IS NOT NULL)
    )
)`

// 复合 FK 保证 current_step_id 只能指向该 Run 自己的 Step。
// DEFERRABLE 允许 Task Runtime 在同一结果事务中创建全部 Step 后再冻结 Run 指针。
const addRunCurrentStepForeignKeySQL = `
ALTER TABLE run
ADD CONSTRAINT run_current_step_foreign_key
FOREIGN KEY (run_id, current_step_id)
REFERENCES step (run_id, step_id)
DEFERRABLE INITIALLY DEFERRED`

const createStepPlanSequenceIndexSQL = `
CREATE INDEX step_plan_sequence_index
ON step (plan_id, sequence)`
