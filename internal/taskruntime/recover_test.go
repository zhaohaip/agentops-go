package taskruntime_test

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/zhaohaip/agentops-go/internal/contracts"
	"github.com/zhaohaip/agentops-go/internal/lifecycle"
	"github.com/zhaohaip/agentops-go/internal/taskruntime"
	"github.com/zhaohaip/agentops-go/internal/taskruntime/activecall"
)

func TestRecoverTaskGeneratePlanIsAtomicAndIdempotent(t *testing.T) {
	h := newRecoverHarness(t)
	result, err := h.service.RecoverTask(context.Background(), taskruntime.RecoverTaskRequest{
		CommandID: "recover-1", TaskID: h.taskID, OperatorID: "operator-1",
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.SourceExecutionVersion != 1 || result.NewExecutionVersion != 2 ||
		result.TaskStatus != contracts.TaskStatusPending || result.RunStatus != contracts.RunStatusPending ||
		result.ExecutionStatus != contracts.TaskExecutionStatusQueued || result.RecoveryCheckpointID != "recovery-start-2" {
		t.Fatalf("Recover result = %+v", result)
	}
	store := h.executor.snapshot()
	task := store.tasks[h.taskID]
	run := store.runs[h.taskID]
	oldExecution := store.executions[executionKey(h.taskID, 1)]
	newExecution := store.executions[executionKey(h.taskID, 2)]
	if task.CurrentExecutionVersion != 2 || task.Status != contracts.TaskStatusPending || task.QueuedAt == nil ||
		run.Status != contracts.RunStatusPending || oldExecution.Status != contracts.TaskExecutionStatusInterrupted ||
		newExecution.Status != contracts.TaskExecutionStatusQueued || newExecution.WorkerID != nil ||
		newExecution.ObservedConfigHash != nil || len(store.checkpoints) != 2 || len(store.receipts) != 1 {
		t.Fatalf("Recover facts = task:%+v run:%+v old:%+v new:%+v checkpoints:%#v receipts:%#v",
			task, run, oldExecution, newExecution, store.checkpoints, store.receipts)
	}
	configCalls := h.configs.calls
	replayed, err := h.service.RecoverTask(context.Background(), taskruntime.RecoverTaskRequest{
		CommandID: "recover-1", TaskID: h.taskID, OperatorID: "operator-1",
	})
	if err != nil || replayed != result {
		t.Fatalf("replay = %+v, %v", replayed, err)
	}
	if h.configs.calls != configCalls || len(h.executor.snapshot().checkpoints) != 2 {
		t.Fatal("Receipt replay re-read configuration or repeated recovery")
	}
	logs := h.executor.snapshot().logs
	if len(logs) != 1 || logs[0].Event != "CheckpointRestored" || logs[0].ExecutionVersion == nil ||
		*logs[0].ExecutionVersion != 2 ||
		logs[0].Message != "checkpoint restored: source_execution_version=1 new_execution_version=2" {
		t.Fatalf("CheckpointRestored logs = %#v", logs)
	}
	if strings.Contains(logs[0].Message, string(h.hash)) {
		t.Fatal("CheckpointRestored log contains execution config hash")
	}
}

func TestRecoverTaskLogFailureOrDropDoesNotChangeDomainResult(t *testing.T) {
	tests := []struct {
		name      string
		configure func(*recoverHarness)
	}{
		{name: "append failure", configure: func(h *recoverHarness) {
			h.repositories.failOperation["task_log.append"] = errors.New("TaskLog unavailable")
		}},
		{name: "TryExecute dropped", configure: func(h *recoverHarness) {
			replaceRecoverExecutor(t, h, droppingRecoverExecutor{core: h.executor})
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			h := newRecoverHarness(t)
			test.configure(h)
			result, err := h.service.RecoverTask(context.Background(), taskruntime.RecoverTaskRequest{
				CommandID: taskruntime.CommandID("recover-log-" + test.name), TaskID: h.taskID, OperatorID: "operator-1",
			})
			if err != nil || result.NewExecutionVersion != 2 {
				t.Fatalf("Recover result = %+v, %v", result, err)
			}
			store := h.executor.snapshot()
			if store.tasks[h.taskID].CurrentExecutionVersion != 2 ||
				store.executions[executionKey(h.taskID, 2)].Status != contracts.TaskExecutionStatusQueued ||
				len(store.receipts) != 1 || len(store.checkpoints) != 2 || len(store.logs) != 0 {
				t.Fatalf("log failure changed Recover facts: %#v", store)
			}
		})
	}
}

func TestRecoverTaskConfigMismatchReceiptRequiresNewCommand(t *testing.T) {
	h := newRecoverHarness(t)
	h.checkpoints.hash = contracts.ExecutionConfigHash(strings.Repeat("b", 64))
	request := taskruntime.RecoverTaskRequest{CommandID: "recover-mismatch", TaskID: h.taskID, OperatorID: "operator-1"}
	if _, err := h.service.RecoverTask(context.Background(), request); !errors.Is(err, taskruntime.ErrRecoverConfigMismatch) {
		t.Fatalf("mismatch error = %v", err)
	}
	store := h.executor.snapshot()
	if store.tasks[h.taskID].CurrentExecutionVersion != 1 || len(store.checkpoints) != 1 || len(store.receipts) != 1 {
		t.Fatalf("mismatch partially wrote domain facts: %#v", store)
	}
	h.checkpoints.hash = h.hash
	if _, err := h.service.RecoverTask(context.Background(), request); !errors.Is(err, taskruntime.ErrRecoverConfigMismatch) {
		t.Fatalf("same command replay = %v", err)
	}
	if _, err := h.service.RecoverTask(context.Background(), taskruntime.RecoverTaskRequest{
		CommandID: "recover-retry", TaskID: h.taskID, OperatorID: "operator-1",
	}); err != nil {
		t.Fatalf("new command retry = %v", err)
	}
}

func TestRecoverTaskRollsBackEveryCoreWriteWhenRecoveryStartFails(t *testing.T) {
	h := newRecoverHarness(t)
	h.checkpoints.failCreate = errors.New("checkpoint unavailable")
	if _, err := h.service.RecoverTask(context.Background(), taskruntime.RecoverTaskRequest{
		CommandID: "recover-rollback", TaskID: h.taskID, OperatorID: "operator-1",
	}); err == nil {
		t.Fatal("RecoverTask() succeeded")
	}
	store := h.executor.snapshot()
	if store.tasks[h.taskID].CurrentExecutionVersion != 1 || len(store.executions) != 1 ||
		len(store.checkpoints) != 1 || len(store.receipts) != 0 {
		t.Fatalf("failed Recover left partial writes: %#v", store)
	}
}

func TestRecoverTaskRejectsOldExecutionResultAfterVersionSwitch(t *testing.T) {
	h := newRecoverHarness(t)
	if _, err := h.service.RecoverTask(context.Background(), taskruntime.RecoverTaskRequest{
		CommandID: "recover-version", TaskID: h.taskID, OperatorID: "operator-1",
	}); err != nil {
		t.Fatal(err)
	}
	err := h.executor.Execute(context.Background(), func(ctx context.Context, tx contracts.RuntimeWriteTx) error {
		updated, err := fakeExecutionRepository{h.repositories}.Update(ctx, tx, taskruntime.TaskExecutionUpdate{
			TaskID: h.taskID, ExecutionVersion: 1, ExpectedStatus: contracts.TaskExecutionStatusInterrupted,
			Status: contracts.TaskExecutionStatusFailed,
		})
		if err != nil {
			return err
		}
		if updated {
			return errors.New("old execution update bypassed current version guard")
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if h.executor.snapshot().executions[executionKey(h.taskID, 1)].Status != contracts.TaskExecutionStatusInterrupted {
		t.Fatal("old execution was modified")
	}
}

func TestExpiredRecoverPreservesTerminalTaskAndReplaysStateConflict(t *testing.T) {
	tests := []struct {
		name            string
		taskStatus      contracts.TaskStatus
		runStatus       contracts.RunStatus
		executionStatus contracts.TaskExecutionStatus
		errorCode       *contracts.ErrorCode
	}{
		{name: "Completed", taskStatus: contracts.TaskStatusCompleted, runStatus: contracts.RunStatusCompleted,
			executionStatus: contracts.TaskExecutionStatusCompleted},
		{name: "Cancelled", taskStatus: contracts.TaskStatusCancelled, runStatus: contracts.RunStatusFailed,
			executionStatus: contracts.TaskExecutionStatusFailed, errorCode: errorCode(contracts.ErrorCodeTaskCancelled)},
		{name: "Failed", taskStatus: contracts.TaskStatusFailed, runStatus: contracts.RunStatusFailed,
			executionStatus: contracts.TaskExecutionStatusFailed, errorCode: errorCode(contracts.ErrorCodeTaskTimeout)},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			h := newRecoverHarness(t)
			store := h.executor.store
			task := store.tasks[h.taskID]
			run := store.runs[h.taskID]
			execution := store.executions[executionKey(h.taskID, 1)]
			endedAt := h.repositories.now.Add(-time.Minute)
			task.Status, task.ErrorCode, task.EndedAt = test.taskStatus, test.errorCode, &endedAt
			task.DeadlineAt = h.repositories.now.Add(-time.Second)
			task.QueuedAt = nil
			run.Status, run.ErrorCode, run.EndedAt = test.runStatus, test.errorCode, &endedAt
			execution.Status, execution.ErrorCode, execution.EndedAt = test.executionStatus, test.errorCode, &endedAt
			execution.ObservedConfigHash = nil
			store.tasks[h.taskID], store.runs[h.taskID] = task, run
			store.executions[executionKey(h.taskID, 1)] = execution
			originalTask, originalRun, originalExecution := task, run, execution
			request := taskruntime.RecoverTaskRequest{CommandID: taskruntime.CommandID("terminal-" + test.name),
				TaskID: h.taskID, OperatorID: "operator-1"}
			for attempt := 0; attempt < 2; attempt++ {
				if _, err := h.service.RecoverTask(context.Background(), request); !errors.Is(err, taskruntime.ErrRecoverStateConflict) {
					t.Fatalf("attempt %d error = %v", attempt+1, err)
				}
			}
			after := h.executor.snapshot()
			if !reflect.DeepEqual(after.tasks[h.taskID], originalTask) ||
				!reflect.DeepEqual(after.runs[h.taskID], originalRun) ||
				!reflect.DeepEqual(after.executions[executionKey(h.taskID, 1)], originalExecution) {
				t.Fatalf("terminal facts changed: task=%+v run=%+v execution=%+v", after.tasks[h.taskID],
					after.runs[h.taskID], after.executions[executionKey(h.taskID, 1)])
			}
			if len(after.receipts) != 1 || h.recovery.applyCalls != 0 || h.checkpoints.validateCalls != 0 {
				t.Fatalf("receipts=%d applyCalls=%d checkpointCalls=%d", len(after.receipts),
					h.recovery.applyCalls, h.checkpoints.validateCalls)
			}
		})
	}
}

type recoverHarness struct {
	taskID       contracts.TaskID
	hash         contracts.ExecutionConfigHash
	executor     *fakeExecutor
	repositories *fakeRepositories
	configs      *fakeAgentConfigSource
	checkpoints  *fakeRecoveryCheckpointPort
	recovery     *fakeRecoveryRepository
	service      *taskruntime.RecoverTaskService
}

func newRecoverHarness(t *testing.T) *recoverHarness {
	t.Helper()
	now := time.Date(2026, 8, 13, 12, 0, 0, 0, time.UTC)
	executor := newFakeExecutor()
	repositories := newFakeRepositories(executor, now)
	config := loadedAgentConfig(t)
	hash, err := taskruntime.HashExecutionConfigV1(config.ExecutionConfig)
	if err != nil {
		t.Fatal(err)
	}
	taskID := contracts.TaskID("task-recover")
	errorCode := contracts.ErrorCodeConfigVersionMismatch
	observedHash := contracts.ExecutionConfigHash(strings.Repeat("b", 64))
	endedAt := now.Add(-time.Minute)
	executor.store.tasks[taskID] = taskruntime.Task{TaskID: taskID, AgentID: "agent-default",
		Status: contracts.TaskStatusInterrupted, CurrentRunID: "run-recover", CurrentExecutionVersion: 1,
		ErrorCode: &errorCode, DeadlineAt: now.Add(time.Hour), CreatedAt: now.Add(-time.Hour)}
	executor.store.runs[taskID] = taskruntime.Run{RunID: "run-recover", TaskID: taskID,
		Status: contracts.RunStatusPending, Context: []byte(`{}`)}
	executor.store.executions[executionKey(taskID, 1)] = taskruntime.TaskExecution{
		TaskExecutionID: "execution-1", TaskID: taskID, ExecutionVersion: 1,
		Status: contracts.TaskExecutionStatusInterrupted, ExecutionConfigHash: hash,
		ObservedConfigHash: &observedHash, ErrorCode: &errorCode, CreatedAt: now.Add(-time.Hour), EndedAt: &endedAt,
	}
	executor.store.checkpoints = append(executor.store.checkpoints, taskruntime.RuntimeCheckpoint{
		CheckpointID: "checkpoint-source", TaskID: taskID, RunID: "run-recover", ExecutionVersion: 1,
		ExecutionConfigHash: hash, NextAction: contracts.CheckpointNextActionGeneratePlan, CheckpointSequence: 1,
	})
	configs := &fakeAgentConfigSource{agents: map[contracts.AgentID]taskruntime.AgentRuntimeConfig{"agent-default": config}}
	checkpoints := &fakeRecoveryCheckpointPort{hash: hash}
	recovery := &fakeRecoveryRepository{repositories: repositories}
	tasks, runs, executions, receipts := fakeRepositoryPorts(repositories)
	service, err := taskruntime.NewRecoverTaskService(taskruntime.RecoverTaskDependencies{
		Executor: executor, Tasks: tasks, Runs: runs, Executions: executions, Recovery: recovery,
		Receipts: receipts, Reports: &fakePendingReportWriter{}, Clock: repositories, Configs: configs,
		Checkpoints: checkpoints, TaskLogs: fakeTaskLogRepository{repositories},
		ActiveCalls: activecall.NewRegistry(), Policy: lifecycle.New(),
	})
	if err != nil {
		t.Fatal(err)
	}
	return &recoverHarness{taskID: taskID, hash: hash, executor: executor, repositories: repositories,
		configs: configs, checkpoints: checkpoints, recovery: recovery, service: service}
}

type fakeRecoveryRepository struct {
	repositories *fakeRepositories
	applyCalls   int
}

func (r *fakeRecoveryRepository) LockRecoveryFacts(_ context.Context, tx contracts.RuntimeWriteTx,
	taskID contracts.TaskID) (taskruntime.TerminationFacts, error) {
	transaction, err := r.repositories.transaction(tx, "recover.lock")
	if err != nil {
		return taskruntime.TerminationFacts{}, err
	}
	task, ok := transaction.store.tasks[taskID]
	if !ok {
		return taskruntime.TerminationFacts{}, taskruntime.ErrRepositoryNotFound
	}
	run, ok := transaction.store.runs[taskID]
	if !ok {
		return taskruntime.TerminationFacts{}, taskruntime.ErrRepositoryNotFound
	}
	execution, ok := transaction.store.executions[executionKey(taskID, task.CurrentExecutionVersion)]
	if !ok {
		return taskruntime.TerminationFacts{}, taskruntime.ErrRepositoryNotFound
	}
	return taskruntime.TerminationFacts{Task: task, Run: run, Execution: execution}, nil
}

func (r *fakeRecoveryRepository) ApplyRecoveryFailure(_ context.Context, tx contracts.RuntimeWriteTx,
	request taskruntime.ApplyRecoveryFailureRequest) (bool, error) {
	r.applyCalls++
	transaction, err := r.repositories.transaction(tx, "recover.fail")
	if err != nil {
		return false, err
	}
	task := transaction.store.tasks[request.TaskID]
	run := transaction.store.runs[request.TaskID]
	execution := transaction.store.executions[executionKey(request.TaskID, request.ExpectedExecutionVersion)]
	if task.Status != request.ExpectedTaskStatus || run.Status != request.ExpectedRunStatus ||
		execution.Status != request.ExpectedExecutionStatus || task.CurrentExecutionVersion != request.ExpectedExecutionVersion {
		return false, nil
	}
	if task.Status.Terminal() || run.Status.Terminal() ||
		execution.Status == contracts.TaskExecutionStatusCompleted || execution.Status == contracts.TaskExecutionStatusFailed {
		return false, nil
	}
	task.Status, run.Status, execution.Status = contracts.TaskStatusFailed, contracts.RunStatusFailed, contracts.TaskExecutionStatusFailed
	task.ErrorCode, run.ErrorCode = &request.ErrorCode, &request.ErrorCode
	task.QueuedAt, task.EndedAt, run.EndedAt = nil, &request.EndedAt, &request.EndedAt
	if execution.ErrorCode == nil || *execution.ErrorCode != contracts.ErrorCodeConfigVersionMismatch {
		execution.ErrorCode = &request.ErrorCode
	}
	execution.TerminationReason = request.TerminationReason
	transaction.store.tasks[request.TaskID], transaction.store.runs[request.TaskID] = task, run
	transaction.store.executions[executionKey(request.TaskID, request.ExpectedExecutionVersion)] = execution
	return true, nil
}

type fakeRecoverySource struct{}

func (fakeRecoverySource) AgentOpsRecoveryCheckpointSource() {}

type fakeRecoveryCheckpointPort struct {
	hash          contracts.ExecutionConfigHash
	failCreate    error
	validateCalls int
}

func (p *fakeRecoveryCheckpointPort) ValidateRecoverySource(_ context.Context, _ contracts.RuntimeWriteTx,
	request taskruntime.ValidateRecoveryCheckpointRequest) (taskruntime.RecoveryCheckpointResult, error) {
	p.validateCalls++
	return taskruntime.RecoveryCheckpointValid{CheckpointID: "checkpoint-source", ExecutionConfigHash: p.hash,
		NextAction: contracts.CheckpointNextActionGeneratePlan, Source: fakeRecoverySource{}}, nil
}

func (p *fakeRecoveryCheckpointPort) CreateRecoveryStart(_ context.Context, tx contracts.RuntimeWriteTx,
	request taskruntime.CreateRecoveryStartRequest) (contracts.CheckpointID, error) {
	if p.failCreate != nil {
		return "", p.failCreate
	}
	transaction := tx.(*fakeWriteTx)
	id := contracts.CheckpointID("recovery-start-2")
	transaction.store.checkpoints = append(transaction.store.checkpoints, taskruntime.RuntimeCheckpoint{
		CheckpointID: id, TaskID: request.TaskID, RunID: request.RunID,
		ExecutionVersion: request.NewExecutionVersion, ExecutionConfigHash: request.ExecutionConfigHash,
		NextAction: contracts.CheckpointNextActionGeneratePlan, CheckpointSequence: 2,
	})
	return id, nil
}

var _ taskruntime.RecoveryRepository = (*fakeRecoveryRepository)(nil)
var _ taskruntime.RecoveryCheckpointPort = (*fakeRecoveryCheckpointPort)(nil)

type droppingRecoverExecutor struct{ core *fakeExecutor }

func (e droppingRecoverExecutor) Execute(ctx context.Context,
	work func(context.Context, contracts.RuntimeWriteTx) error) error {
	return e.core.Execute(ctx, work)
}

func (droppingRecoverExecutor) TryExecute(context.Context,
	func(context.Context, contracts.RuntimeWriteTx) error) (bool, error) {
	return false, nil
}

func replaceRecoverExecutor(t *testing.T, h *recoverHarness, executor contracts.RuntimeWriteExecutor) {
	t.Helper()
	tasks, runs, executions, receipts := fakeRepositoryPorts(h.repositories)
	service, err := taskruntime.NewRecoverTaskService(taskruntime.RecoverTaskDependencies{
		Executor: executor, Tasks: tasks, Runs: runs, Executions: executions, Recovery: h.recovery,
		Receipts: receipts, Reports: &fakePendingReportWriter{}, Clock: h.repositories, Configs: h.configs,
		Checkpoints: h.checkpoints, TaskLogs: fakeTaskLogRepository{h.repositories},
		ActiveCalls: activecall.NewRegistry(), Policy: lifecycle.New(),
	})
	if err != nil {
		t.Fatal(err)
	}
	h.service = service
}

var _ contracts.RuntimeWriteExecutor = droppingRecoverExecutor{}
