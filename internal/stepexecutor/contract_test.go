package stepexecutor

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/zhaohaip/agentops-go/internal/contracts"
)

type contractExecutorFake struct{}

func (contractExecutorFake) ExecuteStep(
	context.Context,
	StepExecutionRequest,
) (StepOutcome, error) {
	return StepOutcomeStale{CauseCode: CauseStaleExecution}, nil
}

func TestPortInterfacesUseFrozenSharedSignatures(t *testing.T) {
	var (
		_ StepExecutorPort              = contractExecutorFake{}
		_ contracts.ModelClient         = (*fakeModelClient)(nil)
		_ contracts.ToolFrameworkPort   = (*fakeToolFrameworkPort)(nil)
		_ contracts.ApprovalRequestPort = (*fakeApprovalRequestPort)(nil)
		_ StepOutcome                   = StepOutcomeCompleted{}
		_ StepOutcome                   = StepOutcomeWaitingApproval{}
		_ StepOutcome                   = StepOutcomeTerminalized{}
		_ StepOutcome                   = StepOutcomeFailed{}
		_ StepOutcome                   = StepOutcomeStale{}
	)

	toolType := reflect.TypeOf((*fakeToolFrameworkPort)(nil))
	if toolType.NumMethod() != 3 {
		t.Fatalf("Fake Tool Framework method count = %d, want 3", toolType.NumMethod())
	}
	if _, exists := toolType.MethodByName("ValidateCapability"); exists {
		t.Fatal("Fake Tool Framework unexpectedly exposes ValidateCapability")
	}

	approvalMethod, exists := reflect.TypeOf((*fakeApprovalRequestPort)(nil)).MethodByName("RequestApproval")
	if !exists || approvalMethod.Type.NumIn() != 3 || approvalMethod.Type.NumOut() != 2 {
		t.Fatalf("RequestApproval signature = %+v", approvalMethod.Type)
	}
	if approvalMethod.Type.In(2) != reflect.TypeOf(contracts.RequestApprovalCommand{}) {
		t.Fatalf("RequestApproval command = %v, want shared RequestApprovalCommand", approvalMethod.Type.In(2))
	}
}

func TestStepOutcomeUnionBranchesAndPayloadValidation(t *testing.T) {
	toolExecutionID := contracts.ToolExecutionID("tool-execution-1")
	toolError := contracts.ErrorCodeToolCallFailed
	completedToolUpdate := ToolResultUpdate{
		ToolExecutionID: toolExecutionID,
		Status:          contracts.ToolExecutionStatusCompleted,
		Output:          mustJSON(`{"result":"safe"}`),
	}
	tests := []struct {
		name    string
		outcome StepOutcome
		kind    contracts.StepOutcomeKind
	}{
		{
			name: "completed",
			outcome: StepOutcomeCompleted{
				SafeOutput:       mustJSON(`{"result":"safe"}`),
				ToolExecutionID:  &toolExecutionID,
				ToolResultUpdate: &completedToolUpdate,
				Continuation: StepContinuation{
					Kind: contracts.StepContinuationFinalizeRun,
				},
			},
			kind: contracts.StepOutcomeCompleted,
		},
		{
			name:    "waiting approval",
			outcome: StepOutcomeWaitingApproval{ApprovalID: "approval-1"},
			kind:    contracts.StepOutcomeWaitingApproval,
		},
		{
			name: "terminalized",
			outcome: StepOutcomeTerminalized{
				TaskID: "task-1", ExecutionVersion: 1,
				ErrorCode: contracts.ErrorCodeCheckpointInvalid, ReportStatus: contracts.ReportStatusPending,
			},
			kind: contracts.StepOutcomeTerminalized,
		},
		{
			name: "failed",
			outcome: StepOutcomeFailed{
				ErrorCode: contracts.ErrorCodeToolCallFailed, CauseCode: CauseModelProviderError,
				SafeSummary: "Tool call failed.", ToolExecutionID: &toolExecutionID,
				ToolResultUpdate: &ToolResultUpdate{
					ToolExecutionID: toolExecutionID, Status: contracts.ToolExecutionStatusFailed,
					ErrorCode: &toolError,
				},
			},
			kind: contracts.StepOutcomeFailed,
		},
		{
			name: "model input too large",
			outcome: StepOutcomeFailed{
				ErrorCode:   contracts.ErrorCodeModelInputTooLarge,
				CauseCode:   CauseModelInputTooLarge,
				SafeSummary: "Model input exceeds the limit.",
			},
			kind: contracts.StepOutcomeFailed,
		},
		{
			name:    "stale",
			outcome: StepOutcomeStale{CauseCode: CauseStaleExecution},
			kind:    contracts.StepOutcomeStale,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if test.outcome.Kind() != test.kind {
				t.Fatalf("Kind() = %s, want %s", test.outcome.Kind(), test.kind)
			}
			if !ValidateStepOutcome(test.outcome) {
				t.Fatalf("ValidateStepOutcome(%T) = false", test.outcome)
			}
		})
	}

	invalid := []StepOutcome{
		StepOutcomeCompleted{Continuation: StepContinuation{Kind: contracts.StepContinuationNextStep}},
		StepOutcomeWaitingApproval{},
		StepOutcomeTerminalized{
			TaskID: "task-1", ExecutionVersion: 1,
			ErrorCode: contracts.ErrorCodeCheckpointInvalid, ReportStatus: contracts.ReportStatusCompleted,
		},
		StepOutcomeFailed{
			ErrorCode: contracts.ErrorCodeToolCallFailed, CauseCode: "UNKNOWN", SafeSummary: "safe",
		},
		StepOutcomeFailed{
			ErrorCode:   contracts.ErrorCodeModelInputTooLarge,
			CauseCode:   CauseModelTimeout,
			SafeSummary: "safe",
		},
		StepOutcomeFailed{
			ErrorCode:   contracts.ErrorCodeModelCallFailed,
			CauseCode:   CauseModelInputTooLarge,
			SafeSummary: "safe",
		},
		StepOutcomeStale{CauseCode: CauseModelTimeout},
	}
	for _, outcome := range invalid {
		if ValidateStepOutcome(outcome) {
			t.Fatalf("ValidateStepOutcome(%T) = true for invalid payload", outcome)
		}
	}
}

func TestValidateStepOutcomeRejectsInvalidCompletedPayloads(t *testing.T) {
	toolExecutionID := contracts.ToolExecutionID("tool-execution-1")
	failedError := contracts.ErrorCodeToolCallFailed
	unknownError := contracts.ErrorCodeWriteToolInterrupted
	validCompleted := func(update *ToolResultUpdate) StepOutcomeCompleted {
		outcome := StepOutcomeCompleted{
			SafeOutput: mustJSON(`{"result":"safe"}`),
			Continuation: StepContinuation{
				Kind: contracts.StepContinuationFinalizeRun,
			},
		}
		if update != nil {
			outcome.ToolExecutionID = &toolExecutionID
			outcome.ToolResultUpdate = update
		}
		return outcome
	}

	tests := []struct {
		name    string
		outcome StepOutcomeCompleted
	}{
		{name: "empty safe output", outcome: validCompleted(nil)},
		{name: "invalid safe output JSON", outcome: validCompleted(nil)},
		{name: "array safe output", outcome: validCompleted(nil)},
		{name: "scalar safe output", outcome: validCompleted(nil)},
		{
			name: "invalid Tool update output JSON",
			outcome: validCompleted(&ToolResultUpdate{
				ToolExecutionID: toolExecutionID, Status: contracts.ToolExecutionStatusCompleted,
				Output: mustJSON(`{"result":`),
			}),
		},
		{
			name: "array Tool update output",
			outcome: validCompleted(&ToolResultUpdate{
				ToolExecutionID: toolExecutionID, Status: contracts.ToolExecutionStatusCompleted,
				Output: mustJSON(`["safe"]`),
			}),
		},
		{
			name: "FAILED Tool update",
			outcome: validCompleted(&ToolResultUpdate{
				ToolExecutionID: toolExecutionID, Status: contracts.ToolExecutionStatusFailed,
				ErrorCode: &failedError,
			}),
		},
		{
			name: "UNKNOWN Tool update",
			outcome: validCompleted(&ToolResultUpdate{
				ToolExecutionID: toolExecutionID, Status: contracts.ToolExecutionStatusUnknown,
				ErrorCode: &unknownError, SideEffectUnknown: true,
			}),
		},
	}
	tests[0].outcome.SafeOutput = nil
	tests[1].outcome.SafeOutput = mustJSON(`{"result":`)
	tests[2].outcome.SafeOutput = mustJSON(`["safe"]`)
	tests[3].outcome.SafeOutput = mustJSON(`"safe"`)

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if ValidateStepOutcome(test.outcome) {
				t.Fatalf("ValidateStepOutcome(%+v) = true", test.outcome)
			}
		})
	}
}

func TestMapModelClientErrorUsesTypedStableClassification(t *testing.T) {
	tests := []struct {
		kind      contracts.ModelClientErrorKind
		wantKind  ErrorKind
		wantError contracts.ErrorCode
		wantCause CauseCode
	}{
		{contracts.ModelClientErrorCanceled, ErrorKindStale, "", CauseStaleExecution},
		{contracts.ModelClientErrorTimeout, ErrorKindFailed, contracts.ErrorCodeModelCallFailed, CauseModelTimeout},
		{contracts.ModelClientErrorAuthentication, ErrorKindFailed, contracts.ErrorCodeModelCallFailed, CauseModelAuthentication},
		{contracts.ModelClientErrorNetwork, ErrorKindFailed, contracts.ErrorCodeModelCallFailed, CauseModelNetwork},
		{contracts.ModelClientErrorRateLimited, ErrorKindFailed, contracts.ErrorCodeModelCallFailed, CauseModelRateLimited},
		{contracts.ModelClientErrorProvider, ErrorKindFailed, contracts.ErrorCodeModelCallFailed, CauseModelProviderError},
		{contracts.ModelClientErrorResponseTooLarge, ErrorKindFailed, contracts.ErrorCodeModelCallFailed, CauseModelResponseTooLarge},
		{contracts.ModelClientErrorInvalidResponse, ErrorKindFailed, contracts.ErrorCodeModelOutputInvalid, CauseModelOutputInvalid},
		{contracts.ModelClientErrorContractViolation, ErrorKindRuntimeFatal, contracts.ErrorCodeStepExecutorContractBroken, CauseRuntimeInvalidModelClientRequest},
	}

	providerText := "provider secret diagnostic"
	root := errors.New(providerText)
	for _, test := range tests {
		t.Run(string(test.kind), func(t *testing.T) {
			mapped := MapModelClientError(contracts.NewModelClientError(test.kind, root))
			if mapped.Kind != test.wantKind || mapped.ErrorCode != test.wantError || mapped.CauseCode != test.wantCause {
				t.Fatalf("mapped error = %+v", mapped)
			}
			if !mapped.Kind.Valid() || !mapped.CauseCode.Valid() || !errors.Is(mapped, root) {
				t.Fatalf("mapped error validity/unwrap = %+v", mapped)
			}
			if strings.Contains(mapped.Error(), providerText) {
				t.Fatal("safe StepError exposed provider text")
			}
		})
	}

	untyped := MapModelClientError(root)
	if untyped.Kind != ErrorKindRuntimeFatal || untyped.CauseCode != CauseStepExecutorContractBroken {
		t.Fatalf("untyped mapping = %+v", untyped)
	}
}

func TestRuntimeFatalMappingsPreserveOnlyFrozenSystemCauses(t *testing.T) {
	tool := MapToolRuntimeFatal(contracts.ToolRuntimeFatal{
		ErrorCode:     contracts.ErrorCodePersistenceInvariantViolation,
		SafeCauseCode: contracts.CauseCodePersistenceInvariantViolation,
	})
	if tool.Kind != ErrorKindRuntimeFatal ||
		tool.ErrorCode != contracts.ErrorCodePersistenceInvariantViolation ||
		tool.CauseCode != CausePersistenceInvariantViolation {
		t.Fatalf("Tool RuntimeFatal mapping = %+v", tool)
	}

	approval := MapApprovalRuntimeFatal(contracts.ApprovalRequestRuntimeFatal{
		ErrorCode: contracts.ErrorCodeRuntimeStaticToolSnapshotInconsistent,
		CauseCode: contracts.CauseCodeRuntimeStaticToolSnapshotInconsistent,
	})
	if approval.Kind != ErrorKindRuntimeFatal ||
		approval.CauseCode != CauseRuntimeStaticToolSnapshotInconsistent {
		t.Fatalf("Approval RuntimeFatal mapping = %+v", approval)
	}

	validPairs := []struct {
		errorCode contracts.ErrorCode
		causeCode CauseCode
	}{
		{contracts.ErrorCodeStepExecutorContractBroken, CauseStepExecutorContractBroken},
		{contracts.ErrorCodeStepExecutorContractBroken, CauseRuntimeInvalidModelClientRequest},
		{contracts.ErrorCodeStepExecutorContractBroken, CauseReferenceCountLimitExceeded},
		{contracts.ErrorCodeRuntimeStaticToolSnapshotInconsistent, CauseRuntimeStaticToolSnapshotInconsistent},
		{contracts.ErrorCodePersistenceInvariantViolation, CausePersistenceInvariantViolation},
	}
	for _, pair := range validPairs {
		mapped := NewRuntimeFatalError(pair.errorCode, pair.causeCode, nil)
		if mapped.ErrorCode != pair.errorCode || mapped.CauseCode != pair.causeCode ||
			!mapped.CauseCode.Valid() || !mapped.CauseCode.RuntimeFatal() {
			t.Fatalf("valid RuntimeFatal pair normalized: %+v", mapped)
		}
	}

	invalidPairs := []struct {
		errorCode contracts.ErrorCode
		causeCode CauseCode
	}{
		{contracts.ErrorCodeStepExecutorContractBroken, CausePersistenceInvariantViolation},
		{contracts.ErrorCodePersistenceInvariantViolation, CauseStepExecutorContractBroken},
		{contracts.ErrorCodeRuntimeStaticToolSnapshotInconsistent, CauseRuntimeInvalidModelClientRequest},
	}
	for _, pair := range invalidPairs {
		mapped := NewRuntimeFatalError(
			pair.errorCode, pair.causeCode, errors.New("invalid system classification"),
		)
		if mapped.ErrorCode != contracts.ErrorCodeStepExecutorContractBroken ||
			mapped.CauseCode != CauseStepExecutorContractBroken {
			t.Fatalf("invalid RuntimeFatal pair was not normalized: %+v", mapped)
		}
	}
}

func TestReferenceCountLimitExceededIsFrozenContractRuntimeFatal(t *testing.T) {
	if CauseReferenceCountLimitExceeded != "REFERENCE_COUNT_LIMIT_EXCEEDED" ||
		!CauseReferenceCountLimitExceeded.Valid() || !CauseReferenceCountLimitExceeded.RuntimeFatal() {
		t.Fatalf("reference count cause = %q", CauseReferenceCountLimitExceeded)
	}

	mapped := NewRuntimeFatalError(
		contracts.ErrorCodeStepExecutorContractBroken,
		CauseReferenceCountLimitExceeded,
		nil,
	)
	if mapped.Kind != ErrorKindRuntimeFatal ||
		mapped.ErrorCode != contracts.ErrorCodeStepExecutorContractBroken ||
		mapped.CauseCode != CauseReferenceCountLimitExceeded {
		t.Fatalf("reference count RuntimeFatal mapping = %+v", mapped)
	}
}

func TestModelInputTooLargeUsesFrozenFailedPair(t *testing.T) {
	if contracts.ErrorCodeModelInputTooLarge != "ModelInputTooLarge" ||
		CauseModelInputTooLarge != "MODEL_INPUT_TOO_LARGE" {
		t.Fatalf("model input limit pair = %q/%q",
			contracts.ErrorCodeModelInputTooLarge, CauseModelInputTooLarge)
	}
	if !validFailedPair(contracts.ErrorCodeModelInputTooLarge, CauseModelInputTooLarge) {
		t.Fatal("ModelInputTooLarge frozen pair was rejected")
	}
	if validFailedPair(contracts.ErrorCodeModelInputTooLarge, CauseModelTimeout) ||
		validFailedPair(contracts.ErrorCodeModelCallFailed, CauseModelInputTooLarge) {
		t.Fatal("mismatched ModelInputTooLarge pair was accepted")
	}
}
