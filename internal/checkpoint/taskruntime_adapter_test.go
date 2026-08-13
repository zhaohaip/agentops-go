package checkpoint

import (
	"context"
	"errors"
	"testing"

	"github.com/zhaohaip/agentops-go/internal/contracts"
	domain "github.com/zhaohaip/agentops-go/internal/taskruntime"
)

func TestTaskRuntimeAdapterFixesCreateAndClaimDrafts(t *testing.T) {
	t.Parallel()
	manager := &recordingRuntimeCheckpointPort{}
	adapter, err := NewTaskRuntimeAdapter(manager)
	if err != nil {
		t.Fatal(err)
	}
	tx := checkpointTestTx{}
	request := domain.SaveRuntimeCheckpointRequest{
		TaskID: "task-1", RunID: "run-1", ExecutionVersion: 1,
		ExecutionConfigHash: checkpointTestHash,
	}
	if err := adapter.SaveInitializationCheckpoint(context.Background(), tx, request); err != nil {
		t.Fatal(err)
	}
	if _, ok := manager.saved[0].Draft.(InitializationDraft); !ok {
		t.Fatalf("Create draft = %T, want InitializationDraft", manager.saved[0].Draft)
	}
	if err := adapter.SaveGeneratePlanExecutionCheckpoint(context.Background(), tx, request); err != nil {
		t.Fatal(err)
	}
	draft, ok := manager.saved[1].Draft.(ExecutionDraft)
	if !ok || draft.NextAction != contracts.CheckpointNextActionGeneratePlan || draft.PlanID != nil ||
		draft.CurrentStepID != nil || draft.ResolvedReferences == nil || len(draft.ResolvedReferences) != 0 {
		t.Fatalf("Claim draft = %#v, want empty GENERATE_PLAN ExecutionDraft", manager.saved[1].Draft)
	}
	for index, saved := range manager.saved {
		if saved.TaskID != request.TaskID || saved.RunID != request.RunID ||
			saved.ExecutionVersion != request.ExecutionVersion || saved.ExecutionConfigHash != request.ExecutionConfigHash {
			t.Fatalf("saved[%d] attribution = %#v", index, saved)
		}
	}
}

func TestTaskRuntimeAdapterMapsClosedClaimResults(t *testing.T) {
	t.Parallel()
	manager := &recordingRuntimeCheckpointPort{}
	adapter, err := NewTaskRuntimeAdapter(manager)
	if err != nil {
		t.Fatal(err)
	}
	view := View{Entity: Entity{
		CheckpointID: "checkpoint-1", TaskID: "task-1", RunID: "run-1", ExecutionVersion: 1,
		CheckpointSequence: 2, ExecutionConfigHash: checkpointTestHash,
	}, Context: contracts.RuntimeContextV1{
		SchemaVersion: 1, TaskID: "task-1", RunID: "run-1", ExecutionVersion: 1,
		NextAction: contracts.CheckpointNextActionGeneratePlan, ResolvedReferences: contracts.CanonicalResolvedReferences{},
	}}
	manager.claimResult = ValidCheckpoint{Checkpoint: view, InferredType: InferredTypeExecution, ExecutionConfigHash: checkpointTestHash}
	result, err := adapter.LoadLatestForClaim(context.Background(), checkpointTestTx{}, "task-1", "run-1", 1, domain.ClaimCheckpointSourceContinuation)
	valid, ok := result.(domain.ClaimCheckpointValid)
	if err != nil || !ok || valid.Checkpoint.CheckpointID != "checkpoint-1" ||
		valid.Checkpoint.ExecutionConfigHash != checkpointTestHash {
		t.Fatalf("valid result = %#v, %v", result, err)
	}
	if manager.claimKind != ClaimQueryContinuation {
		t.Fatalf("claim kind = %v, want continuation", manager.claimKind)
	}

	manager.claimResult = CheckpointInvalid{ReasonCode: contracts.ReasonCodeCheckpointNotFound}
	result, err = adapter.LoadLatestForClaim(context.Background(), checkpointTestTx{}, "task-1", "run-1", 1, domain.ClaimCheckpointSourceInitial)
	invalid, ok := result.(domain.ClaimCheckpointInvalid)
	if err != nil || !ok || invalid.ReasonCode != contracts.ReasonCodeCheckpointNotFound || manager.claimKind != ClaimQueryInitial {
		t.Fatalf("invalid result = %#v, kind=%v, err=%v", result, manager.claimKind, err)
	}

	manager.claimResult = PersistenceInvariantViolation{SafeReasonCode: contracts.CauseCodePersistenceInvariantViolation}
	if _, err := adapter.LoadLatestForClaim(context.Background(), checkpointTestTx{}, "task-1", "run-1", 1, domain.ClaimCheckpointSourceInitial); !errors.Is(err, domain.ErrPersistenceInvariantViolation) {
		t.Fatalf("invariant error = %v", err)
	}
	if _, err := adapter.LoadLatestForClaim(context.Background(), checkpointTestTx{}, "task-1", "run-1", 1, domain.ClaimCheckpointSource("unknown")); !errors.Is(err, domain.ErrPersistenceInvariantViolation) {
		t.Fatalf("unknown source error = %v", err)
	}
}

type recordingRuntimeCheckpointPort struct {
	saved       []RuntimeCheckpointSaveRequest
	claimKind   ClaimQueryKind
	claimResult ValidationResult
}

func (p *recordingRuntimeCheckpointPort) SaveRuntimeCheckpoint(_ context.Context, _ contracts.RuntimeWriteTx, request RuntimeCheckpointSaveRequest) (Ref, error) {
	p.saved = append(p.saved, request)
	return Ref{CheckpointID: "checkpoint", CheckpointSequence: int64(len(p.saved))}, nil
}

func (p *recordingRuntimeCheckpointPort) LoadLatestForClaim(_ context.Context, _ contracts.RuntimeWriteTx, _ RuntimeCheckpointQuery, kind ClaimQueryKind) (ValidationResult, error) {
	p.claimKind = kind
	return p.claimResult, nil
}

func (p *recordingRuntimeCheckpointPort) LoadLatestForExecutionDispatch(context.Context, contracts.RuntimeWriteTx, RuntimeCheckpointQuery) (ValidationResult, error) {
	return p.claimResult, nil
}

func (p *recordingRuntimeCheckpointPort) LoadLatestForStartupCleanup(context.Context, contracts.RuntimeWriteTx, RuntimeCheckpointQuery) (ValidationResult, error) {
	return p.claimResult, nil
}

var _ RuntimeCheckpointPort = (*recordingRuntimeCheckpointPort)(nil)
