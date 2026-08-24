package planner

import (
	"context"
	"errors"
	"slices"
	"testing"

	"github.com/zhaohaip/agentops-go/internal/contracts"
	"github.com/zhaohaip/agentops-go/test/fixtures/catalogcontract"
)

func TestCatalogConsumerPassesSelectorAndValidatesProviderSnapshot(t *testing.T) {
	t.Parallel()

	fixture := catalogcontract.FixedFixture()
	selector := catalogcontract.SelectorFor(
		t,
		fixture,
		catalogcontract.FixedCatalogID,
		[]string{"k8s.get_pod", "k8s.get_deployment"},
	)
	expected := catalogcontract.SnapshotFor(
		t,
		fixture,
		catalogcontract.FixedCatalogID,
		[]string{"k8s.get_pod", "k8s.get_deployment"},
	)
	fake := plannerCatalogFakeFactory{}.New(t, catalogcontract.Scenario{
		Fixture:   fixture,
		Responses: []catalogcontract.Response{{Snapshot: expected}},
	}).(*strictPlannerCatalogFake)
	snapshot, err := NewCatalogConsumer(fake).Load(context.Background(), selector)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if snapshot.SnapshotHash != catalogcontract.FixedSnapshotHash {
		t.Fatalf("snapshot hash = %s", snapshot.SnapshotHash)
	}
	calls := fake.recordedCalls()
	if len(calls) != 1 || !slices.Equal(calls[0].selector.AllowedTools, selector.AllowedTools) ||
		calls[0].selector.CatalogID != selector.CatalogID {
		t.Fatalf("Catalog selector was not passed unchanged: %+v", calls)
	}
	selector.AllowedTools[0] = "caller mutation"
	if calls[0].selector.AllowedTools[0] == "caller mutation" {
		t.Fatal("strict Fake did not record a deep-copied selector")
	}
}

func TestCatalogConsumerRejectsImpossibleProviderResults(t *testing.T) {
	t.Parallel()

	fixture := catalogcontract.FixedFixture()
	selector := catalogcontract.SelectorFor(
		t,
		fixture,
		catalogcontract.FixedCatalogID,
		[]string{"k8s.get_deployment"},
	)
	validSnapshot := catalogcontract.SnapshotFor(
		t,
		fixture,
		catalogcontract.FixedCatalogID,
		[]string{"k8s.get_deployment"},
	)
	providerErr := contracts.NewPlanningToolCatalogError(
		contracts.PlanningToolCatalogErrorToolNotFound,
		nil,
		contracts.CauseCodeRuntimeStaticToolSnapshotInconsistent,
		nil,
	)
	tests := []struct {
		name string
		port contracts.PlanningToolCatalogPort
	}{
		{name: "success and error", port: catalogPortFunc(func(context.Context, contracts.PlanningToolCatalogSelector) (contracts.PlanningToolSnapshot, error) {
			return validSnapshot, providerErr
		})},
		{name: "unknown error", port: catalogPortFunc(func(context.Context, contracts.PlanningToolCatalogSelector) (contracts.PlanningToolSnapshot, error) {
			return contracts.PlanningToolSnapshot{}, errors.New("unknown provider failure")
		})},
		{name: "unknown typed kind", port: catalogPortFunc(func(context.Context, contracts.PlanningToolCatalogSelector) (contracts.PlanningToolSnapshot, error) {
			return contracts.PlanningToolSnapshot{}, contracts.NewPlanningToolCatalogError(
				"UNKNOWN",
				nil,
				contracts.CauseCodeRuntimeStaticToolSnapshotInconsistent,
				nil,
			)
		})},
		{name: "invalid typed cause", port: catalogPortFunc(func(context.Context, contracts.PlanningToolCatalogSelector) (contracts.PlanningToolSnapshot, error) {
			return contracts.PlanningToolSnapshot{}, contracts.NewPlanningToolCatalogError(
				contracts.PlanningToolCatalogErrorRuntimeFatal,
				nil,
				"UNKNOWN",
				nil,
			)
		})},
		{name: "illegal dto", port: catalogPortFunc(func(context.Context, contracts.PlanningToolCatalogSelector) (contracts.PlanningToolSnapshot, error) {
			changed := cloneFakeSnapshot(validSnapshot)
			changed.Tools[0].Description = "tampered without updating hash"
			return changed, nil
		})},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			snapshot, err := NewCatalogConsumer(test.port).Load(context.Background(), selector)
			assertConsumerCatalogError(t, err, contracts.PlanningToolCatalogErrorRuntimeFatal)
			if !planningToolSnapshotIsZero(snapshot) {
				t.Fatalf("impossible result leaked Snapshot: %+v", snapshot)
			}
		})
	}
}

func TestCatalogConsumerPreservesContextErrors(t *testing.T) {
	t.Parallel()

	fixture := catalogcontract.FixedFixture()
	selector := catalogcontract.SelectorFor(t, fixture, catalogcontract.FixedCatalogID, []string{})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := NewCatalogConsumer(plannerCatalogFakeFactory{}.New(t, catalogcontract.Scenario{
		Fixture: fixture,
	})).Load(ctx, selector)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Load() error = %v, want context.Canceled", err)
	}

	wrapped := contracts.NewPlanningToolCatalogError(
		contracts.PlanningToolCatalogErrorRuntimeFatal,
		nil,
		contracts.CauseCodeRuntimeStaticToolSnapshotInconsistent,
		context.DeadlineExceeded,
	)
	_, err = NewCatalogConsumer(catalogPortFunc(func(context.Context, contracts.PlanningToolCatalogSelector) (contracts.PlanningToolSnapshot, error) {
		return contracts.PlanningToolSnapshot{}, wrapped
	})).Load(context.Background(), selector)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("wrapped deadline semantics lost: %v", err)
	}
}

type catalogPortFunc func(
	context.Context,
	contracts.PlanningToolCatalogSelector,
) (contracts.PlanningToolSnapshot, error)

func (function catalogPortFunc) LoadPlanningToolSnapshot(
	ctx context.Context,
	selector contracts.PlanningToolCatalogSelector,
) (contracts.PlanningToolSnapshot, error) {
	return function(ctx, selector)
}

func assertConsumerCatalogError(
	t *testing.T,
	err error,
	want contracts.PlanningToolCatalogErrorKind,
) {
	t.Helper()
	var typed *contracts.PlanningToolCatalogError
	if !errors.As(err, &typed) || typed == nil {
		t.Fatalf("error %v is not PlanningToolCatalogError", err)
	}
	if typed.Kind != want || typed.CauseCode != contracts.CauseCodeRuntimeStaticToolSnapshotInconsistent {
		t.Fatalf("Catalog error = (%s, %s), want (%s, %s)",
			typed.Kind, typed.CauseCode, want, contracts.CauseCodeRuntimeStaticToolSnapshotInconsistent)
	}
}
