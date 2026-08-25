package planner

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/zhaohaip/agentops-go/internal/contracts"
	"github.com/zhaohaip/agentops-go/test/fixtures/catalogcontract"
)

func TestPlannerResultEnumsAreClosed(t *testing.T) {
	t.Parallel()
	for _, kind := range []PlannerErrorKind{
		PlannerErrorRuntimeFatal, PlannerErrorCanceled,
		PlannerErrorPlanGenerationFailed, PlannerErrorPlanValidationFailed,
	} {
		if !kind.Valid() {
			t.Fatalf("PlannerErrorKind %q is not valid", kind)
		}
	}
	if PlannerErrorKind("unknown").Valid() || PlannerPhase("unknown").Valid() ||
		PlannerCauseCode("unknown").Valid() || CancellationCause("unknown").Valid() {
		t.Fatal("unknown Planner result enum was accepted")
	}
}

func TestServiceGeneratePlanSuccessUsesOneSnapshot(t *testing.T) {
	t.Parallel()
	request, snapshot := serviceTestRequest(t)
	catalog := &serviceCatalogFake{snapshot: snapshot}
	model := &serviceModelFake{results: []serviceModelResult{{response: contracts.ModelResponse{
		AssistantContent: minimalPlanV1,
	}}}}
	service := NewService(model, catalog)

	result, err := service.GeneratePlan(context.Background(), request)
	if err != nil {
		t.Fatalf("GeneratePlan() error = %v", err)
	}
	if result.TaskID != request.TaskID || result.RunID != request.RunID ||
		result.ExecutionVersion != request.ExecutionVersion || result.Goal == "" || len(result.Steps) != 1 {
		t.Fatalf("GeneratePlan() result = %+v", result)
	}
	if catalog.callCount() != 1 {
		t.Fatalf("Catalog calls = %d, want 1", catalog.callCount())
	}
	calls := model.recordedCalls()
	if len(calls) != 1 {
		t.Fatalf("Model calls = %d, want 1", len(calls))
	}
	assertModelRequest(t, calls[0], request, PlannerPhaseInitial)
	if !strings.Contains(calls[0].Messages[0].Content, snapshot.RegistryVersion) &&
		!strings.Contains(calls[0].Messages[0].Content, "AVAILABLE_TOOLS") {
		t.Fatal("INITIAL Prompt was not built from the loaded Snapshot")
	}
}

func TestServiceRepairResultMatrix(t *testing.T) {
	t.Parallel()
	invalid := `{"goal":"g","steps":[]}`
	tests := []struct {
		name      string
		results   []serviceModelResult
		wantCause PlannerCauseCode
		wantOK    bool
	}{
		{name: "Repair succeeds", results: []serviceModelResult{
			{response: contracts.ModelResponse{AssistantContent: invalid}},
			{response: contracts.ModelResponse{AssistantContent: minimalPlanV1}},
		}, wantOK: true},
		{name: "Repair fails validation", results: []serviceModelResult{
			{response: contracts.ModelResponse{AssistantContent: invalid}},
			{response: contracts.ModelResponse{AssistantContent: invalid}},
		}, wantCause: PlannerCauseRepairExhausted},
		{name: "invalid response Repair succeeds", results: []serviceModelResult{
			{err: contracts.NewModelClientError(contracts.ModelClientErrorInvalidResponse, nil)},
			{response: contracts.ModelResponse{AssistantContent: minimalPlanV1}},
		}, wantOK: true},
		{name: "invalid Repair response is exhausted", results: []serviceModelResult{
			{response: contracts.ModelResponse{AssistantContent: invalid}},
			{err: contracts.NewModelClientError(contracts.ModelClientErrorInvalidResponse, nil)},
		}, wantCause: PlannerCauseRepairExhausted},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			request, snapshot := serviceTestRequest(t)
			model := &serviceModelFake{results: test.results}
			result, err := NewService(model, &serviceCatalogFake{snapshot: snapshot}).GeneratePlan(
				context.Background(), request,
			)
			if test.wantOK {
				if err != nil || result.Goal == "" {
					t.Fatalf("GeneratePlan() = %+v, %v", result, err)
				}
			} else {
				assertPlannerError(t, err, PlannerErrorPlanValidationFailed, test.wantCause, PlannerPhaseRepair)
				if !reflect.ValueOf(result).IsZero() {
					t.Fatalf("failure returned partial result: %+v", result)
				}
			}
			calls := model.recordedCalls()
			if len(calls) != 2 {
				t.Fatalf("Model calls = %d, want exactly 2", len(calls))
			}
			assertModelRequest(t, calls[0], request, PlannerPhaseInitial)
			assertModelRequest(t, calls[1], request, PlannerPhaseRepair)
		})
	}
}

func TestServiceMapsModelErrorsWithoutRepair(t *testing.T) {
	t.Parallel()
	tests := []struct {
		kind      contracts.ModelClientErrorKind
		wantKind  PlannerErrorKind
		wantCause PlannerCauseCode
	}{
		{contracts.ModelClientErrorTimeout, PlannerErrorPlanGenerationFailed, PlannerCauseModelProviderTimeout},
		{contracts.ModelClientErrorAuthentication, PlannerErrorPlanGenerationFailed, PlannerCauseModelAuthentication},
		{contracts.ModelClientErrorNetwork, PlannerErrorPlanGenerationFailed, PlannerCauseModelNetwork},
		{contracts.ModelClientErrorRateLimited, PlannerErrorPlanGenerationFailed, PlannerCauseModelRateLimited},
		{contracts.ModelClientErrorProvider, PlannerErrorPlanGenerationFailed, PlannerCauseModelProviderError},
		{contracts.ModelClientErrorResponseTooLarge, PlannerErrorPlanGenerationFailed, PlannerCauseModelResponseTooLarge},
		{contracts.ModelClientErrorContractViolation, PlannerErrorRuntimeFatal, PlannerCauseRuntimeInvalidModelClientRequest},
	}
	for _, test := range tests {
		t.Run(string(test.kind), func(t *testing.T) {
			request, snapshot := serviceTestRequest(t)
			model := &serviceModelFake{results: []serviceModelResult{{
				err: contracts.NewModelClientError(test.kind, errors.New("provider secret")),
			}}}
			result, err := NewService(model, &serviceCatalogFake{snapshot: snapshot}).GeneratePlan(
				context.Background(), request,
			)
			assertPlannerError(t, err, test.wantKind, test.wantCause, PlannerPhaseInitial)
			if strings.Contains(err.Error(), "provider secret") || !reflect.ValueOf(result).IsZero() {
				t.Fatalf("unsafe or partial failure = %+v, %v", result, err)
			}
			if len(model.recordedCalls()) != 1 {
				t.Fatal("model error was retried")
			}
		})
	}
}

func TestServiceRejectsInvalidRequestAndCatalogAsRuntimeFatal(t *testing.T) {
	t.Parallel()
	request, snapshot := serviceTestRequest(t)
	request.ExecutionConfigHash = "invalid"
	model := &serviceModelFake{}
	catalog := &serviceCatalogFake{snapshot: snapshot}
	_, err := NewService(model, catalog).GeneratePlan(context.Background(), request)
	assertPlannerError(t, err, PlannerErrorRuntimeFatal, PlannerCauseRuntimeInvalidRequest, PlannerPhaseInitial)
	if catalog.callCount() != 0 || len(model.recordedCalls()) != 0 {
		t.Fatal("invalid request called an external Port")
	}

	request, _ = serviceTestRequest(t)
	kind := contracts.PlanningToolCatalogErrorConfigVersionMismatch
	catalog = &serviceCatalogFake{err: contracts.NewPlanningToolCatalogError(
		kind, nil, contracts.CauseCodeRuntimeStaticToolSnapshotInconsistent, nil,
	)}
	_, err = NewService(model, catalog).GeneratePlan(context.Background(), request)
	typed := assertPlannerError(t, err, PlannerErrorRuntimeFatal,
		PlannerCauseRuntimeStaticToolSnapshotInconsistent, PlannerPhaseInitial)
	if typed.CatalogKind == nil || *typed.CatalogKind != kind || len(model.recordedCalls()) != 0 {
		t.Fatalf("Catalog classification = %+v; model calls = %d", typed.CatalogKind, len(model.recordedCalls()))
	}
}

func TestServiceTaskInputLimitIsTaskLocal(t *testing.T) {
	t.Parallel()
	request, snapshot := serviceTestRequest(t)
	request.TaskInput = strings.Repeat("x", maxTaskInputBytes+1)
	model := &serviceModelFake{}
	catalog := &serviceCatalogFake{snapshot: snapshot}
	_, err := NewService(model, catalog).GeneratePlan(context.Background(), request)
	assertPlannerError(t, err, PlannerErrorPlanGenerationFailed,
		PlannerCauseTaskInputTooLarge, PlannerPhaseInitial)
	if catalog.callCount() != 0 || len(model.recordedCalls()) != 0 {
		t.Fatal("oversized Task input called an external Port")
	}
}

func TestServiceCancellationWinsOverLateModelResult(t *testing.T) {
	t.Parallel()
	request, snapshot := serviceTestRequest(t)
	entered := make(chan struct{})
	release := make(chan struct{})
	model := &serviceModelFake{call: func(context.Context, contracts.ModelRequest) (contracts.ModelResponse, error) {
		close(entered)
		<-release
		return contracts.ModelResponse{AssistantContent: minimalPlanV1}, nil
	}}
	ctx, cancel := context.WithCancelCause(context.Background())
	resultDone := make(chan struct {
		result ValidatedPlanDraft
		err    error
	}, 1)
	go func() {
		result, err := NewService(model, &serviceCatalogFake{snapshot: snapshot}).GeneratePlan(ctx, request)
		resultDone <- struct {
			result ValidatedPlanDraft
			err    error
		}{result: result, err: err}
	}()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("model call did not start")
	}
	cancel(CancellationCause(PlannerCauseRuntimeShutdown))
	close(release)
	select {
	case got := <-resultDone:
		assertPlannerError(t, got.err, PlannerErrorCanceled, PlannerCauseRuntimeShutdown, PlannerPhaseInitial)
		assertContextErrorChain(t, got.err, context.Canceled)
		if !reflect.ValueOf(got.result).IsZero() {
			t.Fatalf("late result escaped: %+v", got.result)
		}
	case <-time.After(time.Second):
		t.Fatal("GeneratePlan did not return after cancellation")
	}
}

func TestServicePreservesStandardContextErrorChain(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name      string
		context   func() (context.Context, context.CancelFunc)
		wantCause PlannerCauseCode
		wantError error
	}{
		{
			name: "ordinary cancellation",
			context: func() (context.Context, context.CancelFunc) {
				ctx, cancel := context.WithCancel(context.Background())
				cancel()
				return ctx, func() {}
			},
			wantCause: PlannerCauseRuntimeShutdown,
			wantError: context.Canceled,
		},
		{
			name: "deadline",
			context: func() (context.Context, context.CancelFunc) {
				return context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
			},
			wantCause: PlannerCauseActionTimeout,
			wantError: context.DeadlineExceeded,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			request, snapshot := serviceTestRequest(t)
			ctx, cancel := test.context()
			defer cancel()
			_, err := NewService(&serviceModelFake{}, &serviceCatalogFake{snapshot: snapshot}).GeneratePlan(ctx, request)
			assertPlannerError(t, err, PlannerErrorCanceled, test.wantCause, PlannerPhaseInitial)
			assertContextErrorChain(t, err, test.wantError)
		})
	}
}

func TestServiceCancellationCausePrecedesAdapterTimeout(t *testing.T) {
	t.Parallel()
	for _, cause := range []PlannerCauseCode{
		PlannerCauseTaskCancelled, PlannerCauseTaskTimedOut, PlannerCauseActionTimeout,
	} {
		t.Run(string(cause), func(t *testing.T) {
			request, snapshot := serviceTestRequest(t)
			ctx, cancel := context.WithCancelCause(context.Background())
			model := &serviceModelFake{call: func(context.Context, contracts.ModelRequest) (contracts.ModelResponse, error) {
				cancel(CancellationCause(cause))
				return contracts.ModelResponse{}, contracts.NewModelClientError(
					contracts.ModelClientErrorTimeout, context.DeadlineExceeded,
				)
			}}
			_, err := NewService(model, &serviceCatalogFake{snapshot: snapshot}).GeneratePlan(ctx, request)
			assertPlannerError(t, err, PlannerErrorCanceled, cause, PlannerPhaseInitial)
			assertContextErrorChain(t, err, context.Canceled)
		})
	}
}

func TestServiceAdapterCanceledPreservesOnlyContextSentinel(t *testing.T) {
	t.Parallel()
	request, snapshot := serviceTestRequest(t)
	providerError := errors.New("provider secret")
	model := &serviceModelFake{results: []serviceModelResult{{
		err: contracts.NewModelClientError(
			contracts.ModelClientErrorCanceled,
			errors.Join(providerError, context.Canceled),
		),
	}}}
	_, err := NewService(model, &serviceCatalogFake{snapshot: snapshot}).GeneratePlan(
		context.Background(), request,
	)
	typed := assertPlannerError(t, err, PlannerErrorCanceled,
		PlannerCauseRuntimeShutdown, PlannerPhaseInitial)
	assertContextErrorChain(t, err, context.Canceled)
	if errors.Is(err, providerError) || typed.Unwrap() != context.Canceled ||
		strings.Contains(err.Error(), providerError.Error()) {
		t.Fatal("PlannerError exposed the Provider error instead of the safe Context sentinel")
	}
}

func TestServiceAppliesBoundedModelCallTimeout(t *testing.T) {
	t.Parallel()
	request, snapshot := serviceTestRequest(t)
	modelEntered := make(chan struct{})
	model := &serviceModelFake{call: func(ctx context.Context, _ contracts.ModelRequest) (contracts.ModelResponse, error) {
		close(modelEntered)
		<-ctx.Done()
		return contracts.ModelResponse{}, contracts.NewModelClientError(
			contracts.ModelClientErrorTimeout, context.DeadlineExceeded,
		)
	}}
	service := NewService(model, &serviceCatalogFake{snapshot: snapshot})
	service.modelCallTimeout = 10 * time.Millisecond
	done := make(chan error, 1)
	go func() {
		_, err := service.GeneratePlan(context.Background(), request)
		done <- err
	}()
	select {
	case <-modelEntered:
	case <-time.After(time.Second):
		t.Fatal("model call did not start")
	}
	select {
	case err := <-done:
		assertPlannerError(t, err, PlannerErrorPlanGenerationFailed,
			PlannerCauseModelCallTimeout, PlannerPhaseInitial)
	case <-time.After(time.Second):
		t.Fatal("Planner model timeout was not bounded")
	}
}

func TestServiceRepairHonorsRemainingBudget(t *testing.T) {
	t.Parallel()
	request, snapshot := serviceTestRequest(t)
	model := &serviceModelFake{results: []serviceModelResult{{response: contracts.ModelResponse{
		AssistantContent: `{"goal":"g","steps":[]}`,
	}}}}
	ctx, cancel := context.WithTimeout(context.Background(), repairMinModelBudget+plannerLocalSafetyMargin)
	defer cancel()
	_, err := NewService(model, &serviceCatalogFake{snapshot: snapshot}).GeneratePlan(ctx, request)
	assertPlannerError(t, err, PlannerErrorPlanValidationFailed,
		PlannerCauseRepairBudgetInsufficient, PlannerPhaseRepair)
	if len(model.recordedCalls()) != 1 {
		t.Fatal("insufficient Repair budget started a second model call")
	}
}

func serviceTestRequest(t *testing.T) (PlannerRequest, contracts.PlanningToolSnapshot) {
	t.Helper()
	fixture := catalogcontract.FixedFixture()
	selector := catalogcontract.SelectorFor(t, fixture, catalogcontract.FixedCatalogID, []string{})
	snapshot := catalogcontract.SnapshotFor(t, fixture, catalogcontract.FixedCatalogID, []string{})
	return PlannerRequest{
		TaskID: "task-1", RunID: "run-1", ExecutionVersion: 1,
		TaskInput: "Inspect the deployment.", AgentID: "agent-1",
		AgentSystemPrompt: "You are a bounded operations planner.",
		AllowedTools:      []string{}, MaxSteps: 4, ModelName: plannerModelName,
		GenerationParams: contracts.GenerationParams{
			Temperature: contracts.NewCanonicalDecimalV1(2, 1),
			TopP:        contracts.NewCanonicalDecimalV1(1, 0), MaxOutputTokens: 1024,
		},
		ExecutionConfigHash: contracts.ExecutionConfigHash(strings.Repeat("a", 64)), ToolCatalogSelector: selector,
	}, snapshot
}

type serviceModelResult struct {
	response contracts.ModelResponse
	err      error
}

type serviceModelFake struct {
	mu      sync.Mutex
	results []serviceModelResult
	calls   []contracts.ModelRequest
	call    func(context.Context, contracts.ModelRequest) (contracts.ModelResponse, error)
}

func (f *serviceModelFake) GenerateStructured(
	ctx context.Context,
	request contracts.ModelRequest,
) (contracts.ModelResponse, error) {
	f.mu.Lock()
	f.calls = append(f.calls, cloneModelRequest(request))
	call := f.call
	if call == nil {
		if len(f.results) == 0 {
			f.mu.Unlock()
			return contracts.ModelResponse{}, errors.New("unexpected Model call")
		}
		result := f.results[0]
		f.results = f.results[1:]
		f.mu.Unlock()
		return result.response, result.err
	}
	f.mu.Unlock()
	return call(ctx, request)
}

func (f *serviceModelFake) recordedCalls() []contracts.ModelRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	result := make([]contracts.ModelRequest, len(f.calls))
	for index := range f.calls {
		result[index] = cloneModelRequest(f.calls[index])
	}
	return result
}

func cloneModelRequest(request contracts.ModelRequest) contracts.ModelRequest {
	clone := request
	clone.Messages = append([]contracts.ModelMessage(nil), request.Messages...)
	return clone
}

type serviceCatalogFake struct {
	mu       sync.Mutex
	snapshot contracts.PlanningToolSnapshot
	err      error
	calls    int
}

func (f *serviceCatalogFake) LoadPlanningToolSnapshot(
	context.Context,
	contracts.PlanningToolCatalogSelector,
) (contracts.PlanningToolSnapshot, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	return cloneFakeSnapshot(f.snapshot), f.err
}

func (f *serviceCatalogFake) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

func assertModelRequest(t *testing.T, got contracts.ModelRequest, request PlannerRequest, phase PlannerPhase) {
	t.Helper()
	if got.Model != plannerModelName || got.Stream || got.ResponseFormat != plannerResponseFormat ||
		got.Metadata.Operation != "GeneratePlan" || got.Metadata.Phase != string(phase) ||
		got.Metadata.TaskID != request.TaskID || got.Metadata.RunID != request.RunID ||
		got.Metadata.ExecutionVersion == nil || *got.Metadata.ExecutionVersion != request.ExecutionVersion ||
		!reflect.DeepEqual(got.GenerationParams, request.GenerationParams) || len(got.Messages) != 1 {
		t.Fatalf("ModelRequest = %+v", got)
	}
}

func assertPlannerError(
	t *testing.T,
	err error,
	wantKind PlannerErrorKind,
	wantCause PlannerCauseCode,
	wantPhase PlannerPhase,
) *PlannerError {
	t.Helper()
	var typed *PlannerError
	if !errors.As(err, &typed) || typed == nil || typed.Kind != wantKind ||
		typed.CauseCode != wantCause || typed.Phase != wantPhase {
		t.Fatalf("error = %#v, want %s/%s/%s", err, wantKind, wantCause, wantPhase)
	}
	return typed
}

func assertContextErrorChain(t *testing.T, err error, want error) {
	t.Helper()
	if !errors.Is(err, want) {
		t.Fatalf("errors.Is(%v, %v) = false", err, want)
	}
	other := context.Canceled
	if want == context.Canceled {
		other = context.DeadlineExceeded
	}
	if errors.Is(err, other) {
		t.Fatalf("errors.Is(%v, %v) = true, want false", err, other)
	}
}
