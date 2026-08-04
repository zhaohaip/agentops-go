package contracts

// ReferenceActionMode 表示 next_action 对应的引用提取模式。
type ReferenceActionMode string

const (
	ReferenceActionModeTargetStepInput ReferenceActionMode = "TARGET_STEP_INPUT"
	ReferenceActionModeNoStepInput     ReferenceActionMode = "NO_STEP_INPUT"
)

// Valid 报告 ReferenceActionMode 是否属于封闭集合。
func (m ReferenceActionMode) Valid() bool {
	return m == ReferenceActionModeTargetStepInput || m == ReferenceActionModeNoStepInput
}

// ReferenceIssueCode 表示共享引用提取器的模块无关问题分类。
type ReferenceIssueCode string

const (
	ReferenceIssueCodeCountLimitExceeded ReferenceIssueCode = "REFERENCE_COUNT_LIMIT_EXCEEDED"
)

// Valid 报告 ReferenceIssueCode 是否属于封闭集合。
func (c ReferenceIssueCode) Valid() bool {
	return c == ReferenceIssueCodeCountLimitExceeded
}

// RuntimeContextV1 表示 Checkpoint 保存的最小执行位置。
type RuntimeContextV1 struct {
	SchemaVersion      uint32                      `json:"schema_version"`
	TaskID             TaskID                      `json:"task_id"`
	RunID              RunID                       `json:"run_id"`
	ExecutionVersion   ExecutionVersion            `json:"execution_version"`
	PlanID             *PlanID                     `json:"plan_id,omitempty"`
	CurrentStepID      *StepID                     `json:"current_step_id,omitempty"`
	NextAction         CheckpointNextAction        `json:"next_action"`
	ResolvedReferences CanonicalResolvedReferences `json:"resolved_references"`
	ApprovalContext    *ApprovalContext            `json:"approval_context,omitempty"`
}

// CanonicalResolvedReferences 表示按共享线协议排序且去重的引用列表。
type CanonicalResolvedReferences []ResolvedReference

// ResolvedReference 表示目标 Step 输入到前序 Step 输出字段的绑定。
type ResolvedReference struct {
	TargetPath        []ReferencePathSegment `json:"target_path"`
	SourceStepID      StepID                 `json:"source_step_id"`
	SourceOutputField string                 `json:"source_output_field"`
}

// ReferencePathSegmentKind 表示结构化目标路径片段种类。
type ReferencePathSegmentKind string

const (
	ReferencePathSegmentKey   ReferencePathSegmentKind = "key"
	ReferencePathSegmentIndex ReferencePathSegmentKind = "index"
)

// Valid 报告 ReferencePathSegmentKind 是否属于封闭集合。
func (k ReferencePathSegmentKind) Valid() bool {
	return k == ReferencePathSegmentKey || k == ReferencePathSegmentIndex
}

// ReferencePathSegment 表示 target_path 中的 key 或 index 片段。
type ReferencePathSegment struct {
	Kind  ReferencePathSegmentKind `json:"kind"`
	Key   *string                  `json:"key,omitempty"`
	Index *uint64                  `json:"index,omitempty"`
}

// ApprovalContext 表示 Runtime Context 中直接引用 Approval 的最小冻结投影。
type ApprovalContext struct {
	ApprovalID               ApprovalID       `json:"approval_id"`
	ApprovalExecutionVersion ExecutionVersion `json:"approval_execution_version"`
	ToolName                 ToolName         `json:"tool_name"`
	FrozenToolInput          FrozenToolInput  `json:"frozen_tool_input"`
	ObservedValues           ObservedValues   `json:"observed_values"`
	ResourceVersion          ResourceVersion  `json:"resource_version"`
	FrozenInputHash          FrozenInputHash  `json:"frozen_input_hash"`
}
