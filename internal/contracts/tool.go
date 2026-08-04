package contracts

import "context"

const (
	// FrozenApprovedToolInputSchemaV1 是冻结审批输入的固定 schema。
	FrozenApprovedToolInputSchemaV1 = "agentops.frozen-approved-tool-input"
	// FrozenApprovedToolInputVersionV1 是冻结审批输入的固定版本。
	FrozenApprovedToolInputVersionV1 uint32 = 1
)

// ToolFrameworkPort 是 Tool Framework 的三个共享执行入口。
type ToolFrameworkPort interface {
	InvokeReadTool(context.Context, ReadToolRequest) (ToolFrameworkResult, error)
	PrepareWriteApproval(context.Context, PrepareWriteApprovalRequest) (ToolFrameworkResult, error)
	InvokeApprovedWrite(context.Context, ApprovedWriteRequest) (ToolFrameworkResult, error)
}

// AgentAuthorization 表示当前执行配置中 Agent 的最小 Tool 授权投影。
type AgentAuthorization struct {
	AgentID      AgentID    `json:"agent_id"`
	AllowedTools []ToolName `json:"allowed_tools"`
}

// StaticToolDefinition 表示当前 execution_config_hash 对应的静态 Tool 投影。
type StaticToolDefinition struct {
	Name           ToolName            `json:"name"`
	Enabled        bool                `json:"enabled"`
	Description    string              `json:"description"`
	CapabilityKind ToolCapabilityKind  `json:"capability_kind"`
	InputSchema    CanonicalJSONSchema `json:"input_schema"`
	OutputSchema   CanonicalJSONSchema `json:"output_schema"`
	RiskLevel      RiskLevel           `json:"risk_level"`
	ReadOnly       bool                `json:"read_only"`
	TimeoutMS      uint64              `json:"timeout_ms"`
}

// ReadToolRequest 表示只读 Tool 调用请求。
type ReadToolRequest struct {
	Scope          ExecutionScope       `json:"scope"`
	Authorization  AgentAuthorization   `json:"authorization"`
	ToolName       ToolName             `json:"tool_name"`
	ResolvedInput  ResolvedToolInput    `json:"resolved_input"`
	ToolDefinition StaticToolDefinition `json:"tool_definition"`
}

// PrepareWriteApprovalRequest 表示高风险写 Tool 的审批准备请求。
type PrepareWriteApprovalRequest struct {
	Scope          ExecutionScope       `json:"scope"`
	Authorization  AgentAuthorization   `json:"authorization"`
	ToolName       ToolName             `json:"tool_name"`
	ResolvedInput  ResolvedToolInput    `json:"resolved_input"`
	ToolDefinition StaticToolDefinition `json:"tool_definition"`
}

// ApprovedWriteRequest 表示已经具备 Approval 和 Checkpoint 直接证据的写请求。
type ApprovedWriteRequest struct {
	Scope              ExecutionScope             `json:"scope"`
	Authorization      AgentAuthorization         `json:"authorization"`
	ApprovedAction     ApprovedAction             `json:"approved_action"`
	CheckpointEvidence ApprovedCheckpointEvidence `json:"checkpoint_evidence"`
	ToolDefinition     StaticToolDefinition       `json:"tool_definition"`
}

// ToolTarget 表示 FrozenToolRequest 中允许公开的资源目标。
type ToolTarget struct {
	Cluster    string  `json:"cluster"`
	Namespace  string  `json:"namespace"`
	Deployment string  `json:"deployment"`
	Container  *string `json:"container,omitempty"`
}

// FrozenToolRequest 表示 Tool Framework 构造的完整不可变审批请求。
type FrozenToolRequest struct {
	TaskID              TaskID              `json:"task_id"`
	RunID               RunID               `json:"run_id"`
	ExecutionVersion    ExecutionVersion    `json:"execution_version"`
	StepID              StepID              `json:"step_id"`
	ToolName            ToolName            `json:"tool_name"`
	RiskLevel           RiskLevel           `json:"risk_level"`
	ReadOnly            bool                `json:"read_only"`
	FrozenInput         FrozenToolInput     `json:"frozen_input"`
	Target              ToolTarget          `json:"target"`
	ObservedValues      ObservedValues      `json:"observed_values"`
	ResourceVersion     ResourceVersion     `json:"resource_version"`
	SafeSummary         string              `json:"safe_summary"`
	ExecutionConfigHash ExecutionConfigHash `json:"execution_config_hash"`
	FrozenInputHash     FrozenInputHash     `json:"frozen_input_hash"`
}

// FrozenApprovedToolInputV1 表示 frozen_input_hash 的唯一输入 DTO。
type FrozenApprovedToolInputV1 struct {
	Schema          string          `json:"schema"`
	Version         uint32          `json:"version"`
	ToolName        ToolName        `json:"tool_name"`
	ToolInput       FrozenToolInput `json:"tool_input"`
	ObservedValues  ObservedValues  `json:"observed_values"`
	ResourceVersion ResourceVersion `json:"resource_version"`
}

// ApprovedAction 表示不可变 Approved Approval 的执行投影。
type ApprovedAction struct {
	ApprovalID               ApprovalID          `json:"approval_id"`
	ApprovalExecutionVersion ExecutionVersion    `json:"approval_execution_version"`
	ApprovalStatus           ApprovalStatus      `json:"approval_status"`
	ExecutionConfigHash      ExecutionConfigHash `json:"execution_config_hash"`
	FrozenInputHash          FrozenInputHash     `json:"frozen_input_hash"`
	TaskID                   TaskID              `json:"task_id"`
	RunID                    RunID               `json:"run_id"`
	StepID                   StepID              `json:"step_id"`
	ToolName                 ToolName            `json:"tool_name"`
	FrozenInput              FrozenToolInput     `json:"frozen_input"`
	ObservedValues           ObservedValues      `json:"observed_values"`
	ResourceVersion          ResourceVersion     `json:"resource_version"`
}

// ApprovedCheckpointType 表示批准执行所依据的 Checkpoint 类型。
type ApprovedCheckpointType string

const (
	ApprovedCheckpointTypeContinuation  ApprovedCheckpointType = "APPROVED_CONTINUATION"
	ApprovedCheckpointTypeRecoveryStart ApprovedCheckpointType = "RECOVERY_START"
)

// Valid 报告 ApprovedCheckpointType 是否属于封闭集合。
func (t ApprovedCheckpointType) Valid() bool {
	return t == ApprovedCheckpointTypeContinuation || t == ApprovedCheckpointTypeRecoveryStart
}

// ApprovedCheckpointEvidence 表示当前最大有效 Checkpoint 的批准直接证据。
type ApprovedCheckpointEvidence struct {
	CheckpointID           CheckpointID           `json:"checkpoint_id"`
	ApprovalID             ApprovalID             `json:"approval_id"`
	ExecutionVersion       ExecutionVersion       `json:"execution_version"`
	CheckpointType         ApprovedCheckpointType `json:"checkpoint_type"`
	SourceExecutionVersion *ExecutionVersion      `json:"source_execution_version,omitempty"`
	SourceCheckpointID     *CheckpointID          `json:"source_checkpoint_id,omitempty"`
	ExecutionConfigHash    ExecutionConfigHash    `json:"execution_config_hash"`
	FrozenInputHash        FrozenInputHash        `json:"frozen_input_hash"`
}

// ToolFrameworkResult 是 Tool Framework 的封闭结果。
type ToolFrameworkResult interface {
	isToolFrameworkResult()
}

// ToolInvocationCompleted 表示 Tool 外部调用已取得明确成功。
type ToolInvocationCompleted struct {
	ToolExecutionID ToolExecutionID `json:"tool_execution_id"`
	Output          SafeToolOutput  `json:"output"`
	Truncated       bool            `json:"truncated"`
	OriginalSize    *uint64         `json:"original_size,omitempty"`
	OriginalCount   *uint64         `json:"original_count,omitempty"`
	ProcessingError *ErrorCode      `json:"processing_error,omitempty"`
}

func (ToolInvocationCompleted) isToolFrameworkResult() {}

// ToolApprovalPrepared 表示冻结审批现场已构造完成。
type ToolApprovalPrepared struct {
	FrozenToolRequest FrozenToolRequest `json:"frozen_tool_request"`
}

func (ToolApprovalPrepared) isToolFrameworkResult() {}

// ToolPreflightRejected 表示外部 Tool 边界前的确定拒绝。
type ToolPreflightRejected struct {
	ErrorCode   ErrorCode `json:"error_code"`
	SafeSummary string    `json:"safe_summary"`
}

func (ToolPreflightRejected) isToolFrameworkResult() {}

// ToolBusinessFailed 表示 Tool 的确定业务或 Provider 失败。
type ToolBusinessFailed struct {
	ErrorCode           ErrorCode            `json:"error_code"`
	SafeSummary         string               `json:"safe_summary"`
	ToolExecutionID     *ToolExecutionID     `json:"tool_execution_id,omitempty"`
	ToolExecutionStatus *ToolExecutionStatus `json:"tool_execution_status,omitempty"`
}

func (ToolBusinessFailed) isToolFrameworkResult() {}

// ToolSideEffectUnknown 表示写 Tool 外部结果无法确认。
type ToolSideEffectUnknown struct {
	ToolExecutionID   ToolExecutionID `json:"tool_execution_id"`
	ErrorCode         ErrorCode       `json:"error_code"`
	SafeSummary       string          `json:"safe_summary"`
	SideEffectUnknown bool            `json:"side_effect_unknown"`
}

func (ToolSideEffectUnknown) isToolFrameworkResult() {}

// ToolCheckpointInvalid 表示可安全归属但无效的 Checkpoint。
type ToolCheckpointInvalid struct {
	ReasonCode ReasonCode `json:"reason_code"`
}

func (ToolCheckpointInvalid) isToolFrameworkResult() {}

// ToolDeadlineExceeded 表示数据库权威 Task deadline 已到。
type ToolDeadlineExceeded struct {
	CauseCode CauseCode `json:"cause_code"`
}

func (ToolDeadlineExceeded) isToolFrameworkResult() {}

// ToolStale 表示版本、所有权或状态已经合法变化。
type ToolStale struct {
	ReasonCode      ReasonCode       `json:"reason_code"`
	ToolExecutionID *ToolExecutionID `json:"tool_execution_id,omitempty"`
}

func (ToolStale) isToolFrameworkResult() {}

// ToolRuntimeFatal 表示可确定的共享契约或持久化不变量破坏。
type ToolRuntimeFatal struct {
	ErrorCode     ErrorCode `json:"error_code"`
	SafeCauseCode CauseCode `json:"safe_cause_code"`
}

func (ToolRuntimeFatal) isToolFrameworkResult() {}
