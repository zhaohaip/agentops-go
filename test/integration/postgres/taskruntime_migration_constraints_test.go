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
)

func TestTaskRuntimeMigrationColumnNullability(t *testing.T) {
	t.Parallel()
	connection := openTaskRuntimeMigrationSchema(t)
	wants := map[string]map[string]bool{
		"task": {
			"task_id": false, "agent_id": false, "created_by": false, "input": false,
			"status": false, "current_run_id": false, "current_execution_version": false,
			"result_summary": true, "error_code": true, "deadline_at": false,
			"queued_at": true, "created_at": false, "started_at": true, "ended_at": true,
		},
		"run": {
			"run_id": false, "task_id": false, "status": false, "plan_id": true,
			"current_step_id": true, "context": false, "error_code": true,
			"started_at": true, "ended_at": true,
		},
		"task_execution": {
			"task_execution_id": false, "task_id": false, "execution_version": false,
			"worker_id": true, "status": false, "execution_config_hash": false,
			"observed_config_hash": true, "error_code": true, "invariant_code": true,
			"termination_reason": true, "created_at": false, "started_at": true, "ended_at": true,
		},
		"command_receipt": {
			"command_id": false, "command_type": false, "target_id": false,
			"request_fingerprint": false, "response": false, "created_at": false,
		},
		"task_log": {
			"log_id": false, "task_id": false, "run_id": false, "step_id": true,
			"execution_version": true, "level": false, "event": false, "message": false,
			"operator": false, "created_at": false,
		},
	}

	for table, want := range wants {
		rows, err := connection.Query(context.Background(), `
SELECT column_name, is_nullable = 'YES'
FROM information_schema.columns
WHERE table_schema = current_schema() AND table_name = $1`, table)
		if err != nil {
			t.Fatalf("query %s nullability: %v", table, err)
		}
		got := make(map[string]bool)
		for rows.Next() {
			var column string
			var nullable bool
			if err := rows.Scan(&column, &nullable); err != nil {
				rows.Close()
				t.Fatalf("scan %s nullability: %v", table, err)
			}
			got[column] = nullable
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			t.Fatalf("iterate %s nullability: %v", table, err)
		}
		rows.Close()
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("%s nullability = %v, want %v", table, got, want)
		}
	}
}

func TestTaskRuntimeMigrationCheckAndNullConstraints(t *testing.T) {
	t.Parallel()
	connection := openTaskRuntimeMigrationSchema(t)
	tests := []migrationConstraintCase{
		{name: "Task required input", statement: "UPDATE task SET input = NULL WHERE task_id = 'task-constraint'", code: "23502", table: "task", column: "input"},
		{name: "Task task_id nonempty", statement: "UPDATE task SET task_id = '' WHERE task_id = 'task-constraint'", code: "23514", constraint: "task_task_id_check"},
		{name: "Task agent_id nonempty", statement: "UPDATE task SET agent_id = '' WHERE task_id = 'task-constraint'", code: "23514", constraint: "task_agent_id_check"},
		{name: "Task created_by nonempty", statement: "UPDATE task SET created_by = '' WHERE task_id = 'task-constraint'", code: "23514", constraint: "task_created_by_check"},
		{name: "Task status closed", prepare: "ALTER TABLE task DROP CONSTRAINT task_error_check", statement: "UPDATE task SET status = 'BLOCKED' WHERE task_id = 'task-constraint'", code: "23514", constraint: "task_status_check"},
		{name: "Task current_run_id nonempty", statement: "UPDATE task SET current_run_id = '' WHERE task_id = 'task-constraint'", code: "23514", constraint: "task_current_run_id_check"},
		{name: "Task execution version positive", statement: "UPDATE task SET current_execution_version = 0 WHERE task_id = 'task-constraint'", code: "23514", constraint: "task_current_execution_version_check"},
		{name: "Task error code closed", prepare: "ALTER TABLE task DROP CONSTRAINT task_error_check", statement: "UPDATE task SET error_code = 'UNKNOWN' WHERE task_id = 'task-constraint'", code: "23514", constraint: "task_error_code_check"},
		{name: "Task time order", statement: "UPDATE task SET deadline_at = created_at - interval '1 second' WHERE task_id = 'task-constraint'", code: "23514", constraint: "task_time_order_check"},
		{name: "Task terminal time", statement: "UPDATE task SET ended_at = created_at WHERE task_id = 'task-constraint'", code: "23514", constraint: "task_terminal_time_check"},
		{name: "Task status error relation", statement: "UPDATE task SET error_code = 'TaskCancelled' WHERE task_id = 'task-constraint'", code: "23514", constraint: "task_error_check"},

		{name: "Run required context", statement: "UPDATE run SET context = NULL WHERE run_id = 'run-constraint'", code: "23502", table: "run", column: "context"},
		{name: "Run run_id nonempty", statement: "UPDATE run SET run_id = '' WHERE run_id = 'run-constraint'", code: "23514", constraint: "run_run_id_check"},
		{name: "Run task_id nonempty", statement: "UPDATE run SET task_id = '' WHERE run_id = 'run-constraint'", code: "23514", constraint: "run_task_id_check"},
		{name: "Run status closed", prepare: "ALTER TABLE run DROP CONSTRAINT run_error_check", statement: "UPDATE run SET status = 'BLOCKED' WHERE run_id = 'run-constraint'", code: "23514", constraint: "run_status_check"},
		{name: "Run context object", statement: "UPDATE run SET context = '[]'::jsonb WHERE run_id = 'run-constraint'", code: "23514", constraint: "run_context_check"},
		{name: "Run error code closed", prepare: "ALTER TABLE run DROP CONSTRAINT run_error_check", statement: "UPDATE run SET error_code = 'UNKNOWN' WHERE run_id = 'run-constraint'", code: "23514", constraint: "run_error_code_check"},
		{name: "Run terminal time", statement: "UPDATE run SET ended_at = now() WHERE run_id = 'run-constraint'", code: "23514", constraint: "run_terminal_time_check"},
		{name: "Run status error relation", statement: "UPDATE run SET error_code = 'TaskCancelled' WHERE run_id = 'run-constraint'", code: "23514", constraint: "run_error_check"},

		{name: "TaskExecution required hash", statement: "UPDATE task_execution SET execution_config_hash = NULL WHERE task_execution_id = 'execution-constraint'", code: "23502", table: "task_execution", column: "execution_config_hash"},
		{name: "TaskExecution id nonempty", statement: "UPDATE task_execution SET task_execution_id = '' WHERE task_execution_id = 'execution-constraint'", code: "23514", constraint: "task_execution_task_execution_id_check"},
		{name: "TaskExecution task_id nonempty", statement: "UPDATE task_execution SET task_id = '' WHERE task_execution_id = 'execution-constraint'", code: "23514", constraint: "task_execution_task_id_check"},
		{name: "TaskExecution version positive", statement: "UPDATE task_execution SET execution_version = 0 WHERE task_execution_id = 'execution-constraint'", code: "23514", constraint: "task_execution_execution_version_check"},
		{name: "TaskExecution worker nonempty", statement: "UPDATE task_execution SET status = 'INTERRUPTED', worker_id = '', ended_at = created_at WHERE task_execution_id = 'execution-constraint'", code: "23514", constraint: "task_execution_worker_id_check"},
		{name: "TaskExecution status closed", prepare: "ALTER TABLE task_execution DROP CONSTRAINT task_execution_status_fields_check", statement: "UPDATE task_execution SET status = 'BLOCKED' WHERE task_execution_id = 'execution-constraint'", code: "23514", constraint: "task_execution_status_check"},
		{name: "TaskExecution hash format", statement: "UPDATE task_execution SET execution_config_hash = 'bad' WHERE task_execution_id = 'execution-constraint'", code: "23514", constraint: "task_execution_execution_config_hash_check"},
		{name: "TaskExecution observed hash format", statement: "UPDATE task_execution SET status = 'INTERRUPTED', observed_config_hash = 'bad', error_code = 'CONFIG_VERSION_MISMATCH', ended_at = created_at WHERE task_execution_id = 'execution-constraint'", code: "23514", constraint: "task_execution_observed_config_hash_check"},
		{name: "TaskExecution error code closed", statement: "UPDATE task_execution SET error_code = 'UNKNOWN' WHERE task_execution_id = 'execution-constraint'", code: "23514", constraint: "task_execution_error_code_check"},
		{name: "TaskExecution invariant closed", statement: "UPDATE task_execution SET invariant_code = 'UNKNOWN', error_code = 'DATA_INCONSISTENT' WHERE task_execution_id = 'execution-constraint'", code: "23514", constraint: "task_execution_invariant_code_check"},
		{name: "TaskExecution termination closed", statement: "UPDATE task_execution SET status = 'FAILED', termination_reason = 'UNKNOWN', ended_at = created_at WHERE task_execution_id = 'execution-constraint'", code: "23514", constraint: "task_execution_termination_reason_check"},
		{name: "TaskExecution time order", statement: "UPDATE task_execution SET started_at = created_at - interval '1 second' WHERE task_execution_id = 'execution-constraint'", code: "23514", constraint: "task_execution_time_order_check"},
		{name: "TaskExecution status fields", statement: "UPDATE task_execution SET status = 'RUNNING' WHERE task_execution_id = 'execution-constraint'", code: "23514", constraint: "task_execution_status_fields_check"},
		{name: "TaskExecution observed relation", statement: "UPDATE task_execution SET observed_config_hash = repeat('b', 64) WHERE task_execution_id = 'execution-constraint'", code: "23514", constraint: "task_execution_observed_config_check"},
		{name: "TaskExecution invariant relation", statement: "UPDATE task_execution SET invariant_code = 'QUEUE_STATE_INVALID' WHERE task_execution_id = 'execution-constraint'", code: "23514", constraint: "task_execution_invariant_check"},
		{name: "TaskExecution termination relation", statement: "UPDATE task_execution SET termination_reason = 'CANCELLED' WHERE task_execution_id = 'execution-constraint'", code: "23514", constraint: "task_execution_termination_check"},

		{name: "CommandReceipt required response", statement: "UPDATE command_receipt SET response = NULL WHERE command_id = 'command-constraint'", code: "23502", table: "command_receipt", column: "response"},
		{name: "CommandReceipt id nonempty", statement: "UPDATE command_receipt SET command_id = '' WHERE command_id = 'command-constraint'", code: "23514", constraint: "command_receipt_command_id_check"},
		{name: "CommandReceipt type closed", statement: "UPDATE command_receipt SET command_type = 'Unknown' WHERE command_id = 'command-constraint'", code: "23514", constraint: "command_receipt_command_type_check"},
		{name: "CommandReceipt target nonempty", statement: "UPDATE command_receipt SET target_id = '' WHERE command_id = 'command-constraint'", code: "23514", constraint: "command_receipt_target_id_check"},
		{name: "CommandReceipt fingerprint format", statement: "UPDATE command_receipt SET request_fingerprint = 'bad' WHERE command_id = 'command-constraint'", code: "23514", constraint: "command_receipt_request_fingerprint_check"},
		{name: "CommandReceipt response object", statement: "UPDATE command_receipt SET response = '[]'::jsonb WHERE command_id = 'command-constraint'", code: "23514", constraint: "command_receipt_response_check"},

		{name: "TaskLog required message", statement: "UPDATE task_log SET message = NULL WHERE log_id = 'log-constraint'", code: "23502", table: "task_log", column: "message"},
		{name: "TaskLog id nonempty", statement: "UPDATE task_log SET log_id = '' WHERE log_id = 'log-constraint'", code: "23514", constraint: "task_log_log_id_check"},
		{name: "TaskLog task_id nonempty", statement: "UPDATE task_log SET task_id = '' WHERE log_id = 'log-constraint'", code: "23514", constraint: "task_log_task_id_check"},
		{name: "TaskLog run_id nonempty", statement: "UPDATE task_log SET run_id = '' WHERE log_id = 'log-constraint'", code: "23514", constraint: "task_log_run_id_check"},
		{name: "TaskLog version positive", statement: "UPDATE task_log SET execution_version = 0 WHERE log_id = 'log-constraint'", code: "23514", constraint: "task_log_execution_version_check"},
		{name: "TaskLog level closed", statement: "UPDATE task_log SET level = 'Debug' WHERE log_id = 'log-constraint'", code: "23514", constraint: "task_log_level_check"},
		{name: "TaskLog event nonempty", statement: "UPDATE task_log SET event = '' WHERE log_id = 'log-constraint'", code: "23514", constraint: "task_log_event_check"},
		{name: "TaskLog operator nonempty", statement: "UPDATE task_log SET operator = '' WHERE log_id = 'log-constraint'", code: "23514", constraint: "task_log_operator_check"},
	}

	for _, current := range tests {
		current := current
		t.Run(current.name, func(t *testing.T) {
			runMigrationConstraintCase(t, connection, current)
		})
	}
}

func TestTaskRuntimeMigrationForeignKeysAndUniqueness(t *testing.T) {
	t.Parallel()
	connection := openTaskRuntimeMigrationSchema(t)
	tests := []migrationConstraintCase{
		{name: "Task current Run FK", statement: "UPDATE task SET current_run_id = 'run-missing' WHERE task_id = 'task-constraint'", code: "23503", constraint: "task_current_run_foreign_key"},
		{name: "Task current Execution FK", statement: "UPDATE task SET current_execution_version = 2 WHERE task_id = 'task-constraint'", code: "23503", constraint: "task_current_execution_foreign_key"},
		{name: "Run Task FK", statement: "INSERT INTO run (run_id, task_id, status, context) VALUES ('run-missing-task', 'task-missing', 'Pending', '{}')", code: "23503", constraint: "run_task_foreign_key"},
		{name: "TaskExecution Task FK", statement: "INSERT INTO task_execution (task_execution_id, task_id, execution_version, status, execution_config_hash, created_at) VALUES ('execution-missing-task', 'task-missing', 1, 'QUEUED', repeat('a', 64), now())", code: "23503", constraint: "task_execution_task_foreign_key"},
		{name: "TaskLog Run FK", statement: "INSERT INTO task_log (log_id, task_id, run_id, level, event, message, operator, created_at) VALUES ('log-missing-run', 'task-constraint', 'run-missing', 'Info', 'event', '', 'System', now())", code: "23503", constraint: "task_log_run_foreign_key"},
		{name: "TaskLog Execution FK", statement: "INSERT INTO task_log (log_id, task_id, run_id, execution_version, level, event, message, operator, created_at) VALUES ('log-missing-execution', 'task-constraint', 'run-constraint', 2, 'Info', 'event', '', 'System', now())", code: "23503", constraint: "task_log_execution_foreign_key"},

		{name: "Task primary key", statement: "INSERT INTO task (task_id, agent_id, created_by, input, status, current_run_id, current_execution_version, deadline_at, queued_at, created_at) VALUES ('task-constraint', 'agent', 'operator', 'input', 'Pending', 'run-other', 1, now() + interval '1 hour', now(), now())", code: "23505", constraint: "task_pkey"},
		{name: "Run primary key", statement: "INSERT INTO run (run_id, task_id, status, context) VALUES ('run-constraint', 'task-constraint', 'Pending', '{}')", code: "23505", constraint: "run_pkey"},
		{name: "one Run per Task", statement: "INSERT INTO run (run_id, task_id, status, context) VALUES ('run-second', 'task-constraint', 'Pending', '{}')", code: "23505", constraint: "run_task_unique"},
		{name: "TaskExecution primary key", statement: "INSERT INTO task_execution (task_execution_id, task_id, execution_version, status, execution_config_hash, created_at) VALUES ('execution-constraint', 'task-constraint', 2, 'QUEUED', repeat('a', 64), now())", code: "23505", constraint: "task_execution_pkey"},
		{name: "TaskExecution version unique", statement: "INSERT INTO task_execution (task_execution_id, task_id, execution_version, status, execution_config_hash, created_at) VALUES ('execution-second', 'task-constraint', 1, 'QUEUED', repeat('a', 64), now())", code: "23505", constraint: "task_execution_task_version_unique"},
		{name: "CommandReceipt primary key", statement: "INSERT INTO command_receipt (command_id, command_type, target_id, request_fingerprint, response, created_at) VALUES ('command-constraint', 'Create', 'task-constraint', repeat('b', 64), '{}', now())", code: "23505", constraint: "command_receipt_pkey"},
		{name: "TaskLog primary key", statement: "INSERT INTO task_log (log_id, task_id, run_id, level, event, message, operator, created_at) VALUES ('log-constraint', 'task-constraint', 'run-constraint', 'Info', 'event', '', 'System', now())", code: "23505", constraint: "task_log_pkey"},
	}

	for _, current := range tests {
		current := current
		t.Run(current.name, func(t *testing.T) {
			runMigrationConstraintCase(t, connection, current)
		})
	}
}

func TestTaskRuntimeMigrationJSONFields(t *testing.T) {
	t.Parallel()
	connection := openTaskRuntimeMigrationSchema(t)
	tests := []migrationConstraintCase{
		{name: "Run malformed JSON", statement: "UPDATE run SET context = '{'::jsonb WHERE run_id = 'run-constraint'", code: "22P02"},
		{name: "Run non-object JSON", statement: "UPDATE run SET context = '[]'::jsonb WHERE run_id = 'run-constraint'", code: "23514", constraint: "run_context_check"},
		{name: "CommandReceipt malformed JSON", statement: "UPDATE command_receipt SET response = '{'::jsonb WHERE command_id = 'command-constraint'", code: "22P02"},
		{name: "CommandReceipt non-object JSON", statement: "UPDATE command_receipt SET response = '[]'::jsonb WHERE command_id = 'command-constraint'", code: "23514", constraint: "command_receipt_response_check"},
	}

	for _, current := range tests {
		current := current
		t.Run(current.name, func(t *testing.T) {
			runMigrationConstraintCase(t, connection, current)
		})
	}
}

type migrationConstraintCase struct {
	name       string
	prepare    string
	statement  string
	code       string
	constraint string
	table      string
	column     string
}

func runMigrationConstraintCase(t *testing.T, connection *pgx.Conn, test migrationConstraintCase) {
	t.Helper()
	tx, err := connection.Begin(context.Background())
	if err != nil {
		t.Fatalf("begin %s constraint transaction: %v", test.name, err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	insertValidConstraintGraph(t, tx)
	if test.prepare != "" {
		if _, err := tx.Exec(context.Background(), test.prepare); err != nil {
			t.Fatalf("prepare isolated %s constraint: %v", test.name, err)
		}
	}
	_, err = tx.Exec(context.Background(), test.statement)
	assertPostgreSQLViolation(t, err, test.code, test.constraint, test.table, test.column)
}

func insertValidConstraintGraph(t *testing.T, tx pgx.Tx) {
	t.Helper()
	now := time.Now().UTC()
	insertValidTaskGraph(t, tx, "task-constraint", "run-constraint", "execution-constraint", now)
	if _, err := tx.Exec(context.Background(), `
INSERT INTO command_receipt (
    command_id, command_type, target_id, request_fingerprint, response, created_at
) VALUES ('command-constraint', 'Create', 'task-constraint', $1, '{"ok":true}'::jsonb, $2)`, strings.Repeat("b", 64), now); err != nil {
		t.Fatalf("insert valid constraint CommandReceipt: %v", err)
	}
	if _, err := tx.Exec(context.Background(), `
INSERT INTO task_log (
    log_id, task_id, run_id, execution_version, level, event, message, operator, created_at
) VALUES ('log-constraint', 'task-constraint', 'run-constraint', 1, 'Info', 'event', '', 'System', $1)`, now); err != nil {
		t.Fatalf("insert valid constraint TaskLog: %v", err)
	}
	if _, err := tx.Exec(context.Background(), "SET CONSTRAINTS ALL IMMEDIATE"); err != nil {
		t.Fatalf("validate legal constraint graph: %v", err)
	}
}

func assertPostgreSQLViolation(
	t *testing.T,
	err error,
	code string,
	constraint string,
	table string,
	column string,
) {
	t.Helper()
	if err == nil {
		t.Fatal("PostgreSQL operation error = nil, want constraint violation")
	}
	var databaseError *pgconn.PgError
	if !errors.As(err, &databaseError) {
		t.Fatalf("PostgreSQL error = %T %v, want *pgconn.PgError", err, err)
	}
	if databaseError.Code != code {
		t.Fatalf("PostgreSQL SQLSTATE = %s, want %s: %v", databaseError.Code, code, err)
	}
	if constraint != "" && databaseError.ConstraintName != constraint {
		t.Fatalf("PostgreSQL constraint = %q, want %q: %v", databaseError.ConstraintName, constraint, err)
	}
	if table != "" && databaseError.TableName != table {
		t.Fatalf("PostgreSQL table = %q, want %q: %v", databaseError.TableName, table, err)
	}
	if column != "" && databaseError.ColumnName != column {
		t.Fatalf("PostgreSQL column = %q, want %q: %v", databaseError.ColumnName, column, err)
	}
}

type taskRuntimeIndexSpec struct {
	name      string
	table     string
	unique    bool
	primary   bool
	columns   []taskRuntimeIndexColumn
	predicate string
}

type taskRuntimeIndexColumn struct {
	name       string
	descending bool
	nullsFirst bool
}

var taskRuntimeIndexSpecs = []taskRuntimeIndexSpec{
	{name: "task_pkey", table: "task", unique: true, primary: true, columns: ascendingIndexColumns("task_id")},
	{name: "task_queue_fifo_index", table: "task", columns: ascendingIndexColumns("queued_at", "created_at", "task_id"), predicate: "(queued_at IS NOT NULL)"},
	{name: "run_pkey", table: "run", unique: true, primary: true, columns: ascendingIndexColumns("run_id")},
	{name: "run_task_unique", table: "run", unique: true, columns: ascendingIndexColumns("task_id")},
	{name: "run_task_identity_unique", table: "run", unique: true, columns: ascendingIndexColumns("task_id", "run_id")},
	{name: "task_execution_pkey", table: "task_execution", unique: true, primary: true, columns: ascendingIndexColumns("task_execution_id")},
	{name: "task_execution_task_version_unique", table: "task_execution", unique: true, columns: ascendingIndexColumns("task_id", "execution_version")},
	{name: "task_execution_status_worker_index", table: "task_execution", columns: ascendingIndexColumns("status", "worker_id", "task_id")},
	{name: "command_receipt_pkey", table: "command_receipt", unique: true, primary: true, columns: ascendingIndexColumns("command_id")},
	{name: "task_log_pkey", table: "task_log", unique: true, primary: true, columns: ascendingIndexColumns("log_id")},
	{name: "task_log_task_created_index", table: "task_log", columns: ascendingIndexColumns("task_id", "created_at", "log_id")},
}

func ascendingIndexColumns(names ...string) []taskRuntimeIndexColumn {
	columns := make([]taskRuntimeIndexColumn, 0, len(names))
	for _, name := range names {
		columns = append(columns, taskRuntimeIndexColumn{name: name})
	}
	return columns
}

func assertTaskRuntimeIndexDefinition(t *testing.T, connection *pgx.Conn, want taskRuntimeIndexSpec) {
	t.Helper()
	var (
		table     string
		unique    bool
		primary   bool
		predicate string
	)
	err := connection.QueryRow(context.Background(), `
SELECT table_class.relname,
       index_meta.indisunique,
       index_meta.indisprimary,
       COALESCE(pg_get_expr(index_meta.indpred, index_meta.indrelid), '')
FROM pg_index AS index_meta
JOIN pg_class AS index_class ON index_class.oid = index_meta.indexrelid
JOIN pg_class AS table_class ON table_class.oid = index_meta.indrelid
JOIN pg_namespace AS namespace ON namespace.oid = index_class.relnamespace
WHERE namespace.nspname = current_schema()
  AND index_class.relname = $1`, want.name).Scan(&table, &unique, &primary, &predicate)
	if err != nil {
		t.Fatalf("query index %s definition: %v", want.name, err)
	}
	if table != want.table || unique != want.unique || primary != want.primary || normalizeSQL(predicate) != normalizeSQL(want.predicate) {
		t.Fatalf(
			"index %s metadata = (table:%s unique:%t primary:%t predicate:%q), want (%s %t %t %q)",
			want.name, table, unique, primary, predicate, want.table, want.unique, want.primary, want.predicate,
		)
	}

	rows, err := connection.Query(context.Background(), `
SELECT attribute.attname,
       (index_meta.indoption[key.ordinality - 1] & 1) <> 0 AS descending,
       (index_meta.indoption[key.ordinality - 1] & 2) <> 0 AS nulls_first
FROM pg_index AS index_meta
JOIN pg_class AS index_class ON index_class.oid = index_meta.indexrelid
JOIN pg_namespace AS namespace ON namespace.oid = index_class.relnamespace
CROSS JOIN LATERAL unnest(index_meta.indkey) WITH ORDINALITY AS key(attnum, ordinality)
JOIN pg_attribute AS attribute
  ON attribute.attrelid = index_meta.indrelid
 AND attribute.attnum = key.attnum
WHERE namespace.nspname = current_schema()
  AND index_class.relname = $1
  AND key.ordinality <= index_meta.indnkeyatts
ORDER BY key.ordinality`, want.name)
	if err != nil {
		t.Fatalf("query index %s columns: %v", want.name, err)
	}
	defer rows.Close()
	var got []taskRuntimeIndexColumn
	for rows.Next() {
		var column taskRuntimeIndexColumn
		if err := rows.Scan(&column.name, &column.descending, &column.nullsFirst); err != nil {
			t.Fatalf("scan index %s column: %v", want.name, err)
		}
		got = append(got, column)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate index %s columns: %v", want.name, err)
	}
	if !reflect.DeepEqual(got, want.columns) {
		t.Fatalf("index %s columns = %+v, want %+v", want.name, got, want.columns)
	}
}

func normalizeSQL(value string) string {
	return strings.Join(strings.Fields(value), " ")
}
