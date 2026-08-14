package postgres_test

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/zhaohaip/agentops-go/internal/contracts"
	"github.com/zhaohaip/agentops-go/internal/lifecycle"
	domain "github.com/zhaohaip/agentops-go/internal/taskruntime"
	"github.com/zhaohaip/agentops-go/internal/taskruntime/activecall"
	"github.com/zhaohaip/agentops-go/test/integration/postgres/postgrestest"
)

func TestRecoverTaskUsesRealPostgreSQLTransaction(t *testing.T) {
	postgrestest.RunRepositoryContract(t, postgrestest.RepositoryContract{
		Name:       "Task Runtime RecoverTask",
		Migrations: checkpointMigrationSet(),
		Cases: []postgrestest.RepositoryCase{
			{Name: "GENERATE_PLAN recovery and Receipt replay", Run: testRecoverTaskGeneratePlan},
			{Name: "configuration mismatch Receipt and new command retry", Run: testRecoverTaskConfigMismatch},
			{Name: "elapsed deadline is terminalized by Recover", Run: testRecoverTaskElapsedDeadline},
			{Name: "Timeout commits while Recover waits on row lock", Run: testRecoverTaskTimeoutWins},
			{Name: "expired Completed remains immutable", Run: terminalRecoverCase(contracts.TaskStatusCompleted)},
			{Name: "expired Cancelled remains immutable", Run: terminalRecoverCase(contracts.TaskStatusCancelled)},
			{Name: "expired Failed remains immutable", Run: terminalRecoverCase(contracts.TaskStatusFailed)},
		},
	})
}

func terminalRecoverCase(status contracts.TaskStatus) func(*testing.T, *postgrestest.RepositoryEnvironment) {
	return func(t *testing.T, environment *postgrestest.RepositoryEnvironment) {
		harness := newRealCheckpointTaskRuntime(t, environment, nil)
		created := harness.create(t, domain.CommandID("create-terminal-"+status))
		setExpiredTerminalFacts(t, environment, created.TaskID, status)
		service := newRealRecoverService(t, harness, newCheckpointAdapter(t), harness.configs)
		beforeTask, beforeRun, beforeExecution := loadTaskRuntimeFacts(t, harness, created.TaskID)
		request := domain.RecoverTaskRequest{CommandID: domain.CommandID("recover-terminal-" + status),
			TaskID: created.TaskID, OperatorID: "operator-1"}
		for attempt := 0; attempt < 2; attempt++ {
			if _, err := service.RecoverTask(context.Background(), request); !errors.Is(err, domain.ErrRecoverStateConflict) {
				t.Fatalf("attempt %d error = %v", attempt+1, err)
			}
		}
		afterTask, afterRun, afterExecution := loadTaskRuntimeFacts(t, harness, created.TaskID)
		if !reflect.DeepEqual(afterTask, beforeTask) || !reflect.DeepEqual(afterRun, beforeRun) ||
			!reflect.DeepEqual(afterExecution, beforeExecution) {
			t.Fatalf("terminal facts changed: before=%+v/%+v/%+v after=%+v/%+v/%+v",
				beforeTask, beforeRun, beforeExecution, afterTask, afterRun, afterExecution)
		}
		receipt, err := harness.repositories.Receipts.Find(context.Background(), request.CommandID)
		if err != nil || receipt.CommandType != domain.CommandTypeRecover {
			t.Fatalf("state conflict Receipt = %+v, %v", receipt, err)
		}
		connection := postgrestest.Connect(t, environment.Database.DSN)
		var receiptCount int
		if err := connection.QueryRow(context.Background(),
			"SELECT count(*) FROM command_receipt WHERE command_id=$1", request.CommandID).Scan(&receiptCount); err != nil || receiptCount != 1 {
			t.Fatalf("Receipt count = %d, %v", receiptCount, err)
		}
		err = harness.executor.Execute(context.Background(), func(ctx context.Context, tx contracts.RuntimeWriteTx) error {
			applied, err := harness.repositories.Recovery.ApplyRecoveryFailure(ctx, tx, domain.ApplyRecoveryFailureRequest{
				TaskID: created.TaskID, ExpectedExecutionVersion: 1, ExpectedTaskStatus: beforeTask.Status,
				ExpectedRunStatus: beforeRun.Status, ExpectedExecutionStatus: beforeExecution.Status,
				ErrorCode: contracts.ErrorCodeTaskTimeout, EndedAt: beforeTask.EndedAt.UTC(),
			})
			if err != nil {
				return err
			}
			if applied {
				return errors.New("ApplyRecoveryFailure accepted terminal source")
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
}

func setExpiredTerminalFacts(t *testing.T, environment *postgrestest.RepositoryEnvironment,
	taskID contracts.TaskID, status contracts.TaskStatus) {
	t.Helper()
	runStatus := contracts.RunStatusCompleted
	executionStatus := contracts.TaskExecutionStatusCompleted
	var errorCode any
	var terminationReason any
	if status != contracts.TaskStatusCompleted {
		runStatus, executionStatus = contracts.RunStatusFailed, contracts.TaskExecutionStatusFailed
		if status == contracts.TaskStatusCancelled {
			errorCode, terminationReason = contracts.ErrorCodeTaskCancelled, contracts.TerminationReasonCancelled
		} else {
			errorCode, terminationReason = contracts.ErrorCodeTaskTimeout, contracts.TerminationReasonTimedOut
		}
	}
	connection := postgrestest.Connect(t, environment.Database.DSN)
	tx, err := connection.Begin(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	if _, err := tx.Exec(context.Background(), `
UPDATE task SET status=$2, error_code=$3, queued_at=NULL,
    deadline_at=created_at+interval '1 microsecond', ended_at=created_at+interval '2 microseconds'
WHERE task_id=$1`, taskID, status, errorCode); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(context.Background(), `
UPDATE run SET status=$2, error_code=$3, ended_at=(SELECT created_at+interval '2 microseconds' FROM task WHERE task_id=$1)
WHERE task_id=$1`, taskID, runStatus, errorCode); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(context.Background(), `
UPDATE task_execution SET status=$2, error_code=$3, observed_config_hash=NULL,
    termination_reason=$4, ended_at=created_at+interval '2 microseconds'
WHERE task_id=$1 AND execution_version=1`, taskID, executionStatus, errorCode, terminationReason); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func testRecoverTaskGeneratePlan(t *testing.T, environment *postgrestest.RepositoryEnvironment) {
	harness := newRealCheckpointTaskRuntime(t, environment, nil)
	created := harness.create(t, "create-for-recover")
	interruptBeforeFirstClaim(t, harness, created.TaskID)
	service := newRealRecoverService(t, harness, newCheckpointAdapter(t), harness.configs)
	request := domain.RecoverTaskRequest{CommandID: "recover-generate-plan", TaskID: created.TaskID, OperatorID: "operator-1"}
	result, err := service.RecoverTask(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	if result.SourceExecutionVersion != 1 || result.NewExecutionVersion != 2 ||
		result.TaskStatus != contracts.TaskStatusPending || result.RunStatus != contracts.RunStatusPending ||
		result.ExecutionStatus != contracts.TaskExecutionStatusQueued || result.RecoveryCheckpointID == "" {
		t.Fatalf("Recover result = %+v", result)
	}
	task, run, current := loadTaskRuntimeFacts(t, harness, created.TaskID)
	old, err := harness.repositories.Executions.FindByTaskVersion(context.Background(), created.TaskID, 1)
	if err != nil {
		t.Fatal(err)
	}
	rows := loadCheckpointRows(t, environment, created.TaskID)
	if task.CurrentExecutionVersion != 2 || task.QueuedAt == nil || task.ErrorCode != nil ||
		run.Status != contracts.RunStatusPending || current.Status != contracts.TaskExecutionStatusQueued ||
		current.WorkerID != nil || current.ObservedConfigHash != nil ||
		old.Status != contracts.TaskExecutionStatusInterrupted || len(rows) != 2 ||
		rows[1].sourceVersion == nil || *rows[1].sourceVersion != 1 {
		t.Fatalf("Recover persisted task=%+v run=%+v old=%+v current=%+v checkpoints=%#v", task, run, old, current, rows)
	}
	replayed, err := service.RecoverTask(context.Background(), request)
	if err != nil || replayed != result {
		t.Fatalf("Receipt replay = %+v, %v", replayed, err)
	}
	if rowsAfter := loadCheckpointRows(t, environment, created.TaskID); len(rowsAfter) != 2 {
		t.Fatalf("Receipt replay created Checkpoint: %#v", rowsAfter)
	}
	connection := postgrestest.Connect(t, environment.Database.DSN)
	var logExecutionVersion contracts.ExecutionVersion
	var logMessage string
	if err := connection.QueryRow(context.Background(), `
SELECT execution_version, message
FROM task_log
WHERE task_id=$1 AND event='CheckpointRestored'`, created.TaskID).Scan(&logExecutionVersion, &logMessage); err != nil {
		t.Fatal(err)
	}
	if logExecutionVersion != 2 ||
		logMessage != "checkpoint restored: source_execution_version=1 new_execution_version=2" ||
		strings.Contains(logMessage, string(harness.hash)) {
		t.Fatalf("CheckpointRestored log = version:%d message:%q", logExecutionVersion, logMessage)
	}
	// A late v1 result remains rejected by the repository's current-version guard.
	err = environment.Runtime.WriteExecutor().Execute(context.Background(), func(ctx context.Context, tx contracts.RuntimeWriteTx) error {
		updated, err := harness.repositories.Executions.Update(ctx, tx, domain.TaskExecutionUpdate{
			TaskID: created.TaskID, ExecutionVersion: 1, ExpectedStatus: contracts.TaskExecutionStatusInterrupted,
			Status: contracts.TaskExecutionStatusFailed,
		})
		if err != nil {
			return err
		}
		if updated {
			return errors.New("old result bypassed current execution version")
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func testRecoverTaskConfigMismatch(t *testing.T, environment *postgrestest.RepositoryEnvironment) {
	harness := newRealCheckpointTaskRuntime(t, environment, nil)
	created := harness.create(t, "create-recover-mismatch")
	interruptBeforeFirstClaim(t, harness, created.TaskID)
	mismatched := harness.config
	mismatched.ExecutionConfig.Planner.Limits.MaxTaskInputBytes++
	mismatchSource := &integrationAgentConfigSource{config: mismatched}
	service := newRealRecoverService(t, harness, newCheckpointAdapter(t), mismatchSource)
	request := domain.RecoverTaskRequest{CommandID: "recover-mismatch", TaskID: created.TaskID, OperatorID: "operator-1"}
	if _, err := service.RecoverTask(context.Background(), request); !errors.Is(err, domain.ErrRecoverConfigMismatch) {
		t.Fatalf("mismatch error = %v", err)
	}
	task, _, _ := loadTaskRuntimeFacts(t, harness, created.TaskID)
	if task.CurrentExecutionVersion != 1 || len(loadCheckpointRows(t, environment, created.TaskID)) != 1 {
		t.Fatal("configuration mismatch partially changed recovery facts")
	}
	service = newRealRecoverService(t, harness, newCheckpointAdapter(t), harness.configs)
	if _, err := service.RecoverTask(context.Background(), request); !errors.Is(err, domain.ErrRecoverConfigMismatch) {
		t.Fatalf("same command was not immutable = %v", err)
	}
	if _, err := service.RecoverTask(context.Background(), domain.RecoverTaskRequest{
		CommandID: "recover-mismatch-retry", TaskID: created.TaskID, OperatorID: "operator-1",
	}); err != nil {
		t.Fatalf("new command retry = %v", err)
	}
}

func testRecoverTaskTimeoutWins(t *testing.T, environment *postgrestest.RepositoryEnvironment) {
	harness := newRealCheckpointTaskRuntime(t, environment, nil)
	created := harness.create(t, "create-recover-timeout")
	interruptBeforeFirstClaim(t, harness, created.TaskID)
	service := newRealRecoverService(t, harness, newCheckpointAdapter(t), harness.configs)
	timeoutConnection := postgrestest.Connect(t, environment.Database.DSN)
	timeoutTx, err := timeoutConnection.Begin(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	committed := false
	defer func() {
		if !committed {
			_ = timeoutTx.Rollback(context.Background())
		}
	}()
	var blockerPID int32
	if err := timeoutTx.QueryRow(context.Background(), "SELECT pg_backend_pid()").Scan(&blockerPID); err != nil {
		t.Fatal(err)
	}
	if _, err := timeoutTx.Exec(context.Background(), `
UPDATE task SET status='Failed', error_code='TaskTimeout', queued_at=NULL,
    deadline_at=created_at+interval '1 microsecond', ended_at=clock_timestamp()
WHERE task_id=$1`, created.TaskID); err != nil {
		t.Fatal(err)
	}
	if _, err := timeoutTx.Exec(context.Background(), `
UPDATE run SET status='Failed', error_code='TaskTimeout', ended_at=clock_timestamp()
WHERE task_id=$1`, created.TaskID); err != nil {
		t.Fatal(err)
	}
	if _, err := timeoutTx.Exec(context.Background(), `
UPDATE task_execution SET status='FAILED', termination_reason='TIMED_OUT'
WHERE task_id=$1 AND execution_version=1`, created.TaskID); err != nil {
		t.Fatal(err)
	}

	tracer := postgrestest.Connect(t, environment.Database.DSN)
	raceContext, cancelRace := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancelRace()
	recoverDone := make(chan error, 1)
	go func() {
		_, err := service.RecoverTask(raceContext, domain.RecoverTaskRequest{
			CommandID: "recover-after-timeout", TaskID: created.TaskID, OperatorID: "operator-1",
		})
		recoverDone <- err
	}()
	waitForBackendBlockedBy(t, tracer, blockerPID)
	if err := timeoutTx.Commit(context.Background()); err != nil {
		t.Fatal(err)
	}
	committed = true
	select {
	case err := <-recoverDone:
		if !errors.Is(err, domain.ErrRecoverStateConflict) {
			t.Fatalf("Recover result after committed Timeout = %v", err)
		}
	case <-raceContext.Done():
		t.Fatalf("Recover did not finish after Timeout commit: %v", raceContext.Err())
	}
	task, run, execution := loadTaskRuntimeFacts(t, harness, created.TaskID)
	if task.Status != contracts.TaskStatusFailed || run.Status != contracts.RunStatusFailed ||
		execution.Status != contracts.TaskExecutionStatusFailed || task.CurrentExecutionVersion != 1 ||
		task.ErrorCode == nil || *task.ErrorCode != contracts.ErrorCodeTaskTimeout ||
		execution.TerminationReason == nil || *execution.TerminationReason != contracts.TerminationReasonTimedOut ||
		len(loadCheckpointRows(t, environment, created.TaskID)) != 1 {
		t.Fatalf("Timeout race facts task=%+v run=%+v execution=%+v", task, run, execution)
	}
	var receiptCount, executionCount int
	if err := tracer.QueryRow(context.Background(), `
SELECT
    (SELECT count(*) FROM command_receipt WHERE command_id='recover-after-timeout'),
    (SELECT count(*) FROM task_execution WHERE task_id=$1)`, created.TaskID).Scan(&receiptCount, &executionCount); err != nil {
		t.Fatal(err)
	}
	if receiptCount != 1 || executionCount != 1 {
		t.Fatalf("Timeout-winning Recover partial writes: receipts=%d executions=%d", receiptCount, executionCount)
	}
}

func testRecoverTaskElapsedDeadline(t *testing.T, environment *postgrestest.RepositoryEnvironment) {
	harness := newRealCheckpointTaskRuntime(t, environment, nil)
	created := harness.create(t, "create-recover-elapsed")
	interruptBeforeFirstClaim(t, harness, created.TaskID)
	connection := postgrestest.Connect(t, environment.Database.DSN)
	if _, err := connection.Exec(context.Background(),
		"UPDATE task SET deadline_at=created_at+interval '1 microsecond' WHERE task_id=$1", created.TaskID); err != nil {
		t.Fatal(err)
	}
	service := newRealRecoverService(t, harness, newCheckpointAdapter(t), harness.configs)
	if _, err := service.RecoverTask(context.Background(), domain.RecoverTaskRequest{
		CommandID: "recover-elapsed", TaskID: created.TaskID, OperatorID: "operator-1",
	}); !errors.Is(err, domain.ErrTaskTimedOut) {
		t.Fatalf("elapsed Recover result = %v", err)
	}
	task, run, execution := loadTaskRuntimeFacts(t, harness, created.TaskID)
	if task.Status != contracts.TaskStatusFailed || run.Status != contracts.RunStatusFailed ||
		execution.Status != contracts.TaskExecutionStatusFailed || task.CurrentExecutionVersion != 1 ||
		len(loadCheckpointRows(t, environment, created.TaskID)) != 1 {
		t.Fatalf("elapsed Recover facts task=%+v run=%+v execution=%+v", task, run, execution)
	}
}

func interruptBeforeFirstClaim(t *testing.T, harness *realCheckpointTaskRuntime, taskID contracts.TaskID) {
	t.Helper()
	err := harness.executor.Execute(context.Background(), func(ctx context.Context, tx contracts.RuntimeWriteTx) error {
		now, err := harness.repositories.Clock.Now(ctx, tx)
		if err != nil {
			return err
		}
		observed := contracts.ExecutionConfigHash("bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb")
		code := contracts.ErrorCodeConfigVersionMismatch
		updated, err := harness.repositories.Executions.Update(ctx, tx, domain.TaskExecutionUpdate{
			TaskID: taskID, ExecutionVersion: 1, ExpectedStatus: contracts.TaskExecutionStatusQueued,
			Status: contracts.TaskExecutionStatusInterrupted, ObservedConfigHash: &observed,
			ErrorCode: &code, EndedAt: &now,
		})
		if err != nil || !updated {
			return errors.New("interrupt source Execution")
		}
		updated, err = harness.repositories.Tasks.Update(ctx, tx, domain.TaskUpdate{
			TaskID: taskID, ExpectedStatus: contracts.TaskStatusPending, ExpectedCurrentExecutionVersion: 1,
			Status: contracts.TaskStatusInterrupted, CurrentExecutionVersion: 1, ErrorCode: &code,
		})
		if err != nil || !updated {
			return errors.New("interrupt source Task")
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func newRealRecoverService(t *testing.T, harness *realCheckpointTaskRuntime,
	checkpointPort domain.RuntimeCheckpointPort, configs domain.AgentConfigSource) *domain.RecoverTaskService {
	t.Helper()
	recoveryCheckpoints, ok := checkpointPort.(domain.RecoveryCheckpointPort)
	if !ok {
		t.Fatal("Checkpoint adapter does not implement RecoveryCheckpointPort")
	}
	service, err := domain.NewRecoverTaskService(domain.RecoverTaskDependencies{
		Executor: harness.executor, Tasks: harness.repositories.Tasks, Runs: harness.repositories.Runs,
		Executions: harness.repositories.Executions, Recovery: harness.repositories.Recovery,
		Receipts: harness.repositories.Receipts, Reports: noOpPendingReportWriter{}, Clock: harness.repositories.Clock,
		Configs: configs, Checkpoints: recoveryCheckpoints, TaskLogs: harness.repositories.TaskLogs,
		ActiveCalls: activecall.NewRegistry(), Policy: lifecycle.New(),
	})
	if err != nil {
		t.Fatal(err)
	}
	return service
}
