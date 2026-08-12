package taskruntime

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/zhaohaip/agentops-go/internal/contracts"
)

var (
	// ErrRepositoryNotFound 表示请求的 Task Runtime 持久化对象不存在。
	ErrRepositoryNotFound = errors.New("Task Runtime repository object not found")
	// ErrPersistenceInvariantViolation 表示数据库返回了不满足冻结领域类型的持久化值。
	ErrPersistenceInvariantViolation = errors.New(string(contracts.ErrorCodePersistenceInvariantViolation))
)

// TaskUpdate 描述调用方已经决定的 Task 可变字段及其条件 Guard。
type TaskUpdate struct {
	TaskID                          contracts.TaskID
	ExpectedStatus                  contracts.TaskStatus
	ExpectedCurrentExecutionVersion contracts.ExecutionVersion
	Status                          contracts.TaskStatus
	CurrentExecutionVersion         contracts.ExecutionVersion
	ResultSummary                   *string
	ErrorCode                       *contracts.ErrorCode
	QueuedAt                        *time.Time
	StartedAt                       *time.Time
	EndedAt                         *time.Time
}

// RunUpdate 描述调用方已经决定的 Run 可变字段及其当前执行版本 Guard。
type RunUpdate struct {
	TaskID           contracts.TaskID
	RunID            contracts.RunID
	ExecutionVersion contracts.ExecutionVersion
	ExpectedStatus   contracts.RunStatus
	Status           contracts.RunStatus
	PlanID           *contracts.PlanID
	CurrentStepID    *contracts.StepID
	Context          json.RawMessage
	ErrorCode        *contracts.ErrorCode
	StartedAt        *time.Time
	EndedAt          *time.Time
}

// TaskExecutionUpdate 描述调用方已经决定的执行尝试字段及其所有权 Guard。
type TaskExecutionUpdate struct {
	TaskID             contracts.TaskID
	ExecutionVersion   contracts.ExecutionVersion
	ExpectedStatus     contracts.TaskExecutionStatus
	ExpectedWorkerID   *contracts.WorkerID
	Status             contracts.TaskExecutionStatus
	WorkerID           *contracts.WorkerID
	ObservedConfigHash *contracts.ExecutionConfigHash
	ErrorCode          *contracts.ErrorCode
	InvariantCode      *contracts.InvariantCode
	TerminationReason  *contracts.TerminationReason
	StartedAt          *time.Time
	EndedAt            *time.Time
}

// QueueCandidate 是按数据库 FIFO 顺序返回的持久化候选事实。
//
// Repository 不过滤或修复跨对象异常；状态是否合法由后续 Task Runtime 用例决定。
type QueueCandidate struct {
	TaskID           contracts.TaskID
	RunID            contracts.RunID
	ExecutionVersion contracts.ExecutionVersion
	TaskStatus       contracts.TaskStatus
	ExecutionStatus  contracts.TaskExecutionStatus
	QueuedAt         time.Time
	CreatedAt        time.Time
}

// TaskRepository 保存、读取和条件更新 Task。
type TaskRepository interface {
	Insert(context.Context, contracts.RuntimeWriteTx, Task) error
	Find(context.Context, contracts.TaskID) (Task, error)
	List(context.Context, *contracts.TaskStatus) ([]Task, error)
	Lock(context.Context, contracts.RuntimeWriteTx, contracts.TaskID) (Task, error)
	LockNextQueueCandidate(context.Context, contracts.RuntimeWriteTx) (QueueCandidate, error)
	Update(context.Context, contracts.RuntimeWriteTx, TaskUpdate) (bool, error)
}

// RunRepository 保存、读取和条件更新唯一 Run。
type RunRepository interface {
	Insert(context.Context, contracts.RuntimeWriteTx, Run) error
	FindByTask(context.Context, contracts.TaskID) (Run, error)
	LockByTask(context.Context, contracts.RuntimeWriteTx, contracts.TaskID) (Run, error)
	Update(context.Context, contracts.RuntimeWriteTx, RunUpdate) (bool, error)
}

// TaskExecutionRepository 保存、读取和条件更新明确版本的 TaskExecution。
type TaskExecutionRepository interface {
	Insert(context.Context, contracts.RuntimeWriteTx, TaskExecution) error
	FindByTaskVersion(context.Context, contracts.TaskID, contracts.ExecutionVersion) (TaskExecution, error)
	LockByTaskVersion(context.Context, contracts.RuntimeWriteTx, contracts.TaskID, contracts.ExecutionVersion) (TaskExecution, error)
	Update(context.Context, contracts.RuntimeWriteTx, TaskExecutionUpdate) (bool, error)
}

// CommandReceiptRepository 保存和按 command_id 读取不可变 Receipt。
type CommandReceiptRepository interface {
	Insert(context.Context, contracts.RuntimeWriteTx, CommandReceipt) error
	Find(context.Context, CommandID) (CommandReceipt, error)
	Lock(context.Context, contracts.RuntimeWriteTx, CommandID) (CommandReceipt, error)
}

// TaskLogRepository 只提供 append-only 写入。
type TaskLogRepository interface {
	Append(context.Context, contracts.RuntimeWriteTx, TaskLog) error
}

// DatabaseClock 在调用方写事务内取得 PostgreSQL UTC 时间。
type DatabaseClock interface {
	Now(context.Context, contracts.RuntimeWriteTx) (time.Time, error)
}
