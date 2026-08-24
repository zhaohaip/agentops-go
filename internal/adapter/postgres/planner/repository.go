// Package planner implements the Planner PostgreSQL Repository Port.
package planner

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	postgresruntime "github.com/zhaohaip/agentops-go/internal/adapter/postgres/runtime"
	"github.com/zhaohaip/agentops-go/internal/contracts"
	domain "github.com/zhaohaip/agentops-go/internal/planner"
)

// Repository stores immutable Plans through the caller's write transaction.
type Repository struct {
	reader *postgresruntime.ReadPool
}

// New creates a Planner PostgreSQL Repository.
func New(reader *postgresruntime.ReadPool) *Repository { return &Repository{reader: reader} }

// InsertIfCurrentExecution inserts one Plan only while the complete Task/Run/Execution Guard still matches.
func (*Repository) InsertIfCurrentExecution(
	ctx context.Context,
	token contracts.RuntimeWriteTx,
	entity domain.Entity,
	guard domain.CreateGuard,
) (bool, error) {
	if !guard.Valid() {
		return false, domain.ErrInvalidCreateGuard
	}
	if entity.PlanID() == "" || entity.RunID() == "" || entity.Goal() == "" || entity.CreatedAt().IsZero() {
		return false, domain.ErrInvalidPlan
	}

	var inserted bool
	err := postgresruntime.WithPostgreSQLWriteTx(token, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `
WITH guarded_run AS (
    SELECT r.run_id
    FROM task AS t
    JOIN run AS r ON r.task_id = t.task_id AND r.run_id = t.current_run_id
    JOIN task_execution AS e
      ON e.task_id = t.task_id AND e.execution_version = t.current_execution_version
    WHERE t.task_id = $1
      AND r.run_id = $2
      AND t.current_execution_version = $3
      AND e.execution_version = $3
      AND e.worker_id = $4
      AND t.status = $5
      AND r.status = $6
      AND e.status = $7
      AND r.plan_id IS NULL
      AND NOT EXISTS (SELECT 1 FROM plan AS existing WHERE existing.run_id = r.run_id)
    FOR UPDATE OF t, r, e
)
INSERT INTO plan (plan_id, run_id, goal, created_at)
SELECT $8, guarded_run.run_id, $9, $10
FROM guarded_run`,
			guard.TaskID, entity.RunID(), guard.ExecutionVersion, guard.WorkerID,
			guard.ExpectedTaskStatus, guard.ExpectedRunStatus, guard.ExpectedExecutionStatus,
			entity.PlanID(), entity.Goal(), entity.CreatedAt(),
		)
		if err != nil {
			return err
		}
		inserted = tag.RowsAffected() == 1
		return nil
	})
	return inserted, err
}

// FindByRun loads the immutable Plan through the Runtime read pool.
func (r *Repository) FindByRun(ctx context.Context, runID contracts.RunID) (domain.Entity, error) {
	if r == nil || r.reader == nil {
		return domain.Entity{}, errors.New("find Plan: read pool is not initialized")
	}
	var planID contracts.PlanID
	var storedRunID contracts.RunID
	var goal string
	var createdAt time.Time
	err := r.reader.QueryRow(ctx, `
SELECT plan_id, run_id, goal, created_at
FROM plan
WHERE run_id = $1`, runID).Scan(&planID, &storedRunID, &goal, &createdAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.Entity{}, domain.ErrRepositoryNotFound
	}
	if err != nil {
		return domain.Entity{}, err
	}
	entity, err := domain.NewEntity(planID, storedRunID, goal, createdAt)
	if err != nil {
		return domain.Entity{}, fmt.Errorf("scan Plan: %w", domain.ErrPersistenceInvariantViolation)
	}
	return entity, nil
}

var _ domain.Repository = (*Repository)(nil)
