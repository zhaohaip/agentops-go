package postgres_test

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	postgresplanner "github.com/zhaohaip/agentops-go/internal/adapter/postgres/planner"
	postgrestaskruntime "github.com/zhaohaip/agentops-go/internal/adapter/postgres/taskruntime"
	"github.com/zhaohaip/agentops-go/internal/contracts"
	plannerdomain "github.com/zhaohaip/agentops-go/internal/planner"
	taskruntimedomain "github.com/zhaohaip/agentops-go/internal/taskruntime"
	"github.com/zhaohaip/agentops-go/test/integration/postgres/postgrestest"
)

func TestPlannerRepositoryContract(t *testing.T) {
	postgrestest.RunRepositoryContract(t, postgrestest.RepositoryContract{
		Name:       "Planner",
		Migrations: plannerMigrationSet(),
		Cases: []postgrestest.RepositoryCase{
			{Name: "conditional insert commit and read", Run: testPlannerRepositoryCommit},
			{Name: "transaction rollback", Run: testPlannerRepositoryRollback},
			{Name: "complete current execution guard", Run: testPlannerRepositoryConditionalGuard},
			{Name: "opaque write token boundary", Run: testPlannerRepositoryWriteToken},
		},
	})
}

func testPlannerRepositoryCommit(t *testing.T, environment *postgrestest.RepositoryEnvironment) {
	repository, taskRepositories, graph, now, guard := plannerRepositoryFixture(t, environment, "commit")
	entity := mustPlanEntity(t, "plan-commit", graph.run.RunID, "safe goal", now)
	if err := environment.Runtime.WriteExecutor().Execute(context.Background(), func(ctx context.Context, tx contracts.RuntimeWriteTx) error {
		inserted, err := repository.InsertIfCurrentExecution(ctx, tx, entity, guard)
		if err != nil || !inserted {
			return errors.Join(err, errors.New("conditional Plan insert did not affect one row"))
		}
		planID := entity.PlanID()
		updated, err := taskRepositories.Runs.Update(ctx, tx, runPlanPointerUpdate(graph, planID, now))
		if err != nil || !updated {
			return errors.Join(err, errors.New("Run Plan pointer update did not affect one row"))
		}
		return nil
	}); err != nil {
		t.Fatalf("commit Plan: %v", err)
	}

	stored, err := repository.FindByRun(context.Background(), graph.run.RunID)
	if err != nil || stored.PlanID() != entity.PlanID() || stored.RunID() != entity.RunID() ||
		stored.Goal() != entity.Goal() || !stored.CreatedAt().Equal(entity.CreatedAt()) {
		t.Fatalf("stored Plan = (%#v, %v), want %#v", stored, err, entity)
	}
	storedRun, err := taskRepositories.Runs.FindByTask(context.Background(), graph.task.TaskID)
	if err != nil || storedRun.PlanID == nil || *storedRun.PlanID != entity.PlanID() {
		t.Fatalf("stored Run Plan pointer = (%+v, %v)", storedRun, err)
	}
}

func testPlannerRepositoryRollback(t *testing.T, environment *postgrestest.RepositoryEnvironment) {
	repository, _, graph, now, guard := plannerRepositoryFixture(t, environment, "rollback")
	entity := mustPlanEntity(t, "plan-rollback", graph.run.RunID, "safe goal", now)
	sentinel := errors.New("force Plan rollback")
	err := environment.Runtime.WriteExecutor().Execute(context.Background(), func(ctx context.Context, tx contracts.RuntimeWriteTx) error {
		inserted, err := repository.InsertIfCurrentExecution(ctx, tx, entity, guard)
		if err != nil || !inserted {
			return errors.Join(err, errors.New("conditional Plan insert did not affect one row"))
		}
		return sentinel
	})
	if !errors.Is(err, sentinel) {
		t.Fatalf("rollback error = %v", err)
	}
	if _, err := repository.FindByRun(context.Background(), graph.run.RunID); !errors.Is(err, plannerdomain.ErrRepositoryNotFound) {
		t.Fatalf("rolled back Plan lookup error = %v", err)
	}
}

func testPlannerRepositoryConditionalGuard(t *testing.T, environment *postgrestest.RepositoryEnvironment) {
	repository, _, graph, now, guard := plannerRepositoryFixture(t, environment, "guard")
	entity := mustPlanEntity(t, "plan-guard", graph.run.RunID, "safe goal", now)

	invalidGuards := []struct {
		name   string
		change func(*plannerdomain.CreateGuard)
	}{
		{name: "Task", change: func(value *plannerdomain.CreateGuard) { value.TaskID = "task-other" }},
		{name: "execution version", change: func(value *plannerdomain.CreateGuard) { value.ExecutionVersion = 2 }},
		{name: "worker", change: func(value *plannerdomain.CreateGuard) { value.WorkerID = "worker-other" }},
		{name: "Task status", change: func(value *plannerdomain.CreateGuard) { value.ExpectedTaskStatus = contracts.TaskStatusPending }},
		{name: "Run status", change: func(value *plannerdomain.CreateGuard) { value.ExpectedRunStatus = contracts.RunStatusPending }},
		{name: "Execution status", change: func(value *plannerdomain.CreateGuard) {
			value.ExpectedExecutionStatus = contracts.TaskExecutionStatusQueued
		}},
	}
	for _, test := range invalidGuards {
		test := test
		t.Run(test.name, func(t *testing.T) {
			mismatch := guard
			test.change(&mismatch)
			if err := environment.Runtime.WriteExecutor().Execute(context.Background(), func(ctx context.Context, tx contracts.RuntimeWriteTx) error {
				inserted, err := repository.InsertIfCurrentExecution(ctx, tx, entity, mismatch)
				if err != nil {
					return err
				}
				if inserted {
					return errors.New("mismatched Plan guard inserted a row")
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			if _, err := repository.FindByRun(context.Background(), graph.run.RunID); !errors.Is(err, plannerdomain.ErrRepositoryNotFound) {
				t.Fatalf("guard rejection left Plan: %v", err)
			}
		})
	}

	if err := environment.Runtime.WriteExecutor().Execute(context.Background(), func(ctx context.Context, tx contracts.RuntimeWriteTx) error {
		inserted, err := repository.InsertIfCurrentExecution(ctx, tx, entity, guard)
		if err != nil || !inserted {
			return errors.Join(err, errors.New("valid Plan guard did not insert"))
		}
		inserted, err = repository.InsertIfCurrentExecution(ctx, tx,
			mustPlanEntity(t, "plan-guard-second", graph.run.RunID, "other goal", now), guard)
		if err != nil {
			return err
		}
		if inserted {
			return errors.New("second Plan for Run was inserted")
		}
		return nil
	}); err != nil {
		t.Fatalf("one Plan conditional write: %v", err)
	}
}

func testPlannerRepositoryWriteToken(t *testing.T, environment *postgrestest.RepositoryEnvironment) {
	repository, _, graph, now, guard := plannerRepositoryFixture(t, environment, "token")
	inserted, err := repository.InsertIfCurrentExecution(
		context.Background(), &foreignRuntimeWriteTx{},
		mustPlanEntity(t, "plan-token", graph.run.RunID, "safe goal", now), guard,
	)
	if err == nil || inserted {
		t.Fatalf("foreign transaction result = (%v, %v)", inserted, err)
	}
}

func plannerRepositoryFixture(
	t *testing.T,
	environment *postgrestest.RepositoryEnvironment,
	suffix string,
) (*postgresplanner.Repository, *postgrestaskruntime.Repositories, repositoryGraphValues, time.Time, plannerdomain.CreateGuard) {
	t.Helper()
	taskRepositories := postgrestaskruntime.New(environment.Runtime.ReadPool())
	now := repositoryDatabaseNow(t, environment, taskRepositories.Clock)
	graph := repositoryGraph("plan-"+suffix, now, now)
	workerID := contracts.WorkerID("worker-plan-" + suffix)
	startedAt := now
	graph.task.Status = contracts.TaskStatusRunning
	graph.task.QueuedAt = nil
	graph.task.StartedAt = &startedAt
	graph.run.Status = contracts.RunStatusRunning
	graph.run.StartedAt = &startedAt
	graph.execution.Status = contracts.TaskExecutionStatusRunning
	graph.execution.WorkerID = &workerID
	graph.execution.StartedAt = &startedAt
	insertRepositoryGraph(t, environment, taskRepositories, graph)
	return postgresplanner.New(environment.Runtime.ReadPool()), taskRepositories, graph, now, plannerdomain.CreateGuard{
		TaskID: graph.task.TaskID, ExecutionVersion: graph.execution.ExecutionVersion, WorkerID: workerID,
		ExpectedTaskStatus: contracts.TaskStatusRunning, ExpectedRunStatus: contracts.RunStatusRunning,
		ExpectedExecutionStatus: contracts.TaskExecutionStatusRunning,
	}
}

func mustPlanEntity(t *testing.T, planID contracts.PlanID, runID contracts.RunID, goal string, createdAt time.Time) plannerdomain.Entity {
	t.Helper()
	entity, err := plannerdomain.NewEntity(planID, runID, goal, createdAt)
	if err != nil {
		t.Fatalf("create Plan Entity: %v", err)
	}
	return entity
}

func runPlanPointerUpdate(graph repositoryGraphValues, planID contracts.PlanID, startedAt time.Time) taskruntimedomain.RunUpdate {
	return taskruntimedomain.RunUpdate{
		TaskID: graph.task.TaskID, RunID: graph.run.RunID,
		ExecutionVersion: graph.execution.ExecutionVersion,
		ExpectedStatus:   contracts.RunStatusRunning,
		Status:           contracts.RunStatusRunning,
		PlanID:           &planID,
		Context:          json.RawMessage(`{}`),
		StartedAt:        &startedAt,
	}
}
