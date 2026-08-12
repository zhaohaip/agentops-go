package taskruntime

import (
	"context"
	"errors"
	"fmt"

	postgresruntime "github.com/zhaohaip/agentops-go/internal/adapter/postgres/runtime"
	"github.com/zhaohaip/agentops-go/internal/contracts"
	domain "github.com/zhaohaip/agentops-go/internal/taskruntime"
)

// SnapshotEvidenceProjection 使用调用方提供的同一个只读快照加载 Checkpoint 与 Tool 恢复证据。
//
// Phase 2/5 的 PostgreSQL 投影实现必须使用传入的 ReadSnapshot，不得另开连接或事务。
type SnapshotEvidenceProjection interface {
	LoadSnapshotEvidence(
		context.Context,
		postgresruntime.ReadSnapshot,
		contracts.TaskID,
		contracts.RunID,
		contracts.ExecutionVersion,
	) (domain.RecoverabilityEvidence, error)
}

// TaskQuerySnapshotRepository 在一个 REPEATABLE READ 只读事务中拼装查询投影。
type TaskQuerySnapshotRepository struct {
	reader   *postgresruntime.ReadPool
	evidence SnapshotEvidenceProjection
}

// NewTaskQuerySnapshotRepository 创建不会跨数据库快照拼装状态的查询 Repository。
func NewTaskQuerySnapshotRepository(
	reader *postgresruntime.ReadPool,
	evidence SnapshotEvidenceProjection,
) (*TaskQuerySnapshotRepository, error) {
	if reader == nil || evidence == nil {
		return nil, errors.New("create Task query snapshot repository: dependencies are required")
	}
	return &TaskQuerySnapshotRepository{reader: reader, evidence: evidence}, nil
}

// FindSnapshot 从一个数据库快照加载单个 Task 的完整查询投影。
func (r *TaskQuerySnapshotRepository) FindSnapshot(
	ctx context.Context,
	taskID contracts.TaskID,
) (domain.TaskQuerySnapshot, error) {
	if r == nil || r.reader == nil || r.evidence == nil {
		return domain.TaskQuerySnapshot{}, errors.New("find Task query snapshot: repository is not initialized")
	}
	var result domain.TaskQuerySnapshot
	err := r.reader.WithSnapshot(ctx, func(snapshotCtx context.Context, snapshot postgresruntime.ReadSnapshot) error {
		task, err := scanTask(snapshot.QueryRow(taskSelectSQL+" WHERE task_id = $1", taskID))
		if err != nil {
			return err
		}
		result, err = r.loadSnapshot(snapshotCtx, snapshot, task)
		return err
	})
	return result, err
}

// ListSnapshots 从一个数据库快照加载按稳定顺序排列的完整 Task 查询投影。
func (r *TaskQuerySnapshotRepository) ListSnapshots(
	ctx context.Context,
	status *contracts.TaskStatus,
) ([]domain.TaskQuerySnapshot, error) {
	if r == nil || r.reader == nil || r.evidence == nil {
		return nil, errors.New("list Task query snapshots: repository is not initialized")
	}
	var result []domain.TaskQuerySnapshot
	err := r.reader.WithSnapshot(ctx, func(snapshotCtx context.Context, snapshot postgresruntime.ReadSnapshot) error {
		query := taskSelectSQL + " ORDER BY created_at DESC, task_id ASC"
		var arguments []any
		if status != nil {
			query = taskSelectSQL + " WHERE status = $1 ORDER BY created_at DESC, task_id ASC"
			arguments = append(arguments, *status)
		}
		rows, err := snapshot.Query(query, arguments...)
		if err != nil {
			return err
		}
		tasks := make([]domain.Task, 0)
		for rows.Next() {
			task, scanErr := scanTask(rows)
			if scanErr != nil {
				rows.Close()
				return scanErr
			}
			tasks = append(tasks, task)
		}
		rowsErr := rows.Err()
		rows.Close()
		if rowsErr != nil {
			return rowsErr
		}

		result = make([]domain.TaskQuerySnapshot, 0, len(tasks))
		for _, task := range tasks {
			loaded, loadErr := r.loadSnapshot(snapshotCtx, snapshot, task)
			if loadErr != nil {
				return loadErr
			}
			result = append(result, loaded)
		}
		return nil
	})
	return result, err
}

func (r *TaskQuerySnapshotRepository) loadSnapshot(
	ctx context.Context,
	snapshot postgresruntime.ReadSnapshot,
	task domain.Task,
) (domain.TaskQuerySnapshot, error) {
	run, err := scanRun(snapshot.QueryRow(runSelectSQL+" WHERE task_id = $1", task.TaskID))
	if err != nil {
		return domain.TaskQuerySnapshot{}, fmt.Errorf("find Task query Run: %w", err)
	}
	execution, err := scanTaskExecution(snapshot.QueryRow(
		taskExecutionSelectSQL+" WHERE task_id = $1 AND execution_version = $2",
		task.TaskID,
		task.CurrentExecutionVersion,
	))
	if err != nil {
		return domain.TaskQuerySnapshot{}, fmt.Errorf("find Task query Execution: %w", err)
	}
	evidence, err := r.evidence.LoadSnapshotEvidence(
		ctx,
		snapshot,
		task.TaskID,
		run.RunID,
		execution.ExecutionVersion,
	)
	if err != nil {
		return domain.TaskQuerySnapshot{}, fmt.Errorf("load Task query recoverability evidence: %w", err)
	}
	return domain.TaskQuerySnapshot{Task: task, Run: run, Execution: execution, Evidence: evidence}, nil
}

var _ domain.TaskQuerySnapshotRepository = (*TaskQuerySnapshotRepository)(nil)
