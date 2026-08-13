// Package checkpoint implements the Checkpoint PostgreSQL Repository Port.
package checkpoint

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	postgresruntime "github.com/zhaohaip/agentops-go/internal/adapter/postgres/runtime"
	domain "github.com/zhaohaip/agentops-go/internal/checkpoint"
	"github.com/zhaohaip/agentops-go/internal/contracts"
)

// Repository stores immutable Checkpoints through the caller's write transaction.
type Repository struct{}

// New creates a Checkpoint PostgreSQL Repository.
func New() *Repository { return &Repository{} }

func (*Repository) AllocateNextSequence(ctx context.Context, token contracts.RuntimeWriteTx, runID contracts.RunID) (int64, error) {
	var sequence int64
	err := withWriteTx(token, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT COALESCE(MAX(checkpoint_sequence), 0) + 1 FROM checkpoint WHERE run_id = $1`, runID).Scan(&sequence)
	})
	return sequence, err
}

func (*Repository) InsertCheckpoint(ctx context.Context, token contracts.RuntimeWriteTx, entity domain.Entity) (time.Time, error) {
	var createdAt time.Time
	err := withWriteTx(token, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `
INSERT INTO checkpoint (
    checkpoint_id, task_id, run_id, execution_version, checkpoint_sequence,
    runtime_context, execution_config_hash, source_execution_version,
    source_checkpoint_id, created_at
) VALUES ($1, $2, $3, $4, $5, $6::jsonb, $7, $8, $9, transaction_timestamp())
RETURNING created_at`, entity.CheckpointID, entity.TaskID, entity.RunID, entity.ExecutionVersion,
			entity.CheckpointSequence, string(entity.RuntimeContext), entity.ExecutionConfigHash,
			entity.SourceExecutionVersion, entity.SourceCheckpointID).Scan(&createdAt)
	})
	return createdAt.UTC(), err
}

func (*Repository) FindLatestByExecutionVersion(ctx context.Context, token contracts.RuntimeWriteTx, taskID contracts.TaskID, runID contracts.RunID, version contracts.ExecutionVersion) (domain.Entity, error) {
	return findOne(ctx, token, `
SELECT checkpoint_id, task_id, run_id, execution_version, checkpoint_sequence,
       runtime_context, execution_config_hash, source_execution_version,
       source_checkpoint_id, created_at
FROM checkpoint
WHERE task_id = $1 AND run_id = $2 AND execution_version = $3
ORDER BY checkpoint_sequence DESC
LIMIT 1`, taskID, runID, version)
}

func (*Repository) FindByID(ctx context.Context, token contracts.RuntimeWriteTx, checkpointID contracts.CheckpointID) (domain.Entity, error) {
	return findOne(ctx, token, `
SELECT checkpoint_id, task_id, run_id, execution_version, checkpoint_sequence,
       runtime_context, execution_config_hash, source_execution_version,
       source_checkpoint_id, created_at
FROM checkpoint WHERE checkpoint_id = $1`, checkpointID)
}

func (*Repository) LoadTaskExecution(ctx context.Context, token contracts.RuntimeWriteTx, taskID contracts.TaskID, version contracts.ExecutionVersion) (domain.TaskExecutionHash, error) {
	var result domain.TaskExecutionHash
	err := withWriteTx(token, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `
SELECT task_id, execution_version, execution_config_hash
FROM task_execution WHERE task_id = $1 AND execution_version = $2`, taskID, version).
			Scan(&result.TaskID, &result.ExecutionVersion, &result.ExecutionConfigHash)
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.TaskExecutionHash{}, domain.ErrPersistenceInvariantViolation
	}
	return result, err
}

func (*Repository) VerifyRunAttribution(ctx context.Context, token contracts.RuntimeWriteTx, taskID contracts.TaskID, runID contracts.RunID) error {
	var exists bool
	err := withWriteTx(token, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM run WHERE task_id = $1 AND run_id = $2)`, taskID, runID).Scan(&exists)
	})
	if err != nil {
		return err
	}
	if !exists {
		return domain.ErrPersistenceInvariantViolation
	}
	return nil
}

// LoadValidationFacts 在调用方事务中加载 Checkpoint 语义校验所需的完整持久化投影。
func (*Repository) LoadValidationFacts(ctx context.Context, token contracts.RuntimeWriteTx, request domain.ValidationFactsRequest) (domain.ValidationFacts, error) {
	var facts domain.ValidationFacts
	err := withWriteTx(token, func(tx pgx.Tx) error {
		if err := tx.QueryRow(ctx, `
SELECT t.task_id, t.status, t.current_run_id, t.current_execution_version, t.queued_at, t.error_code,
       r.run_id, r.task_id, r.status, r.plan_id, r.current_step_id,
       e.task_id, e.execution_version, e.status, e.worker_id, e.execution_config_hash,
       e.observed_config_hash, e.error_code, e.started_at
FROM task AS t
JOIN run AS r ON r.task_id = t.task_id AND r.run_id = $2
JOIN task_execution AS e ON e.task_id = t.task_id AND e.execution_version = $3
WHERE t.task_id = $1`, request.TaskID, request.RunID, request.ExecutionVersion).Scan(
			&facts.Task.TaskID, &facts.Task.Status, &facts.Task.CurrentRunID,
			&facts.Task.CurrentExecutionVersion, &facts.Task.QueuedAt,
			&facts.Task.ErrorCode,
			&facts.Run.RunID, &facts.Run.TaskID, &facts.Run.Status, &facts.Run.PlanID, &facts.Run.CurrentStepID,
			&facts.Execution.TaskID, &facts.Execution.ExecutionVersion, &facts.Execution.Status,
			&facts.Execution.WorkerID, &facts.Execution.ExecutionConfigHash,
			&facts.Execution.ObservedConfigHash, &facts.Execution.ErrorCode, &facts.Execution.StartedAt,
		); err != nil {
			return err
		}
		if request.PlanID == nil && request.CurrentStepID == nil && request.ApprovalID == nil {
			return nil
		}
		if request.PlanID != nil {
			var plan domain.PlanFact
			err := tx.QueryRow(ctx, `SELECT plan_id, run_id FROM plan WHERE plan_id = $1`, *request.PlanID).
				Scan(&plan.PlanID, &plan.RunID)
			if errors.Is(err, pgx.ErrNoRows) {
				return nil
			}
			if err != nil {
				return err
			}
			facts.Plan = &plan
		}
		if request.CurrentStepID != nil {
			step, err := loadStepFact(ctx, tx, *request.CurrentStepID)
			if errors.Is(err, pgx.ErrNoRows) {
				return nil
			}
			if err != nil {
				return err
			}
			facts.Step = &step
			if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM step WHERE run_id = $1 AND sequence > $2)`, step.RunID, step.Sequence).
				Scan(&facts.HasLaterStep); err != nil {
				return err
			}
			if step.Sequence > 1 {
				previous, err := loadStepBySequence(ctx, tx, step.RunID, step.Sequence-1)
				if err != nil && !errors.Is(err, pgx.ErrNoRows) {
					return err
				}
				if err == nil {
					facts.Previous = &previous
				}
			}
			toolExecution, err := loadToolExecution(ctx, tx, request, step.StepID)
			if err != nil && !errors.Is(err, pgx.ErrNoRows) {
				return err
			}
			if err == nil {
				facts.ToolExecution = &toolExecution
			}
		}
		if request.ApprovalID != nil {
			approval, err := loadApprovalByID(ctx, tx, *request.ApprovalID)
			if errors.Is(err, pgx.ErrNoRows) {
				return nil
			}
			if err != nil {
				return err
			}
			facts.Approval = &approval
		} else if request.CurrentStepID != nil {
			approval, err := loadApprovalByStep(ctx, tx, request, *request.CurrentStepID)
			if err != nil && !errors.Is(err, pgx.ErrNoRows) {
				return err
			}
			if err == nil {
				facts.Approval = &approval
			}
		}
		return nil
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.ValidationFacts{}, domain.ErrPersistenceInvariantViolation
	}
	return facts, err
}

func loadApprovalByID(ctx context.Context, tx pgx.Tx, approvalID contracts.ApprovalID) (domain.ApprovalFact, error) {
	var approval domain.ApprovalFact
	var ownerExecutionConfigHash *string
	err := tx.QueryRow(ctx, `
SELECT a.approval_id, a.task_id, a.run_id, a.step_id, a.execution_version,
       a.execution_config_hash,
       (SELECT e.execution_config_hash FROM task_execution AS e WHERE e.task_id = a.task_id AND e.execution_version = a.execution_version),
       a.status, a.tool_name,
       a.frozen_tool_input, a.observed_values, a.resource_version, a.frozen_input_hash
FROM approval AS a
WHERE a.approval_id = $1`, approvalID).Scan(
		&approval.ApprovalID, &approval.TaskID, &approval.RunID, &approval.StepID,
		&approval.ExecutionVersion, &approval.ExecutionConfigHash, &ownerExecutionConfigHash,
		&approval.Status, &approval.ToolName, &approval.FrozenToolInput, &approval.ObservedValues,
		&approval.ResourceVersion, &approval.FrozenInputHash,
	)
	if err == nil && ownerExecutionConfigHash == nil {
		return domain.ApprovalFact{}, domain.ErrPersistenceInvariantViolation
	}
	if ownerExecutionConfigHash != nil {
		approval.OwnerExecutionConfigHash = contracts.ExecutionConfigHash(*ownerExecutionConfigHash)
	}
	return approval, err
}

func loadApprovalByStep(ctx context.Context, tx pgx.Tx, request domain.ValidationFactsRequest, stepID contracts.StepID) (domain.ApprovalFact, error) {
	var approval domain.ApprovalFact
	var ownerExecutionConfigHash *string
	err := tx.QueryRow(ctx, `
SELECT a.approval_id, a.task_id, a.run_id, a.step_id, a.execution_version,
       a.execution_config_hash,
       (SELECT e.execution_config_hash FROM task_execution AS e WHERE e.task_id = a.task_id AND e.execution_version = a.execution_version),
       a.status, a.tool_name,
       a.frozen_tool_input, a.observed_values, a.resource_version, a.frozen_input_hash
FROM approval AS a
WHERE a.task_id = $1 AND a.run_id = $2 AND a.step_id = $3 AND a.execution_version = $4
ORDER BY a.approval_id
LIMIT 1`, request.TaskID, request.RunID, stepID, request.ExecutionVersion).Scan(
		&approval.ApprovalID, &approval.TaskID, &approval.RunID, &approval.StepID,
		&approval.ExecutionVersion, &approval.ExecutionConfigHash, &ownerExecutionConfigHash,
		&approval.Status, &approval.ToolName, &approval.FrozenToolInput, &approval.ObservedValues,
		&approval.ResourceVersion, &approval.FrozenInputHash,
	)
	if err == nil && ownerExecutionConfigHash == nil {
		return domain.ApprovalFact{}, domain.ErrPersistenceInvariantViolation
	}
	if ownerExecutionConfigHash != nil {
		approval.OwnerExecutionConfigHash = contracts.ExecutionConfigHash(*ownerExecutionConfigHash)
	}
	return approval, err
}

func loadStepFact(ctx context.Context, tx pgx.Tx, stepID contracts.StepID) (domain.StepFact, error) {
	var step domain.StepFact
	var sequence int64
	err := tx.QueryRow(ctx, `
SELECT step_id, run_id, plan_id, sequence, type, status, input, output_schema, output, tool_name
FROM step WHERE step_id = $1`, stepID).Scan(
		&step.StepID, &step.RunID, &step.PlanID, &sequence, &step.Type, &step.Status,
		&step.Input, &step.OutputSchema, &step.SafeOutput, &step.ToolName,
	)
	if err != nil {
		return domain.StepFact{}, err
	}
	if sequence <= 0 {
		return domain.StepFact{}, domain.ErrPersistenceInvariantViolation
	}
	step.Sequence = uint32(sequence)
	return step, nil
}

func loadStepBySequence(ctx context.Context, tx pgx.Tx, runID contracts.RunID, sequence uint32) (domain.StepFact, error) {
	var stepID contracts.StepID
	if err := tx.QueryRow(ctx, `SELECT step_id FROM step WHERE run_id = $1 AND sequence = $2`, runID, sequence).Scan(&stepID); err != nil {
		return domain.StepFact{}, err
	}
	return loadStepFact(ctx, tx, stepID)
}

func loadToolExecution(ctx context.Context, tx pgx.Tx, request domain.ValidationFactsRequest, stepID contracts.StepID) (domain.ToolExecutionFact, error) {
	var tool domain.ToolExecutionFact
	var errorCode *string
	err := tx.QueryRow(ctx, `
SELECT tool_execution_id, task_id, run_id, step_id, execution_version, status, error_code, side_effect_unknown
FROM tool_execution
WHERE step_id = $1 AND execution_version = $2
ORDER BY tool_execution_id
LIMIT 1`, stepID, request.ExecutionVersion).Scan(
		&tool.ToolExecutionID, &tool.TaskID, &tool.RunID, &tool.StepID, &tool.ExecutionVersion,
		&tool.Status, &errorCode, &tool.SideEffectUnknown,
	)
	if errorCode != nil {
		converted := contracts.ErrorCode(*errorCode)
		tool.ErrorCode = &converted
	}
	return tool, err
}

func findOne(ctx context.Context, token contracts.RuntimeWriteTx, query string, args ...any) (domain.Entity, error) {
	var result domain.Entity
	err := withWriteTx(token, func(tx pgx.Tx) error {
		var runtimeContext []byte
		err := tx.QueryRow(ctx, query, args...).Scan(
			&result.CheckpointID, &result.TaskID, &result.RunID, &result.ExecutionVersion,
			&result.CheckpointSequence, &runtimeContext, &result.ExecutionConfigHash,
			&result.SourceExecutionVersion, &result.SourceCheckpointID, &result.CreatedAt,
		)
		result.RuntimeContext = append(result.RuntimeContext[:0], runtimeContext...)
		return err
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.Entity{}, domain.ErrRepositoryNotFound
	}
	if err != nil {
		return domain.Entity{}, err
	}
	if result.CheckpointSequence <= 0 || !result.ExecutionVersion.Valid() || !result.ExecutionConfigHash.Valid() {
		return domain.Entity{}, fmt.Errorf("scan Checkpoint: %w", domain.ErrPersistenceInvariantViolation)
	}
	result.CreatedAt = result.CreatedAt.UTC()
	return result, nil
}

func withWriteTx(token contracts.RuntimeWriteTx, work func(pgx.Tx) error) error {
	return postgresruntime.WithPostgreSQLWriteTx(token, work)
}

var _ domain.Repository = (*Repository)(nil)
