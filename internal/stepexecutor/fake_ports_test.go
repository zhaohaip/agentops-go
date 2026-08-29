package stepexecutor

import (
	"context"
	"encoding/json"
	"errors"
	"sync"

	"github.com/zhaohaip/agentops-go/internal/contracts"
)

var errFakePortContract = errors.New("fake dependency Port contract violation")

type fakeModelResult struct {
	response contracts.ModelResponse
	err      error
}

type fakeModelCall struct {
	context         context.Context
	request         contracts.ModelRequest
	contextCanceled bool
}

type fakeModelClient struct {
	mu      sync.Mutex
	results []fakeModelResult
	calls   []fakeModelCall
}

func (f *fakeModelClient) GenerateStructured(
	ctx context.Context,
	request contracts.ModelRequest,
) (contracts.ModelResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, fakeModelCall{
		context: ctx, request: cloneModelRequest(request), contextCanceled: contextCanceled(ctx),
	})
	if len(f.results) == 0 {
		return contracts.ModelResponse{}, errFakePortContract
	}
	result := f.results[0]
	f.results = f.results[1:]
	return cloneModelResponse(result.response), result.err
}

func (f *fakeModelClient) recordedCalls() []fakeModelCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	result := make([]fakeModelCall, len(f.calls))
	for index := range f.calls {
		result[index] = f.calls[index]
		result[index].request = cloneModelRequest(f.calls[index].request)
	}
	return result
}

type fakeToolResult struct {
	result contracts.ToolFrameworkResult
	err    error
}

type fakeReadToolCall struct {
	context         context.Context
	request         contracts.ReadToolRequest
	contextCanceled bool
}

type fakePrepareWriteCall struct {
	context         context.Context
	request         contracts.PrepareWriteApprovalRequest
	contextCanceled bool
}

type fakeApprovedWriteCall struct {
	context         context.Context
	request         contracts.ApprovedWriteRequest
	contextCanceled bool
}

type fakeToolFrameworkPort struct {
	mu sync.Mutex

	readResults     []fakeToolResult
	prepareResults  []fakeToolResult
	approvedResults []fakeToolResult

	readCalls     []fakeReadToolCall
	prepareCalls  []fakePrepareWriteCall
	approvedCalls []fakeApprovedWriteCall
}

func (f *fakeToolFrameworkPort) InvokeReadTool(
	ctx context.Context,
	request contracts.ReadToolRequest,
) (contracts.ToolFrameworkResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.readCalls = append(f.readCalls, fakeReadToolCall{
		context: ctx, request: cloneReadToolRequest(request), contextCanceled: contextCanceled(ctx),
	})
	result, ok := popFakeToolResult(&f.readResults)
	if !ok || !validFakeToolResult(result, toolMethodRead) {
		return nil, errFakePortContract
	}
	return cloneToolFrameworkResult(result.result), result.err
}

func (f *fakeToolFrameworkPort) PrepareWriteApproval(
	ctx context.Context,
	request contracts.PrepareWriteApprovalRequest,
) (contracts.ToolFrameworkResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.prepareCalls = append(f.prepareCalls, fakePrepareWriteCall{
		context: ctx, request: clonePrepareWriteRequest(request), contextCanceled: contextCanceled(ctx),
	})
	result, ok := popFakeToolResult(&f.prepareResults)
	if !ok || !validFakeToolResult(result, toolMethodPrepare) {
		return nil, errFakePortContract
	}
	return cloneToolFrameworkResult(result.result), result.err
}

func (f *fakeToolFrameworkPort) InvokeApprovedWrite(
	ctx context.Context,
	request contracts.ApprovedWriteRequest,
) (contracts.ToolFrameworkResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.approvedCalls = append(f.approvedCalls, fakeApprovedWriteCall{
		context: ctx, request: cloneApprovedWriteRequest(request), contextCanceled: contextCanceled(ctx),
	})
	result, ok := popFakeToolResult(&f.approvedResults)
	if !ok || !validFakeToolResult(result, toolMethodApproved) {
		return nil, errFakePortContract
	}
	return cloneToolFrameworkResult(result.result), result.err
}

func (f *fakeToolFrameworkPort) recordedReadCalls() []fakeReadToolCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	result := make([]fakeReadToolCall, len(f.readCalls))
	for index := range f.readCalls {
		result[index] = f.readCalls[index]
		result[index].request = cloneReadToolRequest(f.readCalls[index].request)
	}
	return result
}

func (f *fakeToolFrameworkPort) recordedPrepareCalls() []fakePrepareWriteCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	result := make([]fakePrepareWriteCall, len(f.prepareCalls))
	for index := range f.prepareCalls {
		result[index] = f.prepareCalls[index]
		result[index].request = clonePrepareWriteRequest(f.prepareCalls[index].request)
	}
	return result
}

func (f *fakeToolFrameworkPort) recordedApprovedCalls() []fakeApprovedWriteCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	result := make([]fakeApprovedWriteCall, len(f.approvedCalls))
	for index := range f.approvedCalls {
		result[index] = f.approvedCalls[index]
		result[index].request = cloneApprovedWriteRequest(f.approvedCalls[index].request)
	}
	return result
}

type fakeApprovalResult struct {
	result contracts.ApprovalRequestResult
	err    error
}

type fakeApprovalCall struct {
	context         context.Context
	command         contracts.RequestApprovalCommand
	contextCanceled bool
}

type fakeApprovalRequestPort struct {
	mu      sync.Mutex
	results []fakeApprovalResult
	calls   []fakeApprovalCall
}

func (f *fakeApprovalRequestPort) RequestApproval(
	ctx context.Context,
	command contracts.RequestApprovalCommand,
) (contracts.ApprovalRequestResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, fakeApprovalCall{
		context: ctx, command: cloneApprovalCommand(command), contextCanceled: contextCanceled(ctx),
	})
	if len(f.results) == 0 {
		return nil, errFakePortContract
	}
	result := f.results[0]
	f.results = f.results[1:]
	if !validFakeApprovalResult(result) {
		return nil, errFakePortContract
	}
	return cloneApprovalResult(result.result), result.err
}

func (f *fakeApprovalRequestPort) recordedCalls() []fakeApprovalCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	result := make([]fakeApprovalCall, len(f.calls))
	for index := range f.calls {
		result[index] = f.calls[index]
		result[index].command = cloneApprovalCommand(f.calls[index].command)
	}
	return result
}

type toolMethod uint8

const (
	toolMethodRead toolMethod = iota
	toolMethodPrepare
	toolMethodApproved
)

func popFakeToolResult(queue *[]fakeToolResult) (fakeToolResult, bool) {
	if len(*queue) == 0 {
		return fakeToolResult{}, false
	}
	result := (*queue)[0]
	*queue = (*queue)[1:]
	return result, true
}

func validFakeToolResult(result fakeToolResult, method toolMethod) bool {
	if (result.result == nil) == (result.err == nil) {
		return false
	}
	if result.err != nil {
		return true
	}
	switch result.result.(type) {
	case contracts.ToolInvocationCompleted:
		return method == toolMethodRead || method == toolMethodApproved
	case contracts.ToolApprovalPrepared:
		return method == toolMethodPrepare
	case contracts.ToolPreflightRejected:
		return method == toolMethodPrepare || method == toolMethodApproved
	case contracts.ToolBusinessFailed, contracts.ToolDeadlineExceeded, contracts.ToolStale,
		contracts.ToolRuntimeFatal:
		return true
	case contracts.ToolSideEffectUnknown, contracts.ToolCheckpointInvalid:
		return method == toolMethodApproved
	default:
		return false
	}
}

func validFakeApprovalResult(result fakeApprovalResult) bool {
	if (result.result == nil) == (result.err == nil) {
		return false
	}
	if result.err != nil {
		return true
	}
	switch result.result.(type) {
	case contracts.ApprovalRequestPending, contracts.ApprovalRequestExisting,
		contracts.ApprovalRequestConflict, contracts.ApprovalRequestCheckpointInvalid,
		contracts.ApprovalRequestRuntimeFatal:
		return true
	default:
		return false
	}
}

func contextCanceled(ctx context.Context) bool {
	return ctx != nil && ctx.Err() != nil
}

func cloneModelRequest(value contracts.ModelRequest) contracts.ModelRequest {
	cloned := value
	cloned.Messages = append([]contracts.ModelMessage(nil), value.Messages...)
	cloned.Metadata.ExecutionVersion = cloneFakePointer(value.Metadata.ExecutionVersion)
	cloned.Metadata.StepID = cloneFakePointer(value.Metadata.StepID)
	cloned.Metadata.StepType = cloneFakePointer(value.Metadata.StepType)
	cloned.Metadata.ReportID = cloneFakePointer(value.Metadata.ReportID)
	return cloned
}

func cloneModelResponse(value contracts.ModelResponse) contracts.ModelResponse {
	value.ProviderRequestID = cloneFakePointer(value.ProviderRequestID)
	return value
}

func cloneReadToolRequest(value contracts.ReadToolRequest) contracts.ReadToolRequest {
	value.Authorization = cloneAuthorization(value.Authorization)
	value.ResolvedInput = contracts.ResolvedToolInput(cloneBytes(value.ResolvedInput))
	value.ToolDefinition = cloneToolDefinition(value.ToolDefinition)
	return value
}

func clonePrepareWriteRequest(value contracts.PrepareWriteApprovalRequest) contracts.PrepareWriteApprovalRequest {
	value.Authorization = cloneAuthorization(value.Authorization)
	value.ResolvedInput = contracts.ResolvedToolInput(cloneBytes(value.ResolvedInput))
	value.ToolDefinition = cloneToolDefinition(value.ToolDefinition)
	return value
}

func cloneApprovedWriteRequest(value contracts.ApprovedWriteRequest) contracts.ApprovedWriteRequest {
	value.Authorization = cloneAuthorization(value.Authorization)
	value.ApprovedAction = cloneApprovedAction(value.ApprovedAction)
	value.CheckpointEvidence.SourceExecutionVersion = cloneFakePointer(value.CheckpointEvidence.SourceExecutionVersion)
	value.CheckpointEvidence.SourceCheckpointID = cloneFakePointer(value.CheckpointEvidence.SourceCheckpointID)
	value.ToolDefinition = cloneToolDefinition(value.ToolDefinition)
	return value
}

func cloneAuthorization(value contracts.AgentAuthorization) contracts.AgentAuthorization {
	value.AllowedTools = append([]contracts.ToolName(nil), value.AllowedTools...)
	return value
}

func cloneToolDefinition(value contracts.StaticToolDefinition) contracts.StaticToolDefinition {
	value.InputSchema = cloneCanonicalSchema(value.InputSchema)
	value.OutputSchema = cloneCanonicalSchema(value.OutputSchema)
	return value
}

func cloneCanonicalSchema(value contracts.CanonicalJSONSchema) contracts.CanonicalJSONSchema {
	value.AdditionalProperties = cloneFakePointer(value.AdditionalProperties)
	if value.Items != nil {
		items := cloneCanonicalSchema(*value.Items)
		value.Items = &items
	}
	if value.Properties != nil {
		properties := make(map[string]contracts.CanonicalJSONSchema, len(value.Properties))
		for name, child := range value.Properties {
			properties[name] = cloneCanonicalSchema(child)
		}
		value.Properties = properties
	}
	value.Required = append([]string(nil), value.Required...)
	return value
}

func cloneApprovedAction(value contracts.ApprovedAction) contracts.ApprovedAction {
	value.FrozenInput = contracts.FrozenToolInput(cloneBytes(value.FrozenInput))
	value.ObservedValues = contracts.ObservedValues(cloneBytes(value.ObservedValues))
	return value
}

func cloneFrozenRequest(value contracts.FrozenToolRequest) contracts.FrozenToolRequest {
	value.FrozenInput = contracts.FrozenToolInput(cloneBytes(value.FrozenInput))
	value.ObservedValues = contracts.ObservedValues(cloneBytes(value.ObservedValues))
	value.Target.Container = cloneFakePointer(value.Target.Container)
	return value
}

func cloneApprovalCommand(value contracts.RequestApprovalCommand) contracts.RequestApprovalCommand {
	value.FrozenRequest = cloneFrozenRequest(value.FrozenRequest)
	return value
}

func cloneToolFrameworkResult(value contracts.ToolFrameworkResult) contracts.ToolFrameworkResult {
	switch result := value.(type) {
	case contracts.ToolInvocationCompleted:
		result.Output = contracts.SafeToolOutput(cloneBytes(result.Output))
		result.OriginalSize = cloneFakePointer(result.OriginalSize)
		result.OriginalCount = cloneFakePointer(result.OriginalCount)
		result.ProcessingError = cloneFakePointer(result.ProcessingError)
		return result
	case contracts.ToolApprovalPrepared:
		result.FrozenToolRequest = cloneFrozenRequest(result.FrozenToolRequest)
		return result
	case contracts.ToolPreflightRejected:
		return result
	case contracts.ToolBusinessFailed:
		result.ToolExecutionID = cloneFakePointer(result.ToolExecutionID)
		result.ToolExecutionStatus = cloneFakePointer(result.ToolExecutionStatus)
		return result
	case contracts.ToolSideEffectUnknown:
		return result
	case contracts.ToolCheckpointInvalid:
		return result
	case contracts.ToolDeadlineExceeded:
		return result
	case contracts.ToolStale:
		result.ToolExecutionID = cloneFakePointer(result.ToolExecutionID)
		return result
	case contracts.ToolRuntimeFatal:
		return result
	default:
		return nil
	}
}

func cloneApprovalResult(value contracts.ApprovalRequestResult) contracts.ApprovalRequestResult {
	switch result := value.(type) {
	case contracts.ApprovalRequestPending:
		return result
	case contracts.ApprovalRequestExisting:
		return result
	case contracts.ApprovalRequestConflict:
		return result
	case contracts.ApprovalRequestCheckpointInvalid:
		return result
	case contracts.ApprovalRequestRuntimeFatal:
		result.TaskID = cloneFakePointer(result.TaskID)
		result.StepID = cloneFakePointer(result.StepID)
		return result
	default:
		return nil
	}
}

func cloneBytes[T ~[]byte](value T) T {
	if value == nil {
		return nil
	}
	return append(T(nil), value...)
}

func cloneFakePointer[T any](value *T) *T {
	if value == nil {
		return nil
	}
	cloned := *value
	return &cloned
}

func mustJSON(value string) json.RawMessage {
	return json.RawMessage(value)
}
