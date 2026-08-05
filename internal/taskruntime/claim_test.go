package taskruntime_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/zhaohaip/agentops-go/internal/contracts"
	"github.com/zhaohaip/agentops-go/internal/lifecycle"
	"github.com/zhaohaip/agentops-go/internal/taskruntime"
)

func TestClaimNextExecutionUsesStrictFIFOAndCommitsClaim(t *testing.T) {
	t.Parallel()
	fixture := newClaimFixture(t)
	queuedAt := fixture.now.Add(-time.Minute)
	createdAt := fixture.now.Add(-2 * time.Minute)
	fixture.seedQueuedTask("task-b", queuedAt, createdAt, fixture.hash)
	fixture.seedQueuedTask("task-a", queuedAt, createdAt, fixture.hash)

	result, err := fixture.service.ClaimNextExecution(context.Background(), fixture.workerID)
	if err != nil {
		t.Fatalf("ClaimNextExecution() error = %v", err)
	}
	claimed, ok := result.(contracts.ClaimResultClaimed)
	if !ok {
		t.Fatalf("ClaimNextExecution() result = %T, want Claimed", result)
	}
	if claimed.Claim.TaskID != "task-a" || claimed.Claim.WorkerID != fixture.workerID ||
		!claimed.Claim.ClaimedAt.Equal(fixture.now) {
		t.Fatalf("claim = %+v, want FIFO task-a and database time", claimed.Claim)
	}
	store := fixture.executor.snapshot()
	task := store.tasks["task-a"]
	run := store.runs["task-a"]
	execution := store.executions[executionKey("task-a", 1)]
	if task.Status != contracts.TaskStatusRunning || task.QueuedAt != nil || task.StartedAt == nil ||
		run.Status != contracts.RunStatusRunning || run.StartedAt == nil ||
		execution.Status != contracts.TaskExecutionStatusRunning || execution.WorkerID == nil ||
		*execution.WorkerID != fixture.workerID || execution.StartedAt == nil {
		t.Fatalf("claimed facts = task=%+v run=%+v execution=%+v", task, run, execution)
	}
	if other := store.tasks["task-b"]; other.Status != contracts.TaskStatusPending || other.QueuedAt == nil {
		t.Fatalf("non-selected FIFO task changed: %+v", other)
	}
	if len(store.checkpoints) != 3 {
		t.Fatalf("checkpoints = %d, want two Initialization plus one GENERATE_PLAN execution checkpoint", len(store.checkpoints))
	}
	assertSameFakeTransaction(t, fixture.checkpoints.seenTx)
}

func TestClaimNextExecutionNoWorkAndWorkerValidation(t *testing.T) {
	t.Parallel()
	fixture := newClaimFixture(t)
	result, err := fixture.service.ClaimNextExecution(context.Background(), fixture.workerID)
	if err != nil {
		t.Fatalf("ClaimNextExecution(empty) error = %v", err)
	}
	if _, ok := result.(contracts.ClaimResultNoWork); !ok {
		t.Fatalf("ClaimNextExecution(empty) = %T, want NoWork", result)
	}
	commits := fixture.executor.commits
	if _, err := fixture.service.ClaimNextExecution(context.Background(), "other-worker"); !errors.Is(err, taskruntime.ErrInvalidArgument) {
		t.Fatalf("ClaimNextExecution(other worker) error = %v, want InvalidArgument", err)
	}
	if fixture.executor.commits != commits {
		t.Fatal("invalid worker entered Runtime write transaction")
	}
}

func TestClaimNextExecutionThreeWayHashMismatchInterruptsAtomically(t *testing.T) {
	t.Parallel()
	tests := map[string]func(*claimFixture, contracts.TaskID){
		"TaskExecution differs": func(fixture *claimFixture, taskID contracts.TaskID) {
			execution := fixture.executor.store.executions[executionKey(taskID, 1)]
			execution.ExecutionConfigHash = contracts.ExecutionConfigHash(strings.Repeat("d", 64))
			fixture.executor.store.executions[executionKey(taskID, 1)] = execution
		},
		"current config differs": func(fixture *claimFixture, taskID contracts.TaskID) {
			oldHash := contracts.ExecutionConfigHash(strings.Repeat("a", 64))
			execution := fixture.executor.store.executions[executionKey(taskID, 1)]
			execution.ExecutionConfigHash = oldHash
			fixture.executor.store.executions[executionKey(taskID, 1)] = execution
			fixture.executor.store.checkpoints[0].ExecutionConfigHash = oldHash
		},
		"checkpoint differs": func(fixture *claimFixture, _ contracts.TaskID) {
			fixture.executor.store.checkpoints[0].ExecutionConfigHash = contracts.ExecutionConfigHash(strings.Repeat("b", 64))
		},
	}
	for name, mutate := range tests {
		name, mutate := name, mutate
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			fixture := newClaimFixture(t)
			taskID := contracts.TaskID("task-hash")
			fixture.seedQueuedTask(taskID, fixture.now.Add(-time.Minute), fixture.now.Add(-2*time.Minute), fixture.hash)
			mutate(fixture, taskID)

			result, err := fixture.service.ClaimNextExecution(context.Background(), fixture.workerID)
			if err != nil {
				t.Fatalf("ClaimNextExecution() error = %v", err)
			}
			if _, ok := result.(contracts.ClaimResultConfigMismatchInterrupted); !ok {
				t.Fatalf("result = %T, want ConfigMismatchInterrupted", result)
			}
			store := fixture.executor.snapshot()
			task := store.tasks[taskID]
			run := store.runs[taskID]
			execution := store.executions[executionKey(taskID, 1)]
			if task.Status != contracts.TaskStatusInterrupted || task.QueuedAt != nil ||
				task.ErrorCode == nil || *task.ErrorCode != contracts.ErrorCodeConfigVersionMismatch ||
				run.Status != contracts.RunStatusPending || run.ErrorCode != nil ||
				execution.Status != contracts.TaskExecutionStatusInterrupted || execution.WorkerID != nil ||
				execution.ObservedConfigHash == nil || *execution.ObservedConfigHash != fixture.hash ||
				execution.ErrorCode == nil || *execution.ErrorCode != contracts.ErrorCodeConfigVersionMismatch ||
				len(store.reports) != 1 {
				t.Fatalf("config mismatch facts = task=%+v run=%+v execution=%+v reports=%d",
					task, run, execution, len(store.reports))
			}
			assertSameFakeTransaction(t, fixture.checkpoints.seenTx, fixture.reports.seenTx)
		})
	}
}

func TestClaimNextExecutionWritesMinimumTaskLogEvents(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name         string
		event        string
		level        taskruntime.TaskLogLevel
		messageParts []string
		configure    func(*claimFixture, contracts.TaskID)
	}{
		{name: "claimed", event: "ExecutionClaimed", level: taskruntime.TaskLogLevelInfo, messageParts: []string{"execution claimed"}},
		{
			name: "config mismatch interrupted", event: "ExecutionInterrupted", level: taskruntime.TaskLogLevelError,
			messageParts: []string{string(contracts.ErrorCodeConfigVersionMismatch)},
			configure: func(fixture *claimFixture, _ contracts.TaskID) {
				fixture.executor.store.checkpoints[0].ExecutionConfigHash = contracts.ExecutionConfigHash(strings.Repeat("b", 64))
			},
		},
		{
			name: "checkpoint invalid terminalized", event: "TaskTerminalized", level: taskruntime.TaskLogLevelError,
			messageParts: []string{string(contracts.TaskStatusFailed), string(contracts.ErrorCodeCheckpointInvalid)},
			configure: func(fixture *claimFixture, taskID contracts.TaskID) {
				fixture.checkpoints.overrides[taskID] = taskruntime.ClaimCheckpointInvalid{
					ReasonCode: contracts.ReasonCodeCheckpointNotFound,
				}
			},
		},
		{
			name: "expired terminalized", event: "TaskTerminalized", level: taskruntime.TaskLogLevelError,
			messageParts: []string{
				string(contracts.TaskStatusFailed), string(contracts.ErrorCodeTaskTimeout),
				string(contracts.TerminationReasonTimedOut),
			},
			configure: func(fixture *claimFixture, taskID contracts.TaskID) {
				task := fixture.executor.store.tasks[taskID]
				task.DeadlineAt = fixture.now
				fixture.executor.store.tasks[taskID] = task
			},
		},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			fixture := newClaimFixture(t)
			taskID := contracts.TaskID("task-log-" + strings.ReplaceAll(test.name, " ", "-"))
			fixture.seedQueuedTask(taskID, fixture.now.Add(-time.Minute), fixture.now.Add(-2*time.Minute), fixture.hash)
			if test.configure != nil {
				test.configure(fixture, taskID)
			}

			result, err := fixture.service.ClaimNextExecution(context.Background(), fixture.workerID)
			if err != nil {
				t.Fatalf("ClaimNextExecution() error = %v", err)
			}
			store := fixture.executor.snapshot()
			assertSingleTaskLog(t, store.logs, test.event, taskID, store.tasks[taskID].CurrentRunID, 1)
			assertIndependentLogTransaction(t, fixture.repositories)
			if store.logs[0].Level != test.level {
				t.Fatalf("%s level = %s, want %s", test.event, store.logs[0].Level, test.level)
			}
			for _, part := range test.messageParts {
				if !strings.Contains(store.logs[0].Message, part) {
					t.Fatalf("%s message = %q, want safe field %q", test.event, store.logs[0].Message, part)
				}
			}
			if strings.Contains(store.logs[0].Message, string(fixture.hash)) {
				t.Fatalf("%s message leaked execution config hash", test.event)
			}
			_ = result
		})
	}
}

func TestClaimTaskLogFailureDoesNotChangeCommittedResults(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name      string
		configure func(*claimFixture, contracts.TaskID)
		assert    func(*testing.T, contracts.ClaimResult, *fakeStore, contracts.TaskID)
	}{
		{
			name: "claimed",
			assert: func(t *testing.T, result contracts.ClaimResult, store *fakeStore, taskID contracts.TaskID) {
				t.Helper()
				if _, ok := result.(contracts.ClaimResultClaimed); !ok || store.tasks[taskID].Status != contracts.TaskStatusRunning {
					t.Fatalf("Claim result/state = %T/%s, want committed Claimed", result, store.tasks[taskID].Status)
				}
			},
		},
		{
			name: "interrupted",
			configure: func(fixture *claimFixture, _ contracts.TaskID) {
				fixture.executor.store.checkpoints[0].ExecutionConfigHash = contracts.ExecutionConfigHash(strings.Repeat("c", 64))
			},
			assert: func(t *testing.T, result contracts.ClaimResult, store *fakeStore, taskID contracts.TaskID) {
				t.Helper()
				if _, ok := result.(contracts.ClaimResultConfigMismatchInterrupted); !ok || store.tasks[taskID].Status != contracts.TaskStatusInterrupted {
					t.Fatalf("Claim result/state = %T/%s, want committed interruption", result, store.tasks[taskID].Status)
				}
			},
		},
		{
			name: "terminalized",
			configure: func(fixture *claimFixture, taskID contracts.TaskID) {
				fixture.checkpoints.overrides[taskID] = taskruntime.ClaimCheckpointInvalid{
					ReasonCode: contracts.ReasonCodeCheckpointNotFound,
				}
			},
			assert: func(t *testing.T, result contracts.ClaimResult, store *fakeStore, taskID contracts.TaskID) {
				t.Helper()
				if _, ok := result.(contracts.ClaimResultCheckpointInvalidTerminalized); !ok || store.tasks[taskID].Status != contracts.TaskStatusFailed {
					t.Fatalf("Claim result/state = %T/%s, want committed terminalization", result, store.tasks[taskID].Status)
				}
			},
		},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			fixture := newClaimFixture(t)
			taskID := contracts.TaskID("task-log-failure-" + test.name)
			fixture.seedQueuedTask(taskID, fixture.now.Add(-time.Minute), fixture.now.Add(-2*time.Minute), fixture.hash)
			if test.configure != nil {
				test.configure(fixture, taskID)
			}
			fixture.repositories.failOperation["task_log.append"] = errors.New("task log unavailable")

			result, err := fixture.service.ClaimNextExecution(context.Background(), fixture.workerID)
			if err != nil {
				t.Fatalf("ClaimNextExecution() error = %v, want committed business result", err)
			}
			store := fixture.executor.snapshot()
			test.assert(t, result, store, taskID)
			if len(store.logs) != 0 || fixture.executor.rollbacks != 1 {
				t.Fatalf("failed TaskLog persisted/changed transaction result: logs=%d rollbacks=%d",
					len(store.logs), fixture.executor.rollbacks)
			}
		})
	}
}

func TestClaimNextExecutionCheckpointInvalidTerminalizes(t *testing.T) {
	t.Parallel()
	fixture := newClaimFixture(t)
	taskID := contracts.TaskID("task-checkpoint-invalid")
	fixture.seedQueuedTask(taskID, fixture.now.Add(-time.Minute), fixture.now.Add(-2*time.Minute), fixture.hash)
	fixture.checkpoints.overrides[taskID] = taskruntime.ClaimCheckpointInvalid{
		ReasonCode: contracts.ReasonCodeCheckpointNotFound,
	}

	result, err := fixture.service.ClaimNextExecution(context.Background(), fixture.workerID)
	if err != nil {
		t.Fatalf("ClaimNextExecution() error = %v", err)
	}
	if _, ok := result.(contracts.ClaimResultCheckpointInvalidTerminalized); !ok {
		t.Fatalf("result = %T, want CheckpointInvalidTerminalized", result)
	}
	assertTerminalClaimFacts(t, fixture.executor.snapshot(), taskID, contracts.ErrorCodeCheckpointInvalid, nil, nil)
}

func TestClaimNextExecutionExpiredTerminalizes(t *testing.T) {
	t.Parallel()
	fixture := newClaimFixture(t)
	taskID := contracts.TaskID("task-expired")
	fixture.seedQueuedTask(taskID, fixture.now.Add(-time.Minute), fixture.now.Add(-2*time.Minute), fixture.hash)
	task := fixture.executor.store.tasks[taskID]
	task.DeadlineAt = fixture.now
	fixture.executor.store.tasks[taskID] = task

	result, err := fixture.service.ClaimNextExecution(context.Background(), fixture.workerID)
	if err != nil {
		t.Fatalf("ClaimNextExecution() error = %v", err)
	}
	if _, ok := result.(contracts.ClaimResultExpiredTerminalized); !ok {
		t.Fatalf("result = %T, want ExpiredTerminalized", result)
	}
	reason := contracts.TerminationReasonTimedOut
	assertTerminalClaimFacts(t, fixture.executor.snapshot(), taskID, contracts.ErrorCodeTaskTimeout, nil, &reason)
}

func TestClaimNextExecutionDataInconsistentTerminalizes(t *testing.T) {
	t.Parallel()
	fixture := newClaimFixture(t)
	taskID := contracts.TaskID("task-inconsistent")
	fixture.seedQueuedTask(taskID, fixture.now.Add(-time.Minute), fixture.now.Add(-2*time.Minute), fixture.hash)
	run := fixture.executor.store.runs[taskID]
	run.Status = contracts.RunStatusRunning
	fixture.executor.store.runs[taskID] = run

	result, err := fixture.service.ClaimNextExecution(context.Background(), fixture.workerID)
	if err != nil {
		t.Fatalf("ClaimNextExecution() error = %v", err)
	}
	if _, ok := result.(contracts.ClaimResultDataInconsistentTerminalized); !ok {
		t.Fatalf("result = %T, want DataInconsistentTerminalized", result)
	}
	invariant := contracts.InvariantCodeCrossObjectStateInvalid
	assertTerminalClaimFacts(t, fixture.executor.snapshot(), taskID, contracts.ErrorCodeDataInconsistent, &invariant, nil)
}

func TestClaimNextExecutionConditionMissRechecksBeforeNoWork(t *testing.T) {
	t.Parallel()
	fixture := newClaimFixture(t)
	taskID := contracts.TaskID("task-race-removed")
	fixture.seedQueuedTask(taskID, fixture.now.Add(-time.Minute), fixture.now.Add(-2*time.Minute), fixture.hash)
	fixture.repositories.missOperation["task.update"] = true
	otherWorker := contracts.WorkerID("other-runtime-worker")
	competitorReady := make(chan struct{})
	startCompetitor := make(chan struct{})
	competitorDone := make(chan error, 1)
	go func() {
		close(competitorReady)
		<-startCompetitor
		competitorDone <- fixture.executor.Execute(context.Background(), func(_ context.Context, token contracts.RuntimeWriteTx) error {
			transaction := token.(*fakeWriteTx)
			task := transaction.store.tasks[taskID]
			task.Status = contracts.TaskStatusRunning
			task.QueuedAt = nil
			task.StartedAt = &fixture.now
			transaction.store.tasks[taskID] = task
			run := transaction.store.runs[taskID]
			run.Status = contracts.RunStatusRunning
			run.StartedAt = &fixture.now
			transaction.store.runs[taskID] = run
			execution := transaction.store.executions[executionKey(taskID, 1)]
			execution.Status = contracts.TaskExecutionStatusRunning
			execution.WorkerID = &otherWorker
			execution.StartedAt = &fixture.now
			transaction.store.executions[executionKey(taskID, 1)] = execution
			return nil
		})
	}()
	<-competitorReady
	var competitorErr error
	fixture.executor.afterRollback = func(_ *fakeStore) {
		close(startCompetitor)
		competitorErr = <-competitorDone
	}

	result, err := fixture.service.ClaimNextExecution(context.Background(), fixture.workerID)
	if competitorErr != nil {
		t.Fatalf("competing Claim transaction error = %v", competitorErr)
	}
	if err != nil {
		t.Fatalf("ClaimNextExecution() error = %v", err)
	}
	if _, ok := result.(contracts.ClaimResultNoWork); !ok {
		t.Fatalf("result = %T, want NoWork after confirmed legal removal", result)
	}
	store := fixture.executor.snapshot()
	if execution := store.executions[executionKey(taskID, 1)]; execution.Status != contracts.TaskExecutionStatusRunning || execution.WorkerID == nil || *execution.WorkerID != otherWorker {
		t.Fatalf("recheck changed winner's Execution: %+v", execution)
	}
	if fixture.executor.rollbacks != 1 || fixture.executor.commits != 2 {
		t.Fatalf("transactions = rollbacks:%d commits:%d, want original rollback, competitor commit, and recheck commit",
			fixture.executor.rollbacks, fixture.executor.commits)
	}
}

func TestClaimNextExecutionConditionMissStillQueuedIsPersistenceError(t *testing.T) {
	t.Parallel()
	fixture := newClaimFixture(t)
	taskID := contracts.TaskID("task-race-still-valid")
	fixture.seedQueuedTask(taskID, fixture.now.Add(-time.Minute), fixture.now.Add(-2*time.Minute), fixture.hash)
	fixture.repositories.missOperation["task.update"] = true

	result, err := fixture.service.ClaimNextExecution(context.Background(), fixture.workerID)
	if result != nil || !errors.Is(err, taskruntime.ErrPersistenceInvariantViolation) {
		t.Fatalf("ClaimNextExecution() = %T, %v; want nil PersistenceInvariantViolation", result, err)
	}
	assertQueuedUnchanged(t, fixture.executor.snapshot(), taskID)
	if fixture.executor.rollbacks != 2 {
		t.Fatalf("rollbacks = %d, want original and recheck rollback", fixture.executor.rollbacks)
	}
}

func TestClaimNextExecutionConditionMissRecheckClassifiesFacts(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		mutate func(*claimFixture, contracts.TaskID, *fakeStore)
		assert func(*testing.T, contracts.ClaimResult, error, *claimFixture, contracts.TaskID)
	}{
		{
			name: "checkpoint invalid",
			mutate: func(_ *claimFixture, taskID contracts.TaskID, store *fakeStore) {
				for index := range store.checkpoints {
					if store.checkpoints[index].TaskID == taskID {
						store.checkpoints[index].CheckpointSequence = 0
					}
				}
			},
			assert: func(t *testing.T, result contracts.ClaimResult, err error, fixture *claimFixture, taskID contracts.TaskID) {
				t.Helper()
				if err != nil {
					t.Fatalf("ClaimNextExecution() error = %v", err)
				}
				if _, ok := result.(contracts.ClaimResultCheckpointInvalidTerminalized); !ok {
					t.Fatalf("result = %T, want CheckpointInvalidTerminalized", result)
				}
				assertTerminalClaimFacts(t, fixture.executor.snapshot(), taskID, contracts.ErrorCodeCheckpointInvalid, nil, nil)
			},
		},
		{
			name: "data inconsistent",
			mutate: func(_ *claimFixture, taskID contracts.TaskID, store *fakeStore) {
				run := store.runs[taskID]
				run.Status = contracts.RunStatusRunning
				store.runs[taskID] = run
			},
			assert: func(t *testing.T, result contracts.ClaimResult, err error, fixture *claimFixture, taskID contracts.TaskID) {
				t.Helper()
				if err != nil {
					t.Fatalf("ClaimNextExecution() error = %v", err)
				}
				if _, ok := result.(contracts.ClaimResultDataInconsistentTerminalized); !ok {
					t.Fatalf("result = %T, want DataInconsistentTerminalized", result)
				}
				invariant := contracts.InvariantCodeCrossObjectStateInvalid
				assertTerminalClaimFacts(t, fixture.executor.snapshot(), taskID, contracts.ErrorCodeDataInconsistent, &invariant, nil)
			},
		},
		{
			name: "repository failure",
			mutate: func(fixture *claimFixture, _ contracts.TaskID, _ *fakeStore) {
				fixture.repositories.failOperation["task.lock"] = errors.New("repository unavailable")
			},
			assert: func(t *testing.T, result contracts.ClaimResult, err error, _ *claimFixture, _ contracts.TaskID) {
				t.Helper()
				if result != nil || err == nil || !strings.Contains(err.Error(), "repository unavailable") {
					t.Fatalf("ClaimNextExecution() = %T, %v; want repository system error", result, err)
				}
			},
		},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			fixture := newClaimFixture(t)
			taskID := contracts.TaskID("task-race-" + strings.ReplaceAll(test.name, " ", "-"))
			fixture.seedQueuedTask(taskID, fixture.now.Add(-time.Minute), fixture.now.Add(-2*time.Minute), fixture.hash)
			fixture.repositories.missOperation["task.update"] = true
			fixture.executor.afterRollback = func(store *fakeStore) {
				test.mutate(fixture, taskID, store)
			}
			result, err := fixture.service.ClaimNextExecution(context.Background(), fixture.workerID)
			test.assert(t, result, err, fixture, taskID)
		})
	}
}

func TestClaimNextExecutionPortFailureRollsBackClaimAndTerminalization(t *testing.T) {
	t.Parallel()
	t.Run("execution checkpoint save", func(t *testing.T) {
		fixture := newClaimFixture(t)
		taskID := contracts.TaskID("task-save-failure")
		fixture.seedQueuedTask(taskID, fixture.now.Add(-time.Minute), fixture.now.Add(-2*time.Minute), fixture.hash)
		fixture.checkpoints.failSave = errors.New("checkpoint write failed")
		if _, err := fixture.service.ClaimNextExecution(context.Background(), fixture.workerID); err == nil {
			t.Fatal("ClaimNextExecution() error = nil, want checkpoint failure")
		}
		assertQueuedUnchanged(t, fixture.executor.snapshot(), taskID)
	})
	t.Run("pending report", func(t *testing.T) {
		fixture := newClaimFixture(t)
		taskID := contracts.TaskID("task-report-failure")
		fixture.seedQueuedTask(taskID, fixture.now.Add(-time.Minute), fixture.now.Add(-2*time.Minute), fixture.hash)
		fixture.executor.store.checkpoints[0].ExecutionConfigHash = contracts.ExecutionConfigHash(strings.Repeat("c", 64))
		fixture.reports.fail = errors.New("report write failed")
		if _, err := fixture.service.ClaimNextExecution(context.Background(), fixture.workerID); err == nil {
			t.Fatal("ClaimNextExecution() error = nil, want report failure")
		}
		assertQueuedUnchanged(t, fixture.executor.snapshot(), taskID)
	})
}

func TestClaimNextExecutionContinuationPreservesStartedAt(t *testing.T) {
	t.Parallel()
	fixture := newClaimFixture(t)
	taskID := contracts.TaskID("task-continuation")
	fixture.seedQueuedTask(taskID, fixture.now.Add(-time.Minute), fixture.now.Add(-10*time.Minute), fixture.hash)
	startedAt := fixture.now.Add(-5 * time.Minute)
	task := fixture.executor.store.tasks[taskID]
	task.Status = contracts.TaskStatusRunning
	task.StartedAt = &startedAt
	fixture.executor.store.tasks[taskID] = task
	planID := contracts.PlanID("plan-1")
	stepID := contracts.StepID("step-1")
	run := fixture.executor.store.runs[taskID]
	run.Status = contracts.RunStatusRunning
	run.PlanID = &planID
	run.CurrentStepID = &stepID
	run.StartedAt = &startedAt
	fixture.executor.store.runs[taskID] = run
	execution := fixture.executor.store.executions[executionKey(taskID, 1)]
	execution.StartedAt = &startedAt
	fixture.executor.store.executions[executionKey(taskID, 1)] = execution
	initialCheckpointCount := len(fixture.executor.store.checkpoints)

	result, err := fixture.service.ClaimNextExecution(context.Background(), fixture.workerID)
	if err != nil {
		t.Fatalf("ClaimNextExecution() error = %v", err)
	}
	if _, ok := result.(contracts.ClaimResultClaimed); !ok {
		t.Fatalf("result = %T, want Claimed", result)
	}
	store := fixture.executor.snapshot()
	if !store.tasks[taskID].StartedAt.Equal(startedAt) || !store.runs[taskID].StartedAt.Equal(startedAt) ||
		!store.executions[executionKey(taskID, 1)].StartedAt.Equal(startedAt) {
		t.Fatal("continuation Claim overwrote first started_at")
	}
	if store.tasks[taskID].QueuedAt != nil {
		t.Fatalf("continuation Claim left queued_at = %v, want nil", store.tasks[taskID].QueuedAt)
	}
	if len(store.checkpoints) != initialCheckpointCount {
		t.Fatal("Claim with existing Plan created a GENERATE_PLAN Checkpoint")
	}
	result, err = fixture.service.ClaimNextExecution(context.Background(), fixture.workerID)
	if err != nil {
		t.Fatalf("second ClaimNextExecution() error = %v", err)
	}
	if _, ok := result.(contracts.ClaimResultNoWork); !ok {
		t.Fatalf("second ClaimNextExecution() result = %T, want NoWork", result)
	}
}

type claimFixture struct {
	service      *taskruntime.ClaimTaskService
	executor     *fakeExecutor
	repositories *fakeRepositories
	checkpoints  *fakeCheckpointPort
	reports      *fakePendingReportWriter
	configs      *fakeAgentConfigSource
	now          time.Time
	workerID     contracts.WorkerID
	hash         contracts.ExecutionConfigHash
}

func newClaimFixture(t *testing.T) *claimFixture {
	t.Helper()
	now := time.Date(2026, time.August, 5, 12, 0, 0, 0, time.UTC)
	executor := newFakeExecutor()
	repositories := newFakeRepositories(executor, now)
	tasks, runs, executions, _ := fakeRepositoryPorts(repositories)
	checkpoint := &fakeCheckpointPort{overrides: make(map[contracts.TaskID]taskruntime.ClaimCheckpointResult)}
	reports := &fakePendingReportWriter{}
	agent := loadedAgentConfig(t)
	hash, err := taskruntime.HashExecutionConfigV1(agent.ExecutionConfig)
	if err != nil {
		t.Fatalf("HashExecutionConfigV1() error = %v", err)
	}
	configs := &fakeAgentConfigSource{agents: map[contracts.AgentID]taskruntime.AgentRuntimeConfig{"agent-default": agent}}
	workerID := contracts.WorkerID("runtime-worker-1")
	service, err := taskruntime.NewClaimTaskService(taskruntime.ClaimTaskDependencies{
		Executor: executor, Tasks: tasks, Runs: runs, Executions: executions,
		TaskLogs: fakeTaskLogRepository{repositories}, Clock: repositories,
		Configs: configs, Checkpoints: checkpoint, Reports: reports,
		Policy: lifecycle.New(), RuntimeWorker: workerID,
	})
	if err != nil {
		t.Fatalf("NewClaimTaskService() error = %v", err)
	}
	return &claimFixture{
		service: service, executor: executor, repositories: repositories, checkpoints: checkpoint,
		reports: reports, configs: configs, now: now, workerID: workerID, hash: hash,
	}
}

func (f *claimFixture) seedQueuedTask(
	taskID contracts.TaskID,
	queuedAt time.Time,
	createdAt time.Time,
	hash contracts.ExecutionConfigHash,
) {
	runID := contracts.RunID("run-" + string(taskID))
	f.executor.store.tasks[taskID] = taskruntime.Task{
		TaskID: taskID, AgentID: "agent-default", CreatedBy: "operator-1", Input: "inspect",
		Status: contracts.TaskStatusPending, CurrentRunID: runID, CurrentExecutionVersion: 1,
		DeadlineAt: f.now.Add(time.Hour), QueuedAt: &queuedAt, CreatedAt: createdAt,
	}
	f.executor.store.runs[taskID] = taskruntime.Run{
		RunID: runID, TaskID: taskID, Status: contracts.RunStatusPending, Context: []byte(`{}`),
	}
	f.executor.store.executions[executionKey(taskID, 1)] = taskruntime.TaskExecution{
		TaskExecutionID: taskruntime.TaskExecutionID("execution-" + string(taskID)),
		TaskID:          taskID, ExecutionVersion: 1, Status: contracts.TaskExecutionStatusQueued,
		ExecutionConfigHash: hash, CreatedAt: createdAt,
	}
	f.executor.store.checkpoints = append(f.executor.store.checkpoints, taskruntime.RuntimeCheckpoint{
		CheckpointID: contracts.CheckpointID("checkpoint-" + string(taskID)),
		TaskID:       taskID, RunID: runID, ExecutionVersion: 1, ExecutionConfigHash: hash,
		NextAction: contracts.CheckpointNextActionGeneratePlan, CheckpointSequence: 1,
	})
}

func assertTerminalClaimFacts(
	t *testing.T,
	store *fakeStore,
	taskID contracts.TaskID,
	errorCode contracts.ErrorCode,
	invariantCode *contracts.InvariantCode,
	terminationReason *contracts.TerminationReason,
) {
	t.Helper()
	task := store.tasks[taskID]
	run := store.runs[taskID]
	execution := store.executions[executionKey(taskID, 1)]
	if task.Status != contracts.TaskStatusFailed || task.QueuedAt != nil || task.EndedAt == nil ||
		task.ErrorCode == nil || *task.ErrorCode != errorCode ||
		run.Status != contracts.RunStatusFailed || run.EndedAt == nil || run.ErrorCode == nil || *run.ErrorCode != errorCode ||
		execution.Status != contracts.TaskExecutionStatusFailed || execution.EndedAt == nil ||
		execution.ErrorCode == nil || *execution.ErrorCode != errorCode ||
		!equalInvariantCode(execution.InvariantCode, invariantCode) ||
		!equalTerminationReason(execution.TerminationReason, terminationReason) || len(store.reports) != 1 {
		t.Fatalf("terminal facts = task=%+v run=%+v execution=%+v reports=%d",
			task, run, execution, len(store.reports))
	}
}

func assertQueuedUnchanged(t *testing.T, store *fakeStore, taskID contracts.TaskID) {
	t.Helper()
	if task := store.tasks[taskID]; task.Status != contracts.TaskStatusPending || task.QueuedAt == nil {
		t.Fatalf("Task changed after rollback: %+v", task)
	}
	if run := store.runs[taskID]; run.Status != contracts.RunStatusPending {
		t.Fatalf("Run changed after rollback: %+v", run)
	}
	if execution := store.executions[executionKey(taskID, 1)]; execution.Status != contracts.TaskExecutionStatusQueued || execution.WorkerID != nil {
		t.Fatalf("Execution changed after rollback: %+v", execution)
	}
	if len(store.reports) != 0 || len(store.checkpoints) != 1 || len(store.logs) != 0 {
		t.Fatalf("port facts changed after rollback: reports=%d checkpoints=%d logs=%d",
			len(store.reports), len(store.checkpoints), len(store.logs))
	}
}

func equalInvariantCode(left, right *contracts.InvariantCode) bool {
	if left == nil || right == nil {
		return left == nil && right == nil
	}
	return *left == *right
}

func equalTerminationReason(left, right *contracts.TerminationReason) bool {
	if left == nil || right == nil {
		return left == nil && right == nil
	}
	return *left == *right
}
