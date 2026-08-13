package contracts

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"math"
	"strings"
	"testing"
)

func TestExecutionVersionAndHashes(t *testing.T) {
	t.Parallel()

	for _, version := range []ExecutionVersion{1, math.MaxInt64} {
		if !version.Valid() {
			t.Fatalf("ExecutionVersion(%d) 应合法", version)
		}
	}
	for _, version := range []ExecutionVersion{0, -1} {
		if version.Valid() {
			t.Fatalf("ExecutionVersion(%d) 应非法", version)
		}
	}

	validHash := strings.Repeat("a", 64)
	for name, valid := range map[string]bool{
		"execution": ExecutionConfigHash(validHash).Valid(),
		"catalog":   CatalogSnapshotHash(validHash).Valid(),
		"frozen":    FrozenInputHash(validHash).Valid(),
	} {
		if !valid {
			t.Fatalf("%s hash 应合法", name)
		}
	}
	for _, invalid := range []string{
		"",
		strings.Repeat("a", 63),
		strings.Repeat("A", 64),
		strings.Repeat("g", 64),
	} {
		if ExecutionConfigHash(invalid).Valid() {
			t.Fatalf("ExecutionConfigHash(%q) 应非法", invalid)
		}
	}
}

func TestCanonicalDecimalV1(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		coefficient int64
		scale       uint32
		want        string
	}{
		{name: "zero", coefficient: 0, scale: 4, want: "0"},
		{name: "integer", coefficient: 10, scale: 1, want: "1"},
		{name: "fraction", coefficient: 200, scale: 3, want: "0.2"},
		{name: "small", coefficient: 2, scale: 4, want: "0.0002"},
		{name: "negative", coefficient: -1250, scale: 3, want: "-1.25"},
		{name: "minimum int64", coefficient: math.MinInt64, scale: 0, want: "-9223372036854775808"},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			value := NewCanonicalDecimalV1(test.coefficient, test.scale)
			if got := value.String(); got != test.want {
				t.Fatalf("String() = %q, want %q", got, test.want)
			}
			encoded, err := json.Marshal(value)
			if err != nil {
				t.Fatalf("Marshal() error = %v", err)
			}
			if got := string(encoded); got != test.want {
				t.Fatalf("Marshal() = %q, want %q", got, test.want)
			}

			var decoded CanonicalDecimalV1
			if err := json.Unmarshal(encoded, &decoded); err != nil {
				t.Fatalf("Unmarshal() error = %v", err)
			}
			if decoded != value {
				t.Fatalf("round trip = %#v, want %#v", decoded, value)
			}
		})
	}

	for _, invalid := range []string{
		"null", `""`, "1e-1", "1E1", "+1", "-0", "01", "1.", ".1", "0.20", "1.0",
	} {
		var value CanonicalDecimalV1
		if err := json.Unmarshal([]byte(invalid), &value); err == nil {
			t.Fatalf("Unmarshal(%q) 应失败", invalid)
		}
	}
}

func TestFrozenApprovedToolInputV1FixedVector(t *testing.T) {
	t.Parallel()

	value := FrozenApprovedToolInputV1{
		Schema:   FrozenApprovedToolInputSchemaV1,
		Version:  FrozenApprovedToolInputVersionV1,
		ToolName: "k8s.patch_deployment",
		ToolInput: FrozenToolInput(
			`{"cluster":"prod","deployment":"web","namespace":"default","replicas":3}`,
		),
		ObservedValues:  ObservedValues(`{"replicas":2}`),
		ResourceVersion: "12345",
	}

	encoded, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("Marshal() error = %v", err)
	}
	const wantJSON = `{"schema":"agentops.frozen-approved-tool-input","version":1,"tool_name":"k8s.patch_deployment","tool_input":{"cluster":"prod","deployment":"web","namespace":"default","replicas":3},"observed_values":{"replicas":2},"resource_version":"12345"}`
	if got := string(encoded); got != wantJSON {
		t.Fatalf("Marshal() = %s, want %s", got, wantJSON)
	}

	sum := sha256.Sum256(encoded)
	if got, want := hex.EncodeToString(sum[:]), "c33d13c983cc54ab1c906c40004b9c2a3ca2efba506ae8db4a12ddca1f4c70f4"; got != want {
		t.Fatalf("hash = %s, want %s", got, want)
	}
	computed, err := ComputeFrozenInputHashV1(value)
	if err != nil || computed != FrozenInputHash("c33d13c983cc54ab1c906c40004b9c2a3ca2efba506ae8db4a12ddca1f4c70f4") {
		t.Fatalf("ComputeFrozenInputHashV1() = %s, %v", computed, err)
	}
}

func TestExecutionConfigV1FixedVector(t *testing.T) {
	t.Parallel()

	const fixture = `{"schema":"agentops.execution-config","version":1,"agent":{"agent_id":"agent-default","enabled":true,"system_instruction":"You are AgentOps.","allowed_tools":["k8s.get_deployment"],"max_steps":20},"model":{"model_name":"deepseek-chat","stream":false,"response_format":"json_object","model_client_contract_version":"model-client-v1","generation_params_schema_version":1,"generation_params":{"temperature":0.2,"top_p":1,"max_output_tokens":4096}},"json":{"canonicalization_version":"agentops-json-v1","max_depth":16,"max_object_fields":64,"reject_duplicate_keys":true,"reject_null":true},"safety":{"sanitization_rule_version":"result-sanitization-v1","safe_summary_max_bytes":512,"log_string_max_bytes":256},"planner":{"contract_version":"planner-v1.3","plan_schema_version":1,"non_tool_input_contract_version":"non-tool-input-v1","tool_schema_subset_version":"tool-schema-subset-v1","repair_policy_version":"single-repair-v1","allowed_step_types":["Analysis","ModelCall","ToolCall","Verification"],"final_step_type":"Verification","sequence_start":1,"requires_contiguous_sequence":true,"max_repairs":1,"limits":{"max_task_input_bytes":16384,"max_agent_prompt_bytes":32768,"max_tool_description_bytes":4096,"max_tool_schema_bytes":65536,"max_planning_tools":32,"max_initial_prompt_bytes":262144,"max_repair_prompt_bytes":393216,"max_model_response_bytes":1048576,"max_plan_steps":20,"max_plan_draft_bytes":262144,"max_step_name_bytes":128,"max_goal_bytes":2048,"max_step_input_bytes":32768,"max_resolved_references_per_step":256,"max_output_fields":32,"max_output_field_name_bytes":64,"max_validation_issues":32,"max_repair_candidate_summary_bytes":65536,"planner_model_call_timeout_ms":60000,"repair_min_model_budget_ms":15000,"planner_local_safety_margin_ms":2000}},"step_executor":{"contract_version":"step-executor-v1","step_input_contract_version":"step-input-v1","reference_protocol_version":"step-output-ref-v1","reference_action_mode_version":"reference-action-mode-v1","output_schema_version":"output-schema-v1","limits":{"max_resolved_step_input_bytes":1048576,"max_step_output_bytes":1048576,"max_model_prompt_bytes":262144,"max_model_response_bytes":1048576,"max_resolved_references_per_step":256,"max_target_path_depth":16}},"tool_framework":{"contract_version":"tool-framework-v1","result_contract_version":"tool-framework-result-v1","tools":[{"name":"k8s.get_deployment","enabled":true,"description":"Get one Deployment.","capability_kind":"K8S_GET_DEPLOYMENT","input_schema":{"additionalProperties":false,"properties":{"cluster":{"type":"string"},"deployment":{"type":"string"},"namespace":{"type":"string"}},"required":["cluster","deployment","namespace"],"type":"object"},"output_schema":{"additionalProperties":false,"properties":{"name":{"type":"string"}},"required":["name"],"type":"object"},"risk_level":"Low","read_only":true,"timeout_ms":30000}],"access_policy":{"clusters":[{"cluster_id":"prod","namespaces":["default"],"resources":[{"kind":"Deployment","verbs":["get"],"write_fields":[]}]}],"replicas_policy":{"enabled":false,"min":0,"max":0},"image_registry_allowlist":[]},"result_limits":{"raw_response_max_bytes":1048576,"safe_dto_max_bytes":1048576,"pod_page_limit":200,"event_page_limit":200,"container_log_default_lines":200,"container_log_max_lines":1000},"event_policy":{"version":"bounded-event-page-v1","sort_keys":["event_time_desc","namespace_asc","name_asc","uid_asc"],"candidate_budget_bytes":983040,"reserve_bytes":65536,"follow_continue":false},"patch_policy":{"version":"deployment-patch-v1","response_classification_version":"patch-final-status-v1","resource_version_test_required":true,"allowed_write_fields":["image","replicas"]}},"checkpoint":{"contract_version":"checkpoint-v1.3","runtime_context_schema_version":1,"resolved_reference_protocol_version":"step-output-ref-v1","action_mode_version":"checkpoint-action-mode-v1","max_resolved_references_per_step":256,"max_target_path_depth":16},"approval":{"policy_version":"approval-policy-v1","required_risk_level":"High","required_read_only":false,"freeze_resource_version":true}}`

	decoder := json.NewDecoder(strings.NewReader(fixture))
	decoder.DisallowUnknownFields()
	var config ExecutionConfigV1
	if err := decoder.Decode(&config); err != nil {
		t.Fatalf("Decode() error = %v", err)
	}

	encoded, err := json.Marshal(config)
	if err != nil {
		t.Fatalf("Marshal() error = %v", err)
	}
	if got := string(encoded); got != fixture {
		t.Fatalf("Marshal() 未保持冻结字节\n got: %s\nwant: %s", got, fixture)
	}

	sum := sha256.Sum256(encoded)
	if got, want := hex.EncodeToString(sum[:]), "27f26b3d37da75facddaca5b9cbc23de91093977d75746fb5c53a0d92d257f43"; got != want {
		t.Fatalf("hash = %s, want %s", got, want)
	}
}

func TestRawJSONValueTypesRejectInvalidAndNull(t *testing.T) {
	t.Parallel()

	for name, target := range map[string]json.Unmarshaler{
		"resolved": new(ResolvedToolInput),
		"frozen":   new(FrozenToolInput),
		"observed": new(ObservedValues),
		"output":   new(SafeToolOutput),
	} {
		if err := target.UnmarshalJSON([]byte("null")); err == nil {
			t.Fatalf("%s 应拒绝 null", name)
		}
		if err := target.UnmarshalJSON([]byte("{")); err == nil {
			t.Fatalf("%s 应拒绝非法 JSON", name)
		}
	}
}

func TestRuntimeContextV1KeepsZeroIndex(t *testing.T) {
	t.Parallel()

	index := uint64(0)
	contextValue := RuntimeContextV1{
		SchemaVersion:    1,
		TaskID:           "task-1",
		RunID:            "run-1",
		ExecutionVersion: 1,
		NextAction:       CheckpointNextActionExecuteStep,
		ResolvedReferences: CanonicalResolvedReferences{
			{
				TargetPath: []ReferencePathSegment{
					{Kind: ReferencePathSegmentIndex, Index: &index},
				},
				SourceStepID:      "step-1",
				SourceOutputField: "items",
			},
		},
	}

	encoded, err := json.Marshal(contextValue)
	if err != nil {
		t.Fatalf("Marshal() error = %v", err)
	}
	if !bytes.Contains(encoded, []byte(`"index":0`)) {
		t.Fatalf("Marshal() = %s, 缺少零下标", encoded)
	}
	if bytes.Contains(encoded, []byte(`"key"`)) {
		t.Fatalf("Marshal() = %s, 不应包含 key", encoded)
	}
}

func TestEnumClosureAndTerminalSemantics(t *testing.T) {
	t.Parallel()

	allStatusValues := []interface{ Valid() bool }{
		TaskStatusPending, TaskStatusRunning, TaskStatusWaitingApproval, TaskStatusInterrupted,
		TaskStatusCompleted, TaskStatusFailed, TaskStatusCancelled,
		RunStatusPending, RunStatusRunning, RunStatusWaitingApproval, RunStatusCompleted, RunStatusFailed,
		StepStatusPending, StepStatusRunning, StepStatusWaitingApproval, StepStatusCompleted, StepStatusFailed,
		TaskExecutionStatusQueued, TaskExecutionStatusRunning, TaskExecutionStatusWaitingApproval,
		TaskExecutionStatusCompleted, TaskExecutionStatusFailed, TaskExecutionStatusInterrupted,
		ToolExecutionStatusRunning, ToolExecutionStatusCompleted, ToolExecutionStatusFailed, ToolExecutionStatusUnknown,
		ApprovalStatusPending, ApprovalStatusApproved, ApprovalStatusRejected,
		ReportStatusPending, ReportStatusGenerating, ReportStatusCompleted, ReportStatusFailed,
	}
	for _, value := range allStatusValues {
		if !value.Valid() {
			t.Fatalf("%T(%v) 应属于冻结状态集合", value, value)
		}
	}

	allErrorCodes := []ErrorCode{
		ErrorCodeTaskCancelled, ErrorCodeTaskTimeout, ErrorCodeConfigVersionMismatch,
		ErrorCodeDataInconsistent, ErrorCodeCheckpointInvalid, ErrorCodePlanGenerationFailed,
		ErrorCodePlanValidationFailed, ErrorCodeInputResolutionFailed, ErrorCodeModelCallFailed,
		ErrorCodeModelOutputInvalid, ErrorCodeResultSanitizationFailed, ErrorCodeStepOutputInvalid,
		ErrorCodeStepOutputTooLarge, ErrorCodeToolNotFound, ErrorCodeToolDisabled,
		ErrorCodeToolNotAuthorized, ErrorCodeToolInputInvalid, ErrorCodeToolAccessDenied,
		ErrorCodeToolTimeout, ErrorCodeToolConnectionLost, ErrorCodeToolCallFailed,
		ErrorCodeApprovalContextChanged, ErrorCodeApprovalRejected, ErrorCodeWriteToolInterrupted,
		ErrorCodeWorkerInterrupted, ErrorCodeResultPersistenceFailed, ErrorCodeReportGenerationFailed,
		ErrorCodeStepExecutorContractBroken, ErrorCodeRuntimeStaticToolSnapshotInconsistent,
		ErrorCodePersistenceInvariantViolation,
	}
	for _, code := range allErrorCodes {
		if !code.Valid() || !CauseCode(code).Valid() {
			t.Fatalf("%q 应同时是合法 error_code 和 cause_code", code)
		}
	}

	allCheckpointReasons := []ReasonCode{
		ReasonCodeCheckpointNotFound, ReasonCodeRuntimeContextMalformed,
		ReasonCodeRuntimeContextVersionUnsupported, ReasonCodeCheckpointAttributionMismatch,
		ReasonCodeCheckpointExecutionHashMismatch, ReasonCodeCheckpointTypeAmbiguous,
		ReasonCodeCheckpointSourceInvalid, ReasonCodeCheckpointPlanReferenceInvalid,
		ReasonCodeCheckpointStepReferenceInvalid, ReasonCodeCheckpointStepOutputReferenceInvalid,
		ReasonCodeCheckpointReferenceSyntaxInvalid, ReasonCodeCheckpointReferencePathInvalid,
		ReasonCodeCheckpointReferencePathTooDeep, ReasonCodeCheckpointReferenceLimitExceeded,
		ReasonCodeCheckpointReferenceDuplicateTarget, ReasonCodeCheckpointReferenceOrderInvalid,
		ReasonCodeCheckpointReferenceMissing, ReasonCodeCheckpointReferenceExtra,
		ReasonCodeCheckpointReferenceSourceInvalid, ReasonCodeCheckpointApprovalReferenceInvalid,
		ReasonCodeCheckpointFrozenActionMismatch, ReasonCodeCheckpointFrozenInputHashMismatch,
		ReasonCodeCheckpointNextActionInvalid,
	}
	for _, code := range allCheckpointReasons {
		if !code.ValidForCheckpointInvalid() {
			t.Fatalf("%q 应是合法 CheckpointInvalid reason_code", code)
		}
	}

	validValues := []interface{ Valid() bool }{
		TaskStatusPending,
		RunStatusPending,
		StepStatusPending,
		TaskExecutionStatusQueued,
		ToolExecutionStatusRunning,
		ApprovalStatusPending,
		ReportStatusPending,
		CheckpointNextActionGeneratePlan,
		ErrorCodeTaskCancelled,
		CauseCodeTaskCancelled,
		TerminationReasonCancelled,
		InvariantCodeCurrentExecutionInvalid,
		StepTypeAnalysis,
		RiskLevelLow,
		ToolCapabilityK8sGetDeployment,
		EventSortKeyEventTimeDesc,
		JSONSchemaTypeObject,
		OutputValueTypeObject,
		ModelMessageRoleSystem,
		ModelClientErrorCanceled,
		PlanningToolCatalogErrorToolNotFound,
		ReferenceActionModeTargetStepInput,
		ReferenceIssueCodeCountLimitExceeded,
		ReferencePathSegmentKey,
		ApprovedCheckpointTypeContinuation,
		StepOutcomeCompleted,
		StepContinuationNextStep,
	}
	for _, value := range validValues {
		if !value.Valid() {
			t.Fatalf("%T(%v) 应合法", value, value)
		}
	}
	invalidValues := []interface{ Valid() bool }{
		TaskStatus("UNKNOWN"),
		RunStatus("UNKNOWN"),
		StepStatus("UNKNOWN"),
		TaskExecutionStatus("UNKNOWN"),
		ToolExecutionStatus("INVALID"),
		ApprovalStatus("UNKNOWN"),
		ReportStatus("UNKNOWN"),
		CheckpointNextAction("UNKNOWN"),
		ErrorCode("UNKNOWN"),
		CauseCode("UNKNOWN"),
		TerminationReason("UNKNOWN"),
		InvariantCode("UNKNOWN"),
		StepType("UNKNOWN"),
		RiskLevel("UNKNOWN"),
		ToolCapabilityKind("UNKNOWN"),
		EventSortKey("UNKNOWN"),
		JSONSchemaType("null"),
		OutputValueType("null"),
		ModelMessageRole("assistant"),
		ModelClientErrorKind("UNKNOWN"),
		PlanningToolCatalogErrorKind("UNKNOWN"),
		ReferenceActionMode("UNKNOWN"),
		ReferenceIssueCode("UNKNOWN"),
		ReferencePathSegmentKind("UNKNOWN"),
		ApprovedCheckpointType("UNKNOWN"),
		StepOutcomeKind("UNKNOWN"),
		StepContinuationKind("UNKNOWN"),
	}
	for _, value := range invalidValues {
		if value.Valid() {
			t.Fatalf("%T(%v) 应非法", value, value)
		}
	}
	if ReasonCode("UNKNOWN").ValidForCheckpointInvalid() {
		t.Fatal("UNKNOWN 不应是合法 CheckpointInvalid reason_code")
	}
	if ErrorCode("TIMED_OUT").Valid() || CauseCode("TIMED_OUT").Valid() {
		t.Fatal("TIMED_OUT 不得作为 error_code 或 cause_code")
	}
	if TaskStatusInterrupted.Terminal() {
		t.Fatal("Task INTERRUPTED 不得是业务终态")
	}
	if !TaskExecutionStatusInterrupted.Ended() {
		t.Fatal("TaskExecution INTERRUPTED 必须结束当前尝试")
	}
	if !ToolExecutionStatusUnknown.Terminal() {
		t.Fatal("ToolExecution UNKNOWN 必须是终态")
	}
}

func TestTypedErrorsPreserveCauseWithoutLeakingText(t *testing.T) {
	t.Parallel()

	modelError := NewModelClientError(ModelClientErrorTimeout, context.DeadlineExceeded)
	if !errors.Is(modelError, context.DeadlineExceeded) {
		t.Fatal("ModelClientError 应保留 deadline cause")
	}
	var typedModelError *ModelClientError
	if !errors.As(modelError, &typedModelError) {
		t.Fatal("ModelClientError 应支持 errors.As")
	}

	const secret = "provider-token-secret"
	catalogError := NewPlanningToolCatalogError(
		PlanningToolCatalogErrorRuntimeFatal,
		nil,
		CauseCodePersistenceInvariantViolation,
		errors.New(secret),
	)
	if strings.Contains(catalogError.Error(), secret) {
		t.Fatal("类型化错误不应泄漏底层错误文本")
	}
}

// 以下赋值在编译期固定所有跨模块 execution_version 字段的共享类型。
var (
	_ ExecutionVersion  = ExecutionScope{}.ExecutionVersion
	_ ExecutionVersion  = ExecutionClaim{}.ExecutionVersion
	_ *ExecutionVersion = ModelRequestMetadata{}.ExecutionVersion
	_ ExecutionVersion  = RuntimeContextV1{}.ExecutionVersion
	_ ExecutionVersion  = ApprovalContext{}.ApprovalExecutionVersion
	_ ExecutionVersion  = ApprovedAction{}.ApprovalExecutionVersion
	_ ExecutionVersion  = ApprovedCheckpointEvidence{}.ExecutionVersion
	_ *ExecutionVersion = ApprovedCheckpointEvidence{}.SourceExecutionVersion
	_ ExecutionVersion  = ApprovalRequestPending{}.ExecutionVersion
	_ ExecutionVersion  = ApprovalRequestExisting{}.ExecutionVersion
	_ ExecutionVersion  = ApprovalRequestConflict{}.ExecutionVersion
	_ ExecutionVersion  = ApprovalRequestCheckpointInvalid{}.ExecutionVersion
)

// 以下赋值在编译期固定所有封闭联合的合法实现。
var (
	_ ClaimResult = ClaimResultClaimed{}
	_ ClaimResult = ClaimResultNoWork{}
	_ ClaimResult = ClaimResultConfigMismatchInterrupted{}
	_ ClaimResult = ClaimResultCheckpointInvalidTerminalized{}
	_ ClaimResult = ClaimResultDataInconsistentTerminalized{}
	_ ClaimResult = ClaimResultExpiredTerminalized{}

	_ ExecuteResult = ExecuteResultWaitingApproval{}
	_ ExecuteResult = ExecuteResultTerminal{}
	_ ExecuteResult = ExecuteResultStale{}

	_ ToolFrameworkResult = ToolInvocationCompleted{}
	_ ToolFrameworkResult = ToolApprovalPrepared{}
	_ ToolFrameworkResult = ToolPreflightRejected{}
	_ ToolFrameworkResult = ToolBusinessFailed{}
	_ ToolFrameworkResult = ToolSideEffectUnknown{}
	_ ToolFrameworkResult = ToolCheckpointInvalid{}
	_ ToolFrameworkResult = ToolDeadlineExceeded{}
	_ ToolFrameworkResult = ToolStale{}
	_ ToolFrameworkResult = ToolRuntimeFatal{}

	_ ApprovalRequestResult = ApprovalRequestPending{}
	_ ApprovalRequestResult = ApprovalRequestExisting{}
	_ ApprovalRequestResult = ApprovalRequestConflict{}
	_ ApprovalRequestResult = ApprovalRequestCheckpointInvalid{}
	_ ApprovalRequestResult = ApprovalRequestRuntimeFatal{}

	_ EnsurePendingReportResult = EnsurePendingReportCreated{}
	_ EnsurePendingReportResult = EnsurePendingReportExisting{}

	_ ReportProcessingResult = ReportProcessingCompleted{}
	_ ReportProcessingResult = ReportProcessingFailed{}
	_ ReportProcessingResult = ReportProcessingNoWork{}
	_ ReportProcessingResult = ReportProcessingInterrupted{}
)

type fakeModelClient struct{}

func (fakeModelClient) GenerateStructured(context.Context, ModelRequest) (ModelResponse, error) {
	return ModelResponse{}, nil
}

type fakePlanningToolCatalog struct{}

func (fakePlanningToolCatalog) LoadPlanningToolSnapshot(
	context.Context,
	PlanningToolCatalogSelector,
) (PlanningToolSnapshot, error) {
	return PlanningToolSnapshot{}, nil
}

type fakeToolFramework struct{}

func (fakeToolFramework) InvokeReadTool(context.Context, ReadToolRequest) (ToolFrameworkResult, error) {
	return ToolInvocationCompleted{}, nil
}

func (fakeToolFramework) PrepareWriteApproval(
	context.Context,
	PrepareWriteApprovalRequest,
) (ToolFrameworkResult, error) {
	return ToolApprovalPrepared{}, nil
}

func (fakeToolFramework) InvokeApprovedWrite(
	context.Context,
	ApprovedWriteRequest,
) (ToolFrameworkResult, error) {
	return ToolInvocationCompleted{}, nil
}

type fakeApprovalRequestPort struct{}

func (fakeApprovalRequestPort) RequestApproval(
	context.Context,
	RequestApprovalCommand,
) (ApprovalRequestResult, error) {
	return ApprovalRequestPending{}, nil
}

type fakeRuntimeWriteTx struct{}

func (fakeRuntimeWriteTx) AgentOpsRuntimeWriteTx() {}

type fakeRuntimeWriteExecutor struct{}

func (fakeRuntimeWriteExecutor) Execute(
	ctx context.Context,
	work func(context.Context, RuntimeWriteTx) error,
) error {
	return work(ctx, fakeRuntimeWriteTx{})
}

func (fakeRuntimeWriteExecutor) TryExecute(
	ctx context.Context,
	work func(context.Context, RuntimeWriteTx) error,
) (bool, error) {
	return true, work(ctx, fakeRuntimeWriteTx{})
}

type fakePendingReportWriter struct{}

func (fakePendingReportWriter) EnsurePending(
	context.Context,
	RuntimeWriteTx,
	EnsurePendingReportRequest,
) (EnsurePendingReportResult, error) {
	return EnsurePendingReportCreated{}, nil
}

var (
	_ ModelClient             = fakeModelClient{}
	_ PlanningToolCatalogPort = fakePlanningToolCatalog{}
	_ ToolFrameworkPort       = fakeToolFramework{}
	_ ApprovalRequestPort     = fakeApprovalRequestPort{}
	_ RuntimeWriteExecutor    = fakeRuntimeWriteExecutor{}
	_ RuntimeWriteTx          = fakeRuntimeWriteTx{}
	_ PendingReportWriter     = fakePendingReportWriter{}
)
