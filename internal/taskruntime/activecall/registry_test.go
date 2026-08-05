package activecall_test

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/zhaohaip/agentops-go/internal/contracts"
	"github.com/zhaohaip/agentops-go/internal/taskruntime/activecall"
)

func TestRegistryPreparedActiveCancelAndUnregister(t *testing.T) {
	t.Parallel()
	registry := activecall.NewRegistry()
	key := validKey()
	handle, err := registry.Prepare(context.Background(), key, activecall.Metadata{
		ActionKind: contracts.CheckpointNextActionExecuteStep, StepID: "step-1",
	})
	if err != nil {
		t.Fatalf("Prepare() error = %v", err)
	}
	if state, ok := registry.State(key); !ok || state != activecall.StatePrepared {
		t.Fatalf("state = %q, %v; want PREPARED", state, ok)
	}
	if err := handle.Activate(); err != nil {
		t.Fatalf("Activate() error = %v", err)
	}
	if state, ok := registry.State(key); !ok || state != activecall.StateActive {
		t.Fatalf("state = %q, %v; want ACTIVE", state, ok)
	}
	cancelled, err := registry.Cancel(key, activecall.CauseTaskTimedOut)
	if err != nil || !cancelled {
		t.Fatalf("Cancel() = %v, %v", cancelled, err)
	}
	<-handle.Context().Done()
	if !errors.Is(context.Cause(handle.Context()), activecall.CauseTaskTimedOut) {
		t.Fatalf("cause = %v, want TASK_TIMED_OUT", context.Cause(handle.Context()))
	}
	handle.Unregister()
	handle.Unregister()
	if _, ok := registry.State(key); ok {
		t.Fatal("unregistered handle remains in Registry")
	}
}

func TestRegistryRejectsDuplicateAndStaleHandleCannotDeleteReplacement(t *testing.T) {
	t.Parallel()
	registry := activecall.NewRegistry()
	key := validKey()
	first, err := registry.Prepare(context.Background(), key, plannerMetadata())
	if err != nil {
		t.Fatalf("Prepare(first) error = %v", err)
	}
	if duplicate, err := registry.Prepare(context.Background(), key, plannerMetadata()); !errors.Is(err, activecall.ErrDuplicate) || duplicate != nil {
		t.Fatalf("Prepare(duplicate) = %v, %v", duplicate, err)
	}
	first.Unregister()
	second, err := registry.Prepare(context.Background(), key, plannerMetadata())
	if err != nil {
		t.Fatalf("Prepare(second) error = %v", err)
	}
	first.Unregister()
	if state, ok := registry.State(key); !ok || state != activecall.StatePrepared {
		t.Fatalf("replacement state = %q, %v", state, ok)
	}
	second.Unregister()
}

func TestRegistryCancelPreparedAndCancelAll(t *testing.T) {
	t.Parallel()
	registry := activecall.NewRegistry()
	first, _ := registry.Prepare(context.Background(), validKey(), plannerMetadata())
	secondKey := activecall.Key{TaskID: "task-2", ExecutionVersion: 2, WorkerID: "worker-1"}
	second, _ := registry.Prepare(context.Background(), secondKey, plannerMetadata())
	if cancelled, err := registry.Cancel(validKey(), activecall.CauseTaskCancelled); err != nil || !cancelled {
		t.Fatalf("Cancel(PREPARED) = %v, %v", cancelled, err)
	}
	if err := registry.CancelAll(activecall.CauseRuntimeShutdown); err != nil {
		t.Fatalf("CancelAll() error = %v", err)
	}
	<-first.Context().Done()
	<-second.Context().Done()
	if !errors.Is(context.Cause(first.Context()), activecall.CauseTaskCancelled) {
		t.Fatalf("first cause = %v", context.Cause(first.Context()))
	}
	if !errors.Is(context.Cause(second.Context()), activecall.CauseRuntimeShutdown) {
		t.Fatalf("second cause = %v", context.Cause(second.Context()))
	}
	first.Unregister()
	second.Unregister()
}

func TestRegistryConcurrentCancelAndUnregisterAreSafe(t *testing.T) {
	t.Parallel()
	registry := activecall.NewRegistry()
	handle, err := registry.Prepare(context.Background(), validKey(), plannerMetadata())
	if err != nil {
		t.Fatalf("Prepare() error = %v", err)
	}
	var group sync.WaitGroup
	for range 20 {
		group.Add(2)
		go func() {
			defer group.Done()
			_, _ = registry.Cancel(validKey(), activecall.CauseLockLost)
		}()
		go func() {
			defer group.Done()
			handle.Unregister()
		}()
	}
	group.Wait()
	if _, ok := registry.State(validKey()); ok {
		t.Fatal("handle remains after concurrent unregister")
	}
}

func TestRegistryValidatesInputsAndCancellationCause(t *testing.T) {
	t.Parallel()
	registry := activecall.NewRegistry()
	if _, err := registry.Prepare(context.Background(), activecall.Key{}, plannerMetadata()); err == nil {
		t.Fatal("Prepare(invalid key) succeeded")
	}
	if _, err := registry.Prepare(context.Background(), validKey(), activecall.Metadata{
		ActionKind: contracts.CheckpointNextActionExecuteStep,
	}); err == nil {
		t.Fatal("Prepare(Step without step ID) succeeded")
	}
	if _, err := registry.Cancel(validKey(), "UNKNOWN"); !errors.Is(err, activecall.ErrInvalidCancellationCause) {
		t.Fatalf("Cancel(invalid cause) error = %v", err)
	}
}

func validKey() activecall.Key {
	return activecall.Key{TaskID: "task-1", ExecutionVersion: 1, WorkerID: "worker-1"}
}

func plannerMetadata() activecall.Metadata {
	return activecall.Metadata{ActionKind: contracts.CheckpointNextActionGeneratePlan}
}
