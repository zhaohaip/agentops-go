package planner

import (
	"context"
	"errors"

	"github.com/zhaohaip/agentops-go/internal/contracts"
)

var (
	// ErrRepositoryNotFound 表示指定 Run 尚无 Plan。
	ErrRepositoryNotFound = errors.New("Plan repository fact not found")
	// ErrInvalidCreateGuard 表示条件写请求缺少冻结 Guard。
	ErrInvalidCreateGuard = errors.New("invalid Plan create guard")
)

// CreateGuard 是 Task Runtime 在结果事务中创建 Plan 必须原子复核的持久化条件。
type CreateGuard struct {
	TaskID                  contracts.TaskID
	ExecutionVersion        contracts.ExecutionVersion
	WorkerID                contracts.WorkerID
	ExpectedTaskStatus      contracts.TaskStatus
	ExpectedRunStatus       contracts.RunStatus
	ExpectedExecutionStatus contracts.TaskExecutionStatus
}

// Valid 报告 Guard 是否包含完整且封闭的期望事实。
func (g CreateGuard) Valid() bool {
	return g.TaskID != "" && g.ExecutionVersion.Valid() && g.WorkerID != "" &&
		g.ExpectedTaskStatus.Valid() && g.ExpectedRunStatus.Valid() && g.ExpectedExecutionStatus.Valid()
}

// Repository 是 Plan 的最小数据访问 Port。没有 Update：Plan 创建后不可变。
type Repository interface {
	InsertIfCurrentExecution(context.Context, contracts.RuntimeWriteTx, Entity, CreateGuard) (bool, error)
	FindByRun(context.Context, contracts.RunID) (Entity, error)
}
