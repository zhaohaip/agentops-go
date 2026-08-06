package taskruntime

import (
	"context"
	"time"

	"github.com/zhaohaip/agentops-go/internal/contracts"
)

// TerminationStep 是 Cancel/Timeout 需要锁定的当前 Step 最小投影。
type TerminationStep struct {
	StepID    contracts.StepID
	Status    contracts.StepStatus
	ErrorCode *contracts.ErrorCode
	EndedAt   *time.Time
}

// TerminationToolExecution 是 Cancel/Timeout 需要锁定的当前 ToolExecution 最小投影。
// read_only 不来自该投影，Runtime 必须使用冻结 ExecutionConfig 中的 Tool 定义判断。
type TerminationToolExecution struct {
	ToolExecutionID   contracts.ToolExecutionID
	ToolName          contracts.ToolName
	Status            contracts.ToolExecutionStatus
	ErrorCode         *contracts.ErrorCode
	SideEffectUnknown bool
	EndedAt           *time.Time
}

// TerminationFacts 是一次终态命令在同一事务内锁定的完整最小事实。
type TerminationFacts struct {
	Task          Task
	Run           Run
	Execution     TaskExecution
	Step          *TerminationStep
	ToolExecution *TerminationToolExecution
}

// ApplyTerminationRequest 描述调用方已由 Lifecycle Policy 决定的原子终态写入。
type ApplyTerminationRequest struct {
	TaskID                   contracts.TaskID
	ExpectedExecutionVersion contracts.ExecutionVersion
	ExpectedTaskStatus       contracts.TaskStatus
	ExpectedRunStatus        contracts.RunStatus
	ExpectedExecutionStatus  contracts.TaskExecutionStatus
	ExpectedStepStatus       *contracts.StepStatus
	ExpectedToolStatus       *contracts.ToolExecutionStatus
	TaskStatus               contracts.TaskStatus
	TaskErrorCode            contracts.ErrorCode
	RunErrorCode             contracts.ErrorCode
	StepErrorCode            contracts.ErrorCode
	ExecutionErrorCode       *contracts.ErrorCode
	TerminationReason        contracts.TerminationReason
	ToolStatus               *contracts.ToolExecutionStatus
	ToolErrorCode            *contracts.ErrorCode
	ToolSideEffectUnknown    bool
	EndedAt                  time.Time
	PreserveExecutionEndedAt bool
}

// TerminationRepository 在调用方事务内锁定并原子提交 Cancel/Timeout 的完整数据库事实。
type TerminationRepository interface {
	LockTerminationFacts(context.Context, contracts.RuntimeWriteTx, contracts.TaskID) (TerminationFacts, error)
	ApplyTermination(context.Context, contracts.RuntimeWriteTx, ApplyTerminationRequest) (bool, error)
}
