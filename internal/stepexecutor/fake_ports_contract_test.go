package stepexecutor

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/zhaohaip/agentops-go/internal/contracts"
)

type contextKey string

func TestFakeModelClientFIFODeepCopyAndContextPropagation(t *testing.T) {
	providerID := "provider-1"
	fake := &fakeModelClient{results: []fakeModelResult{
		{response: contracts.ModelResponse{AssistantContent: `{"first":true}`, ProviderRequestID: &providerID}},
		{err: errors.New("second")},
	}}
	stepID := contracts.StepID("step-1")
	request := contracts.ModelRequest{
		Messages: []contracts.ModelMessage{{Role: contracts.ModelMessageRoleUser, Content: "original"}},
		Metadata: contracts.ModelRequestMetadata{StepID: &stepID},
	}
	ctx := context.WithValue(context.Background(), contextKey("trace"), "trace-1")

	first, err := fake.GenerateStructured(ctx, request)
	if err != nil || first.AssistantContent != `{"first":true}` {
		t.Fatalf("first result = %+v, %v", first, err)
	}
	if _, err = fake.GenerateStructured(ctx, request); err == nil || err.Error() != "second" {
		t.Fatalf("second error = %v", err)
	}

	request.Messages[0].Content = "mutated"
	*request.Metadata.StepID = "mutated"
	*first.ProviderRequestID = "mutated"
	calls := fake.recordedCalls()
	if len(calls) != 2 || calls[0].request.Messages[0].Content != "original" ||
		*calls[0].request.Metadata.StepID != "step-1" {
		t.Fatalf("recorded Model calls were not deep-copied: %+v", calls)
	}
	if calls[0].context != ctx || calls[0].context.Value(contextKey("trace")) != "trace-1" || calls[0].contextCanceled {
		t.Fatalf("Model context was not propagated: %+v", calls[0])
	}
	calls[0].request.Messages[0].Content = "changed-copy"
	if fake.recordedCalls()[0].request.Messages[0].Content != "original" {
		t.Fatal("recordedCalls leaked Fake Model storage")
	}
}

func TestFakeToolFrameworkFIFODeepCopyContextAndAllowedUnions(t *testing.T) {
	readRequest, prepareRequest, approvedRequest := fakeToolRequests()
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	fake := &fakeToolFrameworkPort{
		readResults: []fakeToolResult{
			{result: contracts.ToolInvocationCompleted{ToolExecutionID: "tool-execution-1", Output: contracts.SafeToolOutput(`{"ok":true}`)}},
			{result: contracts.ToolStale{}},
		},
		prepareResults: []fakeToolResult{{result: contracts.ToolApprovalPrepared{
			FrozenToolRequest: fakeFrozenRequest(),
		}}},
		approvedResults: []fakeToolResult{{result: contracts.ToolSideEffectUnknown{
			ToolExecutionID: "tool-execution-2", ErrorCode: contracts.ErrorCodeWriteToolInterrupted,
			SafeSummary: "unknown", SideEffectUnknown: true,
		}}},
	}

	if result, err := fake.InvokeReadTool(canceled, readRequest); err != nil {
		t.Fatalf("InvokeReadTool() error = %v", err)
	} else if _, ok := result.(contracts.ToolInvocationCompleted); !ok {
		t.Fatalf("InvokeReadTool() result = %T", result)
	}
	if result, err := fake.InvokeReadTool(context.Background(), readRequest); err != nil {
		t.Fatalf("second InvokeReadTool() error = %v", err)
	} else if _, ok := result.(contracts.ToolStale); !ok {
		t.Fatalf("second InvokeReadTool() result = %T", result)
	}
	if result, err := fake.PrepareWriteApproval(canceled, prepareRequest); err != nil {
		t.Fatalf("PrepareWriteApproval() error = %v", err)
	} else if _, ok := result.(contracts.ToolApprovalPrepared); !ok {
		t.Fatalf("PrepareWriteApproval() result = %T", result)
	}
	if result, err := fake.InvokeApprovedWrite(canceled, approvedRequest); err != nil {
		t.Fatalf("InvokeApprovedWrite() error = %v", err)
	} else if _, ok := result.(contracts.ToolSideEffectUnknown); !ok {
		t.Fatalf("InvokeApprovedWrite() result = %T", result)
	}

	readRequest.Authorization.AllowedTools[0] = "mutated"
	readRequest.ResolvedInput[2] = 'X'
	readRequest.ToolDefinition.InputSchema.Properties["cluster"] = contracts.CanonicalJSONSchema{}
	approvedRequest.ApprovedAction.FrozenInput[2] = 'X'
	*approvedRequest.CheckpointEvidence.SourceCheckpointID = "mutated"

	readCalls := fake.recordedReadCalls()
	if len(readCalls) != 2 || !readCalls[0].contextCanceled ||
		readCalls[0].request.Authorization.AllowedTools[0] != "k8s.get_deployment" ||
		string(readCalls[0].request.ResolvedInput) != `{"cluster":"prod"}` ||
		readCalls[0].request.ToolDefinition.InputSchema.Properties["cluster"].Type != contracts.JSONSchemaTypeString {
		t.Fatalf("read request copy/context = %+v", readCalls)
	}
	approvedCalls := fake.recordedApprovedCalls()
	if len(approvedCalls) != 1 || !approvedCalls[0].contextCanceled ||
		string(approvedCalls[0].request.ApprovedAction.FrozenInput) != `{"replicas":3}` ||
		*approvedCalls[0].request.CheckpointEvidence.SourceCheckpointID != "source-checkpoint-1" {
		t.Fatalf("approved request copy/context = %+v", approvedCalls)
	}
	prepareCalls := fake.recordedPrepareCalls()
	if len(prepareCalls) != 1 || !prepareCalls[0].contextCanceled ||
		prepareCalls[0].request.Scope.ExecutionConfigHash != prepareRequest.Scope.ExecutionConfigHash {
		t.Fatalf("prepare request copy/context = %+v", prepareCalls)
	}

	readCalls[0].request.Authorization.AllowedTools[0] = "changed-copy"
	if fake.recordedReadCalls()[0].request.Authorization.AllowedTools[0] != "k8s.get_deployment" {
		t.Fatal("recordedReadCalls leaked Fake Tool storage")
	}
}

func TestFakeToolFrameworkRejectsInvalidResultPairsAndMethodBranches(t *testing.T) {
	readRequest, prepareRequest, _ := fakeToolRequests()
	systemErr := errors.New("database outcome unknown")
	tests := []struct {
		name string
		fake *fakeToolFrameworkPort
		call func(*fakeToolFrameworkPort) error
	}{
		{
			name: "empty pair",
			fake: &fakeToolFrameworkPort{readResults: []fakeToolResult{{}}},
			call: func(fake *fakeToolFrameworkPort) error {
				_, err := fake.InvokeReadTool(context.Background(), readRequest)
				return err
			},
		},
		{
			name: "result and error",
			fake: &fakeToolFrameworkPort{readResults: []fakeToolResult{{
				result: contracts.ToolStale{}, err: systemErr,
			}}},
			call: func(fake *fakeToolFrameworkPort) error {
				_, err := fake.InvokeReadTool(context.Background(), readRequest)
				return err
			},
		},
		{
			name: "branch not allowed by method",
			fake: &fakeToolFrameworkPort{prepareResults: []fakeToolResult{{
				result: contracts.ToolInvocationCompleted{},
			}}},
			call: func(fake *fakeToolFrameworkPort) error {
				_, err := fake.PrepareWriteApproval(context.Background(), prepareRequest)
				return err
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if err := test.call(test.fake); !errors.Is(err, errFakePortContract) {
				t.Fatalf("error = %v, want Fake contract violation", err)
			}
		})
	}
}

func TestFakeApprovalFIFODeepCopyContextAndAllowedUnions(t *testing.T) {
	command := fakeApprovalCommand()
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	fake := &fakeApprovalRequestPort{results: []fakeApprovalResult{
		{result: contracts.ApprovalRequestPending{ApprovalID: "approval-1"}},
		{result: contracts.ApprovalRequestExisting{ApprovalID: "approval-1"}},
		{result: contracts.ApprovalRequestConflict{CauseCode: contracts.CauseCodeTaskTimeout}},
		{result: contracts.ApprovalRequestCheckpointInvalid{ErrorCode: contracts.ErrorCodeCheckpointInvalid}},
		{result: contracts.ApprovalRequestRuntimeFatal{CauseCode: contracts.CauseCodePersistenceInvariantViolation}},
	}}

	wantTypes := []reflect.Type{
		reflect.TypeOf(contracts.ApprovalRequestPending{}),
		reflect.TypeOf(contracts.ApprovalRequestExisting{}),
		reflect.TypeOf(contracts.ApprovalRequestConflict{}),
		reflect.TypeOf(contracts.ApprovalRequestCheckpointInvalid{}),
		reflect.TypeOf(contracts.ApprovalRequestRuntimeFatal{}),
	}
	for index, wantType := range wantTypes {
		result, err := fake.RequestApproval(canceled, command)
		if err != nil || reflect.TypeOf(result) != wantType {
			t.Fatalf("result %d = %T, %v; want %v", index, result, err, wantType)
		}
	}

	command.FrozenRequest.FrozenInput[2] = 'X'
	command.FrozenRequest.ObservedValues[2] = 'X'
	*command.FrozenRequest.Target.Container = "mutated"
	calls := fake.recordedCalls()
	if len(calls) != 5 || !calls[0].contextCanceled ||
		string(calls[0].command.FrozenRequest.FrozenInput) != `{"replicas":3}` ||
		string(calls[0].command.FrozenRequest.ObservedValues) != `{"replicas":2}` ||
		*calls[0].command.FrozenRequest.Target.Container != "web" {
		t.Fatalf("Approval command copy/context = %+v", calls[0])
	}
	calls[0].command.FrozenRequest.FrozenInput[2] = 'Y'
	if string(fake.recordedCalls()[0].command.FrozenRequest.FrozenInput) != `{"replicas":3}` {
		t.Fatal("recordedCalls leaked Fake Approval storage")
	}
}

func TestFakeApprovalRejectsInvalidResultPairs(t *testing.T) {
	command := fakeApprovalCommand()
	fake := &fakeApprovalRequestPort{results: []fakeApprovalResult{{
		result: contracts.ApprovalRequestPending{}, err: errors.New("unexpected"),
	}}}
	if _, err := fake.RequestApproval(context.Background(), command); !errors.Is(err, errFakePortContract) {
		t.Fatalf("error = %v, want Fake contract violation", err)
	}
}

func fakeToolRequests() (
	contracts.ReadToolRequest,
	contracts.PrepareWriteApprovalRequest,
	contracts.ApprovedWriteRequest,
) {
	scope := contracts.ExecutionScope{
		TaskID: "task-1", RunID: "run-1", ExecutionVersion: 2,
		ExecutionConfigHash: contracts.ExecutionConfigHash("aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"),
		WorkerID:            "worker-1", StepID: "step-1", DeadlineAt: time.Unix(100, 0).UTC(),
	}
	authorization := contracts.AgentAuthorization{
		AgentID: "agent-1", AllowedTools: []contracts.ToolName{"k8s.get_deployment"},
	}
	definition := contracts.StaticToolDefinition{
		Name: "k8s.get_deployment", Enabled: true,
		InputSchema: contracts.CanonicalJSONSchema{
			Type: contracts.JSONSchemaTypeObject,
			Properties: map[string]contracts.CanonicalJSONSchema{
				"cluster": {Type: contracts.JSONSchemaTypeString},
			},
			Required: []string{"cluster"},
		},
		OutputSchema: contracts.CanonicalJSONSchema{Type: contracts.JSONSchemaTypeObject},
		RiskLevel:    contracts.RiskLevelLow, ReadOnly: true, TimeoutMS: 1000,
	}
	read := contracts.ReadToolRequest{
		Scope: scope, Authorization: authorization, ToolName: definition.Name,
		ResolvedInput: contracts.ResolvedToolInput(`{"cluster":"prod"}`), ToolDefinition: definition,
	}
	prepare := contracts.PrepareWriteApprovalRequest{
		Scope: scope, Authorization: authorization, ToolName: "k8s.patch_deployment",
		ResolvedInput: contracts.ResolvedToolInput(`{"replicas":3}`), ToolDefinition: definition,
	}
	sourceVersion := contracts.ExecutionVersion(1)
	sourceCheckpointID := contracts.CheckpointID("source-checkpoint-1")
	approved := contracts.ApprovedWriteRequest{
		Scope: scope, Authorization: authorization,
		ApprovedAction: contracts.ApprovedAction{
			ApprovalID: "approval-1", FrozenInput: contracts.FrozenToolInput(`{"replicas":3}`),
			ObservedValues: contracts.ObservedValues(`{"replicas":2}`),
		},
		CheckpointEvidence: contracts.ApprovedCheckpointEvidence{
			SourceExecutionVersion: &sourceVersion, SourceCheckpointID: &sourceCheckpointID,
		},
		ToolDefinition: definition,
	}
	return read, prepare, approved
}

func fakeFrozenRequest() contracts.FrozenToolRequest {
	container := "web"
	return contracts.FrozenToolRequest{
		TaskID: "task-1", RunID: "run-1", ExecutionVersion: 2, StepID: "step-1",
		ToolName: "k8s.patch_deployment", RiskLevel: contracts.RiskLevelHigh,
		FrozenInput:         contracts.FrozenToolInput(`{"replicas":3}`),
		ObservedValues:      contracts.ObservedValues(`{"replicas":2}`),
		Target:              contracts.ToolTarget{Cluster: "prod", Namespace: "default", Deployment: "web", Container: &container},
		ExecutionConfigHash: contracts.ExecutionConfigHash("aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"),
	}
}

func fakeApprovalCommand() contracts.RequestApprovalCommand {
	frozen := fakeFrozenRequest()
	return contracts.RequestApprovalCommand{
		Scope: contracts.ExecutionScope{
			TaskID: frozen.TaskID, RunID: frozen.RunID, ExecutionVersion: frozen.ExecutionVersion,
			ExecutionConfigHash: frozen.ExecutionConfigHash, WorkerID: "worker-1", StepID: frozen.StepID,
		},
		FrozenRequest: frozen, StepID: frozen.StepID, ExecutionConfigHash: frozen.ExecutionConfigHash,
		ApprovalContext: contracts.ApprovalRequestContext{
			NextAction: contracts.CheckpointNextActionRequestApproval,
			ToolName:   frozen.ToolName, RiskLevel: contracts.RiskLevelHigh, ReadOnly: false,
		},
	}
}
