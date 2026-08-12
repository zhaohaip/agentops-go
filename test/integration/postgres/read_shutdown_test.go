package postgres_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	postgresruntime "github.com/zhaohaip/agentops-go/internal/adapter/postgres/runtime"
)

const forcedReadCloseUpperBound = 2 * time.Second

func TestCloseWaitsForActiveReadWithinGrace(t *testing.T) {
	environment := newRuntimeTestDatabase(t, "127.0.0.1:0")
	database := openRuntime(t, environment.config, nil)

	rows, err := database.ReadPool().Query(context.Background(), "SELECT generate_series(1, 2)")
	if err != nil {
		t.Fatalf("Query() error = %v", err)
	}
	if !rows.Next() {
		t.Fatalf("first Next() = false: %v", rows.Err())
	}

	closeResult := make(chan error, 1)
	closeCtx, cancelClose := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancelClose()
	go func() { closeResult <- database.Close(closeCtx) }()
	waitForReadRejection(t, database)
	select {
	case err := <-closeResult:
		t.Fatalf("Close() returned while admitted Rows was active: %v", err)
	default:
	}

	rows.Close()
	select {
	case err := <-closeResult:
		if err != nil {
			t.Fatalf("graceful Close() error = %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Close() did not return after active Rows was closed")
	}
}

func TestCloseTimeoutInterruptsBlockedReadQuery(t *testing.T) {
	environment := newRuntimeTestDatabase(t, "127.0.0.1:0")
	database := openRuntime(t, environment.config, nil)
	admin := connect(t, environment.database.DSN)

	const lockKey int64 = 0x5265616451756572
	if _, err := admin.Exec(context.Background(), "SELECT pg_advisory_lock($1)", lockKey); err != nil {
		t.Fatalf("acquire blocking advisory lock: %v", err)
	}
	defer func() { _, _ = admin.Exec(context.Background(), "SELECT pg_advisory_unlock($1)", lockKey) }()

	queryResult := make(chan error, 1)
	go func() {
		rows, err := database.ReadPool().Query(
			context.Background(),
			"SELECT pg_advisory_lock($1) /* agentops_blocked_read_query */",
			lockKey,
		)
		if err != nil {
			queryResult <- err
			return
		}
		defer rows.Close()
		if rows.Next() {
			queryResult <- errors.New("blocked read unexpectedly produced a row")
			return
		}
		queryResult <- rows.Err()
	}()
	waitForReaderWaitEvent(t, admin, environment.identities.RuntimeReadRole(), "agentops_blocked_read_query")

	closeErr := closeRuntimeWithBound(t, database, 50*time.Millisecond)
	if !errors.Is(closeErr, context.DeadlineExceeded) {
		t.Fatalf("Close() error = %v, want deadline exceeded", closeErr)
	}
	select {
	case err := <-queryResult:
		if err == nil {
			t.Fatal("blocked Query Rows iteration succeeded after forced close")
		}
	case <-time.After(time.Second):
		t.Fatal("blocked Query() remained active after Close() returned")
	}
	assertNoReaderSessions(t, admin, environment.identities.RuntimeReadRole())

	second := openRuntime(t, environment.config, nil)
	closeRuntime(t, second)
}

func TestCloseTimeoutReclaimsUnclosedRows(t *testing.T) {
	environment := newRuntimeTestDatabase(t, "127.0.0.1:0")
	database := openRuntime(t, environment.config, nil)
	admin := connect(t, environment.database.DSN)

	rows, err := database.ReadPool().Query(
		context.Background(),
		"SELECT value FROM generate_series(1, 1000000) AS values(value) /* agentops_unclosed_rows */",
	)
	if err != nil {
		t.Fatalf("Query() error = %v", err)
	}
	waitForReaderSession(t, admin, environment.identities.RuntimeReadRole(), "agentops_unclosed_rows")

	closeErr := closeRuntimeWithBound(t, database, 50*time.Millisecond)
	if !errors.Is(closeErr, context.DeadlineExceeded) {
		t.Fatalf("Close() error = %v, want deadline exceeded", closeErr)
	}
	if rows.Next() {
		t.Fatal("unclosed Rows remained usable after Runtime.Close()")
	}
	assertNoReaderSessions(t, admin, environment.identities.RuntimeReadRole())
}

func TestRowsCompletionPathsReleaseReadRegistration(t *testing.T) {
	testCases := []struct {
		name string
		use  func(*testing.T, *postgresruntime.Runtime)
	}{
		{
			name: "EOF",
			use: func(t *testing.T, database *postgresruntime.Runtime) {
				rows, err := database.ReadPool().Query(context.Background(), "SELECT generate_series(1, 3)")
				if err != nil {
					t.Fatalf("Query() error = %v", err)
				}
				var values []int
				for rows.Next() {
					var value int
					if err := rows.Scan(&value); err != nil {
						t.Fatalf("Scan() error = %v", err)
					}
					values = append(values, value)
				}
				if err := rows.Err(); err != nil {
					t.Fatalf("Rows.Err() = %v", err)
				}
				if fmt.Sprint(values) != "[1 2 3]" {
					t.Fatalf("values = %v", values)
				}
			},
		},
		{
			name: "explicit Close",
			use: func(t *testing.T, database *postgresruntime.Runtime) {
				rows, err := database.ReadPool().Query(context.Background(), "SELECT generate_series(1, 3)")
				if err != nil {
					t.Fatalf("Query() error = %v", err)
				}
				rows.Close()
				rows.Close()
			},
		},
		{
			name: "iteration error",
			use: func(t *testing.T, database *postgresruntime.Runtime) {
				rows, err := database.ReadPool().Query(
					context.Background(),
					"SELECT CASE WHEN value = 2 THEN 1 / 0 ELSE value END FROM generate_series(1, 3) AS values(value)",
				)
				if err != nil {
					t.Fatalf("Query() error = %v", err)
				}
				for rows.Next() {
					var value int
					if err := rows.Scan(&value); err != nil {
						break
					}
				}
				if err := rows.Err(); err == nil {
					t.Fatal("Rows.Err() = nil, want database iteration error")
				}
			},
		},
	}

	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			environment := newRuntimeTestDatabase(t, "127.0.0.1:0")
			database := openRuntime(t, environment.config, nil)
			testCase.use(t, database)

			ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
			defer cancel()
			if err := database.Close(ctx); err != nil {
				t.Fatalf("Close() after completed Rows error = %v", err)
			}
		})
	}
}

func TestCloseTimeoutInterruptsBlockedQueryRowScan(t *testing.T) {
	environment := newRuntimeTestDatabase(t, "127.0.0.1:0")
	database := openRuntime(t, environment.config, nil)
	admin := connect(t, environment.database.DSN)

	const lockKey int64 = 0x52656164526f7753
	if _, err := admin.Exec(context.Background(), "SELECT pg_advisory_lock($1)", lockKey); err != nil {
		t.Fatalf("acquire blocking advisory lock: %v", err)
	}
	defer func() { _, _ = admin.Exec(context.Background(), "SELECT pg_advisory_unlock($1)", lockKey) }()

	scanResult := make(chan error, 1)
	go func() {
		var ignored any
		scanResult <- database.ReadPool().QueryRow(
			context.Background(),
			"SELECT pg_advisory_lock($1) /* agentops_blocked_query_row */",
			lockKey,
		).Scan(&ignored)
	}()
	waitForReaderWaitEvent(t, admin, environment.identities.RuntimeReadRole(), "agentops_blocked_query_row")

	closeErr := closeRuntimeWithBound(t, database, 50*time.Millisecond)
	if !errors.Is(closeErr, context.DeadlineExceeded) {
		t.Fatalf("Close() error = %v, want deadline exceeded", closeErr)
	}
	select {
	case err := <-scanResult:
		if err == nil {
			t.Fatal("blocked QueryRow().Scan() succeeded after forced close")
		}
	case <-time.After(time.Second):
		t.Fatal("blocked QueryRow().Scan() remained active after Close() returned")
	}
}

func TestSnapshotQueryRowLinearizesWithForcedClose(t *testing.T) {
	environment := newRuntimeTestDatabase(t, "127.0.0.1:0")
	database := openRuntime(t, environment.config, nil)
	admin := connect(t, environment.database.DSN)

	const lockKey int64 = 0x536e6170526f7753
	if _, err := admin.Exec(context.Background(), "SELECT pg_advisory_lock($1)", lockKey); err != nil {
		t.Fatalf("acquire blocking advisory lock: %v", err)
	}
	defer func() { _, _ = admin.Exec(context.Background(), "SELECT pg_advisory_unlock($1)", lockKey) }()

	snapshotAdmitted := make(chan struct{})
	allowQueryRow := make(chan struct{})
	queryRowReturned := make(chan struct{})
	snapshotResult := make(chan error, 1)
	go func() {
		snapshotResult <- database.ReadPool().WithSnapshot(
			context.Background(),
			func(ctx context.Context, snapshot postgresruntime.ReadSnapshot) error {
				close(snapshotAdmitted)
				select {
				case <-allowQueryRow:
				case <-ctx.Done():
					// Deliberately continue: this exercises QueryRow admission after
					// shutdown cancellation has raced with the admitted snapshot.
				}
				row := snapshot.QueryRow(
					"SELECT pg_advisory_lock($1) /* agentops_snapshot_query_row_close */",
					lockKey,
				)
				close(queryRowReturned)
				var ignored any
				return row.Scan(&ignored)
			},
		)
	}()
	select {
	case <-snapshotAdmitted:
	case <-time.After(time.Second):
		t.Fatal("snapshot was not admitted")
	}

	closeResult := make(chan error, 1)
	closeCtx, cancelClose := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancelClose()
	go func() { closeResult <- database.Close(closeCtx) }()
	waitForReadRejection(t, database)
	close(allowQueryRow)
	waitForReaderWaitEvent(t, admin, environment.identities.RuntimeReadRole(), "agentops_snapshot_query_row_close")

	forcedAt := time.Now()
	cancelClose()

	select {
	case err := <-closeResult:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Close() error = %v, want context canceled", err)
		}
	case <-time.After(forcedReadCloseUpperBound):
		t.Fatal("Close() exceeded the forced read close upper bound")
	}
	if elapsed := time.Since(forcedAt); elapsed > forcedReadCloseUpperBound {
		t.Fatalf("Close() duration after force = %s, want <= %s", elapsed, forcedReadCloseUpperBound)
	}
	select {
	case <-queryRowReturned:
	case <-time.After(time.Second):
		t.Fatal("snapshot QueryRow call remained blocked after Close() returned")
	}
	select {
	case err := <-snapshotResult:
		if err == nil {
			t.Fatal("snapshot QueryRow succeeded after forced close")
		}
	case <-time.After(time.Second):
		t.Fatal("snapshot QueryRow remained active after Close() returned")
	}
	assertNoReaderSessions(t, admin, environment.identities.RuntimeReadRole())

	second := openRuntime(t, environment.config, nil)
	closeRuntime(t, second)
}

func TestQueryRowCompletionPathsReleaseReadRegistration(t *testing.T) {
	environment := newRuntimeTestDatabase(t, "127.0.0.1:0")
	database := openRuntime(t, environment.config, nil)

	var one int
	if err := database.ReadPool().QueryRow(context.Background(), "SELECT 1").Scan(&one); err != nil || one != 1 {
		t.Fatalf("successful QueryRow().Scan() = (%d, %v), want (1, nil)", one, err)
	}
	if err := database.ReadPool().QueryRow(context.Background(), "SELECT 1 WHERE false").Scan(&one); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("empty QueryRow().Scan() error = %v, want no rows", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if err := database.Close(ctx); err != nil {
		t.Fatalf("Close() after QueryRow completion error = %v", err)
	}
}

func TestCloseForcesMultipleReadsAndConcurrentCallers(t *testing.T) {
	environment := newRuntimeTestDatabase(t, "127.0.0.1:0")
	database := openRuntime(t, environment.config, nil)

	// pgxpool guarantees a default maximum of at least four connections.
	const readCount = 4
	rowsSet := make([]postgresruntime.Rows, 0, readCount)
	for range readCount {
		rows, err := database.ReadPool().Query(context.Background(), "SELECT generate_series(1, 1000000)")
		if err != nil {
			t.Fatalf("Query() error = %v", err)
		}
		rowsSet = append(rowsSet, rows)
	}

	closeCtx, cancelClose := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancelClose()
	const closeCallers = 8
	start := make(chan struct{})
	results := make(chan error, closeCallers)
	var callers sync.WaitGroup
	for range closeCallers {
		callers.Add(1)
		go func() {
			defer callers.Done()
			<-start
			results <- database.Close(closeCtx)
		}()
	}
	startedAt := time.Now()
	close(start)
	callers.Wait()
	if elapsed := time.Since(startedAt); elapsed > forcedReadCloseUpperBound {
		t.Fatalf("concurrent Close() duration = %s, want <= %s", elapsed, forcedReadCloseUpperBound)
	}
	close(results)
	var first string
	for err := range results {
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("concurrent Close() error = %v, want deadline exceeded", err)
		}
		if first == "" {
			first = err.Error()
		} else if err.Error() != first {
			t.Fatalf("concurrent Close() error = %v, want consistent %s", err, first)
		}
	}
	for _, rows := range rowsSet {
		if rows.Next() {
			t.Fatal("Rows remained usable after concurrent forced Close()")
		}
	}
	if repeated := database.Close(context.Background()); repeated == nil || repeated.Error() != first {
		t.Fatalf("repeated Close() error = %v, want %s", repeated, first)
	}
}

func waitForReadRejection(t *testing.T, database *postgresruntime.Runtime) {
	t.Helper()
	deadline := time.NewTimer(time.Second)
	defer deadline.Stop()
	ticker := time.NewTicker(time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-deadline.C:
			t.Fatal("Runtime did not stop accepting reads")
		case <-ticker.C:
			var one int
			err := database.ReadPool().QueryRow(context.Background(), "SELECT 1").Scan(&one)
			if errors.Is(err, postgresruntime.ErrReadUnavailable) {
				return
			}
			if err != nil {
				t.Fatalf("QueryRow() while observing read shutdown error = %v", err)
			}
		}
	}
}

func closeRuntimeWithBound(t *testing.T, database *postgresruntime.Runtime, grace time.Duration) error {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), grace)
	defer cancel()
	startedAt := time.Now()
	err := database.Close(ctx)
	if elapsed := time.Since(startedAt); elapsed > forcedReadCloseUpperBound {
		t.Fatalf("Runtime.Close() duration = %s, want <= %s", elapsed, forcedReadCloseUpperBound)
	}
	return err
}

func waitForReaderWaitEvent(t *testing.T, admin *pgx.Conn, role string, marker string) {
	t.Helper()
	waitForReaderActivity(t, admin, role, marker, true)
}

func waitForReaderSession(t *testing.T, admin *pgx.Conn, role string, marker string) {
	t.Helper()
	waitForReaderActivity(t, admin, role, marker, false)
}

func waitForReaderActivity(t *testing.T, admin *pgx.Conn, role string, marker string, requireWait bool) {
	t.Helper()
	deadline := time.NewTimer(3 * time.Second)
	defer deadline.Stop()
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-deadline.C:
			t.Fatalf("Reader activity %q was not observable", marker)
		case <-ticker.C:
			var waitEventType *string
			var waitEvent *string
			err := admin.QueryRow(context.Background(), `
SELECT wait_event_type, wait_event
FROM pg_stat_activity
WHERE datname = current_database()
  AND usename = $1
  AND query LIKE '%' || $2 || '%'
ORDER BY pid
LIMIT 1`, role, marker).Scan(&waitEventType, &waitEvent)
			if errors.Is(err, pgx.ErrNoRows) {
				continue
			}
			if err != nil {
				t.Fatalf("inspect Reader activity: %v", err)
			}
			if !requireWait || (waitEventType != nil && *waitEventType == "Lock" && waitEvent != nil && *waitEvent == "advisory") {
				return
			}
		}
	}
}

func assertNoReaderSessions(t *testing.T, admin *pgx.Conn, role string) {
	t.Helper()
	deadline := time.NewTimer(time.Second)
	defer deadline.Stop()
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-deadline.C:
			t.Fatalf("Reader role %q still has database sessions after Close()", role)
		case <-ticker.C:
			var count int
			if err := admin.QueryRow(context.Background(), `
SELECT count(*)
FROM pg_stat_activity
WHERE datname = current_database() AND usename = $1`, role).Scan(&count); err != nil {
				t.Fatalf("count Reader sessions: %v", err)
			}
			if count == 0 {
				return
			}
		}
	}
}

func TestReadShutdownErrorsDoNotExposeConnectionSettings(t *testing.T) {
	environment := newRuntimeTestDatabase(t, "127.0.0.1:0")
	database := openRuntime(t, environment.config, nil)
	rows, err := database.ReadPool().Query(context.Background(), "SELECT generate_series(1, 1000000)")
	if err != nil {
		t.Fatalf("Query() error = %v", err)
	}
	_ = rows

	closeErr := closeRuntimeWithBound(t, database, 10*time.Millisecond)
	if closeErr == nil {
		t.Fatal("Close() error = nil, want deadline error")
	}
	for _, secret := range []string{"password_", "postgresql://"} {
		if strings.Contains(closeErr.Error(), secret) {
			t.Fatalf("Close() error contains connection settings: %v", closeErr)
		}
	}
}
