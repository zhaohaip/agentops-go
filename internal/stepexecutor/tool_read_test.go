package stepexecutor

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/zhaohaip/agentops-go/internal/contracts"
)

func TestReadToolRunnerInvokesFrozenEntryAndMapsSuccess(t *testing.T) {
	originalSize := uint64(2048)
	originalCount := uint64(3)
	fake := &fakeToolFrameworkPort{readResults: []fakeToolResult{{
		result: contracts.ToolInvocationCompleted{
			ToolExecutionID: "tool-execution-1",
			Output:          contracts.SafeToolOutput(`{"name":"api","extra":"discarded"}`),
			Truncated:       true,
			OriginalSize:    &originalSize,
			OriginalCount:   &originalCount,
		},
	}}}
	request, resolved, continuation := readToolFixture()
	ctx := context.WithValue(context.Background(), contextKey("trace"), "read-tool-1")

	outcome, err := NewReadToolRunner(fake).Execute(ctx, request, resolved, continuation)
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	completed, ok := outcome.(StepOutcomeCompleted)
	if !ok || !ValidateStepOutcome(completed) {
		t.Fatalf("outcome = %T %+v", outcome, outcome)
	}
	if string(completed.SafeOutput) != `{"name":"api"}` || completed.ToolExecutionID == nil ||
		*completed.ToolExecutionID != "tool-execution-1" || completed.Continuation != continuation {
		t.Fatalf("Completed = %+v", completed)
	}
	if completed.ToolResultUpdate == nil || completed.ToolResultUpdate.Status != contracts.ToolExecutionStatusCompleted ||
		string(completed.ToolResultUpdate.Output) != `{"name":"api"}` ||
		!completed.ToolResultUpdate.Truncated || completed.ToolResultUpdate.OriginalSize == nil ||
		*completed.ToolResultUpdate.OriginalSize != originalSize || completed.ToolResultUpdate.OriginalCount == nil ||
		*completed.ToolResultUpdate.OriginalCount != originalCount {
		t.Fatalf("ToolResultUpdate = %+v", completed.ToolResultUpdate)
	}

	calls := fake.recordedReadCalls()
	if len(calls) != 1 || calls[0].context != ctx || calls[0].contextCanceled {
		t.Fatalf("read calls = %+v", calls)
	}
	wantRequest := contracts.ReadToolRequest{
		Scope:          request.Scope,
		Authorization:  *request.AgentAuthorization,
		ToolName:       request.Step.ToolName,
		ResolvedInput:  contracts.ResolvedToolInput(resolved.Value),
		ToolDefinition: *request.ToolCapability,
	}
	if !reflect.DeepEqual(calls[0].request, wantRequest) ||
		calls[0].request.Scope.ExecutionConfigHash != request.Scope.ExecutionConfigHash {
		t.Fatalf("ReadToolRequest = %+v, want %+v", calls[0].request, wantRequest)
	}
	if len(fake.recordedPrepareCalls()) != 0 || len(fake.recordedApprovedCalls()) != 0 {
		t.Fatal("ReadToolRunner invoked a non-read Tool entry")
	}
}

func TestReadToolRunnerMapsBusinessFailures(t *testing.T) {
	tests := []struct {
		name      string
		errorCode contracts.ErrorCode
		causeCode CauseCode
		withTool  bool
	}{
		{name: "ToolNotFound before boundary", errorCode: contracts.ErrorCodeToolNotFound, causeCode: CauseToolNotFound},
		{name: "ToolDisabled before boundary", errorCode: contracts.ErrorCodeToolDisabled, causeCode: CauseToolDisabled},
		{name: "ToolNotAuthorized before boundary", errorCode: contracts.ErrorCodeToolNotAuthorized, causeCode: CauseToolNotAuthorized},
		{name: "ToolInputInvalid before boundary", errorCode: contracts.ErrorCodeToolInputInvalid, causeCode: CauseToolInputInvalid},
		{name: "ToolAccessDenied before boundary", errorCode: contracts.ErrorCodeToolAccessDenied, causeCode: CauseToolAccessDenied},
		{name: "ToolTimeout before boundary", errorCode: contracts.ErrorCodeToolTimeout, causeCode: CauseToolTimeout},
		{name: "ToolTimeout after boundary", errorCode: contracts.ErrorCodeToolTimeout, causeCode: CauseToolTimeout, withTool: true},
		{name: "ToolConnectionLost after boundary", errorCode: contracts.ErrorCodeToolConnectionLost, causeCode: CauseToolConnectionLost, withTool: true},
		{name: "ToolCallFailed after boundary", errorCode: contracts.ErrorCodeToolCallFailed, causeCode: CauseToolCallFailed, withTool: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			result := contracts.ToolBusinessFailed{
				ErrorCode: test.errorCode, SafeSummary: "Safe Tool failure.",
			}
			if test.withTool {
				toolExecutionID := contracts.ToolExecutionID("tool-execution-1")
				status := contracts.ToolExecutionStatusFailed
				result.ToolExecutionID = &toolExecutionID
				result.ToolExecutionStatus = &status
			}
			fake := &fakeToolFrameworkPort{readResults: []fakeToolResult{{result: result}}}
			request, resolved, continuation := readToolFixture()

			outcome, err := NewReadToolRunner(fake).Execute(
				context.Background(), request, resolved, continuation,
			)
			if err != nil {
				t.Fatalf("Execute() error = %v", err)
			}
			failed, ok := outcome.(StepOutcomeFailed)
			if !ok || !ValidateStepOutcome(failed) || failed.ErrorCode != test.errorCode ||
				failed.CauseCode != test.causeCode || failed.SafeSummary != result.SafeSummary {
				t.Fatalf("Failed = %T %+v", outcome, outcome)
			}
			if test.withTool {
				if failed.ToolExecutionID == nil || failed.ToolResultUpdate == nil ||
					failed.ToolResultUpdate.Status != contracts.ToolExecutionStatusFailed ||
					failed.ToolResultUpdate.ErrorCode == nil ||
					*failed.ToolResultUpdate.ErrorCode != test.errorCode {
					t.Fatalf("failed Tool update = %+v", failed)
				}
			} else if failed.ToolExecutionID != nil || failed.ToolResultUpdate != nil {
				t.Fatalf("boundary failure unexpectedly has ToolExecution = %+v", failed)
			}
			if calls := len(fake.recordedReadCalls()); calls != 1 {
				t.Fatalf("InvokeReadTool calls = %d, want 1", calls)
			}
		})
	}
}

func TestReadToolRunnerRejectsBusinessFailureBoundaryViolations(t *testing.T) {
	toolExecutionID := contracts.ToolExecutionID("tool-execution-1")
	emptyToolExecutionID := contracts.ToolExecutionID("")
	failedStatus := contracts.ToolExecutionStatusFailed
	runningStatus := contracts.ToolExecutionStatusRunning
	completedStatus := contracts.ToolExecutionStatusCompleted
	unknownStatus := contracts.ToolExecutionStatusUnknown
	boundaries := []struct {
		name   string
		id     *contracts.ToolExecutionID
		status *contracts.ToolExecutionStatus
	}{
		{name: "absent"},
		{name: "FAILED", id: &toolExecutionID, status: &failedStatus},
		{name: "RUNNING", id: &toolExecutionID, status: &runningStatus},
		{name: "COMPLETED", id: &toolExecutionID, status: &completedStatus},
		{name: "UNKNOWN", id: &toolExecutionID, status: &unknownStatus},
		{name: "ID only", id: &toolExecutionID},
		{name: "status only", status: &failedStatus},
		{name: "empty ID", id: &emptyToolExecutionID, status: &failedStatus},
	}
	rules := []struct {
		errorCode    contracts.ErrorCode
		allowsNone   bool
		allowsFailed bool
	}{
		{errorCode: contracts.ErrorCodeToolNotFound, allowsNone: true},
		{errorCode: contracts.ErrorCodeToolDisabled, allowsNone: true},
		{errorCode: contracts.ErrorCodeToolNotAuthorized, allowsNone: true},
		{errorCode: contracts.ErrorCodeToolInputInvalid, allowsNone: true},
		{errorCode: contracts.ErrorCodeToolAccessDenied, allowsNone: true},
		{errorCode: contracts.ErrorCodeToolTimeout, allowsNone: true, allowsFailed: true},
		{errorCode: contracts.ErrorCodeToolConnectionLost, allowsFailed: true},
		{errorCode: contracts.ErrorCodeToolCallFailed, allowsFailed: true},
	}
	for _, rule := range rules {
		for _, boundary := range boundaries {
			valid := boundary.name == "absent" && rule.allowsNone ||
				boundary.name == "FAILED" && rule.allowsFailed
			if valid {
				continue
			}
			t.Run(string(rule.errorCode)+"/"+boundary.name, func(t *testing.T) {
				result := readToolBusinessFailureWithExecution(rule.errorCode, boundary.id, boundary.status)
				fake := &fakeToolFrameworkPort{readResults: []fakeToolResult{{result: result}}}
				request, resolved, continuation := readToolFixture()

				outcome, err := NewReadToolRunner(fake).Execute(
					context.Background(), request, resolved, continuation,
				)
				if outcome != nil {
					t.Fatalf("invalid boundary produced outcome = %T %+v", outcome, outcome)
				}
				assertModelStepError(t, err, ErrorKindRuntimeFatal,
					contracts.ErrorCodeStepExecutorContractBroken, CauseStepExecutorContractBroken)
				if calls := len(fake.recordedReadCalls()); calls != 1 {
					t.Fatalf("InvokeReadTool calls = %d, want 1", calls)
				}
			})
		}
	}
}

func TestReadToolRunnerDoesNotPrevalidateCapability(t *testing.T) {
	fake := &fakeToolFrameworkPort{readResults: []fakeToolResult{{result: contracts.ToolBusinessFailed{
		ErrorCode: contracts.ErrorCodeToolDisabled, SafeSummary: "Tool is disabled.",
	}}}}
	request, resolved, continuation := readToolFixture()
	request.AgentAuthorization.AllowedTools = nil
	request.ToolCapability.Enabled = false
	request.ToolCapability.RiskLevel = contracts.RiskLevelHigh
	request.ToolCapability.ReadOnly = false

	outcome, err := NewReadToolRunner(fake).Execute(context.Background(), request, resolved, continuation)
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	failed, ok := outcome.(StepOutcomeFailed)
	if !ok || failed.ErrorCode != contracts.ErrorCodeToolDisabled || len(fake.recordedReadCalls()) != 1 {
		t.Fatalf("outcome/calls = %T %+v/%d", outcome, outcome, len(fake.recordedReadCalls()))
	}
}

func TestReadToolRunnerMapsProcessingFailuresWithCompletedToolUpdate(t *testing.T) {
	tests := []struct {
		name      string
		result    contracts.ToolInvocationCompleted
		errorCode contracts.ErrorCode
		causeCode CauseCode
	}{
		{
			name:      "Tool Framework output processing",
			result:    readToolProcessingFailure(contracts.ErrorCodeStepOutputInvalid),
			errorCode: contracts.ErrorCodeStepOutputInvalid,
			causeCode: CauseStepOutputInvalid,
		},
		{
			name:      "Tool Framework sanitization",
			result:    readToolProcessingFailure(contracts.ErrorCodeResultSanitizationFailed),
			errorCode: contracts.ErrorCodeResultSanitizationFailed,
			causeCode: CauseResultSanitizationFailed,
		},
		{
			name:      "Tool Framework output size",
			result:    readToolProcessingFailure(contracts.ErrorCodeStepOutputTooLarge),
			errorCode: contracts.ErrorCodeStepOutputTooLarge,
			causeCode: CauseStepOutputTooLarge,
		},
		{
			name: "Step Executor schema projection",
			result: contracts.ToolInvocationCompleted{
				ToolExecutionID: "tool-execution-1", Output: contracts.SafeToolOutput(`{"wrong":"value"}`),
			},
			errorCode: contracts.ErrorCodeStepOutputInvalid,
			causeCode: CauseStepOutputInvalid,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fake := &fakeToolFrameworkPort{readResults: []fakeToolResult{{result: test.result}}}
			request, resolved, continuation := readToolFixture()
			outcome, err := NewReadToolRunner(fake).Execute(
				context.Background(), request, resolved, continuation,
			)
			if err != nil {
				t.Fatalf("Execute() error = %v", err)
			}
			failed, ok := outcome.(StepOutcomeFailed)
			if !ok || !ValidateStepOutcome(failed) || failed.ErrorCode != test.errorCode ||
				failed.CauseCode != test.causeCode || failed.ToolResultUpdate == nil ||
				failed.ToolResultUpdate.Status != contracts.ToolExecutionStatusCompleted ||
				failed.ToolResultUpdate.Output != nil {
				t.Fatalf("processing Failed = %T %+v", outcome, outcome)
			}
		})
	}
}

func TestReadToolRunnerMapsDeadlineStaleRuntimeFatalAndSystemError(t *testing.T) {
	tests := []struct {
		name   string
		result contracts.ToolFrameworkResult
		check  func(*testing.T, StepOutcome, error)
	}{
		{
			name:   "deadline",
			result: contracts.ToolDeadlineExceeded{CauseCode: contracts.CauseCodeTaskTimeout},
			check: func(t *testing.T, outcome StepOutcome, err error) {
				failed, ok := outcome.(StepOutcomeFailed)
				if err != nil || !ok || failed.ErrorCode != contracts.ErrorCodeTaskTimeout ||
					failed.CauseCode != CauseTaskTimeout || !ValidateStepOutcome(failed) {
					t.Fatalf("deadline outcome/error = %T %+v/%v", outcome, outcome, err)
				}
			},
		},
		{
			name:   "stale",
			result: contracts.ToolStale{ReasonCode: contracts.ReasonCode("UNENUMERATED_STALE_REASON")},
			check: func(t *testing.T, outcome StepOutcome, err error) {
				stale, ok := outcome.(StepOutcomeStale)
				if err != nil || !ok || stale.CauseCode != CauseStaleExecution || !ValidateStepOutcome(stale) {
					t.Fatalf("stale outcome/error = %T %+v/%v", outcome, outcome, err)
				}
			},
		},
		{
			name: "Runtime Fatal",
			result: contracts.ToolRuntimeFatal{
				ErrorCode:     contracts.ErrorCodePersistenceInvariantViolation,
				SafeCauseCode: contracts.CauseCodePersistenceInvariantViolation,
			},
			check: func(t *testing.T, outcome StepOutcome, err error) {
				if outcome != nil {
					t.Fatalf("Runtime Fatal outcome = %T", outcome)
				}
				assertModelStepError(t, err, ErrorKindRuntimeFatal,
					contracts.ErrorCodePersistenceInvariantViolation, CausePersistenceInvariantViolation)
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fake := &fakeToolFrameworkPort{readResults: []fakeToolResult{{result: test.result}}}
			request, resolved, continuation := readToolFixture()
			outcome, err := NewReadToolRunner(fake).Execute(
				context.Background(), request, resolved, continuation,
			)
			test.check(t, outcome, err)
		})
	}

	systemErr := errors.New("Tool Framework transaction outcome is unknown")
	fake := &fakeToolFrameworkPort{readResults: []fakeToolResult{{err: systemErr}}}
	request, resolved, continuation := readToolFixture()
	outcome, err := NewReadToolRunner(fake).Execute(context.Background(), request, resolved, continuation)
	if outcome != nil || !errors.Is(err, systemErr) {
		t.Fatalf("system outcome/error = %T/%v", outcome, err)
	}
}

func TestReadToolRunnerMapsCancellationBeforeInvocation(t *testing.T) {
	tests := []struct {
		name      string
		cause     contracts.ExecutionCancellationCause
		wantKind  contracts.StepOutcomeKind
		wantCause CauseCode
	}{
		{name: "Task canceled", cause: contracts.ExecutionCancellationCauseTaskCancelled,
			wantKind: contracts.StepOutcomeStale, wantCause: CauseTaskCancelled},
		{name: "Task timed out", cause: contracts.ExecutionCancellationCauseTaskTimedOut,
			wantKind: contracts.StepOutcomeStale, wantCause: CauseTaskTimedOut},
		{name: "runtime shutdown", cause: contracts.ExecutionCancellationCauseRuntimeShutdown,
			wantKind: contracts.StepOutcomeStale, wantCause: CauseRuntimeShutdown},
		{name: "lock lost", cause: contracts.ExecutionCancellationCauseLockLost,
			wantKind: contracts.StepOutcomeStale, wantCause: CauseLockLost},
		{name: "action timeout", cause: contracts.ExecutionCancellationCauseActionTimeout,
			wantKind: contracts.StepOutcomeFailed, wantCause: CauseToolTimeout},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			ctx, cancel := context.WithCancelCause(context.Background())
			cancel(test.cause)
			fake := &fakeToolFrameworkPort{readResults: []fakeToolResult{{result: contracts.ToolStale{
				ReasonCode: contracts.ReasonCode("NOT_REACHED_AFTER_PRECALL_CANCELLATION"),
			}}}}
			request, resolved, continuation := readToolFixture()

			outcome, err := NewReadToolRunner(fake).Execute(ctx, request, resolved, continuation)
			if err != nil || outcome == nil || outcome.Kind() != test.wantKind {
				t.Fatalf("canceled outcome/error = %T %+v/%v", outcome, outcome, err)
			}
			switch value := outcome.(type) {
			case StepOutcomeStale:
				if value.CauseCode != test.wantCause {
					t.Fatalf("Stale cause = %s, want %s", value.CauseCode, test.wantCause)
				}
			case StepOutcomeFailed:
				if value.ErrorCode != contracts.ErrorCodeToolTimeout || value.CauseCode != test.wantCause {
					t.Fatalf("Failed = %+v", value)
				}
			}
			if calls := len(fake.recordedReadCalls()); calls != 0 {
				t.Fatalf("InvokeReadTool calls after cancellation = %d, want 0", calls)
			}
		})
	}
}

func TestReadToolRunnerRejectsMalformedPortResults(t *testing.T) {
	tests := []struct {
		name   string
		result contracts.ToolFrameworkResult
		err    error
	}{
		{name: "empty result"},
		{name: "result and error", result: contracts.ToolStale{
			ReasonCode: contracts.ReasonCode("UNENUMERATED_STALE_REASON"),
		}, err: errors.New("unexpected")},
		{name: "empty Stale reason", result: contracts.ToolStale{}},
		{name: "disallowed branch", result: contracts.ToolApprovalPrepared{}},
		{name: "deadline cause", result: contracts.ToolDeadlineExceeded{CauseCode: contracts.CauseCodeToolTimeout}},
		{name: "processing output present", result: contracts.ToolInvocationCompleted{
			ToolExecutionID: "tool-execution-1", Output: contracts.SafeToolOutput(`{}`),
			ProcessingError: errorCodePointer(contracts.ErrorCodeStepOutputInvalid),
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			port := &malformedReadToolPort{result: test.result, err: test.err}
			request, resolved, continuation := readToolFixture()
			outcome, err := NewReadToolRunner(port).Execute(
				context.Background(), request, resolved, continuation,
			)
			if outcome != nil {
				t.Fatalf("malformed outcome = %T", outcome)
			}
			assertModelStepError(t, err, ErrorKindRuntimeFatal,
				contracts.ErrorCodeStepExecutorContractBroken, CauseStepExecutorContractBroken)
			if port.readCalls != 1 || port.prepareCalls != 0 || port.approvedCalls != 0 {
				t.Fatalf("entry calls = read:%d prepare:%d approved:%d",
					port.readCalls, port.prepareCalls, port.approvedCalls)
			}
		})
	}
}

type malformedReadToolPort struct {
	result        contracts.ToolFrameworkResult
	err           error
	readCalls     int
	prepareCalls  int
	approvedCalls int
}

func (p *malformedReadToolPort) InvokeReadTool(
	context.Context,
	contracts.ReadToolRequest,
) (contracts.ToolFrameworkResult, error) {
	p.readCalls++
	return p.result, p.err
}

func (p *malformedReadToolPort) PrepareWriteApproval(
	context.Context,
	contracts.PrepareWriteApprovalRequest,
) (contracts.ToolFrameworkResult, error) {
	p.prepareCalls++
	return nil, errors.New("unexpected PrepareWriteApproval call")
}

func (p *malformedReadToolPort) InvokeApprovedWrite(
	context.Context,
	contracts.ApprovedWriteRequest,
) (contracts.ToolFrameworkResult, error) {
	p.approvedCalls++
	return nil, errors.New("unexpected InvokeApprovedWrite call")
}

func readToolFixture() (StepExecutionRequest, ResolvedStepInput, StepContinuation) {
	scope := contracts.ExecutionScope{
		TaskID: "task-1", RunID: "run-1", ExecutionVersion: 2,
		ExecutionConfigHash: contracts.ExecutionConfigHash(strings.Repeat("a", 64)),
		WorkerID:            "worker-1", StepID: "step-2", DeadlineAt: time.Unix(200, 0).UTC(),
	}
	additionalProperties := false
	toolDefinition := contracts.StaticToolDefinition{
		Name: "k8s.get_deployment", Enabled: true, Description: "Get one Deployment.",
		CapabilityKind: contracts.ToolCapabilityK8sGetDeployment,
		InputSchema: contracts.CanonicalJSONSchema{
			Type: contracts.JSONSchemaTypeObject, AdditionalProperties: &additionalProperties,
			Properties: map[string]contracts.CanonicalJSONSchema{
				"cluster": {Type: contracts.JSONSchemaTypeString},
			},
			Required: []string{"cluster"},
		},
		OutputSchema: contracts.CanonicalJSONSchema{
			Type: contracts.JSONSchemaTypeObject, AdditionalProperties: &additionalProperties,
			Properties: map[string]contracts.CanonicalJSONSchema{
				"name": {Type: contracts.JSONSchemaTypeString},
			},
			Required: []string{"name"},
		},
		RiskLevel: contracts.RiskLevelLow, ReadOnly: true, TimeoutMS: 30000,
	}
	authorization := contracts.AgentAuthorization{
		AgentID: "agent-1", AllowedTools: []contracts.ToolName{"k8s.get_deployment"},
	}
	request := StepExecutionRequest{
		Scope: scope, NextAction: contracts.CheckpointNextActionExecuteStep,
		Step: StepExecutionProjection{
			StepID: "step-2", RunID: "run-1", PlanID: "plan-1", Sequence: 2,
			Type: contracts.StepTypeToolCall, Name: "Read Deployment",
			Input:        json.RawMessage(`{"cluster":"prod"}`),
			OutputSchema: contracts.OutputSchema{"name": {Type: contracts.OutputValueTypeString}},
			ToolName:     "k8s.get_deployment", Status: contracts.StepStatusRunning,
		},
		ResolvedReferences: contracts.CanonicalResolvedReferences{},
		AgentAuthorization: &authorization,
		ToolCapability:     &toolDefinition,
	}
	resolved := ResolvedStepInput{
		StepID: request.Step.StepID, Value: json.RawMessage(`{"cluster":"prod"}`),
		ReferencedFields: contracts.CanonicalResolvedReferences{}, InputContractVersion: stepInputContractVersionV1,
	}
	continuation := StepContinuation{Kind: contracts.StepContinuationFinalizeRun}
	return request, resolved, continuation
}

func readToolProcessingFailure(errorCode contracts.ErrorCode) contracts.ToolInvocationCompleted {
	return contracts.ToolInvocationCompleted{
		ToolExecutionID: "tool-execution-1", ProcessingError: errorCodePointer(errorCode),
	}
}

func readToolBusinessFailureWithExecution(
	errorCode contracts.ErrorCode,
	toolExecutionID *contracts.ToolExecutionID,
	status *contracts.ToolExecutionStatus,
) contracts.ToolBusinessFailed {
	return contracts.ToolBusinessFailed{
		ErrorCode: errorCode, SafeSummary: "safe",
		ToolExecutionID: toolExecutionID, ToolExecutionStatus: status,
	}
}

func errorCodePointer(value contracts.ErrorCode) *contracts.ErrorCode {
	return &value
}
