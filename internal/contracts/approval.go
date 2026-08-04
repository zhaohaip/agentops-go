package contracts

import "context"

// ApprovalRequestPort 请求 Approval Manager 原子建立等待审批现场。
type ApprovalRequestPort interface {
	RequestApproval(
		ctx context.Context,
		command RequestApprovalCommand,
	) (ApprovalRequestResult, error)
}

// RequestApprovalCommand 表示 Step Executor 发起审批的共享命令。
type RequestApprovalCommand struct {
	Scope               ExecutionScope         `json:"scope"`
	FrozenRequest       FrozenToolRequest      `json:"frozen_request"`
	StepID              StepID                 `json:"step_id"`
	ExecutionConfigHash ExecutionConfigHash    `json:"execution_config_hash"`
	ApprovalContext     ApprovalRequestContext `json:"approval_context"`
}

// ApprovalRequestContext 表示审批路由所需的最小冻结上下文。
type ApprovalRequestContext struct {
	NextAction CheckpointNextAction `json:"next_action"`
	ToolName   ToolName             `json:"tool_name"`
	RiskLevel  RiskLevel            `json:"risk_level"`
	ReadOnly   bool                 `json:"read_only"`
}

// ApprovalRequestResult 是 RequestApproval 的封闭结果。
type ApprovalRequestResult interface {
	isApprovalRequestResult()
}

// ApprovalRequestPending 表示首次创建完整 WaitingApproval 现场。
type ApprovalRequestPending struct {
	ApprovalID       ApprovalID       `json:"approval_id"`
	ApprovalStatus   ApprovalStatus   `json:"approval_status"`
	TaskID           TaskID           `json:"task_id"`
	RunID            RunID            `json:"run_id"`
	StepID           StepID           `json:"step_id"`
	ExecutionVersion ExecutionVersion `json:"execution_version"`
}

func (ApprovalRequestPending) isApprovalRequestResult() {}

// ApprovalRequestExisting 表示完全相同的 WaitingApproval 现场已存在。
type ApprovalRequestExisting struct {
	ApprovalID       ApprovalID       `json:"approval_id"`
	ApprovalStatus   ApprovalStatus   `json:"approval_status"`
	TaskID           TaskID           `json:"task_id"`
	RunID            RunID            `json:"run_id"`
	StepID           StepID           `json:"step_id"`
	ExecutionVersion ExecutionVersion `json:"execution_version"`
}

func (ApprovalRequestExisting) isApprovalRequestResult() {}

// ApprovalRequestConflict 表示当前执行事实发生合法竞争。
type ApprovalRequestConflict struct {
	TaskID           TaskID           `json:"task_id"`
	ExecutionVersion ExecutionVersion `json:"execution_version"`
	CauseCode        CauseCode        `json:"cause_code"`
}

func (ApprovalRequestConflict) isApprovalRequestResult() {}

// ApprovalRequestCheckpointInvalid 表示审批入口已提交 CheckpointInvalid 终态。
type ApprovalRequestCheckpointInvalid struct {
	TaskID              TaskID              `json:"task_id"`
	RunID               RunID               `json:"run_id"`
	StepID              StepID              `json:"step_id"`
	ExecutionVersion    ExecutionVersion    `json:"execution_version"`
	ErrorCode           ErrorCode           `json:"error_code"`
	ReasonCode          ReasonCode          `json:"reason_code"`
	TaskExecutionStatus TaskExecutionStatus `json:"task_execution_status"`
	ReportStatus        ReportStatus        `json:"report_status"`
}

func (ApprovalRequestCheckpointInvalid) isApprovalRequestResult() {}

// ApprovalRequestRuntimeFatal 表示审批入口发现 Runtime 级不变量破坏。
type ApprovalRequestRuntimeFatal struct {
	ErrorCode ErrorCode `json:"error_code"`
	CauseCode CauseCode `json:"cause_code"`
	TaskID    *TaskID   `json:"task_id,omitempty"`
	StepID    *StepID   `json:"step_id,omitempty"`
}

func (ApprovalRequestRuntimeFatal) isApprovalRequestResult() {}
