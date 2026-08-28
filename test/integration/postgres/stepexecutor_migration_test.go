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
	stepmigrations "github.com/zhaohaip/agentops-go/migrations/stepexecutor"
	taskruntimemigrations "github.com/zhaohaip/agentops-go/migrations/taskruntime"
	"github.com/zhaohaip/agentops-go/test/integration/postgres/postgrestest"
)

func TestStepMigrationFreshUpgradeRepeatAndRollback(t *testing.T) {
	t.Parallel()
	all := stepMigrationSet()
	for _, test := range []struct {
		name    string
		prepare func(*testing.T, *postgrestest.MigrationHarness)
	}{
		{name: "fresh database"},
		{name: "Phase 3 upgrade", prepare: func(t *testing.T, harness *postgrestest.MigrationHarness) {
			if err := harness.Apply(context.Background(), plannerMigrationSet()); err != nil {
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
				t.Fatalf("apply Step Migration: %v", err)
			}
			if err := harness.Apply(context.Background(), all); err != nil {
				t.Fatalf("repeat Step Migration: %v", err)
			}
			versions, err := harness.AppliedVersions(context.Background())
			if err != nil || !reflect.DeepEqual(versions, []int64{1, 2, 3, 4}) {
				t.Fatalf("applied versions = %v, %v", versions, err)
			}
			assertStepSchema(t, connection)
		})
	}

	schema := postgrestest.NewSchema(t)
	connection := postgrestest.Connect(t, schema.DSN)
	broken := stepmigrations.Migrations()[0]
	broken.Statements = append(append([]string(nil), broken.Statements...), "CREATE TABLE broken_step (")
	err := postgrestest.NewMigrationHarness(connection).Apply(context.Background(), append(plannerMigrationSet(), broken))
	if err == nil {
		t.Fatal("broken Step Migration succeeded")
	}
	var exists bool
	if err := connection.QueryRow(context.Background(), `SELECT to_regclass(current_schema() || '.step') IS NOT NULL`).Scan(&exists); err != nil || exists {
		t.Fatalf("step table survived failed version: exists=%v err=%v", exists, err)
	}
}

func TestStepMigrationConstraintsAndForeignKeys(t *testing.T) {
	t.Parallel()
	connection := openStepMigrationSchema(t)
	now := time.Now().UTC()
	insertStepPlanGraph(t, connection, "a", now)
	insertStepPlanGraph(t, connection, "b", now)
	insertStepPlanGraph(t, connection, "c", now)

	insert := `
INSERT INTO step (
    step_id,run_id,plan_id,sequence,type,name,input,output_schema,
    output,status,tool_name,error_code,started_at,ended_at
) VALUES ($1,$2,$3,$4,$5,$6,$7::jsonb,$8::jsonb,$9::jsonb,$10,$11,$12,$13,$14)`
	if _, err := connection.Exec(context.Background(), insert,
		"step-a-1", "run-step-a", "plan-step-a", 1, "Analysis", "inspect", `{}`, `{"result":{"type":"string"}}`,
		nil, "Pending", "", nil, nil, nil,
	); err != nil {
		t.Fatalf("insert valid Step: %v", err)
	}

	tests := []struct {
		name       string
		args       []any
		code       string
		constraint string
	}{
		{name: "Step ID required", args: stepInsertArgs(nil, "run-step-b", "plan-step-b", 1), code: "23502"},
		{name: "Step ID nonempty", args: stepInsertArgs("", "run-step-b", "plan-step-b", 1), code: "23514", constraint: "step_step_id_check"},
		{name: "Run ID required", args: stepInsertArgs("step-null-run", nil, "plan-step-b", 1), code: "23502"},
		{name: "Plan ID required", args: stepInsertArgs("step-null-plan", "run-step-b", nil, 1), code: "23502"},
		{name: "positive sequence", args: stepInsertArgs("step-zero", "run-step-b", "plan-step-b", 0), code: "23514", constraint: "step_sequence_check"},
		{name: "Step type", args: mutateStepArgs(stepInsertArgs("step-type", "run-step-b", "plan-step-b", 1), 4, "Other"), code: "23514", constraint: "step_type_check"},
		{name: "Step status", args: mutateStepArgs(stepInsertArgs("step-status", "run-step-b", "plan-step-b", 1), 9, "Other"), code: "23514", constraint: "step_status_check"},
		{name: "input object", args: mutateStepArgs(stepInsertArgs("step-input", "run-step-b", "plan-step-b", 1), 6, `[]`), code: "23514", constraint: "step_input_check"},
		{name: "output schema nonempty", args: mutateStepArgs(stepInsertArgs("step-schema", "run-step-b", "plan-step-b", 1), 7, `{}`), code: "23514", constraint: "step_output_schema_check"},
		{name: "ToolCall requires tool", args: mutateStepArgs(stepInsertArgs("step-tool", "run-step-b", "plan-step-b", 1), 4, "ToolCall"), code: "23514", constraint: "step_tool_name_check"},
		{name: "non Tool forbids tool", args: mutateStepArgs(stepInsertArgs("step-nontool", "run-step-b", "plan-step-b", 1), 10, "k8s.get_deployment"), code: "23514", constraint: "step_tool_name_check"},
		{name: "status fields", args: mutateStepArgs(stepInsertArgs("step-fields", "run-step-b", "plan-step-b", 1), 9, "Completed"), code: "23514", constraint: "step_status_fields_check"},
		{name: "Running requires start", args: mutateStepArgs(stepInsertArgs("step-running", "run-step-b", "plan-step-b", 1), 9, "Running"), code: "23514", constraint: "step_status_fields_check"},
		{name: "Failed requires end", args: mutateStepArgs(mutateStepArgs(stepInsertArgs("step-failed", "run-step-b", "plan-step-b", 1), 9, "Failed"), 11, "TaskCancelled"), code: "23514", constraint: "step_status_fields_check"},
		{name: "Plan foreign key", args: stepInsertArgs("step-plan-missing", "run-step-b", "plan-missing", 1), code: "23503", constraint: "step_plan_foreign_key"},
		{name: "cross Run Plan", args: stepInsertArgs("step-cross-plan", "run-step-b", "plan-step-a", 1), code: "23503", constraint: "step_plan_foreign_key"},
		{name: "Run sequence unique", args: stepInsertArgs("step-a-duplicate", "run-step-a", "plan-step-a", 1), code: "23505", constraint: "step_run_sequence_unique"},
		{name: "Step primary key", args: stepInsertArgs("step-a-1", "run-step-b", "plan-step-b", 1), code: "23505", constraint: "step_pkey"},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			_, err := connection.Exec(context.Background(), insert, test.args...)
			assertStepViolation(t, err, test.code, test.constraint)
		})
	}

	failedBeforeStart := stepInsertArgs("step-c-1", "run-step-c", "plan-step-c", 1)
	failedBeforeStart = mutateStepArgs(failedBeforeStart, 9, "Failed")
	failedBeforeStart = mutateStepArgs(failedBeforeStart, 11, "TaskCancelled")
	failedBeforeStart = mutateStepArgs(failedBeforeStart, 13, now)
	if _, err := connection.Exec(context.Background(), insert, failedBeforeStart...); err != nil {
		t.Fatalf("insert Pending to Failed Step: %v", err)
	}
	var startedAt, endedAt *time.Time
	if err := connection.QueryRow(context.Background(),
		`SELECT started_at,ended_at FROM step WHERE step_id='step-c-1'`).Scan(&startedAt, &endedAt); err != nil {
		t.Fatalf("query Pending to Failed timestamps: %v", err)
	}
	if startedAt != nil || endedAt == nil || !endedAt.Equal(now) {
		t.Fatalf("Pending to Failed timestamps = (%v, %v), want (nil, %v)", startedAt, endedAt, now)
	}

	if _, err := connection.Exec(context.Background(), insert, stepInsertArgs("step-b-1", "run-step-b", "plan-step-b", 1)...); err != nil {
		t.Fatalf("insert second valid Step: %v", err)
	}
	if _, err := connection.Exec(context.Background(), `UPDATE run SET current_step_id='step-a-1' WHERE run_id='run-step-a'`); err != nil {
		t.Fatalf("set valid Run current Step pointer: %v", err)
	}
	_, err := connection.Exec(context.Background(), `UPDATE run SET current_step_id='step-a-1' WHERE run_id='run-step-b'`)
	assertStepViolation(t, err, "23503", "run_current_step_foreign_key")
	_, err = connection.Exec(context.Background(), `UPDATE run SET current_step_id='step-missing' WHERE run_id='run-step-b'`)
	assertStepViolation(t, err, "23503", "run_current_step_foreign_key")
}

func stepMigrationSet() []migration.Migration {
	definitions := append(taskruntimemigrations.Migrations(), checkpointmigrations.Migrations()...)
	definitions = append(definitions, plannermigrations.Migrations()...)
	return append(definitions, stepmigrations.Migrations()...)
}

func openStepMigrationSchema(t testing.TB) *pgx.Conn {
	t.Helper()
	schema := postgrestest.NewSchema(t)
	connection := postgrestest.Connect(t, schema.DSN)
	if err := postgrestest.NewMigrationHarness(connection).Apply(context.Background(), stepMigrationSet()); err != nil {
		t.Fatalf("apply Step Migrations: %v", err)
	}
	return connection
}

func insertStepPlanGraph(t *testing.T, connection *pgx.Conn, suffix string, now time.Time) {
	t.Helper()
	taskID, runID, executionID, planID := "task-step-"+suffix, "run-step-"+suffix, "execution-step-"+suffix, "plan-step-"+suffix
	insertCommittedTaskGraph(t, connection, taskID, runID, executionID, now)
	if _, err := connection.Exec(context.Background(),
		`INSERT INTO plan (plan_id,run_id,goal,created_at) VALUES ($1,$2,'goal',$3)`, planID, runID, now); err != nil {
		t.Fatalf("insert Plan fixture: %v", err)
	}
	if _, err := connection.Exec(context.Background(), `UPDATE run SET plan_id=$1 WHERE run_id=$2`, planID, runID); err != nil {
		t.Fatalf("set Run Plan fixture: %v", err)
	}
}

func stepInsertArgs(stepID any, runID any, planID any, sequence any) []any {
	return []any{
		stepID, runID, planID, sequence, "Analysis", "inspect", `{}`, `{"result":{"type":"string"}}`,
		nil, "Pending", "", nil, nil, nil,
	}
}

func mutateStepArgs(args []any, index int, value any) []any {
	copyArgs := append([]any(nil), args...)
	copyArgs[index] = value
	return copyArgs
}

func assertStepSchema(t *testing.T, connection *pgx.Conn) {
	t.Helper()
	rows, err := connection.Query(context.Background(), `
SELECT column_name
FROM information_schema.columns
WHERE table_schema=current_schema() AND table_name='step'
ORDER BY ordinal_position`)
	if err != nil {
		t.Fatalf("query Step columns: %v", err)
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
	want := []string{"step_id", "run_id", "plan_id", "sequence", "type", "name", "input", "output_schema", "output", "status", "tool_name", "error_code", "started_at", "ended_at"}
	if !reflect.DeepEqual(columns, want) {
		t.Fatalf("Step columns = %v, want %v", columns, want)
	}

	var dataType, nullable string
	var defaultValue *string
	if err := connection.QueryRow(context.Background(), `
SELECT data_type,is_nullable,column_default
FROM information_schema.columns
WHERE table_schema=current_schema() AND table_name='run' AND column_name='current_step_id'`).Scan(
		&dataType, &nullable, &defaultValue,
	); err != nil {
		t.Fatalf("query frozen run.current_step_id column: %v", err)
	}
	if dataType != "text" || nullable != "YES" || defaultValue != nil {
		t.Fatalf("run.current_step_id semantics changed: type=%q nullable=%q default=%v", dataType, nullable, defaultValue)
	}

	var definition string
	if err := connection.QueryRow(context.Background(), `
SELECT pg_get_constraintdef(c.oid)
FROM pg_constraint AS c
JOIN pg_class AS table_class ON table_class.oid=c.conrelid
JOIN pg_namespace AS namespace ON namespace.oid=table_class.relnamespace
WHERE namespace.nspname=current_schema() AND table_class.relname='run' AND c.conname='run_current_step_foreign_key'`).Scan(&definition); err != nil {
		t.Fatalf("query Run current Step FK: %v", err)
	}
	if !strings.Contains(definition, "FOREIGN KEY (run_id, current_step_id) REFERENCES step(run_id, step_id)") ||
		!strings.Contains(definition, "DEFERRABLE INITIALLY DEFERRED") {
		t.Fatalf("Run current Step FK definition = %q", definition)
	}

	for _, indexName := range []string{"step_pkey", "step_run_sequence_unique"} {
		var exists bool
		if err := connection.QueryRow(context.Background(), `
SELECT EXISTS (
    SELECT 1 FROM pg_indexes
    WHERE schemaname=current_schema() AND tablename='step' AND indexname=$1
)`, indexName).Scan(&exists); err != nil || !exists {
			t.Fatalf("Step index %q exists=%v err=%v", indexName, exists, err)
		}
	}
	assertStepPlanSequenceIndexDefinition(t, connection)
}

func assertStepPlanSequenceIndexDefinition(t *testing.T, connection *pgx.Conn) {
	t.Helper()
	var (
		table      string
		unique     bool
		definition string
	)
	if err := connection.QueryRow(context.Background(), `
SELECT table_class.relname,
       index_meta.indisunique,
       pg_get_indexdef(index_meta.indexrelid)
FROM pg_index AS index_meta
JOIN pg_class AS index_class ON index_class.oid = index_meta.indexrelid
JOIN pg_class AS table_class ON table_class.oid = index_meta.indrelid
JOIN pg_namespace AS namespace ON namespace.oid = index_class.relnamespace
WHERE namespace.nspname = current_schema()
  AND index_class.relname = 'step_plan_sequence_index'`).Scan(&table, &unique, &definition); err != nil {
		t.Fatalf("query step_plan_sequence_index definition: %v", err)
	}
	if table != "step" || unique || !strings.Contains(definition, "(plan_id, sequence)") {
		t.Fatalf("step_plan_sequence_index = (table:%q unique:%t definition:%q), want step/non-unique/(plan_id, sequence)",
			table, unique, definition)
	}

	rows, err := connection.Query(context.Background(), `
SELECT attribute.attname
FROM pg_index AS index_meta
JOIN pg_class AS index_class ON index_class.oid = index_meta.indexrelid
JOIN pg_namespace AS namespace ON namespace.oid = index_class.relnamespace
CROSS JOIN LATERAL unnest(index_meta.indkey) WITH ORDINALITY AS key(attnum, ordinality)
JOIN pg_attribute AS attribute
  ON attribute.attrelid = index_meta.indrelid
 AND attribute.attnum = key.attnum
WHERE namespace.nspname = current_schema()
  AND index_class.relname = 'step_plan_sequence_index'
  AND key.ordinality <= index_meta.indnkeyatts
ORDER BY key.ordinality`)
	if err != nil {
		t.Fatalf("query step_plan_sequence_index columns: %v", err)
	}
	defer rows.Close()
	var columns []string
	for rows.Next() {
		var column string
		if err := rows.Scan(&column); err != nil {
			t.Fatalf("scan step_plan_sequence_index column: %v", err)
		}
		columns = append(columns, column)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate step_plan_sequence_index columns: %v", err)
	}
	if want := []string{"plan_id", "sequence"}; !reflect.DeepEqual(columns, want) {
		t.Fatalf("step_plan_sequence_index columns = %v, want %v", columns, want)
	}
}

func assertStepViolation(t *testing.T, err error, code, constraint string) {
	t.Helper()
	var databaseError *pgconn.PgError
	if err == nil || !errors.As(err, &databaseError) {
		t.Fatalf("PostgreSQL error = %v, want SQLSTATE %s", err, code)
	}
	if databaseError.Code != code || (constraint != "" && databaseError.ConstraintName != constraint) {
		t.Fatalf("PostgreSQL error = %s/%s, want %s/%s: %v", databaseError.Code, databaseError.ConstraintName, code, constraint, err)
	}
}
