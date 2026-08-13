package postgres_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	postgrescheckpoint "github.com/zhaohaip/agentops-go/internal/adapter/postgres/checkpoint"
	postgrestaskruntime "github.com/zhaohaip/agentops-go/internal/adapter/postgres/taskruntime"
	"github.com/zhaohaip/agentops-go/internal/checkpoint"
	"github.com/zhaohaip/agentops-go/internal/config/business"
	"github.com/zhaohaip/agentops-go/internal/contracts"
	"github.com/zhaohaip/agentops-go/internal/lifecycle"
	domain "github.com/zhaohaip/agentops-go/internal/taskruntime"
	taskruntimemigrations "github.com/zhaohaip/agentops-go/migrations/taskruntime"
	"github.com/zhaohaip/agentops-go/test/integration/postgres/postgrestest"
)

func TestCreateAndClaimUseRealCheckpointAtomically(t *testing.T) {
	postgrestest.RunRepositoryContract(t, postgrestest.RepositoryContract{
		Name:       "Task Runtime real Checkpoint integration",
		Migrations: checkpointMigrationSet(),
		Cases: []postgrestest.RepositoryCase{
			{Name: "Create initialization and first Claim execution checkpoint", Run: testCreateAndFirstClaimCheckpoint},
			{Name: "Recovery Start continuation selects latest and preserves timestamps", Run: testQueuedContinuationCheckpoint},
			{Name: "started GENERATE_PLAN cannot be requeued and claimed", Run: testStartedGeneratePlanCannotBeReclaimed},
			{Name: "candidate execution version mismatch is rejected atomically", Run: testClaimCandidateVersionMismatch},
			{Name: "three-way hash guards reject every mismatch", Run: testClaimThreeWayHashGuards},
			{Name: "Continuation rejects Initialization and never falls back", Run: testContinuationCheckpointInvalidNoFallback},
			{Name: "Continuation rejects damaged latest without fallback", Run: testContinuationDamagedLatestNoFallback},
			{Name: "Claim checkpoint failure rolls back worker and states", Run: testClaimCheckpointFailureRollback},
			{Name: "worker guard rejects before write", Run: testClaimWorkerGuard},
		},
	})
}

func testQueuedContinuationCheckpoint(t *testing.T, environment *postgrestest.RepositoryEnvironment) {
	harness := newRealCheckpointTaskRuntime(t, environment, nil)
	created := harness.create(t, "command-continuation")
	claimTask(t, harness, created.TaskID)
	prepareGeneratePlanRecoveryStart(t, environment, harness, created.TaskID)
	taskBefore, runBefore, executionBefore := loadTaskRuntimeFacts(t, harness, created.TaskID)

	rowsBefore := loadCheckpointRows(t, environment, created.TaskID)
	if len(rowsBefore) != 4 || rowsBefore[2].version != 2 || rowsBefore[2].hash == harness.hash ||
		rowsBefore[3].sequence != 4 || rowsBefore[3].version != 2 ||
		rowsBefore[3].sourceVersion == nil || *rowsBefore[3].sourceVersion != 1 ||
		rowsBefore[3].hash != harness.hash {
		t.Fatalf("pre-continuation Checkpoints = %#v", rowsBefore)
	}
	claimTask(t, harness, created.TaskID)
	taskAfter, runAfter, executionAfter := loadTaskRuntimeFacts(t, harness, created.TaskID)
	if taskAfter.StartedAt == nil || runAfter.StartedAt == nil || executionAfter.StartedAt == nil ||
		!taskAfter.StartedAt.Equal(*taskBefore.StartedAt) || !runAfter.StartedAt.Equal(*runBefore.StartedAt) ||
		executionBefore.StartedAt != nil || executionAfter.WorkerID == nil ||
		*executionAfter.WorkerID != harness.workerID || taskAfter.QueuedAt != nil {
		t.Fatalf("continuation facts = task:%+v run:%+v execution:%+v", taskAfter, runAfter, executionAfter)
	}
	rowsAfter := loadCheckpointRows(t, environment, created.TaskID)
	if len(rowsAfter) != len(rowsBefore)+1 || rowsAfter[len(rowsAfter)-1].sequence != 5 ||
		rowsAfter[len(rowsAfter)-1].sourceVersion != nil {
		t.Fatalf("continuation did not add exactly its required Execution Checkpoint: before=%#v after=%#v", rowsBefore, rowsAfter)
	}
}

func testStartedGeneratePlanCannotBeReclaimed(t *testing.T, environment *postgrestest.RepositoryEnvironment) {
	harness := newRealCheckpointTaskRuntime(t, environment, nil)
	created := harness.create(t, "command-started-requeue")
	claimTask(t, harness, created.TaskID)
	task, run, execution := loadTaskRuntimeFacts(t, harness, created.TaskID)
	requeueGeneratePlanContinuation(t, environment, harness, task, run, execution)
	rowsBefore := loadCheckpointRows(t, environment, created.TaskID)

	result, err := harness.claim.ClaimNextExecution(context.Background(), harness.workerID)
	if err != nil {
		t.Fatalf("started GENERATE_PLAN requeue Claim error = %v", err)
	}
	if _, ok := result.(contracts.ClaimResultCheckpointInvalidTerminalized); !ok {
		t.Fatalf("started GENERATE_PLAN requeue result = %T", result)
	}
	taskAfter, runAfter, executionAfter := loadTaskRuntimeFacts(t, harness, created.TaskID)
	if taskAfter.Status != contracts.TaskStatusFailed || runAfter.Status != contracts.RunStatusFailed ||
		executionAfter.Status != contracts.TaskExecutionStatusFailed || executionAfter.WorkerID != nil ||
		taskAfter.QueuedAt != nil {
		t.Fatalf("rejected requeue left partial Claim facts = task:%+v run:%+v execution:%+v", taskAfter, runAfter, executionAfter)
	}
	if rowsAfter := loadCheckpointRows(t, environment, created.TaskID); len(rowsAfter) != len(rowsBefore) {
		t.Fatalf("rejected requeue changed Checkpoints: before=%#v after=%#v", rowsBefore, rowsAfter)
	}
}

func appendGeneratePlanExecutionCheckpoint(
	t *testing.T,
	environment *postgrestest.RepositoryEnvironment,
	harness *realCheckpointTaskRuntime,
	task domain.Task,
) {
	t.Helper()
	adapter := newCheckpointAdapter(t)
	err := environment.Runtime.WriteExecutor().Execute(context.Background(), func(ctx context.Context, tx contracts.RuntimeWriteTx) error {
		if _, err := harness.repositories.Runs.LockByTask(ctx, tx, task.TaskID); err != nil {
			return err
		}
		return adapter.SaveGeneratePlanExecutionCheckpoint(ctx, tx, domain.SaveRuntimeCheckpointRequest{
			TaskID: task.TaskID, RunID: task.CurrentRunID, ExecutionVersion: task.CurrentExecutionVersion,
			ExecutionConfigHash: harness.hash,
		})
	})
	if err != nil {
		t.Fatal(err)
	}
}

func prepareGeneratePlanRecoveryStart(
	t *testing.T,
	environment *postgrestest.RepositoryEnvironment,
	harness *realCheckpointTaskRuntime,
	taskID contracts.TaskID,
) {
	t.Helper()
	task, run, execution := loadTaskRuntimeFacts(t, harness, taskID)
	rows := loadCheckpointRows(t, environment, taskID)
	if len(rows) != 2 || rows[1].checkpointID == "" {
		t.Fatalf("Recovery source Checkpoints = %#v", rows)
	}
	contextDocument, err := json.Marshal(contracts.RuntimeContextV1{
		SchemaVersion: 1, TaskID: taskID, RunID: run.RunID, ExecutionVersion: 2,
		NextAction:         contracts.CheckpointNextActionGeneratePlan,
		ResolvedReferences: contracts.CanonicalResolvedReferences{},
	})
	if err != nil {
		t.Fatal(err)
	}
	connection := postgrestest.Connect(t, environment.Database.DSN)
	tx, err := connection.Begin(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	if _, err := tx.Exec(context.Background(), `
UPDATE task_execution
SET status='INTERRUPTED', error_code='WORKER_INTERRUPTED', ended_at=transaction_timestamp()
WHERE task_id=$1 AND execution_version=1`, taskID); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(context.Background(), `
INSERT INTO task_execution (
    task_execution_id, task_id, execution_version, status, execution_config_hash, created_at
) VALUES ($1, $2, 2, 'QUEUED', $3, transaction_timestamp())`,
		"execution-recovery-"+string(taskID), taskID, harness.hash); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(context.Background(), `
UPDATE task
SET status='Pending', current_execution_version=2, queued_at=transaction_timestamp(), error_code=NULL
WHERE task_id=$1`, taskID); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(context.Background(), `
UPDATE run SET status='Pending', error_code=NULL WHERE task_id=$1 AND run_id=$2`, taskID, run.RunID); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(context.Background(), `
INSERT INTO checkpoint (
    checkpoint_id, task_id, run_id, execution_version, checkpoint_sequence,
    runtime_context, execution_config_hash, source_execution_version,
    source_checkpoint_id, created_at

) VALUES ($1, $2, $3, 2, 3, $4::jsonb, $5, NULL, NULL, transaction_timestamp())`,
		"checkpoint-superseded-"+string(taskID), taskID, run.RunID, string(contextDocument), strings.Repeat("b", 64)); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(context.Background(), `
INSERT INTO checkpoint (
    checkpoint_id, task_id, run_id, execution_version, checkpoint_sequence,
    runtime_context, execution_config_hash, source_execution_version,
    source_checkpoint_id, created_at
) VALUES ($1, $2, $3, 2, 4, $4::jsonb, $5, 1, $6, transaction_timestamp())`,
		"checkpoint-recovery-"+string(taskID), taskID, run.RunID, string(contextDocument), harness.hash,
		rows[1].checkpointID); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(context.Background()); err != nil {
		t.Fatal(err)
	}
	_ = task
	_ = execution
}

func testClaimCandidateVersionMismatch(t *testing.T, environment *postgrestest.RepositoryEnvironment) {
	base := newRealCheckpointTaskRuntime(t, environment, nil)
	created := base.create(t, "command-version-mismatch")
	replaceClaimTaskRepository(t, base, candidateVersionTaskRepository{TaskRepository: base.repositories.Tasks, version: 2})
	result, err := base.claim.ClaimNextExecution(context.Background(), base.workerID)
	if err != nil {
		t.Fatalf("version mismatch Claim error = %v", err)
	}
	if _, ok := result.(contracts.ClaimResultDataInconsistentTerminalized); !ok {
		t.Fatalf("version mismatch result = %T", result)
	}
	task, run, execution := loadTaskRuntimeFacts(t, base, created.TaskID)
	if task.Status != contracts.TaskStatusFailed || run.Status != contracts.RunStatusFailed ||
		execution.Status != contracts.TaskExecutionStatusFailed || execution.WorkerID != nil ||
		task.CurrentExecutionVersion != 1 || task.QueuedAt != nil {
		t.Fatalf("version mismatch left partial Claim facts = task:%+v run:%+v execution:%+v", task, run, execution)
	}
	if rows := loadCheckpointRows(t, environment, created.TaskID); len(rows) != 1 {
		t.Fatalf("version mismatch changed Checkpoints = %#v", rows)
	}
}

func claimTask(t *testing.T, harness *realCheckpointTaskRuntime, taskID contracts.TaskID) contracts.ExecutionClaim {
	t.Helper()
	result, err := harness.claim.ClaimNextExecution(context.Background(), harness.workerID)
	claimed, ok := result.(contracts.ClaimResultClaimed)
	if err != nil || !ok || claimed.Claim.TaskID != taskID {
		t.Fatalf("ClaimNextExecution() = %#v, %v", result, err)
	}
	return claimed.Claim
}

func replaceClaimTaskRepository(t *testing.T, harness *realCheckpointTaskRuntime, tasks domain.TaskRepository) {
	t.Helper()
	service, err := domain.NewClaimTaskService(domain.ClaimTaskDependencies{
		Executor: harness.executor, Tasks: tasks, Runs: harness.repositories.Runs,
		Executions: harness.repositories.Executions, TaskLogs: harness.repositories.TaskLogs,
		Clock: harness.repositories.Clock, Configs: harness.configs, Checkpoints: harness.checkpoints,
		Reports: noOpPendingReportWriter{}, Policy: lifecycle.New(), RuntimeWorker: harness.workerID,
	})
	if err != nil {
		t.Fatal(err)
	}
	harness.claim = service
}

func loadTaskRuntimeFacts(
	t *testing.T,
	harness *realCheckpointTaskRuntime,
	taskID contracts.TaskID,
) (domain.Task, domain.Run, domain.TaskExecution) {
	t.Helper()
	task, err := harness.repositories.Tasks.Find(context.Background(), taskID)
	if err != nil {
		t.Fatal(err)
	}
	run, err := harness.repositories.Runs.FindByTask(context.Background(), taskID)
	if err != nil {
		t.Fatal(err)
	}
	execution, err := harness.repositories.Executions.FindByTaskVersion(
		context.Background(), taskID, task.CurrentExecutionVersion,
	)
	if err != nil {
		t.Fatal(err)
	}
	return task, run, execution
}

func requeueGeneratePlanContinuation(
	t *testing.T,
	environment *postgrestest.RepositoryEnvironment,
	harness *realCheckpointTaskRuntime,
	task domain.Task,
	run domain.Run,
	execution domain.TaskExecution,
) {
	t.Helper()
	queuedAt := task.CreatedAt.Add(1)
	err := environment.Runtime.WriteExecutor().Execute(context.Background(), func(ctx context.Context, tx contracts.RuntimeWriteTx) error {
		updated, err := harness.repositories.Executions.Update(ctx, tx, domain.TaskExecutionUpdate{
			TaskID: execution.TaskID, ExecutionVersion: execution.ExecutionVersion,
			ExpectedStatus: contracts.TaskExecutionStatusRunning, ExpectedWorkerID: execution.WorkerID,
			Status: contracts.TaskExecutionStatusQueued, StartedAt: execution.StartedAt,
		})
		if err != nil || !updated {
			return fmt.Errorf("requeue Execution = %v: %w", updated, err)
		}
		updated, err = harness.repositories.Tasks.Update(ctx, tx, domain.TaskUpdate{
			TaskID: task.TaskID, ExpectedStatus: contracts.TaskStatusRunning,
			ExpectedCurrentExecutionVersion: task.CurrentExecutionVersion,
			Status:                          contracts.TaskStatusRunning, CurrentExecutionVersion: task.CurrentExecutionVersion,
			QueuedAt: &queuedAt, StartedAt: task.StartedAt,
		})
		if err != nil || !updated {
			return fmt.Errorf("requeue Task = %v: %w", updated, err)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	_ = run
}

func setStartedQueuedContinuation(
	t *testing.T,
	environment *postgrestest.RepositoryEnvironment,
	harness *realCheckpointTaskRuntime,
	taskID contracts.TaskID,
) {
	t.Helper()
	err := environment.Runtime.WriteExecutor().Execute(context.Background(), func(ctx context.Context, tx contracts.RuntimeWriteTx) error {
		now, err := harness.repositories.Clock.Now(ctx, tx)
		if err != nil {
			return err
		}
		task, err := harness.repositories.Tasks.Lock(ctx, tx, taskID)
		if err != nil {
			return err
		}
		run, err := harness.repositories.Runs.LockByTask(ctx, tx, taskID)
		if err != nil {
			return err
		}
		execution, err := harness.repositories.Executions.LockByTaskVersion(ctx, tx, taskID, 1)
		if err != nil {
			return err
		}
		updated, err := harness.repositories.Executions.Update(ctx, tx, domain.TaskExecutionUpdate{
			TaskID: taskID, ExecutionVersion: 1, ExpectedStatus: contracts.TaskExecutionStatusQueued,
			Status: contracts.TaskExecutionStatusQueued, StartedAt: &now,
		})
		if err != nil || !updated {
			return fmt.Errorf("mark Execution started = %v: %w", updated, err)
		}
		updated, err = harness.repositories.Runs.Update(ctx, tx, domain.RunUpdate{
			TaskID: taskID, RunID: run.RunID, ExecutionVersion: 1,
			ExpectedStatus: contracts.RunStatusPending, Status: contracts.RunStatusPending,
			Context: run.Context, StartedAt: &now,
		})
		if err != nil || !updated {
			return fmt.Errorf("mark Run started = %v: %w", updated, err)
		}
		updated, err = harness.repositories.Tasks.Update(ctx, tx, domain.TaskUpdate{
			TaskID: taskID, ExpectedStatus: contracts.TaskStatusPending,
			ExpectedCurrentExecutionVersion: 1, Status: contracts.TaskStatusPending,
			CurrentExecutionVersion: 1, QueuedAt: task.QueuedAt, StartedAt: &now,
		})
		if err != nil || !updated {
			return fmt.Errorf("mark Task started = %v: %w", updated, err)
		}
		_ = execution
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func mutateExecutionHash(t *testing.T, environment *postgrestest.RepositoryEnvironment, _ *realCheckpointTaskRuntime, taskID contracts.TaskID) {
	t.Helper()
	execTestSQL(t, environment, `UPDATE task_execution SET execution_config_hash=$2 WHERE task_id=$1`, taskID, strings.Repeat("b", 64))
}

func mutateCheckpointHash(t *testing.T, environment *postgrestest.RepositoryEnvironment, _ *realCheckpointTaskRuntime, taskID contracts.TaskID) {
	t.Helper()
	execTestSQL(t, environment, `UPDATE checkpoint SET execution_config_hash=$2 WHERE task_id=$1`, taskID, strings.Repeat("b", 64))
}

func mutateCurrentConfigHash(t *testing.T, _ *postgrestest.RepositoryEnvironment, harness *realCheckpointTaskRuntime, _ contracts.TaskID) {
	t.Helper()
	changed := harness.config
	changed.ExecutionConfig.Agent.SystemInstruction += " changed"
	harness.configs.config = changed
}

func execTestSQL(t *testing.T, environment *postgrestest.RepositoryEnvironment, statement string, arguments ...any) {
	t.Helper()
	connection := postgrestest.Connect(t, environment.Database.DSN)
	if _, err := connection.Exec(context.Background(), statement, arguments...); err != nil {
		t.Fatal(err)
	}
}

func testClaimThreeWayHashGuards(t *testing.T, environment *postgrestest.RepositoryEnvironment) {
	tests := []struct {
		name   string
		mutate func(*testing.T, *postgrestest.RepositoryEnvironment, *realCheckpointTaskRuntime, contracts.TaskID)
	}{
		{name: "TaskExecution", mutate: mutateExecutionHash},
		{name: "Checkpoint", mutate: mutateCheckpointHash},
		{name: "current config", mutate: mutateCurrentConfigHash},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			harness := newRealCheckpointTaskRuntime(t, environment, nil)
			created := harness.create(t, domain.CommandID("command-hash-"+strings.ToLower(test.name)))
			test.mutate(t, environment, harness, created.TaskID)
			result, err := harness.claim.ClaimNextExecution(context.Background(), harness.workerID)
			if err != nil {
				t.Fatalf("hash mismatch Claim error = %v", err)
			}
			switch result.(type) {
			case contracts.ClaimResultConfigMismatchInterrupted, contracts.ClaimResultCheckpointInvalidTerminalized:
			default:
				t.Fatalf("hash mismatch result = %T", result)
			}
			task, _, execution := loadTaskRuntimeFacts(t, harness, created.TaskID)
			if execution.WorkerID != nil || task.QueuedAt != nil ||
				execution.Status != contracts.TaskExecutionStatusInterrupted && execution.Status != contracts.TaskExecutionStatusFailed {
				t.Fatalf("hash mismatch passed Claim or partially wrote: task=%+v execution=%+v", task, execution)
			}
			if rows := loadCheckpointRows(t, environment, created.TaskID); len(rows) != 1 {
				t.Fatalf("hash mismatch created Checkpoint = %#v", rows)
			}
		})
	}
}

func testContinuationCheckpointInvalidNoFallback(t *testing.T, environment *postgrestest.RepositoryEnvironment) {
	harness := newRealCheckpointTaskRuntime(t, environment, nil)
	created := harness.create(t, "command-initialization-continuation")
	setStartedQueuedContinuation(t, environment, harness, created.TaskID)
	result, err := harness.claim.ClaimNextExecution(context.Background(), harness.workerID)
	if err != nil {
		t.Fatalf("Initialization continuation Claim error = %v", err)
	}
	if _, ok := result.(contracts.ClaimResultCheckpointInvalidTerminalized); !ok {
		t.Fatalf("Initialization continuation result = %T", result)
	}
	_, _, execution := loadTaskRuntimeFacts(t, harness, created.TaskID)
	if execution.Status != contracts.TaskExecutionStatusFailed || execution.WorkerID != nil {
		t.Fatalf("Initialization continuation Execution = %+v", execution)
	}
	if rows := loadCheckpointRows(t, environment, created.TaskID); len(rows) != 1 || rows[0].sequence != 1 {
		t.Fatalf("Continuation fell back or changed Checkpoints = %#v", rows)
	}
}

func testContinuationDamagedLatestNoFallback(t *testing.T, environment *postgrestest.RepositoryEnvironment) {
	harness := newRealCheckpointTaskRuntime(t, environment, nil)
	created := harness.create(t, "command-damaged-latest")
	claimTask(t, harness, created.TaskID)
	task, run, execution := loadTaskRuntimeFacts(t, harness, created.TaskID)
	appendGeneratePlanExecutionCheckpoint(t, environment, harness, task)
	execTestSQL(t, environment, `
UPDATE checkpoint
SET runtime_context=jsonb_set(runtime_context, '{schema_version}', '9'::jsonb)
WHERE task_id=$1 AND checkpoint_sequence=3`, created.TaskID)
	requeueGeneratePlanContinuation(t, environment, harness, task, run, execution)

	result, err := harness.claim.ClaimNextExecution(context.Background(), harness.workerID)
	if err != nil {
		t.Fatalf("damaged latest Claim error = %v", err)
	}
	if _, ok := result.(contracts.ClaimResultCheckpointInvalidTerminalized); !ok {
		t.Fatalf("damaged latest result = %T", result)
	}
	_, _, after := loadTaskRuntimeFacts(t, harness, created.TaskID)
	if after.Status != contracts.TaskExecutionStatusFailed || after.WorkerID != nil {
		t.Fatalf("damaged latest fell back to valid sequence 2: %+v", after)
	}
	if rows := loadCheckpointRows(t, environment, created.TaskID); len(rows) != 3 {
		t.Fatalf("damaged latest changed Checkpoints = %#v", rows)
	}
}

func TestCreateCheckpointFailureRollsBackRealPostgreSQLTransaction(t *testing.T) {
	postgrestest.RunRepositoryContract(t, postgrestest.RepositoryContract{
		Name:       "Create Checkpoint rollback",
		Migrations: taskruntimemigrations.Migrations(),
		Cases:      []postgrestest.RepositoryCase{{Name: "missing Checkpoint table rolls back all Create facts", Run: testCreateCheckpointFailureRollback}},
	})
}

func testCreateAndFirstClaimCheckpoint(t *testing.T, environment *postgrestest.RepositoryEnvironment) {
	harness := newRealCheckpointTaskRuntime(t, environment, nil)
	created := harness.create(t, "command-real-checkpoint")

	initial := loadCheckpointRows(t, environment, created.TaskID)
	if len(initial) != 1 || initial[0].sequence != 1 || initial[0].version != 1 ||
		initial[0].context.NextAction != contracts.CheckpointNextActionGeneratePlan ||
		initial[0].context.PlanID != nil || initial[0].context.CurrentStepID != nil ||
		initial[0].hash != harness.hash {
		t.Fatalf("Initialization Checkpoint = %#v", initial)
	}

	result, err := harness.claim.ClaimNextExecution(context.Background(), harness.workerID)
	claimed, ok := result.(contracts.ClaimResultClaimed)
	if err != nil || !ok || claimed.Claim.TaskID != created.TaskID || claimed.Claim.ExecutionVersion != 1 {
		t.Fatalf("ClaimNextExecution() = %#v, %v", result, err)
	}
	task, err := harness.repositories.Tasks.Find(context.Background(), created.TaskID)
	if err != nil {
		t.Fatal(err)
	}
	run, err := harness.repositories.Runs.FindByTask(context.Background(), created.TaskID)
	if err != nil {
		t.Fatal(err)
	}
	execution, err := harness.repositories.Executions.FindByTaskVersion(context.Background(), created.TaskID, 1)
	if err != nil {
		t.Fatal(err)
	}
	if task.Status != contracts.TaskStatusRunning || task.QueuedAt != nil || run.Status != contracts.RunStatusRunning ||
		execution.Status != contracts.TaskExecutionStatusRunning || execution.WorkerID == nil ||
		*execution.WorkerID != harness.workerID || execution.ExecutionConfigHash != harness.hash {
		t.Fatalf("claimed facts = task:%+v run:%+v execution:%+v", task, run, execution)
	}
	checkpoints := loadCheckpointRows(t, environment, created.TaskID)
	if len(checkpoints) != 2 || checkpoints[0].sequence != 1 || checkpoints[1].sequence != 2 ||
		checkpoints[1].version != 1 || checkpoints[1].context.NextAction != contracts.CheckpointNextActionGeneratePlan ||
		checkpoints[1].hash != execution.ExecutionConfigHash {
		t.Fatalf("Create/Claim Checkpoints = %#v", checkpoints)
	}
	result, err = harness.claim.ClaimNextExecution(context.Background(), harness.workerID)
	if _, ok := result.(contracts.ClaimResultNoWork); err != nil || !ok {
		t.Fatalf("second ClaimNextExecution() = %#v, %v", result, err)
	}
}

func testClaimCheckpointFailureRollback(t *testing.T, environment *postgrestest.RepositoryEnvironment) {
	base := newCheckpointAdapter(t)
	harness := newRealCheckpointTaskRuntime(t, environment, &failingGeneratePlanCheckpointPort{
		RuntimeCheckpointPort: base, fail: errors.New("injected Execution Checkpoint failure"),
	})
	created := harness.create(t, "command-claim-rollback")
	result, err := harness.claim.ClaimNextExecution(context.Background(), harness.workerID)
	if err == nil || result != nil || !strings.Contains(err.Error(), "injected Execution Checkpoint failure") {
		t.Fatalf("ClaimNextExecution() = %#v, %v", result, err)
	}
	assertPendingCreateFacts(t, harness, created.TaskID)
	checkpoints := loadCheckpointRows(t, environment, created.TaskID)
	if len(checkpoints) != 1 || checkpoints[0].sequence != 1 {
		t.Fatalf("failed Claim persisted Checkpoint = %#v", checkpoints)
	}
}

func testClaimWorkerGuard(t *testing.T, environment *postgrestest.RepositoryEnvironment) {
	harness := newRealCheckpointTaskRuntime(t, environment, nil)
	created := harness.create(t, "command-claim-guards")
	if result, err := harness.claim.ClaimNextExecution(context.Background(), "other-worker"); !errors.Is(err, domain.ErrInvalidArgument) || result != nil {
		t.Fatalf("wrong worker Claim = %#v, %v", result, err)
	}
	assertPendingCreateFacts(t, harness, created.TaskID)
}

func testCreateCheckpointFailureRollback(t *testing.T, environment *postgrestest.RepositoryEnvironment) {
	harness := newRealCheckpointTaskRuntime(t, environment, nil)
	if _, err := harness.createService.CreateTask(context.Background(), domain.CreateTaskRequest{
		CommandID: "command-create-rollback", AgentID: "agent-default", TaskInput: "inspect", OperatorID: "operator-1",
	}); err == nil {
		t.Fatal("CreateTask() error = nil, want missing Checkpoint table failure")
	}
	connection := postgrestest.Connect(t, environment.Database.DSN)
	for _, table := range []string{"task", "run", "task_execution", "command_receipt", "task_log"} {
		var count int
		if err := connection.QueryRow(context.Background(), "SELECT count(*) FROM "+table).Scan(&count); err != nil || count != 0 {
			t.Fatalf("%s rows after failed Create = %d, err=%v", table, count, err)
		}
	}
}

type realCheckpointTaskRuntime struct {
	createService *domain.CreateTaskService
	claim         *domain.ClaimTaskService
	executor      contracts.RuntimeWriteExecutor
	repositories  *postgrestaskruntime.Repositories
	checkpoints   domain.RuntimeCheckpointPort
	configs       *integrationAgentConfigSource
	config        domain.AgentRuntimeConfig
	hash          contracts.ExecutionConfigHash
	workerID      contracts.WorkerID
}

func newRealCheckpointTaskRuntime(
	t *testing.T,
	environment *postgrestest.RepositoryEnvironment,
	checkpointPort domain.RuntimeCheckpointPort,
) *realCheckpointTaskRuntime {
	t.Helper()
	repositories := postgrestaskruntime.New(environment.Runtime.ReadPool())
	if checkpointPort == nil {
		checkpointPort = newCheckpointAdapter(t)
	}
	config := integrationAgentConfig(t)
	hash, err := domain.HashExecutionConfigV1(config.ExecutionConfig)
	if err != nil {
		t.Fatal(err)
	}
	configs := &integrationAgentConfigSource{config: config}
	workerID := contracts.WorkerID("checkpoint-worker")
	createService, err := domain.NewCreateTaskService(domain.CreateTaskDependencies{
		Executor: environment.Runtime.WriteExecutor(), Tasks: repositories.Tasks, Runs: repositories.Runs,
		Executions: repositories.Executions, Receipts: repositories.Receipts, TaskLogs: repositories.TaskLogs,
		Clock: repositories.Clock, Configs: configs, Checkpoints: checkpointPort, Policy: lifecycle.New(),
	})
	if err != nil {
		t.Fatal(err)
	}
	claimService, err := domain.NewClaimTaskService(domain.ClaimTaskDependencies{
		Executor: environment.Runtime.WriteExecutor(), Tasks: repositories.Tasks, Runs: repositories.Runs,
		Executions: repositories.Executions, TaskLogs: repositories.TaskLogs, Clock: repositories.Clock,
		Configs: configs, Checkpoints: checkpointPort, Reports: noOpPendingReportWriter{},
		Policy: lifecycle.New(), RuntimeWorker: workerID,
	})
	if err != nil {
		t.Fatal(err)
	}
	return &realCheckpointTaskRuntime{
		createService: createService, claim: claimService, executor: environment.Runtime.WriteExecutor(),
		repositories: repositories, checkpoints: checkpointPort, configs: configs,
		config: config, hash: hash, workerID: workerID,
	}
}

func newCheckpointAdapter(t *testing.T) domain.RuntimeCheckpointPort {
	t.Helper()
	manager := newCheckpointManager(t)
	adapter, err := checkpoint.NewTaskRuntimeAdapter(manager)
	if err != nil {
		t.Fatal(err)
	}
	return adapter
}

func newCheckpointManager(t *testing.T) *checkpoint.Manager {
	t.Helper()
	codec, err := checkpoint.NewRuntimeContextCodec(checkpoint.RuntimeContextCodecLimits{MaxBytes: 64 * 1024, MaxDepth: 32})
	if err != nil {
		t.Fatal(err)
	}
	manager, err := checkpoint.NewManager(postgrescheckpoint.New(), codec)
	if err != nil {
		t.Fatal(err)
	}
	return manager
}

func integrationAgentConfig(t *testing.T) domain.AgentRuntimeConfig {
	t.Helper()
	loaded, err := business.Load("../../../configs/business.json")
	if err != nil {
		t.Fatal(err)
	}
	agent, ok := loaded.Lookup("agent-default")
	if !ok {
		t.Fatal("agent-default is missing")
	}
	return domain.AgentRuntimeConfig{TaskTimeout: agent.TaskTimeout, ExecutionConfig: agent.ExecutionConfig}
}

func (h *realCheckpointTaskRuntime) create(t *testing.T, commandID domain.CommandID) domain.TaskCreated {
	t.Helper()
	created, err := h.createService.CreateTask(context.Background(), domain.CreateTaskRequest{
		CommandID: commandID, AgentID: "agent-default", TaskInput: "inspect", OperatorID: "operator-1",
	})
	if err != nil {
		t.Fatalf("CreateTask() error = %v", err)
	}
	return created
}

func assertPendingCreateFacts(t *testing.T, harness *realCheckpointTaskRuntime, taskID contracts.TaskID) {
	t.Helper()
	task, err := harness.repositories.Tasks.Find(context.Background(), taskID)
	if err != nil {
		t.Fatal(err)
	}
	run, err := harness.repositories.Runs.FindByTask(context.Background(), taskID)
	if err != nil {
		t.Fatal(err)
	}
	execution, err := harness.repositories.Executions.FindByTaskVersion(context.Background(), taskID, 1)
	if err != nil {
		t.Fatal(err)
	}
	if task.Status != contracts.TaskStatusPending || task.QueuedAt == nil ||
		run.Status != contracts.RunStatusPending || execution.Status != contracts.TaskExecutionStatusQueued ||
		execution.WorkerID != nil || task.CurrentExecutionVersion != 1 {
		t.Fatalf("pending Create facts = task:%+v run:%+v execution:%+v", task, run, execution)
	}
}

type checkpointRow struct {
	checkpointID  contracts.CheckpointID
	sequence      int64
	version       contracts.ExecutionVersion
	hash          contracts.ExecutionConfigHash
	sourceVersion *contracts.ExecutionVersion
	context       contracts.RuntimeContextV1
}

func loadCheckpointRows(t *testing.T, environment *postgrestest.RepositoryEnvironment, taskID contracts.TaskID) []checkpointRow {
	t.Helper()
	connection := postgrestest.Connect(t, environment.Database.DSN)
	rows, err := connection.Query(context.Background(), `
SELECT checkpoint_id, checkpoint_sequence, execution_version, execution_config_hash,
       source_execution_version, runtime_context
FROM checkpoint WHERE task_id=$1 ORDER BY checkpoint_sequence`, taskID)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var result []checkpointRow
	for rows.Next() {
		var row checkpointRow
		var document []byte
		if err := rows.Scan(&row.checkpointID, &row.sequence, &row.version, &row.hash, &row.sourceVersion, &document); err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal(document, &row.context); err != nil {
			t.Fatal(err)
		}
		result = append(result, row)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return result
}

type integrationAgentConfigSource struct{ config domain.AgentRuntimeConfig }

func (s *integrationAgentConfigSource) LookupAgent(agentID contracts.AgentID) (domain.AgentRuntimeConfig, bool) {
	return s.config, agentID == s.config.ExecutionConfig.Agent.AgentID
}

type noOpPendingReportWriter struct{}

func (noOpPendingReportWriter) EnsurePending(context.Context, contracts.RuntimeWriteTx, contracts.EnsurePendingReportRequest) (contracts.EnsurePendingReportResult, error) {
	return contracts.EnsurePendingReportCreated{}, nil
}

type failingGeneratePlanCheckpointPort struct {
	domain.RuntimeCheckpointPort
	fail error
}

type candidateVersionTaskRepository struct {
	domain.TaskRepository
	version contracts.ExecutionVersion
}

func (r candidateVersionTaskRepository) LockNextQueueCandidate(
	ctx context.Context,
	tx contracts.RuntimeWriteTx,
) (domain.QueueCandidate, error) {
	candidate, err := r.TaskRepository.LockNextQueueCandidate(ctx, tx)
	if err != nil {
		return domain.QueueCandidate{}, err
	}
	candidate.ExecutionVersion = r.version
	return candidate, nil
}

func (p *failingGeneratePlanCheckpointPort) SaveGeneratePlanExecutionCheckpoint(context.Context, contracts.RuntimeWriteTx, domain.SaveRuntimeCheckpointRequest) error {
	return fmt.Errorf("save injected Checkpoint: %w", p.fail)
}

var (
	_ domain.AgentConfigSource      = (*integrationAgentConfigSource)(nil)
	_ contracts.PendingReportWriter = noOpPendingReportWriter{}
	_ domain.RuntimeCheckpointPort  = (*failingGeneratePlanCheckpointPort)(nil)
)
