package stepexecutor

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/zhaohaip/agentops-go/internal/contracts"
	"github.com/zhaohaip/agentops-go/internal/contracts/references"
)

func TestInputResolverResolvesObjectAndArrayPathsWithoutChangingTypes(t *testing.T) {
	request := resolverToolRequest(
		json.RawMessage(`{
			"spec":{"payload":"step.output.payload"},
			"values":["step.output.large_integer",7]
		}`),
		contracts.OutputSchema{
			"payload":       {Type: contracts.OutputValueTypeObject},
			"large_integer": {Type: contracts.OutputValueTypeInteger},
		},
		json.RawMessage(`{"payload":{"ready":true},"large_integer":9007199254740993}`),
		objectSchema(map[string]contracts.CanonicalJSONSchema{
			"spec": objectSchema(map[string]contracts.CanonicalJSONSchema{
				"payload": objectSchema(map[string]contracts.CanonicalJSONSchema{
					"ready": {Type: contracts.JSONSchemaTypeBoolean},
				}, "ready"),
			}, "payload"),
			"values": {
				Type: contracts.JSONSchemaTypeArray,
				Items: schemaPointer(contracts.CanonicalJSONSchema{
					Type: contracts.JSONSchemaTypeNumber,
				}),
			},
		}, "spec", "values"),
	)
	setResolverBindings(t, &request)

	resolved, err := NewInputResolver().Resolve(request)
	if err != nil {
		t.Fatalf("Resolve() error = %v", err)
	}
	if resolved.StepID != request.Step.StepID || resolved.InputContractVersion != stepInputContractVersionV1 {
		t.Fatalf("resolved metadata = %+v", resolved)
	}
	if len(resolved.ReferencedFields) != 2 {
		t.Fatalf("len(ReferencedFields) = %d, want 2", len(resolved.ReferencedFields))
	}
	if !bytes.Contains(resolved.Value, []byte(`"payload":{"ready":true}`)) ||
		!bytes.Contains(resolved.Value, []byte(`9007199254740993`)) {
		t.Fatalf("resolved Value = %s", resolved.Value)
	}

	value, err := decodeResolverJSON(resolved.Value)
	if err != nil {
		t.Fatalf("decode resolved Value: %v", err)
	}
	root := value.(map[string]any)
	values := root["values"].([]any)
	if number, ok := values[0].(json.Number); !ok || number.String() != "9007199254740993" {
		t.Fatalf("resolved integer = %#v", values[0])
	}
	if _, ok := root["spec"].(map[string]any)["payload"].(map[string]any); !ok {
		t.Fatalf("resolved object = %#v", root["spec"])
	}
}

func TestInputResolverResolvesNonToolObjectReference(t *testing.T) {
	request := resolverBaseRequest(
		contracts.StepTypeAnalysis,
		json.RawMessage(`{"instruction":"inspect","evidence":"step.output.evidence"}`),
		contracts.OutputSchema{"evidence": {Type: contracts.OutputValueTypeObject}},
		json.RawMessage(`{"evidence":{"status":"ready"}}`),
	)
	setResolverBindings(t, &request)

	resolved, err := NewInputResolver().Resolve(request)
	if err != nil {
		t.Fatalf("Resolve() error = %v", err)
	}
	if string(resolved.Value) != `{"evidence":{"status":"ready"},"instruction":"inspect"}` {
		t.Fatalf("resolved Value = %s", resolved.Value)
	}
}

func TestInputResolverSEIR010RejectsUnsupportedReferenceFormsAsInputFailure(t *testing.T) {
	tests := []struct {
		name   string
		input  string
		target error
	}{
		{name: "multi-level path", input: "step.output.a.b", target: references.ErrReferenceSyntax},
		{name: "array subscript", input: "step.output.a[0]", target: references.ErrReferenceSyntax},
		{name: "dollar template", input: "${step.output.a}", target: references.ErrExpressionNotSupported},
		{name: "mustache template", input: "{{step.output.a}}", target: references.ErrExpressionNotSupported},
		{name: "function expression", input: "lookup(step.output.a)", target: references.ErrExpressionNotSupported},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			input, err := json.Marshal(map[string]string{"prompt": test.input})
			if err != nil {
				t.Fatalf("marshal input: %v", err)
			}
			request := resolverBaseRequest(
				contracts.StepTypeModelCall,
				input,
				contracts.OutputSchema{"a": {Type: contracts.OutputValueTypeString}},
				json.RawMessage(`{"a":"safe"}`),
			)

			_, err = NewInputResolver().Resolve(request)
			assertResolverError(t, err, ErrorKindFailed, contracts.ErrorCodeInputResolutionFailed,
				CauseInputResolutionFailed)
			if !errors.Is(err, test.target) {
				t.Fatalf("Resolve() error = %v, want errors.Is(%v)", err, test.target)
			}
		})
	}
}

func TestInputResolverSEIR011KeepsOrdinaryTextLiteral(t *testing.T) {
	literals := []string{
		"ordinary step.output.a text",
		"value = step.output.a for display",
		"documentation mentions step.output.a",
	}
	for _, literal := range literals {
		t.Run(literal, func(t *testing.T) {
			input, err := json.Marshal(map[string]string{"prompt": literal})
			if err != nil {
				t.Fatalf("marshal input: %v", err)
			}
			request := resolverBaseRequest(contracts.StepTypeModelCall, input, nil, nil)
			request.PreviousStep = nil

			resolved, err := NewInputResolver().Resolve(request)
			if err != nil {
				t.Fatalf("Resolve() error = %v", err)
			}
			if len(resolved.ReferencedFields) != 0 {
				t.Fatalf("ReferencedFields = %#v, want empty", resolved.ReferencedFields)
			}
			var value map[string]string
			if err := json.Unmarshal(resolved.Value, &value); err != nil {
				t.Fatalf("unmarshal resolved input: %v", err)
			}
			if value["prompt"] != literal {
				t.Fatalf("resolved prompt = %q, want %q", value["prompt"], literal)
			}
		})
	}
}

func TestInputResolverSEIR012RejectsInvalidReservedPrefixAsInputFailure(t *testing.T) {
	literals := []string{
		"step.output.",
		"step.output.1invalid",
		"step.output.a-b",
	}
	for _, literal := range literals {
		t.Run(literal, func(t *testing.T) {
			input, err := json.Marshal(map[string]string{"prompt": literal})
			if err != nil {
				t.Fatalf("marshal input: %v", err)
			}
			request := resolverBaseRequest(contracts.StepTypeModelCall, input, nil, nil)

			_, err = NewInputResolver().Resolve(request)
			assertResolverError(t, err, ErrorKindFailed, contracts.ErrorCodeInputResolutionFailed,
				CauseInputResolutionFailed)
			if !errors.Is(err, references.ErrReferenceSyntax) {
				t.Fatalf("Resolve() error = %v, want ErrReferenceSyntax", err)
			}
		})
	}
}

func TestInputResolverRejectsMissingOrInvalidPreviousOutput(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*StepExecutionRequest)
		target error
	}{
		{
			name: "safe field missing",
			mutate: func(request *StepExecutionRequest) {
				request.PreviousStep.SafeOutput = json.RawMessage(`{"other":"value"}`)
			},
			target: references.ErrSourceOutput,
		},
		{
			name: "actual type differs from source Schema",
			mutate: func(request *StepExecutionRequest) {
				request.PreviousStep.SafeOutput = json.RawMessage(`{"prompt":42}`)
			},
			target: references.ErrSourceOutput,
		},
		{
			name: "source is not adjacent",
			mutate: func(request *StepExecutionRequest) {
				request.PreviousStep.Sequence = 1
				request.Step.Sequence = 3
			},
			target: references.ErrSourceStep,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			request := resolverBaseRequest(
				contracts.StepTypeModelCall,
				json.RawMessage(`{"prompt":"step.output.prompt"}`),
				contracts.OutputSchema{"prompt": {Type: contracts.OutputValueTypeString}},
				json.RawMessage(`{"prompt":"continue"}`),
			)
			setResolverBindings(t, &request)
			test.mutate(&request)

			_, err := NewInputResolver().Resolve(request)
			assertResolverError(t, err, ErrorKindFailed, contracts.ErrorCodeInputResolutionFailed,
				CauseInputResolutionFailed)
			if !errors.Is(err, test.target) {
				t.Fatalf("Resolve() error = %v, want errors.Is(%v)", err, test.target)
			}
		})
	}
}

func TestInputResolverRejectsResolvedSchemaErrors(t *testing.T) {
	tests := []struct {
		name    string
		request StepExecutionRequest
	}{
		{
			name: "source declaration incompatible with target",
			request: resolverToolRequest(
				json.RawMessage(`{"replicas":"step.output.value"}`),
				contracts.OutputSchema{"value": {Type: contracts.OutputValueTypeString}},
				json.RawMessage(`{"value":"three"}`),
				objectSchema(map[string]contracts.CanonicalJSONSchema{
					"replicas": {Type: contracts.JSONSchemaTypeInteger},
				}, "replicas"),
			),
		},
		{
			name: "required Tool field missing after resolution",
			request: resolverToolRequest(
				json.RawMessage(`{"name":"web"}`), nil, nil,
				objectSchema(map[string]contracts.CanonicalJSONSchema{
					"name":     {Type: contracts.JSONSchemaTypeString},
					"replicas": {Type: contracts.JSONSchemaTypeInteger},
				}, "name", "replicas"),
			),
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			setResolverBindings(t, &test.request)
			_, err := NewInputResolver().Resolve(test.request)
			assertResolverError(t, err, ErrorKindFailed, contracts.ErrorCodeInputResolutionFailed,
				CauseInputResolutionFailed)
			if !errors.Is(err, ErrResolvedInputSchema) {
				t.Fatalf("Resolve() error = %v, want ErrResolvedInputSchema", err)
			}
		})
	}
}

func TestInputResolverEnforcesResolvedInputStructuralLimits(t *testing.T) {
	tests := []struct {
		name    string
		request StepExecutionRequest
		wantErr bool
	}{
		{
			name:    "non-Tool depth 16",
			request: resolverNonToolStructureRequest(t, nestedResolverObject(15)),
		},
		{
			name:    "non-Tool depth 17",
			request: resolverNonToolStructureRequest(t, nestedResolverObject(16)),
			wantErr: true,
		},
		{
			name:    "non-Tool object 64 fields",
			request: resolverNonToolStructureRequest(t, resolverObjectWithFields(64)),
		},
		{
			name:    "non-Tool object 65 fields",
			request: resolverNonToolStructureRequest(t, resolverObjectWithFields(65)),
			wantErr: true,
		},
		{
			name:    "Tool depth 16",
			request: resolverToolStructureRequest(t, nestedResolverObject(15), nestedResolverSchema(15)),
		},
		{
			name:    "Tool depth 17",
			request: resolverToolStructureRequest(t, nestedResolverObject(16), nestedResolverSchema(16)),
			wantErr: true,
		},
		{
			name: "Tool object 64 fields",
			request: resolverToolStructureRequest(t, resolverObjectWithFields(64),
				resolverObjectFieldsSchema(64)),
		},
		{
			name: "Tool object 65 fields",
			request: resolverToolStructureRequest(t, resolverObjectWithFields(65),
				resolverObjectFieldsSchema(65)),
			wantErr: true,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			resolved, err := NewInputResolver().Resolve(test.request)
			if !test.wantErr {
				if err != nil {
					t.Fatalf("Resolve() error = %v", err)
				}
				if len(resolved.Value) == 0 {
					t.Fatal("Resolve() returned empty Value")
				}
				return
			}
			assertResolverError(t, err, ErrorKindFailed, contracts.ErrorCodeInputResolutionFailed,
				CauseInputResolutionFailed)
			if !errors.Is(err, ErrResolvedInputStructure) {
				t.Fatalf("Resolve() error = %v, want ErrResolvedInputStructure", err)
			}
		})
	}
}

func TestInputResolverRejectsStructuralLimitsIntroducedByReference(t *testing.T) {
	tests := []struct {
		name  string
		value any
	}{
		{name: "depth becomes 17", value: nestedResolverObject(16)},
		{name: "object becomes 65 fields", value: resolverObjectWithFields(65)},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			safeOutput, err := json.Marshal(map[string]any{"context": test.value})
			if err != nil {
				t.Fatalf("marshal safe output: %v", err)
			}
			request := resolverBaseRequest(
				contracts.StepTypeModelCall,
				json.RawMessage(`{"prompt":"inspect","context":"step.output.context"}`),
				contracts.OutputSchema{"context": {Type: contracts.OutputValueTypeObject}},
				safeOutput,
			)
			setResolverBindings(t, &request)

			_, err = NewInputResolver().Resolve(request)
			assertResolverError(t, err, ErrorKindFailed, contracts.ErrorCodeInputResolutionFailed,
				CauseInputResolutionFailed)
			if !errors.Is(err, ErrResolvedInputStructure) {
				t.Fatalf("Resolve() error = %v, want ErrResolvedInputStructure", err)
			}
		})
	}
}

func TestInputResolverRejectsResolvedInputOverFrozenSize(t *testing.T) {
	large := strings.Repeat("x", 600*1024)
	safeOutput, err := json.Marshal(map[string]string{"large": large})
	if err != nil {
		t.Fatalf("marshal safe output: %v", err)
	}
	request := resolverToolRequest(
		json.RawMessage(`{"first":"step.output.large","second":"step.output.large"}`),
		contracts.OutputSchema{"large": {Type: contracts.OutputValueTypeString}},
		safeOutput,
		objectSchema(map[string]contracts.CanonicalJSONSchema{
			"first":  {Type: contracts.JSONSchemaTypeString},
			"second": {Type: contracts.JSONSchemaTypeString},
		}, "first", "second"),
	)
	setResolverBindings(t, &request)

	_, err = NewInputResolver().Resolve(request)
	assertResolverError(t, err, ErrorKindFailed, contracts.ErrorCodeInputResolutionFailed,
		CauseInputResolutionFailed)
	if !errors.Is(err, ErrResolvedInputTooLarge) {
		t.Fatalf("Resolve() error = %v, want ErrResolvedInputTooLarge", err)
	}
}

func TestInputResolverRejectsCheckpointBindingMismatch(t *testing.T) {
	request := resolverBaseRequest(
		contracts.StepTypeModelCall,
		json.RawMessage(`{"prompt":"step.output.prompt"}`),
		contracts.OutputSchema{
			"prompt": {Type: contracts.OutputValueTypeString},
			"other":  {Type: contracts.OutputValueTypeString},
		},
		json.RawMessage(`{"prompt":"continue","other":"wrong"}`),
	)
	setResolverBindings(t, &request)
	request.ResolvedReferences[0].SourceOutputField = "other"

	_, err := NewInputResolver().Resolve(request)
	assertResolverError(t, err, ErrorKindRuntimeFatal,
		contracts.ErrorCodeStepExecutorContractBroken, CauseStepExecutorContractBroken)
	if !errors.Is(err, ErrResolvedReferenceBinding) {
		t.Fatalf("Resolve() error = %v, want ErrResolvedReferenceBinding", err)
	}
}

func TestInputResolverKeepsInvalidCanonicalTargetPathAsRuntimeFatal(t *testing.T) {
	request := resolverBaseRequest(
		contracts.StepTypeModelCall,
		json.RawMessage(`{"prompt":"step.output.prompt"}`),
		contracts.OutputSchema{"prompt": {Type: contracts.OutputValueTypeString}},
		json.RawMessage(`{"prompt":"continue"}`),
	)
	setResolverBindings(t, &request)
	request.ResolvedReferences[0].TargetPath = []contracts.ReferencePathSegment{{
		Kind: contracts.ReferencePathSegmentKey,
	}}

	_, err := NewInputResolver().Resolve(request)
	assertResolverError(t, err, ErrorKindRuntimeFatal,
		contracts.ErrorCodeStepExecutorContractBroken, CauseStepExecutorContractBroken)
	if !errors.Is(err, ErrResolvedReferenceBinding) {
		t.Fatalf("Resolve() error = %v, want ErrResolvedReferenceBinding", err)
	}
}

func TestInputResolverMapsReferenceCountLimitToFrozenRuntimeFatal(t *testing.T) {
	values := make([]string, references.MaxResolvedReferencesPerStep+1)
	for index := range values {
		values[index] = "step.output.value"
	}
	input, err := json.Marshal(map[string]any{"items": values})
	if err != nil {
		t.Fatalf("marshal input: %v", err)
	}
	request := resolverToolRequest(
		input,
		contracts.OutputSchema{"value": {Type: contracts.OutputValueTypeString}},
		json.RawMessage(`{"value":"safe"}`),
		objectSchema(map[string]contracts.CanonicalJSONSchema{
			"items": {
				Type:  contracts.JSONSchemaTypeArray,
				Items: schemaPointer(contracts.CanonicalJSONSchema{Type: contracts.JSONSchemaTypeString}),
			},
		}, "items"),
	)
	request.ResolvedReferences = contracts.CanonicalResolvedReferences{}

	_, err = NewInputResolver().Resolve(request)
	assertResolverError(t, err, ErrorKindRuntimeFatal,
		contracts.ErrorCodeStepExecutorContractBroken, CauseReferenceCountLimitExceeded)
}

func resolverBaseRequest(
	stepType contracts.StepType,
	input json.RawMessage,
	outputSchema contracts.OutputSchema,
	safeOutput json.RawMessage,
) StepExecutionRequest {
	return StepExecutionRequest{
		NextAction: contracts.CheckpointNextActionExecuteStep,
		Step: StepExecutionProjection{
			StepID: "step-2", RunID: "run-1", PlanID: "plan-1", Sequence: 2,
			Type: stepType, Name: "resolve", Input: input,
			OutputSchema: contracts.OutputSchema{"result": {Type: contracts.OutputValueTypeString}},
			Status:       contracts.StepStatusRunning,
		},
		PreviousStep: &PreviousStepProjection{
			StepID: "step-1", Sequence: 1, Status: contracts.StepStatusCompleted,
			SafeOutput: safeOutput, OutputSchema: outputSchema,
		},
		ResolvedReferences: contracts.CanonicalResolvedReferences{},
	}
}

func resolverToolRequest(
	input json.RawMessage,
	outputSchema contracts.OutputSchema,
	safeOutput json.RawMessage,
	inputSchema contracts.CanonicalJSONSchema,
) StepExecutionRequest {
	request := resolverBaseRequest(contracts.StepTypeToolCall, input, outputSchema, safeOutput)
	request.Step.ToolName = "tool.test"
	request.ToolCapability = &contracts.StaticToolDefinition{
		Name: "tool.test", Enabled: true, InputSchema: inputSchema,
		RiskLevel: contracts.RiskLevelLow, ReadOnly: true, TimeoutMS: 1000,
	}
	return request
}

func setResolverBindings(t *testing.T, request *StepExecutionRequest) {
	t.Helper()
	result, err := references.NewStepReferenceExtractor().Extract(references.ExtractRequest{
		ActionMode:              contracts.ReferenceActionModeTargetStepInput,
		StepInput:               request.Step.Input,
		TargetStepSequence:      request.Step.Sequence,
		SourceStep:              resolverSourceStep(request.PreviousStep),
		ValidatePersistedOutput: true,
	})
	if err != nil {
		t.Fatalf("extract test bindings: %v", err)
	}
	request.ResolvedReferences = result.ResolvedReferences
}

func assertResolverError(
	t *testing.T,
	err error,
	wantKind ErrorKind,
	wantErrorCode contracts.ErrorCode,
	wantCause CauseCode,
) *StepError {
	t.Helper()
	var typed *StepError
	if !errors.As(err, &typed) || typed == nil {
		t.Fatalf("Resolve() error = %v, want StepError", err)
	}
	if typed.Kind != wantKind || typed.ErrorCode != wantErrorCode || typed.CauseCode != wantCause {
		t.Fatalf("Resolve() StepError = %+v", typed)
	}
	return typed
}

func objectSchema(
	properties map[string]contracts.CanonicalJSONSchema,
	required ...string,
) contracts.CanonicalJSONSchema {
	additionalProperties := false
	return contracts.CanonicalJSONSchema{
		Type: contracts.JSONSchemaTypeObject, Properties: properties, Required: required,
		AdditionalProperties: &additionalProperties,
	}
}

func resolverNonToolStructureRequest(t *testing.T, contextValue any) StepExecutionRequest {
	t.Helper()
	input, err := json.Marshal(map[string]any{"prompt": "inspect", "context": contextValue})
	if err != nil {
		t.Fatalf("marshal non-Tool input: %v", err)
	}
	request := resolverBaseRequest(contracts.StepTypeModelCall, input, nil, nil)
	request.PreviousStep = nil
	return request
}

func resolverToolStructureRequest(
	t *testing.T,
	payload any,
	payloadSchema contracts.CanonicalJSONSchema,
) StepExecutionRequest {
	t.Helper()
	input, err := json.Marshal(map[string]any{"payload": payload})
	if err != nil {
		t.Fatalf("marshal Tool input: %v", err)
	}
	request := resolverToolRequest(input, nil, nil,
		objectSchema(map[string]contracts.CanonicalJSONSchema{"payload": payloadSchema}, "payload"))
	request.PreviousStep = nil
	return request
}

func nestedResolverObject(depth int) map[string]any {
	value := map[string]any{"leaf": true}
	for level := 1; level < depth; level++ {
		value = map[string]any{"nested": value}
	}
	return value
}

func nestedResolverSchema(depth int) contracts.CanonicalJSONSchema {
	schema := objectSchema(map[string]contracts.CanonicalJSONSchema{
		"leaf": {Type: contracts.JSONSchemaTypeBoolean},
	}, "leaf")
	for level := 1; level < depth; level++ {
		schema = objectSchema(map[string]contracts.CanonicalJSONSchema{"nested": schema}, "nested")
	}
	return schema
}

func resolverObjectWithFields(count int) map[string]any {
	value := make(map[string]any, count)
	for index := 0; index < count; index++ {
		value[fmt.Sprintf("field_%d", index)] = true
	}
	return value
}

func resolverObjectFieldsSchema(count int) contracts.CanonicalJSONSchema {
	properties := make(map[string]contracts.CanonicalJSONSchema, count)
	for index := 0; index < count; index++ {
		properties[fmt.Sprintf("field_%d", index)] = contracts.CanonicalJSONSchema{
			Type: contracts.JSONSchemaTypeBoolean,
		}
	}
	return objectSchema(properties)
}

func schemaPointer(value contracts.CanonicalJSONSchema) *contracts.CanonicalJSONSchema {
	return &value
}
