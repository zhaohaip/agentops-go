package contracts

import (
	"reflect"
	"testing"
)

func TestNonToolInputContractIsFrozenAndCopied(t *testing.T) {
	t.Parallel()
	tests := []struct {
		stepType StepType
		want     []NonToolInputFieldContract
	}{
		{stepType: StepTypeModelCall, want: []NonToolInputFieldContract{
			{Name: "prompt", Type: JSONSchemaTypeString, Required: true, ReferenceAllowed: true},
			{Name: "context", Type: JSONSchemaTypeObject, ReferenceAllowed: true},
		}},
		{stepType: StepTypeAnalysis, want: []NonToolInputFieldContract{
			{Name: "instruction", Type: JSONSchemaTypeString, Required: true, ReferenceAllowed: true},
			{Name: "evidence", Type: JSONSchemaTypeObject, Required: true, ReferenceAllowed: true},
		}},
		{stepType: StepTypeVerification, want: []NonToolInputFieldContract{
			{Name: "criteria", Type: JSONSchemaTypeString, Required: true},
			{Name: "evidence", Type: JSONSchemaTypeObject, Required: true, ReferenceAllowed: true},
		}},
	}
	for _, test := range tests {
		test := test
		t.Run(string(test.stepType), func(t *testing.T) {
			t.Parallel()
			got, ok := NonToolInputContract(test.stepType)
			if !ok || !reflect.DeepEqual(got, test.want) {
				t.Fatalf("NonToolInputContract() = (%#v, %v), want %#v", got, ok, test.want)
			}
			got[0].Name = "changed"
			again, _ := NonToolInputContract(test.stepType)
			if again[0].Name != test.want[0].Name {
				t.Fatal("NonToolInputContract() returned mutable shared state")
			}
		})
	}
	for _, stepType := range []StepType{StepTypeToolCall, StepType("unknown")} {
		if fields, ok := NonToolInputContract(stepType); ok || fields != nil {
			t.Fatalf("NonToolInputContract(%q) = (%#v, %v)", stepType, fields, ok)
		}
	}
}
