package checkpoint

import (
	"bytes"
	"strings"
	"testing"

	"github.com/zhaohaip/agentops-go/internal/contracts"
)

const testFrozenHash = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

func TestRuntimeContextCodecRoundTripsFiveNextActions(t *testing.T) {
	t.Parallel()

	codec := mustRuntimeContextCodec(t, RuntimeContextCodecLimits{MaxBytes: 16 * 1024, MaxDepth: 16})
	tests := []struct {
		name  string
		value contracts.RuntimeContextV1
	}{
		{name: "generate plan", value: validRuntimeContext(contracts.CheckpointNextActionGeneratePlan)},
		{name: "execute step", value: validRuntimeContext(contracts.CheckpointNextActionExecuteStep)},
		{name: "request approval before approval exists", value: validRuntimeContext(contracts.CheckpointNextActionRequestApproval)},
		{name: "execute approved tool", value: validRuntimeContext(contracts.CheckpointNextActionExecuteApprovedTool)},
		{name: "finalize run", value: validRuntimeContext(contracts.CheckpointNextActionFinalizeRun)},
	}

	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			encoded, err := codec.Encode(test.value)
			if err != nil {
				t.Fatalf("Encode() error = %v", err)
			}
			decoded, err := codec.Decode(encoded)
			if err != nil {
				t.Fatalf("Decode() error = %v", err)
			}
			if decoded.NextAction != test.value.NextAction || decoded.TaskID != test.value.TaskID ||
				decoded.RunID != test.value.RunID || decoded.ExecutionVersion != test.value.ExecutionVersion {
				t.Fatalf("Decode() = %#v, want action/ownership from %#v", decoded, test.value)
			}
		})
	}
}

func TestRuntimeContextCodecRejectsUnknownFields(t *testing.T) {
	t.Parallel()

	codec := mustRuntimeContextCodec(t, RuntimeContextCodecLimits{MaxBytes: 16 * 1024, MaxDepth: 16})
	tests := map[string]string{
		"top-level":    `{"schema_version":1,"task_id":"task-1","run_id":"run-1","execution_version":1,"next_action":"GENERATE_PLAN","resolved_references":[],"prompt":"secret"}`,
		"reference":    `{"schema_version":1,"task_id":"task-1","run_id":"run-1","execution_version":1,"plan_id":"plan-1","current_step_id":"step-2","next_action":"EXECUTE_STEP","resolved_references":[{"target_path":[{"kind":"key","key":"input"}],"source_step_id":"step-1","source_output_field":"result","extra":true}]}`,
		"path segment": `{"schema_version":1,"task_id":"task-1","run_id":"run-1","execution_version":1,"plan_id":"plan-1","current_step_id":"step-2","next_action":"EXECUTE_STEP","resolved_references":[{"target_path":[{"kind":"key","key":"input","json_path":"$.input"}],"source_step_id":"step-1","source_output_field":"result"}]}`,
		"approval":     `{"schema_version":1,"task_id":"task-1","run_id":"run-1","execution_version":1,"plan_id":"plan-1","current_step_id":"step-1","next_action":"EXECUTE_APPROVED_TOOL","resolved_references":[],"approval_context":{"approval_id":"approval-1","approval_execution_version":1,"tool_name":"k8s.patch_deployment","frozen_tool_input":{},"observed_values":{},"resource_version":"42","frozen_input_hash":"` + testFrozenHash + `","raw_response":{}}}`,
	}

	for name, document := range tests {
		name, document := name, document
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if _, err := codec.Decode([]byte(document)); err == nil {
				t.Fatal("Decode() accepted unknown field")
			}
		})
	}
}

func TestRuntimeContextCodecRejectsNullMissingAndDuplicateFields(t *testing.T) {
	t.Parallel()

	codec := mustRuntimeContextCodec(t, RuntimeContextCodecLimits{MaxBytes: 16 * 1024, MaxDepth: 16})
	tests := map[string]string{
		"root null":                   `null`,
		"missing references":          `{"schema_version":1,"task_id":"task-1","run_id":"run-1","execution_version":1,"next_action":"GENERATE_PLAN"}`,
		"null references":             `{"schema_version":1,"task_id":"task-1","run_id":"run-1","execution_version":1,"next_action":"GENERATE_PLAN","resolved_references":null}`,
		"null optional is not absent": `{"schema_version":1,"task_id":"task-1","run_id":"run-1","execution_version":1,"plan_id":null,"next_action":"GENERATE_PLAN","resolved_references":[]}`,
		"null approval is not absent": `{"schema_version":1,"task_id":"task-1","run_id":"run-1","execution_version":1,"next_action":"GENERATE_PLAN","resolved_references":[],"approval_context":null}`,
		"duplicate field":             `{"schema_version":1,"task_id":"task-1","task_id":"task-2","run_id":"run-1","execution_version":1,"next_action":"GENERATE_PLAN","resolved_references":[]}`,
	}

	for name, document := range tests {
		name, document := name, document
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if _, err := codec.Decode([]byte(document)); err == nil {
				t.Fatal("Decode() accepted invalid null/missing/duplicate input")
			}
		})
	}
}

func TestRuntimeContextCodecRoundTripsNullableFrozenValues(t *testing.T) {
	t.Parallel()

	codec := mustRuntimeContextCodec(t, RuntimeContextCodecLimits{MaxBytes: 16 * 1024, MaxDepth: 16})
	value := validRuntimeContext(contracts.CheckpointNextActionExecuteApprovedTool)
	value.ApprovalContext.FrozenToolInput = contracts.FrozenToolInput(
		`{"optional":null,"nested":{"value":null},"items":[null,{"value":null}]}`,
	)
	value.ApprovalContext.ObservedValues = contracts.ObservedValues(
		`{"previous":null,"nested":{"value":null},"items":[null]}`,
	)

	encoded, err := codec.Encode(value)
	if err != nil {
		t.Fatalf("Encode() error = %v", err)
	}
	decoded, err := codec.Decode(encoded)
	if err != nil {
		t.Fatalf("Decode() error = %v", err)
	}
	if got, want := string(decoded.ApprovalContext.FrozenToolInput), string(value.ApprovalContext.FrozenToolInput); got != want {
		t.Fatalf("FrozenToolInput = %s, want %s", got, want)
	}
	if got, want := string(decoded.ApprovalContext.ObservedValues), string(value.ApprovalContext.ObservedValues); got != want {
		t.Fatalf("ObservedValues = %s, want %s", got, want)
	}
}

func TestRuntimeContextCodecEnforcesByteAndJSONDepthLimits(t *testing.T) {
	t.Parallel()

	smallCodec := mustRuntimeContextCodec(t, RuntimeContextCodecLimits{MaxBytes: 32, MaxDepth: 16})
	if _, err := smallCodec.Decode([]byte(`{"schema_version":1,"task_id":"task-1"}`)); err == nil {
		t.Fatal("Decode() accepted document over MaxBytes")
	}
	if _, err := smallCodec.Encode(validRuntimeContext(contracts.CheckpointNextActionGeneratePlan)); err == nil {
		t.Fatal("Encode() accepted document over MaxBytes")
	}

	depthCodec := mustRuntimeContextCodec(t, RuntimeContextCodecLimits{MaxBytes: 16 * 1024, MaxDepth: 2})
	deep := `{"schema_version":1,"task_id":"task-1","run_id":"run-1","execution_version":1,"plan_id":"plan-1","current_step_id":"step-1","next_action":"EXECUTE_APPROVED_TOOL","resolved_references":[],"approval_context":{"approval_id":"approval-1","approval_execution_version":1,"tool_name":"tool","frozen_tool_input":{"nested":{}},"observed_values":{},"resource_version":"42","frozen_input_hash":"` + testFrozenHash + `"}}`
	if _, err := depthCodec.Decode([]byte(deep)); err == nil {
		t.Fatal("Decode() accepted document over MaxDepth")
	}
}

func TestRuntimeContextCodecRejectsInvalidUTF8AndReferenceShape(t *testing.T) {
	t.Parallel()

	codec := mustRuntimeContextCodec(t, RuntimeContextCodecLimits{MaxBytes: 64 * 1024, MaxDepth: 32})
	invalidUTF8 := append([]byte(`{"schema_version":1,"task_id":"`), 0xff)
	invalidUTF8 = append(invalidUTF8, []byte(`","run_id":"run-1","execution_version":1,"next_action":"GENERATE_PLAN","resolved_references":[]}`)...)
	if _, err := codec.Decode(invalidUTF8); err == nil {
		t.Fatal("Decode() accepted invalid UTF-8")
	}

	value := validRuntimeContext(contracts.CheckpointNextActionExecuteStep)
	key := "input"
	value.ResolvedReferences = contracts.CanonicalResolvedReferences{{
		TargetPath:        []contracts.ReferencePathSegment{{Kind: contracts.ReferencePathSegmentKey, Key: &key}},
		SourceStepID:      "step-previous",
		SourceOutputField: "not.valid",
	}}
	if _, err := codec.Encode(value); err == nil {
		t.Fatal("Encode() accepted invalid source_output_field")
	}

	value.ResolvedReferences[0].SourceOutputField = "result"
	value.ResolvedReferences[0].TargetPath = make([]contracts.ReferencePathSegment, maxReferencePathDepth+1)
	for index := range value.ResolvedReferences[0].TargetPath {
		segmentKey := "key"
		value.ResolvedReferences[0].TargetPath[index] = contracts.ReferencePathSegment{
			Kind: contracts.ReferencePathSegmentKey,
			Key:  &segmentKey,
		}
	}
	if _, err := codec.Encode(value); err == nil {
		t.Fatal("Encode() accepted target_path over protocol depth")
	}
}

func TestRuntimeContextCodecValidatesNextActionShape(t *testing.T) {
	t.Parallel()

	codec := mustRuntimeContextCodec(t, RuntimeContextCodecLimits{MaxBytes: 16 * 1024, MaxDepth: 16})
	planID := contracts.PlanID("plan-1")
	approval := validApprovalContext()
	tests := []struct {
		name   string
		mutate func(*contracts.RuntimeContextV1)
	}{
		{name: "unknown action", mutate: func(value *contracts.RuntimeContextV1) { value.NextAction = "UNKNOWN" }},
		{name: "generate plan has plan", mutate: func(value *contracts.RuntimeContextV1) { value.PlanID = &planID }},
		{name: "execute step missing step", mutate: func(value *contracts.RuntimeContextV1) { value.CurrentStepID = nil }},
		{name: "execute step has approval", mutate: func(value *contracts.RuntimeContextV1) { value.ApprovalContext = &approval }},
		{name: "request approval missing plan", mutate: func(value *contracts.RuntimeContextV1) { value.PlanID = nil }},
		{name: "execute approved tool missing approval", mutate: func(value *contracts.RuntimeContextV1) { value.ApprovalContext = nil }},
		{name: "finalize run missing step", mutate: func(value *contracts.RuntimeContextV1) { value.CurrentStepID = nil }},
	}

	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			var value contracts.RuntimeContextV1
			switch {
			case strings.HasPrefix(test.name, "generate"):
				value = validRuntimeContext(contracts.CheckpointNextActionGeneratePlan)
			case strings.HasPrefix(test.name, "request"):
				value = validRuntimeContext(contracts.CheckpointNextActionRequestApproval)
			case strings.HasPrefix(test.name, "execute approved"):
				value = validRuntimeContext(contracts.CheckpointNextActionExecuteApprovedTool)
			case strings.HasPrefix(test.name, "finalize"):
				value = validRuntimeContext(contracts.CheckpointNextActionFinalizeRun)
			default:
				value = validRuntimeContext(contracts.CheckpointNextActionExecuteStep)
			}
			test.mutate(&value)
			if _, err := codec.Encode(value); err == nil {
				t.Fatal("Encode() accepted invalid next_action shape")
			}
		})
	}
}

func TestRuntimeContextCodecValidatesApprovalContextConditions(t *testing.T) {
	t.Parallel()

	codec := mustRuntimeContextCodec(t, RuntimeContextCodecLimits{MaxBytes: 16 * 1024, MaxDepth: 16})
	tests := []struct {
		name   string
		mutate func(*contracts.ApprovalContext)
	}{
		{name: "empty approval ID", mutate: func(value *contracts.ApprovalContext) { value.ApprovalID = "" }},
		{name: "invalid approval version", mutate: func(value *contracts.ApprovalContext) { value.ApprovalExecutionVersion = 0 }},
		{name: "future approval version", mutate: func(value *contracts.ApprovalContext) { value.ApprovalExecutionVersion = 2 }},
		{name: "empty tool name", mutate: func(value *contracts.ApprovalContext) { value.ToolName = "" }},
		{name: "non-object frozen input", mutate: func(value *contracts.ApprovalContext) { value.FrozenToolInput = contracts.FrozenToolInput(`[]`) }},
		{name: "non-object observed values", mutate: func(value *contracts.ApprovalContext) { value.ObservedValues = contracts.ObservedValues(`"value"`) }},
		{name: "empty resource version", mutate: func(value *contracts.ApprovalContext) { value.ResourceVersion = "" }},
		{name: "invalid frozen input hash", mutate: func(value *contracts.ApprovalContext) { value.FrozenInputHash = "ABC" }},
	}

	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			value := validRuntimeContext(contracts.CheckpointNextActionExecuteApprovedTool)
			test.mutate(value.ApprovalContext)
			if _, err := codec.Encode(value); err == nil {
				t.Fatal("Encode() accepted invalid ApprovalContext")
			}
		})
	}

	waiting := validRuntimeContext(contracts.CheckpointNextActionRequestApproval)
	waiting.ApprovalContext = pointerTo(validApprovalContext())
	if _, err := codec.Encode(waiting); err != nil {
		t.Fatalf("Encode(waiting approval) error = %v", err)
	}
}

func TestRuntimeContextCodecOutputIsDeterministicAndMinimal(t *testing.T) {
	t.Parallel()

	codec := mustRuntimeContextCodec(t, RuntimeContextCodecLimits{MaxBytes: 16 * 1024, MaxDepth: 16})
	value := validRuntimeContext(contracts.CheckpointNextActionGeneratePlan)
	first, err := codec.Encode(value)
	if err != nil {
		t.Fatalf("Encode() error = %v", err)
	}
	second, err := codec.Encode(value)
	if err != nil {
		t.Fatalf("Encode() second error = %v", err)
	}
	if !bytes.Equal(first, second) {
		t.Fatalf("Encode() is not deterministic:\n%s\n%s", first, second)
	}
	if bytes.Contains(first, []byte("prompt")) || bytes.Contains(first, []byte("history")) ||
		bytes.Contains(first, []byte("raw_response")) {
		t.Fatalf("Encode() persisted forbidden data: %s", first)
	}
}

func validRuntimeContext(action contracts.CheckpointNextAction) contracts.RuntimeContextV1 {
	value := contracts.RuntimeContextV1{
		SchemaVersion:      runtimeContextSchemaVersion,
		TaskID:             "task-1",
		RunID:              "run-1",
		ExecutionVersion:   1,
		NextAction:         action,
		ResolvedReferences: contracts.CanonicalResolvedReferences{},
	}
	if action != contracts.CheckpointNextActionGeneratePlan {
		planID := contracts.PlanID("plan-1")
		value.PlanID = &planID
	}
	if action == contracts.CheckpointNextActionExecuteStep || action == contracts.CheckpointNextActionRequestApproval ||
		action == contracts.CheckpointNextActionExecuteApprovedTool || action == contracts.CheckpointNextActionFinalizeRun {
		stepID := contracts.StepID("step-1")
		value.CurrentStepID = &stepID
	}
	if action == contracts.CheckpointNextActionExecuteApprovedTool {
		value.ApprovalContext = pointerTo(validApprovalContext())
	}
	return value
}

func validApprovalContext() contracts.ApprovalContext {
	return contracts.ApprovalContext{
		ApprovalID:               "approval-1",
		ApprovalExecutionVersion: 1,
		ToolName:                 "k8s.patch_deployment",
		FrozenToolInput:          contracts.FrozenToolInput(`{"namespace":"default","name":"api"}`),
		ObservedValues:           contracts.ObservedValues(`{"resource_version":"42"}`),
		ResourceVersion:          "42",
		FrozenInputHash:          testFrozenHash,
	}
}

func mustRuntimeContextCodec(t *testing.T, limits RuntimeContextCodecLimits) RuntimeContextCodec {
	t.Helper()
	codec, err := NewRuntimeContextCodec(limits)
	if err != nil {
		t.Fatalf("NewRuntimeContextCodec() error = %v", err)
	}
	return codec
}

func pointerTo[T any](value T) *T {
	return &value
}
