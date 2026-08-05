package taskruntime

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/zhaohaip/agentops-go/internal/contracts"
)

func TestTaskLogTryExecuteDoesNotOvertakeWaitingDomainWrite(t *testing.T) {
	t.Parallel()
	executor := newPriorityFakeExecutor()
	domainEntered := make(chan struct{})
	domainDone := make(chan error, 1)
	go func() {
		domainDone <- executor.Execute(context.Background(), func(context.Context, contracts.RuntimeWriteTx) error {
			close(domainEntered)
			return nil
		})
	}()
	<-executor.waiterRegistered

	logs := &countingTaskLogRepository{}
	logReturned := make(chan struct{})
	go func() {
		appendTaskLogBestEffort(context.Background(), executor, logs, fixedTaskLogClock{}, taskLogDraft{
			taskID: "task-priority", runID: "run-priority", executionVersion: 1,
			level: TaskLogLevelInfo, event: taskLogEventTaskCreated, message: "task created",
		})
		close(logReturned)
	}()
	select {
	case <-logReturned:
	case <-time.After(time.Second):
		t.Fatal("TaskLog waited behind the occupied write gate")
	}
	select {
	case <-domainEntered:
		t.Fatal("waiting domain write entered before the active gate was released")
	default:
	}
	if logs.calls.Load() != 0 {
		t.Fatal("dropped TaskLog executed its Repository callback")
	}
	if executor.tryCalls.Load() != 1 || executor.executeCalls.Load() != 1 {
		t.Fatalf("executor calls = Execute:%d TryExecute:%d, want domain Execute and TaskLog TryExecute",
			executor.executeCalls.Load(), executor.tryCalls.Load())
	}

	executor.releaseActive()
	select {
	case err := <-domainDone:
		if err != nil {
			t.Fatalf("waiting domain write error = %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("waiting domain write did not proceed after gate release")
	}
}

type priorityFakeExecutor struct {
	gate             chan struct{}
	stateMu          sync.Mutex
	waiters          int
	waiterRegistered chan struct{}
	waiterOnce       sync.Once
	executeCalls     atomic.Int64
	tryCalls         atomic.Int64
}

func newPriorityFakeExecutor() *priorityFakeExecutor {
	return &priorityFakeExecutor{gate: make(chan struct{}, 1), waiterRegistered: make(chan struct{})}
}

func (e *priorityFakeExecutor) Execute(
	ctx context.Context,
	work func(context.Context, contracts.RuntimeWriteTx) error,
) error {
	e.executeCalls.Add(1)
	e.stateMu.Lock()
	e.waiters++
	e.waiterOnce.Do(func() { close(e.waiterRegistered) })
	e.stateMu.Unlock()
	select {
	case <-ctx.Done():
		e.stateMu.Lock()
		e.waiters--
		e.stateMu.Unlock()
		return ctx.Err()
	case <-e.gate:
	}
	e.stateMu.Lock()
	e.waiters--
	e.stateMu.Unlock()
	defer func() { e.gate <- struct{}{} }()
	return work(ctx, fakePriorityWriteTx{})
}

func (e *priorityFakeExecutor) TryExecute(
	ctx context.Context,
	work func(context.Context, contracts.RuntimeWriteTx) error,
) (bool, error) {
	e.tryCalls.Add(1)
	e.stateMu.Lock()
	defer e.stateMu.Unlock()
	if e.waiters != 0 {
		return false, nil
	}
	select {
	case <-e.gate:
		defer func() { e.gate <- struct{}{} }()
		return true, work(ctx, fakePriorityWriteTx{})
	default:
		return false, nil
	}
}

func (e *priorityFakeExecutor) releaseActive() {
	e.gate <- struct{}{}
}

type fakePriorityWriteTx struct{}

func (fakePriorityWriteTx) AgentOpsRuntimeWriteTx() {}

type countingTaskLogRepository struct {
	calls atomic.Int64
}

func (r *countingTaskLogRepository) Append(context.Context, contracts.RuntimeWriteTx, TaskLog) error {
	r.calls.Add(1)
	return nil
}

type fixedTaskLogClock struct{}

func (fixedTaskLogClock) Now(context.Context, contracts.RuntimeWriteTx) (time.Time, error) {
	return time.Date(2026, time.August, 5, 0, 0, 0, 0, time.UTC), nil
}

var _ contracts.RuntimeWriteExecutor = (*priorityFakeExecutor)(nil)
