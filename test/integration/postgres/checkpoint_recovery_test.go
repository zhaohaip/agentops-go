package postgres_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	postgrescheckpoint "github.com/zhaohaip/agentops-go/internal/adapter/postgres/checkpoint"
	"github.com/zhaohaip/agentops-go/internal/checkpoint"
	"github.com/zhaohaip/agentops-go/internal/contracts"
	domain "github.com/zhaohaip/agentops-go/internal/taskruntime"
	"github.com/zhaohaip/agentops-go/test/integration/postgres/postgrestest"
)

func TestGeneratePlanRecoveryStartUsesRealPostgreSQL(t *testing.T) {
	postgrestest.RunRepositoryContract(t, postgrestest.RepositoryContract{
		Name:       "Checkpoint GENERATE_PLAN Recovery",
		Migrations: checkpointMigrationSet(),
		Cases: []postgrestest.RepositoryCase{
			{Name: "validates maximum and creates direct next version", Run: testGeneratePlanRecoveryStart},
			{Name: "Recovery Start config mismatch before Claim remains recoverable", Run: testRecoveryStartConfigMismatchBeforeClaim},
			{Name: "failure rolls back double-version facts", Run: testGeneratePlanRecoveryRollback},
		},
	})
}

func testRecoveryStartConfigMismatchBeforeClaim(t *testing.T, environment *postgrestest.RepositoryEnvironment) {
	harness := newRealCheckpointTaskRuntime(t, environment, nil)
	created := harness.create(t, "command-recovery-config-mismatch")
	claimTask(t, harness, created.TaskID)
	prepareGeneratePlanRecoveryStart(t, environment, harness, created.TaskID)
	manager := newCheckpointManager(t)

	err := environment.Runtime.WriteExecutor().Execute(context.Background(), func(ctx context.Context, tx contracts.RuntimeWriteTx) error {
		task, err := harness.repositories.Tasks.Lock(ctx, tx, created.TaskID)
		if err != nil {
			return err
		}
		run, err := harness.repositories.Runs.LockByTask(ctx, tx, created.TaskID)
		if err != nil {
			return err
		}
		execution, err := harness.repositories.Executions.LockByTaskVersion(ctx, tx, created.TaskID, 2)
		if err != nil {
			return err
		}
		if execution.StartedAt != nil {
			return errors.New("Recovery Start execution unexpectedly started")
		}
		now, err := harness.repositories.Clock.Now(ctx, tx)
		if err != nil {
			return err
		}
		observed := contracts.ExecutionConfigHash(strings.Repeat("b", 64))
		errorCode := contracts.ErrorCodeConfigVersionMismatch
		updated, err := harness.repositories.Executions.Update(ctx, tx, domain.TaskExecutionUpdate{
			TaskID: created.TaskID, ExecutionVersion: 2, ExpectedStatus: contracts.TaskExecutionStatusQueued,
			Status: contracts.TaskExecutionStatusInterrupted, ObservedConfigHash: &observed,
			ErrorCode: &errorCode, EndedAt: &now,
		})
		if err != nil || !updated {
			return errors.New("interrupt Recovery Start Execution")
		}
		updated, err = harness.repositories.Tasks.Update(ctx, tx, domain.TaskUpdate{
			TaskID: created.TaskID, ExpectedStatus: contracts.TaskStatusPending, ExpectedCurrentExecutionVersion: 2,
			Status: contracts.TaskStatusInterrupted, CurrentExecutionVersion: 2, ErrorCode: &errorCode,
		})
		if err != nil || !updated {
			return errors.New("interrupt Recovery Start Task")
		}
		result, err := manager.ValidateRecoverySource(ctx, tx, checkpoint.RecoverySourceQuery{
			TaskID: created.TaskID, RunID: run.RunID, SourceExecutionVersion: 2,
			Phase: checkpoint.RecoverySourceBeforeFirstExecution,
		})
		if _, ok := result.(checkpoint.ValidatedRecoverySource); err != nil || !ok {
			return errors.New("validate unstarted Recovery Start config mismatch")
		}
		_ = task
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}

	task, run, execution := loadTaskRuntimeFacts(t, harness, created.TaskID)
	if task.Status != contracts.TaskStatusInterrupted || task.QueuedAt != nil || run.Status != contracts.RunStatusPending ||
		execution.Status != contracts.TaskExecutionStatusInterrupted || execution.StartedAt != nil ||
		execution.ErrorCode == nil || *execution.ErrorCode != contracts.ErrorCodeConfigVersionMismatch {
		t.Fatalf("config mismatch facts task=%+v run=%+v execution=%+v", task, run, execution)
	}
}

func testGeneratePlanRecoveryStart(t *testing.T, environment *postgrestest.RepositoryEnvironment) {
	harness := newRealCheckpointTaskRuntime(t, environment, nil)
	created := harness.create(t, "command-recovery-source")
	manager := newCheckpointManager(t)

	err := environment.Runtime.WriteExecutor().Execute(context.Background(), func(ctx context.Context, tx contracts.RuntimeWriteTx) error {
		task, err := harness.repositories.Tasks.Lock(ctx, tx, created.TaskID)
		if err != nil {
			return err
		}
		run, err := harness.repositories.Runs.LockByTask(ctx, tx, created.TaskID)
		if err != nil {
			return err
		}
		execution, err := harness.repositories.Executions.LockByTaskVersion(ctx, tx, created.TaskID, 1)
		if err != nil {
			return err
		}
		now, err := harness.repositories.Clock.Now(ctx, tx)
		if err != nil {
			return err
		}
		observed := contracts.ExecutionConfigHash(strings.Repeat("b", 64))
		errorCode := contracts.ErrorCodeConfigVersionMismatch
		updated, err := harness.repositories.Executions.Update(ctx, tx, domain.TaskExecutionUpdate{
			TaskID: created.TaskID, ExecutionVersion: 1, ExpectedStatus: contracts.TaskExecutionStatusQueued,
			Status: contracts.TaskExecutionStatusInterrupted, ObservedConfigHash: &observed,
			ErrorCode: &errorCode, EndedAt: &now,
		})
		if err != nil || !updated {
			return errors.New("interrupt source Execution")
		}
		updated, err = harness.repositories.Tasks.Update(ctx, tx, domain.TaskUpdate{
			TaskID: created.TaskID, ExpectedStatus: contracts.TaskStatusPending, ExpectedCurrentExecutionVersion: 1,
			Status: contracts.TaskStatusInterrupted, CurrentExecutionVersion: 1, ErrorCode: &errorCode,
		})
		if err != nil || !updated {
			return errors.New("interrupt source Task")
		}
		result, err := manager.ValidateRecoverySource(ctx, tx, checkpoint.RecoverySourceQuery{
			TaskID: created.TaskID, RunID: run.RunID, SourceExecutionVersion: 1,
			Phase: checkpoint.RecoverySourceBeforeFirstExecution,
		})
		validated, ok := result.(checkpoint.ValidatedRecoverySource)
		if err != nil || !ok {
			return errors.New("validate Initialization recovery source")
		}
		if err := harness.repositories.Executions.Insert(ctx, tx, domain.TaskExecution{
			TaskExecutionID: "execution-recovery-v2", TaskID: created.TaskID, ExecutionVersion: 2,
			Status: contracts.TaskExecutionStatusQueued, ExecutionConfigHash: execution.ExecutionConfigHash, CreatedAt: now,
		}); err != nil {
			return err
		}
		queuedAt := now
		updated, err = harness.repositories.Tasks.Update(ctx, tx, domain.TaskUpdate{
			TaskID: created.TaskID, ExpectedStatus: contracts.TaskStatusInterrupted, ExpectedCurrentExecutionVersion: 1,
			Status: contracts.TaskStatusPending, CurrentExecutionVersion: 2, QueuedAt: &queuedAt,
		})
		if err != nil || !updated {
			return errors.New("advance current execution version")
		}
		if _, err := manager.CreateRecoveryStart(ctx, tx, checkpoint.RuntimeRecoveryStartRequest{
			TaskID: created.TaskID, RunID: run.RunID, NewExecutionVersion: 2,
			ExecutionConfigHash: execution.ExecutionConfigHash, ValidatedSource: validated,
		}); err != nil {
			return err
		}
		_ = task
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}

	task, _, execution := loadTaskRuntimeFacts(t, harness, created.TaskID)
	rows := loadCheckpointRows(t, environment, created.TaskID)
	if task.Status != contracts.TaskStatusPending || task.CurrentExecutionVersion != 2 || task.QueuedAt == nil ||
		execution.ExecutionVersion != 2 || execution.Status != contracts.TaskExecutionStatusQueued || len(rows) != 2 ||
		rows[1].version != 2 || rows[1].sourceVersion == nil || *rows[1].sourceVersion != 1 ||
		rows[1].context.NextAction != contracts.CheckpointNextActionGeneratePlan {
		t.Fatalf("Recovery result task=%+v execution=%+v checkpoints=%#v", task, execution, rows)
	}
}

func testGeneratePlanRecoveryRollback(t *testing.T, environment *postgrestest.RepositoryEnvironment) {
	harness := newRealCheckpointTaskRuntime(t, environment, nil)
	created := harness.create(t, "command-recovery-rollback")
	manager, err := checkpoint.NewManager(postgrescheckpoint.New(), mustCheckpointCodec(t))
	if err != nil {
		t.Fatal(err)
	}
	injected := errors.New("rollback after Recovery Start")
	err = environment.Runtime.WriteExecutor().Execute(context.Background(), func(ctx context.Context, tx contracts.RuntimeWriteTx) error {
		// 完整成功路径由上一用例验证；这里在事务内插入 v2 后注入错误，证明外层事务拥有回滚边界。
		now, err := harness.repositories.Clock.Now(ctx, tx)
		if err != nil {
			return err
		}
		if err := harness.repositories.Executions.Insert(ctx, tx, domain.TaskExecution{
			TaskExecutionID: "execution-rollback-v2", TaskID: created.TaskID, ExecutionVersion: 2,
			Status: contracts.TaskExecutionStatusQueued, ExecutionConfigHash: harness.hash, CreatedAt: now,
		}); err != nil {
			return err
		}
		_ = manager
		return injected
	})
	if !errors.Is(err, injected) {
		t.Fatalf("rollback error = %v", err)
	}
	if _, err := harness.repositories.Executions.FindByTaskVersion(context.Background(), created.TaskID, 2); !errors.Is(err, domain.ErrRepositoryNotFound) {
		t.Fatalf("rolled back v2 error = %v", err)
	}
	if rows := loadCheckpointRows(t, environment, created.TaskID); len(rows) != 1 {
		t.Fatalf("rollback Checkpoints = %#v", rows)
	}
}

func mustCheckpointCodec(t *testing.T) checkpoint.RuntimeContextCodec {
	t.Helper()
	codec, err := checkpoint.NewRuntimeContextCodec(checkpoint.RuntimeContextCodecLimits{MaxBytes: 64 * 1024, MaxDepth: 32})
	if err != nil {
		t.Fatal(err)
	}
	return codec
}
