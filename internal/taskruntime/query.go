package taskruntime

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/zhaohaip/agentops-go/internal/contracts"
)

// RecoverabilityEvidence 是未来 Checkpoint/Tool Provider 为查询提供的只读恢复证据。
type RecoverabilityEvidence struct {
	DatabaseNow                   time.Time
	CheckpointValid               bool
	CheckpointExecutionConfigHash contracts.ExecutionConfigHash
	HasRunningOrUnknownWriteTool  bool
}

// TaskView 是 Task 查询用例返回的当前持久化视图。
type TaskView struct {
	Task        Task
	Run         Run
	Execution   TaskExecution
	Recoverable bool
}

// TaskQuerySnapshot 是同一数据库一致性快照中的完整 Task 查询投影。
type TaskQuerySnapshot struct {
	Task      Task
	Run       Run
	Execution TaskExecution
	Evidence  RecoverabilityEvidence
}

// TaskQuerySnapshotRepository 原子读取 Get/List 所需的完整一致性投影。
type TaskQuerySnapshotRepository interface {
	FindSnapshot(context.Context, contracts.TaskID) (TaskQuerySnapshot, error)
	ListSnapshots(context.Context, *contracts.TaskStatus) ([]TaskQuerySnapshot, error)
}

// TaskQueryService 编排 Task、Run、当前 Execution 与恢复证据的只读查询。
type TaskQueryService struct {
	snapshots TaskQuerySnapshotRepository
	configs   AgentConfigSource
}

// NewTaskQueryService 创建未接入生产组合根的 Task 查询服务。
func NewTaskQueryService(snapshots TaskQuerySnapshotRepository, configs AgentConfigSource) (*TaskQueryService, error) {
	if snapshots == nil || configs == nil {
		return nil, errors.New("create Task query service: dependencies are required")
	}
	return &TaskQueryService{snapshots: snapshots, configs: configs}, nil
}

// GetTask 返回一个 Task 的当前查询视图。
func (s *TaskQueryService) GetTask(ctx context.Context, taskID contracts.TaskID) (TaskView, error) {
	if s == nil || taskID == "" {
		return TaskView{}, ErrInvalidArgument
	}
	snapshot, err := s.snapshots.FindSnapshot(ctx, taskID)
	if err != nil {
		return TaskView{}, err
	}
	return s.loadView(snapshot)
}

// ListTasks 按可选状态过滤返回稳定排序的 Task 当前视图。
func (s *TaskQueryService) ListTasks(ctx context.Context, status *contracts.TaskStatus) ([]TaskView, error) {
	if s == nil || (status != nil && !status.Valid()) {
		return nil, ErrInvalidArgument
	}
	snapshots, err := s.snapshots.ListSnapshots(ctx, status)
	if err != nil {
		return nil, err
	}
	views := make([]TaskView, 0, len(snapshots))
	for _, snapshot := range snapshots {
		view, err := s.loadView(snapshot)
		if err != nil {
			return nil, err
		}
		views = append(views, view)
	}
	return views, nil
}

func (s *TaskQueryService) loadView(snapshot TaskQuerySnapshot) (TaskView, error) {
	task := snapshot.Task
	execution := snapshot.Execution
	view := TaskView{Task: task, Run: snapshot.Run, Execution: execution}
	if (task.Status != contracts.TaskStatusRunning && task.Status != contracts.TaskStatusInterrupted) ||
		execution.Status != contracts.TaskExecutionStatusInterrupted || task.QueuedAt != nil {
		return view, nil
	}
	evidence := snapshot.Evidence
	if evidence.DatabaseNow.IsZero() || !evidence.DatabaseNow.Before(task.DeadlineAt) ||
		!evidence.CheckpointValid || evidence.HasRunningOrUnknownWriteTool {
		return view, nil
	}
	config, exists := s.configs.LookupAgent(task.AgentID)
	if !exists || !config.ExecutionConfig.Agent.Enabled {
		return view, nil
	}
	currentHash, err := HashExecutionConfigV1(config.ExecutionConfig)
	if err != nil {
		return TaskView{}, fmt.Errorf("hash Task query configuration: %w", err)
	}
	view.Recoverable = currentHash == execution.ExecutionConfigHash &&
		evidence.CheckpointExecutionConfigHash == execution.ExecutionConfigHash
	return view, nil
}
