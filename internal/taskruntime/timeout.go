package taskruntime

import (
	"context"
	"errors"
	"fmt"

	"github.com/zhaohaip/agentops-go/internal/contracts"
	"github.com/zhaohaip/agentops-go/internal/lifecycle"
	"github.com/zhaohaip/agentops-go/internal/taskruntime/activecall"
)

// ExpireTaskRequest 携带 Scanner 观察到的执行版本。
type ExpireTaskRequest struct {
	TaskID                   contracts.TaskID
	ObservedExecutionVersion contracts.ExecutionVersion
}

// ExpireTaskResult 是 ExpireTask 的封闭业务结果。
type ExpireTaskResult interface{ isExpireTaskResult() }

// ExpireTaskExpired 表示当前候选已提交 Timeout 终态。
type ExpireTaskExpired struct{}

// ExpireTaskSkipped 表示 deadline 未到或观察版本已经过期。
type ExpireTaskSkipped struct{}

// ExpireTaskAlreadyTerminal 表示目标 Task 已经处于业务终态。
type ExpireTaskAlreadyTerminal struct{}

func (ExpireTaskExpired) isExpireTaskResult()         {}
func (ExpireTaskSkipped) isExpireTaskResult()         {}
func (ExpireTaskAlreadyTerminal) isExpireTaskResult() {}

// ExpireTaskService 编排一个 Timeout 候选的独立短事务。
type ExpireTaskService struct{ cancel *CancelTaskService }

// ExpireTaskDependencies 声明 ExpireTask 的最小出站依赖。
type ExpireTaskDependencies struct {
	Executor     contracts.RuntimeWriteExecutor
	Terminations TerminationRepository
	Reports      contracts.PendingReportWriter
	Clock        DatabaseClock
	Configs      AgentConfigSource
	TaskLogs     TaskLogRepository
	ActiveCalls  *activecall.Registry
	Policy       lifecycle.Policy
}

// NewExpireTaskService 创建未接入生产组合根的 ExpireTask 服务。
func NewExpireTaskService(dependencies ExpireTaskDependencies) (*ExpireTaskService, error) {
	if dependencies.Executor == nil || dependencies.Terminations == nil || dependencies.Reports == nil ||
		dependencies.Clock == nil || dependencies.Configs == nil || dependencies.TaskLogs == nil || dependencies.ActiveCalls == nil {
		return nil, errors.New("create ExpireTask service: dependencies are required")
	}
	return &ExpireTaskService{cancel: &CancelTaskService{executor: dependencies.Executor,
		terminations: dependencies.Terminations, reports: dependencies.Reports, clock: dependencies.Clock,
		configs: dependencies.Configs, taskLogs: dependencies.TaskLogs, activeCalls: dependencies.ActiveCalls,
		policy: dependencies.Policy}}, nil
}

// ExpireTask 只在观察版本仍为当前版本且数据库 deadline 已到时提交 Timeout 终态。
func (s *ExpireTaskService) ExpireTask(ctx context.Context, request ExpireTaskRequest) (ExpireTaskResult, error) {
	if s == nil || s.cancel == nil {
		return nil, errors.New("expire Task: service is not initialized")
	}
	if request.TaskID == "" || !request.ObservedExecutionVersion.Valid() {
		return nil, ErrInvalidArgument
	}
	var result ExpireTaskResult
	var cancelKey *activecall.Key
	var terminalLog taskLogDraft
	err := s.cancel.executor.Execute(ctx, func(ctx context.Context, tx contracts.RuntimeWriteTx) error {
		facts, err := s.cancel.terminations.LockTerminationFacts(ctx, tx, request.TaskID)
		if err != nil {
			return fmt.Errorf("lock Timeout termination facts: %w", err)
		}
		if facts.Task.Status.Terminal() {
			result = ExpireTaskAlreadyTerminal{}
			return nil
		}
		if facts.Task.CurrentExecutionVersion != request.ObservedExecutionVersion ||
			facts.Execution.ExecutionVersion != request.ObservedExecutionVersion {
			result = ExpireTaskSkipped{}
			return nil
		}
		now, err := s.cancel.clock.Now(ctx, tx)
		if err != nil {
			return fmt.Errorf("read Timeout database clock: %w", err)
		}
		if now.Before(facts.Task.DeadlineAt) {
			result = ExpireTaskSkipped{}
			return nil
		}
		update, err := s.cancel.terminationRequest(facts, now, contracts.TaskStatusFailed,
			contracts.ErrorCodeTaskTimeout, contracts.TerminationReasonTimedOut)
		if err != nil {
			return err
		}
		applied, err := s.cancel.terminations.ApplyTermination(ctx, tx, update)
		if err != nil {
			return fmt.Errorf("apply Timeout termination: %w", err)
		}
		if !applied {
			return fmt.Errorf("Timeout termination condition missed: %w", ErrPersistenceInvariantViolation)
		}
		if err := ensureTerminationReport(ctx, tx, s.cancel.reports, facts, now); err != nil {
			return err
		}
		if facts.Execution.WorkerID != nil {
			key := activecall.Key{TaskID: facts.Task.TaskID, ExecutionVersion: facts.Execution.ExecutionVersion, WorkerID: *facts.Execution.WorkerID}
			cancelKey = &key
		}
		reason := contracts.TerminationReasonTimedOut
		terminalLog = terminalTaskLogDraft(QueueCandidate{TaskID: facts.Task.TaskID, RunID: facts.Run.RunID,
			ExecutionVersion: facts.Execution.ExecutionVersion}, contracts.ErrorCodeTaskTimeout, &reason)
		result = ExpireTaskExpired{}
		return nil
	})
	if err != nil {
		return nil, err
	}
	if _, expired := result.(ExpireTaskExpired); expired {
		if cancelKey != nil {
			_, _ = s.cancel.activeCalls.Cancel(*cancelKey, activecall.CauseTaskTimedOut)
		}
		appendTaskLogBestEffort(ctx, s.cancel.executor, s.cancel.taskLogs, s.cancel.clock, terminalLog)
	}
	if result == nil {
		return nil, fmt.Errorf("ExpireTask result is missing: %w", ErrPersistenceInvariantViolation)
	}
	return result, nil
}
