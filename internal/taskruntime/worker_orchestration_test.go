package taskruntime_test

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/zhaohaip/agentops-go/internal/contracts"
	"github.com/zhaohaip/agentops-go/internal/taskruntime"
	"github.com/zhaohaip/agentops-go/internal/worker"
)

func TestWorkerOrchestratesClaimWithRealExecuteTaskService(t *testing.T) {
	t.Parallel()
	harness := newExecuteHarness(t, contracts.CheckpointNextActionRequestApproval)
	harness.steps.call = harness.waitingApprovalCall()
	ctx, cancel := context.WithCancelCause(context.Background())
	port := &workerRuntimeHarness{execute: harness.service, claim: harness.claim, cancel: cancel}
	instance, err := worker.New(port, harness.claim.WorkerID, time.Millisecond)
	if err != nil {
		t.Fatalf("worker.New() error = %v", err)
	}
	if err := instance.Run(ctx); err != nil {
		t.Fatalf("Worker.Run() error = %v", err)
	}
	if port.claims.Load() != 2 || port.executes.Load() != 1 || harness.steps.called.Load() != 1 {
		t.Fatalf("calls = claim:%d execute:%d step:%d, want 2/1/1",
			port.claims.Load(), port.executes.Load(), harness.steps.called.Load())
	}
}

type workerRuntimeHarness struct {
	execute  *taskruntime.ExecuteTaskService
	claim    contracts.ExecutionClaim
	cancel   context.CancelCauseFunc
	claims   atomic.Int64
	executes atomic.Int64
}

func (p *workerRuntimeHarness) ClaimNextExecution(
	context.Context,
	contracts.WorkerID,
) (contracts.ClaimResult, error) {
	if p.claims.Add(1) == 1 {
		return contracts.ClaimResultClaimed{Claim: p.claim}, nil
	}
	p.cancel(worker.ErrRuntimeShutdown)
	return nil, context.Canceled
}

func (p *workerRuntimeHarness) ExecuteClaimedExecution(
	ctx context.Context,
	claim contracts.ExecutionClaim,
) (contracts.ExecuteResult, error) {
	p.executes.Add(1)
	return p.execute.ExecuteClaimedExecution(ctx, claim)
}

var _ worker.RuntimePort = (*workerRuntimeHarness)(nil)
