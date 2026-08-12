package postgres_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	postgresruntime "github.com/zhaohaip/agentops-go/internal/adapter/postgres/runtime"
	postgrestaskruntime "github.com/zhaohaip/agentops-go/internal/adapter/postgres/taskruntime"
	"github.com/zhaohaip/agentops-go/internal/contracts"
	domain "github.com/zhaohaip/agentops-go/internal/taskruntime"
	taskruntimemigrations "github.com/zhaohaip/agentops-go/migrations/taskruntime"
	"github.com/zhaohaip/agentops-go/test/integration/postgres/postgrestest"
)

func TestTaskRuntimeRepositoryContract(t *testing.T) {
	postgrestest.RunRepositoryContract(t, postgrestest.RepositoryContract{
		Name:       "Task Runtime",
		Migrations: taskruntimemigrations.Migrations(),
		Cases: []postgrestest.RepositoryCase{
			{Name: "atomic insert commit and read", Run: testTaskRuntimeRepositoryCommit},
			{Name: "Task list filter and order", Run: testTaskRuntimeRepositoryList},
			{Name: "Get uses one snapshot during Claim", Run: testTaskQueryGetSnapshotDuringClaim},
			{Name: "List uses one snapshot during Cancel", Run: testTaskQueryListSnapshotDuringCancel},
			{Name: "transaction rollback", Run: testTaskRuntimeRepositoryRollback},
			{Name: "conditional updates and locks", Run: testTaskRuntimeRepositoryConditionalUpdates},
			{Name: "current version update guards", Run: testTaskRuntimeRepositoryCurrentVersionGuards},
			{Name: "observed config hash is write once", Run: testTaskRuntimeRepositoryObservedConfigHash},
			{Name: "strict FIFO candidates", Run: testTaskRuntimeRepositoryFIFO},
			{Name: "FIFO candidate locking", Run: testTaskRuntimeRepositoryFIFOConcurrency},
			{Name: "current execution pointer", Run: testTaskRuntimeRepositoryCurrentExecutionPointer},
			{Name: "receipt uniqueness and SQL errors", Run: testTaskRuntimeRepositoryReceiptUniqueness},
			{Name: "database clock and read write boundary", Run: testTaskRuntimeRepositoryClockAndChannels},
			{Name: "TaskLog append", Run: testTaskRuntimeRepositoryTaskLogAppend},
		},
	})
}

func testTaskQueryGetSnapshotDuringClaim(t *testing.T, environment *postgrestest.RepositoryEnvironment) {
	repositories := postgrestaskruntime.New(environment.Runtime.ReadPool())
	now := repositoryDatabaseNow(t, environment, repositories.Clock)
	graph := repositoryGraph("query-claim", now, now)
	insertRepositoryGraph(t, environment, repositories, graph)

	barrier := newQuerySnapshotBarrier()
	t.Cleanup(barrier.release)
	service := newRepositoryTaskQueryService(t, environment, barrier)
	result := make(chan taskViewQueryResult, 1)
	go func() {
		view, err := service.GetTask(context.Background(), graph.task.TaskID)
		result <- taskViewQueryResult{view: view, err: err}
	}()
	barrier.awaitEntered(t)

	workerID := contracts.WorkerID("worker-query-claim")
	if err := environment.Runtime.WriteExecutor().Execute(context.Background(), func(ctx context.Context, tx contracts.RuntimeWriteTx) error {
		taskUpdated, err := repositories.Tasks.Update(ctx, tx, domain.TaskUpdate{
			TaskID: graph.task.TaskID, ExpectedStatus: contracts.TaskStatusPending,
			ExpectedCurrentExecutionVersion: 1, Status: contracts.TaskStatusRunning,
			CurrentExecutionVersion: 1, StartedAt: &now,
		})
		if err != nil || !taskUpdated {
			return fmt.Errorf("claim Task update = %v, %w", taskUpdated, err)
		}
		runUpdated, err := repositories.Runs.Update(ctx, tx, domain.RunUpdate{
			TaskID: graph.task.TaskID, RunID: graph.run.RunID, ExecutionVersion: 1,
			ExpectedStatus: contracts.RunStatusPending, Status: contracts.RunStatusRunning,
			Context: json.RawMessage(`{}`), StartedAt: &now,
		})
		if err != nil || !runUpdated {
			return fmt.Errorf("claim Run update = %v, %w", runUpdated, err)
		}
		executionUpdated, err := repositories.Executions.Update(ctx, tx, domain.TaskExecutionUpdate{
			TaskID: graph.task.TaskID, ExecutionVersion: 1,
			ExpectedStatus: contracts.TaskExecutionStatusQueued,
			Status:         contracts.TaskExecutionStatusRunning, WorkerID: &workerID, StartedAt: &now,
		})
		if err != nil || !executionUpdated {
			return fmt.Errorf("claim Execution update = %v, %w", executionUpdated, err)
		}
		return nil
	}); err != nil {
		t.Fatalf("commit synchronized Claim transition: %v", err)
	}
	barrier.release()

	before := awaitTaskQueryResult(t, result)
	if before.err != nil {
		t.Fatalf("GetTask during Claim: %v", before.err)
	}
	if before.view.Task.Status != contracts.TaskStatusPending ||
		before.view.Run.Status != contracts.RunStatusPending ||
		before.view.Execution.Status != contracts.TaskExecutionStatusQueued ||
		before.view.Task.QueuedAt == nil || before.view.Execution.WorkerID != nil {
		t.Fatalf("GetTask mixed pre/post Claim facts: %+v", before.view)
	}
	if observed := barrier.awaitObservedStatus(t); observed != contracts.TaskStatusPending {
		t.Fatalf("evidence snapshot Task status = %s, want pre-Claim Pending", observed)
	}

	afterService := newRepositoryTaskQueryService(t, environment, databaseNowSnapshotEvidence{})
	after, err := afterService.GetTask(context.Background(), graph.task.TaskID)
	if err != nil {
		t.Fatalf("GetTask after Claim: %v", err)
	}
	if after.Task.Status != contracts.TaskStatusRunning || after.Run.Status != contracts.RunStatusRunning ||
		after.Execution.Status != contracts.TaskExecutionStatusRunning || after.Task.QueuedAt != nil ||
		after.Execution.WorkerID == nil || *after.Execution.WorkerID != workerID {
		t.Fatalf("GetTask post-Claim facts = %+v", after)
	}
}

func testTaskQueryListSnapshotDuringCancel(t *testing.T, environment *postgrestest.RepositoryEnvironment) {
	repositories := postgrestaskruntime.New(environment.Runtime.ReadPool())
	now := repositoryDatabaseNow(t, environment, repositories.Clock)
	graph := repositoryGraph("query-cancel", now, now)
	workerID := contracts.WorkerID("worker-query-cancel")
	graph.task.Status = contracts.TaskStatusRunning
	graph.task.QueuedAt = nil
	graph.task.StartedAt = &now
	graph.run.Status = contracts.RunStatusRunning
	graph.run.StartedAt = &now
	graph.execution.Status = contracts.TaskExecutionStatusRunning
	graph.execution.WorkerID = &workerID
	graph.execution.StartedAt = &now
	insertRepositoryGraph(t, environment, repositories, graph)

	barrier := newQuerySnapshotBarrier()
	t.Cleanup(barrier.release)
	service := newRepositoryTaskQueryService(t, environment, barrier)
	result := make(chan taskViewListResult, 1)
	go func() {
		views, err := service.ListTasks(context.Background(), nil)
		result <- taskViewListResult{views: views, err: err}
	}()
	barrier.awaitEntered(t)

	endedAt := now.Add(time.Second)
	errorCode := contracts.ErrorCodeTaskCancelled
	reason := contracts.TerminationReasonCancelled
	if err := environment.Runtime.WriteExecutor().Execute(context.Background(), func(ctx context.Context, tx contracts.RuntimeWriteTx) error {
		taskUpdated, err := repositories.Tasks.Update(ctx, tx, domain.TaskUpdate{
			TaskID: graph.task.TaskID, ExpectedStatus: contracts.TaskStatusRunning,
			ExpectedCurrentExecutionVersion: 1, Status: contracts.TaskStatusCancelled,
			CurrentExecutionVersion: 1, ErrorCode: &errorCode, StartedAt: &now, EndedAt: &endedAt,
		})
		if err != nil || !taskUpdated {
			return fmt.Errorf("cancel Task update = %v, %w", taskUpdated, err)
		}
		runUpdated, err := repositories.Runs.Update(ctx, tx, domain.RunUpdate{
			TaskID: graph.task.TaskID, RunID: graph.run.RunID, ExecutionVersion: 1,
			ExpectedStatus: contracts.RunStatusRunning, Status: contracts.RunStatusFailed,
			Context: json.RawMessage(`{}`), ErrorCode: &errorCode, StartedAt: &now, EndedAt: &endedAt,
		})
		if err != nil || !runUpdated {
			return fmt.Errorf("cancel Run update = %v, %w", runUpdated, err)
		}
		executionUpdated, err := repositories.Executions.Update(ctx, tx, domain.TaskExecutionUpdate{
			TaskID: graph.task.TaskID, ExecutionVersion: 1,
			ExpectedStatus: contracts.TaskExecutionStatusRunning, ExpectedWorkerID: &workerID,
			Status: contracts.TaskExecutionStatusFailed, WorkerID: &workerID, ErrorCode: &errorCode,
			TerminationReason: &reason, StartedAt: &now, EndedAt: &endedAt,
		})
		if err != nil || !executionUpdated {
			return fmt.Errorf("cancel Execution update = %v, %w", executionUpdated, err)
		}
		return nil
	}); err != nil {
		t.Fatalf("commit synchronized Cancel transition: %v", err)
	}
	barrier.release()

	before := awaitTaskListResult(t, result)
	if before.err != nil || len(before.views) != 1 {
		t.Fatalf("ListTasks during Cancel = %+v, %v", before.views, before.err)
	}
	view := before.views[0]
	if view.Task.Status != contracts.TaskStatusRunning || view.Run.Status != contracts.RunStatusRunning ||
		view.Execution.Status != contracts.TaskExecutionStatusRunning || view.Task.EndedAt != nil ||
		view.Execution.WorkerID == nil || *view.Execution.WorkerID != workerID {
		t.Fatalf("ListTasks mixed pre/post Cancel facts: %+v", view)
	}
	if observed := barrier.awaitObservedStatus(t); observed != contracts.TaskStatusRunning {
		t.Fatalf("evidence snapshot Task status = %s, want pre-Cancel Running", observed)
	}

	afterService := newRepositoryTaskQueryService(t, environment, databaseNowSnapshotEvidence{})
	after, err := afterService.ListTasks(context.Background(), nil)
	if err != nil || len(after) != 1 {
		t.Fatalf("ListTasks after Cancel = %+v, %v", after, err)
	}
	view = after[0]
	if view.Task.Status != contracts.TaskStatusCancelled || view.Run.Status != contracts.RunStatusFailed ||
		view.Execution.Status != contracts.TaskExecutionStatusFailed || view.Task.EndedAt == nil ||
		view.Execution.TerminationReason == nil || *view.Execution.TerminationReason != reason ||
		view.Execution.WorkerID == nil || *view.Execution.WorkerID != workerID {
		t.Fatalf("ListTasks post-Cancel facts = %+v", view)
	}
}

func testTaskRuntimeRepositoryList(t *testing.T, environment *postgrestest.RepositoryEnvironment) {
	repositories := postgrestaskruntime.New(environment.Runtime.ReadPool())
	now := repositoryDatabaseNow(t, environment, repositories.Clock)
	older := repositoryGraph("list-older", now.Add(-time.Second), now.Add(-time.Second))
	newer := repositoryGraph("list-newer", now, now)
	insertRepositoryGraph(t, environment, repositories, older)
	insertRepositoryGraph(t, environment, repositories, newer)

	tasks, err := repositories.Tasks.List(context.Background(), nil)
	if err != nil {
		t.Fatalf("list all Tasks: %v", err)
	}
	if len(tasks) != 2 || tasks[0].TaskID != newer.task.TaskID || tasks[1].TaskID != older.task.TaskID {
		t.Fatalf("listed Tasks = %+v, want newest first", tasks)
	}

	pending := contracts.TaskStatusPending
	tasks, err = repositories.Tasks.List(context.Background(), &pending)
	if err != nil || len(tasks) != 2 {
		t.Fatalf("list Pending Tasks = %+v, %v", tasks, err)
	}
	failed := contracts.TaskStatusFailed
	tasks, err = repositories.Tasks.List(context.Background(), &failed)
	if err != nil || len(tasks) != 0 {
		t.Fatalf("list Failed Tasks = %+v, %v", tasks, err)
	}
}

func testTaskRuntimeRepositoryCommit(t *testing.T, environment *postgrestest.RepositoryEnvironment) {
	repositories := postgrestaskruntime.New(environment.Runtime.ReadPool())
	now := repositoryDatabaseNow(t, environment, repositories.Clock)
	graph := repositoryGraph("commit", now, now)

	if err := environment.Runtime.WriteExecutor().Execute(context.Background(), func(ctx context.Context, tx contracts.RuntimeWriteTx) error {
		if err := repositories.Tasks.Insert(ctx, tx, graph.task); err != nil {
			return err
		}
		if err := repositories.Runs.Insert(ctx, tx, graph.run); err != nil {
			return err
		}
		if err := repositories.Executions.Insert(ctx, tx, graph.execution); err != nil {
			return err
		}
		return repositories.Receipts.Insert(ctx, tx, graph.receipt)
	}); err != nil {
		t.Fatalf("commit Task Runtime graph: %v", err)
	}

	task, err := repositories.Tasks.Find(context.Background(), graph.task.TaskID)
	if err != nil {
		t.Fatalf("find committed Task: %v", err)
	}
	run, err := repositories.Runs.FindByTask(context.Background(), graph.task.TaskID)
	if err != nil {
		t.Fatalf("find committed Run: %v", err)
	}
	execution, err := repositories.Executions.FindByTaskVersion(
		context.Background(),
		graph.task.TaskID,
		graph.execution.ExecutionVersion,
	)
	if err != nil {
		t.Fatalf("find committed TaskExecution: %v", err)
	}
	receipt, err := repositories.Receipts.Find(context.Background(), graph.receipt.CommandID)
	if err != nil {
		t.Fatalf("find committed Receipt: %v", err)
	}

	if task.TaskID != graph.task.TaskID || task.CurrentExecutionVersion != 1 || task.QueuedAt == nil {
		t.Fatalf("committed Task = %+v", task)
	}
	if run.RunID != graph.run.RunID || string(run.Context) != `{}` {
		t.Fatalf("committed Run = %+v", run)
	}
	if execution.TaskExecutionID != graph.execution.TaskExecutionID || execution.ExecutionVersion != 1 {
		t.Fatalf("committed TaskExecution = %+v", execution)
	}
	if receipt.CommandID != graph.receipt.CommandID || !json.Valid(receipt.Response) {
		t.Fatalf("committed Receipt = %+v", receipt)
	}

	if _, err := repositories.Tasks.Find(context.Background(), "missing-task"); !errors.Is(err, domain.ErrRepositoryNotFound) {
		t.Fatalf("missing Task error = %v, want ErrRepositoryNotFound", err)
	}
}

func testTaskRuntimeRepositoryRollback(t *testing.T, environment *postgrestest.RepositoryEnvironment) {
	repositories := postgrestaskruntime.New(environment.Runtime.ReadPool())
	now := repositoryDatabaseNow(t, environment, repositories.Clock)
	graph := repositoryGraph("rollback", now, now)
	wantErr := errors.New("force Task Runtime transaction rollback")

	err := environment.Runtime.WriteExecutor().Execute(context.Background(), func(ctx context.Context, tx contracts.RuntimeWriteTx) error {
		if err := repositories.Tasks.Insert(ctx, tx, graph.task); err != nil {
			return err
		}
		if err := repositories.Runs.Insert(ctx, tx, graph.run); err != nil {
			return err
		}
		if err := repositories.Executions.Insert(ctx, tx, graph.execution); err != nil {
			return err
		}
		return wantErr
	})
	if !errors.Is(err, wantErr) {
		t.Fatalf("rollback transaction error = %v, want %v", err, wantErr)
	}
	if _, err := repositories.Tasks.Find(context.Background(), graph.task.TaskID); !errors.Is(err, domain.ErrRepositoryNotFound) {
		t.Fatalf("Task after rollback error = %v, want not found", err)
	}
}

func testTaskRuntimeRepositoryConditionalUpdates(t *testing.T, environment *postgrestest.RepositoryEnvironment) {
	repositories := postgrestaskruntime.New(environment.Runtime.ReadPool())
	now := repositoryDatabaseNow(t, environment, repositories.Clock)
	graph := repositoryGraph("conditional", now, now)
	insertRepositoryGraph(t, environment, repositories, graph)
	workerID := contracts.WorkerID("worker-conditional")

	if err := environment.Runtime.WriteExecutor().Execute(context.Background(), func(ctx context.Context, tx contracts.RuntimeWriteTx) error {
		lockedTask, err := repositories.Tasks.Lock(ctx, tx, graph.task.TaskID)
		if err != nil {
			return err
		}
		lockedRun, err := repositories.Runs.LockByTask(ctx, tx, graph.task.TaskID)
		if err != nil {
			return err
		}
		lockedExecution, err := repositories.Executions.LockByTaskVersion(ctx, tx, graph.task.TaskID, 1)
		if err != nil {
			return err
		}
		if lockedTask.Status != contracts.TaskStatusPending || lockedRun.Status != contracts.RunStatusPending ||
			lockedExecution.Status != contracts.TaskExecutionStatusQueued {
			return errors.New("locked Task Runtime graph has unexpected state")
		}

		stale, err := repositories.Tasks.Update(ctx, tx, domain.TaskUpdate{
			TaskID:                          graph.task.TaskID,
			ExpectedStatus:                  contracts.TaskStatusRunning,
			ExpectedCurrentExecutionVersion: 1,
			Status:                          contracts.TaskStatusRunning,
			CurrentExecutionVersion:         1,
			StartedAt:                       &now,
		})
		if err != nil {
			return err
		}
		if stale {
			return errors.New("stale Task update unexpectedly matched")
		}

		taskUpdated, err := repositories.Tasks.Update(ctx, tx, domain.TaskUpdate{
			TaskID:                          graph.task.TaskID,
			ExpectedStatus:                  contracts.TaskStatusPending,
			ExpectedCurrentExecutionVersion: 1,
			Status:                          contracts.TaskStatusRunning,
			CurrentExecutionVersion:         1,
			StartedAt:                       &now,
		})
		if err != nil || !taskUpdated {
			return errors.Join(err, errors.New("Task conditional update did not affect one row"))
		}
		runUpdated, err := repositories.Runs.Update(ctx, tx, domain.RunUpdate{
			TaskID:           graph.task.TaskID,
			RunID:            graph.run.RunID,
			ExecutionVersion: 1,
			ExpectedStatus:   contracts.RunStatusPending,
			Status:           contracts.RunStatusRunning,
			Context:          json.RawMessage(`{}`),
			StartedAt:        &now,
		})
		if err != nil || !runUpdated {
			return errors.Join(err, errors.New("Run conditional update did not affect one row"))
		}
		executionUpdated, err := repositories.Executions.Update(ctx, tx, domain.TaskExecutionUpdate{
			TaskID:           graph.task.TaskID,
			ExecutionVersion: 1,
			ExpectedStatus:   contracts.TaskExecutionStatusQueued,
			Status:           contracts.TaskExecutionStatusRunning,
			WorkerID:         &workerID,
			StartedAt:        &now,
		})
		if err != nil || !executionUpdated {
			return errors.Join(err, errors.New("TaskExecution conditional update did not affect one row"))
		}
		return nil
	}); err != nil {
		t.Fatalf("conditional update transaction: %v", err)
	}

	task, err := repositories.Tasks.Find(context.Background(), graph.task.TaskID)
	if err != nil || task.Status != contracts.TaskStatusRunning || task.QueuedAt != nil {
		t.Fatalf("Task after conditional update = (%+v, %v)", task, err)
	}
	execution, err := repositories.Executions.FindByTaskVersion(context.Background(), graph.task.TaskID, 1)
	if err != nil || execution.Status != contracts.TaskExecutionStatusRunning || execution.WorkerID == nil || *execution.WorkerID != workerID {
		t.Fatalf("TaskExecution after conditional update = (%+v, %v)", execution, err)
	}
}

func testTaskRuntimeRepositoryCurrentVersionGuards(t *testing.T, environment *postgrestest.RepositoryEnvironment) {
	repositories := postgrestaskruntime.New(environment.Runtime.ReadPool())
	now := repositoryDatabaseNow(t, environment, repositories.Clock)
	graph := repositoryGraph("version-guard", now, now)
	insertRepositoryGraph(t, environment, repositories, graph)
	workerV1 := contracts.WorkerID("worker-v1")

	// v1 仍为当前版本时，Task、Run 和 TaskExecution 可以在同一个 RuntimeWriteTx 中推进。
	if err := environment.Runtime.WriteExecutor().Execute(context.Background(), func(ctx context.Context, tx contracts.RuntimeWriteTx) error {
		taskUpdated, err := repositories.Tasks.Update(ctx, tx, domain.TaskUpdate{
			TaskID:                          graph.task.TaskID,
			ExpectedStatus:                  contracts.TaskStatusPending,
			ExpectedCurrentExecutionVersion: 1,
			Status:                          contracts.TaskStatusRunning,
			CurrentExecutionVersion:         1,
			StartedAt:                       &now,
		})
		if err != nil || !taskUpdated {
			return errors.Join(err, errors.New("current v1 Task update did not affect one row"))
		}
		runUpdated, err := repositories.Runs.Update(ctx, tx, domain.RunUpdate{
			TaskID:           graph.task.TaskID,
			RunID:            graph.run.RunID,
			ExecutionVersion: 1,
			ExpectedStatus:   contracts.RunStatusPending,
			Status:           contracts.RunStatusRunning,
			Context:          json.RawMessage(`{"version":1}`),
			StartedAt:        &now,
		})
		if err != nil || !runUpdated {
			return errors.Join(err, errors.New("current v1 Run update did not affect one row"))
		}
		executionUpdated, err := repositories.Executions.Update(ctx, tx, domain.TaskExecutionUpdate{
			TaskID:           graph.task.TaskID,
			ExecutionVersion: 1,
			ExpectedStatus:   contracts.TaskExecutionStatusQueued,
			Status:           contracts.TaskExecutionStatusRunning,
			WorkerID:         &workerV1,
			StartedAt:        &now,
		})
		if err != nil || !executionUpdated {
			return errors.Join(err, errors.New("current v1 TaskExecution update did not affect one row"))
		}
		return nil
	}); err != nil {
		t.Fatalf("advance current v1 graph: %v", err)
	}

	interruptedAt := now.Add(time.Second)
	workerInterrupted := contracts.ErrorCodeWorkerInterrupted
	if err := environment.Runtime.WriteExecutor().Execute(context.Background(), func(ctx context.Context, tx contracts.RuntimeWriteTx) error {
		updated, err := repositories.Executions.Update(ctx, tx, domain.TaskExecutionUpdate{
			TaskID:           graph.task.TaskID,
			ExecutionVersion: 1,
			ExpectedStatus:   contracts.TaskExecutionStatusRunning,
			ExpectedWorkerID: &workerV1,
			Status:           contracts.TaskExecutionStatusInterrupted,
			WorkerID:         &workerV1,
			ErrorCode:        &workerInterrupted,
			StartedAt:        &now,
			EndedAt:          &interruptedAt,
		})
		if err != nil || !updated {
			return errors.Join(err, errors.New("current v1 interrupt did not affect one row"))
		}
		return nil
	}); err != nil {
		t.Fatalf("interrupt current v1: %v", err)
	}

	queuedV2 := now.Add(2 * time.Second)
	second := graph.execution
	second.TaskExecutionID = "execution-version-guard-v2"
	second.ExecutionVersion = 2
	second.CreatedAt = queuedV2
	if err := environment.Runtime.WriteExecutor().Execute(context.Background(), func(ctx context.Context, tx contracts.RuntimeWriteTx) error {
		if err := repositories.Executions.Insert(ctx, tx, second); err != nil {
			return err
		}
		updated, err := repositories.Tasks.Update(ctx, tx, domain.TaskUpdate{
			TaskID:                          graph.task.TaskID,
			ExpectedStatus:                  contracts.TaskStatusRunning,
			ExpectedCurrentExecutionVersion: 1,
			Status:                          contracts.TaskStatusRunning,
			CurrentExecutionVersion:         2,
			QueuedAt:                        &queuedV2,
			StartedAt:                       &now,
		})
		if err != nil || !updated {
			return errors.Join(err, errors.New("advance Task current pointer to v2 did not affect one row"))
		}
		return nil
	}); err != nil {
		t.Fatalf("create v2 and advance current pointer: %v", err)
	}

	var staleExecutionUpdated, staleRunUpdated bool
	if err := environment.Runtime.WriteExecutor().Execute(context.Background(), func(ctx context.Context, tx contracts.RuntimeWriteTx) error {
		var err error
		staleExecutionUpdated, err = repositories.Executions.Update(ctx, tx, domain.TaskExecutionUpdate{
			TaskID:           graph.task.TaskID,
			ExecutionVersion: 1,
			ExpectedStatus:   contracts.TaskExecutionStatusInterrupted,
			ExpectedWorkerID: &workerV1,
			Status:           contracts.TaskExecutionStatusFailed,
			WorkerID:         &workerV1,
			ErrorCode:        &workerInterrupted,
			StartedAt:        &now,
			EndedAt:          &interruptedAt,
		})
		if err != nil {
			return err
		}
		staleRunUpdated, err = repositories.Runs.Update(ctx, tx, domain.RunUpdate{
			TaskID:           graph.task.TaskID,
			RunID:            graph.run.RunID,
			ExecutionVersion: 1,
			ExpectedStatus:   contracts.RunStatusRunning,
			Status:           contracts.RunStatusRunning,
			Context:          json.RawMessage(`{"stale":true}`),
			StartedAt:        &now,
		})
		return err
	}); err != nil {
		t.Fatalf("execute stale v1 updates: %v", err)
	}
	if staleExecutionUpdated || staleRunUpdated {
		t.Fatalf("stale v1 update results = (execution:%t, run:%t), want both false", staleExecutionUpdated, staleRunUpdated)
	}

	assertVersionGuardState(t, repositories, graph.task.TaskID, graph.run.RunID, 2, `{"version": 1}`, contracts.TaskExecutionStatusInterrupted)

	wrongWorker := contracts.WorkerID("wrong-worker")
	var wrongStatusUpdated, wrongWorkerUpdated, wrongRunStatusUpdated bool
	if err := environment.Runtime.WriteExecutor().Execute(context.Background(), func(ctx context.Context, tx contracts.RuntimeWriteTx) error {
		var err error
		wrongStatusUpdated, err = repositories.Executions.Update(ctx, tx, domain.TaskExecutionUpdate{
			TaskID:           graph.task.TaskID,
			ExecutionVersion: 2,
			ExpectedStatus:   contracts.TaskExecutionStatusRunning,
			Status:           contracts.TaskExecutionStatusRunning,
			WorkerID:         &wrongWorker,
			StartedAt:        &queuedV2,
		})
		if err != nil {
			return err
		}
		wrongWorkerUpdated, err = repositories.Executions.Update(ctx, tx, domain.TaskExecutionUpdate{
			TaskID:           graph.task.TaskID,
			ExecutionVersion: 2,
			ExpectedStatus:   contracts.TaskExecutionStatusQueued,
			ExpectedWorkerID: &wrongWorker,
			Status:           contracts.TaskExecutionStatusRunning,
			WorkerID:         &wrongWorker,
			StartedAt:        &queuedV2,
		})
		if err != nil {
			return err
		}
		wrongRunStatusUpdated, err = repositories.Runs.Update(ctx, tx, domain.RunUpdate{
			TaskID:           graph.task.TaskID,
			RunID:            graph.run.RunID,
			ExecutionVersion: 2,
			ExpectedStatus:   contracts.RunStatusPending,
			Status:           contracts.RunStatusRunning,
			Context:          json.RawMessage(`{"invalid":true}`),
			StartedAt:        &now,
		})
		return err
	}); err != nil {
		t.Fatalf("execute current-version condition mismatches: %v", err)
	}
	if wrongStatusUpdated || wrongWorkerUpdated || wrongRunStatusUpdated {
		t.Fatalf(
			"condition mismatch results = (status:%t, worker:%t, run:%t), want all false",
			wrongStatusUpdated,
			wrongWorkerUpdated,
			wrongRunStatusUpdated,
		)
	}

	workerV2 := contracts.WorkerID("worker-v2")
	if err := environment.Runtime.WriteExecutor().Execute(context.Background(), func(ctx context.Context, tx contracts.RuntimeWriteTx) error {
		taskUpdated, err := repositories.Tasks.Update(ctx, tx, domain.TaskUpdate{
			TaskID:                          graph.task.TaskID,
			ExpectedStatus:                  contracts.TaskStatusRunning,
			ExpectedCurrentExecutionVersion: 2,
			Status:                          contracts.TaskStatusRunning,
			CurrentExecutionVersion:         2,
			StartedAt:                       &now,
		})
		if err != nil || !taskUpdated {
			return errors.Join(err, errors.New("current v2 Task update did not affect one row"))
		}
		executionUpdated, err := repositories.Executions.Update(ctx, tx, domain.TaskExecutionUpdate{
			TaskID:           graph.task.TaskID,
			ExecutionVersion: 2,
			ExpectedStatus:   contracts.TaskExecutionStatusQueued,
			Status:           contracts.TaskExecutionStatusRunning,
			WorkerID:         &workerV2,
			StartedAt:        &queuedV2,
		})
		if err != nil || !executionUpdated {
			return errors.Join(err, errors.New("current v2 TaskExecution update did not affect one row"))
		}
		runUpdated, err := repositories.Runs.Update(ctx, tx, domain.RunUpdate{
			TaskID:           graph.task.TaskID,
			RunID:            graph.run.RunID,
			ExecutionVersion: 2,
			ExpectedStatus:   contracts.RunStatusRunning,
			Status:           contracts.RunStatusRunning,
			Context:          json.RawMessage(`{"version":2}`),
			StartedAt:        &now,
		})
		if err != nil || !runUpdated {
			return errors.Join(err, errors.New("current v2 Run update did not affect one row"))
		}
		return nil
	}); err != nil {
		t.Fatalf("advance current v2 graph in one RuntimeWriteTx: %v", err)
	}

	assertVersionGuardState(t, repositories, graph.task.TaskID, graph.run.RunID, 2, `{"version": 2}`, contracts.TaskExecutionStatusInterrupted)
	v2, err := repositories.Executions.FindByTaskVersion(context.Background(), graph.task.TaskID, 2)
	if err != nil {
		t.Fatalf("find current v2 after legal update: %v", err)
	}
	if v2.Status != contracts.TaskExecutionStatusRunning || v2.WorkerID == nil || *v2.WorkerID != workerV2 {
		t.Fatalf("current v2 after legal update = %+v", v2)
	}
}

func assertVersionGuardState(
	t *testing.T,
	repositories *postgrestaskruntime.Repositories,
	taskID contracts.TaskID,
	runID contracts.RunID,
	wantCurrentVersion contracts.ExecutionVersion,
	wantRunContext string,
	wantV1Status contracts.TaskExecutionStatus,
) {
	t.Helper()
	task, err := repositories.Tasks.Find(context.Background(), taskID)
	if err != nil {
		t.Fatalf("find Task after version Guard: %v", err)
	}
	if task.CurrentRunID != runID || task.CurrentExecutionVersion != wantCurrentVersion {
		t.Fatalf("Task pointers after version Guard = (%s, %d), want (%s, %d)", task.CurrentRunID, task.CurrentExecutionVersion, runID, wantCurrentVersion)
	}
	run, err := repositories.Runs.FindByTask(context.Background(), taskID)
	if err != nil {
		t.Fatalf("find Run after version Guard: %v", err)
	}
	if string(run.Context) != wantRunContext {
		t.Fatalf("Run context after version Guard = %s, want %s", run.Context, wantRunContext)
	}
	v1, err := repositories.Executions.FindByTaskVersion(context.Background(), taskID, 1)
	if err != nil {
		t.Fatalf("find historical v1 after version Guard: %v", err)
	}
	if v1.Status != wantV1Status {
		t.Fatalf("historical v1 status after version Guard = %s, want %s", v1.Status, wantV1Status)
	}
}

func testTaskRuntimeRepositoryObservedConfigHash(t *testing.T, environment *postgrestest.RepositoryEnvironment) {
	repositories := postgrestaskruntime.New(environment.Runtime.ReadPool())
	now := repositoryDatabaseNow(t, environment, repositories.Clock)
	graph := repositoryGraph("observed-config", now, now)
	insertRepositoryGraph(t, environment, repositories, graph)

	observedHash := contracts.ExecutionConfigHash(strings.Repeat("1", 64))
	differentHash := contracts.ExecutionConfigHash(strings.Repeat("2", 64))
	mismatch := contracts.ErrorCodeConfigVersionMismatch
	interruptedAt := now.Add(time.Second)

	updated := updateTaskExecutionForTest(t, environment, repositories, domain.TaskExecutionUpdate{
		TaskID:             graph.task.TaskID,
		ExecutionVersion:   1,
		ExpectedStatus:     contracts.TaskExecutionStatusQueued,
		Status:             contracts.TaskExecutionStatusInterrupted,
		ObservedConfigHash: &observedHash,
		ErrorCode:          &mismatch,
		EndedAt:            &interruptedAt,
	})
	if !updated {
		t.Fatal("first CONFIG_VERSION_MISMATCH update did not affect one row")
	}
	assertObservedConfigExecution(
		t,
		repositories,
		graph.task.TaskID,
		contracts.TaskExecutionStatusInterrupted,
		&observedHash,
		&mismatch,
		nil,
		&interruptedAt,
	)

	cancelled := contracts.TerminationReasonCancelled
	for _, attempt := range []struct {
		name string
		hash contracts.ExecutionConfigHash
	}{
		{name: "same hash", hash: observedHash},
		{name: "different hash", hash: differentHash},
	} {
		t.Run(attempt.name, func(t *testing.T) {
			updated := updateTaskExecutionForTest(t, environment, repositories, domain.TaskExecutionUpdate{
				TaskID:             graph.task.TaskID,
				ExecutionVersion:   1,
				ExpectedStatus:     contracts.TaskExecutionStatusInterrupted,
				Status:             contracts.TaskExecutionStatusFailed,
				ObservedConfigHash: &attempt.hash,
				ErrorCode:          &mismatch,
				TerminationReason:  &cancelled,
				EndedAt:            &interruptedAt,
			})
			if updated {
				t.Fatal("repeated observed_config_hash update affected a row")
			}
			assertObservedConfigExecution(
				t,
				repositories,
				graph.task.TaskID,
				contracts.TaskExecutionStatusInterrupted,
				&observedHash,
				&mismatch,
				nil,
				&interruptedAt,
			)
		})
	}

	// nil 表示本次更新不写该证据；后续合法转换无需由调用方回传数据库原值。
	updated = updateTaskExecutionForTest(t, environment, repositories, domain.TaskExecutionUpdate{
		TaskID:            graph.task.TaskID,
		ExecutionVersion:  1,
		ExpectedStatus:    contracts.TaskExecutionStatusInterrupted,
		Status:            contracts.TaskExecutionStatusFailed,
		ErrorCode:         &mismatch,
		TerminationReason: &cancelled,
		EndedAt:           &interruptedAt,
	})
	if !updated {
		t.Fatal("legal transition preserving observed_config_hash did not affect one row")
	}
	assertObservedConfigExecution(
		t,
		repositories,
		graph.task.TaskID,
		contracts.TaskExecutionStatusFailed,
		&observedHash,
		&mismatch,
		&cancelled,
		&interruptedAt,
	)

	ordinaryGraph := repositoryGraph("observed-config-ordinary", now, now)
	insertRepositoryGraph(t, environment, repositories, ordinaryGraph)
	workerInterrupted := contracts.ErrorCodeWorkerInterrupted
	rejected := updateTaskExecutionForTest(t, environment, repositories, domain.TaskExecutionUpdate{
		TaskID:             ordinaryGraph.task.TaskID,
		ExecutionVersion:   1,
		ExpectedStatus:     contracts.TaskExecutionStatusQueued,
		Status:             contracts.TaskExecutionStatusInterrupted,
		ObservedConfigHash: &observedHash,
		ErrorCode:          &workerInterrupted,
		EndedAt:            &interruptedAt,
	})
	if rejected {
		t.Fatal("non-CONFIG_VERSION_MISMATCH update wrote observed_config_hash")
	}
	assertObservedConfigExecution(
		t,
		repositories,
		ordinaryGraph.task.TaskID,
		contracts.TaskExecutionStatusQueued,
		nil,
		nil,
		nil,
		nil,
	)

	workerID := contracts.WorkerID("worker-observed-config")
	updated = updateTaskExecutionForTest(t, environment, repositories, domain.TaskExecutionUpdate{
		TaskID:           ordinaryGraph.task.TaskID,
		ExecutionVersion: 1,
		ExpectedStatus:   contracts.TaskExecutionStatusQueued,
		Status:           contracts.TaskExecutionStatusRunning,
		WorkerID:         &workerID,
		StartedAt:        &now,
	})
	if !updated {
		t.Fatal("ordinary TaskExecution update did not affect one row")
	}
	execution, err := repositories.Executions.FindByTaskVersion(context.Background(), ordinaryGraph.task.TaskID, 1)
	if err != nil {
		t.Fatalf("find ordinary TaskExecution: %v", err)
	}
	if execution.Status != contracts.TaskExecutionStatusRunning || execution.WorkerID == nil ||
		*execution.WorkerID != workerID || execution.ObservedConfigHash != nil {
		t.Fatalf("ordinary TaskExecution after update = %+v", execution)
	}
}

func updateTaskExecutionForTest(
	t *testing.T,
	environment *postgrestest.RepositoryEnvironment,
	repositories *postgrestaskruntime.Repositories,
	update domain.TaskExecutionUpdate,
) bool {
	t.Helper()
	var updated bool
	if err := environment.Runtime.WriteExecutor().Execute(context.Background(), func(ctx context.Context, tx contracts.RuntimeWriteTx) error {
		var err error
		updated, err = repositories.Executions.Update(ctx, tx, update)
		return err
	}); err != nil {
		t.Fatalf("update TaskExecution: %v", err)
	}
	return updated
}

func assertObservedConfigExecution(
	t *testing.T,
	repositories *postgrestaskruntime.Repositories,
	taskID contracts.TaskID,
	wantStatus contracts.TaskExecutionStatus,
	wantHash *contracts.ExecutionConfigHash,
	wantError *contracts.ErrorCode,
	wantTermination *contracts.TerminationReason,
	wantEndedAt *time.Time,
) {
	t.Helper()
	execution, err := repositories.Executions.FindByTaskVersion(context.Background(), taskID, 1)
	if err != nil {
		t.Fatalf("find TaskExecution after observed_config_hash update: %v", err)
	}
	if execution.Status != wantStatus || !reflect.DeepEqual(execution.ObservedConfigHash, wantHash) ||
		!reflect.DeepEqual(execution.ErrorCode, wantError) ||
		!reflect.DeepEqual(execution.TerminationReason, wantTermination) ||
		!reflect.DeepEqual(execution.EndedAt, wantEndedAt) {
		t.Fatalf(
			"TaskExecution after observed_config_hash update = %+v, want status=%s hash=%v error=%v termination=%v ended_at=%v",
			execution,
			wantStatus,
			wantHash,
			wantError,
			wantTermination,
			wantEndedAt,
		)
	}
}

func testTaskRuntimeRepositoryFIFO(t *testing.T, environment *postgrestest.RepositoryEnvironment) {
	repositories := postgrestaskruntime.New(environment.Runtime.ReadPool())
	now := repositoryDatabaseNow(t, environment, repositories.Clock)
	graphs := []repositoryGraphValues{
		repositoryGraph("d", now.Add(-time.Minute), now),
		repositoryGraph("b", now, now.Add(2*time.Second)),
		repositoryGraph("c", now, now.Add(time.Second)),
		repositoryGraph("a", now, now.Add(time.Second)),
	}
	for _, graph := range graphs {
		insertRepositoryGraph(t, environment, repositories, graph)
	}

	filtered := repositoryGraph("filtered", now.Add(-2*time.Minute), now.Add(-time.Minute))
	filteredWorker := contracts.WorkerID("worker-filtered")
	filtered.task.Status = contracts.TaskStatusRunning
	filtered.task.QueuedAt = nil
	filtered.task.StartedAt = &now
	filtered.run.Status = contracts.RunStatusRunning
	filtered.run.StartedAt = &now
	filtered.execution.Status = contracts.TaskExecutionStatusRunning
	filtered.execution.WorkerID = &filteredWorker
	filtered.execution.StartedAt = &now
	insertRepositoryGraph(t, environment, repositories, filtered)

	claimedAt := now.Add(10 * time.Second)
	got := make([]contracts.TaskID, 0, len(graphs))
	for index := range graphs {
		workerID := contracts.WorkerID(fmt.Sprintf("worker-fifo-%d", index))
		if err := environment.Runtime.WriteExecutor().Execute(context.Background(), func(ctx context.Context, tx contracts.RuntimeWriteTx) error {
			candidate, err := repositories.Tasks.LockNextQueueCandidate(ctx, tx)
			if err != nil {
				return err
			}
			got = append(got, candidate.TaskID)
			return advanceQueueCandidateForTest(ctx, tx, repositories, candidate, workerID, claimedAt)
		}); err != nil {
			t.Fatalf("lock and advance FIFO candidate %d: %v", index, err)
		}
	}
	want := []contracts.TaskID{"task-d", "task-a", "task-c", "task-b"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("FIFO candidates = %v, want %v", got, want)
	}

	err := environment.Runtime.WriteExecutor().Execute(context.Background(), func(ctx context.Context, tx contracts.RuntimeWriteTx) error {
		_, err := repositories.Tasks.LockNextQueueCandidate(ctx, tx)
		return err
	})
	if !errors.Is(err, domain.ErrRepositoryNotFound) {
		t.Fatalf("empty FIFO queue error = %v, want ErrRepositoryNotFound", err)
	}

	if _, err := repositories.Tasks.LockNextQueueCandidate(context.Background(), &foreignRuntimeWriteTx{}); !errors.Is(err, postgresruntime.ErrInvalidRuntimeWriteTx) {
		t.Fatalf("foreign FIFO transaction token error = %v, want ErrInvalidRuntimeWriteTx", err)
	}
}

func testTaskRuntimeRepositoryFIFOConcurrency(t *testing.T, environment *postgrestest.RepositoryEnvironment) {
	repositories := postgrestaskruntime.New(environment.Runtime.ReadPool())
	now := repositoryDatabaseNow(t, environment, repositories.Clock)
	head := repositoryGraph("lock-head", now.Add(-time.Minute), now)
	next := repositoryGraph("lock-next", now, now.Add(time.Second))
	insertRepositoryGraph(t, environment, repositories, head)
	insertRepositoryGraph(t, environment, repositories, next)

	locker := postgrestest.Connect(t, environment.Identities.RuntimeWriteDSN)
	observer := postgrestest.Connect(t, environment.Database.DSN)
	lockerTx, err := locker.BeginTx(context.Background(), pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		t.Fatalf("begin competing FIFO transaction: %v", err)
	}
	defer func() { _ = lockerTx.Rollback(context.Background()) }()

	var blockerPID int32
	if err := lockerTx.QueryRow(context.Background(), "SELECT pg_backend_pid()").Scan(&blockerPID); err != nil {
		t.Fatalf("read competing transaction backend PID: %v", err)
	}
	var lockedTaskID contracts.TaskID
	if err := lockerTx.QueryRow(context.Background(), `
SELECT task_id
FROM task
WHERE queued_at IS NOT NULL
ORDER BY queued_at ASC, created_at ASC, task_id ASC
LIMIT 1
FOR UPDATE`).Scan(&lockedTaskID); err != nil {
		t.Fatalf("lock competing FIFO head: %v", err)
	}
	if lockedTaskID != head.task.TaskID {
		t.Fatalf("competing transaction locked %s, want %s", lockedTaskID, head.task.TaskID)
	}

	started := make(chan struct{})
	result := make(chan queueCandidateResult, 1)
	go func() {
		close(started)
		var candidate domain.QueueCandidate
		err := environment.Runtime.WriteExecutor().Execute(context.Background(), func(ctx context.Context, tx contracts.RuntimeWriteTx) error {
			var err error
			candidate, err = repositories.Tasks.LockNextQueueCandidate(ctx, tx)
			return err
		})
		result <- queueCandidateResult{candidate: candidate, err: err}
	}()
	<-started
	waitForBackendBlockedBy(t, observer, blockerPID)
	select {
	case completed := <-result:
		t.Fatalf("FIFO selector skipped locked queue head: candidate=%+v error=%v", completed.candidate, completed.err)
	default:
	}

	workerID := contracts.WorkerID("worker-lock-head")
	if _, err := lockerTx.Exec(context.Background(), `
UPDATE task
SET status = 'Running', queued_at = NULL, started_at = $2
WHERE task_id = $1`, head.task.TaskID, now); err != nil {
		t.Fatalf("advance locked FIFO Task: %v", err)
	}
	if _, err := lockerTx.Exec(context.Background(), `
UPDATE run
SET status = 'Running', started_at = $2
WHERE task_id = $1`, head.task.TaskID, now); err != nil {
		t.Fatalf("advance locked FIFO Run: %v", err)
	}
	if _, err := lockerTx.Exec(context.Background(), `
UPDATE task_execution
SET status = 'RUNNING', worker_id = $2, started_at = $3
WHERE task_id = $1 AND execution_version = 1`, head.task.TaskID, workerID, now); err != nil {
		t.Fatalf("advance locked FIFO TaskExecution: %v", err)
	}
	if err := lockerTx.Commit(context.Background()); err != nil {
		t.Fatalf("commit competing FIFO transaction: %v", err)
	}

	waitCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	var selected queueCandidateResult
	select {
	case selected = <-result:
	case <-waitCtx.Done():
		t.Fatalf("wait for blocked FIFO selector: %v", waitCtx.Err())
	}
	if selected.err != nil || selected.candidate.TaskID != next.task.TaskID {
		t.Fatalf("candidate after locked head advanced = (%+v, %v), want %s", selected.candidate, selected.err, next.task.TaskID)
	}

	wantRollback := errors.New("force FIFO lock rollback")
	err = environment.Runtime.WriteExecutor().Execute(context.Background(), func(ctx context.Context, tx contracts.RuntimeWriteTx) error {
		candidate, err := repositories.Tasks.LockNextQueueCandidate(ctx, tx)
		if err != nil {
			return err
		}
		if candidate.TaskID != next.task.TaskID {
			return fmt.Errorf("rollback transaction selected %s, want %s", candidate.TaskID, next.task.TaskID)
		}
		return wantRollback
	})
	if !errors.Is(err, wantRollback) {
		t.Fatalf("FIFO rollback error = %v, want %v", err, wantRollback)
	}

	if err := environment.Runtime.WriteExecutor().Execute(context.Background(), func(ctx context.Context, tx contracts.RuntimeWriteTx) error {
		candidate, err := repositories.Tasks.LockNextQueueCandidate(ctx, tx)
		if err != nil {
			return err
		}
		if candidate.TaskID != next.task.TaskID {
			return fmt.Errorf("candidate after rollback = %s, want %s", candidate.TaskID, next.task.TaskID)
		}
		return nil
	}); err != nil {
		t.Fatalf("lock FIFO candidate after rollback: %v", err)
	}

	start := make(chan struct{})
	advanceResults := make(chan queueAdvanceResult, 2)
	advanceCtx, cancelAdvance := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancelAdvance()
	for contender := range 2 {
		go func(index int) {
			<-start
			outcome := queueAdvanceResult{}
			outcome.err = environment.Runtime.WriteExecutor().Execute(advanceCtx, func(ctx context.Context, tx contracts.RuntimeWriteTx) error {
				candidate, err := repositories.Tasks.LockNextQueueCandidate(ctx, tx)
				if errors.Is(err, domain.ErrRepositoryNotFound) {
					outcome.noWork = true
					return nil
				}
				if err != nil {
					return err
				}
				outcome.taskID = candidate.TaskID
				workerID := contracts.WorkerID(fmt.Sprintf("worker-concurrent-%d", index))
				return advanceQueueCandidateForTest(ctx, tx, repositories, candidate, workerID, now.Add(10*time.Second))
			})
			advanceResults <- outcome
		}(contender)
	}
	close(start)

	var advanced, noWork int
	for range 2 {
		select {
		case outcome := <-advanceResults:
			if outcome.err != nil {
				t.Fatalf("concurrent FIFO advance: %v", outcome.err)
			}
			if outcome.noWork {
				noWork++
				continue
			}
			if outcome.taskID != next.task.TaskID {
				t.Fatalf("concurrent FIFO advance selected %s, want %s", outcome.taskID, next.task.TaskID)
			}
			advanced++
		case <-advanceCtx.Done():
			t.Fatalf("wait for concurrent FIFO advances: %v", advanceCtx.Err())
		}
	}
	if advanced != 1 || noWork != 1 {
		t.Fatalf("concurrent FIFO outcomes = (advanced:%d, no-work:%d), want (1, 1)", advanced, noWork)
	}
}

type queueCandidateResult struct {
	candidate domain.QueueCandidate
	err       error
}

type queueAdvanceResult struct {
	taskID contracts.TaskID
	noWork bool
	err    error
}

func waitForBackendBlockedBy(t *testing.T, observer *pgx.Conn, blockerPID int32) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		var blocked bool
		err := observer.QueryRow(ctx, `
SELECT EXISTS (
    SELECT 1
    FROM pg_stat_activity AS activity
    WHERE activity.datname = current_database()
      AND $1 = ANY(pg_blocking_pids(activity.pid))
)`, blockerPID).Scan(&blocked)
		if err != nil {
			t.Fatalf("observe blocked FIFO selector: %v", err)
		}
		if blocked {
			return
		}
		select {
		case <-ctx.Done():
			t.Fatalf("FIFO selector did not block on queue head: %v", ctx.Err())
		case <-ticker.C:
		}
	}
}

func advanceQueueCandidateForTest(
	ctx context.Context,
	tx contracts.RuntimeWriteTx,
	repositories *postgrestaskruntime.Repositories,
	candidate domain.QueueCandidate,
	workerID contracts.WorkerID,
	startedAt time.Time,
) error {
	taskUpdated, err := repositories.Tasks.Update(ctx, tx, domain.TaskUpdate{
		TaskID:                          candidate.TaskID,
		ExpectedStatus:                  candidate.TaskStatus,
		ExpectedCurrentExecutionVersion: candidate.ExecutionVersion,
		Status:                          contracts.TaskStatusRunning,
		CurrentExecutionVersion:         candidate.ExecutionVersion,
		StartedAt:                       &startedAt,
	})
	if err != nil || !taskUpdated {
		return errors.Join(err, errors.New("advance FIFO Task did not affect one row"))
	}
	runUpdated, err := repositories.Runs.Update(ctx, tx, domain.RunUpdate{
		TaskID:           candidate.TaskID,
		RunID:            candidate.RunID,
		ExecutionVersion: candidate.ExecutionVersion,
		ExpectedStatus:   contracts.RunStatusPending,
		Status:           contracts.RunStatusRunning,
		Context:          json.RawMessage(`{}`),
		StartedAt:        &startedAt,
	})
	if err != nil || !runUpdated {
		return errors.Join(err, errors.New("advance FIFO Run did not affect one row"))
	}
	executionUpdated, err := repositories.Executions.Update(ctx, tx, domain.TaskExecutionUpdate{
		TaskID:           candidate.TaskID,
		ExecutionVersion: candidate.ExecutionVersion,
		ExpectedStatus:   candidate.ExecutionStatus,
		Status:           contracts.TaskExecutionStatusRunning,
		WorkerID:         &workerID,
		StartedAt:        &startedAt,
	})
	if err != nil || !executionUpdated {
		return errors.Join(err, errors.New("advance FIFO TaskExecution did not affect one row"))
	}
	return nil
}

func testTaskRuntimeRepositoryCurrentExecutionPointer(t *testing.T, environment *postgrestest.RepositoryEnvironment) {
	repositories := postgrestaskruntime.New(environment.Runtime.ReadPool())
	now := repositoryDatabaseNow(t, environment, repositories.Clock)
	graph := repositoryGraph("pointer", now, now)
	insertRepositoryGraph(t, environment, repositories, graph)
	second := graph.execution
	second.TaskExecutionID = "execution-pointer-v2"
	second.ExecutionVersion = 2
	second.CreatedAt = now.Add(time.Second)

	if err := environment.Runtime.WriteExecutor().Execute(context.Background(), func(ctx context.Context, tx contracts.RuntimeWriteTx) error {
		if err := repositories.Executions.Insert(ctx, tx, second); err != nil {
			return err
		}
		updated, err := repositories.Tasks.Update(ctx, tx, domain.TaskUpdate{
			TaskID:                          graph.task.TaskID,
			ExpectedStatus:                  contracts.TaskStatusPending,
			ExpectedCurrentExecutionVersion: 1,
			Status:                          contracts.TaskStatusPending,
			CurrentExecutionVersion:         2,
			QueuedAt:                        graph.task.QueuedAt,
		})
		if err != nil {
			return err
		}
		if !updated {
			return errors.New("current execution pointer update did not match")
		}
		return nil
	}); err != nil {
		t.Fatalf("advance current execution pointer: %v", err)
	}

	task, err := repositories.Tasks.Find(context.Background(), graph.task.TaskID)
	if err != nil || task.CurrentExecutionVersion != 2 {
		t.Fatalf("current Task pointer = (%d, %v), want 2", task.CurrentExecutionVersion, err)
	}
	first, err := repositories.Executions.FindByTaskVersion(context.Background(), graph.task.TaskID, 1)
	if err != nil || first.TaskExecutionID != graph.execution.TaskExecutionID {
		t.Fatalf("explicit v1 lookup = (%+v, %v)", first, err)
	}
	current, err := repositories.Executions.FindByTaskVersion(context.Background(), graph.task.TaskID, task.CurrentExecutionVersion)
	if err != nil || current.TaskExecutionID != second.TaskExecutionID {
		t.Fatalf("current pointer lookup = (%+v, %v)", current, err)
	}
}

func testTaskRuntimeRepositoryReceiptUniqueness(t *testing.T, environment *postgrestest.RepositoryEnvironment) {
	repositories := postgrestaskruntime.New(environment.Runtime.ReadPool())
	now := repositoryDatabaseNow(t, environment, repositories.Clock)
	receipt := domain.CommandReceipt{
		CommandID:          "command-unique",
		CommandType:        domain.CommandTypeCreate,
		TargetID:           "task-unique",
		RequestFingerprint: strings.Repeat("a", 64),
		Response:           json.RawMessage(`{"ok":true}`),
		CreatedAt:          now,
	}
	insert := func() error {
		return environment.Runtime.WriteExecutor().Execute(context.Background(), func(ctx context.Context, tx contracts.RuntimeWriteTx) error {
			return repositories.Receipts.Insert(ctx, tx, receipt)
		})
	}
	if err := insert(); err != nil {
		t.Fatalf("insert first Receipt: %v", err)
	}
	err := insert()
	var databaseError *pgconn.PgError
	if !errors.As(err, &databaseError) || databaseError.Code != "23505" {
		t.Fatalf("duplicate Receipt error = %v, want transparent PostgreSQL unique violation", err)
	}

	stored, err := repositories.Receipts.Find(context.Background(), receipt.CommandID)
	if err != nil || stored.RequestFingerprint != receipt.RequestFingerprint {
		t.Fatalf("stored immutable Receipt = (%+v, %v)", stored, err)
	}
}

func testTaskRuntimeRepositoryClockAndChannels(t *testing.T, environment *postgrestest.RepositoryEnvironment) {
	repositories := postgrestaskruntime.New(environment.Runtime.ReadPool())
	before := time.Now().UTC().Add(-time.Second)
	now := repositoryDatabaseNow(t, environment, repositories.Clock)
	after := time.Now().UTC().Add(time.Second)
	if now.Location() != time.UTC || now.Before(before) || now.After(after) {
		t.Fatalf("database UTC time = %v, expected within [%v, %v]", now, before, after)
	}

	err := repositories.TaskLogs.Append(context.Background(), &foreignRuntimeWriteTx{}, domain.TaskLog{})
	if err == nil {
		t.Fatal("foreign write transaction token unexpectedly accepted")
	}

	row := environment.Runtime.ReadPool().QueryRow(
		context.Background(),
		"INSERT INTO command_receipt (command_id, command_type, target_id, request_fingerprint, response, created_at) "+
			"VALUES ('reader-write', 'Create', 'task', repeat('a', 64), '{}'::jsonb, now()) RETURNING command_id",
	)
	var commandID string
	if err := row.Scan(&commandID); err == nil {
		t.Fatal("Runtime read pool completed a direct write")
	}

	var one int
	if err := environment.Runtime.ReadPool().QueryRow(context.Background(), "SELECT 1").Scan(&one); err != nil || one != 1 {
		t.Fatalf("legal read query = (%d, %v), want (1, nil)", one, err)
	}
}

func testTaskRuntimeRepositoryTaskLogAppend(t *testing.T, environment *postgrestest.RepositoryEnvironment) {
	repositories := postgrestaskruntime.New(environment.Runtime.ReadPool())
	now := repositoryDatabaseNow(t, environment, repositories.Clock)
	graph := repositoryGraph("log", now, now)
	insertRepositoryGraph(t, environment, repositories, graph)
	version := contracts.ExecutionVersion(1)
	log := domain.TaskLog{
		LogID:            "log-one",
		TaskID:           graph.task.TaskID,
		RunID:            graph.run.RunID,
		ExecutionVersion: &version,
		Level:            domain.TaskLogLevelInfo,
		Event:            "TaskCreated",
		Message:          "created",
		Operator:         "System",
		CreatedAt:        now,
	}
	if err := environment.Runtime.WriteExecutor().Execute(context.Background(), func(ctx context.Context, tx contracts.RuntimeWriteTx) error {
		return repositories.TaskLogs.Append(ctx, tx, log)
	}); err != nil {
		t.Fatalf("append TaskLog: %v", err)
	}

	var (
		count int
		event string
	)
	if err := environment.Runtime.ReadPool().QueryRow(
		context.Background(),
		"SELECT count(*), max(event) FROM task_log WHERE task_id = $1",
		graph.task.TaskID,
	).Scan(&count, &event); err != nil {
		t.Fatalf("query appended TaskLog: %v", err)
	}
	if count != 1 || event != log.Event {
		t.Fatalf("TaskLog aggregate = (%d, %q), want (1, %q)", count, event, log.Event)
	}
}

type repositoryGraphValues struct {
	task      domain.Task
	run       domain.Run
	execution domain.TaskExecution
	receipt   domain.CommandReceipt
}

func repositoryGraph(suffix string, queuedAt time.Time, createdAt time.Time) repositoryGraphValues {
	queued := queuedAt.UTC()
	return repositoryGraphValues{
		task: domain.Task{
			TaskID:                  contracts.TaskID("task-" + suffix),
			AgentID:                 "agent-default",
			CreatedBy:               "operator",
			Input:                   "input",
			Status:                  contracts.TaskStatusPending,
			CurrentRunID:            contracts.RunID("run-" + suffix),
			CurrentExecutionVersion: 1,
			DeadlineAt:              createdAt.UTC().Add(time.Hour),
			QueuedAt:                &queued,
			CreatedAt:               createdAt.UTC(),
		},
		run: domain.Run{
			RunID:   contracts.RunID("run-" + suffix),
			TaskID:  contracts.TaskID("task-" + suffix),
			Status:  contracts.RunStatusPending,
			Context: json.RawMessage(`{}`),
		},
		execution: domain.TaskExecution{
			TaskExecutionID:     domain.TaskExecutionID("execution-" + suffix),
			TaskID:              contracts.TaskID("task-" + suffix),
			ExecutionVersion:    1,
			Status:              contracts.TaskExecutionStatusQueued,
			ExecutionConfigHash: contracts.ExecutionConfigHash(strings.Repeat("a", 64)),
			CreatedAt:           createdAt.UTC(),
		},
		receipt: domain.CommandReceipt{
			CommandID:          domain.CommandID("command-" + suffix),
			CommandType:        domain.CommandTypeCreate,
			TargetID:           "task-" + suffix,
			RequestFingerprint: strings.Repeat("b", 64),
			Response:           json.RawMessage(`{"status":"Pending"}`),
			CreatedAt:          createdAt.UTC(),
		},
	}
}

func insertRepositoryGraph(
	t *testing.T,
	environment *postgrestest.RepositoryEnvironment,
	repositories *postgrestaskruntime.Repositories,
	graph repositoryGraphValues,
) {
	t.Helper()
	if err := environment.Runtime.WriteExecutor().Execute(context.Background(), func(ctx context.Context, tx contracts.RuntimeWriteTx) error {
		if err := repositories.Tasks.Insert(ctx, tx, graph.task); err != nil {
			return err
		}
		if err := repositories.Runs.Insert(ctx, tx, graph.run); err != nil {
			return err
		}
		return repositories.Executions.Insert(ctx, tx, graph.execution)
	}); err != nil {
		t.Fatalf("insert Repository graph %s: %v", graph.task.TaskID, err)
	}
}

func repositoryDatabaseNow(
	t *testing.T,
	environment *postgrestest.RepositoryEnvironment,
	clock domain.DatabaseClock,
) time.Time {
	t.Helper()
	var now time.Time
	if err := environment.Runtime.WriteExecutor().Execute(context.Background(), func(ctx context.Context, tx contracts.RuntimeWriteTx) error {
		var err error
		now, err = clock.Now(ctx, tx)
		return err
	}); err != nil {
		t.Fatalf("read Repository database clock: %v", err)
	}
	return now
}

type taskViewQueryResult struct {
	view domain.TaskView
	err  error
}

type taskViewListResult struct {
	views []domain.TaskView
	err   error
}

type querySnapshotBarrier struct {
	entered  chan struct{}
	releaseC chan struct{}
	observed chan contracts.TaskStatus
}

func newQuerySnapshotBarrier() *querySnapshotBarrier {
	return &querySnapshotBarrier{
		entered:  make(chan struct{}, 1),
		releaseC: make(chan struct{}, 1),
		observed: make(chan contracts.TaskStatus, 1),
	}
}

func (b *querySnapshotBarrier) LoadSnapshotEvidence(
	ctx context.Context,
	snapshot postgresruntime.ReadSnapshot,
	taskID contracts.TaskID,
	_ contracts.RunID,
	_ contracts.ExecutionVersion,
) (domain.RecoverabilityEvidence, error) {
	select {
	case b.entered <- struct{}{}:
	default:
	}
	select {
	case <-ctx.Done():
		return domain.RecoverabilityEvidence{}, ctx.Err()
	case <-b.releaseC:
	}

	var (
		statusText  string
		databaseNow time.Time
	)
	if err := snapshot.QueryRow(
		"SELECT status, clock_timestamp() FROM task WHERE task_id = $1",
		taskID,
	).Scan(&statusText, &databaseNow); err != nil {
		return domain.RecoverabilityEvidence{}, err
	}
	status := contracts.TaskStatus(statusText)
	b.observed <- status
	return domain.RecoverabilityEvidence{DatabaseNow: databaseNow.UTC()}, nil
}

func (b *querySnapshotBarrier) awaitEntered(t *testing.T) {
	t.Helper()
	select {
	case <-b.entered:
	case <-time.After(5 * time.Second):
		b.release()
		t.Fatal("Task query did not reach synchronized evidence barrier")
	}
}

func (b *querySnapshotBarrier) awaitObservedStatus(t *testing.T) contracts.TaskStatus {
	t.Helper()
	select {
	case status := <-b.observed:
		return status
	case <-time.After(5 * time.Second):
		t.Fatal("Task query evidence did not report its snapshot status")
		return ""
	}
}

func (b *querySnapshotBarrier) release() {
	select {
	case b.releaseC <- struct{}{}:
	default:
	}
}

type databaseNowSnapshotEvidence struct{}

func (databaseNowSnapshotEvidence) LoadSnapshotEvidence(
	_ context.Context,
	snapshot postgresruntime.ReadSnapshot,
	_ contracts.TaskID,
	_ contracts.RunID,
	_ contracts.ExecutionVersion,
) (domain.RecoverabilityEvidence, error) {
	var databaseNow time.Time
	if err := snapshot.QueryRow("SELECT clock_timestamp()").Scan(&databaseNow); err != nil {
		return domain.RecoverabilityEvidence{}, err
	}
	return domain.RecoverabilityEvidence{DatabaseNow: databaseNow.UTC()}, nil
}

type emptyTaskQueryConfigSource struct{}

func (emptyTaskQueryConfigSource) LookupAgent(contracts.AgentID) (domain.AgentRuntimeConfig, bool) {
	return domain.AgentRuntimeConfig{}, false
}

func newRepositoryTaskQueryService(
	t *testing.T,
	environment *postgrestest.RepositoryEnvironment,
	evidence postgrestaskruntime.SnapshotEvidenceProjection,
) *domain.TaskQueryService {
	t.Helper()
	snapshots, err := postgrestaskruntime.NewTaskQuerySnapshotRepository(environment.Runtime.ReadPool(), evidence)
	if err != nil {
		t.Fatalf("create Task query snapshot repository: %v", err)
	}
	service, err := domain.NewTaskQueryService(snapshots, emptyTaskQueryConfigSource{})
	if err != nil {
		t.Fatalf("create Task query service: %v", err)
	}
	return service
}

func awaitTaskQueryResult(t *testing.T, result <-chan taskViewQueryResult) taskViewQueryResult {
	t.Helper()
	select {
	case received := <-result:
		return received
	case <-time.After(5 * time.Second):
		t.Fatal("GetTask did not complete after releasing synchronized barrier")
		return taskViewQueryResult{}
	}
}

func awaitTaskListResult(t *testing.T, result <-chan taskViewListResult) taskViewListResult {
	t.Helper()
	select {
	case received := <-result:
		return received
	case <-time.After(5 * time.Second):
		t.Fatal("ListTasks did not complete after releasing synchronized barrier")
		return taskViewListResult{}
	}
}

type foreignRuntimeWriteTx struct{}

func (*foreignRuntimeWriteTx) AgentOpsRuntimeWriteTx() {}
