package runtime

import (
	"context"
	"errors"
	"fmt"
	"net"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
)

const readTransactionReleaseTimeout = time.Second

type readOperationPhase uint8

const (
	readOperationActive readOperationPhase = iota
	readOperationReleasing
	readOperationDone
)

// readLifecycle linearizes read admission with shutdown and owns every public
// read operation until its transaction has been returned to pgxpool.
type readLifecycle struct {
	mu        sync.Mutex
	accepting bool
	nextID    uint64
	active    map[uint64]*readOperation
	idle      chan struct{}
}

func newReadLifecycle() *readLifecycle {
	idle := make(chan struct{})
	close(idle)
	return &readLifecycle{
		accepting: true,
		active:    make(map[uint64]*readOperation),
		idle:      idle,
	}
}

func (l *readLifecycle) begin(parent context.Context) (*readOperation, error) {
	if parent == nil {
		return nil, errors.New("context is required")
	}

	operationContext, cancel := context.WithCancel(parent)
	l.mu.Lock()
	defer l.mu.Unlock()
	if !l.accepting {
		cancel()
		return nil, ErrReadUnavailable
	}
	if len(l.active) == 0 {
		l.idle = make(chan struct{})
	}
	l.nextID++
	operation := &readOperation{
		id:        l.nextID,
		lifecycle: l,
		ctx:       operationContext,
		cancel:    cancel,
	}
	l.active[operation.id] = operation
	return operation, nil
}

func (l *readLifecycle) stopAccepting() <-chan struct{} {
	l.mu.Lock()
	l.accepting = false
	idle := l.idle
	l.mu.Unlock()
	return idle
}

func (l *readLifecycle) complete(id uint64) {
	l.mu.Lock()
	if _, present := l.active[id]; present {
		delete(l.active, id)
		if len(l.active) == 0 {
			close(l.idle)
		}
	}
	l.mu.Unlock()
}

func (l *readLifecycle) snapshot() []*readOperation {
	l.mu.Lock()
	defer l.mu.Unlock()
	operations := make([]*readOperation, 0, len(l.active))
	for _, operation := range l.active {
		operations = append(operations, operation)
	}
	return operations
}

// readOperation owns one pgx read-only transaction. useMu protects pgx objects,
// which are not safe for concurrent use. A force close first closes the socket,
// which interrupts a blocked pgx call, and only then waits for useMu.
type readOperation struct {
	id        uint64
	lifecycle *readLifecycle
	ctx       context.Context
	cancel    context.CancelFunc

	useMu sync.Mutex
	mu    sync.Mutex
	phase readOperationPhase
	tx    pgx.Tx
	rows  pgx.Rows

	finishOnce sync.Once
	finishErr  error
}

func (o *readOperation) attachTransaction(tx pgx.Tx) {
	o.mu.Lock()
	o.tx = tx
	o.mu.Unlock()
}

func (o *readOperation) attachRows(rows pgx.Rows) {
	o.mu.Lock()
	o.rows = rows
	o.mu.Unlock()
}

func (o *readOperation) clearRows() {
	o.mu.Lock()
	o.rows = nil
	o.mu.Unlock()
}

func (o *readOperation) isDone() bool {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.phase == readOperationDone
}

func (o *readOperation) error() error {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.finishErr
}

func (o *readOperation) finish() error {
	o.finishOnce.Do(func() {
		o.useMu.Lock()

		o.mu.Lock()
		rows := o.rows
		tx := o.tx
		o.mu.Unlock()

		// Rows.Close can itself wait for the server. Keep the operation forceable
		// until it returns; a concurrent force closes the socket and unblocks it.
		if rows != nil {
			rows.Close()
		}
		o.mu.Lock()
		o.phase = readOperationReleasing
		o.mu.Unlock()
		releaseErr := rollbackReadTransaction(tx)

		o.mu.Lock()
		o.rows = nil
		o.tx = nil
		o.finishErr = releaseErr
		o.phase = readOperationDone
		o.mu.Unlock()
		o.useMu.Unlock()

		o.cancel()
		o.lifecycle.complete(o.id)
	})
	return o.error()
}

func (o *readOperation) force() error {
	o.cancel()

	// Holding mu across net.Conn.Close prevents a concurrent normal finish from
	// returning this connection to the pool before the socket is closed.
	var connectionErr error
	o.mu.Lock()
	if o.phase == readOperationActive && o.tx != nil {
		connection := o.tx.Conn()
		if connection != nil && connection.PgConn() != nil && connection.PgConn().Conn() != nil {
			if err := connection.PgConn().Conn().Close(); err != nil && !errors.Is(err, net.ErrClosed) {
				connectionErr = fmt.Errorf("force close PostgreSQL read connection: %w", err)
			}
		}
	}
	o.mu.Unlock()

	return errors.Join(connectionErr, o.finish())
}

func rollbackReadTransaction(tx pgx.Tx) error {
	if tx == nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), readTransactionReleaseTimeout)
	defer cancel()
	err := tx.Rollback(ctx)
	if errors.Is(err, pgx.ErrTxClosed) {
		return nil
	}
	return err
}
