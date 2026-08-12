package runtime

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Rows 是不会暴露底层连接的只读查询结果。
type Rows interface {
	Close()
	Err() error
	Next() bool
	Scan(...any) error
}

// Row 是不会暴露 pgx 类型的单行只读结果。
type Row interface {
	Scan(...any) error
}

// ReadSnapshot 是一个 PostgreSQL REPEATABLE READ 只读事务内的查询能力。
//
// 该接口仅供 PostgreSQL Adapter 组合一致查询投影，不进入业务契约层。
type ReadSnapshot interface {
	Query(string, ...any) (Rows, error)
	QueryRow(string, ...any) Row
}

// ReadPool 仅暴露 PostgreSQL 查询能力。
//
// 底层每条普通连接都验证未切换登录身份并设置 default_transaction_read_only=on；
// 调用方不能取得连接或开启事务，Exec 入口也会无条件拒绝。Migration 后的数据库
// ACL 检查构成最终写权限边界。
type ReadPool struct {
	pool      *pgxpool.Pool
	lifecycle *readLifecycle
}

func newReadPool(pool *pgxpool.Pool) *ReadPool {
	return &ReadPool{pool: pool, lifecycle: newReadLifecycle()}
}

// Query 执行返回多行的只读查询。
func (p *ReadPool) Query(ctx context.Context, sql string, args ...any) (Rows, error) {
	if p == nil || p.pool == nil || p.lifecycle == nil {
		return nil, errors.New("query PostgreSQL read pool: pool is not initialized")
	}
	operation, err := p.lifecycle.begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("query PostgreSQL read pool: %w", err)
	}

	// Admission is registered before acquiring a pool connection. useMu keeps a
	// concurrent force close from completing the registration too early.
	operation.useMu.Lock()
	tx, err := p.pool.BeginTx(operation.ctx, pgx.TxOptions{AccessMode: pgx.ReadOnly})
	if err != nil {
		operation.useMu.Unlock()
		finishErr := operation.finish()
		return nil, errors.Join(
			fmt.Errorf("query PostgreSQL read pool: begin read-only transaction: %w", err),
			finishErr,
		)
	}
	operation.attachTransaction(tx)
	rows, err := tx.Query(operation.ctx, sql, args...)
	if err != nil {
		operation.useMu.Unlock()
		finishErr := operation.finish()
		return nil, errors.Join(err, finishErr)
	}
	operation.attachRows(rows)
	operation.useMu.Unlock()
	return &readRows{rows: rows, operation: operation}, nil
}

// QueryRow 执行返回单行的只读查询。
func (p *ReadPool) QueryRow(ctx context.Context, sql string, args ...any) Row {
	if p == nil || p.pool == nil || p.lifecycle == nil {
		return errorRow{err: errors.New("query PostgreSQL read pool: pool is not initialized")}
	}
	operation, err := p.lifecycle.begin(ctx)
	if err != nil {
		return errorRow{err: fmt.Errorf("query PostgreSQL read pool: %w", err)}
	}

	operation.useMu.Lock()
	tx, err := p.pool.BeginTx(operation.ctx, pgx.TxOptions{AccessMode: pgx.ReadOnly})
	if err != nil {
		operation.useMu.Unlock()
		finishErr := operation.finish()
		return errorRow{err: errors.Join(
			fmt.Errorf("query PostgreSQL read pool: begin read-only transaction: %w", err),
			finishErr,
		)}
	}
	operation.attachTransaction(tx)
	row := tx.QueryRow(operation.ctx, sql, args...)
	operation.useMu.Unlock()
	return &readRow{row: row, operation: operation}
}

// WithSnapshot 在一个 REPEATABLE READ 只读事务快照内执行组合查询。
func (p *ReadPool) WithSnapshot(
	ctx context.Context,
	work func(context.Context, ReadSnapshot) error,
) (result error) {
	if p == nil || p.pool == nil || p.lifecycle == nil {
		return errors.New("open PostgreSQL read snapshot: pool is not initialized")
	}
	if work == nil {
		return errors.New("open PostgreSQL read snapshot: work is required")
	}
	operation, err := p.lifecycle.begin(ctx)
	if err != nil {
		return fmt.Errorf("open PostgreSQL read snapshot: %w", err)
	}
	defer func() {
		result = errors.Join(result, operation.finish())
	}()

	operation.useMu.Lock()
	tx, err := p.pool.BeginTx(operation.ctx, pgx.TxOptions{
		IsoLevel:   pgx.RepeatableRead,
		AccessMode: pgx.ReadOnly,
	})
	if err != nil {
		operation.useMu.Unlock()
		return fmt.Errorf("open PostgreSQL read snapshot: begin transaction: %w", err)
	}
	operation.attachTransaction(tx)
	operation.useMu.Unlock()
	return work(operation.ctx, &readSnapshot{tx: tx, operation: operation})
}

// Exec 为通用数据库调用方提供 fail-closed 边界，并无条件拒绝执行。
//
// 只读查询必须使用 Query 或 QueryRow；所有写入必须使用 WriteExecutor。
func (*ReadPool) Exec(context.Context, string, ...any) (pgconn.CommandTag, error) {
	return pgconn.CommandTag{}, ErrReadOnlyPoolWrite
}

type errorRow struct {
	err error
}

type readRows struct {
	rows      pgx.Rows
	operation *readOperation
}

func (r *readRows) Close() {
	_ = r.operation.finish()
}

func (r *readRows) Err() error {
	r.operation.useMu.Lock()
	rowsErr := r.rows.Err()
	r.operation.useMu.Unlock()
	return errors.Join(rowsErr, r.operation.error())
}

func (r *readRows) Next() bool {
	r.operation.useMu.Lock()
	if r.operation.isDone() {
		r.operation.useMu.Unlock()
		return false
	}
	next := r.rows.Next()
	r.operation.useMu.Unlock()
	if next {
		return true
	}
	_ = r.operation.finish()
	return false
}

func (r *readRows) Scan(destinations ...any) error {
	r.operation.useMu.Lock()
	if r.operation.isDone() {
		r.operation.useMu.Unlock()
		return errors.Join(r.operation.ctx.Err(), r.operation.error())
	}
	err := r.rows.Scan(destinations...)
	r.operation.useMu.Unlock()
	if err != nil {
		return errors.Join(err, r.operation.finish())
	}
	return nil
}

type readRow struct {
	row       pgx.Row
	operation *readOperation
}

type readSnapshot struct {
	tx        pgx.Tx
	operation *readOperation
}

func (s *readSnapshot) Query(sql string, args ...any) (Rows, error) {
	s.operation.useMu.Lock()
	if s.operation.isDone() {
		s.operation.useMu.Unlock()
		return nil, errors.Join(s.operation.ctx.Err(), s.operation.error())
	}
	rows, err := s.tx.Query(s.operation.ctx, sql, args...)
	if err != nil {
		s.operation.useMu.Unlock()
		return nil, err
	}
	s.operation.attachRows(rows)
	s.operation.useMu.Unlock()
	return &snapshotRows{rows: rows, operation: s.operation}, nil
}

func (s *readSnapshot) QueryRow(sql string, args ...any) Row {
	s.operation.useMu.Lock()
	defer s.operation.useMu.Unlock()
	if s.operation.isDone() {
		return errorRow{err: errors.Join(s.operation.ctx.Err(), s.operation.error())}
	}
	return &snapshotRow{row: s.tx.QueryRow(s.operation.ctx, sql, args...), operation: s.operation}
}

type snapshotRows struct {
	rows      pgx.Rows
	operation *readOperation
	closed    bool
}

func (r *snapshotRows) Close() {
	r.operation.useMu.Lock()
	r.closeLocked()
	r.operation.useMu.Unlock()
}

func (r *snapshotRows) Err() error {
	r.operation.useMu.Lock()
	err := r.rows.Err()
	r.operation.useMu.Unlock()
	return errors.Join(err, r.operation.error())
}

func (r *snapshotRows) Next() bool {
	r.operation.useMu.Lock()
	defer r.operation.useMu.Unlock()
	if r.closed || r.operation.isDone() {
		return false
	}
	if r.rows.Next() {
		return true
	}
	r.closeLocked()
	return false
}

func (r *snapshotRows) Scan(destinations ...any) error {
	r.operation.useMu.Lock()
	defer r.operation.useMu.Unlock()
	if r.closed || r.operation.isDone() {
		return errors.Join(r.operation.ctx.Err(), r.operation.error())
	}
	if err := r.rows.Scan(destinations...); err != nil {
		r.closeLocked()
		return err
	}
	return nil
}

func (r *snapshotRows) closeLocked() {
	if r.closed {
		return
	}
	r.rows.Close()
	r.operation.clearRows()
	r.closed = true
}

type snapshotRow struct {
	row       pgx.Row
	operation *readOperation
}

func (r *snapshotRow) Scan(destinations ...any) error {
	r.operation.useMu.Lock()
	defer r.operation.useMu.Unlock()
	if r.operation.isDone() {
		return errors.Join(r.operation.ctx.Err(), r.operation.error())
	}
	return r.row.Scan(destinations...)
}

func (r *readRow) Scan(destinations ...any) error {
	r.operation.useMu.Lock()
	if r.operation.isDone() {
		r.operation.useMu.Unlock()
		return errors.Join(r.operation.ctx.Err(), r.operation.error())
	}
	scanErr := r.row.Scan(destinations...)
	r.operation.useMu.Unlock()
	rollbackErr := r.operation.finish()
	return errors.Join(scanErr, rollbackErr)
}

func (r errorRow) Scan(...any) error {
	return r.err
}

func (p *ReadPool) stopAccepting() <-chan struct{} {
	if p == nil || p.lifecycle == nil {
		idle := make(chan struct{})
		close(idle)
		return idle
	}
	return p.lifecycle.stopAccepting()
}

// shutdown waits for admitted reads, forcibly terminates them when ctx expires,
// and closes pgxpool only after every tracked transaction has returned.
func (p *ReadPool) shutdown(ctx context.Context) error {
	if p == nil || p.pool == nil || p.lifecycle == nil {
		return nil
	}
	idle := p.stopAccepting()
	select {
	case <-idle:
		p.pool.Close()
		return nil
	default:
	}

	select {
	case <-idle:
		p.pool.Close()
		return nil
	case <-ctx.Done():
	}

	operations := p.lifecycle.snapshot()
	forceErrors := make(chan error, len(operations))
	var waitGroup sync.WaitGroup
	for _, operation := range operations {
		waitGroup.Add(1)
		go func(active *readOperation) {
			defer waitGroup.Done()
			if err := active.force(); err != nil {
				forceErrors <- err
			}
		}(operation)
	}
	waitGroup.Wait()
	close(forceErrors)
	<-idle

	// pgxpool.Close has no error return. At this point no public read operation
	// still owns a pool resource, so this call cannot wait on an application-held
	// Rows or Row.
	p.pool.Close()
	shutdownErr := fmt.Errorf("close PostgreSQL read pool: %w", ctx.Err())
	for err := range forceErrors {
		shutdownErr = errors.Join(shutdownErr, err)
	}
	return shutdownErr
}

// DatabaseClock 从 PostgreSQL 获取权威 UTC 时间。
type DatabaseClock struct {
	reader *ReadPool
}

// Now 返回数据库 clock_timestamp，而不是进程本地时间。
func (c *DatabaseClock) Now(ctx context.Context) (time.Time, error) {
	if c == nil || c.reader == nil {
		return time.Time{}, errors.New("read database clock: clock is not initialized")
	}

	var now time.Time
	if err := c.reader.QueryRow(ctx, "SELECT clock_timestamp()").Scan(&now); err != nil {
		return time.Time{}, fmt.Errorf("read database clock: %w", err)
	}
	return now.UTC(), nil
}
