package postgres_test

import (
	"context"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/zhaohaip/agentops-go/internal/adapter/postgres/migration"
	taskruntimemigrations "github.com/zhaohaip/agentops-go/migrations/taskruntime"
	"github.com/zhaohaip/agentops-go/test/integration/postgres/postgrestest"
)

func TestTaskRuntimeMigrationFreshUpgradeAndRepeat(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		prepare func(*testing.T, *postgrestest.MigrationHarness)
	}{
		{name: "fresh database"},
		{
			name: "phase zero metadata already initialized",
			prepare: func(t *testing.T, harness *postgrestest.MigrationHarness) {
				t.Helper()
				if err := harness.Apply(context.Background(), nil); err != nil {
					t.Fatalf("initialize Phase 0 metadata: %v", err)
				}
			},
		},
	}

	for _, current := range tests {
		current := current
		t.Run(current.name, func(t *testing.T) {
			t.Parallel()
			schema := postgrestest.NewSchema(t)
			connection := postgrestest.Connect(t, schema.DSN)
			harness := postgrestest.NewMigrationHarness(connection)
			if current.prepare != nil {
				current.prepare(t, harness)
			}

			if err := harness.Apply(context.Background(), taskruntimemigrations.Migrations()); err != nil {
				t.Fatalf("apply Task Runtime Migration: %v", err)
			}
			if err := harness.Apply(context.Background(), taskruntimemigrations.Migrations()); err != nil {
				t.Fatalf("repeat Task Runtime Migration: %v", err)
			}

			versions, err := harness.AppliedVersions(context.Background())
			if err != nil {
				t.Fatalf("read applied versions: %v", err)
			}
			if want := []int64{1}; !reflect.DeepEqual(versions, want) {
				t.Fatalf("applied versions = %v, want %v", versions, want)
			}
			assertTaskRuntimeTables(t, connection)
			assertTaskRuntimeColumns(t, connection)
			assertTaskRuntimeIndexes(t, connection)
		})
	}
}

func TestTaskRuntimeMigrationAcceptsValidEntityGraph(t *testing.T) {
	t.Parallel()

	connection := openTaskRuntimeMigrationSchema(t)
	tx, err := connection.Begin(context.Background())
	if err != nil {
		t.Fatalf("begin entity graph transaction: %v", err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()

	now := time.Now().UTC()
	insertValidTaskGraph(t, tx, "task-valid", "run-valid", "execution-valid", now)
	if _, err := tx.Exec(context.Background(), `
INSERT INTO command_receipt (
    command_id, command_type, target_id, request_fingerprint, response, created_at
) VALUES ($1, 'Create', $2, $3, $4::jsonb, $5)`,
		"command-valid", "task-valid", strings.Repeat("a", 64), `{"task_id":"task-valid"}`, now,
	); err != nil {
		t.Fatalf("insert valid Command Receipt: %v", err)
	}
	version := int64(1)
	if _, err := tx.Exec(context.Background(), `
INSERT INTO task_log (
    log_id, task_id, run_id, execution_version, level, event, message, operator, created_at
) VALUES ($1, $2, $3, $4, 'Info', 'TaskCreated', 'created', 'System', $5)`,
		"log-valid", "task-valid", "run-valid", version, now,
	); err != nil {
		t.Fatalf("insert valid TaskLog: %v", err)
	}
	if err := tx.Commit(context.Background()); err != nil {
		t.Fatalf("commit valid entity graph: %v", err)
	}
}

func TestTaskRuntimeMigrationRejectsInvalidFacts(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		run  func(*testing.T, *pgx.Conn)
	}{
		{
			name: "required task field",
			run: func(t *testing.T, connection *pgx.Conn) {
				_, err := connection.Exec(context.Background(), `
INSERT INTO task (
    task_id, agent_id, created_by, input, status, current_run_id,
    current_execution_version, deadline_at, queued_at, created_at
) VALUES ('task-null', 'agent', NULL, 'input', 'Pending', 'run-null', 1, now(), now(), now())`)
				assertPostgreSQLError(t, err)
			},
		},
		{
			name: "closed status enum",
			run: func(t *testing.T, connection *pgx.Conn) {
				_, err := connection.Exec(context.Background(), `
INSERT INTO task (
    task_id, agent_id, created_by, input, status, current_run_id,
    current_execution_version, deadline_at, queued_at, created_at
) VALUES ('task-status', 'agent', 'operator', 'input', 'BLOCKED', 'run-status', 1, now(), now(), now())`)
				assertPostgreSQLError(t, err)
			},
		},
		{
			name: "execution version check",
			run: func(t *testing.T, connection *pgx.Conn) {
				tx, err := connection.Begin(context.Background())
				if err != nil {
					t.Fatalf("begin invalid execution transaction: %v", err)
				}
				defer func() { _ = tx.Rollback(context.Background()) }()
				now := time.Now().UTC()
				if _, err := tx.Exec(context.Background(), `
INSERT INTO task (
    task_id, agent_id, created_by, input, status, current_run_id,
    current_execution_version, deadline_at, queued_at, created_at
) VALUES ('task-execution-check', 'agent', 'operator', 'input', 'Pending', 'run-execution-check', 1, $1, $2, $2)`, now.Add(time.Hour), now); err != nil {
					t.Fatalf("insert deferred Task: %v", err)
				}
				if _, err := tx.Exec(context.Background(), `
INSERT INTO run (run_id, task_id, status, context)
VALUES ('run-execution-check', 'task-execution-check', 'Pending', '{}'::jsonb)`); err != nil {
					t.Fatalf("insert Run: %v", err)
				}
				_, err = tx.Exec(context.Background(), `
INSERT INTO task_execution (
    task_execution_id, task_id, execution_version, status,
    execution_config_hash, created_at
) VALUES ('execution-check', 'task-execution-check', 0, 'QUEUED', $2, $1)`, now, strings.Repeat("a", 64))
				assertPostgreSQLError(t, err)
			},
		},
		{
			name: "one run per task",
			run: func(t *testing.T, connection *pgx.Conn) {
				now := time.Now().UTC()
				insertCommittedTaskGraph(t, connection, "task-one-run", "run-one", "execution-one", now)
				_, err := connection.Exec(context.Background(), `
INSERT INTO run (run_id, task_id, status, context)
VALUES ('run-two', 'task-one-run', 'Pending', '{}'::jsonb)`)
				assertPostgreSQLError(t, err)
			},
		},
		{
			name: "task execution version unique",
			run: func(t *testing.T, connection *pgx.Conn) {
				now := time.Now().UTC()
				insertCommittedTaskGraph(t, connection, "task-version", "run-version", "execution-version-one", now)
				_, err := connection.Exec(context.Background(), `
INSERT INTO task_execution (
    task_execution_id, task_id, execution_version, status,
    execution_config_hash, created_at
) VALUES ($1, $2, 1, 'QUEUED', $3, $4)`,
					"execution-version-two", "task-version", strings.Repeat("b", 64), now,
				)
				assertPostgreSQLError(t, err)
			},
		},
		{
			name: "current execution foreign key",
			run: func(t *testing.T, connection *pgx.Conn) {
				tx, err := connection.Begin(context.Background())
				if err != nil {
					t.Fatalf("begin broken pointer transaction: %v", err)
				}
				now := time.Now().UTC()
				if _, err := tx.Exec(context.Background(), `
INSERT INTO task (
    task_id, agent_id, created_by, input, status, current_run_id,
    current_execution_version, deadline_at, queued_at, created_at
) VALUES ('task-pointer', 'agent', 'operator', 'input', 'Pending', 'run-pointer', 2, $1, $2, $2)`, now.Add(time.Hour), now); err != nil {
					t.Fatalf("insert deferred Task: %v", err)
				}
				if _, err := tx.Exec(context.Background(), `
INSERT INTO run (run_id, task_id, status, context)
VALUES ('run-pointer', 'task-pointer', 'Pending', '{}'::jsonb)`); err != nil {
					t.Fatalf("insert Run: %v", err)
				}
				if _, err := tx.Exec(context.Background(), `
INSERT INTO task_execution (
    task_execution_id, task_id, execution_version, status,
    execution_config_hash, created_at
) VALUES ('execution-pointer', 'task-pointer', 1, 'QUEUED', $1, $2)`, strings.Repeat("a", 64), now); err != nil {
					t.Fatalf("insert TaskExecution: %v", err)
				}
				assertPostgreSQLError(t, tx.Commit(context.Background()))
			},
		},
		{
			name: "observed hash relationship",
			run: func(t *testing.T, connection *pgx.Conn) {
				now := time.Now().UTC()
				insertCommittedTaskGraph(t, connection, "task-observed", "run-observed", "execution-observed-one", now)
				_, err := connection.Exec(context.Background(), `
INSERT INTO task_execution (
    task_execution_id, task_id, execution_version, status,
    execution_config_hash, observed_config_hash, created_at, ended_at
) VALUES ($1, $2, 2, 'INTERRUPTED', $3, $4, $5, $5)`,
					"execution-observed-two", "task-observed", strings.Repeat("a", 64), strings.Repeat("b", 64), now,
				)
				assertPostgreSQLError(t, err)
			},
		},
		{
			name: "task log execution foreign key",
			run: func(t *testing.T, connection *pgx.Conn) {
				now := time.Now().UTC()
				insertCommittedTaskGraph(t, connection, "task-log-fk", "run-log-fk", "execution-log-fk", now)
				_, err := connection.Exec(context.Background(), `
INSERT INTO task_log (
    log_id, task_id, run_id, execution_version, level, event, message, operator, created_at
) VALUES ('log-fk', 'task-log-fk', 'run-log-fk', 2, 'Info', 'event', '', 'System', $1)`, now)
				assertPostgreSQLError(t, err)
			},
		},
	}

	for _, current := range tests {
		current := current
		t.Run(current.name, func(t *testing.T) {
			t.Parallel()
			current.run(t, openTaskRuntimeMigrationSchema(t))
		})
	}
}

func TestTaskRuntimeMigrationFailureRollsBackWholeVersion(t *testing.T) {
	t.Parallel()

	schema := postgrestest.NewSchema(t)
	connection := postgrestest.Connect(t, schema.DSN)
	definition := taskruntimemigrations.Migrations()[0]
	definition.Statements = append(append([]string(nil), definition.Statements...), "CREATE TABLE broken_task_runtime_table (")
	err := postgrestest.NewMigrationHarness(connection).Apply(context.Background(), []migration.Migration{definition})
	if err == nil {
		t.Fatal("broken Task Runtime Migration error = nil")
	}

	for _, table := range []string{"task", "run", "task_execution", "command_receipt", "task_log"} {
		var exists bool
		if err := connection.QueryRow(context.Background(), "SELECT to_regclass($1) IS NOT NULL", table).Scan(&exists); err != nil {
			t.Fatalf("inspect rolled-back table %q: %v", table, err)
		}
		if exists {
			t.Fatalf("table %q remained after failed Migration", table)
		}
	}
}

func openTaskRuntimeMigrationSchema(t *testing.T) *pgx.Conn {
	t.Helper()
	schema := postgrestest.NewSchema(t)
	connection := postgrestest.Connect(t, schema.DSN)
	if err := postgrestest.NewMigrationHarness(connection).Apply(context.Background(), taskruntimemigrations.Migrations()); err != nil {
		t.Fatalf("apply Task Runtime Migration: %v", err)
	}
	return connection
}

func insertCommittedTaskGraph(
	t *testing.T,
	connection *pgx.Conn,
	taskID string,
	runID string,
	executionID string,
	now time.Time,
) {
	t.Helper()
	tx, err := connection.Begin(context.Background())
	if err != nil {
		t.Fatalf("begin valid graph transaction: %v", err)
	}
	insertValidTaskGraph(t, tx, taskID, runID, executionID, now)
	if err := tx.Commit(context.Background()); err != nil {
		t.Fatalf("commit valid graph: %v", err)
	}
}

func insertValidTaskGraph(t *testing.T, tx pgx.Tx, taskID string, runID string, executionID string, now time.Time) {
	t.Helper()
	if _, err := tx.Exec(context.Background(), `
INSERT INTO task (
    task_id, agent_id, created_by, input, status, current_run_id,
    current_execution_version, deadline_at, queued_at, created_at
) VALUES ($1, 'agent', 'operator', 'input', 'Pending', $2, 1, $3, $4, $4)`,
		taskID, runID, now.Add(time.Hour), now,
	); err != nil {
		t.Fatalf("insert valid Task: %v", err)
	}
	if _, err := tx.Exec(context.Background(), `
INSERT INTO run (run_id, task_id, status, context)
VALUES ($1, $2, 'Pending', '{}'::jsonb)`, runID, taskID); err != nil {
		t.Fatalf("insert valid Run: %v", err)
	}
	if _, err := tx.Exec(context.Background(), `
INSERT INTO task_execution (
    task_execution_id, task_id, execution_version, status,
    execution_config_hash, created_at
) VALUES ($1, $2, 1, 'QUEUED', $3, $4)`,
		executionID, taskID, strings.Repeat("a", 64), now,
	); err != nil {
		t.Fatalf("insert valid TaskExecution: %v", err)
	}
}

func assertTaskRuntimeTables(t *testing.T, connection *pgx.Conn) {
	t.Helper()
	rows, err := connection.Query(context.Background(), `
SELECT table_name
FROM information_schema.tables
WHERE table_schema = current_schema() AND table_type = 'BASE TABLE'
ORDER BY table_name`)
	if err != nil {
		t.Fatalf("query Task Runtime tables: %v", err)
	}
	defer rows.Close()
	var tables []string
	for rows.Next() {
		var table string
		if err := rows.Scan(&table); err != nil {
			t.Fatalf("scan Task Runtime table: %v", err)
		}
		tables = append(tables, table)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate Task Runtime tables: %v", err)
	}
	want := []string{"agentops_schema_migrations", "command_receipt", "run", "task", "task_execution", "task_log"}
	if !reflect.DeepEqual(tables, want) {
		t.Fatalf("Task Runtime tables = %v, want %v", tables, want)
	}
}

func assertTaskRuntimeColumns(t *testing.T, connection *pgx.Conn) {
	t.Helper()
	wants := map[string][]string{
		"task":            {"task_id", "agent_id", "created_by", "input", "status", "current_run_id", "current_execution_version", "result_summary", "error_code", "deadline_at", "queued_at", "created_at", "started_at", "ended_at"},
		"run":             {"run_id", "task_id", "status", "plan_id", "current_step_id", "context", "error_code", "started_at", "ended_at"},
		"task_execution":  {"task_execution_id", "task_id", "execution_version", "worker_id", "status", "execution_config_hash", "observed_config_hash", "error_code", "invariant_code", "termination_reason", "created_at", "started_at", "ended_at"},
		"command_receipt": {"command_id", "command_type", "target_id", "request_fingerprint", "response", "created_at"},
		"task_log":        {"log_id", "task_id", "run_id", "step_id", "execution_version", "level", "event", "message", "operator", "created_at"},
	}
	for table, want := range wants {
		rows, err := connection.Query(context.Background(), `
SELECT column_name
FROM information_schema.columns
WHERE table_schema = current_schema() AND table_name = $1
ORDER BY ordinal_position`, table)
		if err != nil {
			t.Fatalf("query %s columns: %v", table, err)
		}
		var got []string
		for rows.Next() {
			var column string
			if err := rows.Scan(&column); err != nil {
				rows.Close()
				t.Fatalf("scan %s column: %v", table, err)
			}
			got = append(got, column)
		}
		rows.Close()
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("%s columns = %v, want %v", table, got, want)
		}
	}
}

func assertTaskRuntimeIndexes(t *testing.T, connection *pgx.Conn) {
	t.Helper()
	for _, index := range taskRuntimeIndexSpecs {
		assertTaskRuntimeIndexDefinition(t, connection, index)
	}
}

func assertPostgreSQLError(t *testing.T, err error) {
	t.Helper()
	if err == nil {
		t.Fatal("PostgreSQL operation error = nil, want constraint failure")
	}
}
