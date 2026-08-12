package runtime_test

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/zhaohaip/agentops-go/internal/contracts"
	domain "github.com/zhaohaip/agentops-go/internal/taskruntime"
	"github.com/zhaohaip/agentops-go/internal/worker"
)

// These tests compose the real Phase 1 application services over one strict,
// transactional fake store. Checkpoint and Pending Report assertions validate
// orchestration and transaction-token propagation only; PostgreSQL atomicity for
// those future providers is deliberately not claimed here.
func TestPhase1FIFOCreateClaimExecuteToTerminal(t *testing.T) {
	h := newPhase1Harness(t)
	first := h.createTask(t, "command-create-first", "first secret input")
	second := h.createTask(t, "command-create-second", "second secret input")

	ctx, stop := context.WithCancelCause(context.Background())
	port := &serviceRuntimePort{claims: h.claim, execute: h.execute, stop: stop, completedTwo: make(chan struct{})}
	instance, err := worker.New(port, h.workerID, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- instance.Run(ctx) }()
	awaitClosed(t, port.completedTwo, "two FIFO executions did not complete")
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Worker.Run() error = %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Worker did not stop after two executions")
	}

	if len(port.claimed) != 2 || port.claimed[0] != first.TaskID || port.claimed[1] != second.TaskID {
		t.Fatalf("FIFO claims = %v, want [%s %s]", port.claimed, first.TaskID, second.TaskID)
	}
	if port.maximum.Load() != 1 {
		t.Fatalf("maximum concurrent executions = %d, want 1", port.maximum.Load())
	}
	store := h.executor.snapshot()
	for _, taskID := range []contracts.TaskID{first.TaskID, second.TaskID} {
		assertTerminalSuccess(t, store, taskID)
	}
	assertSafeTaskLogs(t, store.logs, "first secret input", "second secret input")
	assertEvents(t, store.logs, map[string]int{"TaskCreated": 2, "ExecutionClaimed": 2})
	if store.rollupReports() != 0 {
		t.Fatalf("successful flow Pending Reports = %d, want 0", store.rollupReports())
	}
}

func TestPhase1CancelAndClaimCommitOrders(t *testing.T) {
	t.Run("Claim commits before Cancel", func(t *testing.T) {
		h := newPhase1Harness(t)
		created := h.createTask(t, "command-claim-first", "input")
		barrier := newOperationBarrier()
		h.repositories.claimBarrier = barrier
		claimResult := make(chan claimCallResult, 1)
		go func() {
			result, err := h.claim.ClaimNextExecution(context.Background(), h.workerID)
			claimResult <- claimCallResult{result: result, err: err}
		}()
		barrier.awaitEntered(t)

		cancelResult := make(chan cancelCallResult, 1)
		go func() {
			result, err := h.cancel.CancelTask(context.Background(), domain.CancelTaskRequest{
				CommandID: "command-cancel-after-claim", TaskID: created.TaskID, OperatorID: "operator",
			})
			cancelResult <- cancelCallResult{result: result, err: err}
		}()
		h.executor.awaitWaiter(t)
		barrier.release()

		claimed := awaitClaimCall(t, claimResult)
		if claimed.err != nil {
			t.Fatalf("ClaimNextExecution() error = %v", claimed.err)
		}
		if _, ok := claimed.result.(contracts.ClaimResultClaimed); !ok {
			t.Fatalf("Claim result = %T, want Claimed", claimed.result)
		}
		cancelled := awaitCancelCall(t, cancelResult)
		if cancelled.err != nil || cancelled.result.TaskStatus != contracts.TaskStatusCancelled {
			t.Fatalf("Cancel after Claim = %+v, %v", cancelled.result, cancelled.err)
		}
		assertCancelled(t, h.executor.snapshot(), created.TaskID)
	})

	t.Run("Cancel commits before Claim", func(t *testing.T) {
		h := newPhase1Harness(t)
		created := h.createTask(t, "command-cancel-first", "input")
		barrier := newOperationBarrier()
		h.repositories.terminationBarrier = barrier
		cancelResult := make(chan cancelCallResult, 1)
		go func() {
			result, err := h.cancel.CancelTask(context.Background(), domain.CancelTaskRequest{
				CommandID: "command-cancel-before-claim", TaskID: created.TaskID, OperatorID: "operator",
			})
			cancelResult <- cancelCallResult{result: result, err: err}
		}()
		barrier.awaitEntered(t)

		claimResult := make(chan claimCallResult, 1)
		go func() {
			result, err := h.claim.ClaimNextExecution(context.Background(), h.workerID)
			claimResult <- claimCallResult{result: result, err: err}
		}()
		h.executor.awaitWaiter(t)
		barrier.release()

		cancelled := awaitCancelCall(t, cancelResult)
		if cancelled.err != nil || cancelled.result.TaskStatus != contracts.TaskStatusCancelled {
			t.Fatalf("Cancel before Claim = %+v, %v", cancelled.result, cancelled.err)
		}
		claimed := awaitClaimCall(t, claimResult)
		if claimed.err != nil {
			t.Fatalf("Claim after Cancel error = %v", claimed.err)
		}
		if _, ok := claimed.result.(contracts.ClaimResultNoWork); !ok {
			t.Fatalf("Claim after Cancel = %T, want NoWork", claimed.result)
		}
		assertCancelled(t, h.executor.snapshot(), created.TaskID)
	})
}

func TestPhase1TimeoutWinsAgainstActiveExecution(t *testing.T) {
	h := newPhase1Harness(t)
	created := h.createTask(t, "command-timeout-active", "input")
	claimResult, err := h.claim.ClaimNextExecution(context.Background(), h.workerID)
	if err != nil {
		t.Fatal(err)
	}
	claimed, ok := claimResult.(contracts.ClaimResultClaimed)
	if !ok {
		t.Fatalf("Claim result = %T", claimResult)
	}

	planner := &blockingPlanner{entered: make(chan struct{})}
	h.replacePlanner(t, planner)
	executeResult := make(chan executeCallResult, 1)
	go func() {
		result, executeErr := h.execute.ExecuteClaimedExecution(context.Background(), claimed.Claim)
		executeResult <- executeCallResult{result: result, err: executeErr}
	}()
	awaitClosed(t, planner.entered, "Planner active call did not start")

	store := h.executor.snapshot()
	h.clock.set(store.tasks[created.TaskID].DeadlineAt)
	expired, err := h.expire.ExpireTask(context.Background(), domain.ExpireTaskRequest{
		TaskID: created.TaskID, ObservedExecutionVersion: claimed.Claim.ExecutionVersion,
	})
	if err != nil {
		t.Fatalf("ExpireTask() error = %v", err)
	}
	if _, ok := expired.(domain.ExpireTaskExpired); !ok {
		t.Fatalf("ExpireTask() = %T, want Expired", expired)
	}
	executed := awaitExecuteCall(t, executeResult)
	if executed.err != nil {
		t.Fatalf("Execute after timeout error = %v", executed.err)
	}
	if _, ok := executed.result.(contracts.ExecuteResultStale); !ok {
		t.Fatalf("late Execute result = %T, want Stale", executed.result)
	}
	assertTimedOut(t, h.executor.snapshot(), created.TaskID)
	store = h.executor.snapshot()
	assertSafeTaskLogs(t, store.logs, "input")
	assertEvents(t, store.logs, map[string]int{"TaskTerminalized": 1})
}

func TestPhase1TimeoutCommitsBeforeClaim(t *testing.T) {
	h := newPhase1Harness(t)
	created := h.createTask(t, "command-timeout-before-claim", "input")
	store := h.executor.snapshot()
	h.clock.set(store.tasks[created.TaskID].DeadlineAt)
	barrier := newOperationBarrier()
	h.repositories.terminationBarrier = barrier

	timeoutResult := make(chan expireCallResult, 1)
	go func() {
		result, err := h.expire.ExpireTask(context.Background(), domain.ExpireTaskRequest{
			TaskID: created.TaskID, ObservedExecutionVersion: 1,
		})
		timeoutResult <- expireCallResult{result: result, err: err}
	}()
	barrier.awaitEntered(t)

	claimResult := make(chan claimCallResult, 1)
	go func() {
		result, err := h.claim.ClaimNextExecution(context.Background(), h.workerID)
		claimResult <- claimCallResult{result: result, err: err}
	}()
	h.executor.awaitWaiter(t)
	barrier.release()

	expired := awaitExpireCall(t, timeoutResult)
	if expired.err != nil {
		t.Fatalf("ExpireTask before Claim error = %v", expired.err)
	}
	if _, ok := expired.result.(domain.ExpireTaskExpired); !ok {
		t.Fatalf("ExpireTask before Claim = %T, want Expired", expired.result)
	}
	claimed := awaitClaimCall(t, claimResult)
	if claimed.err != nil {
		t.Fatalf("Claim after Timeout error = %v", claimed.err)
	}
	if _, ok := claimed.result.(contracts.ClaimResultNoWork); !ok {
		t.Fatalf("Claim after Timeout = %T, want NoWork", claimed.result)
	}
	assertTimedOut(t, h.executor.snapshot(), created.TaskID)
}

func TestPhase1StartupCleanupGate(t *testing.T) {
	t.Run("cleanup succeeds before component start", func(t *testing.T) {
		h := newPhase1Harness(t)
		created := h.createTask(t, "command-startup-success", "input")
		claim, err := h.claim.ClaimNextExecution(context.Background(), h.workerID)
		if err != nil {
			t.Fatal(err)
		}
		if _, ok := claim.(contracts.ClaimResultClaimed); !ok {
			t.Fatalf("Claim result = %T", claim)
		}
		started := atomic.Bool{}
		summary, err := runStartupGate(context.Background(), h.startup, "replacement-worker", func() { started.Store(true) })
		if err != nil || !started.Load() || summary.Interrupted != 1 {
			t.Fatalf("startup gate = %+v, %v, started=%v", summary, err, started.Load())
		}
		store := h.executor.snapshot()
		execution := store.executions[executionKey(created.TaskID, 1)]
		if execution.Status != contracts.TaskExecutionStatusInterrupted {
			t.Fatalf("StartupCleanup Execution status = %s, want INTERRUPTED", execution.Status)
		}
	})

	t.Run("cleanup failure rolls back and blocks component", func(t *testing.T) {
		h := newPhase1Harness(t)
		created := h.createTask(t, "command-startup-failure", "input")
		claim, err := h.claim.ClaimNextExecution(context.Background(), h.workerID)
		if err != nil {
			t.Fatal(err)
		}
		if _, ok := claim.(contracts.ClaimResultClaimed); !ok {
			t.Fatalf("Claim result = %T", claim)
		}
		h.checkpoints.startupErr = errors.New("checkpoint provider unavailable")
		before := h.executor.snapshot()
		rollbacksBefore := h.executor.rollbacks.Load()
		started := atomic.Bool{}
		_, err = runStartupGate(context.Background(), h.startup, "replacement-worker", func() { started.Store(true) })
		if err == nil || started.Load() {
			t.Fatalf("failed startup gate error=%v, started=%v", err, started.Load())
		}
		if h.executor.rollbacks.Load() != rollbacksBefore+1 {
			t.Fatalf("StartupCleanup rollback count = %d, want %d", h.executor.rollbacks.Load(), rollbacksBefore+1)
		}
		after := h.executor.snapshot()
		beforeExecution := before.executions[executionKey(created.TaskID, 1)]
		afterExecution := after.executions[executionKey(created.TaskID, 1)]
		if beforeExecution.Status != afterExecution.Status || before.rollupReports() != after.rollupReports() {
			t.Fatalf("failed StartupCleanup partially committed: before=%+v after=%+v", beforeExecution, afterExecution)
		}
	})
}

type serviceRuntimePort struct {
	claims       *domain.ClaimTaskService
	execute      *domain.ExecuteTaskService
	stop         context.CancelCauseFunc
	claimed      []contracts.TaskID
	active       atomic.Int64
	maximum      atomic.Int64
	completed    atomic.Int64
	completedTwo chan struct{}
}

func (p *serviceRuntimePort) ClaimNextExecution(ctx context.Context, workerID contracts.WorkerID) (contracts.ClaimResult, error) {
	result, err := p.claims.ClaimNextExecution(ctx, workerID)
	if claimed, ok := result.(contracts.ClaimResultClaimed); ok {
		p.claimed = append(p.claimed, claimed.Claim.TaskID)
	}
	return result, err
}

func (p *serviceRuntimePort) ExecuteClaimedExecution(ctx context.Context, claim contracts.ExecutionClaim) (contracts.ExecuteResult, error) {
	active := p.active.Add(1)
	defer p.active.Add(-1)
	for maximum := p.maximum.Load(); active > maximum && !p.maximum.CompareAndSwap(maximum, active); maximum = p.maximum.Load() {
	}
	result, err := p.execute.ExecuteClaimedExecution(ctx, claim)
	if err == nil && p.completed.Add(1) == 2 {
		close(p.completedTwo)
		p.stop(worker.ErrRuntimeShutdown)
	}
	return result, err
}

func assertSafeTaskLogs(t *testing.T, logs []domain.TaskLog, forbidden ...string) {
	t.Helper()
	for _, log := range logs {
		payload := log.Event + " " + log.Message + " " + log.Operator
		for _, secret := range forbidden {
			if strings.Contains(payload, secret) {
				t.Fatalf("TaskLog leaked raw input %q: %+v", secret, log)
			}
		}
	}
}
