package stepexecutor

import (
	"encoding/json"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/zhaohaip/agentops-go/internal/contracts"
)

func TestEntityOwnsStepFactsAndMutableValues(t *testing.T) {
	t.Parallel()
	input := json.RawMessage(`{"prompt":"inspect"}`)
	schema := contracts.OutputSchema{"result": {Type: contracts.OutputValueTypeString}}
	entity, err := NewEntity(EntityParams{
		StepID: "step-1", RunID: "run-1", PlanID: "plan-1", Sequence: 1,
		Type: contracts.StepTypeAnalysis, Name: "inspect", Input: input, OutputSchema: schema,
		Status: contracts.StepStatusPending,
	})
	if err != nil {
		t.Fatalf("NewEntity() error = %v", err)
	}

	input[2] = 'X'
	schema["result"] = contracts.OutputFieldSchema{Type: "invalid"}
	readInput := entity.Input()
	readInput[2] = 'Y'
	readSchema := entity.OutputSchema()
	readSchema["result"] = contracts.OutputFieldSchema{Type: "invalid"}
	if got := string(entity.Input()); got != `{"prompt":"inspect"}` {
		t.Fatalf("Entity input mutated: %s", got)
	}
	if got := entity.OutputSchema()["result"].Type; got != contracts.OutputValueTypeString {
		t.Fatalf("Entity output schema mutated: %s", got)
	}
	if entity.StepID() != "step-1" || entity.RunID() != "run-1" || entity.PlanID() != "plan-1" ||
		entity.Sequence() != 1 || entity.Type() != contracts.StepTypeAnalysis || entity.Name() != "inspect" ||
		entity.Status() != contracts.StepStatusPending || entity.ToolName() != "" || entity.Output() != nil ||
		entity.ErrorCode() != nil || entity.StartedAt() != nil || entity.EndedAt() != nil {
		t.Fatalf("Entity accessors returned unexpected facts: %#v", entity)
	}

	typeOfEntity := reflect.TypeOf(entity)
	for index := 0; index < typeOfEntity.NumField(); index++ {
		if field := typeOfEntity.Field(index); field.IsExported() {
			t.Fatalf("Entity field %q is exported", field.Name)
		}
	}
}

func TestNewEntityAcceptsFrozenStepStates(t *testing.T) {
	t.Parallel()
	startedAt := time.Date(2026, 8, 27, 1, 2, 3, 0, time.FixedZone("test", 8*60*60))
	endedAt := startedAt.Add(time.Second)
	errorCode := contracts.ErrorCodeModelCallFailed
	tests := []EntityParams{
		{Status: contracts.StepStatusPending},
		{Status: contracts.StepStatusRunning, StartedAt: &startedAt},
		{Status: contracts.StepStatusWaitingApproval, StartedAt: &startedAt},
		{Status: contracts.StepStatusCompleted, Output: json.RawMessage(`{"result":"ok"}`), StartedAt: &startedAt, EndedAt: &endedAt},
		{Status: contracts.StepStatusFailed, ErrorCode: &errorCode, StartedAt: &startedAt, EndedAt: &endedAt},
	}
	for _, params := range tests {
		params.StepID, params.RunID, params.PlanID = "step-1", "run-1", "plan-1"
		params.Sequence, params.Type, params.Name = 1, contracts.StepTypeAnalysis, "inspect"
		params.Input = json.RawMessage(`{}`)
		params.OutputSchema = contracts.OutputSchema{"result": {Type: contracts.OutputValueTypeString}}
		entity, err := NewEntity(params)
		if err != nil {
			t.Fatalf("NewEntity(%s) error = %v", params.Status, err)
		}
		if entity.StartedAt() != nil && entity.StartedAt().Location() != time.UTC {
			t.Fatalf("StartedAt location = %v, want UTC", entity.StartedAt().Location())
		}
	}
}

func TestNewEntityAcceptsPendingStepFailedBeforeStart(t *testing.T) {
	t.Parallel()
	endedAt := time.Now().UTC()
	errorCode := contracts.ErrorCodeTaskCancelled
	entity, err := NewEntity(EntityParams{
		StepID: "step-1", RunID: "run-1", PlanID: "plan-1", Sequence: 1,
		Type: contracts.StepTypeAnalysis, Name: "inspect", Input: json.RawMessage(`{}`),
		OutputSchema: contracts.OutputSchema{"result": {Type: contracts.OutputValueTypeString}},
		Status:       contracts.StepStatusFailed, ErrorCode: &errorCode, EndedAt: &endedAt,
	})
	if err != nil {
		t.Fatalf("NewEntity() error = %v", err)
	}
	if entity.StartedAt() != nil || entity.EndedAt() == nil || !entity.EndedAt().Equal(endedAt) {
		t.Fatalf("Pending to Failed timestamps = (%v, %v)", entity.StartedAt(), entity.EndedAt())
	}
}

func TestNewEntityRejectsInvalidFacts(t *testing.T) {
	t.Parallel()
	startedAt := time.Now().UTC()
	endedBeforeStart := startedAt.Add(-time.Second)
	invalidError := contracts.ErrorCode("unknown")
	valid := EntityParams{
		StepID: "step-1", RunID: "run-1", PlanID: "plan-1", Sequence: 1,
		Type: contracts.StepTypeToolCall, Name: "inspect", Input: json.RawMessage(`{}`),
		OutputSchema: contracts.OutputSchema{"result": {Type: contracts.OutputValueTypeString}},
		Status:       contracts.StepStatusPending, ToolName: "k8s.get_deployment",
	}
	tests := []struct {
		name   string
		mutate func(*EntityParams)
	}{
		{name: "Step ID", mutate: func(value *EntityParams) { value.StepID = "" }},
		{name: "Run ID", mutate: func(value *EntityParams) { value.RunID = "" }},
		{name: "Plan ID", mutate: func(value *EntityParams) { value.PlanID = "" }},
		{name: "sequence", mutate: func(value *EntityParams) { value.Sequence = 0 }},
		{name: "type", mutate: func(value *EntityParams) { value.Type = "unknown" }},
		{name: "name", mutate: func(value *EntityParams) { value.Name = "" }},
		{name: "input", mutate: func(value *EntityParams) { value.Input = json.RawMessage(`[]`) }},
		{name: "output schema empty", mutate: func(value *EntityParams) { value.OutputSchema = contracts.OutputSchema{} }},
		{name: "output schema field", mutate: func(value *EntityParams) {
			value.OutputSchema = contracts.OutputSchema{"bad-name": {Type: contracts.OutputValueTypeString}}
		}},
		{name: "ToolCall tool", mutate: func(value *EntityParams) { value.ToolName = "" }},
		{name: "non Tool tool", mutate: func(value *EntityParams) { value.Type = contracts.StepTypeAnalysis }},
		{name: "Pending started", mutate: func(value *EntityParams) { value.StartedAt = &startedAt }},
		{name: "Completed without output", mutate: func(value *EntityParams) {
			value.Status, value.StartedAt, value.EndedAt = contracts.StepStatusCompleted, &startedAt, &startedAt
		}},
		{name: "Failed without end", mutate: func(value *EntityParams) {
			code := contracts.ErrorCodeTaskCancelled
			value.Status, value.ErrorCode = contracts.StepStatusFailed, &code
		}},
		{name: "invalid error", mutate: func(value *EntityParams) {
			value.Status, value.StartedAt, value.EndedAt, value.ErrorCode = contracts.StepStatusFailed, &startedAt, &startedAt, &invalidError
		}},
		{name: "time order", mutate: func(value *EntityParams) {
			code := contracts.ErrorCodeModelCallFailed
			value.Status, value.StartedAt, value.EndedAt, value.ErrorCode = contracts.StepStatusFailed, &startedAt, &endedBeforeStart, &code
		}},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			params := valid
			test.mutate(&params)
			if _, err := NewEntity(params); !errors.Is(err, ErrInvalidStep) {
				t.Fatalf("NewEntity() error = %v", err)
			}
		})
	}
}
