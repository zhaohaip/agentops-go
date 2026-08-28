package stepexecutor

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/zhaohaip/agentops-go/internal/contracts"
)

var (
	// ErrRepositoryNotFound 表示指定 Step 不存在。
	ErrRepositoryNotFound = errors.New("Step repository fact not found")
	// ErrInvalidStepBatch 表示待创建 Step 不是同一 Run/Plan 下从 1 开始的连续 Pending 序列。
	ErrInvalidStepBatch = errors.New("invalid Step batch")
	// ErrInvalidUpdateGuard 表示条件更新请求缺少冻结 Guard。
	ErrInvalidUpdateGuard = errors.New("invalid Step update guard")
)

// UpdateGuard 是 Step 条件更新必须原子复核的当前执行事实。
type UpdateGuard struct {
	TaskID                  contracts.TaskID
	ExecutionVersion        contracts.ExecutionVersion
	ExpectedWorkerID        *contracts.WorkerID
	ExpectedTaskStatus      contracts.TaskStatus
	ExpectedRunStatus       contracts.RunStatus
	ExpectedExecutionStatus contracts.TaskExecutionStatus
}

// Valid 报告 Guard 是否包含完整且封闭的期望事实。
func (g UpdateGuard) Valid() bool {
	return g.TaskID != "" && g.ExecutionVersion.Valid() &&
		g.ExpectedTaskStatus.Valid() && g.ExpectedRunStatus.Valid() && g.ExpectedExecutionStatus.Valid() &&
		(g.ExpectedWorkerID == nil || *g.ExpectedWorkerID != "")
}

// Update 描述调用方已经决定的 Step 可变字段及其条件 Guard。
type Update struct {
	Guard          UpdateGuard
	RunID          contracts.RunID
	StepID         contracts.StepID
	ExpectedStatus contracts.StepStatus
	Status         contracts.StepStatus
	Output         json.RawMessage
	ErrorCode      *contracts.ErrorCode
	StartedAt      *time.Time
	EndedAt        *time.Time
}

// Valid 报告更新载荷是否满足结构和目标状态约束。
func (u Update) Valid() bool {
	return u.Guard.Valid() && u.RunID != "" && u.StepID != "" && u.ExpectedStatus.Valid() &&
		u.Status.Valid() && validStepTransition(u)
}

func validStepTransition(update Update) bool {
	switch update.ExpectedStatus {
	case contracts.StepStatusPending:
		switch update.Status {
		case contracts.StepStatusRunning:
			return update.Output == nil && update.ErrorCode == nil && update.StartedAt != nil && update.EndedAt == nil
		case contracts.StepStatusFailed:
			return validFailedUpdate(update) && update.StartedAt == nil
		}
	case contracts.StepStatusRunning:
		switch update.Status {
		case contracts.StepStatusWaitingApproval:
			return update.Output == nil && update.ErrorCode == nil && update.StartedAt == nil && update.EndedAt == nil
		case contracts.StepStatusCompleted:
			return validJSONObject(update.Output) && update.ErrorCode == nil && update.StartedAt == nil && update.EndedAt != nil
		case contracts.StepStatusFailed:
			return validFailedUpdate(update) && update.StartedAt == nil
		}
	case contracts.StepStatusWaitingApproval:
		switch update.Status {
		case contracts.StepStatusRunning:
			return update.Output == nil && update.ErrorCode == nil && update.StartedAt == nil && update.EndedAt == nil
		case contracts.StepStatusFailed:
			return validFailedUpdate(update) && update.StartedAt == nil
		}
	}
	return false
}

func validFailedUpdate(update Update) bool {
	return update.Output == nil && update.ErrorCode != nil && update.ErrorCode.Valid() && update.EndedAt != nil
}

// Repository 是 Step 的最小数据访问 Port。
type Repository interface {
	InsertAll(context.Context, contracts.RuntimeWriteTx, []Entity) error
	FindByID(context.Context, contracts.StepID) (Entity, error)
	ListByRun(context.Context, contracts.RunID) ([]Entity, error)
	LockByID(context.Context, contracts.RuntimeWriteTx, contracts.StepID) (Entity, error)
	Update(context.Context, contracts.RuntimeWriteTx, Update) (bool, error)
}
