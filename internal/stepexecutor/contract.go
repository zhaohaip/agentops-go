package stepexecutor

import (
	"bytes"
	"context"
	"encoding/json"

	"github.com/zhaohaip/agentops-go/internal/contracts"
)

// StepExecutorPort 执行一个由冻结 Checkpoint 明确指定的 Step 动作。
type StepExecutorPort interface {
	ExecuteStep(context.Context, StepExecutionRequest) (StepOutcome, error)
}

// Port 是 StepExecutorPort 在模块内部使用的短名称。
type Port = StepExecutorPort

// StepExecutionRequest 是 Task Runtime 传给 Step Executor 的不可变应用投影。
//
// 请求只携带一次 Step 动作需要的值，不包含 Repository、事务或外部 SDK 类型。
type StepExecutionRequest struct {
	Scope                      contracts.ExecutionScope
	NextAction                 contracts.CheckpointNextAction
	Step                       StepExecutionProjection
	PreviousStep               *PreviousStepProjection
	ResolvedReferences         contracts.CanonicalResolvedReferences
	Agent                      *AgentProjection
	AgentAuthorization         *contracts.AgentAuthorization
	ToolCapability             *contracts.StaticToolDefinition
	ApprovedAction             *contracts.ApprovedAction
	ApprovedCheckpointEvidence *contracts.ApprovedCheckpointEvidence
}

// StepExecutionProjection 是当前 Step 的不可变执行投影。
type StepExecutionProjection struct {
	StepID       contracts.StepID
	RunID        contracts.RunID
	PlanID       contracts.PlanID
	Sequence     uint32
	Type         contracts.StepType
	Name         string
	Input        json.RawMessage
	OutputSchema contracts.OutputSchema
	ToolName     contracts.ToolName
	Status       contracts.StepStatus
}

// PreviousStepProjection 是紧邻前序 Step 的安全结果投影。
type PreviousStepProjection struct {
	StepID       contracts.StepID
	Sequence     uint32
	Status       contracts.StepStatus
	SafeOutput   json.RawMessage
	OutputSchema contracts.OutputSchema
}

// AgentProjection 是模型 Step 使用的冻结 Agent 和 Model 配置投影。
type AgentProjection struct {
	AgentID          contracts.AgentID
	SystemPrompt     string
	ModelName        string
	GenerationParams contracts.GenerationParams
}

// StepOutcome 是 ExecuteStep 的封闭结果联合。
type StepOutcome interface {
	Kind() contracts.StepOutcomeKind
	isStepOutcome()
}

// StepContinuation 描述 Completed 后唯一允许的推进方式。
type StepContinuation struct {
	Kind       contracts.StepContinuationKind
	NextStepID contracts.StepID
}

// Valid 报告后续动作的联合载荷是否完整。
func (c StepContinuation) Valid() bool {
	switch c.Kind {
	case contracts.StepContinuationNextStep:
		return c.NextStepID != ""
	case contracts.StepContinuationFinalizeRun:
		return c.NextStepID == ""
	default:
		return false
	}
}

// ToolResultUpdate 是 Task Runtime 结果事务要提交的 ToolExecution 终态草案。
type ToolResultUpdate struct {
	ToolExecutionID   contracts.ToolExecutionID
	Status            contracts.ToolExecutionStatus
	Output            json.RawMessage
	ErrorCode         *contracts.ErrorCode
	SideEffectUnknown bool
	Truncated         bool
	OriginalSize      *uint64
	OriginalCount     *uint64
}

// Valid 报告 ToolExecution 草案是否已经收敛到一个合法终态。
func (u ToolResultUpdate) Valid() bool {
	if u.ToolExecutionID == "" || !u.Status.Terminal() {
		return false
	}
	switch u.Status {
	case contracts.ToolExecutionStatusCompleted:
		return validOptionalOutcomeJSONObject(u.Output) && u.ErrorCode == nil && !u.SideEffectUnknown
	case contracts.ToolExecutionStatusFailed:
		return u.Output == nil && validErrorCode(u.ErrorCode) && !u.SideEffectUnknown
	case contracts.ToolExecutionStatusUnknown:
		return u.Output == nil && validErrorCode(u.ErrorCode) && u.SideEffectUnknown
	default:
		return false
	}
}

// StepOutcomeCompleted 表示已经取得安全且确定的 Step 结果。
type StepOutcomeCompleted struct {
	SafeOutput       json.RawMessage
	ToolExecutionID  *contracts.ToolExecutionID
	ToolResultUpdate *ToolResultUpdate
	Continuation     StepContinuation
}

// Kind 返回共享 Completed 分支标识。
func (StepOutcomeCompleted) Kind() contracts.StepOutcomeKind { return contracts.StepOutcomeCompleted }
func (StepOutcomeCompleted) isStepOutcome()                  {}

// StepOutcomeWaitingApproval 表示 Approval Manager 已提交完整等待现场。
type StepOutcomeWaitingApproval struct {
	ApprovalID contracts.ApprovalID
}

// Kind 返回共享 WaitingApproval 分支标识。
func (StepOutcomeWaitingApproval) Kind() contracts.StepOutcomeKind {
	return contracts.StepOutcomeWaitingApproval
}
func (StepOutcomeWaitingApproval) isStepOutcome() {}

// StepOutcomeTerminalized 表示 Approval Manager 已提交 CheckpointInvalid 终态。
type StepOutcomeTerminalized struct {
	TaskID           contracts.TaskID
	ExecutionVersion contracts.ExecutionVersion
	ErrorCode        contracts.ErrorCode
	ReportStatus     contracts.ReportStatus
}

// Kind 返回共享 Terminalized 分支标识。
func (StepOutcomeTerminalized) Kind() contracts.StepOutcomeKind {
	return contracts.StepOutcomeTerminalized
}
func (StepOutcomeTerminalized) isStepOutcome() {}

// StepOutcomeFailed 表示当前 Step 的确定业务失败。
type StepOutcomeFailed struct {
	ErrorCode         contracts.ErrorCode
	CauseCode         CauseCode
	SafeSummary       string
	ToolExecutionID   *contracts.ToolExecutionID
	ToolResultUpdate  *ToolResultUpdate
	SideEffectUnknown bool
}

// Kind 返回共享 Failed 分支标识。
func (StepOutcomeFailed) Kind() contracts.StepOutcomeKind { return contracts.StepOutcomeFailed }
func (StepOutcomeFailed) isStepOutcome()                  {}

// StepOutcomeStale 表示执行事实或所有权已合法变化。
type StepOutcomeStale struct {
	CauseCode CauseCode
}

// Kind 返回共享 Stale 分支标识。
func (StepOutcomeStale) Kind() contracts.StepOutcomeKind { return contracts.StepOutcomeStale }
func (StepOutcomeStale) isStepOutcome()                  {}

// ValidateStepOutcome 校验封闭分支及其关联载荷。
func ValidateStepOutcome(outcome StepOutcome) bool {
	switch value := outcome.(type) {
	case StepOutcomeCompleted:
		return validOutcomeJSONObject(value.SafeOutput) && value.Continuation.Valid() &&
			validCompletedToolOutcome(value.ToolExecutionID, value.ToolResultUpdate)
	case StepOutcomeWaitingApproval:
		return value.ApprovalID != ""
	case StepOutcomeTerminalized:
		return value.TaskID != "" && value.ExecutionVersion.Valid() &&
			value.ErrorCode == contracts.ErrorCodeCheckpointInvalid &&
			value.ReportStatus == contracts.ReportStatusPending
	case StepOutcomeFailed:
		return validFailedPair(value.ErrorCode, value.CauseCode) && value.SafeSummary != "" &&
			validToolOutcome(value.ToolExecutionID, value.ToolResultUpdate, value.SideEffectUnknown)
	case StepOutcomeStale:
		return value.CauseCode.Stale()
	default:
		return false
	}
}

func validCompletedToolOutcome(
	toolExecutionID *contracts.ToolExecutionID,
	update *ToolResultUpdate,
) bool {
	return validToolOutcome(toolExecutionID, update, false) &&
		(update == nil || update.Status == contracts.ToolExecutionStatusCompleted)
}

func validToolOutcome(
	toolExecutionID *contracts.ToolExecutionID,
	update *ToolResultUpdate,
	sideEffectUnknown bool,
) bool {
	if toolExecutionID == nil || *toolExecutionID == "" {
		return toolExecutionID == nil && update == nil && !sideEffectUnknown
	}
	return update != nil && update.Valid() && update.ToolExecutionID == *toolExecutionID &&
		update.SideEffectUnknown == sideEffectUnknown
}

func validErrorCode(code *contracts.ErrorCode) bool {
	return code != nil && code.Valid()
}

func validOptionalOutcomeJSONObject(value json.RawMessage) bool {
	return value == nil || validOutcomeJSONObject(value)
}

func validOutcomeJSONObject(value json.RawMessage) bool {
	trimmed := bytes.TrimSpace(value)
	return len(trimmed) > 1 && trimmed[0] == '{' && json.Valid(trimmed)
}
