package postgres_test

import (
	"context"
	"encoding/json"
	"errors"
	"strconv"
	"testing"
	"time"

	postgresstep "github.com/zhaohaip/agentops-go/internal/adapter/postgres/stepexecutor"
	postgrestaskruntime "github.com/zhaohaip/agentops-go/internal/adapter/postgres/taskruntime"
	"github.com/zhaohaip/agentops-go/internal/contracts"
	stepdomain "github.com/zhaohaip/agentops-go/internal/stepexecutor"
	"github.com/zhaohaip/agentops-go/test/integration/postgres/postgrestest"
)

func TestStepRepositoryContract(t *testing.T) {
	postgrestest.RunRepositoryContract(t, postgrestest.RepositoryContract{
		Name:       "Step Executor",
		Migrations: stepMigrationSet(),
		Cases: []postgrestest.RepositoryCase{
			{Name: "batch insert update commit and read", Run: testStepRepositoryCommit},
			{Name: "Pending Step fails before start", Run: testStepRepositoryPendingFailure},
			{Name: "started at is immutable after start", Run: testStepRepositoryStartedAtImmutable},
			{Name: "illegal transitions are rejected", Run: testStepRepositoryIllegalTransitions},
			{Name: "transaction rollback", Run: testStepRepositoryRollback},
			{Name: "complete current execution guard", Run: testStepRepositoryConditionalGuard},
			{Name: "contiguous sequence query", Run: testStepRepositoryContiguousSequence},
			{Name: "opaque write token boundary", Run: testStepRepositoryWriteToken},
		},
	})
}

func testStepRepositoryCommit(t *testing.T, environment *postgrestest.RepositoryEnvironment) {
	repository, _, _, entities, guard, now := stepRepositoryFixture(t, environment, "commit", 2)
	startedAt := now.Add(time.Second)
	endedAt := startedAt.Add(time.Second)
	output := json.RawMessage(`{"result":"safe"}`)
	if err := environment.Runtime.WriteExecutor().Execute(context.Background(), func(ctx context.Context, tx contracts.RuntimeWriteTx) error {
		locked, err := repository.LockByID(ctx, tx, entities[0].StepID())
		if err != nil || locked.Status() != contracts.StepStatusPending {
			return errors.Join(err, errors.New("locked Step is not Pending"))
		}
		started, err := repository.Update(ctx, tx, stepUpdate(guard, entities[0], contracts.StepStatusPending, contracts.StepStatusRunning, nil, nil, &startedAt, nil))
		if err != nil || !started {
			return errors.Join(err, errors.New("start Step did not affect one row"))
		}
		completed, err := repository.Update(ctx, tx, stepUpdate(guard, entities[0], contracts.StepStatusRunning, contracts.StepStatusCompleted, output, nil, nil, &endedAt))
		if err != nil || !completed {
			return errors.Join(err, errors.New("complete Step did not affect one row"))
		}
		return nil
	}); err != nil {
		t.Fatalf("commit Step updates: %v", err)
	}

	stored, err := repository.FindByID(context.Background(), entities[0].StepID())
	if err != nil || stored.Status() != contracts.StepStatusCompleted || string(stored.Output()) != `{"result": "safe"}` && string(stored.Output()) != `{"result":"safe"}` ||
		stored.StartedAt() == nil || !stored.StartedAt().Equal(startedAt) || stored.EndedAt() == nil || !stored.EndedAt().Equal(endedAt) {
		t.Fatalf("stored Step = (%#v, output=%s, err=%v)", stored, stored.Output(), err)
	}
	steps, err := repository.ListByRun(context.Background(), entities[0].RunID())
	if err != nil || len(steps) != 2 || steps[0].Sequence() != 1 || steps[1].Sequence() != 2 {
		t.Fatalf("ListByRun() = (%v, %v)", steps, err)
	}
}

func testStepRepositoryPendingFailure(t *testing.T, environment *postgrestest.RepositoryEnvironment) {
	repository, _, _, entities, guard, now := stepRepositoryFixture(t, environment, "pending-failure", 1)
	errorCode := contracts.ErrorCodeTaskCancelled
	endedAt := now.Add(time.Second)
	if err := environment.Runtime.WriteExecutor().Execute(context.Background(), func(ctx context.Context, tx contracts.RuntimeWriteTx) error {
		updated, err := repository.Update(ctx, tx, stepUpdate(
			guard, entities[0], contracts.StepStatusPending, contracts.StepStatusFailed,
			nil, &errorCode, nil, &endedAt,
		))
		if err != nil || !updated {
			return errors.Join(err, errors.New("Pending to Failed did not affect one row"))
		}
		return nil
	}); err != nil {
		t.Fatalf("fail Pending Step: %v", err)
	}
	stored, err := repository.FindByID(context.Background(), entities[0].StepID())
	if err != nil || stored.Status() != contracts.StepStatusFailed || stored.StartedAt() != nil ||
		stored.EndedAt() == nil || !stored.EndedAt().Equal(endedAt) || stored.ErrorCode() == nil || *stored.ErrorCode() != errorCode {
		t.Fatalf("Pending to Failed Step = (%#v, %v)", stored, err)
	}
}

func testStepRepositoryStartedAtImmutable(t *testing.T, environment *postgrestest.RepositoryEnvironment) {
	repository, taskRepositories, graph, entities, guard, now := stepRepositoryFixture(t, environment, "started-at", 2)
	startedAt := now.Add(time.Second)
	overwriteAt := startedAt.Add(time.Hour)
	completedAt := startedAt.Add(2 * time.Second)
	output := json.RawMessage(`{"result":"safe"}`)

	applyStepUpdate(t, environment, repository, stepUpdate(
		guard, entities[0], contracts.StepStatusPending, contracts.StepStatusRunning,
		nil, nil, &startedAt, nil,
	))
	assertStepUpdateRejected(t, environment, repository, stepUpdate(
		guard, entities[0], contracts.StepStatusRunning, contracts.StepStatusWaitingApproval,
		nil, nil, &overwriteAt, nil,
	))
	applyStepUpdate(t, environment, repository, stepUpdate(
		guard, entities[0], contracts.StepStatusRunning, contracts.StepStatusWaitingApproval,
		nil, nil, nil, nil,
	))
	assertStepStartedAt(t, repository, entities[0].StepID(), contracts.StepStatusWaitingApproval, startedAt)

	applyStepUpdate(t, environment, repository, stepUpdate(
		guard, entities[0], contracts.StepStatusWaitingApproval, contracts.StepStatusRunning,
		nil, nil, nil, nil,
	))
	assertStepUpdateRejected(t, environment, repository, stepUpdate(
		guard, entities[0], contracts.StepStatusRunning, contracts.StepStatusCompleted,
		output, nil, &overwriteAt, &completedAt,
	))
	applyStepUpdate(t, environment, repository, stepUpdate(
		guard, entities[0], contracts.StepStatusRunning, contracts.StepStatusCompleted,
		output, nil, nil, &completedAt,
	))
	assertStepStartedAt(t, repository, entities[0].StepID(), contracts.StepStatusCompleted, startedAt)

	if err := environment.Runtime.WriteExecutor().Execute(context.Background(), func(ctx context.Context, tx contracts.RuntimeWriteTx) error {
		update := runPlanPointerUpdate(graph, entities[1].PlanID(), now)
		stepID := entities[1].StepID()
		update.CurrentStepID = &stepID
		updated, err := taskRepositories.Runs.Update(ctx, tx, update)
		if err != nil || !updated {
			return errors.Join(err, errors.New("advance Run current Step did not affect one row"))
		}
		return nil
	}); err != nil {
		t.Fatalf("advance Run current Step: %v", err)
	}

	secondStartedAt := now.Add(3 * time.Second)
	failedAt := secondStartedAt.Add(time.Second)
	errorCode := contracts.ErrorCodeModelCallFailed
	applyStepUpdate(t, environment, repository, stepUpdate(
		guard, entities[1], contracts.StepStatusPending, contracts.StepStatusRunning,
		nil, nil, &secondStartedAt, nil,
	))
	assertStepUpdateRejected(t, environment, repository, stepUpdate(
		guard, entities[1], contracts.StepStatusRunning, contracts.StepStatusFailed,
		nil, &errorCode, &overwriteAt, &failedAt,
	))
	applyStepUpdate(t, environment, repository, stepUpdate(
		guard, entities[1], contracts.StepStatusRunning, contracts.StepStatusFailed,
		nil, &errorCode, nil, &failedAt,
	))
	assertStepStartedAt(t, repository, entities[1].StepID(), contracts.StepStatusFailed, secondStartedAt)
}

func testStepRepositoryIllegalTransitions(t *testing.T, environment *postgrestest.RepositoryEnvironment) {
	repository, _, _, entities, guard, now := stepRepositoryFixture(t, environment, "illegal-transition", 1)
	endedAt := now.Add(time.Second)
	output := json.RawMessage(`{"result":"safe"}`)
	errorCode := contracts.ErrorCodeTaskCancelled
	tests := []stepdomain.Update{
		stepUpdate(guard, entities[0], contracts.StepStatusPending, contracts.StepStatusCompleted, output, nil, nil, &endedAt),
		stepUpdate(guard, entities[0], contracts.StepStatusPending, contracts.StepStatusWaitingApproval, nil, nil, nil, nil),
		stepUpdate(guard, entities[0], contracts.StepStatusPending, contracts.StepStatusRunning, nil, nil, nil, nil),
		stepUpdate(guard, entities[0], contracts.StepStatusPending, contracts.StepStatusFailed, nil, &errorCode, &now, &endedAt),
	}
	for _, update := range tests {
		assertStepUpdateRejected(t, environment, repository, update)
	}
	stored, err := repository.FindByID(context.Background(), entities[0].StepID())
	if err != nil || stored.Status() != contracts.StepStatusPending || stored.StartedAt() != nil || stored.EndedAt() != nil {
		t.Fatalf("Step after illegal transitions = (%#v, %v)", stored, err)
	}
}

func testStepRepositoryRollback(t *testing.T, environment *postgrestest.RepositoryEnvironment) {
	repository, _, _, entities, guard, now := stepRepositoryFixture(t, environment, "rollback", 1)
	startedAt := now.Add(time.Second)
	sentinel := errors.New("force Step rollback")
	err := environment.Runtime.WriteExecutor().Execute(context.Background(), func(ctx context.Context, tx contracts.RuntimeWriteTx) error {
		updated, err := repository.Update(ctx, tx, stepUpdate(guard, entities[0], contracts.StepStatusPending, contracts.StepStatusRunning, nil, nil, &startedAt, nil))
		if err != nil || !updated {
			return errors.Join(err, errors.New("Step rollback update did not affect one row"))
		}
		return sentinel
	})
	if !errors.Is(err, sentinel) {
		t.Fatalf("rollback error = %v", err)
	}
	stored, err := repository.FindByID(context.Background(), entities[0].StepID())
	if err != nil || stored.Status() != contracts.StepStatusPending {
		t.Fatalf("Step after rollback = (%#v, %v)", stored, err)
	}
}

func testStepRepositoryConditionalGuard(t *testing.T, environment *postgrestest.RepositoryEnvironment) {
	repository, _, _, entities, guard, now := stepRepositoryFixture(t, environment, "guard", 1)
	startedAt := now.Add(time.Second)
	base := stepUpdate(guard, entities[0], contracts.StepStatusPending, contracts.StepStatusRunning, nil, nil, &startedAt, nil)

	tests := []struct {
		name   string
		mutate func(*stepdomain.Update)
	}{
		{name: "Task", mutate: func(value *stepdomain.Update) { value.Guard.TaskID = "task-other" }},
		{name: "Run", mutate: func(value *stepdomain.Update) { value.RunID = "run-other" }},
		{name: "Step", mutate: func(value *stepdomain.Update) { value.StepID = "step-other" }},
		{name: "execution version", mutate: func(value *stepdomain.Update) { value.Guard.ExecutionVersion = 2 }},
		{name: "worker", mutate: func(value *stepdomain.Update) {
			worker := contracts.WorkerID("worker-other")
			value.Guard.ExpectedWorkerID = &worker
		}},
		{name: "Task status", mutate: func(value *stepdomain.Update) { value.Guard.ExpectedTaskStatus = contracts.TaskStatusPending }},
		{name: "Run status", mutate: func(value *stepdomain.Update) { value.Guard.ExpectedRunStatus = contracts.RunStatusPending }},
		{name: "Execution status", mutate: func(value *stepdomain.Update) {
			value.Guard.ExpectedExecutionStatus = contracts.TaskExecutionStatusQueued
		}},
		{name: "Step status", mutate: func(value *stepdomain.Update) {
			value.ExpectedStatus = contracts.StepStatusWaitingApproval
			value.StartedAt = nil
		}},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			mismatch := base
			test.mutate(&mismatch)
			if err := environment.Runtime.WriteExecutor().Execute(context.Background(), func(ctx context.Context, tx contracts.RuntimeWriteTx) error {
				updated, err := repository.Update(ctx, tx, mismatch)
				if err != nil {
					return err
				}
				if updated {
					return errors.New("mismatched Step guard updated a row")
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
		})
	}

	if err := environment.Runtime.WriteExecutor().Execute(context.Background(), func(ctx context.Context, tx contracts.RuntimeWriteTx) error {
		updated, err := repository.Update(ctx, tx, base)
		if err != nil || !updated {
			return errors.Join(err, errors.New("valid Step guard did not update"))
		}
		updated, err = repository.Update(ctx, tx, base)
		if err != nil {
			return err
		}
		if updated {
			return errors.New("stale Step status updated twice")
		}
		return nil
	}); err != nil {
		t.Fatalf("valid and stale Step updates: %v", err)
	}
}

func testStepRepositoryContiguousSequence(t *testing.T, environment *postgrestest.RepositoryEnvironment) {
	repository, _, _, entities, _, _ := stepRepositoryFixture(t, environment, "sequence", 3)
	steps, err := repository.ListByRun(context.Background(), entities[0].RunID())
	if err != nil || len(steps) != 3 {
		t.Fatalf("initial contiguous Steps = (%v, %v)", steps, err)
	}

	connection := postgrestest.Connect(t, environment.Identities.MigrationDSN)
	if _, err := connection.Exec(context.Background(), `DELETE FROM step WHERE step_id=$1`, entities[1].StepID()); err != nil {
		t.Fatalf("create persisted sequence gap: %v", err)
	}
	if _, err := repository.ListByRun(context.Background(), entities[0].RunID()); !errors.Is(err, stepdomain.ErrPersistenceInvariantViolation) {
		t.Fatalf("ListByRun() gap error = %v", err)
	}

	invalidBatch := []stepdomain.Entity{entities[0], entities[2]}
	if err := environment.Runtime.WriteExecutor().Execute(context.Background(), func(ctx context.Context, tx contracts.RuntimeWriteTx) error {
		return repository.InsertAll(ctx, tx, invalidBatch)
	}); !errors.Is(err, stepdomain.ErrInvalidStepBatch) {
		t.Fatalf("InsertAll() discontinuous batch error = %v", err)
	}
}

func testStepRepositoryWriteToken(t *testing.T, environment *postgrestest.RepositoryEnvironment) {
	repository, _, _, entities, guard, now := stepRepositoryFixture(t, environment, "token", 1)
	startedAt := now.Add(time.Second)
	updated, err := repository.Update(
		context.Background(), &foreignRuntimeWriteTx{},
		stepUpdate(guard, entities[0], contracts.StepStatusPending, contracts.StepStatusRunning, nil, nil, &startedAt, nil),
	)
	if err == nil || updated {
		t.Fatalf("foreign transaction result = (%v, %v)", updated, err)
	}
}

func stepRepositoryFixture(
	t *testing.T,
	environment *postgrestest.RepositoryEnvironment,
	suffix string,
	count int,
) (*postgresstep.Repository, *postgrestaskruntime.Repositories, repositoryGraphValues, []stepdomain.Entity, stepdomain.UpdateGuard, time.Time) {
	t.Helper()
	planRepository, taskRepositories, graph, now, planGuard := plannerRepositoryFixture(t, environment, "step-"+suffix)
	repository := postgresstep.New(environment.Runtime.ReadPool())
	planID := contracts.PlanID("plan-step-" + suffix)
	planEntity := mustPlanEntity(t, planID, graph.run.RunID, "safe goal", now)
	entities := mustStepEntities(t, graph.run.RunID, planID, count)
	if err := environment.Runtime.WriteExecutor().Execute(context.Background(), func(ctx context.Context, tx contracts.RuntimeWriteTx) error {
		inserted, err := planRepository.InsertIfCurrentExecution(ctx, tx, planEntity, planGuard)
		if err != nil || !inserted {
			return errors.Join(err, errors.New("fixture Plan insert did not affect one row"))
		}
		if err := repository.InsertAll(ctx, tx, entities); err != nil {
			return err
		}
		update := runPlanPointerUpdate(graph, planID, now)
		stepID := entities[0].StepID()
		update.CurrentStepID = &stepID
		updated, err := taskRepositories.Runs.Update(ctx, tx, update)
		if err != nil || !updated {
			return errors.Join(err, errors.New("fixture Run Step pointer update did not affect one row"))
		}
		return nil
	}); err != nil {
		t.Fatalf("create Step Repository fixture: %v", err)
	}
	workerID := planGuard.WorkerID
	return repository, taskRepositories, graph, entities, stepdomain.UpdateGuard{
		TaskID: graph.task.TaskID, ExecutionVersion: graph.execution.ExecutionVersion, ExpectedWorkerID: &workerID,
		ExpectedTaskStatus: contracts.TaskStatusRunning, ExpectedRunStatus: contracts.RunStatusRunning,
		ExpectedExecutionStatus: contracts.TaskExecutionStatusRunning,
	}, now
}

func mustStepEntities(t *testing.T, runID contracts.RunID, planID contracts.PlanID, count int) []stepdomain.Entity {
	t.Helper()
	entities := make([]stepdomain.Entity, 0, count)
	for sequence := 1; sequence <= count; sequence++ {
		stepType := contracts.StepTypeAnalysis
		if sequence == count {
			stepType = contracts.StepTypeVerification
		}
		entity, err := stepdomain.NewEntity(stepdomain.EntityParams{
			StepID: contracts.StepID(string(planID) + "-step-" + strconv.Itoa(sequence)),
			RunID:  runID, PlanID: planID, Sequence: uint32(sequence), Type: stepType, Name: "step",
			Input:        json.RawMessage(`{}`),
			OutputSchema: contracts.OutputSchema{"result": {Type: contracts.OutputValueTypeString}},
			Status:       contracts.StepStatusPending,
		})
		if err != nil {
			t.Fatalf("create Step Entity %d: %v", sequence, err)
		}
		entities = append(entities, entity)
	}
	return entities
}

func stepUpdate(
	guard stepdomain.UpdateGuard,
	entity stepdomain.Entity,
	expected contracts.StepStatus,
	status contracts.StepStatus,
	output json.RawMessage,
	errorCode *contracts.ErrorCode,
	startedAt *time.Time,
	endedAt *time.Time,
) stepdomain.Update {
	return stepdomain.Update{
		Guard: guard, RunID: entity.RunID(), StepID: entity.StepID(), ExpectedStatus: expected,
		Status: status, Output: output, ErrorCode: errorCode, StartedAt: startedAt, EndedAt: endedAt,
	}
}

func applyStepUpdate(
	t *testing.T,
	environment *postgrestest.RepositoryEnvironment,
	repository *postgresstep.Repository,
	update stepdomain.Update,
) {
	t.Helper()
	if err := environment.Runtime.WriteExecutor().Execute(context.Background(), func(ctx context.Context, tx contracts.RuntimeWriteTx) error {
		updated, err := repository.Update(ctx, tx, update)
		if err != nil || !updated {
			return errors.Join(err, errors.New("Step update did not affect one row"))
		}
		return nil
	}); err != nil {
		t.Fatalf("apply Step update %s to %s: %v", update.ExpectedStatus, update.Status, err)
	}
}

func assertStepUpdateRejected(
	t *testing.T,
	environment *postgrestest.RepositoryEnvironment,
	repository *postgresstep.Repository,
	update stepdomain.Update,
) {
	t.Helper()
	err := environment.Runtime.WriteExecutor().Execute(context.Background(), func(ctx context.Context, tx contracts.RuntimeWriteTx) error {
		updated, err := repository.Update(ctx, tx, update)
		if updated {
			return errors.New("invalid Step update affected one row")
		}
		return err
	})
	if !errors.Is(err, stepdomain.ErrInvalidUpdateGuard) {
		t.Fatalf("invalid Step update %s to %s error = %v", update.ExpectedStatus, update.Status, err)
	}
}

func assertStepStartedAt(
	t *testing.T,
	repository *postgresstep.Repository,
	stepID contracts.StepID,
	status contracts.StepStatus,
	want time.Time,
) {
	t.Helper()
	stored, err := repository.FindByID(context.Background(), stepID)
	if err != nil || stored.Status() != status || stored.StartedAt() == nil || !stored.StartedAt().Equal(want) {
		t.Fatalf("Step %s state = (%s, started_at=%v, err=%v), want (%s, %v)",
			stepID, stored.Status(), stored.StartedAt(), err, status, want)
	}
}
