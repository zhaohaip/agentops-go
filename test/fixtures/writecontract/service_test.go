package writecontract_test

import (
	"context"
	"errors"
	"testing"

	"github.com/zhaohaip/agentops-go/internal/contracts"
	"github.com/zhaohaip/agentops-go/test/fixtures/writecontract"
)

func TestServiceUsesFakeExecutorAndSharesOpaqueTransaction(t *testing.T) {
	t.Parallel()

	transaction := &fakeRuntimeWriteTx{}
	executor := &fakeRuntimeWriteExecutor{tx: transaction}
	first := &recordingRepository{}
	second := &recordingRepository{}
	service := writecontract.NewService(executor, first, second)

	if err := service.StorePair(context.Background(), "first", "second"); err != nil {
		t.Fatalf("StorePair() error = %v", err)
	}
	if executor.calls != 1 || !executor.committed || executor.rolledBack {
		t.Fatalf("executor state = calls:%d committed:%t rolledBack:%t", executor.calls, executor.committed, executor.rolledBack)
	}
	if first.tx != transaction || second.tx != transaction || first.tx != second.tx {
		t.Fatal("Repositories did not receive the same RuntimeWriteTx")
	}
}

func TestServiceFakeExecutorRollsBackRepositoryError(t *testing.T) {
	t.Parallel()

	wantErr := errors.New("second repository failed")
	executor := &fakeRuntimeWriteExecutor{tx: &fakeRuntimeWriteTx{}}
	service := writecontract.NewService(
		executor,
		&recordingRepository{},
		&recordingRepository{err: wantErr},
	)

	if err := service.StorePair(context.Background(), "first", "second"); !errors.Is(err, wantErr) {
		t.Fatalf("StorePair() error = %v, want %v", err, wantErr)
	}
	if executor.committed || !executor.rolledBack {
		t.Fatalf("executor state = committed:%t rolledBack:%t", executor.committed, executor.rolledBack)
	}
}

type fakeRuntimeWriteTx struct{}

func (*fakeRuntimeWriteTx) AgentOpsRuntimeWriteTx() {}

type fakeRuntimeWriteExecutor struct {
	tx         contracts.RuntimeWriteTx
	calls      int
	committed  bool
	rolledBack bool
}

func (e *fakeRuntimeWriteExecutor) Execute(
	ctx context.Context,
	work func(context.Context, contracts.RuntimeWriteTx) error,
) error {
	e.calls++
	err := work(ctx, e.tx)
	e.committed = err == nil
	e.rolledBack = err != nil
	return err
}

type recordingRepository struct {
	tx    contracts.RuntimeWriteTx
	value string
	err   error
}

func (r *recordingRepository) Store(
	_ context.Context,
	tx contracts.RuntimeWriteTx,
	value string,
) error {
	r.tx = tx
	r.value = value
	return r.err
}

var (
	_ contracts.RuntimeWriteTx       = (*fakeRuntimeWriteTx)(nil)
	_ contracts.RuntimeWriteExecutor = (*fakeRuntimeWriteExecutor)(nil)
)
