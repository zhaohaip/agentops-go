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

func TestCreateTaskCommitsAtomicInitialFactsUsingDatabaseTime(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, time.August, 5, 10, 11, 12, 0, time.UTC)
	service, executor, repositories, checkpoints, _ := newCreateFixture(t, now)

	created, err := service.CreateTask(context.Background(), taskruntime.CreateTaskRequest{
		CommandID: "command-create", AgentID: "agent-default", TaskInput: "inspect deployment",
		OperatorID: "operator-1",
	})
	if err != nil {
		t.Fatalf("CreateTask() error = %v", err)
	}
	store := executor.snapshot()
	task := store.tasks[created.TaskID]
	run := store.runs[created.TaskID]
	execution := store.executions[executionKey(created.TaskID, 1)]
	if task.Status != contracts.TaskStatusPending || run.Status != contracts.RunStatusPending ||
		execution.Status != contracts.TaskExecutionStatusQueued {
		t.Fatalf("initial states = %s/%s/%s", task.Status, run.Status, execution.Status)
	}
	if task.QueuedAt == nil || !task.QueuedAt.Equal(now) || !created.QueuedAt.Equal(now) {
		t.Fatalf("queued_at = %v/%v, want database time %v", task.QueuedAt, created.QueuedAt, now)
	}
	wantDeadline := now.Add(30 * time.Minute)
	if !task.DeadlineAt.Equal(wantDeadline) || !created.DeadlineAt.Equal(wantDeadline) {
		t.Fatalf("deadline = %v/%v, want %v", task.DeadlineAt, created.DeadlineAt, wantDeadline)
	}
	if task.CurrentRunID != run.RunID || task.CurrentExecutionVersion != 1 ||
		execution.ExecutionConfigHash == "" || len(store.checkpoints) != 1 || len(store.receipts) != 1 {
		t.Fatalf("atomic Create facts incomplete: task=%+v run=%+v execution=%+v checkpoints=%d receipts=%d",
			task, run, execution, len(store.checkpoints), len(store.receipts))
	}
	checkpoint := store.checkpoints[0]
	if checkpoint.NextAction != contracts.CheckpointNextActionGeneratePlan ||
		checkpoint.ExecutionConfigHash != execution.ExecutionConfigHash || checkpoint.ExecutionVersion != 1 {
		t.Fatalf("initial checkpoint = %+v, want GENERATE_PLAN with v1 hash", checkpoint)
	}
	assertSameFakeTransaction(t, checkpoints.seenTx)
	assertSingleTaskLog(t, store.logs, "TaskCreated", created.TaskID, created.RunID, 1)
	if store.logs[0].Level != taskruntime.TaskLogLevelInfo || store.logs[0].Message != "task created" ||
		strings.Contains(store.logs[0].Message, "inspect deployment") {
		t.Fatalf("TaskCreated log contains unexpected content: %+v", store.logs[0])
	}
	assertIndependentLogTransaction(t, repositories)
}

func TestCreateTaskReceiptReplayAndConflict(t *testing.T) {
	t.Parallel()
	service, executor, _, _, configs := newCreateFixture(t, time.Now().UTC())
	request := taskruntime.CreateTaskRequest{
		CommandID: "command-replay", AgentID: "agent-default", TaskInput: "inspect deployment", OperatorID: "operator-1",
	}
	first, err := service.CreateTask(context.Background(), request)
	if err != nil {
		t.Fatalf("first CreateTask() error = %v", err)
	}
	lookupCalls := configs.calls
	second, err := service.CreateTask(context.Background(), request)
	if err != nil {
		t.Fatalf("replayed CreateTask() error = %v", err)
	}
	if first != second {
		t.Fatalf("replayed result = %+v, want %+v", second, first)
	}
	if configs.calls != lookupCalls {
		t.Fatalf("receipt replay looked up mutable config: calls %d -> %d", lookupCalls, configs.calls)
	}
	store := executor.snapshot()
	if len(store.tasks) != 1 || len(store.runs) != 1 || len(store.executions) != 1 ||
		len(store.checkpoints) != 1 || len(store.receipts) != 1 || len(store.logs) != 1 {
		t.Fatalf("replay duplicated facts: %+v", store)
	}

	request.TaskInput = "different request"
	if _, err := service.CreateTask(context.Background(), request); !errors.Is(err, taskruntime.ErrCommandConflict) {
		t.Fatalf("conflicting CreateTask() error = %v, want CommandConflict", err)
	}
	store = executor.snapshot()
	if len(store.tasks) != 1 || len(store.receipts) != 1 {
		t.Fatal("command conflict changed committed facts")
	}
}

func TestCreateTaskLogFailureDoesNotChangeCommittedResult(t *testing.T) {
	t.Parallel()
	service, executor, repositories, _, _ := newCreateFixture(t, time.Now().UTC())
	repositories.failOperation["task_log.append"] = errors.New("task log unavailable")

	created, err := service.CreateTask(context.Background(), taskruntime.CreateTaskRequest{
		CommandID: "command-log-failure", AgentID: "agent-default", TaskInput: "inspect",
		OperatorID: "operator-1",
	})
	if err != nil {
		t.Fatalf("CreateTask() error = %v, want committed success", err)
	}
	store := executor.snapshot()
	if created.TaskID == "" || store.tasks[created.TaskID].Status != contracts.TaskStatusPending ||
		len(store.receipts) != 1 || len(store.logs) != 0 {
		t.Fatalf("TaskLog failure changed Create result: created=%+v store=%+v", created, store)
	}
	if executor.commits != 1 || executor.rollbacks != 1 {
		t.Fatalf("transactions = commits:%d rollbacks:%d, want core commit and log rollback",
			executor.commits, executor.rollbacks)
	}
}

func TestCreateTaskDeterministicAgentRejectionIsReceipted(t *testing.T) {
	t.Parallel()
	service, executor, _, _, configs := newCreateFixture(t, time.Now().UTC())
	agent := configs.agents["agent-default"]
	agent.ExecutionConfig.Agent.Enabled = false
	configs.agents["agent-default"] = agent
	request := taskruntime.CreateTaskRequest{
		CommandID: "command-disabled", AgentID: "agent-default", TaskInput: "inspect", OperatorID: "operator-1",
	}
	if _, err := service.CreateTask(context.Background(), request); !errors.Is(err, taskruntime.ErrAgentUnavailable) {
		t.Fatalf("disabled CreateTask() error = %v, want AgentUnavailable", err)
	}
	store := executor.snapshot()
	if len(store.receipts) != 1 || len(store.tasks) != 0 || len(store.checkpoints) != 0 {
		t.Fatalf("disabled Agent facts = receipts=%d tasks=%d checkpoints=%d", len(store.receipts), len(store.tasks), len(store.checkpoints))
	}

	agent.ExecutionConfig.Agent.Enabled = true
	configs.agents["agent-default"] = agent
	lookupCalls := configs.calls
	if _, err := service.CreateTask(context.Background(), request); !errors.Is(err, taskruntime.ErrAgentUnavailable) {
		t.Fatalf("replayed disabled result error = %v, want AgentUnavailable", err)
	}
	if configs.calls != lookupCalls || len(executor.snapshot().tasks) != 0 {
		t.Fatal("failure receipt replay re-evaluated changed Agent config")
	}
}

func TestCreateTaskInvalidSyntaxDoesNotSaveReceipt(t *testing.T) {
	t.Parallel()
	service, executor, _, _, _ := newCreateFixture(t, time.Now().UTC())
	if _, err := service.CreateTask(context.Background(), taskruntime.CreateTaskRequest{
		CommandID: "command-invalid", AgentID: "agent-default", TaskInput: " ", OperatorID: "operator-1",
	}); !errors.Is(err, taskruntime.ErrInvalidArgument) {
		t.Fatalf("CreateTask(invalid) error = %v, want InvalidArgument", err)
	}
	store := executor.snapshot()
	if len(store.receipts) != 0 || len(store.tasks) != 0 {
		t.Fatal("unnormalizable Create request persisted facts")
	}
}

func TestCreateTaskFailureRollsBackAllFacts(t *testing.T) {
	t.Parallel()
	service, executor, _, checkpoints, _ := newCreateFixture(t, time.Now().UTC())
	checkpoints.failSave = errors.New("checkpoint unavailable")
	if _, err := service.CreateTask(context.Background(), taskruntime.CreateTaskRequest{
		CommandID: "command-rollback", AgentID: "agent-default", TaskInput: "inspect", OperatorID: "operator-1",
	}); err == nil {
		t.Fatal("CreateTask() error = nil, want checkpoint failure")
	}
	store := executor.snapshot()
	if len(store.tasks) != 0 || len(store.runs) != 0 || len(store.executions) != 0 ||
		len(store.checkpoints) != 0 || len(store.receipts) != 0 || len(store.logs) != 0 {
		t.Fatalf("failed Create left partial facts: %+v", store)
	}
	if executor.rollbacks != 1 {
		t.Fatalf("rollbacks = %d, want 1", executor.rollbacks)
	}
}

func newCreateFixture(
	t *testing.T,
	now time.Time,
) (*taskruntime.CreateTaskService, *fakeExecutor, *fakeRepositories, *fakeCheckpointPort, *fakeAgentConfigSource) {
	t.Helper()
	executor := newFakeExecutor()
	repositories := newFakeRepositories(executor, now)
	tasks, runs, executions, receipts := fakeRepositoryPorts(repositories)
	checkpoint := &fakeCheckpointPort{overrides: make(map[contracts.TaskID]taskruntime.ClaimCheckpointResult)}
	agent := loadedAgentConfig(t)
	configs := &fakeAgentConfigSource{agents: map[contracts.AgentID]taskruntime.AgentRuntimeConfig{"agent-default": agent}}
	service, err := taskruntime.NewCreateTaskService(taskruntime.CreateTaskDependencies{
		Executor: executor, Tasks: tasks, Runs: runs, Executions: executions, Receipts: receipts,
		TaskLogs: fakeTaskLogRepository{repositories}, Clock: repositories,
		Configs: configs, Checkpoints: checkpoint, Policy: lifecycle.New(),
	})
	if err != nil {
		t.Fatalf("NewCreateTaskService() error = %v", err)
	}
	return service, executor, repositories, checkpoint, configs
}

func assertSameFakeTransaction(t *testing.T, groups ...[]contracts.RuntimeWriteTx) {
	t.Helper()
	var expected contracts.RuntimeWriteTx
	for _, group := range groups {
		for _, token := range group {
			if expected == nil {
				expected = token
				continue
			}
			if token != expected {
				t.Fatalf("operation used a different RuntimeWriteTx: %p != %p", token, expected)
			}
		}
	}
	if expected == nil {
		t.Fatal("no RuntimeWriteTx was observed")
	}
}

func assertSingleTaskLog(
	t *testing.T,
	logs []taskruntime.TaskLog,
	event string,
	taskID contracts.TaskID,
	runID contracts.RunID,
	version contracts.ExecutionVersion,
) {
	t.Helper()
	if len(logs) != 1 {
		t.Fatalf("TaskLogs = %d, want one %s event", len(logs), event)
	}
	log := logs[0]
	if log.Event != event || log.TaskID != taskID || log.RunID != runID ||
		log.ExecutionVersion == nil || *log.ExecutionVersion != version || log.Operator != "System" ||
		log.Message == "" || log.CreatedAt.IsZero() {
		t.Fatalf("TaskLog = %+v, want safe %s identity fields", log, event)
	}
}

func assertIndependentLogTransaction(t *testing.T, repositories *fakeRepositories) {
	t.Helper()
	repositories.mu.Lock()
	defer repositories.mu.Unlock()
	core := repositories.operationTx["task.insert"]
	if len(core) == 0 {
		core = repositories.operationTx["execution.update"]
	}
	logs := repositories.operationTx["task_log.append"]
	if len(core) != 1 || len(logs) != 1 || core[0] == logs[0] {
		t.Fatalf("core/log transactions = %v/%v, want separate RuntimeWriteTx tokens", core, logs)
	}
	if !repositories.logDeadline {
		t.Fatal("TaskLog write context has no finite deadline")
	}
}
