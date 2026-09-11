package stepexecutor

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"unicode/utf8"

	"github.com/zhaohaip/agentops-go/internal/contracts"
)

const maxReadToolSafeSummaryBytes = 512

var (
	// ErrReadToolContract 表示只读 Tool 请求或返回值违反冻结契约。
	ErrReadToolContract = errors.New("read Tool contract is invalid")
	// ErrReadToolOutputInvalid 表示只读 Tool 的安全输出不满足当前 Step OutputSchema。
	ErrReadToolOutputInvalid = errors.New("read Tool output is invalid")
	// ErrReadToolSanitization 表示只读 Tool 输出未能通过最终安全处理。
	ErrReadToolSanitization = errors.New("read Tool output sanitization failed")
	// ErrReadToolOutputTooLarge 表示只读 Tool 的最终安全输出超过冻结上限。
	ErrReadToolOutputTooLarge = errors.New("read Tool output exceeds size limit")
)

// ReadToolRunner 通过 Tool Framework 的唯一只读入口执行 ToolCall。
type ReadToolRunner struct {
	toolFramework contracts.ToolFrameworkPort
}

// NewReadToolRunner 创建无状态的只读 Tool Runner。
func NewReadToolRunner(toolFramework contracts.ToolFrameworkPort) *ReadToolRunner {
	return &ReadToolRunner{toolFramework: toolFramework}
}

// Execute 构造共享 ReadToolRequest，并把冻结 ToolFrameworkResult 映射为 StepOutcome。
func (r *ReadToolRunner) Execute(
	ctx context.Context,
	request StepExecutionRequest,
	resolved ResolvedStepInput,
	continuation StepContinuation,
) (StepOutcome, error) {
	if err := validateReadToolStepRequest(ctx, request, resolved, continuation); err != nil {
		return nil, contractResolutionError(err)
	}
	if r == nil || r.toolFramework == nil {
		return nil, contractResolutionError(ErrReadToolContract)
	}
	if outcome := readToolCancellation(ctx); outcome != nil {
		return outcome, nil
	}

	toolRequest := contracts.ReadToolRequest{
		Scope:          request.Scope,
		Authorization:  *request.AgentAuthorization,
		ToolName:       request.Step.ToolName,
		ResolvedInput:  contracts.ResolvedToolInput(append([]byte(nil), resolved.Value...)),
		ToolDefinition: *request.ToolCapability,
	}
	if toolRequest.Scope != request.Scope {
		return nil, contractResolutionError(ErrReadToolContract)
	}

	result, err := r.toolFramework.InvokeReadTool(ctx, toolRequest)
	if err != nil {
		if result != nil {
			return nil, contractResolutionError(ErrReadToolContract)
		}
		return nil, err
	}
	if result == nil {
		return nil, contractResolutionError(ErrReadToolContract)
	}
	return mapReadToolResult(ctx, result, request.Step.OutputSchema, continuation)
}

func validateReadToolStepRequest(
	ctx context.Context,
	request StepExecutionRequest,
	resolved ResolvedStepInput,
	continuation StepContinuation,
) error {
	if ctx == nil || request.NextAction != contracts.CheckpointNextActionExecuteStep ||
		request.Scope.TaskID == "" || request.Scope.RunID == "" ||
		!request.Scope.ExecutionVersion.Valid() || !request.Scope.ExecutionConfigHash.Valid() ||
		request.Scope.WorkerID == "" || request.Scope.StepID == "" || request.Scope.DeadlineAt.IsZero() ||
		request.Step.StepID == "" || request.Step.StepID != request.Scope.StepID ||
		request.Step.RunID != request.Scope.RunID || request.Step.PlanID == "" || request.Step.Sequence == 0 ||
		request.Step.Type != contracts.StepTypeToolCall || strings.TrimSpace(request.Step.Name) == "" ||
		!utf8.ValidString(request.Step.Name) || request.Step.ToolName == "" ||
		(request.Step.Status != contracts.StepStatusPending && request.Step.Status != contracts.StepStatusRunning) ||
		request.AgentAuthorization == nil || request.AgentAuthorization.AgentID == "" ||
		request.ToolCapability == nil || resolved.StepID != request.Step.StepID ||
		resolved.InputContractVersion != stepInputContractVersionV1 || !validOutcomeJSONObject(resolved.Value) ||
		!resolvedReferencesEqual(resolved.ReferencedFields, request.ResolvedReferences) ||
		!validModelOutputSchema(request.Step.OutputSchema) || !continuation.Valid() {
		return ErrReadToolContract
	}
	return nil
}

func mapReadToolResult(
	ctx context.Context,
	result contracts.ToolFrameworkResult,
	outputSchema contracts.OutputSchema,
	continuation StepContinuation,
) (StepOutcome, error) {
	switch value := result.(type) {
	case contracts.ToolInvocationCompleted:
		return mapReadToolCompleted(value, outputSchema, continuation)
	case contracts.ToolBusinessFailed:
		outcome, ok := mapReadToolBusinessFailed(value)
		if !ok {
			return nil, contractResolutionError(ErrReadToolContract)
		}
		return outcome, nil
	case contracts.ToolDeadlineExceeded:
		if value.CauseCode != contracts.CauseCodeTaskTimeout {
			return nil, contractResolutionError(ErrReadToolContract)
		}
		return StepOutcomeFailed{
			ErrorCode:   contracts.ErrorCodeTaskTimeout,
			CauseCode:   CauseTaskTimeout,
			SafeSummary: "Task deadline exceeded.",
		}, nil
	case contracts.ToolStale:
		if value.ReasonCode == "" || value.ToolExecutionID != nil && *value.ToolExecutionID == "" {
			return nil, contractResolutionError(ErrReadToolContract)
		}
		return StepOutcomeStale{CauseCode: readToolStaleCause(ctx)}, nil
	case contracts.ToolRuntimeFatal:
		return nil, MapToolRuntimeFatal(value)
	default:
		return nil, contractResolutionError(ErrReadToolContract)
	}
}

func mapReadToolCompleted(
	result contracts.ToolInvocationCompleted,
	outputSchema contracts.OutputSchema,
	continuation StepContinuation,
) (StepOutcome, error) {
	if result.ToolExecutionID == "" {
		return nil, contractResolutionError(ErrReadToolContract)
	}
	toolExecutionID := result.ToolExecutionID
	update := &ToolResultUpdate{
		ToolExecutionID: toolExecutionID,
		Status:          contracts.ToolExecutionStatusCompleted,
		Truncated:       result.Truncated,
		OriginalSize:    cloneUint64(result.OriginalSize),
		OriginalCount:   cloneUint64(result.OriginalCount),
	}
	if result.ProcessingError != nil {
		if len(result.Output) != 0 || !validReadToolProcessingError(*result.ProcessingError) {
			return nil, contractResolutionError(ErrReadToolContract)
		}
		return StepOutcomeFailed{
			ErrorCode:        *result.ProcessingError,
			CauseCode:        CauseCode(*result.ProcessingError),
			SafeSummary:      "Tool result processing failed.",
			ToolExecutionID:  &toolExecutionID,
			ToolResultUpdate: update,
		}, nil
	}

	safeOutput, processingError := processReadToolOutput(result.Output, outputSchema)
	if processingError != nil {
		return StepOutcomeFailed{
			ErrorCode:        processingError.ErrorCode,
			CauseCode:        processingError.CauseCode,
			SafeSummary:      "Tool result processing failed.",
			ToolExecutionID:  &toolExecutionID,
			ToolResultUpdate: update,
		}, nil
	}
	update.Output = append(json.RawMessage(nil), safeOutput...)
	return StepOutcomeCompleted{
		SafeOutput:       append(json.RawMessage(nil), safeOutput...),
		ToolExecutionID:  &toolExecutionID,
		ToolResultUpdate: update,
		Continuation:     continuation,
	}, nil
}

func mapReadToolBusinessFailed(result contracts.ToolBusinessFailed) (StepOutcomeFailed, bool) {
	causeCode, ok := readToolFailureCause(result.ErrorCode)
	if !ok || !validReadToolSummary(result.SafeSummary) || !validReadToolFailureBoundary(result) {
		return StepOutcomeFailed{}, false
	}
	outcome := StepOutcomeFailed{
		ErrorCode:   result.ErrorCode,
		CauseCode:   causeCode,
		SafeSummary: result.SafeSummary,
	}
	if result.ToolExecutionID == nil {
		return outcome, true
	}
	if *result.ToolExecutionID == "" || *result.ToolExecutionStatus != contracts.ToolExecutionStatusFailed {
		return StepOutcomeFailed{}, false
	}
	toolExecutionID := *result.ToolExecutionID
	errorCode := result.ErrorCode
	outcome.ToolExecutionID = &toolExecutionID
	outcome.ToolResultUpdate = &ToolResultUpdate{
		ToolExecutionID: toolExecutionID,
		Status:          contracts.ToolExecutionStatusFailed,
		ErrorCode:       &errorCode,
	}
	return outcome, true
}

func validReadToolFailureBoundary(result contracts.ToolBusinessFailed) bool {
	hasToolExecution := result.ToolExecutionID != nil || result.ToolExecutionStatus != nil
	if (result.ToolExecutionID == nil) != (result.ToolExecutionStatus == nil) {
		return false
	}
	if hasToolExecution && (*result.ToolExecutionID == "" ||
		*result.ToolExecutionStatus != contracts.ToolExecutionStatusFailed) {
		return false
	}

	switch result.ErrorCode {
	case contracts.ErrorCodeToolConnectionLost, contracts.ErrorCodeToolCallFailed:
		return hasToolExecution
	case contracts.ErrorCodeToolNotFound, contracts.ErrorCodeToolDisabled,
		contracts.ErrorCodeToolNotAuthorized, contracts.ErrorCodeToolInputInvalid,
		contracts.ErrorCodeToolAccessDenied:
		return !hasToolExecution
	case contracts.ErrorCodeToolTimeout:
		return true
	default:
		return false
	}
}

func processReadToolOutput(
	output contracts.SafeToolOutput,
	outputSchema contracts.OutputSchema,
) (json.RawMessage, *StepError) {
	value, err := decodeStrictModelOutput(string(output))
	if err != nil {
		return nil, newStepError(ErrorKindFailed, contracts.ErrorCodeStepOutputInvalid,
			CauseStepOutputInvalid, ErrReadToolOutputInvalid)
	}
	projected := make(map[string]any, len(outputSchema))
	for name, field := range outputSchema {
		actual, exists := value[name]
		if !exists || actual == nil || !matchesOutputType(actual, field.Type) {
			return nil, newStepError(ErrorKindFailed, contracts.ErrorCodeStepOutputInvalid,
				CauseStepOutputInvalid, ErrReadToolOutputInvalid)
		}
		projected[name] = actual
	}
	sanitized, ok := sanitizeModelOutput(projected)
	if !ok || !matchesModelOutputSchema(sanitized, outputSchema) {
		return nil, newStepError(ErrorKindFailed, contracts.ErrorCodeResultSanitizationFailed,
			CauseResultSanitizationFailed, ErrReadToolSanitization)
	}
	encoded, err := json.Marshal(sanitized)
	if err != nil {
		return nil, newStepError(ErrorKindFailed, contracts.ErrorCodeResultSanitizationFailed,
			CauseResultSanitizationFailed, ErrReadToolSanitization)
	}
	if len(encoded) > maxModelStepOutputBytes {
		return nil, newStepError(ErrorKindFailed, contracts.ErrorCodeStepOutputTooLarge,
			CauseStepOutputTooLarge, ErrReadToolOutputTooLarge)
	}
	return append(json.RawMessage(nil), encoded...), nil
}

func readToolFailureCause(errorCode contracts.ErrorCode) (CauseCode, bool) {
	switch errorCode {
	case contracts.ErrorCodeToolNotFound, contracts.ErrorCodeToolDisabled,
		contracts.ErrorCodeToolNotAuthorized, contracts.ErrorCodeToolInputInvalid,
		contracts.ErrorCodeToolAccessDenied, contracts.ErrorCodeToolTimeout,
		contracts.ErrorCodeToolConnectionLost, contracts.ErrorCodeToolCallFailed:
		return CauseCode(errorCode), true
	default:
		return "", false
	}
}

func validReadToolProcessingError(errorCode contracts.ErrorCode) bool {
	switch errorCode {
	case contracts.ErrorCodeStepOutputInvalid, contracts.ErrorCodeResultSanitizationFailed,
		contracts.ErrorCodeStepOutputTooLarge:
		return true
	default:
		return false
	}
}

func validReadToolSummary(summary string) bool {
	return summary != "" && utf8.ValidString(summary) && len(summary) <= maxReadToolSafeSummaryBytes
}

func readToolCancellation(ctx context.Context) StepOutcome {
	if ctx == nil || ctx.Err() == nil {
		return nil
	}
	causeCode := executionCancellationCode(context.Cause(ctx), ctx.Err())
	if causeCode == CauseActionTimeout {
		return StepOutcomeFailed{
			ErrorCode:   contracts.ErrorCodeToolTimeout,
			CauseCode:   CauseToolTimeout,
			SafeSummary: "Read Tool call timed out.",
		}
	}
	if causeCode.Stale() {
		return StepOutcomeStale{CauseCode: causeCode}
	}
	return StepOutcomeStale{CauseCode: CauseStaleExecution}
}

func readToolStaleCause(ctx context.Context) CauseCode {
	if ctx != nil && ctx.Err() != nil {
		causeCode := executionCancellationCode(context.Cause(ctx), ctx.Err())
		if causeCode.Stale() {
			return causeCode
		}
	}
	return CauseStaleExecution
}

func cloneUint64(value *uint64) *uint64 {
	if value == nil {
		return nil
	}
	cloned := *value
	return &cloned
}
