package planner

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"sync"
	"testing"

	"github.com/zhaohaip/agentops-go/internal/contracts"
	"github.com/zhaohaip/agentops-go/test/fixtures/catalogcontract"
)

func TestStrictPlannerCatalogFakeContract(t *testing.T) {
	catalogcontract.Run(t, plannerCatalogFakeFactory{}, validateCatalogSnapshotWithPlanner)
}

func TestStrictPlannerCatalogFakeFIFOAndIsolation(t *testing.T) {
	t.Parallel()

	fixture := catalogcontract.FixedFixture()
	snapshot := catalogcontract.SnapshotFor(
		t,
		fixture,
		catalogcontract.FixedCatalogID,
		[]string{"k8s.get_deployment", "k8s.get_pod"},
	)
	wantErr := errors.New("queued Catalog failure")
	toolName := "tool.missing"
	typedErr := contracts.NewPlanningToolCatalogError(
		contracts.PlanningToolCatalogErrorToolNotFound,
		&toolName,
		contracts.CauseCodeRuntimeStaticToolSnapshotInconsistent,
		nil,
	)
	fake := plannerCatalogFakeFactory{}.New(t, catalogcontract.Scenario{
		Fixture: fixture,
		Responses: []catalogcontract.Response{
			{Snapshot: snapshot},
			{Snapshot: snapshot},
			{Err: wantErr},
			{Err: typedErr},
			{Err: typedErr},
		},
	}).(*strictPlannerCatalogFake)
	selector := catalogcontract.SelectorFor(
		t,
		fixture,
		catalogcontract.FixedCatalogID,
		[]string{"k8s.get_deployment", "k8s.get_pod"},
	)

	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	if got, err := fake.LoadPlanningToolSnapshot(canceled, selector); !errors.Is(err, context.Canceled) ||
		!planningToolSnapshotIsZero(got) {
		t.Fatalf("canceled result = (%+v, %v)", got, err)
	}
	first, err := fake.LoadPlanningToolSnapshot(context.Background(), selector)
	if err != nil {
		t.Fatalf("first FIFO result: %v", err)
	}
	first.Tools[0].Description = "caller mutation"
	first.Tools[0].InputSchema.Properties["poison"] = contracts.CanonicalJSONSchema{
		Type: contracts.JSONSchemaTypeString,
	}
	second, err := fake.LoadPlanningToolSnapshot(context.Background(), selector)
	if err != nil {
		t.Fatalf("second FIFO result: %v", err)
	}
	if second.Tools[0].Description == "caller mutation" ||
		second.Tools[0].InputSchema.Properties["poison"].Type != "" {
		t.Fatal("Fake returned shared mutable Snapshot state")
	}
	if got, err := fake.LoadPlanningToolSnapshot(context.Background(), selector); !errors.Is(err, wantErr) ||
		!planningToolSnapshotIsZero(got) {
		t.Fatalf("third FIFO result = (%+v, %v), want queued error", got, err)
	}
	_, firstTypedErr := fake.LoadPlanningToolSnapshot(context.Background(), selector)
	var firstTyped *contracts.PlanningToolCatalogError
	if !errors.As(firstTypedErr, &firstTyped) || firstTyped.ToolName == nil {
		t.Fatalf("fourth FIFO result = %v, want typed error", firstTypedErr)
	}
	*firstTyped.ToolName = "caller mutation"
	_, secondTypedErr := fake.LoadPlanningToolSnapshot(context.Background(), selector)
	var secondTyped *contracts.PlanningToolCatalogError
	if !errors.As(secondTypedErr, &secondTyped) || secondTyped.ToolName == nil ||
		*secondTyped.ToolName != "tool.missing" {
		t.Fatalf("Fake returned shared mutable error state: %v", secondTypedErr)
	}

	selector.AllowedTools[0] = "caller mutation"
	calls := fake.recordedCalls()
	if len(calls) != 6 || !calls[0].canceled || calls[1].canceled ||
		calls[0].selector.AllowedTools[0] == "caller mutation" {
		t.Fatalf("Fake call recording/context state = %+v", calls)
	}
}

type plannerCatalogFakeFactory struct{}

func (plannerCatalogFakeFactory) New(
	_ testing.TB,
	scenario catalogcontract.Scenario,
) contracts.PlanningToolCatalogPort {
	responses := make([]catalogcontract.Response, len(scenario.Responses))
	for index, response := range scenario.Responses {
		responses[index] = response
		responses[index].Snapshot = cloneFakeSnapshot(response.Snapshot)
		responses[index].Err = cloneFakeError(response.Err)
	}
	return &strictPlannerCatalogFake{responses: responses}
}

type catalogFakeCall struct {
	selector contracts.PlanningToolCatalogSelector
	canceled bool
}

type strictPlannerCatalogFake struct {
	mu        sync.Mutex
	responses []catalogcontract.Response
	calls     []catalogFakeCall
}

var _ contracts.PlanningToolCatalogPort = (*strictPlannerCatalogFake)(nil)

func (fake *strictPlannerCatalogFake) LoadPlanningToolSnapshot(
	ctx context.Context,
	selector contracts.PlanningToolCatalogSelector,
) (contracts.PlanningToolSnapshot, error) {
	if ctx == nil {
		fake.record(selector, true)
		return contracts.PlanningToolSnapshot{}, context.Canceled
	}
	contextErr := ctx.Err()
	fake.record(selector, contextErr != nil)
	if contextErr != nil {
		return contracts.PlanningToolSnapshot{}, contextErr
	}

	fake.mu.Lock()
	defer fake.mu.Unlock()
	if len(fake.responses) == 0 {
		return contracts.PlanningToolSnapshot{}, errors.New("strict Planner Catalog Fake response queue exhausted")
	}
	response := fake.responses[0]
	fake.responses = fake.responses[1:]
	return cloneFakeSnapshot(response.Snapshot), cloneFakeError(response.Err)
}

func (fake *strictPlannerCatalogFake) record(
	selector contracts.PlanningToolCatalogSelector,
	canceled bool,
) {
	fake.mu.Lock()
	defer fake.mu.Unlock()
	recorded := selector
	recorded.AllowedTools = slices.Clone(selector.AllowedTools)
	fake.calls = append(fake.calls, catalogFakeCall{selector: recorded, canceled: canceled})
}

func (fake *strictPlannerCatalogFake) recordedCalls() []catalogFakeCall {
	fake.mu.Lock()
	defer fake.mu.Unlock()
	result := make([]catalogFakeCall, len(fake.calls))
	for index, call := range fake.calls {
		result[index] = call
		result[index].selector.AllowedTools = slices.Clone(call.selector.AllowedTools)
	}
	return result
}

func cloneFakeSnapshot(input contracts.PlanningToolSnapshot) contracts.PlanningToolSnapshot {
	result := input
	if input.Tools == nil {
		return result
	}
	result.Tools = make([]contracts.PlanningToolSpec, len(input.Tools))
	for index, definition := range input.Tools {
		result.Tools[index] = definition
		result.Tools[index].InputSchema = cloneFakeSchema(definition.InputSchema)
	}
	return result
}

func cloneFakeError(input error) error {
	typed, ok := input.(*contracts.PlanningToolCatalogError)
	if !ok || typed == nil {
		return input
	}
	var toolName *string
	if typed.ToolName != nil {
		value := *typed.ToolName
		toolName = &value
	}
	return contracts.NewPlanningToolCatalogError(
		typed.Kind,
		toolName,
		typed.CauseCode,
		typed.Unwrap(),
	)
}

func cloneFakeSchema(input contracts.CanonicalJSONSchema) contracts.CanonicalJSONSchema {
	result := input
	if input.Items != nil {
		items := cloneFakeSchema(*input.Items)
		result.Items = &items
	}
	if input.Properties != nil {
		result.Properties = make(map[string]contracts.CanonicalJSONSchema, len(input.Properties))
		for name, child := range input.Properties {
			result.Properties[name] = cloneFakeSchema(child)
		}
	}
	result.Required = slices.Clone(input.Required)
	if input.AdditionalProperties != nil {
		additional := *input.AdditionalProperties
		result.AdditionalProperties = &additional
	}
	return result
}

func validateCatalogSnapshotWithPlanner(
	t testing.TB,
	selector contracts.PlanningToolCatalogSelector,
	snapshot contracts.PlanningToolSnapshot,
) {
	t.Helper()
	steps := make([]StepDraft, 0, 2)
	if len(snapshot.Tools) != 0 {
		definition := snapshot.Tools[0]
		input := make(map[string]string, len(definition.InputSchema.Required))
		for _, name := range definition.InputSchema.Required {
			input[name] = "fixture"
		}
		encoded, err := json.Marshal(input)
		if err != nil {
			t.Fatalf("encode Tool input: %v", err)
		}
		stepInput, err := newStepInput(encoded)
		if err != nil {
			t.Fatalf("construct Tool input: %v", err)
		}
		toolName := contracts.ToolName(definition.ToolName)
		steps = append(steps, StepDraft{
			Sequence: 1,
			Type:     contracts.StepTypeToolCall,
			Name:     "contract tool",
			Input:    stepInput,
			OutputSchema: contracts.OutputSchema{
				"value": {Type: contracts.OutputValueTypeString},
			},
			ToolName: &toolName,
		})
	}
	verificationInput, err := newStepInput([]byte(`{"criteria":"contract","evidence":{}}`))
	if err != nil {
		t.Fatalf("construct Verification input: %v", err)
	}
	steps = append(steps, StepDraft{
		Sequence: uint32(len(steps) + 1),
		Type:     contracts.StepTypeVerification,
		Name:     "verify",
		Input:    verificationInput,
		OutputSchema: contracts.OutputSchema{
			"verified": {Type: contracts.OutputValueTypeBoolean},
		},
	})
	issues := NewValidator().Validate(ValidatePlanRequest{
		Draft:        PlanDraft{Goal: "contract", Steps: steps},
		MaxSteps:     uint32(len(steps)),
		AllowedTools: slices.Clone(selector.AllowedTools),
		ToolSnapshot: snapshot,
	})
	if len(issues) != 0 {
		t.Fatalf("Planner rejected shared Catalog fixture: %+v", issues)
	}
}
