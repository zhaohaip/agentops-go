package stepexecutor

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/zhaohaip/agentops-go/internal/contracts"
	"github.com/zhaohaip/agentops-go/internal/taskruntime/activecall"
)

func TestModelStepRunnerExecutesThreeFrozenStepTypes(t *testing.T) {
	tests := []struct {
		stepType contracts.StepType
		input    string
		phrase   string
	}{
		{
			stepType: contracts.StepTypeModelCall,
			input:    `{"prompt":"summarize","context":{"source":"safe"}}`,
			phrase:   "Execute prompt using only optional context.",
		},
		{
			stepType: contracts.StepTypeAnalysis,
			input:    `{"instruction":"analyze","evidence":{"source":"safe"}}`,
			phrase:   "Analyze evidence according to instruction.",
		},
		{
			stepType: contracts.StepTypeVerification,
			input:    `{"criteria":"match","evidence":{"source":"safe"}}`,
			phrase:   "Verify evidence only against criteria",
		},
	}
	for _, test := range tests {
		t.Run(string(test.stepType), func(t *testing.T) {
			providerRequestID := "provider-in-memory"
			fake := &fakeModelClient{results: []fakeModelResult{{response: contracts.ModelResponse{
				AssistantContent: `{"result":"safe"}`, ProviderRequestID: &providerRequestID,
			}}}}
			request, resolved := modelStepFixture(test.stepType, test.input,
				contracts.OutputSchema{"result": {Type: contracts.OutputValueTypeString}})
			request.PreviousStep = &PreviousStepProjection{
				StepID: "step-previous", Sequence: 1, Status: contracts.StepStatusCompleted,
				SafeOutput: json.RawMessage(`{"private":"historical-secret"}`),
			}

			result, err := NewModelStepRunner(fake).Execute(context.Background(), request, resolved)
			if err != nil {
				t.Fatalf("Execute() error = %v", err)
			}
			if string(result.SafeOutput) != `{"result":"safe"}` {
				t.Fatalf("SafeOutput = %s", result.SafeOutput)
			}
			calls := fake.recordedCalls()
			if len(calls) != 1 {
				t.Fatalf("Model calls = %d, want 1", len(calls))
			}
			call := calls[0]
			if call.request.Model != modelStepModelName || call.request.Stream ||
				call.request.ResponseFormat != modelStepResponseFormat ||
				!reflect.DeepEqual(call.request.GenerationParams, request.Agent.GenerationParams) {
				t.Fatalf("Model request contract = %+v", call.request)
			}
			if call.request.Metadata.TaskID != request.Scope.TaskID ||
				call.request.Metadata.RunID != request.Scope.RunID ||
				call.request.Metadata.ExecutionVersion == nil ||
				*call.request.Metadata.ExecutionVersion != request.Scope.ExecutionVersion ||
				call.request.Metadata.StepID == nil || *call.request.Metadata.StepID != request.Step.StepID ||
				call.request.Metadata.StepType == nil || *call.request.Metadata.StepType != test.stepType {
				t.Fatalf("Model metadata = %+v", call.request.Metadata)
			}
			combined := call.request.Messages[0].Content + call.request.Messages[1].Content
			if !strings.Contains(combined, test.phrase) || !strings.Contains(combined, test.input) ||
				strings.Contains(combined, "historical-secret") {
				t.Fatalf("Model messages violate bounded input: %s", combined)
			}
		})
	}
}

func TestModelStepRunnerValidatesStrictOutputSchema(t *testing.T) {
	completeSchema := contracts.OutputSchema{
		"array":   {Type: contracts.OutputValueTypeArray},
		"boolean": {Type: contracts.OutputValueTypeBoolean},
		"integer": {Type: contracts.OutputValueTypeInteger},
		"number":  {Type: contracts.OutputValueTypeNumber},
		"object":  {Type: contracts.OutputValueTypeObject},
		"string":  {Type: contracts.OutputValueTypeString},
	}
	request, resolved := modelStepFixture(contracts.StepTypeModelCall,
		`{"prompt":"produce"}`, completeSchema)
	fake := &fakeModelClient{results: []fakeModelResult{{response: contracts.ModelResponse{
		AssistantContent: `{"array":[1],"boolean":true,"integer":2,"number":3,"object":{"ok":true},"string":"value"}`,
	}}}}
	if result, err := NewModelStepRunner(fake).Execute(context.Background(), request, resolved); err != nil {
		t.Fatalf("valid output error = %v", err)
	} else if len(result.SafeOutput) == 0 {
		t.Fatal("valid output is empty")
	}

	invalid := []struct {
		name    string
		content string
	}{
		{name: "missing field", content: `{}`},
		{name: "extra field", content: `{"result":"ok","extra":true}`},
		{name: "null", content: `{"result":null}`},
		{name: "wrong type", content: `{"result":1}`},
		{name: "duplicate key", content: `{"result":"first","result":"second"}`},
		{name: "trailing text", content: `{"result":"ok"} trailing`},
		{name: "Markdown fence", content: "```json\n{\"result\":\"ok\"}\n```"},
		{name: "non-object", content: `["ok"]`},
	}
	for _, test := range invalid {
		t.Run(test.name, func(t *testing.T) {
			request, resolved := modelStepFixture(contracts.StepTypeModelCall,
				`{"prompt":"produce"}`,
				contracts.OutputSchema{"result": {Type: contracts.OutputValueTypeString}})
			fake := &fakeModelClient{results: []fakeModelResult{
				{response: contracts.ModelResponse{AssistantContent: test.content}},
				{response: contracts.ModelResponse{AssistantContent: `{"result":"repair-must-not-run"}`}},
			}}

			_, err := NewModelStepRunner(fake).Execute(context.Background(), request, resolved)
			assertModelStepError(t, err, ErrorKindFailed, contracts.ErrorCodeModelOutputInvalid,
				CauseModelOutputInvalid)
			if !errors.Is(err, ErrModelStepOutputInvalid) {
				t.Fatalf("Execute() error = %v, want ErrModelStepOutputInvalid", err)
			}
			if calls := len(fake.recordedCalls()); calls != 1 {
				t.Fatalf("Model calls after invalid output = %d, want 1 without Repair", calls)
			}
		})
	}
}

func TestModelStepRunnerMapsModelClientErrorsWithoutRetry(t *testing.T) {
	tests := []struct {
		kind      contracts.ModelClientErrorKind
		wantKind  ErrorKind
		wantError contracts.ErrorCode
		wantCause CauseCode
	}{
		{contracts.ModelClientErrorTimeout, ErrorKindFailed, contracts.ErrorCodeModelCallFailed, CauseModelTimeout},
		{contracts.ModelClientErrorAuthentication, ErrorKindFailed, contracts.ErrorCodeModelCallFailed, CauseModelAuthentication},
		{contracts.ModelClientErrorNetwork, ErrorKindFailed, contracts.ErrorCodeModelCallFailed, CauseModelNetwork},
		{contracts.ModelClientErrorRateLimited, ErrorKindFailed, contracts.ErrorCodeModelCallFailed, CauseModelRateLimited},
		{contracts.ModelClientErrorProvider, ErrorKindFailed, contracts.ErrorCodeModelCallFailed, CauseModelProviderError},
		{contracts.ModelClientErrorResponseTooLarge, ErrorKindFailed, contracts.ErrorCodeModelCallFailed, CauseModelResponseTooLarge},
		{contracts.ModelClientErrorInvalidResponse, ErrorKindFailed, contracts.ErrorCodeModelOutputInvalid, CauseModelOutputInvalid},
		{contracts.ModelClientErrorContractViolation, ErrorKindRuntimeFatal, contracts.ErrorCodeStepExecutorContractBroken, CauseRuntimeInvalidModelClientRequest},
	}
	for _, test := range tests {
		t.Run(string(test.kind), func(t *testing.T) {
			request, resolved := modelStepFixture(contracts.StepTypeModelCall,
				`{"prompt":"produce"}`,
				contracts.OutputSchema{"result": {Type: contracts.OutputValueTypeString}})
			fake := &fakeModelClient{results: []fakeModelResult{
				{err: contracts.NewModelClientError(test.kind, errors.New("provider secret diagnostic"))},
				{response: contracts.ModelResponse{AssistantContent: `{"result":"retry-must-not-run"}`}},
			}}

			_, err := NewModelStepRunner(fake).Execute(context.Background(), request, resolved)
			assertModelStepError(t, err, test.wantKind, test.wantError, test.wantCause)
			if len(fake.recordedCalls()) != 1 {
				t.Fatalf("Model calls = %d, want exactly 1", len(fake.recordedCalls()))
			}
		})
	}
}

func TestModelStepRunnerUsesMinimumDeadlineAndMapsTimeout(t *testing.T) {
	if modelStepCallTimeout != 60*time.Second {
		t.Fatalf("default Model Step timeout = %s, want 60s", modelStepCallTimeout)
	}

	tests := []struct {
		name          string
		runnerTimeout time.Duration
		parentTimeout time.Duration
		wantParentMin bool
	}{
		{name: "Runner limit is earlier", runnerTimeout: 25 * time.Millisecond, parentTimeout: time.Second},
		{name: "parent deadline is earlier", runnerTimeout: time.Second, parentTimeout: 25 * time.Millisecond, wantParentMin: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			parentCtx, cancel := context.WithTimeout(context.Background(), test.parentTimeout)
			defer cancel()
			parentDeadline, ok := parentCtx.Deadline()
			if !ok {
				t.Fatal("parent context has no deadline")
			}

			started := make(chan context.Context, 1)
			fake := &fakeModelClient{results: []fakeModelResult{
				{
					err:                contracts.NewModelClientError(contracts.ModelClientErrorTimeout, context.DeadlineExceeded),
					started:            started,
					waitForContextDone: true,
				},
				{response: contracts.ModelResponse{AssistantContent: `{"result":"retry-must-not-run"}`}},
			}}
			runner := NewModelStepRunner(fake)
			runner.callTimeout = test.runnerTimeout
			request, resolved := modelStepFixture(contracts.StepTypeModelCall,
				`{"prompt":"produce"}`,
				contracts.OutputSchema{"result": {Type: contracts.OutputValueTypeString}})

			done := make(chan error, 1)
			go func() {
				_, err := runner.Execute(parentCtx, request, resolved)
				done <- err
			}()

			var callCtx context.Context
			select {
			case callCtx = <-started:
			case <-time.After(time.Second):
				t.Fatal("Fake Model did not receive the request")
			}
			callDeadline, ok := callCtx.Deadline()
			if !ok {
				t.Fatal("Model call context has no deadline")
			}
			if test.wantParentMin {
				if !callDeadline.Equal(parentDeadline) {
					t.Fatalf("Model deadline = %s, want parent deadline %s", callDeadline, parentDeadline)
				}
			} else if !callDeadline.Before(parentDeadline) || time.Until(callDeadline) > test.runnerTimeout {
				t.Fatalf("Model deadline = %s, want Runner limit before %s", callDeadline, parentDeadline)
			}

			var err error
			select {
			case err = <-done:
			case <-time.After(time.Second):
				t.Fatal("Runner did not return after context timeout")
			}
			assertModelStepError(t, err, ErrorKindFailed,
				contracts.ErrorCodeModelCallFailed, CauseModelTimeout)
			if calls := len(fake.recordedCalls()); calls != 1 {
				t.Fatalf("Model calls after timeout = %d, want 1 without retry", calls)
			}
		})
	}
}

func TestModelStepRunnerClassifiesCancellationAndDiscardsLateResult(t *testing.T) {
	tests := []struct {
		name      string
		cause     activecall.CancellationCause
		wantKind  ErrorKind
		wantError contracts.ErrorCode
		wantCause CauseCode
	}{
		{name: "Task canceled", cause: activecall.CauseTaskCancelled, wantKind: ErrorKindStale, wantCause: CauseTaskCancelled},
		{name: "Task timed out", cause: activecall.CauseTaskTimedOut, wantKind: ErrorKindStale, wantCause: CauseTaskTimedOut},
		{name: "runtime shutdown", cause: activecall.CauseRuntimeShutdown, wantKind: ErrorKindStale, wantCause: CauseRuntimeShutdown},
		{name: "lock lost", cause: activecall.CauseLockLost, wantKind: ErrorKindStale, wantCause: CauseLockLost},
		{
			name: "action timeout", cause: activecall.CauseActionTimeout, wantKind: ErrorKindFailed,
			wantError: contracts.ErrorCodeModelCallFailed, wantCause: CauseModelTimeout,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			registry := activecall.NewRegistry()
			key := activecall.Key{TaskID: "task-1", ExecutionVersion: 2, WorkerID: "worker-1"}
			handle, err := registry.Prepare(context.Background(), key, activecall.Metadata{
				ActionKind: contracts.CheckpointNextActionExecuteStep, StepID: "step-2",
			})
			if err != nil {
				t.Fatalf("Prepare() error = %v", err)
			}
			defer handle.Unregister()
			if err := handle.Activate(); err != nil {
				t.Fatalf("Activate() error = %v", err)
			}
			request, resolved := modelStepFixture(contracts.StepTypeModelCall,
				`{"prompt":"produce"}`,
				contracts.OutputSchema{"result": {Type: contracts.OutputValueTypeString}})
			fake := &fakeModelClient{results: []fakeModelResult{{
				response: contracts.ModelResponse{AssistantContent: `{"result":"late-secret"}`},
				beforeReturn: func() {
					if canceled, cancelErr := registry.Cancel(key, test.cause); cancelErr != nil || !canceled {
						t.Errorf("Registry.Cancel() = (%v, %v)", canceled, cancelErr)
					}
				},
			}}}

			result, err := NewModelStepRunner(fake).Execute(handle.Context(), request, resolved)
			assertModelStepError(t, err, test.wantKind, test.wantError, test.wantCause)
			if result.SafeOutput != nil {
				t.Fatalf("late SafeOutput = %s, want nil", result.SafeOutput)
			}
			if len(fake.recordedCalls()) != 1 {
				t.Fatalf("Model calls = %d, want 1", len(fake.recordedCalls()))
			}
		})
	}

	registry := activecall.NewRegistry()
	key := activecall.Key{TaskID: "task-1", ExecutionVersion: 2, WorkerID: "worker-1"}
	handle, err := registry.Prepare(context.Background(), key, activecall.Metadata{
		ActionKind: contracts.CheckpointNextActionExecuteStep, StepID: "step-2",
	})
	if err != nil {
		t.Fatalf("Prepare() error = %v", err)
	}
	defer handle.Unregister()
	if canceled, cancelErr := registry.Cancel(key, activecall.CauseTaskCancelled); cancelErr != nil || !canceled {
		t.Fatalf("Registry.Cancel() = (%v, %v)", canceled, cancelErr)
	}
	request, resolved := modelStepFixture(contracts.StepTypeModelCall,
		`{"prompt":"produce"}`, contracts.OutputSchema{"result": {Type: contracts.OutputValueTypeString}})
	fake := &fakeModelClient{results: []fakeModelResult{{response: contracts.ModelResponse{
		AssistantContent: `{"result":"must-not-run"}`,
	}}}}
	_, err = NewModelStepRunner(fake).Execute(handle.Context(), request, resolved)
	assertModelStepError(t, err, ErrorKindStale, "", CauseTaskCancelled)
	if len(fake.recordedCalls()) != 0 {
		t.Fatal("pre-canceled call reached Model Client")
	}
}

func TestModelStepRunnerEnforcesResponseAndSafeOutputLimits(t *testing.T) {
	request, resolved := modelStepFixture(contracts.StepTypeModelCall,
		`{"prompt":"produce"}`, contracts.OutputSchema{"result": {Type: contracts.OutputValueTypeString}})
	fake := &fakeModelClient{results: []fakeModelResult{{response: contracts.ModelResponse{
		AssistantContent: strings.Repeat("x", maxModelStepResponseBytes+1),
	}}}}
	_, err := NewModelStepRunner(fake).Execute(context.Background(), request, resolved)
	assertModelStepError(t, err, ErrorKindFailed, contracts.ErrorCodeModelCallFailed,
		CauseModelResponseTooLarge)
	if !errors.Is(err, ErrModelStepResponseTooLarge) {
		t.Fatalf("response limit error = %v", err)
	}

	items := make([]map[string]string, 60000)
	for index := range items {
		items[index] = map[string]string{"token": "x"}
	}
	content, marshalErr := json.Marshal(map[string]any{"items": items})
	if marshalErr != nil || len(content) > maxModelStepResponseBytes {
		t.Fatalf("build expansion fixture: bytes=%d error=%v", len(content), marshalErr)
	}
	request, resolved = modelStepFixture(contracts.StepTypeModelCall,
		`{"prompt":"produce"}`, contracts.OutputSchema{"items": {Type: contracts.OutputValueTypeArray}})
	fake = &fakeModelClient{results: []fakeModelResult{{response: contracts.ModelResponse{
		AssistantContent: string(content),
	}}}}
	_, err = NewModelStepRunner(fake).Execute(context.Background(), request, resolved)
	assertModelStepError(t, err, ErrorKindFailed, contracts.ErrorCodeStepOutputTooLarge,
		CauseStepOutputTooLarge)
	if !errors.Is(err, ErrModelStepOutputTooLarge) {
		t.Fatalf("safe output limit error = %v", err)
	}
}

func TestModelStepRunnerEnforcesFrozenOutputStructureBoundaries(t *testing.T) {
	tests := []struct {
		name      string
		content   string
		wantError bool
	}{
		{name: "depth 16", content: modelOutputAtDepth(maxModelStepOutputDepth)},
		{name: "depth 17", content: modelOutputAtDepth(maxModelStepOutputDepth + 1), wantError: true},
		{name: "64 fields", content: modelOutputWithObjectFields(maxModelStepObjectFields)},
		{name: "65 fields", content: modelOutputWithObjectFields(maxModelStepObjectFields + 1), wantError: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			request, resolved := modelStepFixture(contracts.StepTypeModelCall,
				`{"prompt":"produce"}`,
				contracts.OutputSchema{"result": {Type: contracts.OutputValueTypeObject}})
			fake := &fakeModelClient{results: []fakeModelResult{
				{response: contracts.ModelResponse{AssistantContent: test.content}},
				{response: contracts.ModelResponse{AssistantContent: `{"result":{"repair":"must-not-run"}}`}},
			}}

			result, err := NewModelStepRunner(fake).Execute(context.Background(), request, resolved)
			if test.wantError {
				assertModelStepError(t, err, ErrorKindFailed,
					contracts.ErrorCodeModelOutputInvalid, CauseModelOutputInvalid)
				if result.SafeOutput != nil {
					t.Fatalf("invalid boundary SafeOutput = %s", result.SafeOutput)
				}
			} else if err != nil || len(result.SafeOutput) == 0 {
				t.Fatalf("valid boundary result/error = %s/%v", result.SafeOutput, err)
			}
			if calls := len(fake.recordedCalls()); calls != 1 {
				t.Fatalf("Model calls = %d, want 1 without Repair", calls)
			}
		})
	}
}

func TestModelStepRunnerSanitizesSecretsAndRejectsUnsafeResult(t *testing.T) {
	request, resolved := modelStepFixture(contracts.StepTypeAnalysis,
		`{"instruction":"inspect","evidence":{}}`,
		contracts.OutputSchema{"result": {Type: contracts.OutputValueTypeObject}})
	content := `{"result":{"Password":{"raw":"hidden"},"auth":"  Bearer credential","pem":"line\n-----BEGIN PRIVATE KEY-----\nvalue"}}`
	fake := &fakeModelClient{results: []fakeModelResult{{response: contracts.ModelResponse{
		AssistantContent: content,
	}}}}
	result, err := NewModelStepRunner(fake).Execute(context.Background(), request, resolved)
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	if strings.Contains(string(result.SafeOutput), "hidden") ||
		strings.Contains(string(result.SafeOutput), "credential") ||
		strings.Contains(string(result.SafeOutput), "PRIVATE KEY") ||
		strings.Count(string(result.SafeOutput), modelRedactedValue) != 3 {
		t.Fatalf("SafeOutput was not deterministically redacted: %s", result.SafeOutput)
	}

	unsafe := []struct {
		name    string
		schema  contracts.OutputSchema
		content string
	}{
		{
			name:    "redaction changes declared type",
			schema:  contracts.OutputSchema{"secret": {Type: contracts.OutputValueTypeObject}},
			content: `{"secret":{"value":"hidden"}}`,
		},
		{
			name:    "forbidden control character",
			schema:  contracts.OutputSchema{"result": {Type: contracts.OutputValueTypeString}},
			content: `{"result":"unsafe\u0001value"}`,
		},
	}
	for _, test := range unsafe {
		t.Run(test.name, func(t *testing.T) {
			request, resolved := modelStepFixture(contracts.StepTypeModelCall,
				`{"prompt":"produce"}`, test.schema)
			fake := &fakeModelClient{results: []fakeModelResult{{response: contracts.ModelResponse{
				AssistantContent: test.content,
			}}}}
			_, err := NewModelStepRunner(fake).Execute(context.Background(), request, resolved)
			assertModelStepError(t, err, ErrorKindFailed,
				contracts.ErrorCodeResultSanitizationFailed, CauseResultSanitizationFailed)
			if !errors.Is(err, ErrModelStepSanitization) || strings.Contains(err.Error(), "hidden") ||
				strings.Contains(err.Error(), "unsafe") {
				t.Fatalf("sanitization error exposed unsafe value: %v", err)
			}
		})
	}
}

func TestModelStepRunnerRedactsAllFrozenSensitiveKeysAndPEMFormats(t *testing.T) {
	sensitiveValues := map[string]any{
		"password":            map[string]any{"raw": "password-value"},
		"PASSWD":              "passwd-value",
		"secret":              "secret-value",
		"TOKEN":               "token-value",
		"api_key":             "api-key-value",
		"APIKEY":              "apikey-value",
		"private_key":         "private-key-value",
		"CLIENT_SECRET":       "client-secret-value",
		"authorization":       "authorization-value",
		"basic_authorization": "  \tBaSiC credential-value",
		"generic_private_pem": "prefix\n-----BEGIN PRIVATE KEY-----\nvalue",
		"rsa_private_pem":     "-----BEGIN RSA PRIVATE KEY-----\nvalue",
		"ec_private_pem":      "prefix\n-----BEGIN EC PRIVATE KEY-----\nvalue",
		"openssh_private_pem": "-----BEGIN OPENSSH PRIVATE KEY-----\nvalue",
	}
	content, err := json.Marshal(map[string]any{"result": sensitiveValues})
	if err != nil {
		t.Fatalf("marshal sensitive fixture: %v", err)
	}
	request, resolved := modelStepFixture(contracts.StepTypeAnalysis,
		`{"instruction":"inspect","evidence":{}}`,
		contracts.OutputSchema{"result": {Type: contracts.OutputValueTypeObject}})
	fake := &fakeModelClient{results: []fakeModelResult{{response: contracts.ModelResponse{
		AssistantContent: string(content),
	}}}}

	result, err := NewModelStepRunner(fake).Execute(context.Background(), request, resolved)
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	var decoded struct {
		Result map[string]any `json:"result"`
	}
	if err := json.Unmarshal(result.SafeOutput, &decoded); err != nil {
		t.Fatalf("decode SafeOutput: %v", err)
	}
	if len(decoded.Result) != len(sensitiveValues) {
		t.Fatalf("redacted field count = %d, want %d", len(decoded.Result), len(sensitiveValues))
	}
	for key, value := range decoded.Result {
		if value != modelRedactedValue {
			t.Errorf("redacted %q = %#v, want %q", key, value, modelRedactedValue)
		}
	}
}

func TestModelStepRunnerEnforcesFrozenPromptBoundary(t *testing.T) {
	request, resolved := modelStepPromptSizedFixture(t, maxModelStepPromptBytes)
	fake := &fakeModelClient{results: []fakeModelResult{{response: contracts.ModelResponse{
		AssistantContent: `{"result":"safe"}`,
	}}}}
	result, err := NewModelStepRunner(fake).Execute(context.Background(), request, resolved)
	if err != nil || string(result.SafeOutput) != `{"result":"safe"}` {
		t.Fatalf("exact-limit result/error = %s/%v", result.SafeOutput, err)
	}
	if calls := len(fake.recordedCalls()); calls != 1 {
		t.Fatalf("exact-limit Model Client calls = %d, want 1", calls)
	}

	request, resolved = modelStepPromptSizedFixture(t, maxModelStepPromptBytes+1)
	fake = &fakeModelClient{results: []fakeModelResult{{response: contracts.ModelResponse{
		AssistantContent: `{"result":"must-not-run"}`,
	}}}}
	_, err = NewModelStepRunner(fake).Execute(context.Background(), request, resolved)
	assertModelStepError(t, err, ErrorKindFailed, contracts.ErrorCodeModelInputTooLarge,
		CauseModelInputTooLarge)
	if !errors.Is(err, ErrModelStepInputTooLarge) {
		t.Fatalf("prompt limit error = %v", err)
	}
	if calls := len(fake.recordedCalls()); calls != 0 {
		t.Fatalf("over-limit Model Client calls = %d, want 0", calls)
	}
}

func TestModelStepRunnerHasNoPersistenceOrTransactionCapability(t *testing.T) {
	typeOfRunner := reflect.TypeOf(ModelStepRunner{})
	if typeOfRunner.NumField() != 2 || typeOfRunner.Field(0).Name != "model" ||
		typeOfRunner.Field(1).Name != "callTimeout" {
		t.Fatalf("ModelStepRunner fields = %v", typeOfRunner)
	}
	method, exists := reflect.TypeOf((*ModelStepRunner)(nil)).MethodByName("Execute")
	if !exists || method.Type.NumIn() != 4 || method.Type.NumOut() != 2 {
		t.Fatalf("Execute signature = %v", method.Type)
	}
}

func modelStepFixture(
	stepType contracts.StepType,
	input string,
	outputSchema contracts.OutputSchema,
) (StepExecutionRequest, ResolvedStepInput) {
	request := StepExecutionRequest{
		Scope: contracts.ExecutionScope{
			TaskID: "task-1", RunID: "run-1", ExecutionVersion: 2,
			ExecutionConfigHash: contracts.ExecutionConfigHash(strings.Repeat("a", 64)),
			WorkerID:            "worker-1", StepID: "step-2", DeadlineAt: time.Now().Add(time.Minute).UTC(),
		},
		NextAction: contracts.CheckpointNextActionExecuteStep,
		Step: StepExecutionProjection{
			StepID: "step-2", RunID: "run-1", PlanID: "plan-1", Sequence: 2,
			Type: stepType, Name: fmt.Sprintf("%s step", stepType), Input: json.RawMessage(input),
			OutputSchema: outputSchema, Status: contracts.StepStatusRunning,
		},
		ResolvedReferences: contracts.CanonicalResolvedReferences{},
		Agent: &AgentProjection{
			AgentID: "agent-1", SystemPrompt: "You are a bounded AgentOps model.",
			ModelName: modelStepModelName,
			GenerationParams: contracts.GenerationParams{
				Temperature: contracts.NewCanonicalDecimalV1(2, 1),
				TopP:        contracts.NewCanonicalDecimalV1(1, 0), MaxOutputTokens: 4096,
			},
		},
	}
	resolved := ResolvedStepInput{
		StepID: request.Step.StepID, Value: json.RawMessage(input),
		ReferencedFields:     contracts.CanonicalResolvedReferences{},
		InputContractVersion: stepInputContractVersionV1,
	}
	return request, resolved
}

func modelStepPromptSizedFixture(
	t *testing.T,
	targetBytes int,
) (StepExecutionRequest, ResolvedStepInput) {
	t.Helper()
	request, resolved := modelStepFixture(contracts.StepTypeModelCall,
		`{"prompt":"produce"}`, contracts.OutputSchema{"result": {Type: contracts.OutputValueTypeString}})
	modelRequest, err := buildModelStepRequest(request, resolved)
	if err != nil {
		t.Fatalf("build base Model request: %v", err)
	}
	baseBytes := 0
	for _, message := range modelRequest.Messages {
		baseBytes += len(message.Content)
	}
	fixedBytes := baseBytes - len(request.Agent.SystemPrompt)
	if targetBytes <= fixedBytes {
		t.Fatalf("target Prompt bytes = %d, fixed bytes = %d", targetBytes, fixedBytes)
	}
	request.Agent.SystemPrompt = strings.Repeat("s", targetBytes-fixedBytes)
	modelRequest, err = buildModelStepRequest(request, resolved)
	if err != nil {
		t.Fatalf("build sized Model request: %v", err)
	}
	actualBytes := 0
	for _, message := range modelRequest.Messages {
		actualBytes += len(message.Content)
	}
	if actualBytes != targetBytes {
		t.Fatalf("Prompt bytes = %d, want %d", actualBytes, targetBytes)
	}
	return request, resolved
}

func modelOutputAtDepth(depth int) string {
	value := `{"leaf":"safe"}`
	for currentDepth := 2; currentDepth < depth; currentDepth++ {
		value = `{"nested":` + value + `}`
	}
	return `{"result":` + value + `}`
}

func modelOutputWithObjectFields(count int) string {
	fields := make([]string, count)
	for index := range fields {
		fields[index] = fmt.Sprintf(`"field_%02d":%d`, index, index)
	}
	return `{"result":{` + strings.Join(fields, ",") + `}}`
}

func assertModelStepError(
	t *testing.T,
	err error,
	wantKind ErrorKind,
	wantError contracts.ErrorCode,
	wantCause CauseCode,
) *StepError {
	t.Helper()
	var typed *StepError
	if !errors.As(err, &typed) || typed == nil {
		t.Fatalf("error = %v, want StepError", err)
	}
	if typed.Kind != wantKind || typed.ErrorCode != wantError || typed.CauseCode != wantCause {
		t.Fatalf("StepError = %+v, want %s/%s/%s", typed, wantKind, wantError, wantCause)
	}
	return typed
}
