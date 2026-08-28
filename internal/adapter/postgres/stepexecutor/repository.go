// Package stepexecutor 实现 Step Executor 的 PostgreSQL Repository Port。
package stepexecutor

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/jackc/pgx/v5"
	postgresruntime "github.com/zhaohaip/agentops-go/internal/adapter/postgres/runtime"
	"github.com/zhaohaip/agentops-go/internal/contracts"
	domain "github.com/zhaohaip/agentops-go/internal/stepexecutor"
)

// Repository 通过调用方事务保存并读取 Step。
type Repository struct {
	reader *postgresruntime.ReadPool
}

// New 创建 Step Executor PostgreSQL Repository。
func New(reader *postgresruntime.ReadPool) *Repository { return &Repository{reader: reader} }

// InsertAll 在调用方事务内创建一组完整、连续的 Pending Step。
func (*Repository) InsertAll(ctx context.Context, token contracts.RuntimeWriteTx, entities []domain.Entity) error {
	if err := validateInsertBatch(entities); err != nil {
		return err
	}
	return withWriteTx(token, func(tx pgx.Tx) error {
		for _, entity := range entities {
			outputSchema, err := json.Marshal(entity.OutputSchema())
			if err != nil {
				return fmt.Errorf("encode Step output schema: %w", err)
			}
			_, err = tx.Exec(ctx, `
INSERT INTO step (
    step_id, run_id, plan_id, sequence, type, name, input, output_schema,
    output, status, tool_name, error_code, started_at, ended_at
) VALUES (
    $1, $2, $3, $4, $5, $6, $7::jsonb, $8::jsonb,
    $9::jsonb, $10, $11, $12, $13, $14
)`,
				entity.StepID(), entity.RunID(), entity.PlanID(), entity.Sequence(), entity.Type(), entity.Name(),
				string(entity.Input()), string(outputSchema), nullableJSON(entity.Output()), entity.Status(),
				entity.ToolName(), entity.ErrorCode(), entity.StartedAt(), entity.EndedAt(),
			)
			if err != nil {
				return err
			}
		}
		return nil
	})
}

// FindByID 通过 Runtime 只读池加载一个 Step。
func (r *Repository) FindByID(ctx context.Context, stepID contracts.StepID) (domain.Entity, error) {
	if r == nil || r.reader == nil {
		return domain.Entity{}, errors.New("find Step: read pool is not initialized")
	}
	return scanStep(r.reader.QueryRow(ctx, stepSelectSQL+" WHERE step_id = $1", stepID))
}

// ListByRun 按顺序返回 Run 的全部 Step，并拒绝非 1 起始或存在缺口的持久化序列。
func (r *Repository) ListByRun(ctx context.Context, runID contracts.RunID) ([]domain.Entity, error) {
	if r == nil || r.reader == nil {
		return nil, errors.New("list Steps: read pool is not initialized")
	}
	rows, err := r.reader.Query(ctx, stepSelectSQL+" WHERE run_id = $1 ORDER BY sequence ASC", runID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	result := make([]domain.Entity, 0)
	for rows.Next() {
		entity, err := scanStep(rows)
		if err != nil {
			return nil, err
		}
		if entity.RunID() != runID || entity.Sequence() != uint32(len(result)+1) {
			return nil, fmt.Errorf("list Steps for Run %q: %w", runID, domain.ErrPersistenceInvariantViolation)
		}
		result = append(result, entity)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return result, nil
}

// LockByID 在调用方事务内加载并锁定一个 Step。
func (*Repository) LockByID(ctx context.Context, token contracts.RuntimeWriteTx, stepID contracts.StepID) (domain.Entity, error) {
	var result domain.Entity
	err := withWriteTx(token, func(tx pgx.Tx) error {
		var err error
		result, err = scanStep(tx.QueryRow(ctx, stepSelectSQL+" WHERE step_id = $1 FOR UPDATE", stepID))
		return err
	})
	return result, err
}

// Update 仅在完整当前执行 Guard 仍匹配时写入调用方已经决定的 Step 字段。
func (*Repository) Update(ctx context.Context, token contracts.RuntimeWriteTx, update domain.Update) (bool, error) {
	if !update.Valid() {
		return false, domain.ErrInvalidUpdateGuard
	}

	var updated bool
	err := withWriteTx(token, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `
WITH guarded_step AS (
    SELECT s.step_id
    FROM task AS t
    JOIN run AS r ON r.task_id = t.task_id AND r.run_id = t.current_run_id
    JOIN task_execution AS e
      ON e.task_id = t.task_id AND e.execution_version = t.current_execution_version
    JOIN step AS s ON s.run_id = r.run_id AND s.step_id = r.current_step_id
    WHERE t.task_id = $1
      AND r.run_id = $2
      AND s.step_id = $3
      AND t.current_execution_version = $4
      AND e.execution_version = $4
      AND e.worker_id IS NOT DISTINCT FROM $5
      AND t.status = $6
      AND r.status = $7
      AND e.status = $8
      AND s.status = $9
    FOR UPDATE OF t, r, e, s
)
UPDATE step AS s
SET status = $10,
    output = $11::jsonb,
    error_code = $12,
    started_at = CASE
        WHEN s.status = 'Pending' AND $10 = 'Running' THEN $13
        ELSE s.started_at
    END,
    ended_at = $14
FROM guarded_step AS guarded
WHERE s.step_id = guarded.step_id`,
			update.Guard.TaskID, update.RunID, update.StepID, update.Guard.ExecutionVersion,
			update.Guard.ExpectedWorkerID, update.Guard.ExpectedTaskStatus, update.Guard.ExpectedRunStatus,
			update.Guard.ExpectedExecutionStatus, update.ExpectedStatus, update.Status,
			nullableJSON(update.Output), update.ErrorCode, update.StartedAt, update.EndedAt,
		)
		if err != nil {
			return err
		}
		updated = tag.RowsAffected() == 1
		return nil
	})
	return updated, err
}

func validateInsertBatch(entities []domain.Entity) error {
	if len(entities) == 0 {
		return domain.ErrInvalidStepBatch
	}
	runID := entities[0].RunID()
	planID := entities[0].PlanID()
	for index, entity := range entities {
		_, err := domain.NewEntity(domain.EntityParams{
			StepID: entity.StepID(), RunID: entity.RunID(), PlanID: entity.PlanID(), Sequence: entity.Sequence(),
			Type: entity.Type(), Name: entity.Name(), Input: entity.Input(), OutputSchema: entity.OutputSchema(),
			Output: entity.Output(), Status: entity.Status(), ToolName: entity.ToolName(),
			ErrorCode: entity.ErrorCode(), StartedAt: entity.StartedAt(), EndedAt: entity.EndedAt(),
		})
		if err != nil || entity.RunID() != runID || entity.PlanID() != planID ||
			entity.Sequence() != uint32(index+1) || entity.Status() != contracts.StepStatusPending {
			return domain.ErrInvalidStepBatch
		}
	}
	return nil
}

func scanStep(row interface{ Scan(...any) error }) (domain.Entity, error) {
	var (
		stepID           contracts.StepID
		runID            contracts.RunID
		planID           contracts.PlanID
		sequence         int64
		stepType         contracts.StepType
		name             string
		input            []byte
		outputSchemaJSON []byte
		output           []byte
		status           contracts.StepStatus
		toolName         contracts.ToolName
		errorCode        *contracts.ErrorCode
		startedAt        *time.Time
		endedAt          *time.Time
	)
	err := row.Scan(
		&stepID, &runID, &planID, &sequence, &stepType, &name, &input, &outputSchemaJSON,
		&output, &status, &toolName, &errorCode, &startedAt, &endedAt,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.Entity{}, domain.ErrRepositoryNotFound
	}
	if err != nil {
		return domain.Entity{}, err
	}
	if sequence <= 0 || sequence > int64(^uint32(0)) {
		return domain.Entity{}, fmt.Errorf("scan Step: %w", domain.ErrPersistenceInvariantViolation)
	}
	outputSchema, err := decodeOutputSchema(outputSchemaJSON)
	if err != nil {
		return domain.Entity{}, fmt.Errorf("scan Step output schema: %w", domain.ErrPersistenceInvariantViolation)
	}
	entity, err := domain.NewEntity(domain.EntityParams{
		StepID: stepID, RunID: runID, PlanID: planID, Sequence: uint32(sequence), Type: stepType, Name: name,
		Input: append(json.RawMessage(nil), input...), OutputSchema: outputSchema,
		Output: append(json.RawMessage(nil), output...), Status: status, ToolName: toolName,
		ErrorCode: errorCode, StartedAt: startedAt, EndedAt: endedAt,
	})
	if err != nil {
		return domain.Entity{}, fmt.Errorf("scan Step: %w", domain.ErrPersistenceInvariantViolation)
	}
	return entity, nil
}

func decodeOutputSchema(encoded []byte) (contracts.OutputSchema, error) {
	decoder := json.NewDecoder(bytes.NewReader(encoded))
	decoder.DisallowUnknownFields()
	var schema contracts.OutputSchema
	if err := decoder.Decode(&schema); err != nil {
		return nil, err
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		return nil, errors.New("output schema has trailing JSON value")
	}
	return schema, nil
}

func nullableJSON(value json.RawMessage) any {
	if value == nil {
		return nil
	}
	return string(value)
}

func withWriteTx(token contracts.RuntimeWriteTx, work func(pgx.Tx) error) error {
	return postgresruntime.WithPostgreSQLWriteTx(token, work)
}

const stepSelectSQL = `
SELECT step_id, run_id, plan_id, sequence, type, name, input, output_schema,
       output, status, tool_name, error_code, started_at, ended_at
FROM step`

var _ domain.Repository = (*Repository)(nil)
