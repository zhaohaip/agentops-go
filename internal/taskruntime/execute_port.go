package taskruntime

import (
	"context"
	"encoding/json"
	"time"

	"github.com/zhaohaip/agentops-go/internal/contracts"
)

// ExecutionPlan 是派发循环需要的最小 Plan 持久化投影。
type ExecutionPlan struct {
	PlanID contracts.PlanID
}

// ExecutionStep 是派发循环需要的最小 Step 持久化投影。
type ExecutionStep struct {
	StepID   contracts.StepID
	Sequence uint32
	Type     contracts.StepType
	Status   contracts.StepStatus
	Input    json.RawMessage
	ToolName contracts.ToolName
}

// ExecutionDispatchFacts 是每轮从数据库重新锁定的执行事实。
type ExecutionDispatchFacts struct {
	Task      Task
	Run       Run
	Execution TaskExecution
	Plan      *ExecutionPlan
	Step      *ExecutionStep
}

// PlanStepDraft 是 Planner 已校验 PlanDraft 中的一个顺序 Step。
type PlanStepDraft struct {
	StepID   contracts.StepID
	Sequence uint32
	Type     contracts.StepType
	Input    json.RawMessage
	ToolName contracts.ToolName
}

// ValidatedPlanDraft 是 Planner Port 返回的已完成静态校验的计划。
type ValidatedPlanDraft struct {
	PlanID contracts.PlanID
	Goal   string
	Steps  []PlanStepDraft
}

// PlannerRequest 是 Task Runtime 在动作开始事务提交后发送给 Planner 的请求。
type PlannerRequest struct {
	TaskID              contracts.TaskID
	RunID               contracts.RunID
	ExecutionVersion    contracts.ExecutionVersion
	WorkerID            contracts.WorkerID
	TaskInput           string
	DeadlineAt          time.Time
	ExecutionConfigHash contracts.ExecutionConfigHash
	ExecutionConfig     contracts.ExecutionConfigV1
	ToolCatalogSelector contracts.PlanningToolCatalogSelector
}

// PlannerOutcome 是 Planner 的封闭结果。
type PlannerOutcome interface {
	isPlannerOutcome()
}

type PlannerOutcomeCompleted struct{ Draft ValidatedPlanDraft }
type PlannerOutcomeFailed struct{ ErrorCode contracts.ErrorCode }

func (PlannerOutcomeCompleted) isPlannerOutcome() {}
func (PlannerOutcomeFailed) isPlannerOutcome()    {}

// PlannerPort 只负责生成并返回已校验 PlanDraft。
type PlannerPort interface {
	GeneratePlan(context.Context, PlannerRequest) (PlannerOutcome, error)
}

// StepExecutionRequest 将冻结 Checkpoint 动作原样交给 Step Executor。
type StepExecutionRequest struct {
	Scope              contracts.ExecutionScope
	NextAction         contracts.CheckpointNextAction
	Step               ExecutionStep
	ResolvedReferences contracts.CanonicalResolvedReferences
	ApprovalContext    *contracts.ApprovalContext
	ExecutionConfig    contracts.ExecutionConfigV1
}

// StepOutcome 是 Step Executor 模块载荷的封闭结果。
type StepOutcome interface {
	isStepOutcome()
}

type StepOutcomeCompleted struct {
	Output       json.RawMessage
	RunContext   json.RawMessage
	Continuation contracts.StepContinuationKind
	NextStepID   contracts.StepID
}
type StepOutcomeWaitingApproval struct{}
type StepOutcomeTerminalized struct{}
type StepOutcomeFailed struct{ ErrorCode contracts.ErrorCode }
type StepOutcomeStale struct{}

func (StepOutcomeCompleted) isStepOutcome()       {}
func (StepOutcomeWaitingApproval) isStepOutcome() {}
func (StepOutcomeTerminalized) isStepOutcome()    {}
func (StepOutcomeFailed) isStepOutcome()          {}
func (StepOutcomeStale) isStepOutcome()           {}

// StepExecutorPort 执行一个由冻结 next_action 明确指定的 Step。
type StepExecutorPort interface {
	ExecuteStep(context.Context, StepExecutionRequest) (StepOutcome, error)
}

// ExecutionActionGuard 是所有动作开始条件更新的数据库 Guard。
type ExecutionActionGuard struct {
	Claim        contracts.ExecutionClaim
	CheckpointID contracts.CheckpointID
	NextAction   contracts.CheckpointNextAction
	StepID       contracts.StepID
}

// ApplyPlannerCompletedRequest 原子保存 Plan、Steps 与下一 Checkpoint。
type ApplyPlannerCompletedRequest struct {
	Guard      ExecutionActionGuard
	Draft      ValidatedPlanDraft
	NextAction contracts.CheckpointNextAction
}

// ApplyStepCompletedRequest 原子保存 Step 结果、Run Context 与下一 Checkpoint。
type ApplyStepCompletedRequest struct {
	Guard      ExecutionActionGuard
	Outcome    StepOutcomeCompleted
	NextAction contracts.CheckpointNextAction
}

// TerminalizeCheckpointInvalidRequest 在仍归属于当前 Execution 时携带稳定的损坏原因和活动 Step。
//
// Port 实现必须在调用方事务内原子关闭 Task、Run、TaskExecution 和可确定的活动 Step，
// 清空 queued_at，并确保存在唯一 Pending Report。
type TerminalizeCheckpointInvalidRequest struct {
	Claim        contracts.ExecutionClaim
	ActiveStepID *contracts.StepID
	ReasonCode   contracts.ReasonCode
}

// ExecutionDispatchRepository 在调用方事务内锁定事实并执行带版本/所有权 Guard 的条件写。
type ExecutionDispatchRepository interface {
	LockExecutionDispatch(context.Context, contracts.RuntimeWriteTx, contracts.ExecutionClaim) (ExecutionDispatchFacts, error)
	LockStep(context.Context, contracts.RuntimeWriteTx, contracts.ExecutionClaim, contracts.StepID) (ExecutionStep, error)
	StartExecutionAction(context.Context, contracts.RuntimeWriteTx, ExecutionActionGuard) (bool, error)
	ApplyPlannerCompleted(context.Context, contracts.RuntimeWriteTx, ApplyPlannerCompletedRequest) (bool, error)
	ApplyStepCompleted(context.Context, contracts.RuntimeWriteTx, ApplyStepCompletedRequest) (bool, error)
	TerminalizeCheckpointInvalid(context.Context, contracts.RuntimeWriteTx, TerminalizeCheckpointInvalidRequest) (bool, error)
	TerminalizeExecution(context.Context, contracts.RuntimeWriteTx, ExecutionActionGuard, contracts.ErrorCode) (bool, error)
	FinalizeExecution(context.Context, contracts.RuntimeWriteTx, ExecutionActionGuard) (bool, error)
	// ConfirmExecutionWaitingApproval 仅在 Approval Manager 已原子形成完整等待审批现场时返回 true。
	ConfirmExecutionWaitingApproval(context.Context, contracts.RuntimeWriteTx, contracts.ExecutionClaim, contracts.StepID) (bool, error)
	ConfirmExecutionTerminal(context.Context, contracts.RuntimeWriteTx, contracts.ExecutionClaim) (bool, error)
}
