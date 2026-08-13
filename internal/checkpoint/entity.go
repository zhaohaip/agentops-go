package checkpoint

import (
	"context"
	"encoding/json"
	"time"

	"github.com/zhaohaip/agentops-go/internal/contracts"
)

// Entity 是 Checkpoint 模块拥有的不可变持久化实体。
//
// 模块刻意不提供更新 DTO 或更新方法；持久化后只允许读取。
type Entity struct {
	CheckpointID           contracts.CheckpointID
	TaskID                 contracts.TaskID
	RunID                  contracts.RunID
	ExecutionVersion       contracts.ExecutionVersion
	CheckpointSequence     int64
	RuntimeContext         json.RawMessage
	ExecutionConfigHash    contracts.ExecutionConfigHash
	SourceExecutionVersion *contracts.ExecutionVersion
	SourceCheckpointID     *contracts.CheckpointID
	CreatedAt              time.Time
}

// View 是读取后携带严格解码 Runtime Context 的不可变视图。
type View struct {
	Entity
	Context contracts.RuntimeContextV1
}

// Ref 是保存后返回给调用方的最小引用。
type Ref struct {
	CheckpointID       contracts.CheckpointID
	CheckpointSequence int64
	CreatedAt          time.Time
}

// InferredType 是从不可变 Checkpoint 顶层事实推断的封闭类型。
type InferredType string

const (
	// InferredTypeInitialization 表示首次执行的初始化 Checkpoint。
	InferredTypeInitialization InferredType = "INITIALIZATION"
	// InferredTypeExecution 表示同版本普通执行 Checkpoint。
	InferredTypeExecution InferredType = "EXECUTION"
	// InferredTypeRecoveryStart 表示直接引用前一版本的恢复起点。
	InferredTypeRecoveryStart InferredType = "RECOVERY_START"
)

// TaskExecutionHash 是 Checkpoint 校验所需的最小 TaskExecution 投影。
type TaskExecutionHash struct {
	TaskID              contracts.TaskID
	ExecutionVersion    contracts.ExecutionVersion
	ExecutionConfigHash contracts.ExecutionConfigHash
}

// ValidationFacts 是 Manager 在同一事务内加载的只读持久化投影。
// 调用方无法构造或传入该事实集合。
type ValidationFacts struct {
	Task          TaskFact
	Run           RunFact
	Execution     ExecutionFact
	Plan          *PlanFact
	Step          *StepFact
	Previous      *StepFact
	HasLaterStep  bool
	Approval      *ApprovalFact
	ToolExecution *ToolExecutionFact
}

// TaskFact 是 Checkpoint 状态矩阵所需的最小 Task 投影。
type TaskFact struct {
	TaskID                  contracts.TaskID
	Status                  contracts.TaskStatus
	CurrentRunID            contracts.RunID
	CurrentExecutionVersion contracts.ExecutionVersion
	QueuedAt                *time.Time
}

// RunFact 是 Checkpoint 状态矩阵所需的最小 Run 投影。
type RunFact struct {
	RunID         contracts.RunID
	TaskID        contracts.TaskID
	Status        contracts.RunStatus
	PlanID        *contracts.PlanID
	CurrentStepID *contracts.StepID
}

// ExecutionFact 是 Checkpoint 状态矩阵所需的最小 TaskExecution 投影。
type ExecutionFact struct {
	TaskID              contracts.TaskID
	ExecutionVersion    contracts.ExecutionVersion
	Status              contracts.TaskExecutionStatus
	WorkerID            *contracts.WorkerID
	ExecutionConfigHash contracts.ExecutionConfigHash
}

// PlanFact 是 Checkpoint 引用校验所需的最小 Plan 投影。
type PlanFact struct {
	PlanID contracts.PlanID
	RunID  contracts.RunID
}

// StepFact 是动作和共享引用校验所需的最小 Step 投影。
type StepFact struct {
	StepID       contracts.StepID
	RunID        contracts.RunID
	PlanID       contracts.PlanID
	Sequence     uint32
	Type         contracts.StepType
	Status       contracts.StepStatus
	Input        json.RawMessage
	OutputSchema contracts.OutputSchema
	SafeOutput   json.RawMessage
	ToolName     contracts.ToolName
}

// ApprovalFact 是冻结 Approval 动作校验所需的最小投影。
type ApprovalFact struct {
	ApprovalID               contracts.ApprovalID
	TaskID                   contracts.TaskID
	RunID                    contracts.RunID
	StepID                   contracts.StepID
	ExecutionVersion         contracts.ExecutionVersion
	ExecutionConfigHash      contracts.ExecutionConfigHash
	OwnerExecutionConfigHash contracts.ExecutionConfigHash
	Status                   contracts.ApprovalStatus
	ToolName                 contracts.ToolName
	FrozenToolInput          contracts.FrozenToolInput
	ObservedValues           contracts.ObservedValues
	ResourceVersion          contracts.ResourceVersion
	FrozenInputHash          contracts.FrozenInputHash
}

// ToolExecutionFact 是动作持久化后果校验所需的最小投影。
type ToolExecutionFact struct {
	ToolExecutionID   contracts.ToolExecutionID
	TaskID            contracts.TaskID
	RunID             contracts.RunID
	StepID            contracts.StepID
	ExecutionVersion  contracts.ExecutionVersion
	Status            contracts.ToolExecutionStatus
	ErrorCode         *contracts.ErrorCode
	SideEffectUnknown bool
}

// ValidationFactsRequest 是 Repository 的模块内部加载计划，不属于公开 Manager 请求。
type ValidationFactsRequest struct {
	TaskID           contracts.TaskID
	RunID            contracts.RunID
	ExecutionVersion contracts.ExecutionVersion
	PlanID           *contracts.PlanID
	CurrentStepID    *contracts.StepID
	ApprovalID       *contracts.ApprovalID
}

// Repository 是 Checkpoint 持久化的最小事务 Port。
type Repository interface {
	AllocateNextSequence(ctx context.Context, tx contracts.RuntimeWriteTx, runID contracts.RunID) (int64, error)
	InsertCheckpoint(ctx context.Context, tx contracts.RuntimeWriteTx, entity Entity) (time.Time, error)
	FindLatestByExecutionVersion(ctx context.Context, tx contracts.RuntimeWriteTx, taskID contracts.TaskID, runID contracts.RunID, version contracts.ExecutionVersion) (Entity, error)
	FindByID(ctx context.Context, tx contracts.RuntimeWriteTx, checkpointID contracts.CheckpointID) (Entity, error)
	LoadTaskExecution(ctx context.Context, tx contracts.RuntimeWriteTx, taskID contracts.TaskID, version contracts.ExecutionVersion) (TaskExecutionHash, error)
	VerifyRunAttribution(ctx context.Context, tx contracts.RuntimeWriteTx, taskID contracts.TaskID, runID contracts.RunID) error
	LoadValidationFacts(ctx context.Context, tx contracts.RuntimeWriteTx, request ValidationFactsRequest) (ValidationFacts, error)
}
