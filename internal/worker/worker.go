// Package worker 提供单执行槽 Task Worker Run Loop。
package worker

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"
	"time"

	"github.com/zhaohaip/agentops-go/internal/contracts"
)

const maxPollInterval = 5 * time.Second

var (
	// ErrAlreadyStarted 表示同一个 Worker 实例已经启动过。
	ErrAlreadyStarted = errors.New("WorkerAlreadyStarted")
	// ErrPortContractViolation 表示 Task Runtime Worker Port 返回了非法组合或未知结果。
	ErrPortContractViolation = errors.New("WorkerPortContractViolation")
	// ErrRuntimeShutdown 是 Runtime Host 正常关闭 process context 时使用的原因。
	ErrRuntimeShutdown = errors.New("RUNTIME_SHUTDOWN")
	// ErrLockLost 是 Runtime Host 因 advisory lock 失效关闭 process context 时使用的原因。
	ErrLockLost = errors.New("LOCK_LOST")
)

// RuntimePort 是 Worker 调用 Task Runtime 的最小入站用例集合。
type RuntimePort interface {
	ClaimNextExecution(context.Context, contracts.WorkerID) (contracts.ClaimResult, error)
	ExecuteClaimedExecution(context.Context, contracts.ExecutionClaim) (contracts.ExecuteResult, error)
}

// Worker 串行执行 Claim、Execute 和可取消的空队列等待。
type Worker struct {
	runtime      RuntimePort
	workerID     contracts.WorkerID
	pollInterval time.Duration
	started      atomic.Bool
}

// New 创建一个尚未启动的单执行槽 Worker。
func New(runtime RuntimePort, workerID contracts.WorkerID, pollInterval time.Duration) (*Worker, error) {
	if runtime == nil {
		return nil, errors.New("create Worker: Runtime Port is required")
	}
	if workerID == "" {
		return nil, errors.New("create Worker: worker ID is required")
	}
	if pollInterval <= 0 || pollInterval > maxPollInterval {
		return nil, errors.New("create Worker: poll interval must be within (0, 5s]")
	}
	return &Worker{runtime: runtime, workerID: workerID, pollInterval: pollInterval}, nil
}

// Run 阻塞运行唯一 Poll 循环，直到 process context 取消或 Port 返回系统错误。
func (w *Worker) Run(ctx context.Context) error {
	if w == nil {
		return errors.New("run Worker: Worker is not initialized")
	}
	if ctx == nil {
		return errors.New("run Worker: context is required")
	}
	if !w.started.CompareAndSwap(false, true) {
		return ErrAlreadyStarted
	}
	if ctx.Err() != nil {
		return nil
	}

	for {
		claimResult, err := w.runtime.ClaimNextExecution(ctx, w.workerID)
		if err != nil {
			return w.portError(ctx, "claim next execution", claimResult != nil, err)
		}
		if claimResult == nil {
			return fmt.Errorf("claim next execution returned no result: %w", ErrPortContractViolation)
		}

		switch result := claimResult.(type) {
		case contracts.ClaimResultClaimed:
			if err := w.execute(ctx, result.Claim); err != nil {
				return err
			}
		case contracts.ClaimResultNoWork:
			if !w.wait(ctx) {
				return nil
			}
		case contracts.ClaimResultConfigMismatchInterrupted,
			contracts.ClaimResultCheckpointInvalidTerminalized,
			contracts.ClaimResultDataInconsistentTerminalized,
			contracts.ClaimResultExpiredTerminalized:
		default:
			return fmt.Errorf("claim next execution returned unknown result %T: %w", claimResult, ErrPortContractViolation)
		}
		if ctx.Err() != nil {
			return nil
		}
	}
}

func (w *Worker) execute(ctx context.Context, claim contracts.ExecutionClaim) error {
	if claim.TaskID == "" || claim.RunID == "" || !claim.ExecutionVersion.Valid() ||
		claim.WorkerID == "" || claim.WorkerID != w.workerID {
		return fmt.Errorf("execute invalid ExecutionClaim: %w", ErrPortContractViolation)
	}
	result, err := w.runtime.ExecuteClaimedExecution(ctx, claim)
	if err != nil {
		return w.portError(ctx, "execute claimed execution", result != nil, err)
	}
	if result == nil {
		return fmt.Errorf("execute claimed execution returned no result: %w", ErrPortContractViolation)
	}
	switch result.(type) {
	case contracts.ExecuteResultWaitingApproval, contracts.ExecuteResultTerminal, contracts.ExecuteResultStale:
		return nil
	default:
		return fmt.Errorf("execute claimed execution returned unknown result %T: %w", result, ErrPortContractViolation)
	}
}

func (w *Worker) wait(ctx context.Context) bool {
	timer := time.NewTimer(w.pollInterval)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

func (w *Worker) portError(ctx context.Context, operation string, hasResult bool, err error) error {
	if hasResult {
		return errors.Join(
			fmt.Errorf("%s returned both result and error: %w", operation, ErrPortContractViolation),
			err,
		)
	}
	if isNormalStop(ctx, err) {
		return nil
	}
	if ctx.Err() == nil && (errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)) {
		return errors.Join(
			fmt.Errorf("%s returned cancellation while process context is active: %w", operation, ErrPortContractViolation),
			err,
		)
	}
	return err
}

func isNormalStop(ctx context.Context, err error) bool {
	if ctx.Err() == nil || (!errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded)) {
		return false
	}
	cause := context.Cause(ctx)
	return errors.Is(cause, ErrRuntimeShutdown) || errors.Is(cause, ErrLockLost)
}
