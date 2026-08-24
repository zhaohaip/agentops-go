package postgres_test

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/zhaohaip/agentops-go/internal/adapter/postgres/migration"
	checkpointmigrations "github.com/zhaohaip/agentops-go/migrations/checkpoint"
	plannermigrations "github.com/zhaohaip/agentops-go/migrations/planner"
	taskruntimemigrations "github.com/zhaohaip/agentops-go/migrations/taskruntime"
	"github.com/zhaohaip/agentops-go/test/integration/postgres/postgrestest"
)

func TestPlannerMigrationFreshUpgradeRepeatAndRollback(t *testing.T) {
	t.Parallel()
	all := plannerMigrationSet()
	for _, test := range []struct {
		name    string
		prepare func(*testing.T, *postgrestest.MigrationHarness)
	}{
		{name: "fresh database"},
		{name: "Phase 1 upgrade", prepare: func(t *testing.T, harness *postgrestest.MigrationHarness) {
			if err := harness.Apply(context.Background(), taskruntimemigrations.Migrations()); err != nil {
				t.Fatalf("apply Phase 1 Migration: %v", err)
			}
		}},
		{name: "Phase 2 upgrade", prepare: func(t *testing.T, harness *postgrestest.MigrationHarness) {
			predecessors := append(taskruntimemigrations.Migrations(), checkpointmigrations.Migrations()...)
			if err := harness.Apply(context.Background(), predecessors); err != nil {
				t.Fatalf("apply predecessor Migrations: %v", err)
			}
		}},
	} {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			schema := postgrestest.NewSchema(t)
			connection := postgrestest.Connect(t, schema.DSN)
			harness := postgrestest.NewMigrationHarness(connection)
			if test.prepare != nil {
				test.prepare(t, harness)
			}
			if err := harness.Apply(context.Background(), all); err != nil {
				t.Fatalf("apply Planner Migration: %v", err)
			}
			if err := harness.Apply(context.Background(), all); err != nil {
				t.Fatalf("repeat Planner Migration: %v", err)
			}
			versions, err := harness.AppliedVersions(context.Background())
			if err != nil || !reflect.DeepEqual(versions, []int64{1, 2, 3}) {
				t.Fatalf("applied versions = %v, %v", versions, err)
			}
			assertPlannerSchema(t, connection)
		})
	}

	schema := postgrestest.NewSchema(t)
	connection := postgrestest.Connect(t, schema.DSN)
	broken := plannermigrations.Migrations()[0]
	broken.Statements = append(append([]string(nil), broken.Statements...), "CREATE TABLE broken_plan (")
	err := postgrestest.NewMigrationHarness(connection).Apply(
		context.Background(),
		append(append(taskruntimemigrations.Migrations(), checkpointmigrations.Migrations()...), broken),
	)
	if err == nil {
		t.Fatal("broken Planner Migration succeeded")
	}
	var exists bool
	if err := connection.QueryRow(context.Background(), `SELECT to_regclass(current_schema() || '.plan') IS NOT NULL`).Scan(&exists); err != nil || exists {
		t.Fatalf("plan table survived failed version: exists=%v err=%v", exists, err)
	}
}

func TestPlannerMigrationConstraintsForeignKeysAndOneRunOnePlan(t *testing.T) {
	t.Parallel()
	connection := openPlannerMigrationSchema(t)
	now := time.Now().UTC()
	insertCommittedTaskGraph(t, connection, "task-plan-a", "run-plan-a", "execution-plan-a", now)
	insertCommittedTaskGraph(t, connection, "task-plan-b", "run-plan-b", "execution-plan-b", now)
	insert := `INSERT INTO plan (plan_id,run_id,goal,created_at) VALUES ($1,$2,$3,$4)`
	if _, err := connection.Exec(context.Background(), insert, "plan-a", "run-plan-a", "goal", now); err != nil {
		t.Fatalf("insert valid Plan: %v", err)
	}

	for _, test := range []struct {
		name       string
		args       []any
		code       string
		constraint string
	}{
		{name: "Plan ID required", args: []any{nil, "run-plan-b", "goal", now}, code: "23502"},
		{name: "Plan ID nonempty", args: []any{"", "run-plan-b", "goal", now}, code: "23514", constraint: "plan_plan_id_check"},
		{name: "Run ID required", args: []any{"plan-run-null", nil, "goal", now}, code: "23502"},
		{name: "Run ID nonempty", args: []any{"plan-run-empty", "", "goal", now}, code: "23514", constraint: "plan_run_id_check"},
		{name: "goal required", args: []any{"plan-goal-null", "run-plan-b", nil, now}, code: "23502"},
		{name: "goal nonempty", args: []any{"plan-goal-empty", "run-plan-b", "", now}, code: "23514", constraint: "plan_goal_check"},
		{name: "created at required", args: []any{"plan-time-null", "run-plan-b", "goal", nil}, code: "23502"},
		{name: "Run foreign key", args: []any{"plan-run-missing", "run-missing", "goal", now}, code: "23503", constraint: "plan_run_foreign_key"},
		{name: "one Plan per Run", args: []any{"plan-a-second", "run-plan-a", "goal", now}, code: "23505", constraint: "plan_run_unique"},
		{name: "Plan primary key", args: []any{"plan-a", "run-plan-b", "goal", now}, code: "23505", constraint: "plan_pkey"},
	} {
		test := test
		t.Run(test.name, func(t *testing.T) {
			_, err := connection.Exec(context.Background(), insert, test.args...)
			assertPlannerViolation(t, err, test.code, test.constraint)
		})
	}

	if _, err := connection.Exec(context.Background(), insert, "plan-b", "run-plan-b", "goal", now); err != nil {
		t.Fatalf("insert second valid Plan: %v", err)
	}
	if _, err := connection.Exec(context.Background(), `UPDATE run SET plan_id='plan-a' WHERE run_id='run-plan-a'`); err != nil {
		t.Fatalf("set valid Run Plan pointer: %v", err)
	}
	_, err := connection.Exec(context.Background(), `UPDATE run SET plan_id='plan-a' WHERE run_id='run-plan-b'`)
	assertPlannerViolation(t, err, "23503", "run_plan_foreign_key")
	_, err = connection.Exec(context.Background(), `UPDATE run SET plan_id='plan-missing' WHERE run_id='run-plan-b'`)
	assertPlannerViolation(t, err, "23503", "run_plan_foreign_key")
}

func plannerMigrationSet() []migration.Migration {
	definitions := append(taskruntimemigrations.Migrations(), checkpointmigrations.Migrations()...)
	return append(definitions, plannermigrations.Migrations()...)
}

func openPlannerMigrationSchema(t testing.TB) *pgx.Conn {
	t.Helper()
	schema := postgrestest.NewSchema(t)
	connection := postgrestest.Connect(t, schema.DSN)
	if err := postgrestest.NewMigrationHarness(connection).Apply(context.Background(), plannerMigrationSet()); err != nil {
		t.Fatalf("apply Planner Migrations: %v", err)
	}
	return connection
}

func assertPlannerSchema(t *testing.T, connection *pgx.Conn) {
	t.Helper()
	rows, err := connection.Query(context.Background(), `
SELECT column_name
FROM information_schema.columns
WHERE table_schema=current_schema() AND table_name='plan'
ORDER BY ordinal_position`)
	if err != nil {
		t.Fatalf("query Plan columns: %v", err)
	}
	defer rows.Close()
	var columns []string
	for rows.Next() {
		var column string
		if err := rows.Scan(&column); err != nil {
			t.Fatal(err)
		}
		columns = append(columns, column)
	}
	if !reflect.DeepEqual(columns, []string{"plan_id", "run_id", "goal", "created_at"}) {
		t.Fatalf("Plan columns = %v", columns)
	}
	for _, forbidden := range []string{"raw_response", "prompt", "provider_request_id", "model_response"} {
		if slicesContain(columns, forbidden) {
			t.Fatalf("Plan schema contains forbidden transient column %q", forbidden)
		}
	}
	var dataType, nullable string
	var defaultValue *string
	if err := connection.QueryRow(context.Background(), `
SELECT data_type,is_nullable,column_default
FROM information_schema.columns
WHERE table_schema=current_schema() AND table_name='run' AND column_name='plan_id'`).Scan(
		&dataType, &nullable, &defaultValue,
	); err != nil {
		t.Fatalf("query frozen run.plan_id column: %v", err)
	}
	if dataType != "text" || nullable != "YES" || defaultValue != nil {
		t.Fatalf("run.plan_id semantics changed: type=%q nullable=%q default=%v", dataType, nullable, defaultValue)
	}

	var definition string
	if err := connection.QueryRow(context.Background(), `
SELECT pg_get_constraintdef(c.oid)
FROM pg_constraint AS c
JOIN pg_class AS table_class ON table_class.oid=c.conrelid
JOIN pg_namespace AS namespace ON namespace.oid=table_class.relnamespace
WHERE namespace.nspname=current_schema() AND table_class.relname='run' AND c.conname='run_plan_foreign_key'`).Scan(&definition); err != nil {
		t.Fatalf("query Run Plan FK: %v", err)
	}
	if !strings.Contains(definition, "FOREIGN KEY (run_id, plan_id) REFERENCES plan(run_id, plan_id)") ||
		!strings.Contains(definition, "DEFERRABLE INITIALLY DEFERRED") {
		t.Fatalf("Run Plan FK definition = %q", definition)
	}
}

func slicesContain(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}

func assertPlannerViolation(t *testing.T, err error, code, constraint string) {
	t.Helper()
	var databaseError *pgconn.PgError
	if err == nil || !errors.As(err, &databaseError) {
		t.Fatalf("PostgreSQL error = %v, want SQLSTATE %s", err, code)
	}
	if databaseError.Code != code || (constraint != "" && databaseError.ConstraintName != constraint) {
		t.Fatalf("PostgreSQL error = %s/%s, want %s/%s: %v", databaseError.Code, databaseError.ConstraintName, code, constraint, err)
	}
}
