package checkpoint

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/zhaohaip/agentops-go/internal/contracts"
)

func TestRuntimeCheckpointPortFixesPurposeAndUsage(t *testing.T) {
	t.Parallel()
	manager, repository := newManagerHarness(t)
	token := checkpointTestTx{}

	ref, err := manager.SaveRuntimeCheckpoint(context.Background(), token, RuntimeCheckpointSaveRequest{
		TaskID: "task-1", RunID: "run-1", ExecutionVersion: 1,
		ExecutionConfigHash: checkpointTestHash, Draft: InitializationDraft{},
	})
	if err != nil || ref.CheckpointSequence != 1 || len(repository.inserted) != 1 {
		t.Fatalf("SaveRuntimeCheckpoint() = %#v, %v; inserts=%d", ref, err, len(repository.inserted))
	}
	if repository.inserted[0].SourceCheckpointID != nil || repository.inserted[0].SourceExecutionVersion != nil {
		t.Fatalf("public save created Recovery source: %#v", repository.inserted[0])
	}
	if repository.inserted[0].CheckpointID == "" {
		t.Fatal("Manager did not generate Checkpoint ID")
	}

	if _, err := manager.LoadLatestForClaim(context.Background(), token, checkpointTestQuery(), ClaimQueryKind(99)); !errors.Is(err, ErrInvalidDraft) {
		t.Fatalf("unknown Claim usage error = %v", err)
	}
	if _, err := manager.SaveRuntimeCheckpoint(context.Background(), token, RuntimeCheckpointSaveRequest{
		TaskID: "task-1", RunID: "run-1", ExecutionVersion: 2,
		ExecutionConfigHash: checkpointTestHash, Draft: InitializationDraft{},
	}); !errors.Is(err, ErrInvalidDraft) {
		t.Fatalf("version 2 Initialization error = %v", err)
	}
}

func TestRuntimeCheckpointValidatesPlanStepActionAndReferences(t *testing.T) {
	t.Parallel()
	key := "payload"
	canonical := contracts.CanonicalResolvedReferences{{
		TargetPath:   []contracts.ReferencePathSegment{{Kind: contracts.ReferencePathSegmentKey, Key: &key}},
		SourceStepID: "step-1", SourceOutputField: "result",
	}}
	validFacts := executionValidationFacts()
	validFacts.Plan = &PlanFact{PlanID: "plan-1", RunID: "run-1"}
	validFacts.Run.PlanID = pointer(contracts.PlanID("plan-1"))
	validFacts.Run.CurrentStepID = pointer(contracts.StepID("step-2"))
	validFacts.Step = &StepFact{
		StepID: "step-2", RunID: "run-1", PlanID: "plan-1", Sequence: 2,
		Type: contracts.StepTypeModelCall, Status: contracts.StepStatusRunning,
		Input: json.RawMessage(`{"payload":"step.output.result"}`),
	}
	validFacts.Previous = &StepFact{
		StepID: "step-1", RunID: "run-1", PlanID: "plan-1", Sequence: 1,
		Type: contracts.StepTypeAnalysis, Status: contracts.StepStatusCompleted,
		OutputSchema: contracts.OutputSchema{"result": {}}, SafeOutput: json.RawMessage(`{"result":"safe"}`),
	}

	tests := []struct {
		name   string
		mutate func(*ValidationFacts, *ExecutionDraft)
		reason contracts.ReasonCode
	}{
		{name: "valid", reason: ""},
		{name: "Plan missing", mutate: func(f *ValidationFacts, _ *ExecutionDraft) { f.Plan = nil }, reason: contracts.ReasonCodeCheckpointPlanReferenceInvalid},
		{name: "Plan cross Run", mutate: func(f *ValidationFacts, _ *ExecutionDraft) { f.Plan.RunID = "run-other" }, reason: contracts.ReasonCodeCheckpointPlanReferenceInvalid},
		{name: "Step missing", mutate: func(f *ValidationFacts, _ *ExecutionDraft) { f.Step = nil }, reason: contracts.ReasonCodeCheckpointStepReferenceInvalid},
		{name: "Step cross Run", mutate: func(f *ValidationFacts, _ *ExecutionDraft) { f.Step.RunID = "run-other" }, reason: contracts.ReasonCodeCheckpointStepReferenceInvalid},
		{name: "reference missing", mutate: func(_ *ValidationFacts, d *ExecutionDraft) {
			d.ResolvedReferences = contracts.CanonicalResolvedReferences{}
		}, reason: contracts.ReasonCodeCheckpointReferenceMissing},
		{name: "reference extra", mutate: func(_ *ValidationFacts, d *ExecutionDraft) {
			d.ResolvedReferences = append(d.ResolvedReferences, d.ResolvedReferences[0])
		}, reason: contracts.ReasonCodeCheckpointReferenceExtra},
		{name: "source not completed", mutate: func(f *ValidationFacts, _ *ExecutionDraft) { f.Previous.Status = contracts.StepStatusRunning }, reason: contracts.ReasonCodeCheckpointReferenceSourceInvalid},
		{name: "REQUEST_APPROVAL on model Step", mutate: func(_ *ValidationFacts, d *ExecutionDraft) {
			d.NextAction = contracts.CheckpointNextActionRequestApproval
		}, reason: contracts.ReasonCodeCheckpointNextActionInvalid},
		{name: "EXECUTE_STEP on completed Step", mutate: func(f *ValidationFacts, _ *ExecutionDraft) {
			f.Step.Status = contracts.StepStatusCompleted
		}, reason: contracts.ReasonCodeCheckpointNextActionInvalid},
	}

	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			facts := cloneValidationFacts(validFacts)
			draft := ExecutionDraft{
				PlanID: pointer(contracts.PlanID("plan-1")), CurrentStepID: pointer(contracts.StepID("step-2")),
				NextAction: contracts.CheckpointNextActionExecuteStep, ResolvedReferences: cloneReferences(canonical),
			}
			if test.mutate != nil {
				test.mutate(&facts, &draft)
			}
			manager, repository := newManagerHarnessWithFacts(t, facts)
			_, err := manager.SaveRuntimeCheckpoint(context.Background(), checkpointTestTx{}, RuntimeCheckpointSaveRequest{
				TaskID: "task-1", RunID: "run-1", ExecutionVersion: 1,
				ExecutionConfigHash: checkpointTestHash, Draft: draft,
			})
			if test.reason == "" {
				if err != nil || len(repository.inserted) != 1 {
					t.Fatalf("valid save = %v; inserts=%d", err, len(repository.inserted))
				}
				return
			}
			if err == nil || len(repository.inserted) != 0 {
				t.Fatalf("invalid save error=%v; inserts=%d", err, len(repository.inserted))
			}
			if !containsReason(err, test.reason) {
				t.Fatalf("error = %v, want reason %s", err, test.reason)
			}
		})
	}
}

func TestRuntimeCheckpointLoadUsesFixedUsageMatrixAndMaximumOnly(t *testing.T) {
	t.Parallel()
	manager, repository := newManagerHarness(t)
	_, err := manager.SaveRuntimeCheckpoint(context.Background(), checkpointTestTx{}, RuntimeCheckpointSaveRequest{
		TaskID: "task-1", RunID: "run-1", ExecutionVersion: 1,
		ExecutionConfigHash: checkpointTestHash, Draft: InitializationDraft{},
	})
	if err != nil {
		t.Fatal(err)
	}
	repository.latest = repository.inserted[0]

	result, err := manager.LoadLatestForClaim(context.Background(), checkpointTestTx{}, checkpointTestQuery(), ClaimQueryInitial)
	if err != nil {
		t.Fatal(err)
	}
	valid, ok := result.(ValidCheckpoint)
	if !ok {
		t.Fatalf("initial Claim result = %#v", result)
	}
	if valid.InferredType != InferredTypeInitialization || valid.ExecutionConfigHash != checkpointTestHash {
		t.Fatalf("ValidCheckpoint metadata = %#v", valid)
	}

	result, err = manager.LoadLatestForExecutionDispatch(context.Background(), checkpointTestTx{}, checkpointTestQuery())
	if err != nil {
		t.Fatal(err)
	}
	invalid, ok := result.(CheckpointInvalid)
	if !ok || invalid.ReasonCode != contracts.ReasonCodeCheckpointNextActionInvalid {
		t.Fatalf("dispatch before Claim = %#v", result)
	}

	repository.latest.RuntimeContext = json.RawMessage(`{}`)
	result, err = manager.LoadLatestForClaim(context.Background(), checkpointTestTx{}, checkpointTestQuery(), ClaimQueryInitial)
	if err != nil {
		t.Fatal(err)
	}
	invalid, ok = result.(CheckpointInvalid)
	if !ok || invalid.ReasonCode != contracts.ReasonCodeRuntimeContextMalformed || repository.findLatestCalls != 3 {
		t.Fatalf("damaged maximum result=%#v calls=%d", result, repository.findLatestCalls)
	}
}

func TestCheckpointValidationResultThreeBranchesAndErrorIsolation(t *testing.T) {
	t.Parallel()
	manager, repository := newManagerHarness(t)
	if _, err := manager.SaveRuntimeCheckpoint(context.Background(), checkpointTestTx{}, RuntimeCheckpointSaveRequest{
		TaskID: "task-1", RunID: "run-1", ExecutionVersion: 1,
		ExecutionConfigHash: checkpointTestHash, Draft: InitializationDraft{},
	}); err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name      string
		mutate    func(*fakeCheckpointRepository)
		assertion func(*testing.T, ValidationResult, error)
	}{
		{name: "Valid", assertion: func(t *testing.T, result ValidationResult, err error) {
			if err != nil {
				t.Fatal(err)
			}
			valid, ok := result.(ValidCheckpoint)
			if !ok || valid.InferredType != InferredTypeInitialization || valid.ExecutionConfigHash != checkpointTestHash {
				t.Fatalf("result = %#v", result)
			}
		}},
		{name: "unsupported schema", mutate: func(r *fakeCheckpointRepository) {
			r.latest.RuntimeContext = json.RawMessage(`{"schema_version":2,"task_id":"task-1","run_id":"run-1","execution_version":1,"next_action":"GENERATE_PLAN","resolved_references":[]}`)
		}, assertion: assertCheckpointInvalidReason(contracts.ReasonCodeRuntimeContextVersionUnsupported)},
		{name: "malformed", mutate: func(r *fakeCheckpointRepository) {
			r.latest.RuntimeContext = json.RawMessage(`{}`)
		}, assertion: assertCheckpointInvalidReason(contracts.ReasonCodeRuntimeContextMalformed)},
		{name: "attribution invariant", mutate: func(r *fakeCheckpointRepository) {
			r.facts.Run.TaskID = "task-other"
		}, assertion: func(t *testing.T, result ValidationResult, err error) {
			if err != nil {
				t.Fatal(err)
			}
			violation, ok := result.(PersistenceInvariantViolation)
			if !ok || violation.SafeReasonCode != contracts.CauseCodePersistenceInvariantViolation {
				t.Fatalf("result = %#v", result)
			}
		}},
		{name: "repository invariant", mutate: func(r *fakeCheckpointRepository) {
			r.verifyErr = ErrPersistenceInvariantViolation
		}, assertion: func(t *testing.T, result ValidationResult, err error) {
			if err != nil {
				t.Fatal(err)
			}
			if _, ok := result.(PersistenceInvariantViolation); !ok {
				t.Fatalf("result = %#v", result)
			}
		}},
		{name: "infrastructure error", mutate: func(r *fakeCheckpointRepository) {
			r.latestErr = errors.New("database connection unavailable")
		}, assertion: func(t *testing.T, result ValidationResult, err error) {
			if result != nil || err == nil {
				t.Fatalf("result=%#v error=%v", result, err)
			}
		}},
	}

	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			copyRepository := *repository
			copyRepository.inserted = append([]Entity(nil), repository.inserted...)
			if test.mutate != nil {
				test.mutate(&copyRepository)
			}
			copyManager := *manager
			copyManager.repository = &copyRepository
			result, err := copyManager.LoadLatestForClaim(context.Background(), checkpointTestTx{}, checkpointTestQuery(), ClaimQueryInitial)
			test.assertion(t, result, err)
		})
	}
}

func TestRuntimeContextCodecErrorKinds(t *testing.T) {
	t.Parallel()
	codec, err := NewRuntimeContextCodec(RuntimeContextCodecLimits{MaxBytes: 16 * 1024, MaxDepth: 16})
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name     string
		document []byte
		kind     RuntimeContextCodecErrorKind
	}{
		{name: "unsupported", document: []byte(`{"schema_version":9,"task_id":"task-1","run_id":"run-1","execution_version":1,"next_action":"GENERATE_PLAN","resolved_references":[]}`), kind: RuntimeContextCodecVersionUnsupported},
		{name: "malformed", document: []byte(`{"schema_version":1}`), kind: RuntimeContextCodecMalformed},
	} {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			_, err := codec.Decode(test.document)
			var codecError *RuntimeContextCodecError
			if !errors.As(err, &codecError) || codecError.Kind != test.kind {
				t.Fatalf("Decode() error = %#v, want kind %d", err, test.kind)
			}
		})
	}
}

const checkpointTestHash contracts.ExecutionConfigHash = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

type checkpointTestTx struct{}

func (checkpointTestTx) AgentOpsRuntimeWriteTx() {}

type fakeCheckpointRepository struct {
	facts           ValidationFacts
	inserted        []Entity
	latest          Entity
	findLatestCalls int
	verifyErr       error
	latestErr       error
}

func (r *fakeCheckpointRepository) AllocateNextSequence(context.Context, contracts.RuntimeWriteTx, contracts.RunID) (int64, error) {
	return int64(len(r.inserted) + 1), nil
}
func (r *fakeCheckpointRepository) InsertCheckpoint(_ context.Context, _ contracts.RuntimeWriteTx, entity Entity) (time.Time, error) {
	entity.CreatedAt = time.Now().UTC()
	r.inserted = append(r.inserted, entity)
	r.latest = entity
	return entity.CreatedAt, nil
}
func (r *fakeCheckpointRepository) FindLatestByExecutionVersion(context.Context, contracts.RuntimeWriteTx, contracts.TaskID, contracts.RunID, contracts.ExecutionVersion) (Entity, error) {
	r.findLatestCalls++
	if r.latestErr != nil {
		return Entity{}, r.latestErr
	}
	if r.latest.CheckpointID == "" {
		return Entity{}, ErrRepositoryNotFound
	}
	return r.latest, nil
}
func (*fakeCheckpointRepository) FindByID(context.Context, contracts.RuntimeWriteTx, contracts.CheckpointID) (Entity, error) {
	return Entity{}, ErrRepositoryNotFound
}
func (r *fakeCheckpointRepository) LoadTaskExecution(context.Context, contracts.RuntimeWriteTx, contracts.TaskID, contracts.ExecutionVersion) (TaskExecutionHash, error) {
	return TaskExecutionHash{TaskID: r.facts.Execution.TaskID, ExecutionVersion: r.facts.Execution.ExecutionVersion, ExecutionConfigHash: r.facts.Execution.ExecutionConfigHash}, nil
}
func (r *fakeCheckpointRepository) VerifyRunAttribution(context.Context, contracts.RuntimeWriteTx, contracts.TaskID, contracts.RunID) error {
	return r.verifyErr
}
func (r *fakeCheckpointRepository) LoadValidationFacts(context.Context, contracts.RuntimeWriteTx, ValidationFactsRequest) (ValidationFacts, error) {
	return r.facts, nil
}

func newManagerHarness(t *testing.T) (*Manager, *fakeCheckpointRepository) {
	return newManagerHarnessWithFacts(t, initializationValidationFacts())
}
func newManagerHarnessWithFacts(t *testing.T, facts ValidationFacts) (*Manager, *fakeCheckpointRepository) {
	t.Helper()
	codec, err := NewRuntimeContextCodec(RuntimeContextCodecLimits{MaxBytes: 64 * 1024, MaxDepth: 32})
	if err != nil {
		t.Fatal(err)
	}
	repository := &fakeCheckpointRepository{facts: facts}
	manager, err := NewManager(repository, codec)
	if err != nil {
		t.Fatal(err)
	}
	return manager, repository
}
func initializationValidationFacts() ValidationFacts {
	queued := time.Now().UTC()
	return ValidationFacts{Task: TaskFact{TaskID: "task-1", Status: contracts.TaskStatusPending, CurrentRunID: "run-1", CurrentExecutionVersion: 1, QueuedAt: &queued}, Run: RunFact{RunID: "run-1", TaskID: "task-1", Status: contracts.RunStatusPending}, Execution: ExecutionFact{TaskID: "task-1", ExecutionVersion: 1, Status: contracts.TaskExecutionStatusQueued, ExecutionConfigHash: checkpointTestHash}}
}
func executionValidationFacts() ValidationFacts {
	facts := initializationValidationFacts()
	facts.Task.Status = contracts.TaskStatusRunning
	facts.Task.QueuedAt = nil
	facts.Run.Status = contracts.RunStatusRunning
	worker := contracts.WorkerID("worker-1")
	facts.Execution.Status = contracts.TaskExecutionStatusRunning
	facts.Execution.WorkerID = &worker
	return facts
}
func checkpointTestQuery() RuntimeCheckpointQuery {
	return RuntimeCheckpointQuery{TaskID: "task-1", RunID: "run-1", ExecutionVersion: 1}
}
func pointer[T any](value T) *T { return &value }
func containsReason(err error, reason contracts.ReasonCode) bool {
	return err != nil && strings.Contains(err.Error(), string(reason))
}
func cloneValidationFacts(value ValidationFacts) ValidationFacts {
	encoded, _ := json.Marshal(value)
	var cloned ValidationFacts
	_ = json.Unmarshal(encoded, &cloned)
	return cloned
}

func assertCheckpointInvalidReason(reason contracts.ReasonCode) func(*testing.T, ValidationResult, error) {
	return func(t *testing.T, result ValidationResult, err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
		invalid, ok := result.(CheckpointInvalid)
		if !ok || invalid.ReasonCode != reason {
			t.Fatalf("result = %#v, want %s", result, reason)
		}
	}
}
