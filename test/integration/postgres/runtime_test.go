package postgres_test

import (
	"context"
	"errors"
	"io"
	"net"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/zhaohaip/agentops-go/internal/adapter/postgres/migration"
	postgresruntime "github.com/zhaohaip/agentops-go/internal/adapter/postgres/runtime"
	"github.com/zhaohaip/agentops-go/internal/app"
	"github.com/zhaohaip/agentops-go/internal/config/infra"
	"github.com/zhaohaip/agentops-go/internal/contracts"
	"github.com/zhaohaip/agentops-go/migrations"
	"github.com/zhaohaip/agentops-go/test/integration/postgres/postgrestest"
)

func TestConnectionFailureIsReported(t *testing.T) {
	config := postgrestest.RuntimeConfigForDSNs(
		t,
		"postgresql://migration:secret@127.0.0.1:1/agentops?connect_timeout=1",
		"postgresql://writer:secret@127.0.0.1:1/agentops?connect_timeout=1",
		"postgresql://reader:secret@127.0.0.1:1/agentops?connect_timeout=1",
		"127.0.0.1:0",
	)

	opened, err := postgresruntime.Open(context.Background(), config.PostgreSQL, config.Runtime, nil)
	if err == nil {
		_ = opened.Close(context.Background())
		t.Fatal("Open() error = nil, want connection failure")
	}
	if strings.Contains(err.Error(), "secret") {
		t.Fatalf("Open() exposed PostgreSQL password: %v", err)
	}
}

func TestRuntimeAcceptsThreeDirectLoginIdentities(t *testing.T) {
	environment := newRuntimeTestDatabase(t, "127.0.0.1:0")
	database := openRuntime(t, environment.config, []migration.Migration{{
		Version:    1,
		Name:       "create_identity_boundary_probe",
		Statements: []string{"CREATE TABLE identity_boundary_probe (value BIGINT NOT NULL)"},
	}})
	t.Cleanup(func() { closeRuntime(t, database) })
	if err := database.Migrate(context.Background()); err != nil {
		t.Fatalf("Migrate() error = %v", err)
	}
	environment.identities.GrantRuntimePrivileges(t)
	if err := database.StartMonitoring(); err != nil {
		t.Fatalf("StartMonitoring() error = %v", err)
	}

	users := make(map[string]string, 3)
	for label, dsn := range map[string]string{
		"Migration":    environment.identities.MigrationDSN,
		"RuntimeWrite": environment.identities.RuntimeWriteDSN,
		"RuntimeRead":  environment.identities.RuntimeReadDSN,
	} {
		connection := connect(t, dsn)
		var sessionUser string
		var currentUser string
		if err := connection.QueryRow(context.Background(), "SELECT session_user, current_user").Scan(
			&sessionUser,
			&currentUser,
		); err != nil {
			t.Fatalf("query %s identity: %v", label, err)
		}
		if sessionUser != currentUser {
			t.Fatalf("%s identity = session %q, current %q, want equal", label, sessionUser, currentUser)
		}
		if previous, exists := users[sessionUser]; exists {
			t.Fatalf("%s and %s use the same login identity", previous, label)
		}
		users[sessionUser] = label
	}

	if err := executePostgreSQLWrite(context.Background(), database.WriteExecutor(), func(
		ctx context.Context,
		tx *testPostgreSQLWriteTx,
	) error {
		_, err := tx.Exec(ctx, "INSERT INTO identity_boundary_probe (value) VALUES (1)")
		return err
	}); err != nil {
		t.Fatalf("Writer insert: %v", err)
	}
	var value int64
	if err := database.ReadPool().QueryRow(
		context.Background(),
		"SELECT value FROM identity_boundary_probe",
	).Scan(&value); err != nil {
		t.Fatalf("Reader query: %v", err)
	}
	if value != 1 {
		t.Fatalf("Reader value = %d, want 1", value)
	}
}

func TestRuntimeRejectsRepeatedSessionLoginIdentity(t *testing.T) {
	testCases := []struct {
		name string
		dsns func(*postgrestest.DatabaseIdentities) (string, string, string)
	}{
		{
			name: "Migration and Writer",
			dsns: func(identities *postgrestest.DatabaseIdentities) (string, string, string) {
				return identities.MigrationDSN, identities.MigrationDSN, identities.RuntimeReadDSN
			},
		},
		{
			name: "Migration and Reader",
			dsns: func(identities *postgrestest.DatabaseIdentities) (string, string, string) {
				return identities.MigrationDSN, identities.RuntimeWriteDSN, identities.MigrationDSN
			},
		},
		{
			name: "Writer and Reader",
			dsns: func(identities *postgrestest.DatabaseIdentities) (string, string, string) {
				return identities.MigrationDSN, identities.RuntimeWriteDSN, identities.RuntimeWriteDSN
			},
		},
	}

	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			environment := newRuntimeTestDatabase(t, "127.0.0.1:0")
			migrationDSN, writeDSN, readDSN := testCase.dsns(environment.identities)
			config := postgrestest.RuntimeConfigForDSNs(
				t,
				migrationDSN,
				writeDSN,
				readDSN,
				"127.0.0.1:0",
			)
			database, err := postgresruntime.Open(context.Background(), config.PostgreSQL, config.Runtime, nil)
			if database != nil {
				closeRuntime(t, database)
			}
			assertUnsafeIdentityError(t, err, config)
		})
	}
}

func TestRuntimeRejectsSessionUserMasqueradingAsReader(t *testing.T) {
	environment := newRuntimeTestDatabase(t, "127.0.0.1:0")
	migrationConnection := connect(t, environment.identities.MigrationDSN)
	if _, err := migrationConnection.Exec(
		context.Background(),
		"CREATE TABLE role_switch_probe (value BIGINT NOT NULL)",
	); err != nil {
		t.Fatalf("create role switch probe: %v", err)
	}

	masqueradeDSN := dsnWithStartupRole(
		t,
		environment.database.DSN,
		environment.identities.RuntimeReadRole(),
	)
	masqueradingConnection := connect(t, masqueradeDSN)
	var sessionUser string
	var currentUser string
	if err := masqueradingConnection.QueryRow(
		context.Background(),
		"SELECT session_user, current_user",
	).Scan(&sessionUser, &currentUser); err != nil {
		t.Fatalf("query masquerading identity: %v", err)
	}
	if sessionUser == currentUser || currentUser != environment.identities.RuntimeReadRole() {
		t.Fatalf("masquerading identity = session %q, current %q", sessionUser, currentUser)
	}
	if _, err := masqueradingConnection.Exec(context.Background(), "SET default_transaction_read_only = off"); err != nil {
		t.Fatalf("disable masquerading read-only default: %v", err)
	}
	if _, err := masqueradingConnection.Exec(context.Background(), "RESET ROLE"); err != nil {
		t.Fatalf("reset masquerading role: %v", err)
	}
	if err := masqueradingConnection.QueryRow(context.Background(), "SELECT current_user").Scan(&currentUser); err != nil {
		t.Fatalf("query identity after RESET ROLE: %v", err)
	}
	if currentUser != sessionUser {
		if _, err := masqueradingConnection.Exec(
			context.Background(),
			"SET ROLE "+pgx.Identifier{sessionUser}.Sanitize(),
		); err != nil {
			t.Fatalf("restore high-privilege session role: %v", err)
		}
	}
	if _, err := masqueradingConnection.Exec(
		context.Background(),
		"INSERT INTO role_switch_probe (value) VALUES (1)",
	); err != nil {
		t.Fatalf("reproduce high-privilege session write after restoring the session role: %v", err)
	}

	config := postgrestest.RuntimeConfigForDSNs(
		t,
		environment.identities.MigrationDSN,
		environment.identities.RuntimeWriteDSN,
		masqueradeDSN,
		"127.0.0.1:0",
	)
	database, err := postgresruntime.Open(context.Background(), config.PostgreSQL, config.Runtime, nil)
	if database != nil {
		closeRuntime(t, database)
	}
	assertUnsafeIdentityError(t, err, config)
}

func TestAdvisoryLockAllowsOnlyOneRuntimeAndShutdownReleasesIt(t *testing.T) {
	environment := newRuntimeTestDatabase(t, "127.0.0.1:0")
	config := environment.config

	first := openRuntime(t, config, nil)
	second, err := postgresruntime.Open(context.Background(), config.PostgreSQL, config.Runtime, nil)
	if second != nil {
		_ = second.Close(context.Background())
	}
	if !errors.Is(err, postgresruntime.ErrAdvisoryLockUnavailable) {
		t.Fatalf("second Open() error = %v, want advisory lock unavailable", err)
	}

	closeRuntime(t, first)
	third := openRuntime(t, config, nil)
	closeRuntime(t, third)
}

func TestWriteExecutorReadPoolAndDatabaseClock(t *testing.T) {
	environment := newRuntimeTestDatabase(t, "127.0.0.1:0")
	database := openRuntime(t, environment.config, []migration.Migration{{
		Version:    1,
		Name:       "create_runtime_write_probe",
		Statements: []string{"CREATE TABLE runtime_write_probe (value BIGINT NOT NULL)"},
	}})
	t.Cleanup(func() { closeRuntime(t, database) })
	if err := database.Migrate(context.Background()); err != nil {
		t.Fatalf("Migrate() error = %v", err)
	}
	environment.identities.GrantRuntimePrivileges(t)
	if err := database.StartMonitoring(); err != nil {
		t.Fatalf("StartMonitoring() error = %v", err)
	}

	executor := database.WriteExecutor()
	var isolation string
	err := executePostgreSQLWrite(context.Background(), executor, func(ctx context.Context, tx *testPostgreSQLWriteTx) error {
		if err := tx.QueryRow(ctx, "SHOW transaction_isolation").Scan(&isolation); err != nil {
			return err
		}
		return nil
	})
	if err != nil {
		t.Fatalf("create committed probe: %v", err)
	}
	if isolation != "read committed" {
		t.Fatalf("transaction isolation = %q, want read committed", isolation)
	}

	if err := executePostgreSQLWrite(context.Background(), executor, func(ctx context.Context, tx *testPostgreSQLWriteTx) error {
		_, err := tx.Exec(ctx, "INSERT INTO runtime_write_probe (value) VALUES (1)")
		return err
	}); err != nil {
		t.Fatalf("commit write: %v", err)
	}

	wantRollback := errors.New("rollback requested")
	err = executePostgreSQLWrite(context.Background(), executor, func(ctx context.Context, tx *testPostgreSQLWriteTx) error {
		if _, err := tx.Exec(ctx, "INSERT INTO runtime_write_probe (value) VALUES (2)"); err != nil {
			return err
		}
		return wantRollback
	})
	if !errors.Is(err, wantRollback) {
		t.Fatalf("rollback Execute() error = %v, want sentinel", err)
	}

	panicValue := errors.New("panic requested")
	var recovered any
	func() {
		defer func() {
			recovered = recover()
		}()
		_ = executePostgreSQLWrite(context.Background(), executor, func(ctx context.Context, tx *testPostgreSQLWriteTx) error {
			if _, err := tx.Exec(ctx, "INSERT INTO runtime_write_probe (value) VALUES (3)"); err != nil {
				return err
			}
			panic(panicValue)
		})
	}()
	if recovered != panicValue {
		t.Fatalf("Execute() recovered panic = %v, want %v", recovered, panicValue)
	}

	var values []int64
	rows, err := database.ReadPool().Query(context.Background(), "SELECT value FROM runtime_write_probe ORDER BY value")
	if err != nil {
		t.Fatalf("query committed rows: %v", err)
	}
	for rows.Next() {
		var value int64
		if err := rows.Scan(&value); err != nil {
			rows.Close()
			t.Fatalf("scan committed row: %v", err)
		}
		values = append(values, value)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate committed rows: %v", err)
	}
	if len(values) != 1 || values[0] != 1 {
		t.Fatalf("committed values = %v, want [1]", values)
	}

	var forbiddenValue int64
	err = database.ReadPool().QueryRow(
		context.Background(),
		"INSERT INTO runtime_write_probe (value) VALUES (3) RETURNING value",
	).Scan(&forbiddenValue)
	if err == nil {
		t.Fatal("read pool accepted an INSERT")
	}
	if _, err := database.ReadPool().Exec(
		context.Background(),
		"INSERT INTO runtime_write_probe (value) VALUES (4)",
	); !errors.Is(err, postgresruntime.ErrReadOnlyPoolWrite) {
		t.Fatalf("read pool Exec() error = %v, want explicit read-only rejection", err)
	}

	now, err := database.Clock().Now(context.Background())
	if err != nil {
		t.Fatalf("Clock().Now() error = %v", err)
	}
	if now.Location() != time.UTC {
		t.Fatalf("database clock location = %v, want UTC", now.Location())
	}
	if now.IsZero() {
		t.Fatal("database clock returned zero time")
	}
}

func TestReadPoolDatabasePermissionsCannotBeBypassed(t *testing.T) {
	environment := newRuntimeTestDatabase(t, "127.0.0.1:0")
	database := openRuntime(t, environment.config, []migration.Migration{{
		Version:    1,
		Name:       "create_read_pool_acl_probe",
		Statements: []string{"CREATE TABLE read_pool_acl_probe (value BIGINT NOT NULL)"},
	}})
	t.Cleanup(func() { closeRuntime(t, database) })
	if err := database.Migrate(context.Background()); err != nil {
		t.Fatalf("Migrate() error = %v", err)
	}
	environment.identities.GrantRuntimePrivileges(t)

	var backendBefore int64
	if err := database.ReadPool().QueryRow(context.Background(), "SELECT pg_backend_pid()").Scan(&backendBefore); err != nil {
		t.Fatalf("query read pool backend before session change: %v", err)
	}
	rows, err := database.ReadPool().Query(context.Background(), "SET default_transaction_read_only = off")
	if err != nil {
		t.Fatalf("change read pool session default: %v", err)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		t.Fatalf("finish read pool session change: %v", err)
	}

	var backendAfter int64
	var readOnly string
	if err := database.ReadPool().QueryRow(
		context.Background(),
		"SELECT pg_backend_pid(), current_setting('default_transaction_read_only')",
	).Scan(&backendAfter, &readOnly); err != nil {
		t.Fatalf("query read pool backend after session change: %v", err)
	}
	if backendAfter != backendBefore || readOnly != "on" {
		t.Fatalf("read pool session = (backend %d, read_only %q), want reused clean backend %d with on", backendAfter, readOnly, backendBefore)
	}

	var inserted int64
	err = database.ReadPool().QueryRow(
		context.Background(),
		"INSERT INTO read_pool_acl_probe (value) VALUES (1) RETURNING value",
	).Scan(&inserted)
	var poolError *pgconn.PgError
	if !errors.As(err, &poolError) || poolError.Code != "25006" {
		t.Fatalf("read pool write error = %v, want read_only_sql_transaction", err)
	}

	readConnection := connect(t, environment.identities.RuntimeReadDSN)
	var sessionUserBefore string
	var currentUserBefore string
	if err := readConnection.QueryRow(context.Background(), "SELECT session_user, current_user").Scan(
		&sessionUserBefore,
		&currentUserBefore,
	); err != nil {
		t.Fatalf("query raw read identity before RESET ROLE: %v", err)
	}
	if sessionUserBefore != currentUserBefore {
		t.Fatalf("raw read identity = session %q, current %q, want equal", sessionUserBefore, currentUserBefore)
	}
	if _, err := readConnection.Exec(context.Background(), "SET default_transaction_read_only = off"); err != nil {
		t.Fatalf("disable raw read identity transaction default: %v", err)
	}
	if _, err := readConnection.Exec(context.Background(), "RESET ROLE"); err != nil {
		t.Fatalf("reset raw read identity role: %v", err)
	}
	var sessionUserAfter string
	var currentUserAfter string
	if err := readConnection.QueryRow(
		context.Background(),
		"SELECT session_user, current_user, current_setting('default_transaction_read_only')",
	).Scan(&sessionUserAfter, &currentUserAfter, &readOnly); err != nil {
		t.Fatalf("query raw read identity setting: %v", err)
	}
	if sessionUserAfter != sessionUserBefore || currentUserAfter != currentUserBefore {
		t.Fatalf(
			"raw read identity after RESET ROLE = session %q, current %q, want %q",
			sessionUserAfter,
			currentUserAfter,
			sessionUserBefore,
		)
	}
	if readOnly != "off" {
		t.Fatalf("raw read identity default_transaction_read_only = %q, want off", readOnly)
	}
	_, err = readConnection.Exec(context.Background(), "INSERT INTO read_pool_acl_probe (value) VALUES (2)")
	var permissionError *pgconn.PgError
	if !errors.As(err, &permissionError) || permissionError.Code != "42501" {
		t.Fatalf("raw read identity write error = %v, want insufficient_privilege", err)
	}

	var committed int64
	if err := executePostgreSQLWrite(context.Background(), database.WriteExecutor(), func(ctx context.Context, tx *testPostgreSQLWriteTx) error {
		return tx.QueryRow(ctx, "INSERT INTO read_pool_acl_probe (value) VALUES (3) RETURNING value").Scan(&committed)
	}); err != nil {
		t.Fatalf("WriteExecutor insert after read bypass attempts: %v", err)
	}
	if committed != 3 {
		t.Fatalf("WriteExecutor committed value = %d, want 3", committed)
	}
}

func TestRuntimeRejectsReaderTableWritePrivileges(t *testing.T) {
	permissions := []string{"INSERT", "UPDATE", "DELETE", "TRUNCATE", "REFERENCES", "TRIGGER"}
	for _, permission := range permissions {
		t.Run(permission, func(t *testing.T) {
			environment := newRuntimeTestDatabase(t, "127.0.0.1:0")
			database := openRuntime(t, environment.config, []migration.Migration{{
				Version: 1,
				Name:    "grant_unsafe_read_table_" + strings.ToLower(permission),
				Statements: []string{
					"CREATE TABLE unsafe_read_table_probe (value BIGINT NOT NULL)",
					"GRANT " + permission + " ON unsafe_read_table_probe TO " +
						pgx.Identifier{environment.identities.RuntimeReadRole()}.Sanitize(),
				},
			}})
			t.Cleanup(func() { closeRuntime(t, database) })

			err := database.Migrate(context.Background())
			if !errors.Is(err, postgresruntime.ErrUnsafeDatabaseIdentity) {
				t.Fatalf("Migrate() error = %v, want unsafe read identity", err)
			}
			assertRawTablePrivilege(t, environment.identities.RuntimeReadDSN, permission)
			if err := database.StartMonitoring(); err == nil {
				t.Fatal("StartMonitoring() error = nil after unsafe read table privilege")
			}
		})
	}
}

func TestRuntimeRejectsReaderColumnWritePrivileges(t *testing.T) {
	permissions := []string{"INSERT", "UPDATE", "REFERENCES"}
	for _, permission := range permissions {
		t.Run(permission, func(t *testing.T) {
			environment := newRuntimeTestDatabase(t, "127.0.0.1:0")
			database := openRuntime(t, environment.config, []migration.Migration{{
				Version: 1,
				Name:    "grant_unsafe_read_column_" + strings.ToLower(permission),
				Statements: []string{
					"CREATE TABLE unsafe_read_column_probe (value BIGINT NOT NULL)",
					"GRANT " + permission + " (value) ON unsafe_read_column_probe TO " +
						pgx.Identifier{environment.identities.RuntimeReadRole()}.Sanitize(),
				},
			}})
			t.Cleanup(func() { closeRuntime(t, database) })

			err := database.Migrate(context.Background())
			if !errors.Is(err, postgresruntime.ErrUnsafeDatabaseIdentity) {
				t.Fatalf("Migrate() error = %v, want unsafe read identity", err)
			}
			assertRawColumnPrivilege(t, environment.identities.RuntimeReadDSN, permission)
			if err := database.StartMonitoring(); err == nil {
				t.Fatal("StartMonitoring() error = nil after unsafe read column privilege")
			}
		})
	}
}

func TestWriteTransactionsAreSerialized(t *testing.T) {
	environment := newRuntimeTestDatabase(t, "127.0.0.1:0")
	database := openRuntime(t, environment.config, nil)
	t.Cleanup(func() { closeRuntime(t, database) })
	executor := database.WriteExecutor()

	firstEntered := make(chan struct{})
	releaseFirst := make(chan struct{})
	firstResult := make(chan error, 1)
	go func() {
		firstResult <- executePostgreSQLWrite(context.Background(), executor, func(context.Context, *testPostgreSQLWriteTx) error {
			close(firstEntered)
			<-releaseFirst
			return nil
		})
	}()
	<-firstEntered

	secondCtx, cancelSecond := context.WithCancel(context.Background())
	cancelSecond()
	var secondEntered atomic.Bool
	err := executePostgreSQLWrite(secondCtx, executor, func(context.Context, *testPostgreSQLWriteTx) error {
		secondEntered.Store(true)
		return nil
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("blocked Execute() error = %v, want context canceled", err)
	}
	if secondEntered.Load() {
		t.Fatal("second write entered while first transaction held the serial gate")
	}

	close(releaseFirst)
	if err := <-firstResult; err != nil {
		t.Fatalf("first Execute() error = %v", err)
	}
	if err := executePostgreSQLWrite(context.Background(), executor, func(context.Context, *testPostgreSQLWriteTx) error { return nil }); err != nil {
		t.Fatalf("write after serial gate release: %v", err)
	}
}

func TestStopAcceptingWritesIsIdempotentAndLetsAcceptedWriteFinish(t *testing.T) {
	environment := newRuntimeTestDatabase(t, "127.0.0.1:0")
	database := openRuntime(t, environment.config, []migration.Migration{{
		Version:    1,
		Name:       "create_write_seal_probe",
		Statements: []string{"CREATE TABLE write_seal_probe (value BIGINT NOT NULL)"},
	}})
	t.Cleanup(func() { closeRuntime(t, database) })
	if err := database.Migrate(context.Background()); err != nil {
		t.Fatalf("Migrate() error = %v", err)
	}
	environment.identities.GrantRuntimePrivileges(t)

	writeAccepted := make(chan struct{})
	releaseWrite := make(chan struct{})
	writeResult := make(chan error, 1)
	go func() {
		writeResult <- executePostgreSQLWrite(context.Background(), database.WriteExecutor(), func(
			ctx context.Context,
			tx *testPostgreSQLWriteTx,
		) error {
			if _, err := tx.Exec(ctx, "INSERT INTO write_seal_probe (value) VALUES (1)"); err != nil {
				return err
			}
			close(writeAccepted)
			<-releaseWrite
			return nil
		})
	}()
	select {
	case <-writeAccepted:
	case <-time.After(3 * time.Second):
		close(releaseWrite)
		t.Fatal("write transaction was not accepted")
	}

	database.StopAcceptingWrites()
	database.StopAcceptingWrites()

	var one int
	if err := database.ReadPool().QueryRow(context.Background(), "SELECT 1").Scan(&one); err != nil || one != 1 {
		close(releaseWrite)
		t.Fatalf("read pool after StopAcceptingWrites() = %d, %v", one, err)
	}
	var rejectedWorkEntered atomic.Bool
	err := executePostgreSQLWrite(context.Background(), database.WriteExecutor(), func(
		context.Context,
		*testPostgreSQLWriteTx,
	) error {
		rejectedWorkEntered.Store(true)
		return nil
	})
	if !errors.Is(err, postgresruntime.ErrWriteUnavailable) {
		close(releaseWrite)
		t.Fatalf("Execute() after StopAcceptingWrites() error = %v, want write unavailable", err)
	}
	if rejectedWorkEntered.Load() {
		close(releaseWrite)
		t.Fatal("rejected write entered callback")
	}

	close(releaseWrite)
	select {
	case err := <-writeResult:
		if err != nil {
			t.Fatalf("accepted write result = %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("accepted write did not finish")
	}
	var rows int
	if err := database.ReadPool().QueryRow(context.Background(), "SELECT count(*) FROM write_seal_probe").Scan(&rows); err != nil {
		t.Fatalf("query accepted write: %v", err)
	}
	if rows != 1 {
		t.Fatalf("accepted rows = %d, want 1", rows)
	}
}

func TestStopAcceptingWritesAndExecuteHaveLinearAdmissionBoundary(t *testing.T) {
	environment := newRuntimeTestDatabase(t, "127.0.0.1:0")
	database := openRuntime(t, environment.config, []migration.Migration{{
		Version:    1,
		Name:       "create_write_admission_probe",
		Statements: []string{"CREATE TABLE write_admission_probe (value BIGINT NOT NULL)"},
	}})
	t.Cleanup(func() { closeRuntime(t, database) })
	if err := database.Migrate(context.Background()); err != nil {
		t.Fatalf("Migrate() error = %v", err)
	}
	environment.identities.GrantRuntimePrivileges(t)

	const contenders = 16
	start := make(chan struct{})
	results := make(chan error, contenders)
	var waitGroup sync.WaitGroup
	waitGroup.Add(contenders + 1)
	for value := range contenders {
		go func(value int) {
			defer waitGroup.Done()
			<-start
			results <- executePostgreSQLWrite(context.Background(), database.WriteExecutor(), func(
				ctx context.Context,
				tx *testPostgreSQLWriteTx,
			) error {
				_, err := tx.Exec(ctx, "INSERT INTO write_admission_probe (value) VALUES ($1)", value)
				return err
			})
		}(value)
	}
	go func() {
		defer waitGroup.Done()
		<-start
		database.StopAcceptingWrites()
	}()
	close(start)
	allDone := make(chan struct{})
	go func() {
		waitGroup.Wait()
		close(allDone)
	}()
	select {
	case <-allDone:
	case <-time.After(5 * time.Second):
		t.Fatal("concurrent StopAcceptingWrites and Execute calls did not finish")
	}
	close(results)

	committed := 0
	for err := range results {
		switch {
		case err == nil:
			committed++
		case errors.Is(err, postgresruntime.ErrWriteUnavailable):
		default:
			t.Fatalf("concurrent Execute() error = %v", err)
		}
	}
	var rows int
	if err := database.ReadPool().QueryRow(
		context.Background(),
		"SELECT count(*) FROM write_admission_probe",
	).Scan(&rows); err != nil {
		t.Fatalf("query admission results: %v", err)
	}
	if rows != committed {
		t.Fatalf("committed rows = %d, successful admissions = %d", rows, committed)
	}
	if err := executePostgreSQLWrite(context.Background(), database.WriteExecutor(), func(
		context.Context,
		*testPostgreSQLWriteTx,
	) error {
		return nil
	}); !errors.Is(err, postgresruntime.ErrWriteUnavailable) {
		t.Fatalf("Execute() after concurrent seal error = %v, want write unavailable", err)
	}
}

func TestCloseWaitsForActiveWriteWithinGraceAndRejectsNewWrites(t *testing.T) {
	environment := newRuntimeTestDatabase(t, "127.0.0.1:0")
	database := openRuntime(t, environment.config, nil)
	executor := database.WriteExecutor()

	writeEntered := make(chan struct{})
	releaseWrite := make(chan struct{})
	writeResult := make(chan error, 1)
	go func() {
		writeResult <- executePostgreSQLWrite(context.Background(), executor, func(context.Context, *testPostgreSQLWriteTx) error {
			close(writeEntered)
			<-releaseWrite
			return nil
		})
	}()
	select {
	case <-writeEntered:
	case <-time.After(3 * time.Second):
		t.Fatal("write transaction did not enter work")
	}

	closeCtx, cancelClose := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancelClose()
	closeResult := make(chan error, 1)
	go func() {
		closeResult <- database.Close(closeCtx)
	}()

	closingObserved := make(chan struct{})
	go func() {
		defer close(closingObserved)
		for {
			attemptCtx, cancelAttempt := context.WithTimeout(context.Background(), 10*time.Millisecond)
			err := executePostgreSQLWrite(attemptCtx, executor, func(context.Context, *testPostgreSQLWriteTx) error {
				return errors.New("new write entered after shutdown began")
			})
			cancelAttempt()
			if errors.Is(err, postgresruntime.ErrWriteUnavailable) {
				return
			}
			if !errors.Is(err, context.DeadlineExceeded) {
				t.Errorf("new Execute() while Close waited error = %v, want write unavailable", err)
				return
			}
		}
	}()
	select {
	case <-closingObserved:
	case <-time.After(time.Second):
		close(releaseWrite)
		t.Fatal("Runtime did not enter closing state")
	}
	select {
	case err := <-closeResult:
		close(releaseWrite)
		t.Fatalf("Close() returned before active write completed: %v", err)
	default:
	}

	close(releaseWrite)
	select {
	case err := <-writeResult:
		if err != nil {
			t.Fatalf("active write within grace error = %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("active write did not complete within grace")
	}
	select {
	case err := <-closeResult:
		if err != nil {
			t.Fatalf("graceful Close() error = %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Close() did not finish after active write completed")
	}
}

func TestCloseTimeoutAbortsActiveWriteAndReleasesLock(t *testing.T) {
	environment := newRuntimeTestDatabase(t, "127.0.0.1:0")
	database := openRuntime(t, environment.config, []migration.Migration{{
		Version:    1,
		Name:       "create_shutdown_probe",
		Statements: []string{"CREATE TABLE shutdown_probe (value BIGINT NOT NULL)"},
	}})
	if err := database.Migrate(context.Background()); err != nil {
		t.Fatalf("Migrate() error = %v", err)
	}
	environment.identities.GrantRuntimePrivileges(t)

	writeEntered := make(chan struct{})
	releaseWrite := make(chan struct{})
	writeResult := make(chan error, 1)
	go func() {
		writeResult <- executePostgreSQLWrite(context.Background(), database.WriteExecutor(), func(ctx context.Context, tx *testPostgreSQLWriteTx) error {
			if _, err := tx.Exec(ctx, "INSERT INTO shutdown_probe (value) VALUES (1)"); err != nil {
				return err
			}
			close(writeEntered)
			<-releaseWrite
			return nil
		})
	}()
	select {
	case <-writeEntered:
	case <-time.After(3 * time.Second):
		t.Fatal("write transaction did not enter work")
	}

	closeCtx, cancelClose := context.WithTimeout(context.Background(), 50*time.Millisecond)
	const closeCallers = 8
	closeResults := make(chan error, closeCallers)
	startClose := make(chan struct{})
	for range closeCallers {
		go func() {
			<-startClose
			closeResults <- database.Close(closeCtx)
		}()
	}
	close(startClose)
	var closeErr error
	for range closeCallers {
		select {
		case currentErr := <-closeResults:
			if closeErr == nil {
				closeErr = currentErr
			}
			if !errors.Is(currentErr, context.DeadlineExceeded) {
				t.Errorf("concurrent Close() error = %v, want deadline exceeded", currentErr)
			}
			if currentErr == nil || currentErr.Error() != closeErr.Error() {
				t.Errorf("concurrent Close() error = %v, want consistent %v", currentErr, closeErr)
			}
		case <-time.After(3 * time.Second):
			t.Fatal("concurrent Close() did not return")
		}
	}
	cancelClose()
	if !errors.Is(closeErr, context.DeadlineExceeded) {
		close(releaseWrite)
		t.Fatalf("Close() error = %v, want deadline exceeded", closeErr)
	}

	var poolStillUsable int
	if err := database.ReadPool().QueryRow(context.Background(), "SELECT 1").Scan(&poolStillUsable); err == nil {
		t.Errorf("read pool remained usable after timed-out Close; SELECT returned %d", poolStillUsable)
	}

	second, err := postgresruntime.Open(context.Background(), environment.config.PostgreSQL, environment.config.Runtime, nil)
	if err != nil {
		t.Errorf("second Runtime Open() after forced Close: %v", err)
	} else {
		closeRuntime(t, second)
	}

	close(releaseWrite)
	select {
	case err := <-writeResult:
		if err == nil {
			t.Error("write transaction committed after timed-out Close returned")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("forced write transaction did not return")
	}
	probe := connect(t, environment.identities.MigrationDSN)
	defer func() { _ = probe.Close(context.Background()) }()
	var committedRows int
	if err := probe.QueryRow(context.Background(), "SELECT count(*) FROM shutdown_probe").Scan(&committedRows); err != nil {
		t.Fatalf("query shutdown probe: %v", err)
	}
	if committedRows != 0 {
		t.Errorf("rows committed by forced transaction = %d, want 0", committedRows)
	}

	repeatedErr := database.Close(context.Background())
	if repeatedErr == nil || repeatedErr.Error() != closeErr.Error() {
		t.Errorf("repeated Close() error = %v, want %v", repeatedErr, closeErr)
	}
}

func TestCloseTimeoutInterruptsBlockedDatabaseOperation(t *testing.T) {
	environment := newRuntimeTestDatabase(t, "127.0.0.1:0")
	database := openRuntime(t, environment.config, nil)
	admin := connect(t, environment.database.DSN)
	defer func() { _ = admin.Close(context.Background()) }()

	const blockingLockKey int64 = 0x53687574646f776e
	if _, err := admin.Exec(context.Background(), "SELECT pg_advisory_lock($1)", blockingLockKey); err != nil {
		t.Fatalf("acquire blocking advisory lock: %v", err)
	}
	defer func() {
		_, _ = admin.Exec(context.Background(), "SELECT pg_advisory_unlock($1)", blockingLockKey)
	}()

	var backendPID uint32
	if err := executePostgreSQLWrite(context.Background(), database.WriteExecutor(), func(ctx context.Context, tx *testPostgreSQLWriteTx) error {
		return tx.QueryRow(ctx, "SELECT pg_backend_pid()").Scan(&backendPID)
	}); err != nil {
		t.Fatalf("query Runtime backend PID: %v", err)
	}
	writeResult := make(chan error, 1)
	go func() {
		writeResult <- executePostgreSQLWrite(context.Background(), database.WriteExecutor(), func(ctx context.Context, tx *testPostgreSQLWriteTx) error {
			_, err := tx.Exec(ctx, "SELECT pg_advisory_xact_lock($1)", blockingLockKey)
			return err
		})
	}()
	waitForBackendAdvisoryLock(t, admin, backendPID)

	closeCtx, cancelClose := context.WithTimeout(context.Background(), 50*time.Millisecond)
	closeErr := database.Close(closeCtx)
	cancelClose()
	if !errors.Is(closeErr, context.DeadlineExceeded) {
		t.Fatalf("Close() error = %v, want deadline exceeded", closeErr)
	}
	select {
	case err := <-writeResult:
		if err == nil {
			t.Fatal("blocked database operation succeeded after forced Close")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("blocked database operation was not interrupted")
	}

	second := openRuntime(t, environment.config, nil)
	closeRuntime(t, second)
}

func waitForBackendAdvisoryLock(t *testing.T, admin *pgx.Conn, backendPID uint32) {
	t.Helper()
	deadline := time.NewTimer(3 * time.Second)
	defer deadline.Stop()
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-deadline.C:
			t.Fatal("Runtime backend did not block on the test advisory lock")
		case <-ticker.C:
			var blocked bool
			if err := admin.QueryRow(context.Background(), `
SELECT wait_event_type = 'Lock' AND wait_event = 'advisory'
FROM pg_stat_activity
WHERE pid = $1`, backendPID).Scan(&blocked); err != nil {
				t.Fatalf("inspect Runtime backend wait event: %v", err)
			}
			if blocked {
				return
			}
		}
	}
}

func TestLostLockConnectionDisablesWrites(t *testing.T) {
	environment := newRuntimeTestDatabase(t, "127.0.0.1:0")
	database := openRuntime(t, environment.config, nil)
	t.Cleanup(func() { closeRuntime(t, database) })
	if err := database.StartMonitoring(); err != nil {
		t.Fatalf("StartMonitoring() error = %v", err)
	}

	var backendPID uint32
	if err := executePostgreSQLWrite(context.Background(), database.WriteExecutor(), func(ctx context.Context, tx *testPostgreSQLWriteTx) error {
		return tx.QueryRow(ctx, "SELECT pg_backend_pid()").Scan(&backendPID)
	}); err != nil {
		t.Fatalf("query lock connection backend PID: %v", err)
	}

	admin := connect(t, baseDSN(t))
	defer func() { _ = admin.Close(context.Background()) }()
	var terminated bool
	if err := admin.QueryRow(context.Background(), "SELECT pg_terminate_backend($1)", backendPID).Scan(&terminated); err != nil {
		t.Fatalf("terminate lock connection: %v", err)
	}
	if !terminated {
		t.Fatal("pg_terminate_backend returned false")
	}

	select {
	case err := <-database.Done():
		if !errors.Is(err, postgresruntime.ErrLockConnectionLost) {
			t.Fatalf("Done() error = %v, want lock connection lost", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("runtime did not report the lost lock connection")
	}

	err := executePostgreSQLWrite(context.Background(), database.WriteExecutor(), func(context.Context, *testPostgreSQLWriteTx) error {
		return nil
	})
	if !errors.Is(err, postgresruntime.ErrWriteUnavailable) {
		t.Fatalf("write after lock loss error = %v, want write unavailable", err)
	}
}

func TestRuntimeHostDoesNotStartWhenLockOrMigrationFails(t *testing.T) {
	t.Run("lock unavailable", func(t *testing.T) {
		environment := newRuntimeTestDatabase(t, "127.0.0.1:0")
		config := environment.config
		lockOwner := openRuntime(t, config, nil)
		defer closeRuntime(t, lockOwner)

		host, err := app.NewHost(config, io.Discard)
		if err != nil {
			t.Fatalf("NewHost() error = %v", err)
		}
		err = host.Run(context.Background())
		if !errors.Is(err, postgresruntime.ErrAdvisoryLockUnavailable) {
			t.Fatalf("Host.Run() error = %v, want advisory lock unavailable", err)
		}
	})

	t.Run("migration failure", func(t *testing.T) {
		environment := newRuntimeTestDatabase(t, "127.0.0.1:0")
		admin := connect(t, environment.identities.MigrationDSN)
		if _, err := admin.Exec(context.Background(), "CREATE TABLE agentops_schema_migrations (version BIGINT)"); err != nil {
			t.Fatalf("create damaged migration metadata: %v", err)
		}
		_ = admin.Close(context.Background())

		config := environment.config
		host, err := app.NewHost(config, io.Discard)
		if err != nil {
			t.Fatalf("NewHost() error = %v", err)
		}
		if err := host.Run(context.Background()); err == nil {
			t.Fatal("Host.Run() error = nil, want migration failure")
		}

		probe := openRuntime(t, config, nil)
		closeRuntime(t, probe)
	})
}

func TestRuntimeHostStartsHTTPAfterMigrationAndReleasesResources(t *testing.T) {
	address := reserveAddress(t)
	environment := newRuntimeTestDatabase(t, address)
	config := environment.config
	host, err := app.NewHost(config, io.Discard)
	if err != nil {
		t.Fatalf("NewHost() error = %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan error, 1)
	go func() { result <- host.Run(ctx) }()
	waitForHTTP(t, address, result)

	second, err := postgresruntime.Open(context.Background(), config.PostgreSQL, config.Runtime, migrations.All())
	if second != nil {
		_ = second.Close(context.Background())
	}
	if !errors.Is(err, postgresruntime.ErrAdvisoryLockUnavailable) {
		cancel()
		<-result
		t.Fatalf("second Runtime error = %v, want advisory lock unavailable", err)
	}

	cancel()
	if err := <-result; err != nil {
		t.Fatalf("Host.Run() shutdown error = %v", err)
	}

	probe := openRuntime(t, config, nil)
	defer closeRuntime(t, probe)
	var metadataExists bool
	if err := probe.ReadPool().QueryRow(
		context.Background(),
		"SELECT to_regclass('agentops_schema_migrations') IS NOT NULL",
	).Scan(&metadataExists); err != nil {
		t.Fatalf("query migration metadata: %v", err)
	}
	if !metadataExists {
		t.Fatal("Runtime Host did not initialize migration metadata")
	}
}

func openRuntime(t *testing.T, config infra.Config, definitions []migration.Migration) *postgresruntime.Runtime {
	t.Helper()
	database, err := postgresruntime.Open(context.Background(), config.PostgreSQL, config.Runtime, definitions)
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	return database
}

func closeRuntime(t *testing.T, database *postgresruntime.Runtime) {
	t.Helper()
	if database == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := database.Close(ctx); err != nil && !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("Close() error = %v", err)
	}
}

func baseDSN(t *testing.T) string {
	t.Helper()
	return postgrestest.BaseDSN(t)
}

func connect(t *testing.T, dsn string) *pgx.Conn {
	t.Helper()
	return postgrestest.Connect(t, dsn)
}

func executePostgreSQLWrite(
	ctx context.Context,
	executor contracts.RuntimeWriteExecutor,
	work func(context.Context, *testPostgreSQLWriteTx) error,
) error {
	return executor.Execute(ctx, func(ctx context.Context, token contracts.RuntimeWriteTx) error {
		return work(ctx, &testPostgreSQLWriteTx{token: token})
	})
}

func dsnWithStartupRole(t *testing.T, dsn string, role string) string {
	t.Helper()
	parsed, err := url.Parse(dsn)
	if err != nil {
		t.Fatalf("parse startup role DSN: %v", err)
	}
	query := parsed.Query()
	query.Set("options", "-c role="+role+" -c default_transaction_read_only=on")
	parsed.RawQuery = query.Encode()
	return parsed.String()
}

func assertUnsafeIdentityError(t *testing.T, err error, config infra.Config) {
	t.Helper()
	if !errors.Is(err, postgresruntime.ErrUnsafeDatabaseIdentity) {
		t.Fatalf("Open() error = %v, want unsafe database identity", err)
	}
	rendered := err.Error()
	for _, dsn := range []string{
		config.PostgreSQL.MigrationDSN.Value(),
		config.PostgreSQL.RuntimeWriteDSN.Value(),
		config.PostgreSQL.RuntimeReadDSN.Value(),
	} {
		if strings.Contains(rendered, dsn) {
			t.Fatalf("Open() error contains full PostgreSQL DSN: %v", err)
		}
		parsed, parseErr := url.Parse(dsn)
		if parseErr != nil || parsed.User == nil {
			continue
		}
		if password, present := parsed.User.Password(); present && password != "" && strings.Contains(rendered, password) {
			t.Fatalf("Open() error contains PostgreSQL password: %v", err)
		}
	}
}

func assertRawTablePrivilege(t *testing.T, dsn string, permission string) {
	t.Helper()
	connection := connect(t, dsn)
	var granted bool
	if err := connection.QueryRow(context.Background(), `
SELECT has_table_privilege(current_user, 'unsafe_read_table_probe', $1)`, permission).Scan(&granted); err != nil {
		t.Fatalf("query raw Reader table privilege %s: %v", permission, err)
	}
	if !granted {
		t.Fatalf("raw Reader table privilege %s = false, want true", permission)
	}
}

func assertRawColumnPrivilege(t *testing.T, dsn string, permission string) {
	t.Helper()
	connection := connect(t, dsn)
	var tableGranted bool
	var columnGranted bool
	if err := connection.QueryRow(context.Background(), `
SELECT has_table_privilege(current_user, 'unsafe_read_column_probe', $1),
       has_column_privilege(current_user, 'unsafe_read_column_probe', 'value', $1)`, permission).Scan(
		&tableGranted,
		&columnGranted,
	); err != nil {
		t.Fatalf("query raw Reader column privilege %s: %v", permission, err)
	}
	if tableGranted || !columnGranted {
		t.Fatalf(
			"raw Reader %s privilege = table %t, column %t, want table false and column true",
			permission,
			tableGranted,
			columnGranted,
		)
	}
}

// testPostgreSQLWriteTx models a PostgreSQL Repository Adapter: every database
// operation unwraps the opaque contract token only for the duration of that
// operation. Deliberately blocked application work therefore does not prevent
// Runtime shutdown from closing the dedicated connection.
type testPostgreSQLWriteTx struct {
	token contracts.RuntimeWriteTx
}

func (t *testPostgreSQLWriteTx) Exec(
	ctx context.Context,
	sql string,
	arguments ...any,
) (commandTag pgconn.CommandTag, resultErr error) {
	resultErr = postgresruntime.WithPostgreSQLWriteTx(t.token, func(tx pgx.Tx) error {
		var err error
		commandTag, err = tx.Exec(ctx, sql, arguments...)
		return err
	})
	return commandTag, resultErr
}

func (t *testPostgreSQLWriteTx) QueryRow(
	ctx context.Context,
	sql string,
	arguments ...any,
) pgx.Row {
	return &testPostgreSQLWriteRow{
		token:     t.token,
		ctx:       ctx,
		sql:       sql,
		arguments: arguments,
	}
}

type testPostgreSQLWriteRow struct {
	token     contracts.RuntimeWriteTx
	ctx       context.Context
	sql       string
	arguments []any
}

func (r *testPostgreSQLWriteRow) Scan(destinations ...any) error {
	return postgresruntime.WithPostgreSQLWriteTx(r.token, func(tx pgx.Tx) error {
		return tx.QueryRow(r.ctx, r.sql, r.arguments...).Scan(destinations...)
	})
}

type runtimeTestDatabase struct {
	database   *postgrestest.Database
	identities *postgrestest.DatabaseIdentities
	config     infra.Config
}

func newRuntimeTestDatabase(t *testing.T, address string) *runtimeTestDatabase {
	t.Helper()
	database := postgrestest.NewDatabase(t)
	identities := postgrestest.NewDatabaseIdentities(t, database)
	return &runtimeTestDatabase{
		database:   database,
		identities: identities,
		config:     postgrestest.RuntimeConfig(t, identities, address),
	}
}

func reserveAddress(t *testing.T) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserve HTTP address: %v", err)
	}
	address := listener.Addr().String()
	if err := listener.Close(); err != nil {
		t.Fatalf("release reserved HTTP address: %v", err)
	}
	return address
}

func waitForHTTP(t *testing.T, address string, hostResult <-chan error) {
	t.Helper()
	deadline := time.NewTimer(3 * time.Second)
	defer deadline.Stop()
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()

	for {
		select {
		case err := <-hostResult:
			t.Fatalf("Host.Run() stopped before HTTP became ready: %v", err)
		case <-deadline.C:
			t.Fatal("HTTP server did not become ready")
		case <-ticker.C:
			connection, err := net.DialTimeout("tcp", address, 50*time.Millisecond)
			if err == nil {
				_ = connection.Close()
				return
			}
		}
	}
}
