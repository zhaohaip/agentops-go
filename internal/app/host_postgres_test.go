package app

import (
	"context"
	"errors"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/zhaohaip/agentops-go/internal/adapter/postgres/migration"
	postgresruntime "github.com/zhaohaip/agentops-go/internal/adapter/postgres/runtime"
	"github.com/zhaohaip/agentops-go/internal/config/infra"
	"github.com/zhaohaip/agentops-go/internal/contracts"
	"github.com/zhaohaip/agentops-go/test/integration/postgres/postgrestest"
)

func TestHostSealsWritesBeforeWaitingForEnteredHTTPHandler(t *testing.T) {
	_, identities, config, definitions := newHostPostgresTestDatabase(t)
	handlerEntered := make(chan struct{})
	releaseHandler := make(chan struct{})
	handlerResult := make(chan error, 1)
	var observed *observedHostDatabase
	handler := http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		close(handlerEntered)
		<-releaseHandler
		err := observed.runtime.WriteExecutor().Execute(
			request.Context(),
			func(ctx context.Context, token contracts.RuntimeWriteTx) error {
				return postgresruntime.WithPostgreSQLWriteTx(token, func(tx pgx.Tx) error {
					_, err := tx.Exec(ctx, "INSERT INTO host_shutdown_probe (value) VALUES (1)")
					return err
				})
			},
		)
		handlerResult <- err
		if errors.Is(err, postgresruntime.ErrWriteUnavailable) {
			response.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		if err != nil {
			response.WriteHeader(http.StatusInternalServerError)
			return
		}
		response.WriteHeader(http.StatusNoContent)
	})

	host, httpComponent := newPostgresHostForTest(t, config, definitions, handler, func(runtime *postgresruntime.Runtime) databaseRuntime {
		observed = newObservedHostDatabase(runtime)
		return observed
	})
	ctx, cancelHost := context.WithCancel(context.Background())
	releaseOnce := sync.Once{}
	t.Cleanup(func() {
		releaseOnce.Do(func() { close(releaseHandler) })
		cancelHost()
	})
	hostResult := make(chan error, 1)
	go func() { hostResult <- host.Run(ctx) }()
	waitSignal(t, httpComponent.started, "HTTP component did not start")

	clientResult := startHostTestRequest(httpComponent.server.Address())
	waitSignal(t, handlerEntered, "HTTP handler did not enter")
	cancelHost()
	waitSignal(t, observed.writesSealed, "Host did not seal Runtime writes")

	select {
	case <-observed.closeStarted:
		t.Fatal("Runtime resource close started before HTTP handler completed")
	default:
	}
	var one int
	if err := observed.runtime.ReadPool().QueryRow(context.Background(), "SELECT 1").Scan(&one); err != nil || one != 1 {
		t.Fatalf("read pool during HTTP grace period = %d, %v", one, err)
	}

	releaseOnce.Do(func() { close(releaseHandler) })
	select {
	case err := <-handlerResult:
		if !errors.Is(err, postgresruntime.ErrWriteUnavailable) {
			t.Fatalf("Handler Execute() error = %v, want write unavailable", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("HTTP handler did not finish after release")
	}
	assertHostHTTPStatus(t, clientResult, http.StatusServiceUnavailable)
	assertHostRunResult(t, hostResult)

	connection := postgrestest.Connect(t, identities.MigrationDSN)
	var rows int
	if err := connection.QueryRow(context.Background(), "SELECT count(*) FROM host_shutdown_probe").Scan(&rows); err != nil {
		t.Fatalf("query rejected Handler write: %v", err)
	}
	if rows != 0 {
		t.Fatalf("rows written by Handler after Host shutdown = %d, want 0", rows)
	}
}

func TestHostLetsAlreadyAcceptedHandlerWriteFinishDuringGracePeriod(t *testing.T) {
	_, identities, config, definitions := newHostPostgresTestDatabase(t)
	writeAccepted := make(chan struct{})
	releaseWrite := make(chan struct{})
	handlerResult := make(chan error, 1)
	var observed *observedHostDatabase
	handler := http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		err := observed.runtime.WriteExecutor().Execute(
			request.Context(),
			func(ctx context.Context, token contracts.RuntimeWriteTx) error {
				if err := postgresruntime.WithPostgreSQLWriteTx(token, func(tx pgx.Tx) error {
					if _, err := tx.Exec(ctx, "INSERT INTO host_shutdown_probe (value) VALUES (1)"); err != nil {
						return err
					}
					return nil
				}); err != nil {
					return err
				}
				close(writeAccepted)
				<-releaseWrite
				return nil
			},
		)
		handlerResult <- err
		if err != nil {
			response.WriteHeader(http.StatusInternalServerError)
			return
		}
		response.WriteHeader(http.StatusNoContent)
	})

	host, httpComponent := newPostgresHostForTest(t, config, definitions, handler, func(runtime *postgresruntime.Runtime) databaseRuntime {
		observed = newObservedHostDatabase(runtime)
		return observed
	})
	ctx, cancelHost := context.WithCancel(context.Background())
	releaseOnce := sync.Once{}
	t.Cleanup(func() {
		releaseOnce.Do(func() { close(releaseWrite) })
		cancelHost()
	})
	hostResult := make(chan error, 1)
	go func() { hostResult <- host.Run(ctx) }()
	waitSignal(t, httpComponent.started, "HTTP component did not start")

	clientResult := startHostTestRequest(httpComponent.server.Address())
	waitSignal(t, writeAccepted, "Handler write transaction was not accepted")
	cancelHost()
	waitSignal(t, observed.writesSealed, "Host did not seal Runtime writes")

	if err := observed.runtime.WriteExecutor().Execute(
		context.Background(),
		func(context.Context, contracts.RuntimeWriteTx) error { return nil },
	); !errors.Is(err, postgresruntime.ErrWriteUnavailable) {
		t.Fatalf("new Execute() during HTTP grace period error = %v, want write unavailable", err)
	}
	var one int
	if err := observed.runtime.ReadPool().QueryRow(context.Background(), "SELECT 1").Scan(&one); err != nil || one != 1 {
		t.Fatalf("read pool while accepted write drains = %d, %v", one, err)
	}
	select {
	case <-observed.closeStarted:
		t.Fatal("Runtime resource close started before accepted write completed")
	default:
	}

	releaseOnce.Do(func() { close(releaseWrite) })
	select {
	case err := <-handlerResult:
		if err != nil {
			t.Fatalf("accepted Handler write error = %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("accepted Handler write did not finish")
	}
	assertHostHTTPStatus(t, clientResult, http.StatusNoContent)
	assertHostRunResult(t, hostResult)

	connection := postgrestest.Connect(t, identities.MigrationDSN)
	var rows int
	if err := connection.QueryRow(context.Background(), "SELECT count(*) FROM host_shutdown_probe").Scan(&rows); err != nil {
		t.Fatalf("query accepted Handler write: %v", err)
	}
	if rows != 1 {
		t.Fatalf("rows committed by accepted Handler write = %d, want 1", rows)
	}
}

type observedHostDatabase struct {
	runtime      *postgresruntime.Runtime
	writesSealed chan struct{}
	closeStarted chan struct{}
	sealOnce     sync.Once
	closeOnce    sync.Once
}

func newObservedHostDatabase(runtime *postgresruntime.Runtime) *observedHostDatabase {
	return &observedHostDatabase{
		runtime:      runtime,
		writesSealed: make(chan struct{}),
		closeStarted: make(chan struct{}),
	}
}

func (d *observedHostDatabase) Migrate(ctx context.Context) error {
	return d.runtime.Migrate(ctx)
}

func (d *observedHostDatabase) StartMonitoring() error {
	return d.runtime.StartMonitoring()
}

func (d *observedHostDatabase) Done() <-chan error {
	return d.runtime.Done()
}

func (d *observedHostDatabase) StopAcceptingWrites() {
	d.runtime.StopAcceptingWrites()
	d.sealOnce.Do(func() { close(d.writesSealed) })
}

func (d *observedHostDatabase) Close(ctx context.Context) error {
	d.closeOnce.Do(func() { close(d.closeStarted) })
	return d.runtime.Close(ctx)
}

type signalingHTTPComponent struct {
	server  *HTTPServer
	started chan struct{}
}

func (c *signalingHTTPComponent) name() string {
	return c.server.name()
}

func (c *signalingHTTPComponent) Start() error {
	if err := c.server.Start(); err != nil {
		return err
	}
	close(c.started)
	return nil
}

func (c *signalingHTTPComponent) doneSignal() <-chan error {
	return c.server.doneSignal()
}

func (c *signalingHTTPComponent) Shutdown(ctx context.Context) error {
	return c.server.Shutdown(ctx)
}

type hostHTTPResult struct {
	status int
	err    error
}

func newHostPostgresTestDatabase(
	t *testing.T,
) (*postgrestest.Database, *postgrestest.DatabaseIdentities, infra.Config, []migration.Migration) {
	t.Helper()
	database := postgrestest.NewDatabase(t)
	identities := postgrestest.NewDatabaseIdentities(t, database)
	config := postgrestest.RuntimeConfig(t, identities, "127.0.0.1:0")
	writeConfig, err := pgx.ParseConfig(identities.RuntimeWriteDSN)
	if err != nil {
		t.Fatalf("parse Runtime write test DSN: %v", err)
	}
	readConfig, err := pgx.ParseConfig(identities.RuntimeReadDSN)
	if err != nil {
		t.Fatalf("parse Runtime read test DSN: %v", err)
	}
	definitions := []migration.Migration{{
		Version: 1,
		Name:    "create_host_shutdown_probe",
		Statements: []string{
			"CREATE TABLE host_shutdown_probe (value BIGINT NOT NULL)",
			"GRANT INSERT ON host_shutdown_probe TO " + pgx.Identifier{writeConfig.User}.Sanitize(),
			"GRANT SELECT ON host_shutdown_probe TO " + pgx.Identifier{readConfig.User}.Sanitize(),
		},
	}}
	return database, identities, config, definitions
}

func newPostgresHostForTest(
	t *testing.T,
	config infra.Config,
	definitions []migration.Migration,
	handler http.Handler,
	wrap func(*postgresruntime.Runtime) databaseRuntime,
) (*Host, *signalingHTTPComponent) {
	t.Helper()
	server, err := NewHTTPServer(config.HTTP, testLogger(), handler)
	if err != nil {
		t.Fatalf("create Host test HTTP server: %v", err)
	}
	component := &signalingHTTPComponent{server: server, started: make(chan struct{})}
	factory := func(ctx context.Context) (databaseRuntime, error) {
		runtime, err := postgresruntime.Open(ctx, config.PostgreSQL, config.Runtime, definitions)
		if err != nil {
			return nil, err
		}
		return wrap(runtime), nil
	}
	return newHostWithDatabase(testLogger(), config.Shutdown.Timeout, factory, component), component
}

func startHostTestRequest(address string) <-chan hostHTTPResult {
	result := make(chan hostHTTPResult, 1)
	go func() {
		response, err := http.Get("http://" + address + "/shutdown")
		if err != nil {
			result <- hostHTTPResult{err: err}
			return
		}
		defer response.Body.Close()
		result <- hostHTTPResult{status: response.StatusCode}
	}()
	return result
}

func assertHostHTTPStatus(t *testing.T, result <-chan hostHTTPResult, want int) {
	t.Helper()
	select {
	case response := <-result:
		if response.err != nil || response.status != want {
			t.Fatalf("HTTP result = status %d, error %v, want status %d", response.status, response.err, want)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("HTTP client did not finish")
	}
}

func assertHostRunResult(t *testing.T, result <-chan error) {
	t.Helper()
	select {
	case err := <-result:
		if err != nil {
			t.Fatalf("Host.Run() error = %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Host.Run() did not finish")
	}
}

func waitSignal(t *testing.T, signal <-chan struct{}, message string) {
	t.Helper()
	select {
	case <-signal:
	case <-time.After(3 * time.Second):
		t.Fatal(message)
	}
}
