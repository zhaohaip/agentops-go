package postgres_test

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/zhaohaip/agentops-go/internal/adapter/postgres/migration"
	postgresruntime "github.com/zhaohaip/agentops-go/internal/adapter/postgres/runtime"
	fixtures "github.com/zhaohaip/agentops-go/test/fixtures/postgres"
	"github.com/zhaohaip/agentops-go/test/integration/postgres/postgrestest"
)

func TestSchemaIsolationSupportsParallelTestsAndAutomaticCleanup(t *testing.T) {
	var (
		namesMu sync.Mutex
		names   []string
	)

	t.Run("parallel schemas", func(t *testing.T) {
		for index := range 4 {
			index := index
			t.Run(fmt.Sprintf("schema-%d", index), func(t *testing.T) {
				t.Parallel()
				schema := postgrestest.NewSchema(t)
				namesMu.Lock()
				names = append(names, schema.Name)
				namesMu.Unlock()

				connection := postgrestest.Connect(t, schema.DSN)
				if _, err := connection.Exec(
					context.Background(),
					"CREATE TABLE isolation_probe (value BIGINT NOT NULL)",
				); err != nil {
					t.Fatalf("create isolated probe table: %v", err)
				}
				if _, err := connection.Exec(
					context.Background(),
					"INSERT INTO isolation_probe (value) VALUES ($1)",
					index,
				); err != nil {
					t.Fatalf("insert isolated probe: %v", err)
				}

				var currentSchema string
				var value int
				if err := connection.QueryRow(
					context.Background(),
					"SELECT current_schema(), value FROM isolation_probe",
				).Scan(&currentSchema, &value); err != nil {
					t.Fatalf("query isolated probe: %v", err)
				}
				if currentSchema != schema.Name || value != index {
					t.Fatalf("isolated result = (%q, %d), want (%q, %d)", currentSchema, value, schema.Name, index)
				}
			})
		}
	})

	admin := postgrestest.Connect(t, postgrestest.BaseDSN(t))
	for _, name := range names {
		var exists bool
		if err := admin.QueryRow(
			context.Background(),
			"SELECT EXISTS (SELECT 1 FROM pg_namespace WHERE nspname = $1)",
			name,
		).Scan(&exists); err != nil {
			t.Fatalf("query cleaned schema: %v", err)
		}
		if exists {
			t.Fatalf("isolated schema %q remained after test cleanup", name)
		}
	}
}

func TestDatabaseIsolationAllowsParallelAdvisoryLocks(t *testing.T) {
	var (
		namesMu sync.Mutex
		names   []string
		roles   []string
	)
	t.Run("parallel databases", func(t *testing.T) {
		for index := range 2 {
			t.Run(fmt.Sprintf("database-%d", index), func(t *testing.T) {
				t.Parallel()
				database := postgrestest.NewDatabase(t)
				identities := postgrestest.NewDatabaseIdentities(t, database)
				namesMu.Lock()
				names = append(names, database.Name)
				roles = append(roles,
					postgresUserFromDSN(t, identities.MigrationDSN),
					postgresUserFromDSN(t, identities.RuntimeWriteDSN),
					postgresUserFromDSN(t, identities.RuntimeReadDSN),
				)
				namesMu.Unlock()

				config := postgrestest.RuntimeConfig(t, identities, "127.0.0.1:0")
				first, err := postgresruntime.Open(context.Background(), config.PostgreSQL, config.Runtime, nil)
				if err != nil {
					t.Fatalf("open first isolated runtime: %v", err)
				}
				t.Cleanup(func() { closeFoundationRuntime(t, first) })

				second, err := postgresruntime.Open(context.Background(), config.PostgreSQL, config.Runtime, nil)
				if second != nil {
					closeFoundationRuntime(t, second)
				}
				if !errors.Is(err, postgresruntime.ErrAdvisoryLockUnavailable) {
					t.Fatalf("second runtime error = %v, want advisory lock unavailable", err)
				}
			})
		}
	})

	admin := postgrestest.Connect(t, postgrestest.BaseDSN(t))
	for _, name := range names {
		var exists bool
		if err := admin.QueryRow(
			context.Background(),
			"SELECT EXISTS (SELECT 1 FROM pg_database WHERE datname = $1)",
			name,
		).Scan(&exists); err != nil {
			t.Fatalf("query cleaned database: %v", err)
		}
		if exists {
			t.Fatalf("isolated database %q remained after test cleanup", name)
		}
	}
	for _, role := range roles {
		var exists bool
		if err := admin.QueryRow(
			context.Background(),
			"SELECT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = $1)",
			role,
		).Scan(&exists); err != nil {
			t.Fatalf("query cleaned role: %v", err)
		}
		if exists {
			t.Fatalf("isolated role %q remained after test cleanup", role)
		}
	}
}

func TestMigrationHarnessScenarios(t *testing.T) {
	testCases := []struct {
		name string
		run  func(*testing.T, *postgrestest.MigrationHarness, *pgx.Conn)
	}{
		{
			name: "empty migration set",
			run: func(t *testing.T, harness *postgrestest.MigrationHarness, _ *pgx.Conn) {
				if err := harness.Apply(context.Background(), nil); err != nil {
					t.Fatalf("apply empty migrations: %v", err)
				}
				assertAppliedVersions(t, harness, nil)
			},
		},
		{
			name: "specified migration set",
			run: func(t *testing.T, harness *postgrestest.MigrationHarness, _ *pgx.Conn) {
				if err := harness.Apply(context.Background(), fixtures.InitialMigrations()); err != nil {
					t.Fatalf("apply specified migrations: %v", err)
				}
				assertAppliedVersions(t, harness, []int64{1})
			},
		},
		{
			name: "incremental upgrade from previous set",
			run: func(t *testing.T, harness *postgrestest.MigrationHarness, _ *pgx.Conn) {
				if err := harness.Apply(context.Background(), fixtures.InitialMigrations()); err != nil {
					t.Fatalf("apply previous migrations: %v", err)
				}
				if err := harness.Apply(context.Background(), fixtures.CurrentMigrations()); err != nil {
					t.Fatalf("apply incremental migrations: %v", err)
				}
				assertAppliedVersions(t, harness, []int64{1, 2})
			},
		},
		{
			name: "repeat is idempotent",
			run: func(t *testing.T, harness *postgrestest.MigrationHarness, _ *pgx.Conn) {
				for range 2 {
					if err := harness.Apply(context.Background(), fixtures.CurrentMigrations()); err != nil {
						t.Fatalf("repeat current migrations: %v", err)
					}
				}
				assertAppliedVersions(t, harness, []int64{1, 2})
			},
		},
		{
			name: "failure rolls back and preserves SQL error",
			run: func(t *testing.T, harness *postgrestest.MigrationHarness, connection *pgx.Conn) {
				if err := harness.Apply(context.Background(), fixtures.InitialMigrations()); err != nil {
					t.Fatalf("apply previous migrations: %v", err)
				}
				broken := append(fixtures.InitialMigrations(), migration.Migration{
					Version: 2,
					Name:    "broken_increment",
					Statements: []string{
						"INSERT INTO repository_contract_probe (natural_key, value) VALUES ('rolled-back', 'value')",
						"CREATE TABLE broken_syntax (",
					},
				})
				err := harness.Apply(context.Background(), broken)
				var postgresError *pgconn.PgError
				if !errors.As(err, &postgresError) {
					t.Fatalf("migration error = %v, want transparent PostgreSQL error", err)
				}
				assertAppliedVersions(t, harness, []int64{1})

				var rows int
				if err := connection.QueryRow(
					context.Background(),
					"SELECT count(*) FROM repository_contract_probe WHERE natural_key = 'rolled-back'",
				).Scan(&rows); err != nil {
					t.Fatalf("query rolled back migration row: %v", err)
				}
				if rows != 0 {
					t.Fatalf("failed migration rows = %d, want 0", rows)
				}
			},
		},
	}

	for _, current := range testCases {
		current := current
		t.Run(current.name, func(t *testing.T) {
			t.Parallel()
			schema := postgrestest.NewSchema(t)
			connection := postgrestest.Connect(t, schema.DSN)
			current.run(t, postgrestest.NewMigrationHarness(connection), connection)
		})
	}
}

func TestMigrationHarnessSupportsFreshDatabase(t *testing.T) {
	database := postgrestest.NewDatabase(t)
	connection := postgrestest.Connect(t, database.DSN)
	harness := postgrestest.NewMigrationHarness(connection)
	if err := harness.Apply(context.Background(), fixtures.CurrentMigrations()); err != nil {
		t.Fatalf("apply migrations to fresh database: %v", err)
	}
	assertAppliedVersions(t, harness, []int64{1, 2})
}

func TestRepositoryContractFramework(t *testing.T) {
	postgrestest.RunRepositoryContract(t, postgrestest.RepositoryContract{
		Name:       "repository probe",
		Migrations: fixtures.CurrentMigrations(),
		Cases: []postgrestest.RepositoryCase{
			{Name: "conditional update", Run: verifyConditionalUpdate},
			{Name: "unique constraint and SQL error", Run: verifyUniqueConstraint},
			{Name: "commit and rollback", Run: verifyCommitAndRollback},
			{Name: "database clock and read-only boundary", Run: verifyClockAndReadOnlyBoundary},
			{Name: "concurrent competition", Run: verifyConcurrentCompetition},
		},
	})
}

func TestPhase0DatabaseContainsNoBusinessTables(t *testing.T) {
	connection := postgrestest.Connect(t, postgrestest.BaseDSN(t))
	forbidden := []string{
		"task", "run", "task_execution", "command_receipt", "task_log", "checkpoint",
		"plan", "step", "tool_execution", "approval", "report",
	}
	rows, err := connection.Query(context.Background(), `
SELECT table_schema, table_name
FROM information_schema.tables
WHERE table_schema NOT IN ('pg_catalog', 'information_schema')
  AND table_name = ANY($1::text[])
ORDER BY table_schema, table_name`, forbidden)
	if err != nil {
		t.Fatalf("query forbidden Phase 0 tables: %v", err)
	}
	defer rows.Close()

	var found []string
	for rows.Next() {
		var schema string
		var table string
		if err := rows.Scan(&schema, &table); err != nil {
			t.Fatalf("scan forbidden Phase 0 table: %v", err)
		}
		found = append(found, schema+"."+table)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate forbidden Phase 0 tables: %v", err)
	}
	if len(found) != 0 {
		t.Fatalf("Phase 0 database contains business tables: %v", found)
	}
}

func verifyConditionalUpdate(t *testing.T, environment *postgrestest.RepositoryEnvironment) {
	t.Helper()
	executor := environment.Runtime.WriteExecutor()
	if err := executePostgreSQLWrite(context.Background(), executor, func(ctx context.Context, tx *testPostgreSQLWriteTx) error {
		_, err := tx.Exec(ctx, "INSERT INTO repository_contract_probe (natural_key, value) VALUES ('conditional', 'before')")
		return err
	}); err != nil {
		t.Fatalf("insert conditional probe: %v", err)
	}

	var firstAffected int64
	var secondAffected int64
	if err := executePostgreSQLWrite(context.Background(), executor, func(ctx context.Context, tx *testPostgreSQLWriteTx) error {
		first, err := tx.Exec(ctx, `
UPDATE repository_contract_probe
SET value = 'after', version = version + 1
WHERE natural_key = 'conditional' AND version = 1`)
		if err != nil {
			return err
		}
		firstAffected = first.RowsAffected()
		second, err := tx.Exec(ctx, `
UPDATE repository_contract_probe
SET value = 'stale'
WHERE natural_key = 'conditional' AND version = 1`)
		if err != nil {
			return err
		}
		secondAffected = second.RowsAffected()
		return nil
	}); err != nil {
		t.Fatalf("conditional update: %v", err)
	}
	if firstAffected != 1 || secondAffected != 0 {
		t.Fatalf("conditional affected rows = (%d, %d), want (1, 0)", firstAffected, secondAffected)
	}
}

func verifyUniqueConstraint(t *testing.T, environment *postgrestest.RepositoryEnvironment) {
	t.Helper()
	executor := environment.Runtime.WriteExecutor()
	insert := func() error {
		return executePostgreSQLWrite(context.Background(), executor, func(ctx context.Context, tx *testPostgreSQLWriteTx) error {
			_, err := tx.Exec(ctx, "INSERT INTO repository_contract_probe (natural_key, value) VALUES ('unique', 'value')")
			return err
		})
	}
	if err := insert(); err != nil {
		t.Fatalf("insert unique probe: %v", err)
	}
	err := insert()
	var postgresError *pgconn.PgError
	if !errors.As(err, &postgresError) || postgresError.Code != "23505" {
		t.Fatalf("duplicate error = %v, want PostgreSQL unique_violation", err)
	}
}

func verifyCommitAndRollback(t *testing.T, environment *postgrestest.RepositoryEnvironment) {
	t.Helper()
	executor := environment.Runtime.WriteExecutor()
	if err := executePostgreSQLWrite(context.Background(), executor, func(ctx context.Context, tx *testPostgreSQLWriteTx) error {
		_, err := tx.Exec(ctx, "INSERT INTO repository_contract_probe (natural_key, value) VALUES ('committed', 'value')")
		return err
	}); err != nil {
		t.Fatalf("commit probe transaction: %v", err)
	}

	wantRollback := errors.New("contract rollback")
	err := executePostgreSQLWrite(context.Background(), executor, func(ctx context.Context, tx *testPostgreSQLWriteTx) error {
		if _, err := tx.Exec(ctx, "INSERT INTO repository_contract_probe (natural_key, value) VALUES ('rolled-back', 'value')"); err != nil {
			return err
		}
		return wantRollback
	})
	if !errors.Is(err, wantRollback) {
		t.Fatalf("rollback transaction error = %v, want sentinel", err)
	}

	var rows int
	if err := environment.Runtime.ReadPool().QueryRow(
		context.Background(),
		"SELECT count(*) FROM repository_contract_probe",
	).Scan(&rows); err != nil {
		t.Fatalf("query transaction result: %v", err)
	}
	if rows != 1 {
		t.Fatalf("transaction result rows = %d, want 1", rows)
	}
}

func verifyClockAndReadOnlyBoundary(t *testing.T, environment *postgrestest.RepositoryEnvironment) {
	t.Helper()
	now, err := environment.Runtime.Clock().Now(context.Background())
	if err != nil {
		t.Fatalf("read Database Clock: %v", err)
	}
	if now.Location() != time.UTC || now.IsZero() {
		t.Fatalf("Database Clock = %v, want non-zero UTC", now)
	}

	var id int64
	err = environment.Runtime.ReadPool().QueryRow(
		context.Background(),
		"INSERT INTO repository_contract_probe (natural_key, value) VALUES ('read-only', 'value') RETURNING id",
	).Scan(&id)
	var postgresError *pgconn.PgError
	if !errors.As(err, &postgresError) || postgresError.Code != "25006" {
		t.Fatalf("read pool write error = %v, want read_only_sql_transaction", err)
	}
	if _, err := environment.Runtime.ReadPool().Exec(context.Background(), "DELETE FROM repository_contract_probe"); !errors.Is(err, postgresruntime.ErrReadOnlyPoolWrite) {
		t.Fatalf("read pool Exec error = %v, want explicit read-only rejection", err)
	}
}

func verifyConcurrentCompetition(t *testing.T, environment *postgrestest.RepositoryEnvironment) {
	t.Helper()
	executor := environment.Runtime.WriteExecutor()
	if err := executePostgreSQLWrite(context.Background(), executor, func(ctx context.Context, tx *testPostgreSQLWriteTx) error {
		_, err := tx.Exec(ctx, "INSERT INTO repository_contract_probe (natural_key, value) VALUES ('competition', 'value')")
		return err
	}); err != nil {
		t.Fatalf("insert competition probe: %v", err)
	}

	var winners atomic.Int64
	results := postgrestest.ExecuteConcurrent(context.Background(), 4, func(ctx context.Context, _ int) error {
		return executePostgreSQLWrite(ctx, executor, func(ctx context.Context, tx *testPostgreSQLWriteTx) error {
			tag, err := tx.Exec(ctx, `
UPDATE repository_contract_probe
SET claimed = TRUE
WHERE natural_key = 'competition' AND claimed = FALSE`)
			if err == nil {
				winners.Add(tag.RowsAffected())
			}
			return err
		})
	})
	if err := postgrestest.FormatConcurrentErrors(results); err != nil {
		t.Fatalf("concurrent repository competition: %v", err)
	}
	if winners.Load() != 1 {
		t.Fatalf("concurrent winners = %d, want 1", winners.Load())
	}
}

func assertAppliedVersions(t *testing.T, harness *postgrestest.MigrationHarness, want []int64) {
	t.Helper()
	got, err := harness.AppliedVersions(context.Background())
	if err != nil {
		t.Fatalf("query applied versions: %v", err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("applied versions = %v, want %v", got, want)
	}
}

func postgresUserFromDSN(t *testing.T, dsn string) string {
	t.Helper()
	config, err := pgx.ParseConfig(dsn)
	if err != nil {
		t.Fatalf("parse PostgreSQL test identity DSN: %v", err)
	}
	return config.User
}

func closeFoundationRuntime(t testing.TB, runtime *postgresruntime.Runtime) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := runtime.Close(ctx); err != nil && !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("close foundation runtime: %v", err)
	}
}
