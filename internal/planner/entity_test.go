package planner

import (
	"reflect"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/zhaohaip/agentops-go/internal/contracts"
)

func TestEntityIsImmutableSafePlanProjection(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 8, 20, 1, 2, 3, 0, time.FixedZone("test", 8*60*60))
	entity, err := NewEntity("plan-1", "run-1", "合法 Unicode 目标", now)
	if err != nil {
		t.Fatalf("NewEntity() error = %v", err)
	}
	if entity.PlanID() != "plan-1" || entity.RunID() != "run-1" || entity.Goal() != "合法 Unicode 目标" || !entity.CreatedAt().Equal(now.UTC()) {
		t.Fatalf("Entity accessors returned unexpected values: %#v", entity)
	}

	typeOfEntity := reflect.TypeOf(entity)
	for index := 0; index < typeOfEntity.NumField(); index++ {
		field := typeOfEntity.Field(index)
		if field.IsExported() {
			t.Fatalf("Entity field %q is exported; immutable facts must use accessors", field.Name)
		}
		if field.Name == "rawResponse" || field.Name == "prompt" || field.Name == "providerRequestID" {
			t.Fatalf("Entity contains forbidden transient field %q", field.Name)
		}
	}
}

func TestNewEntityRejectsInvalidFacts(t *testing.T) {
	t.Parallel()
	invalidUTF8 := string([]byte{0xff})
	if utf8.ValidString(invalidUTF8) {
		t.Fatal("fixture unexpectedly contains valid UTF-8")
	}
	now := time.Now()
	for _, test := range []struct {
		name   string
		planID contracts.PlanID
		runID  contracts.RunID
		goal   string
		at     time.Time
	}{
		{name: "Plan ID", runID: "run-1", goal: "goal", at: now},
		{name: "Run ID", planID: "plan-1", goal: "goal", at: now},
		{name: "goal", planID: "plan-1", runID: "run-1", at: now},
		{name: "goal UTF-8", planID: "plan-1", runID: "run-1", goal: invalidUTF8, at: now},
		{name: "created at", planID: "plan-1", runID: "run-1", goal: "goal"},
	} {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			if _, err := NewEntity(test.planID, test.runID, test.goal, test.at); err == nil {
				t.Fatal("NewEntity() succeeded")
			}
		})
	}
}
