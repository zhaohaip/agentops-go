package app

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"reflect"
	"sync"
	"testing"
	"time"
)

func TestEmptyHostStartsAndStops(t *testing.T) {
	t.Parallel()

	host := newHost(testLogger(), time.Second)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if err := host.Run(ctx); err != nil {
		t.Fatalf("Run() error = %v", err)
	}
}

func TestHostStartsInOrderAndStopsInReverse(t *testing.T) {
	t.Parallel()

	var (
		eventsMu sync.Mutex
		events   []string
	)
	first := newRecordingComponent("first", &eventsMu, &events)
	second := newRecordingComponent("second", &eventsMu, &events)
	host := newHost(testLogger(), time.Second, first, second)

	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan error, 1)
	go func() {
		result <- host.Run(ctx)
	}()

	<-first.started
	<-second.started
	cancel()

	if err := <-result; err != nil {
		t.Fatalf("Run() error = %v", err)
	}

	eventsMu.Lock()
	defer eventsMu.Unlock()
	want := []string{"start:first", "start:second", "shutdown:second", "shutdown:first"}
	if !reflect.DeepEqual(events, want) {
		t.Fatalf("events = %v, want %v", events, want)
	}
}

func TestHostBoundsShutdownByTimeout(t *testing.T) {
	t.Parallel()

	component := &blockingShutdownComponent{
		started: make(chan struct{}),
		done:    make(chan error),
	}
	host := newHost(testLogger(), 10*time.Millisecond, component)

	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan error, 1)
	go func() {
		result <- host.Run(ctx)
	}()

	<-component.started
	cancel()

	err := <-result
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Run() error = %v, want deadline exceeded", err)
	}
	close(component.done)
}

func TestHostCanOnlyRunOnce(t *testing.T) {
	t.Parallel()

	host := newHost(testLogger(), time.Second)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := host.Run(ctx); err != nil {
		t.Fatalf("first Run() error = %v", err)
	}
	if err := host.Run(ctx); err == nil {
		t.Fatal("second Run() error = nil, want error")
	}
}

func TestHostStartsDatabaseBeforeComponentsAndStopsInReverse(t *testing.T) {
	t.Parallel()

	var (
		eventsMu sync.Mutex
		events   []string
	)
	database := newRecordingDatabase(&eventsMu, &events)
	component := newRecordingComponent("http", &eventsMu, &events)
	host := newHostWithDatabase(testLogger(), time.Second, func(context.Context) (databaseRuntime, error) {
		return database, nil
	}, component)

	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan error, 1)
	go func() {
		result <- host.Run(ctx)
	}()

	<-component.started
	cancel()
	if err := <-result; err != nil {
		t.Fatalf("Run() error = %v", err)
	}

	eventsMu.Lock()
	defer eventsMu.Unlock()
	want := []string{
		"open:postgres",
		"migrate",
		"monitor",
		"start:http",
		"seal:postgres",
		"shutdown:http",
		"close:postgres",
	}
	if !reflect.DeepEqual(events, want) {
		t.Fatalf("events = %v, want %v", events, want)
	}
}

func TestHostCompletesTimedOutComponentShutdownBeforeClosingDatabase(t *testing.T) {
	t.Parallel()

	var (
		eventsMu sync.Mutex
		events   []string
	)
	database := newRecordingDatabase(&eventsMu, &events)
	component := &timedShutdownComponent{
		eventsMu: &eventsMu,
		events:   &events,
		started:  make(chan struct{}),
		done:     make(chan error),
	}
	host := newHostWithDatabase(testLogger(), 20*time.Millisecond, func(context.Context) (databaseRuntime, error) {
		return database, nil
	}, component)

	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan error, 1)
	go func() {
		result <- host.Run(ctx)
	}()
	<-component.started
	cancel()
	if err := <-result; !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Run() error = %v, want deadline exceeded", err)
	}

	eventsMu.Lock()
	defer eventsMu.Unlock()
	want := []string{
		"open:postgres",
		"migrate",
		"monitor",
		"start:http",
		"seal:postgres",
		"shutdown:http:begin",
		"shutdown:http:forced",
		"close:postgres",
	}
	if !reflect.DeepEqual(events, want) {
		t.Fatalf("events = %v, want %v", events, want)
	}
}

func TestHostMigrationFailurePreventsComponentStart(t *testing.T) {
	t.Parallel()

	wantErr := errors.New("migration failed")
	database := newRecordingDatabase(nil, nil)
	database.migrateErr = wantErr
	component := newRecordingComponent("http", new(sync.Mutex), new([]string))
	host := newHostWithDatabase(testLogger(), time.Second, func(context.Context) (databaseRuntime, error) {
		return database, nil
	}, component)

	err := host.Run(context.Background())
	if !errors.Is(err, wantErr) {
		t.Fatalf("Run() error = %v, want migration error", err)
	}
	select {
	case <-component.started:
		t.Fatal("component started after migration failure")
	default:
	}
	if !database.closed {
		t.Fatal("database was not closed after migration failure")
	}
}

func TestHostDatabaseOpenFailurePreventsComponentStart(t *testing.T) {
	t.Parallel()

	wantErr := errors.New("lock unavailable")
	component := newRecordingComponent("http", new(sync.Mutex), new([]string))
	host := newHostWithDatabase(testLogger(), time.Second, func(context.Context) (databaseRuntime, error) {
		return nil, wantErr
	}, component)

	err := host.Run(context.Background())
	if !errors.Is(err, wantErr) {
		t.Fatalf("Run() error = %v, want database error", err)
	}
	select {
	case <-component.started:
		t.Fatal("component started after database open failure")
	default:
	}
}

func TestHostDatabaseFailureStopsComponents(t *testing.T) {
	t.Parallel()

	database := newRecordingDatabase(nil, nil)
	component := newRecordingComponent("http", new(sync.Mutex), new([]string))
	host := newHostWithDatabase(testLogger(), time.Second, func(context.Context) (databaseRuntime, error) {
		return database, nil
	}, component)

	result := make(chan error, 1)
	go func() {
		result <- host.Run(context.Background())
	}()
	<-component.started
	wantErr := errors.New("lock connection lost")
	database.done <- wantErr

	err := <-result
	if !errors.Is(err, wantErr) {
		t.Fatalf("Run() error = %v, want lock error", err)
	}
	if !database.closed {
		t.Fatal("database was not closed after runtime failure")
	}
}

func TestHostPreservesComponentAndDatabaseShutdownErrors(t *testing.T) {
	t.Parallel()

	componentErr := errors.New("HTTP shutdown failed")
	databaseErr := errors.New("database close failed")
	database := newRecordingDatabase(nil, nil)
	database.closeErr = databaseErr
	component := newRecordingComponent("http", new(sync.Mutex), new([]string))
	component.shutdownErr = componentErr
	host := newHostWithDatabase(testLogger(), time.Second, func(context.Context) (databaseRuntime, error) {
		return database, nil
	}, component)

	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan error, 1)
	go func() { result <- host.Run(ctx) }()
	<-component.started
	cancel()

	err := <-result
	if !errors.Is(err, componentErr) || !errors.Is(err, databaseErr) {
		t.Fatalf("Run() error = %v, want component and database shutdown errors", err)
	}
}

type recordingDatabase struct {
	eventsMu *sync.Mutex
	events   *[]string
	done     chan error

	migrateErr error
	monitorErr error
	closeErr   error
	closed     bool
}

func newRecordingDatabase(eventsMu *sync.Mutex, events *[]string) *recordingDatabase {
	database := &recordingDatabase{
		eventsMu: eventsMu,
		events:   events,
		done:     make(chan error, 1),
	}
	database.record("open:postgres")
	return database
}

func (d *recordingDatabase) Migrate(context.Context) error {
	d.record("migrate")
	return d.migrateErr
}

func (d *recordingDatabase) StartMonitoring() error {
	d.record("monitor")
	return d.monitorErr
}

func (d *recordingDatabase) Done() <-chan error {
	return d.done
}

func (d *recordingDatabase) StopAcceptingWrites() {
	d.record("seal:postgres")
}

func (d *recordingDatabase) Close(context.Context) error {
	d.record("close:postgres")
	d.closed = true
	return d.closeErr
}

func (d *recordingDatabase) record(event string) {
	if d.eventsMu == nil || d.events == nil {
		return
	}
	d.eventsMu.Lock()
	defer d.eventsMu.Unlock()
	*d.events = append(*d.events, event)
}

type recordingComponent struct {
	componentName string
	eventsMu      *sync.Mutex
	events        *[]string
	started       chan struct{}
	done          chan error
	closeOnce     sync.Once
	shutdownErr   error
}

func newRecordingComponent(name string, eventsMu *sync.Mutex, events *[]string) *recordingComponent {
	return &recordingComponent{
		componentName: name,
		eventsMu:      eventsMu,
		events:        events,
		started:       make(chan struct{}),
		done:          make(chan error),
	}
}

func (c *recordingComponent) name() string {
	return c.componentName
}

func (c *recordingComponent) Start() error {
	c.record("start:" + c.componentName)
	close(c.started)
	return nil
}

func (c *recordingComponent) doneSignal() <-chan error {
	return c.done
}

func (c *recordingComponent) Shutdown(context.Context) error {
	c.record("shutdown:" + c.componentName)
	c.closeOnce.Do(func() {
		close(c.done)
	})
	return c.shutdownErr
}

func (c *recordingComponent) record(event string) {
	c.eventsMu.Lock()
	defer c.eventsMu.Unlock()
	*c.events = append(*c.events, event)
}

type blockingShutdownComponent struct {
	started chan struct{}
	done    chan error
}

type timedShutdownComponent struct {
	eventsMu *sync.Mutex
	events   *[]string
	started  chan struct{}
	done     chan error
}

func (*timedShutdownComponent) name() string {
	return "http"
}

func (c *timedShutdownComponent) Start() error {
	c.record("start:http")
	close(c.started)
	return nil
}

func (c *timedShutdownComponent) doneSignal() <-chan error {
	return c.done
}

func (c *timedShutdownComponent) Shutdown(ctx context.Context) error {
	c.record("shutdown:http:begin")
	<-ctx.Done()
	c.record("shutdown:http:forced")
	close(c.done)
	return ctx.Err()
}

func (c *timedShutdownComponent) record(event string) {
	c.eventsMu.Lock()
	defer c.eventsMu.Unlock()
	*c.events = append(*c.events, event)
}

func (*blockingShutdownComponent) name() string {
	return "blocking"
}

func (c *blockingShutdownComponent) Start() error {
	close(c.started)
	return nil
}

func (c *blockingShutdownComponent) doneSignal() <-chan error {
	return c.done
}

func (*blockingShutdownComponent) Shutdown(ctx context.Context) error {
	<-ctx.Done()
	return ctx.Err()
}

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}
