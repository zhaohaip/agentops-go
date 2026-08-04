package contracts

const (
	// ExecutionConfigSchemaV1 是 ExecutionConfigV1 固定 schema。
	ExecutionConfigSchemaV1 = "agentops.execution-config"
	// ExecutionConfigVersionV1 是 ExecutionConfigV1 固定版本。
	ExecutionConfigVersionV1 uint32 = 1
)

// StepType 表示顺序 Plan 支持的 Step 类型。
type StepType string

const (
	StepTypeAnalysis     StepType = "Analysis"
	StepTypeModelCall    StepType = "ModelCall"
	StepTypeToolCall     StepType = "ToolCall"
	StepTypeVerification StepType = "Verification"
)

// Valid 报告 StepType 是否属于封闭集合。
func (t StepType) Valid() bool {
	switch t {
	case StepTypeAnalysis, StepTypeModelCall, StepTypeToolCall, StepTypeVerification:
		return true
	default:
		return false
	}
}

// RiskLevel 表示 Tool 风险等级。
type RiskLevel string

const (
	RiskLevelLow  RiskLevel = "Low"
	RiskLevelHigh RiskLevel = "High"
)

// Valid 报告 RiskLevel 是否属于封闭集合。
func (l RiskLevel) Valid() bool {
	return l == RiskLevelLow || l == RiskLevelHigh
}

// ToolCapabilityKind 表示 MVP Tool 的稳定能力种类。
type ToolCapabilityKind string

const (
	ToolCapabilityK8sGetDeployment   ToolCapabilityKind = "K8S_GET_DEPLOYMENT"
	ToolCapabilityK8sGetPod          ToolCapabilityKind = "K8S_GET_POD"
	ToolCapabilityK8sGetEvent        ToolCapabilityKind = "K8S_GET_EVENT"
	ToolCapabilityK8sGetContainerLog ToolCapabilityKind = "K8S_GET_CONTAINER_LOG"
	ToolCapabilityK8sPatchDeployment ToolCapabilityKind = "K8S_PATCH_DEPLOYMENT"
)

// Valid 报告 ToolCapabilityKind 是否属于 MVP 封闭集合。
func (k ToolCapabilityKind) Valid() bool {
	switch k {
	case ToolCapabilityK8sGetDeployment, ToolCapabilityK8sGetPod, ToolCapabilityK8sGetEvent,
		ToolCapabilityK8sGetContainerLog, ToolCapabilityK8sPatchDeployment:
		return true
	default:
		return false
	}
}

// EventSortKey 表示 Event 有界结果的稳定排序键。
type EventSortKey string

const (
	EventSortKeyEventTimeDesc EventSortKey = "event_time_desc"
	EventSortKeyNamespaceAsc  EventSortKey = "namespace_asc"
	EventSortKeyNameAsc       EventSortKey = "name_asc"
	EventSortKeyUIDAsc        EventSortKey = "uid_asc"
)

// Valid 报告 EventSortKey 是否属于封闭集合。
func (k EventSortKey) Valid() bool {
	switch k {
	case EventSortKeyEventTimeDesc, EventSortKeyNamespaceAsc, EventSortKeyNameAsc, EventSortKeyUIDAsc:
		return true
	default:
		return false
	}
}

// GenerationParams 表示共享 Model Client V1 生成参数。
type GenerationParams struct {
	Temperature     CanonicalDecimalV1 `json:"temperature"`
	TopP            CanonicalDecimalV1 `json:"top_p"`
	MaxOutputTokens uint32             `json:"max_output_tokens"`
}

// ExecutionConfigV1 表示执行语义和安全边界的唯一共享配置值。
type ExecutionConfigV1 struct {
	Schema        string                         `json:"schema"`
	Version       uint32                         `json:"version"`
	Agent         AgentExecutionConfigV1         `json:"agent"`
	Model         ModelExecutionConfigV1         `json:"model"`
	JSON          JSONExecutionContractV1        `json:"json"`
	Safety        SafetyExecutionContractV1      `json:"safety"`
	Planner       PlannerExecutionConfigV1       `json:"planner"`
	StepExecutor  StepExecutorExecutionConfigV1  `json:"step_executor"`
	ToolFramework ToolFrameworkExecutionConfigV1 `json:"tool_framework"`
	Checkpoint    CheckpointExecutionConfigV1    `json:"checkpoint"`
	Approval      ApprovalExecutionConfigV1      `json:"approval"`
}

// AgentExecutionConfigV1 表示 Agent 的执行语义投影。
type AgentExecutionConfigV1 struct {
	AgentID           AgentID    `json:"agent_id"`
	Enabled           bool       `json:"enabled"`
	SystemInstruction string     `json:"system_instruction"`
	AllowedTools      []ToolName `json:"allowed_tools"`
	MaxSteps          uint32     `json:"max_steps"`
}

// ModelExecutionConfigV1 表示 Model Client 的执行语义投影。
type ModelExecutionConfigV1 struct {
	ModelName                     string           `json:"model_name"`
	Stream                        bool             `json:"stream"`
	ResponseFormat                string           `json:"response_format"`
	ModelClientContractVersion    string           `json:"model_client_contract_version"`
	GenerationParamsSchemaVersion uint32           `json:"generation_params_schema_version"`
	GenerationParams              GenerationParams `json:"generation_params"`
}

// JSONExecutionContractV1 表示共享 JSON 解析和规范约束。
type JSONExecutionContractV1 struct {
	CanonicalizationVersion string `json:"canonicalization_version"`
	MaxDepth                uint32 `json:"max_depth"`
	MaxObjectFields         uint32 `json:"max_object_fields"`
	RejectDuplicateKeys     bool   `json:"reject_duplicate_keys"`
	RejectNull              bool   `json:"reject_null"`
}

// SafetyExecutionContractV1 表示共享安全输出约束。
type SafetyExecutionContractV1 struct {
	SanitizationRuleVersion string `json:"sanitization_rule_version"`
	SafeSummaryMaxBytes     uint32 `json:"safe_summary_max_bytes"`
	LogStringMaxBytes       uint32 `json:"log_string_max_bytes"`
}

// PlannerExecutionConfigV1 表示 Planner 的冻结协议和限制。
type PlannerExecutionConfigV1 struct {
	ContractVersion             string          `json:"contract_version"`
	PlanSchemaVersion           uint32          `json:"plan_schema_version"`
	NonToolInputContractVersion string          `json:"non_tool_input_contract_version"`
	ToolSchemaSubsetVersion     string          `json:"tool_schema_subset_version"`
	RepairPolicyVersion         string          `json:"repair_policy_version"`
	AllowedStepTypes            []StepType      `json:"allowed_step_types"`
	FinalStepType               StepType        `json:"final_step_type"`
	SequenceStart               uint32          `json:"sequence_start"`
	RequiresContiguousSequence  bool            `json:"requires_contiguous_sequence"`
	MaxRepairs                  uint32          `json:"max_repairs"`
	Limits                      PlannerLimitsV1 `json:"limits"`
}

// PlannerLimitsV1 表示 Planner 的全部冻结资源上限。
type PlannerLimitsV1 struct {
	MaxTaskInputBytes              uint64 `json:"max_task_input_bytes"`
	MaxAgentPromptBytes            uint64 `json:"max_agent_prompt_bytes"`
	MaxToolDescriptionBytes        uint64 `json:"max_tool_description_bytes"`
	MaxToolSchemaBytes             uint64 `json:"max_tool_schema_bytes"`
	MaxPlanningTools               uint64 `json:"max_planning_tools"`
	MaxInitialPromptBytes          uint64 `json:"max_initial_prompt_bytes"`
	MaxRepairPromptBytes           uint64 `json:"max_repair_prompt_bytes"`
	MaxModelResponseBytes          uint64 `json:"max_model_response_bytes"`
	MaxPlanSteps                   uint64 `json:"max_plan_steps"`
	MaxPlanDraftBytes              uint64 `json:"max_plan_draft_bytes"`
	MaxStepNameBytes               uint64 `json:"max_step_name_bytes"`
	MaxGoalBytes                   uint64 `json:"max_goal_bytes"`
	MaxStepInputBytes              uint64 `json:"max_step_input_bytes"`
	MaxResolvedReferencesPerStep   uint64 `json:"max_resolved_references_per_step"`
	MaxOutputFields                uint64 `json:"max_output_fields"`
	MaxOutputFieldNameBytes        uint64 `json:"max_output_field_name_bytes"`
	MaxValidationIssues            uint64 `json:"max_validation_issues"`
	MaxRepairCandidateSummaryBytes uint64 `json:"max_repair_candidate_summary_bytes"`
	PlannerModelCallTimeoutMS      uint64 `json:"planner_model_call_timeout_ms"`
	RepairMinModelBudgetMS         uint64 `json:"repair_min_model_budget_ms"`
	PlannerLocalSafetyMarginMS     uint64 `json:"planner_local_safety_margin_ms"`
}

// StepExecutorExecutionConfigV1 表示 Step Executor 的冻结协议和限制。
type StepExecutorExecutionConfigV1 struct {
	ContractVersion            string               `json:"contract_version"`
	StepInputContractVersion   string               `json:"step_input_contract_version"`
	ReferenceProtocolVersion   string               `json:"reference_protocol_version"`
	ReferenceActionModeVersion string               `json:"reference_action_mode_version"`
	OutputSchemaVersion        string               `json:"output_schema_version"`
	Limits                     StepExecutorLimitsV1 `json:"limits"`
}

// StepExecutorLimitsV1 表示 Step Executor 的全部冻结资源上限。
type StepExecutorLimitsV1 struct {
	MaxResolvedStepInputBytes    uint64 `json:"max_resolved_step_input_bytes"`
	MaxStepOutputBytes           uint64 `json:"max_step_output_bytes"`
	MaxModelPromptBytes          uint64 `json:"max_model_prompt_bytes"`
	MaxModelResponseBytes        uint64 `json:"max_model_response_bytes"`
	MaxResolvedReferencesPerStep uint64 `json:"max_resolved_references_per_step"`
	MaxTargetPathDepth           uint64 `json:"max_target_path_depth"`
}

// ToolFrameworkExecutionConfigV1 表示 Tool Framework 的冻结协议和策略。
type ToolFrameworkExecutionConfigV1 struct {
	ContractVersion       string             `json:"contract_version"`
	ResultContractVersion string             `json:"result_contract_version"`
	Tools                 []ToolDefinitionV1 `json:"tools"`
	AccessPolicy          ToolAccessPolicyV1 `json:"access_policy"`
	ResultLimits          ToolResultLimitsV1 `json:"result_limits"`
	EventPolicy           EventPolicyV1      `json:"event_policy"`
	PatchPolicy           PatchPolicyV1      `json:"patch_policy"`
}

// ToolDefinitionV1 表示一个冻结 Tool 定义。
type ToolDefinitionV1 struct {
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

// ToolAccessPolicyV1 表示 Tool 的静态访问策略。
type ToolAccessPolicyV1 struct {
	Clusters               []ClusterPolicyV1 `json:"clusters"`
	ReplicasPolicy         ReplicasPolicyV1  `json:"replicas_policy"`
	ImageRegistryAllowlist []string          `json:"image_registry_allowlist"`
}

// ClusterPolicyV1 表示一个 Cluster 的命名空间和资源权限。
type ClusterPolicyV1 struct {
	ClusterID  string             `json:"cluster_id"`
	Namespaces []string           `json:"namespaces"`
	Resources  []ResourcePolicyV1 `json:"resources"`
}

// ResourcePolicyV1 表示一种 Kubernetes Resource 的允许操作。
type ResourcePolicyV1 struct {
	Kind        string   `json:"kind"`
	Verbs       []string `json:"verbs"`
	WriteFields []string `json:"write_fields"`
}

// ReplicasPolicyV1 表示 Deployment replicas 修改边界。
type ReplicasPolicyV1 struct {
	Enabled bool  `json:"enabled"`
	Min     int64 `json:"min"`
	Max     int64 `json:"max"`
}

// ToolResultLimitsV1 表示 Tool 原始响应和安全 DTO 的资源上限。
type ToolResultLimitsV1 struct {
	RawResponseMaxBytes      uint64 `json:"raw_response_max_bytes"`
	SafeDTOMaxBytes          uint64 `json:"safe_dto_max_bytes"`
	PodPageLimit             uint32 `json:"pod_page_limit"`
	EventPageLimit           uint32 `json:"event_page_limit"`
	ContainerLogDefaultLines uint32 `json:"container_log_default_lines"`
	ContainerLogMaxLines     uint32 `json:"container_log_max_lines"`
}

// EventPolicyV1 表示 Event 有界读取和稳定排序策略。
type EventPolicyV1 struct {
	Version              string         `json:"version"`
	SortKeys             []EventSortKey `json:"sort_keys"`
	CandidateBudgetBytes uint64         `json:"candidate_budget_bytes"`
	ReserveBytes         uint64         `json:"reserve_bytes"`
	FollowContinue       bool           `json:"follow_continue"`
}

// PatchPolicyV1 表示 Deployment Patch 的冻结安全策略。
type PatchPolicyV1 struct {
	Version                       string   `json:"version"`
	ResponseClassificationVersion string   `json:"response_classification_version"`
	ResourceVersionTestRequired   bool     `json:"resource_version_test_required"`
	AllowedWriteFields            []string `json:"allowed_write_fields"`
}

// CheckpointExecutionConfigV1 表示 Checkpoint 的冻结协议限制。
type CheckpointExecutionConfigV1 struct {
	ContractVersion                  string `json:"contract_version"`
	RuntimeContextSchemaVersion      uint32 `json:"runtime_context_schema_version"`
	ResolvedReferenceProtocolVersion string `json:"resolved_reference_protocol_version"`
	ActionModeVersion                string `json:"action_mode_version"`
	MaxResolvedReferencesPerStep     uint32 `json:"max_resolved_references_per_step"`
	MaxTargetPathDepth               uint32 `json:"max_target_path_depth"`
}

// ApprovalExecutionConfigV1 表示 Approval 的冻结策略。
type ApprovalExecutionConfigV1 struct {
	PolicyVersion         string    `json:"policy_version"`
	RequiredRiskLevel     RiskLevel `json:"required_risk_level"`
	RequiredReadOnly      bool      `json:"required_read_only"`
	FreezeResourceVersion bool      `json:"freeze_resource_version"`
}
