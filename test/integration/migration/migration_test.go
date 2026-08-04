package migration_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"reflect"
	"sort"
	"sync/atomic"
	"testing"

	"github.com/jackc/pgx/v5"
	adapter "github.com/zhaohaip/agentops-go/internal/adapter/postgres/migration"
	productionmigrations "github.com/zhaohaip/agentops-go/migrations"
)

const testPostgreSQLDSNEnvironment = "AGENTOPS_TEST_POSTGRES_DSN"

var schemaSequence atomic.Uint64

func TestEmptyMigrationSetInitializesOnlyMetadata(t *testing.T) {
	t.Parallel()

	conn := openIsolatedSchema(t)
	runner := newRunner(t, conn, productionmigrations.All())
	if err := runner.Migrate(context.Background()); err != nil {
		t.Fatalf("Migrate() error = %v", err)
	}

	if got := appliedVersions(t, conn); len(got) != 0 {
		t.Fatalf("applied versions = %v, want empty", got)
	}
	tables := schemaTables(t, conn)
	if want := []string{"agentops_schema_migrations"}; !reflect.DeepEqual(tables, want) {
		t.Fatalf("schema tables = %v, want %v", tables, want)
	}

	forbidden := []string{
		"task", "run", "task_execution", "command_receipt", "task_log", "checkpoint",
		"plan", "step", "tool_execution", "approval", "report",
	}
	for _, table := range forbidden {
		if tableExists(t, conn, table) {
			t.Fatalf("forbidden business table %q exists", table)
		}
	}
}

func TestSingleMigration(t *testing.T) {
	t.Parallel()

	conn := openIsolatedSchema(t)
	definition := adapter.Migration{
		Version:    42,
		Name:       "create_single_probe",
		Statements: []string{"CREATE TABLE single_probe (id BIGINT PRIMARY KEY)"},
	}
	if err := newRunner(t, conn, []adapter.Migration{definition}).Migrate(context.Background()); err != nil {
		t.Fatalf("Migrate() error = %v", err)
	}

	if got, want := appliedVersions(t, conn), []int64{42}; !reflect.DeepEqual(got, want) {
		t.Fatalf("applied versions = %v, want %v", got, want)
	}
	if !tableExists(t, conn, "single_probe") {
		t.Fatal("single migration did not create probe table")
	}
}

func TestMigrationsExecuteInDeterministicOrderAndAreIdempotent(t *testing.T) {
	t.Parallel()

	conn := openIsolatedSchema(t)
	definitions := []adapter.Migration{
		{
			Version:    30,
			Name:       "insert_third",
			Statements: []string{"INSERT INTO migration_order (version) VALUES (30)"},
		},
		{
			Version: 10,
			Name:    "create_order_probe",
			Statements: []string{
				"CREATE TABLE migration_order (position BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY, version BIGINT NOT NULL)",
				"INSERT INTO migration_order (version) VALUES (10)",
			},
		},
		{
			Version:    20,
			Name:       "insert_second",
			Statements: []string{"INSERT INTO migration_order (version) VALUES (20)"},
		},
	}
	runner := newRunner(t, conn, definitions)
	if err := runner.Migrate(context.Background()); err != nil {
		t.Fatalf("first Migrate() error = %v", err)
	}

	if got, want := appliedVersions(t, conn), []int64{10, 20, 30}; !reflect.DeepEqual(got, want) {
		t.Fatalf("applied versions = %v, want %v", got, want)
	}
	if got, want := queryInt64s(t, conn, "SELECT version FROM migration_order ORDER BY position"), []int64{10, 20, 30}; !reflect.DeepEqual(got, want) {
		t.Fatalf("execution order = %v, want %v", got, want)
	}

	if err := runner.Migrate(context.Background()); err != nil {
		t.Fatalf("second Migrate() error = %v", err)
	}
	if got, want := queryInt64s(t, conn, "SELECT version FROM migration_order ORDER BY position"), []int64{10, 20, 30}; !reflect.DeepEqual(got, want) {
		t.Fatalf("rows after repeat = %v, want %v", got, want)
	}
}

func TestFailedMigrationRollsBackAndCanBeFixed(t *testing.T) {
	t.Parallel()

	conn := openIsolatedSchema(t)
	first := adapter.Migration{
		Version: 1,
		Name:    "create_probe",
		Statements: []string{
			"CREATE TABLE rollback_probe (value BIGINT NOT NULL)",
			"INSERT INTO rollback_probe (value) VALUES (1)",
		},
	}
	broken := adapter.Migration{
		Version: 2,
		Name:    "insert_broken",
		Statements: []string{
			"INSERT INTO rollback_probe (value) VALUES (2)",
			"CREATE TABLE syntactically_broken (",
		},
	}
	third := adapter.Migration{
		Version:    3,
		Name:       "insert_third",
		Statements: []string{"INSERT INTO rollback_probe (value) VALUES (3)"},
	}

	brokenRunner := newRunner(t, conn, []adapter.Migration{third, broken, first})
	if err := brokenRunner.Migrate(context.Background()); err == nil {
		t.Fatal("Migrate() error = nil, want failing second version")
	}
	if got, want := appliedVersions(t, conn), []int64{1}; !reflect.DeepEqual(got, want) {
		t.Fatalf("applied versions after failure = %v, want %v", got, want)
	}
	if got, want := queryInt64s(t, conn, "SELECT value FROM rollback_probe ORDER BY value"), []int64{1}; !reflect.DeepEqual(got, want) {
		t.Fatalf("probe values after rollback = %v, want %v", got, want)
	}

	fixed := adapter.Migration{
		Version:    2,
		Name:       "insert_broken",
		Statements: []string{"INSERT INTO rollback_probe (value) VALUES (2)"},
	}
	fixedRunner := newRunner(t, conn, []adapter.Migration{third, fixed, first})
	if err := fixedRunner.Migrate(context.Background()); err != nil {
		t.Fatalf("fixed Migrate() error = %v", err)
	}
	if got, want := appliedVersions(t, conn), []int64{1, 2, 3}; !reflect.DeepEqual(got, want) {
		t.Fatalf("applied versions after fix = %v, want %v", got, want)
	}
	if got, want := queryInt64s(t, conn, "SELECT value FROM rollback_probe ORDER BY value"), []int64{1, 2, 3}; !reflect.DeepEqual(got, want) {
		t.Fatalf("probe values after fix = %v, want %v", got, want)
	}
}

func TestRunnerReportsLostDatabaseConnection(t *testing.T) {
	t.Parallel()

	dsn := os.Getenv(testPostgreSQLDSNEnvironment)
	if dsn == "" {
		t.Skipf("%s is not set", testPostgreSQLDSNEnvironment)
	}

	conn, err := pgx.Connect(context.Background(), dsn)
	if err != nil {
		t.Fatalf("connect test PostgreSQL: %v", err)
	}
	if err := conn.Close(context.Background()); err != nil {
		t.Fatalf("close test PostgreSQL: %v", err)
	}

	runner := newRunner(t, conn, nil)
	if err := runner.Migrate(context.Background()); err == nil {
		t.Fatal("Migrate() error = nil, want lost connection error")
	}
}

func TestAppliedHistoryAnomaliesAreRejected(t *testing.T) {
	t.Parallel()

	t.Run("changed content", func(t *testing.T) {
		conn := openIsolatedSchema(t)
		original := adapter.Migration{Version: 1, Name: "first", Statements: []string{"CREATE TABLE original_probe (id BIGINT)"}}
		if err := newRunner(t, conn, []adapter.Migration{original}).Migrate(context.Background()); err != nil {
			t.Fatalf("initial Migrate() error = %v", err)
		}

		changed := adapter.Migration{Version: 1, Name: "first", Statements: []string{"CREATE TABLE changed_probe (id BIGINT)"}}
		err := newRunner(t, conn, []adapter.Migration{changed}).Migrate(context.Background())
		if !errors.Is(err, adapter.ErrAppliedMigrationMismatch) {
			t.Fatalf("Migrate() error = %v, want mismatch", err)
		}
	})

	t.Run("unknown applied version", func(t *testing.T) {
		conn := openIsolatedSchema(t)
		known := adapter.Migration{Version: 1, Name: "first", Statements: []string{"SELECT 1"}}
		if err := newRunner(t, conn, []adapter.Migration{known}).Migrate(context.Background()); err != nil {
			t.Fatalf("initial Migrate() error = %v", err)
		}
		if _, err := conn.Exec(
			context.Background(),
			"INSERT INTO agentops_schema_migrations (version, name, checksum) VALUES (99, 'unknown', repeat('a', 64))",
		); err != nil {
			t.Fatalf("insert unknown version: %v", err)
		}

		err := newRunner(t, conn, []adapter.Migration{known}).Migrate(context.Background())
		if !errors.Is(err, adapter.ErrUnknownAppliedVersion) {
			t.Fatalf("Migrate() error = %v, want unknown version", err)
		}
	})

	t.Run("missing earlier version", func(t *testing.T) {
		conn := openIsolatedSchema(t)
		definitions := []adapter.Migration{
			{Version: 1, Name: "first", Statements: []string{"SELECT 1"}},
			{Version: 2, Name: "second", Statements: []string{"SELECT 2"}},
		}
		runner := newRunner(t, conn, definitions)
		if err := runner.Migrate(context.Background()); err != nil {
			t.Fatalf("initial Migrate() error = %v", err)
		}
		if _, err := conn.Exec(context.Background(), "DELETE FROM agentops_schema_migrations WHERE version = 1"); err != nil {
			t.Fatalf("delete earlier version: %v", err)
		}

		err := runner.Migrate(context.Background())
		if !errors.Is(err, adapter.ErrAppliedHistoryInconsistent) {
			t.Fatalf("Migrate() error = %v, want inconsistent history", err)
		}
	})

	t.Run("damaged metadata table", func(t *testing.T) {
		conn := openIsolatedSchema(t)
		if _, err := conn.Exec(context.Background(), "CREATE TABLE agentops_schema_migrations (version BIGINT)"); err != nil {
			t.Fatalf("create damaged metadata: %v", err)
		}

		err := newRunner(t, conn, nil).Migrate(context.Background())
		if err == nil {
			t.Fatal("Migrate() error = nil, want damaged metadata error")
		}
	})
}

func newRunner(t *testing.T, conn *pgx.Conn, definitions []adapter.Migration) *adapter.Runner {
	t.Helper()

	runner, err := adapter.NewRunner(conn, definitions)
	if err != nil {
		t.Fatalf("NewRunner() error = %v", err)
	}
	return runner
}

func openIsolatedSchema(t *testing.T) *pgx.Conn {
	t.Helper()

	dsn := os.Getenv(testPostgreSQLDSNEnvironment)
	if dsn == "" {
		t.Skipf("%s is not set", testPostgreSQLDSNEnvironment)
	}

	ctx := context.Background()
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("connect test PostgreSQL: %v", err)
	}
	t.Cleanup(func() {
		if err := conn.Close(context.Background()); err != nil {
			t.Errorf("close test PostgreSQL: %v", err)
		}
	})

	schema := fmt.Sprintf("agentops_migration_%d_%d", os.Getpid(), schemaSequence.Add(1))
	identifier := pgx.Identifier{schema}.Sanitize()
	if _, err := conn.Exec(ctx, "CREATE SCHEMA "+identifier); err != nil {
		t.Fatalf("create isolated schema: %v", err)
	}
	t.Cleanup(func() {
		if _, err := conn.Exec(context.Background(), "DROP SCHEMA "+identifier+" CASCADE"); err != nil {
			t.Errorf("drop isolated schema: %v", err)
		}
	})
	if _, err := conn.Exec(ctx, "SET search_path TO "+identifier); err != nil {
		t.Fatalf("set isolated search_path: %v", err)
	}

	return conn
}

func appliedVersions(t *testing.T, conn *pgx.Conn) []int64 {
	t.Helper()
	return queryInt64s(t, conn, "SELECT version FROM agentops_schema_migrations ORDER BY version")
}

func queryInt64s(t *testing.T, conn *pgx.Conn, query string) []int64 {
	t.Helper()

	rows, err := conn.Query(context.Background(), query)
	if err != nil {
		t.Fatalf("query int64 values: %v", err)
	}
	defer rows.Close()

	var values []int64
	for rows.Next() {
		var value int64
		if err := rows.Scan(&value); err != nil {
			t.Fatalf("scan int64 value: %v", err)
		}
		values = append(values, value)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate int64 values: %v", err)
	}
	return values
}

func schemaTables(t *testing.T, conn *pgx.Conn) []string {
	t.Helper()

	rows, err := conn.Query(
		context.Background(),
		"SELECT table_name FROM information_schema.tables WHERE table_schema = current_schema()",
	)
	if err != nil {
		t.Fatalf("query schema tables: %v", err)
	}
	defer rows.Close()

	var tables []string
	for rows.Next() {
		var table string
		if err := rows.Scan(&table); err != nil {
			t.Fatalf("scan schema table: %v", err)
		}
		tables = append(tables, table)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate schema tables: %v", err)
	}
	sort.Strings(tables)
	return tables
}

func tableExists(t *testing.T, conn *pgx.Conn, table string) bool {
	t.Helper()

	var exists bool
	if err := conn.QueryRow(
		context.Background(),
		`SELECT EXISTS (
            SELECT 1
            FROM information_schema.tables
            WHERE table_schema = current_schema() AND table_name = $1
        )`,
		table,
	).Scan(&exists); err != nil {
		t.Fatalf("query table existence: %v", err)
	}
	return exists
}
