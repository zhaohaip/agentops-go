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
	taskruntimemigrations "github.com/zhaohaip/agentops-go/migrations/taskruntime"
	"github.com/zhaohaip/agentops-go/test/integration/postgres/postgrestest"
)

func TestCheckpointMigrationFreshUpgradeRepeatAndRollback(t *testing.T) {
	t.Parallel()
	all := checkpointMigrationSet()
	for _, test := range []struct {
		name    string
		prepare func(*testing.T, *postgrestest.MigrationHarness)
	}{
		{name: "fresh"},
		{name: "Phase 1 upgrade", prepare: func(t *testing.T, h *postgrestest.MigrationHarness) {
			if err := h.Apply(context.Background(), taskruntimemigrations.Migrations()); err != nil {
				t.Fatalf("apply Phase 1 Migration: %v", err)
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
				t.Fatalf("apply Checkpoint Migration: %v", err)
			}
			if err := harness.Apply(context.Background(), all); err != nil {
				t.Fatalf("repeat Checkpoint Migration: %v", err)
			}
			versions, err := harness.AppliedVersions(context.Background())
			if err != nil || !reflect.DeepEqual(versions, []int64{1, 2}) {
				t.Fatalf("applied versions = %v, %v", versions, err)
			}
			assertCheckpointSchema(t, connection)
		})
	}

	schema := postgrestest.NewSchema(t)
	connection := postgrestest.Connect(t, schema.DSN)
	broken := checkpointmigrations.Migrations()[0]
	broken.Statements = append(append([]string(nil), broken.Statements...), "CREATE TABLE broken_checkpoint (")
	err := postgrestest.NewMigrationHarness(connection).Apply(context.Background(), append(taskruntimemigrations.Migrations(), broken))
	if err == nil {
		t.Fatal("broken Checkpoint Migration succeeded")
	}
	var exists bool
	if err := connection.QueryRow(context.Background(), `SELECT to_regclass(current_schema() || '.checkpoint') IS NOT NULL`).Scan(&exists); err != nil || exists {
		t.Fatalf("checkpoint table survived failed version: exists=%v err=%v", exists, err)
	}
}

func TestCheckpointMigrationConstraints(t *testing.T) {
	t.Parallel()
	connection := openCheckpointMigrationSchema(t)
	now := time.Now().UTC()
	insertCommittedTaskGraph(t, connection, "task-checkpoint", "run-checkpoint", "execution-checkpoint", now)
	if _, err := connection.Exec(context.Background(), `
INSERT INTO task_execution (task_execution_id,task_id,execution_version,status,execution_config_hash,created_at)
VALUES ('execution-checkpoint-v2','task-checkpoint',2,'QUEUED',repeat('a',64),now())`); err != nil {
		t.Fatalf("insert second TaskExecution: %v", err)
	}
	valid := `{"schema_version":1,"task_id":"task-checkpoint","run_id":"run-checkpoint","execution_version":1,"next_action":"GENERATE_PLAN","resolved_references":[]}`
	insert := `INSERT INTO checkpoint (checkpoint_id,task_id,run_id,execution_version,checkpoint_sequence,runtime_context,execution_config_hash,source_execution_version,source_checkpoint_id,created_at) VALUES ($1,$2,$3,$4,$5,$6::jsonb,$7,$8,$9,now())`
	if _, err := connection.Exec(context.Background(), insert, "checkpoint-valid", "task-checkpoint", "run-checkpoint", 1, 1, valid, strings.Repeat("a", 64), nil, nil); err != nil {
		t.Fatalf("insert valid Checkpoint: %v", err)
	}

	tests := []struct {
		name       string
		args       []any
		code       string
		constraint string
	}{
		{name: "Checkpoint ID nonempty", args: []any{"", "task-checkpoint", "run-checkpoint", 1, 2, valid, strings.Repeat("a", 64), nil, nil}, code: "23514", constraint: "checkpoint_checkpoint_id_check"},
		{name: "Task ID nonempty", args: []any{"cp-task-empty", "", "run-checkpoint", 1, 2, valid, strings.Repeat("a", 64), nil, nil}, code: "23514", constraint: "checkpoint_task_id_check"},
		{name: "Run ID nonempty", args: []any{"cp-run-empty", "task-checkpoint", "", 1, 2, valid, strings.Repeat("a", 64), nil, nil}, code: "23514", constraint: "checkpoint_run_id_check"},
		{name: "required context", args: []any{"cp-null", "task-checkpoint", "run-checkpoint", 1, 2, nil, strings.Repeat("a", 64), nil, nil}, code: "23502"},
		{name: "required hash", args: []any{"cp-hash-null", "task-checkpoint", "run-checkpoint", 1, 2, valid, nil, nil, nil}, code: "23502"},
		{name: "positive Execution version", args: []any{"cp-version", "task-checkpoint", "run-checkpoint", 0, 2, valid, strings.Repeat("a", 64), nil, nil}, code: "23514", constraint: "checkpoint_execution_version_check"},
		{name: "positive sequence", args: []any{"cp-seq", "task-checkpoint", "run-checkpoint", 1, 0, valid, strings.Repeat("a", 64), nil, nil}, code: "23514", constraint: "checkpoint_checkpoint_sequence_check"},
		{name: "context object", args: []any{"cp-json", "task-checkpoint", "run-checkpoint", 1, 2, `[]`, strings.Repeat("a", 64), nil, nil}, code: "23514", constraint: "checkpoint_runtime_context_check"},
		{name: "hash format", args: []any{"cp-hash", "task-checkpoint", "run-checkpoint", 1, 2, valid, "BAD", nil, nil}, code: "23514", constraint: "checkpoint_execution_config_hash_check"},
		{name: "Run attribution FK", args: []any{"cp-run", "task-checkpoint", "run-missing", 1, 2, valid, strings.Repeat("a", 64), nil, nil}, code: "23503", constraint: "checkpoint_run_foreign_key"},
		{name: "Execution attribution FK", args: []any{"cp-execution", "task-checkpoint", "run-checkpoint", 3, 2, valid, strings.Repeat("a", 64), nil, nil}, code: "23503", constraint: "checkpoint_execution_foreign_key"},
		{name: "source pair", args: []any{"cp-pair", "task-checkpoint", "run-checkpoint", 1, 2, valid, strings.Repeat("a", 64), nil, "checkpoint-valid"}, code: "23514", constraint: "checkpoint_source_pair_check"},
		{name: "source version ordering", args: []any{"cp-source-version", "task-checkpoint", "run-checkpoint", 1, 2, valid, strings.Repeat("a", 64), 1, "checkpoint-valid"}, code: "23514", constraint: "checkpoint_source_version_check"},
		{name: "positive source version", args: []any{"cp-source-positive", "task-checkpoint", "run-checkpoint", 2, 8, valid, strings.Repeat("a", 64), 0, "checkpoint-valid"}, code: "23514", constraint: "checkpoint_source_execution_version_check"},
		{name: "source FK", args: []any{"cp-source", "task-checkpoint", "run-checkpoint", 2, 9, valid, strings.Repeat("a", 64), 1, "checkpoint-missing"}, code: "23503", constraint: "checkpoint_source_foreign_key"},
		{name: "Run sequence unique", args: []any{"cp-duplicate", "task-checkpoint", "run-checkpoint", 1, 1, valid, strings.Repeat("a", 64), nil, nil}, code: "23505", constraint: "checkpoint_run_sequence_unique"},
		{name: "Checkpoint primary key", args: []any{"checkpoint-valid", "task-checkpoint", "run-checkpoint", 1, 10, valid, strings.Repeat("a", 64), nil, nil}, code: "23505", constraint: "checkpoint_pkey"},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			_, err := connection.Exec(context.Background(), insert, test.args...)
			assertCheckpointViolation(t, err, test.code, test.constraint)
		})
	}
}

func checkpointMigrationSet() []migration.Migration {
	return append(taskruntimemigrations.Migrations(), checkpointmigrations.Migrations()...)
}

func openCheckpointMigrationSchema(t testing.TB) *pgx.Conn {
	t.Helper()
	schema := postgrestest.NewSchema(t)
	connection := postgrestest.Connect(t, schema.DSN)
	if err := postgrestest.NewMigrationHarness(connection).Apply(context.Background(), checkpointMigrationSet()); err != nil {
		t.Fatalf("apply Checkpoint Migrations: %v", err)
	}
	return connection
}

func assertCheckpointSchema(t *testing.T, connection *pgx.Conn) {
	t.Helper()
	var columns int
	if err := connection.QueryRow(context.Background(), `SELECT count(*) FROM information_schema.columns WHERE table_schema=current_schema() AND table_name='checkpoint'`).Scan(&columns); err != nil || columns != 10 {
		t.Fatalf("checkpoint columns = %d, err=%v", columns, err)
	}
	rows, err := connection.Query(context.Background(), `SELECT indexname,indexdef FROM pg_indexes WHERE schemaname=current_schema() AND tablename='checkpoint'`)
	if err != nil {
		t.Fatalf("query Checkpoint indexes: %v", err)
	}
	defer rows.Close()
	definitions := map[string]string{}
	for rows.Next() {
		var name, definition string
		if err := rows.Scan(&name, &definition); err != nil {
			t.Fatal(err)
		}
		definitions[name] = definition
	}
	latest := definitions["checkpoint_latest_execution_index"]
	if !strings.Contains(latest, "(task_id, run_id, execution_version, checkpoint_sequence DESC)") || strings.Contains(latest, "UNIQUE") {
		t.Fatalf("latest index definition = %q", latest)
	}
	source := definitions["checkpoint_source_checkpoint_index"]
	if !strings.Contains(source, "(source_checkpoint_id)") || !strings.Contains(source, "WHERE (source_checkpoint_id IS NOT NULL)") {
		t.Fatalf("source index definition = %q", source)
	}
}

func assertCheckpointViolation(t *testing.T, err error, code, constraint string) {
	t.Helper()
	var databaseError *pgconn.PgError
	if err == nil || !errors.As(err, &databaseError) {
		t.Fatalf("PostgreSQL error = %v, want SQLSTATE %s", err, code)
	}
	if databaseError.Code != code || (constraint != "" && databaseError.ConstraintName != constraint) {
		t.Fatalf("PostgreSQL error = %s/%s, want %s/%s: %v", databaseError.Code, databaseError.ConstraintName, code, constraint, err)
	}
}
