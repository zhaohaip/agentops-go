package runtime

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/zhaohaip/agentops-go/internal/contracts"
)

var (
	// ErrInvalidRuntimeWriteTx 表示 PostgreSQL Repository 收到了非当前 Adapter 创建的事务令牌。
	ErrInvalidRuntimeWriteTx = errors.New("runtime write transaction is not owned by PostgreSQL adapter")
)

// writeExecutor 在持锁连接上串行执行 READ COMMITTED 短事务。
type writeExecutor struct {
	runtime *Runtime
}

// Execute 串行开始事务，并根据 work 结果提交或回滚。
func (e *writeExecutor) Execute(
	ctx context.Context,
	work func(context.Context, contracts.RuntimeWriteTx) error,
) error {
	if e == nil || e.runtime == nil {
		return errors.New("execute PostgreSQL runtime write: executor is not initialized")
	}
	if ctx == nil {
		return errors.New("execute PostgreSQL runtime write: context is required")
	}
	if work == nil {
		return errors.New("execute PostgreSQL runtime write: work is required")
	}

	runtime := e.runtime
	if err := runtime.requireAcceptingWrites(); err != nil {
		return fmt.Errorf("execute PostgreSQL runtime write: %w", err)
	}
	if err := runtime.acquireGate(ctx); err != nil {
		return fmt.Errorf("execute PostgreSQL runtime write: %w", err)
	}
	defer runtime.releaseGate()

	// gate 是活动事务登记；本次检查与 StopAcceptingWrites 通过 stateMu 线性化。
	// 检查成功后即使 Host 随后封闭准入，本事务仍由 Close 等待或超时中止。
	if err := runtime.admitWrite(); err != nil {
		return fmt.Errorf("execute PostgreSQL runtime write: %w", err)
	}
	runtime.lockConnUse.Lock()
	tx, err := runtime.lockConn.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	runtime.lockConnUse.Unlock()
	if err != nil {
		runtime.observeConnectionFailure(err)
		return fmt.Errorf("execute PostgreSQL runtime write: begin transaction: %w", err)
	}

	committed := false
	defer func() {
		if !committed {
			runtime.lockConnUse.Lock()
			_ = tx.Rollback(context.WithoutCancel(ctx))
			runtime.lockConnUse.Unlock()
		}
	}()

	if err := work(ctx, &writeTx{tx: tx, runtime: runtime}); err != nil {
		runtime.lockConnUse.Lock()
		rollbackErr := tx.Rollback(context.WithoutCancel(ctx))
		runtime.lockConnUse.Unlock()
		if rollbackErr != nil && !errors.Is(rollbackErr, pgx.ErrTxClosed) {
			runtime.observeConnectionFailure(rollbackErr)
			return errors.Join(err, fmt.Errorf("execute PostgreSQL runtime write: rollback transaction: %w", rollbackErr))
		}
		runtime.observeConnectionFailure(err)
		return err
	}
	runtime.lockConnUse.Lock()
	err = tx.Commit(ctx)
	runtime.lockConnUse.Unlock()
	if err != nil {
		runtime.observeConnectionFailure(err)
		return fmt.Errorf("execute PostgreSQL runtime write: commit transaction: %w", err)
	}
	committed = true
	return nil
}

type writeTx struct {
	tx      pgx.Tx
	runtime *Runtime
}

var (
	_ contracts.RuntimeWriteExecutor = (*writeExecutor)(nil)
	_ contracts.RuntimeWriteTx       = (*writeTx)(nil)
)

func (*writeTx) AgentOpsRuntimeWriteTx() {}

// WithPostgreSQLWriteTx 供 PostgreSQL Repository Adapter 在当前不透明令牌中执行操作。
//
// 应用层不得调用此函数；它只属于 PostgreSQL Adapter 内部的事务解包边界。
func WithPostgreSQLWriteTx(
	token contracts.RuntimeWriteTx,
	work func(pgx.Tx) error,
) error {
	transaction, ok := token.(*writeTx)
	if !ok || transaction == nil || transaction.tx == nil || transaction.runtime == nil {
		return ErrInvalidRuntimeWriteTx
	}
	if work == nil {
		return errors.New("use PostgreSQL runtime write transaction: work is required")
	}

	transaction.runtime.lockConnUse.Lock()
	defer transaction.runtime.lockConnUse.Unlock()
	return work(transaction.tx)
}
