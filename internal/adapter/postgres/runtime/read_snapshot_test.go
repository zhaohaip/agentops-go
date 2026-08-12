package runtime

import (
	"context"
	"errors"
	"runtime"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

func TestReadSnapshotQueryRowSerializesForcedRollback(t *testing.T) {
	previousMaxProcs := runtime.GOMAXPROCS(1)
	t.Cleanup(func() { runtime.GOMAXPROCS(previousMaxProcs) })

	lifecycle := newReadLifecycle()
	operation, err := lifecycle.begin(context.Background())
	if err != nil {
		t.Fatalf("begin read operation: %v", err)
	}
	fake := &blockingQueryRowTx{
		queryEntered:    make(chan struct{}),
		releaseQuery:    make(chan struct{}),
		rollbackEntered: make(chan struct{}),
	}
	operation.attachTransaction(fake)
	snapshot := &readSnapshot{tx: fake, operation: operation}

	queryResult := make(chan Row, 1)
	go func() {
		queryResult <- snapshot.QueryRow("SELECT blocked")
	}()
	<-fake.queryEntered

	forceResult := make(chan error, 1)
	go func() { forceResult <- operation.force() }()
	<-operation.ctx.Done()

	// On one P, Gosched lets force run until it either blocks on useMu (correct)
	// or enters Rollback while QueryRow is still active (missing lock).
	runtime.Gosched()
	select {
	case <-fake.rollbackEntered:
		t.Fatal("Rollback entered while QueryRow still owned operation.useMu")
	default:
	}
	select {
	case err := <-forceResult:
		t.Fatalf("force completed before QueryRow was released: %v", err)
	default:
	}

	close(fake.releaseQuery)
	// Yield until the unblocked QueryRow publishes its result; this is a
	// synchronization wait, not a timing window.
	for len(queryResult) == 0 {
		runtime.Gosched()
	}
	if row := <-queryResult; row == nil {
		t.Fatal("QueryRow returned a nil Row")
	}
	<-fake.rollbackEntered
	if err := <-forceResult; err != nil {
		t.Fatalf("force read operation: %v", err)
	}

	idle := lifecycle.stopAccepting()
	select {
	case <-idle:
	default:
		t.Fatal("forced read operation was not removed from lifecycle")
	}
}

type blockingQueryRowTx struct {
	queryEntered    chan struct{}
	releaseQuery    chan struct{}
	rollbackEntered chan struct{}
	queryOnce       sync.Once
	rollbackOnce    sync.Once
}

func (t *blockingQueryRowTx) Begin(context.Context) (pgx.Tx, error) {
	return nil, errors.New("not implemented")
}
func (t *blockingQueryRowTx) Commit(context.Context) error { return errors.New("not implemented") }

func (t *blockingQueryRowTx) Rollback(context.Context) error {
	t.rollbackOnce.Do(func() { close(t.rollbackEntered) })
	return nil
}

func (t *blockingQueryRowTx) CopyFrom(
	context.Context,
	pgx.Identifier,
	[]string,
	pgx.CopyFromSource,
) (int64, error) {
	return 0, errors.New("not implemented")
}

func (t *blockingQueryRowTx) SendBatch(context.Context, *pgx.Batch) pgx.BatchResults { return nil }
func (t *blockingQueryRowTx) LargeObjects() pgx.LargeObjects                         { return pgx.LargeObjects{} }
func (t *blockingQueryRowTx) Prepare(context.Context, string, string) (*pgconn.StatementDescription, error) {
	return nil, errors.New("not implemented")
}
func (t *blockingQueryRowTx) Exec(context.Context, string, ...any) (pgconn.CommandTag, error) {
	return pgconn.CommandTag{}, errors.New("not implemented")
}
func (t *blockingQueryRowTx) Query(context.Context, string, ...any) (pgx.Rows, error) {
	return nil, errors.New("not implemented")
}

func (t *blockingQueryRowTx) QueryRow(context.Context, string, ...any) pgx.Row {
	t.queryOnce.Do(func() { close(t.queryEntered) })
	<-t.releaseQuery
	return fakeReadRow{}
}

func (t *blockingQueryRowTx) Conn() *pgx.Conn { return nil }

type fakeReadRow struct{}

func (fakeReadRow) Scan(...any) error { return nil }

var _ pgx.Tx = (*blockingQueryRowTx)(nil)
