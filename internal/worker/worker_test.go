package worker_test

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/zhaohaip/agentops-go/internal/contracts"
	"github.com/zhaohaip/agentops-go/internal/worker"
)

func TestNewValidatesWorkerConfiguration(t *testing.T) {
	t.Parallel()
	port := &scriptedRuntimePort{}
	tests := []struct {
		name     string
		port     worker.RuntimePort
		workerID contracts.WorkerID
		interval time.Duration
	}{
		{name: "nil port", workerID: "worker-1", interval: time.Second},
		{name: "empty worker", port: port, interval: time.Second},
		{name: "zero interval", port: port, workerID: "worker-1"},
		{name: "negative interval", port: port, workerID: "worker-1", interval: -time.Second},
		{name: "interval above maximum", port: port, workerID: "worker-1", interval: 5*time.Second + time.Nanosecond},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			if instance, err := worker.New(test.port, test.workerID, test.interval); err == nil || instance != nil {
				t.Fatalf("New() = %v, %v; want configuration error", instance, err)
			}
		})
	}
	if instance, err := worker.New(port, "worker-1", 5*time.Second); err != nil || instance == nil {
		t.Fatalf("New(valid) = %v, %v", instance, err)
	}
}

func TestWorkerNoWorkWaitIsCancelableAndRunCannotRestart(t *testing.T) {
	t.Parallel()
	claimCalled := make(chan struct{})
	port := &scriptedRuntimePort{claim: func(context.Context, contracts.WorkerID) (contracts.ClaimResult, error) {
		select {
		case <-claimCalled:
		default:
			close(claimCalled)
		}
		return contracts.ClaimResultNoWork{}, nil
	}}
	instance := newWorker(t, port)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- instance.Run(ctx) }()
	<-claimCalled
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run() error = %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Run did not leave NoWork wait after cancellation")
	}
	if err := instance.Run(context.Background()); !errors.Is(err, worker.ErrAlreadyStarted) {
		t.Fatalf("second Run() error = %v, want AlreadyStarted", err)
	}
}

func TestWorkerPreCanceledContextDoesNotClaim(t *testing.T) {
	t.Parallel()
	var claims atomic.Int64
	port := &scriptedRuntimePort{claim: func(context.Context, contracts.WorkerID) (contracts.ClaimResult, error) {
		claims.Add(1)
		return contracts.ClaimResultNoWork{}, nil
	}}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := newWorker(t, port).Run(ctx); err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if claims.Load() != 0 {
		t.Fatalf("Claim calls = %d, want 0", claims.Load())
	}
}

func TestWorkerNoWorkPollsAgainAfterInterval(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancelCause(context.Background())
	var claims atomic.Int64
	port := &scriptedRuntimePort{claim: func(context.Context, contracts.WorkerID) (contracts.ClaimResult, error) {
		if claims.Add(1) == 1 {
			return contracts.ClaimResultNoWork{}, nil
		}
		cancel(worker.ErrRuntimeShutdown)
		return nil, context.Canceled
	}}
	instance, err := worker.New(port, "worker-1", time.Millisecond)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	if err := instance.Run(ctx); err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if claims.Load() != 2 {
		t.Fatalf("Claim calls = %d, want 2", claims.Load())
	}
}

func TestWorkerClaimExecuteSequenceAndSingleSlot(t *testing.T) {
	t.Parallel()
	workerID := contracts.WorkerID("worker-1")
	firstClaim := contracts.ExecutionClaim{TaskID: "task-1", RunID: "run-1", ExecutionVersion: 1, WorkerID: workerID}
	secondClaim := contracts.ExecutionClaim{TaskID: "task-2", RunID: "run-2", ExecutionVersion: 1, WorkerID: workerID}
	releaseFirst := make(chan struct{})
	firstExecuteEntered := make(chan struct{})
	var claimCalls atomic.Int64
	var active atomic.Int64
	var maximum atomic.Int64
	port := &scriptedRuntimePort{}
	port.claim = func(ctx context.Context, gotWorkerID contracts.WorkerID) (contracts.ClaimResult, error) {
		call := claimCalls.Add(1)
		if gotWorkerID != workerID {
			t.Errorf("worker ID = %q, want %q", gotWorkerID, workerID)
		}
		switch call {
		case 1:
			return contracts.ClaimResultClaimed{Claim: firstClaim}, nil
		case 2:
			return contracts.ClaimResultClaimed{Claim: secondClaim}, nil
		default:
			<-ctx.Done()
			return nil, ctx.Err()
		}
	}
	ctx, cancel := context.WithCancelCause(context.Background())
	port.execute = func(_ context.Context, claim contracts.ExecutionClaim) (contracts.ExecuteResult, error) {
		current := active.Add(1)
		defer active.Add(-1)
		for {
			old := maximum.Load()
			if current <= old || maximum.CompareAndSwap(old, current) {
				break
			}
		}
		if claim == firstClaim {
			close(firstExecuteEntered)
			<-releaseFirst
			return contracts.ExecuteResultWaitingApproval{}, nil
		}
		cancel(worker.ErrRuntimeShutdown)
		return contracts.ExecuteResultTerminal{}, nil
	}
	instance := newWorker(t, port)
	done := make(chan error, 1)
	go func() { done <- instance.Run(ctx) }()
	<-firstExecuteEntered
	if claimCalls.Load() != 1 {
		t.Fatalf("Claim calls while Execute blocked = %d, want 1", claimCalls.Load())
	}
	close(releaseFirst)
	if err := <-done; err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if claimCalls.Load() != 2 || port.executeCalls.Load() != 2 || maximum.Load() != 1 {
		t.Fatalf("calls/max = claim:%d execute:%d max:%d, want 2/2/1",
			claimCalls.Load(), port.executeCalls.Load(), maximum.Load())
	}
}

func TestWorkerPassesClaimAfterContextCancellation(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancelCause(context.Background())
	claim := contracts.ExecutionClaim{
		TaskID: "task-1", RunID: "run-1", ExecutionVersion: 1, WorkerID: "worker-1",
	}
	executed := make(chan struct{})
	port := &scriptedRuntimePort{
		claim: func(context.Context, contracts.WorkerID) (contracts.ClaimResult, error) {
			cancel(worker.ErrRuntimeShutdown)
			return contracts.ClaimResultClaimed{Claim: claim}, nil
		},
		execute: func(gotContext context.Context, gotClaim contracts.ExecutionClaim) (contracts.ExecuteResult, error) {
			if gotClaim != claim {
				t.Fatalf("claim = %+v, want %+v", gotClaim, claim)
			}
			if !errors.Is(gotContext.Err(), context.Canceled) {
				t.Fatalf("Execute context error = %v, want canceled", gotContext.Err())
			}
			close(executed)
			return contracts.ExecuteResultStale{}, nil
		},
	}
	if err := newWorker(t, port).Run(ctx); err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	select {
	case <-executed:
	default:
		t.Fatal("Execute was not called for an already claimed execution")
	}
}

func TestWorkerAcceptsStaleExecuteResult(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancelCause(context.Background())
	var claims atomic.Int64
	port := &scriptedRuntimePort{
		claim: func(context.Context, contracts.WorkerID) (contracts.ClaimResult, error) {
			if claims.Add(1) == 1 {
				return validClaim(), nil
			}
			cancel(worker.ErrRuntimeShutdown)
			return nil, context.Canceled
		},
		execute: func(context.Context, contracts.ExecutionClaim) (contracts.ExecuteResult, error) {
			return contracts.ExecuteResultStale{}, nil
		},
	}
	if err := newWorker(t, port).Run(ctx); err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if port.executeCalls.Load() != 1 {
		t.Fatalf("Execute calls = %d, want 1", port.executeCalls.Load())
	}
}

func TestWorkerRejectsInvalidClaim(t *testing.T) {
	t.Parallel()
	claims := []contracts.ExecutionClaim{
		{},
		{TaskID: "task", RunID: "run", ExecutionVersion: 1, WorkerID: "another-worker"},
	}
	for index, claim := range claims {
		claim := claim
		t.Run(fmt.Sprintf("claim-%d", index), func(t *testing.T) {
			t.Parallel()
			port := &scriptedRuntimePort{claim: func(context.Context, contracts.WorkerID) (contracts.ClaimResult, error) {
				return contracts.ClaimResultClaimed{Claim: claim}, nil
			}}
			if err := newWorker(t, port).Run(context.Background()); !errors.Is(err, worker.ErrPortContractViolation) {
				t.Fatalf("Run() error = %v, want port contract violation", err)
			}
			if port.executeCalls.Load() != 0 {
				t.Fatalf("Execute calls = %d, want 0", port.executeCalls.Load())
			}
		})
	}
}

func TestWorkerHandledClaimBranchesContinueWithoutExecute(t *testing.T) {
	t.Parallel()
	results := []contracts.ClaimResult{
		contracts.ClaimResultConfigMismatchInterrupted{},
		contracts.ClaimResultCheckpointInvalidTerminalized{},
		contracts.ClaimResultDataInconsistentTerminalized{},
		contracts.ClaimResultExpiredTerminalized{},
	}
	ctx, cancel := context.WithCancelCause(context.Background())
	var calls atomic.Int64
	port := &scriptedRuntimePort{claim: func(context.Context, contracts.WorkerID) (contracts.ClaimResult, error) {
		index := int(calls.Add(1)) - 1
		if index < len(results) {
			return results[index], nil
		}
		cancel(worker.ErrRuntimeShutdown)
		return nil, context.Canceled
	}}
	if err := newWorker(t, port).Run(ctx); err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if calls.Load() != 5 || port.executeCalls.Load() != 0 {
		t.Fatalf("calls = claim:%d execute:%d, want 5/0", calls.Load(), port.executeCalls.Load())
	}
}

func TestWorkerPortFailuresAndContractViolationsStopRun(t *testing.T) {
	t.Parallel()
	systemErr := errors.New("persistence unavailable")
	unknownClaim := unknownClaimResult{}
	unknownExecute := unknownExecuteResult{}
	tests := []struct {
		name     string
		claim    contracts.ClaimResult
		claimErr error
		execute  contracts.ExecuteResult
		execErr  error
		want     error
	}{
		{name: "claim system error", claimErr: systemErr, want: systemErr},
		{name: "claim result and error", claim: contracts.ClaimResultNoWork{}, claimErr: systemErr, want: worker.ErrPortContractViolation},
		{name: "claim no result", want: worker.ErrPortContractViolation},
		{name: "claim unknown result", claim: unknownClaim, want: worker.ErrPortContractViolation},
		{name: "execute system error", claim: validClaim(), execErr: systemErr, want: systemErr},
		{name: "execute result and error", claim: validClaim(), execute: contracts.ExecuteResultTerminal{}, execErr: systemErr, want: worker.ErrPortContractViolation},
		{name: "execute no result", claim: validClaim(), want: worker.ErrPortContractViolation},
		{name: "execute unknown result", claim: validClaim(), execute: unknownExecute, want: worker.ErrPortContractViolation},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			port := &scriptedRuntimePort{
				claim: func(context.Context, contracts.WorkerID) (contracts.ClaimResult, error) {
					return test.claim, test.claimErr
				},
				execute: func(context.Context, contracts.ExecutionClaim) (contracts.ExecuteResult, error) {
					return test.execute, test.execErr
				},
			}
			err := newWorker(t, port).Run(context.Background())
			if !errors.Is(err, test.want) {
				t.Fatalf("Run() error = %v, want %v", err, test.want)
			}
		})
	}
}

func TestWorkerCancellationPropagation(t *testing.T) {
	t.Parallel()
	for _, cause := range []error{worker.ErrRuntimeShutdown, worker.ErrLockLost} {
		cause := cause
		t.Run(cause.Error(), func(t *testing.T) {
			t.Parallel()
			ctx, cancel := context.WithCancelCause(context.Background())
			port := &scriptedRuntimePort{claim: func(ctx context.Context, _ contracts.WorkerID) (contracts.ClaimResult, error) {
				cancel(cause)
				<-ctx.Done()
				if !errors.Is(context.Cause(ctx), cause) {
					t.Errorf("Port context cause = %v, want %v", context.Cause(ctx), cause)
				}
				return nil, ctx.Err()
			}}
			if err := newWorker(t, port).Run(ctx); err != nil {
				t.Fatalf("Run() error = %v, want normal stop", err)
			}
		})
	}
}

func TestWorkerConcurrentRunAllowsOneLoop(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	entered := make(chan struct{})
	port := &scriptedRuntimePort{claim: func(ctx context.Context, _ contracts.WorkerID) (contracts.ClaimResult, error) {
		select {
		case <-entered:
		default:
			close(entered)
		}
		<-ctx.Done()
		return nil, ctx.Err()
	}}
	instance := newWorker(t, port)
	firstDone := make(chan error, 1)
	go func() { firstDone <- instance.Run(ctx) }()
	<-entered
	if err := instance.Run(ctx); !errors.Is(err, worker.ErrAlreadyStarted) {
		t.Fatalf("concurrent Run() error = %v, want AlreadyStarted", err)
	}
	cancel()
	if err := <-firstDone; !errors.Is(err, context.Canceled) {
		t.Fatalf("first Run() error = %v, want context cancellation for untyped stop", err)
	}
}

type scriptedRuntimePort struct {
	claim        func(context.Context, contracts.WorkerID) (contracts.ClaimResult, error)
	execute      func(context.Context, contracts.ExecutionClaim) (contracts.ExecuteResult, error)
	executeCalls atomic.Int64
	mu           sync.Mutex
}

func (p *scriptedRuntimePort) ClaimNextExecution(
	ctx context.Context,
	workerID contracts.WorkerID,
) (contracts.ClaimResult, error) {
	if p.claim == nil {
		return nil, nil
	}
	return p.claim(ctx, workerID)
}

func (p *scriptedRuntimePort) ExecuteClaimedExecution(
	ctx context.Context,
	claim contracts.ExecutionClaim,
) (contracts.ExecuteResult, error) {
	p.executeCalls.Add(1)
	if p.execute == nil {
		return nil, nil
	}
	return p.execute(ctx, claim)
}

func newWorker(t *testing.T, port worker.RuntimePort) *worker.Worker {
	t.Helper()
	instance, err := worker.New(port, "worker-1", time.Hour/1000)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	return instance
}

func validClaim() contracts.ClaimResult {
	return contracts.ClaimResultClaimed{Claim: contracts.ExecutionClaim{
		TaskID: "task-1", RunID: "run-1", ExecutionVersion: 1, WorkerID: "worker-1",
	}}
}

type unknownClaimResult struct{ contracts.ClaimResult }
type unknownExecuteResult struct{ contracts.ExecuteResult }

var _ worker.RuntimePort = (*scriptedRuntimePort)(nil)
