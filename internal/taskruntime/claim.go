package taskruntime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/zhaohaip/agentops-go/internal/contracts"
	"github.com/zhaohaip/agentops-go/internal/lifecycle"
)

var errClaimConditionMiss = errors.New("claim condition no longer matches")

// ClaimTaskService 在一个 Runtime 写事务内完成 FIFO 领取和配置门禁。
type ClaimTaskService struct {
	executor      contracts.RuntimeWriteExecutor
	tasks         TaskRepository
	runs          RunRepository
	executions    TaskExecutionRepository
	taskLogs      TaskLogRepository
	clock         DatabaseClock
	configs       AgentConfigSource
	checkpoints   RuntimeCheckpointPort
	reports       contracts.PendingReportWriter
	policy        lifecycle.Policy
	runtimeWorker contracts.WorkerID
}

// ClaimTaskDependencies 声明 ClaimNextExecution 的最小出站依赖。
type ClaimTaskDependencies struct {
	Executor      contracts.RuntimeWriteExecutor
	Tasks         TaskRepository
	Runs          RunRepository
	Executions    TaskExecutionRepository
	TaskLogs      TaskLogRepository
	Clock         DatabaseClock
	Configs       AgentConfigSource
	Checkpoints   RuntimeCheckpointPort
	Reports       contracts.PendingReportWriter
	Policy        lifecycle.Policy
	RuntimeWorker contracts.WorkerID
}

// NewClaimTaskService 创建未接入生产组合根的 ClaimNextExecution 应用服务。
func NewClaimTaskService(dependencies ClaimTaskDependencies) (*ClaimTaskService, error) {
	if dependencies.Executor == nil || dependencies.Tasks == nil || dependencies.Runs == nil ||
		dependencies.Executions == nil || dependencies.Clock == nil || dependencies.Configs == nil ||
		dependencies.TaskLogs == nil || dependencies.Checkpoints == nil || dependencies.Reports == nil ||
		dependencies.RuntimeWorker == "" {
		return nil, errors.New("create ClaimNextExecution service: dependencies and runtime worker are required")
	}
	return &ClaimTaskService{
		executor: dependencies.Executor, tasks: dependencies.Tasks, runs: dependencies.Runs,
		executions: dependencies.Executions, clock: dependencies.Clock, configs: dependencies.Configs,
		taskLogs:    dependencies.TaskLogs,
		checkpoints: dependencies.Checkpoints, reports: dependencies.Reports,
		policy: dependencies.Policy, runtimeWorker: dependencies.RuntimeWorker,
	}, nil
}

// ClaimNextExecution 返回封闭业务结果；基础设施与持久化不变量故障使用 error 通道。
func (s *ClaimTaskService) ClaimNextExecution(
	ctx context.Context,
	workerID contracts.WorkerID,
) (contracts.ClaimResult, error) {
	if s == nil {
		return nil, errors.New("claim next execution: service is not initialized")
	}
	if workerID == "" || workerID != s.runtimeWorker {
		return nil, ErrInvalidArgument
	}

	var result contracts.ClaimResult
	var missedCandidate QueueCandidate
	err := s.executor.Execute(ctx, func(ctx context.Context, tx contracts.RuntimeWriteTx) error {
		candidate, err := s.tasks.LockNextQueueCandidate(ctx, tx)
		if errors.Is(err, ErrRepositoryNotFound) {
			result = contracts.ClaimResultNoWork{}
			return nil
		}
		if err != nil {
			return fmt.Errorf("lock FIFO queue candidate: %w", err)
		}
		missedCandidate = candidate

		facts, err := s.lockClaimFacts(ctx, tx, candidate)
		if err != nil {
			return err
		}
		now, err := s.clock.Now(ctx, tx)
		if err != nil {
			return fmt.Errorf("read Claim database clock: %w", err)
		}

		if invariant := validateClaimFacts(candidate, facts); invariant != "" {
			if err := s.terminalizeClaim(ctx, tx, facts, now, contracts.ErrorCodeDataInconsistent, &invariant, nil); err != nil {
				return err
			}
			result = contracts.ClaimResultDataInconsistentTerminalized{}
			return nil
		}
		if !now.Before(facts.task.DeadlineAt) {
			reason := contracts.TerminationReasonTimedOut
			if err := s.terminalizeClaim(ctx, tx, facts, now, contracts.ErrorCodeTaskTimeout, nil, &reason); err != nil {
				return err
			}
			result = contracts.ClaimResultExpiredTerminalized{}
			return nil
		}

		source, invariant := classifyClaimSource(facts)
		if invariant != "" {
			if err := s.terminalizeClaim(ctx, tx, facts, now, contracts.ErrorCodeDataInconsistent, &invariant, nil); err != nil {
				return err
			}
			result = contracts.ClaimResultDataInconsistentTerminalized{}
			return nil
		}
		checkpointResult, err := s.checkpoints.LoadLatestForClaim(
			ctx, tx, facts.task.TaskID, facts.run.RunID, facts.execution.ExecutionVersion, source,
		)
		if err != nil {
			return fmt.Errorf("load Claim Checkpoint: %w", err)
		}
		checkpoint, checkpointInvalid, err := validatedClaimCheckpoint(checkpointResult, facts)
		if err != nil {
			return err
		}
		if checkpointInvalid {
			if err := s.terminalizeClaim(ctx, tx, facts, now, contracts.ErrorCodeCheckpointInvalid, nil, nil); err != nil {
				return err
			}
			result = contracts.ClaimResultCheckpointInvalidTerminalized{}
			return nil
		}

		agent, exists := s.configs.LookupAgent(facts.task.AgentID)
		if !exists || !agent.ExecutionConfig.Agent.Enabled || agent.ExecutionConfig.Agent.AgentID != facts.task.AgentID {
			return errors.New("claim next execution: validated runtime Agent configuration is unavailable")
		}
		currentHash, err := HashExecutionConfigV1(agent.ExecutionConfig)
		if err != nil {
			return fmt.Errorf("hash Claim execution config: %w", err)
		}
		if !facts.execution.ExecutionConfigHash.Valid() {
			return fmt.Errorf("claim TaskExecution hash is invalid: %w", ErrPersistenceInvariantViolation)
		}
		if facts.execution.ExecutionConfigHash != checkpoint.ExecutionConfigHash ||
			facts.execution.ExecutionConfigHash != currentHash {
			if err := s.interruptConfigMismatch(ctx, tx, facts, now, currentHash); err != nil {
				return err
			}
			result = contracts.ClaimResultConfigMismatchInterrupted{}
			return nil
		}

		if err := s.commitClaim(ctx, tx, facts, now, workerID); err != nil {
			return err
		}
		if facts.run.PlanID == nil {
			if err := s.checkpoints.SaveRuntimeCheckpoint(ctx, tx, SaveRuntimeCheckpointRequest{
				TaskID: facts.task.TaskID, RunID: facts.run.RunID,
				ExecutionVersion:    facts.execution.ExecutionVersion,
				ExecutionConfigHash: facts.execution.ExecutionConfigHash,
				NextAction:          contracts.CheckpointNextActionGeneratePlan, CreatedAt: now,
			}); err != nil {
				return fmt.Errorf("save Claim GENERATE_PLAN Checkpoint: %w", err)
			}
		}
		result = contracts.ClaimResultClaimed{Claim: contracts.ExecutionClaim{
			TaskID: facts.task.TaskID, RunID: facts.run.RunID,
			ExecutionVersion: facts.execution.ExecutionVersion, WorkerID: workerID, ClaimedAt: now,
		}}
		return nil
	})
	if errors.Is(err, errClaimConditionMiss) {
		reconciled, reconcileErr := s.reconcileClaimConditionMiss(ctx, missedCandidate)
		if reconcileErr != nil {
			return nil, reconcileErr
		}
		s.appendClaimTaskLog(ctx, reconciled, missedCandidate)
		return reconciled, nil
	}
	if err != nil {
		return nil, err
	}
	if result == nil {
		return nil, fmt.Errorf("claim result is missing: %w", ErrPersistenceInvariantViolation)
	}
	s.appendClaimTaskLog(ctx, result, missedCandidate)
	return result, nil
}

func (s *ClaimTaskService) appendClaimTaskLog(
	ctx context.Context,
	result contracts.ClaimResult,
	candidate QueueCandidate,
) {
	draft, ok := claimTaskLogDraft(result, candidate)
	if !ok {
		return
	}
	appendTaskLogBestEffort(ctx, s.executor, s.taskLogs, s.clock, draft)
}

// reconcileClaimConditionMiss 在原事务回滚后，以新短事务判断条件未命中的真实归因。
func (s *ClaimTaskService) reconcileClaimConditionMiss(
	ctx context.Context,
	candidate QueueCandidate,
) (contracts.ClaimResult, error) {
	if candidate.TaskID == "" || !candidate.ExecutionVersion.Valid() {
		return nil, fmt.Errorf("reconcile Claim condition miss without candidate: %w", ErrPersistenceInvariantViolation)
	}

	var result contracts.ClaimResult
	err := s.executor.Execute(ctx, func(ctx context.Context, tx contracts.RuntimeWriteTx) error {
		task, err := s.tasks.Lock(ctx, tx, candidate.TaskID)
		if err != nil {
			return fmt.Errorf("reconcile Claim Task: %w", persistenceTargetError(err))
		}
		if task.CurrentRunID != candidate.RunID {
			return fmt.Errorf("Claim candidate Run changed during condition reconciliation: %w", ErrPersistenceInvariantViolation)
		}
		if task.CurrentExecutionVersion != candidate.ExecutionVersion {
			removed, err := s.confirmSupersededClaimCandidate(ctx, tx, task, candidate)
			if err != nil {
				return err
			}
			if !removed {
				return fmt.Errorf("Claim candidate version changed without a valid replacement: %w", ErrPersistenceInvariantViolation)
			}
			result = contracts.ClaimResultNoWork{}
			return nil
		}
		run, err := s.runs.LockByTask(ctx, tx, task.TaskID)
		if err != nil {
			return fmt.Errorf("reconcile Claim Run: %w", persistenceTargetError(err))
		}
		execution, err := s.executions.LockByTaskVersion(ctx, tx, task.TaskID, task.CurrentExecutionVersion)
		if err != nil {
			return fmt.Errorf("reconcile Claim TaskExecution: %w", persistenceTargetError(err))
		}
		facts := claimFacts{task: task, run: run, execution: execution}
		if task.QueuedAt == nil && claimCandidateLegallyRemoved(facts) {
			result = contracts.ClaimResultNoWork{}
			return nil
		}

		now, err := s.clock.Now(ctx, tx)
		if err != nil {
			return fmt.Errorf("read reconciled Claim database clock: %w", err)
		}
		currentCandidate := QueueCandidate{
			TaskID: task.TaskID, RunID: task.CurrentRunID, ExecutionVersion: task.CurrentExecutionVersion,
			TaskStatus: task.Status, ExecutionStatus: execution.Status, CreatedAt: task.CreatedAt,
		}
		if task.QueuedAt != nil {
			currentCandidate.QueuedAt = *task.QueuedAt
		}
		if invariant := validateClaimFacts(currentCandidate, facts); invariant != "" {
			if err := s.terminalizeClaim(ctx, tx, facts, now, contracts.ErrorCodeDataInconsistent, &invariant, nil); err != nil {
				return reconcileClaimWriteError(err)
			}
			result = contracts.ClaimResultDataInconsistentTerminalized{}
			return nil
		}
		if !now.Before(task.DeadlineAt) {
			reason := contracts.TerminationReasonTimedOut
			if err := s.terminalizeClaim(ctx, tx, facts, now, contracts.ErrorCodeTaskTimeout, nil, &reason); err != nil {
				return reconcileClaimWriteError(err)
			}
			result = contracts.ClaimResultExpiredTerminalized{}
			return nil
		}

		source, invariant := classifyClaimSource(facts)
		if invariant != "" {
			if err := s.terminalizeClaim(ctx, tx, facts, now, contracts.ErrorCodeDataInconsistent, &invariant, nil); err != nil {
				return reconcileClaimWriteError(err)
			}
			result = contracts.ClaimResultDataInconsistentTerminalized{}
			return nil
		}
		checkpointResult, err := s.checkpoints.LoadLatestForClaim(
			ctx, tx, task.TaskID, run.RunID, execution.ExecutionVersion, source,
		)
		if err != nil {
			return fmt.Errorf("load reconciled Claim Checkpoint: %w", err)
		}
		checkpoint, checkpointInvalid, err := validatedClaimCheckpoint(checkpointResult, facts)
		if err != nil {
			return err
		}
		if checkpointInvalid {
			if err := s.terminalizeClaim(ctx, tx, facts, now, contracts.ErrorCodeCheckpointInvalid, nil, nil); err != nil {
				return reconcileClaimWriteError(err)
			}
			result = contracts.ClaimResultCheckpointInvalidTerminalized{}
			return nil
		}

		agent, exists := s.configs.LookupAgent(task.AgentID)
		if !exists || !agent.ExecutionConfig.Agent.Enabled || agent.ExecutionConfig.Agent.AgentID != task.AgentID {
			return errors.New("reconcile Claim condition miss: validated runtime Agent configuration is unavailable")
		}
		currentHash, err := HashExecutionConfigV1(agent.ExecutionConfig)
		if err != nil {
			return fmt.Errorf("hash reconciled Claim execution config: %w", err)
		}
		if !execution.ExecutionConfigHash.Valid() {
			return fmt.Errorf("reconciled Claim TaskExecution hash is invalid: %w", ErrPersistenceInvariantViolation)
		}
		if execution.ExecutionConfigHash != checkpoint.ExecutionConfigHash || execution.ExecutionConfigHash != currentHash {
			if err := s.interruptConfigMismatch(ctx, tx, facts, now, currentHash); err != nil {
				return reconcileClaimWriteError(err)
			}
			result = contracts.ClaimResultConfigMismatchInterrupted{}
			return nil
		}
		return fmt.Errorf("Claim condition missed while candidate remains valid and queued: %w", ErrPersistenceInvariantViolation)
	})
	if err != nil {
		return nil, err
	}
	if result == nil {
		return nil, fmt.Errorf("reconciled Claim result is missing: %w", ErrPersistenceInvariantViolation)
	}
	return result, nil
}

func (s *ClaimTaskService) confirmSupersededClaimCandidate(
	ctx context.Context,
	tx contracts.RuntimeWriteTx,
	task Task,
	candidate QueueCandidate,
) (bool, error) {
	if task.CurrentExecutionVersion <= candidate.ExecutionVersion || task.QueuedAt == nil {
		return false, nil
	}
	run, err := s.runs.LockByTask(ctx, tx, task.TaskID)
	if err != nil {
		return false, fmt.Errorf("reconcile superseded Claim Run: %w", persistenceTargetError(err))
	}
	previous, err := s.executions.LockByTaskVersion(ctx, tx, task.TaskID, candidate.ExecutionVersion)
	if err != nil {
		return false, fmt.Errorf("reconcile superseded TaskExecution: %w", persistenceTargetError(err))
	}
	current, err := s.executions.LockByTaskVersion(ctx, tx, task.TaskID, task.CurrentExecutionVersion)
	if err != nil {
		return false, fmt.Errorf("reconcile replacement TaskExecution: %w", persistenceTargetError(err))
	}
	if previous.Status != contracts.TaskExecutionStatusInterrupted || previous.WorkerID != nil ||
		current.Status != contracts.TaskExecutionStatusQueued || current.WorkerID != nil {
		return false, nil
	}
	switch task.Status {
	case contracts.TaskStatusPending:
		return run.Status == contracts.RunStatusPending, nil
	case contracts.TaskStatusRunning:
		return run.Status == contracts.RunStatusRunning, nil
	default:
		return false, nil
	}
}

func claimCandidateLegallyRemoved(facts claimFacts) bool {
	switch facts.execution.Status {
	case contracts.TaskExecutionStatusRunning:
		return facts.execution.WorkerID != nil && facts.task.Status == contracts.TaskStatusRunning &&
			facts.run.Status == contracts.RunStatusRunning
	case contracts.TaskExecutionStatusWaitingApproval:
		return facts.execution.WorkerID == nil && facts.task.Status == contracts.TaskStatusWaitingApproval &&
			facts.run.Status == contracts.RunStatusWaitingApproval
	case contracts.TaskExecutionStatusInterrupted:
		return facts.execution.WorkerID == nil &&
			(facts.task.Status == contracts.TaskStatusInterrupted || facts.task.Status == contracts.TaskStatusRunning) &&
			(facts.run.Status == contracts.RunStatusPending || facts.run.Status == contracts.RunStatusRunning)
	case contracts.TaskExecutionStatusCompleted:
		return facts.task.Status == contracts.TaskStatusCompleted && facts.run.Status == contracts.RunStatusCompleted
	case contracts.TaskExecutionStatusFailed:
		return (facts.task.Status == contracts.TaskStatusFailed || facts.task.Status == contracts.TaskStatusCancelled) &&
			facts.run.Status == contracts.RunStatusFailed
	default:
		return false
	}
}

func reconcileClaimWriteError(err error) error {
	if errors.Is(err, errClaimConditionMiss) {
		return fmt.Errorf("reconciled Claim condition update missed: %w", ErrPersistenceInvariantViolation)
	}
	return err
}

type claimFacts struct {
	task      Task
	run       Run
	execution TaskExecution
}

func (s *ClaimTaskService) lockClaimFacts(
	ctx context.Context,
	tx contracts.RuntimeWriteTx,
	candidate QueueCandidate,
) (claimFacts, error) {
	task, err := s.tasks.Lock(ctx, tx, candidate.TaskID)
	if err != nil {
		return claimFacts{}, fmt.Errorf("lock Claim Task: %w", persistenceTargetError(err))
	}
	run, err := s.runs.LockByTask(ctx, tx, candidate.TaskID)
	if err != nil {
		return claimFacts{}, fmt.Errorf("lock Claim Run: %w", persistenceTargetError(err))
	}
	execution, err := s.executions.LockByTaskVersion(ctx, tx, candidate.TaskID, task.CurrentExecutionVersion)
	if err != nil {
		return claimFacts{}, fmt.Errorf("lock Claim TaskExecution: %w", persistenceTargetError(err))
	}
	return claimFacts{task: task, run: run, execution: execution}, nil
}

func persistenceTargetError(err error) error {
	if errors.Is(err, ErrRepositoryNotFound) {
		return ErrPersistenceInvariantViolation
	}
	return err
}

func validateClaimFacts(candidate QueueCandidate, facts claimFacts) contracts.InvariantCode {
	if facts.task.TaskID != candidate.TaskID || facts.task.CurrentRunID != candidate.RunID ||
		facts.run.TaskID != facts.task.TaskID || facts.run.RunID != facts.task.CurrentRunID ||
		facts.execution.TaskID != facts.task.TaskID ||
		facts.execution.ExecutionVersion != facts.task.CurrentExecutionVersion ||
		candidate.ExecutionVersion != facts.task.CurrentExecutionVersion {
		return contracts.InvariantCodeCurrentExecutionInvalid
	}
	if facts.task.QueuedAt == nil || candidate.QueuedAt.IsZero() ||
		facts.execution.Status != contracts.TaskExecutionStatusQueued || facts.execution.WorkerID != nil ||
		candidate.ExecutionStatus != contracts.TaskExecutionStatusQueued {
		return contracts.InvariantCodeQueueStateInvalid
	}
	if candidate.TaskStatus != facts.task.Status {
		return contracts.InvariantCodeQueueStateInvalid
	}
	switch facts.task.Status {
	case contracts.TaskStatusPending:
		if facts.run.Status != contracts.RunStatusPending {
			return contracts.InvariantCodeCrossObjectStateInvalid
		}
	case contracts.TaskStatusRunning:
		if facts.run.Status != contracts.RunStatusRunning {
			return contracts.InvariantCodeCrossObjectStateInvalid
		}
	default:
		return contracts.InvariantCodeQueueStateInvalid
	}
	return ""
}

func classifyClaimSource(facts claimFacts) (ClaimCheckpointSource, contracts.InvariantCode) {
	if facts.run.CurrentStepID != nil && facts.run.PlanID == nil {
		return "", contracts.InvariantCodeCrossObjectStateInvalid
	}
	if facts.execution.ExecutionVersion == 1 && facts.run.PlanID == nil && facts.run.CurrentStepID == nil {
		return ClaimCheckpointSourceInitial, ""
	}
	if facts.run.PlanID != nil && facts.run.CurrentStepID == nil {
		return "", contracts.InvariantCodeClaimSourceAmbiguous
	}
	return ClaimCheckpointSourceContinuation, ""
}

func validatedClaimCheckpoint(
	result ClaimCheckpointResult,
	facts claimFacts,
) (RuntimeCheckpoint, bool, error) {
	switch typed := result.(type) {
	case ClaimCheckpointValid:
		checkpoint := typed.Checkpoint
		if checkpoint.CheckpointID == "" || checkpoint.CheckpointSequence <= 0 ||
			checkpoint.TaskID != facts.task.TaskID || checkpoint.RunID != facts.run.RunID ||
			checkpoint.ExecutionVersion != facts.execution.ExecutionVersion ||
			!checkpoint.ExecutionConfigHash.Valid() || !checkpoint.NextAction.Valid() {
			return RuntimeCheckpoint{}, true, nil
		}
		return checkpoint, false, nil
	case ClaimCheckpointInvalid:
		if !typed.ReasonCode.ValidForCheckpointInvalid() {
			return RuntimeCheckpoint{}, false, fmt.Errorf("invalid Checkpoint reason: %w", ErrPersistenceInvariantViolation)
		}
		return RuntimeCheckpoint{}, true, nil
	default:
		return RuntimeCheckpoint{}, false, fmt.Errorf("unknown Claim Checkpoint result: %w", ErrPersistenceInvariantViolation)
	}
}

func (s *ClaimTaskService) commitClaim(
	ctx context.Context,
	tx contracts.RuntimeWriteTx,
	facts claimFacts,
	now time.Time,
	workerID contracts.WorkerID,
) error {
	if decision := s.policy.CanExecutionTransition(facts.execution.Status, contracts.TaskExecutionStatusRunning); !decision.Allowed {
		return fmt.Errorf("validate Claim Execution lifecycle: %s", decision.Reason)
	}
	startedAt := facts.execution.StartedAt
	if startedAt == nil {
		startedAt = &now
	}
	updated, err := s.executions.Update(ctx, tx, TaskExecutionUpdate{
		TaskID: facts.task.TaskID, ExecutionVersion: facts.execution.ExecutionVersion,
		ExpectedStatus: facts.execution.Status, ExpectedWorkerID: facts.execution.WorkerID,
		Status: contracts.TaskExecutionStatusRunning, WorkerID: &workerID,
		StartedAt: startedAt,
	})
	if err != nil {
		return fmt.Errorf("claim TaskExecution: %w", err)
	}
	if !updated {
		return errClaimConditionMiss
	}

	if facts.run.Status == contracts.RunStatusPending {
		if decision := s.policy.CanRunTransition(facts.run.Status, contracts.RunStatusRunning); !decision.Allowed {
			return fmt.Errorf("validate Claim Run lifecycle: %s", decision.Reason)
		}
		runStartedAt := facts.run.StartedAt
		if runStartedAt == nil {
			runStartedAt = &now
		}
		updated, err = s.runs.Update(ctx, tx, RunUpdate{
			TaskID: facts.task.TaskID, RunID: facts.run.RunID,
			ExecutionVersion: facts.execution.ExecutionVersion, ExpectedStatus: facts.run.Status,
			Status: contracts.RunStatusRunning, PlanID: facts.run.PlanID,
			CurrentStepID: facts.run.CurrentStepID, Context: facts.run.Context,
			ErrorCode: facts.run.ErrorCode, StartedAt: runStartedAt, EndedAt: facts.run.EndedAt,
		})
		if err != nil {
			return fmt.Errorf("claim Run: %w", err)
		}
		if !updated {
			return errClaimConditionMiss
		}
	}

	taskStatus := facts.task.Status
	if facts.task.Status == contracts.TaskStatusPending {
		if decision := s.policy.CanTaskTransition(facts.task.Status, contracts.TaskStatusRunning); !decision.Allowed {
			return fmt.Errorf("validate Claim Task lifecycle: %s", decision.Reason)
		}
		taskStatus = contracts.TaskStatusRunning
	}
	taskStartedAt := facts.task.StartedAt
	if facts.task.Status == contracts.TaskStatusPending && taskStartedAt == nil {
		taskStartedAt = &now
	}
	updated, err = s.tasks.Update(ctx, tx, TaskUpdate{
		TaskID: facts.task.TaskID, ExpectedStatus: facts.task.Status,
		ExpectedCurrentExecutionVersion: facts.task.CurrentExecutionVersion,
		Status:                          taskStatus, CurrentExecutionVersion: facts.task.CurrentExecutionVersion,
		ResultSummary: facts.task.ResultSummary, ErrorCode: facts.task.ErrorCode,
		StartedAt: taskStartedAt, EndedAt: facts.task.EndedAt,
	})
	if err != nil {
		return fmt.Errorf("claim Task: %w", err)
	}
	if !updated {
		return errClaimConditionMiss
	}
	return nil
}

func (s *ClaimTaskService) interruptConfigMismatch(
	ctx context.Context,
	tx contracts.RuntimeWriteTx,
	facts claimFacts,
	now time.Time,
	observedHash contracts.ExecutionConfigHash,
) error {
	errorCode := contracts.ErrorCodeConfigVersionMismatch
	if decision := s.policy.CanExecutionTransition(facts.execution.Status, contracts.TaskExecutionStatusInterrupted); !decision.Allowed {
		return fmt.Errorf("validate config mismatch Execution lifecycle: %s", decision.Reason)
	}
	updated, err := s.executions.Update(ctx, tx, TaskExecutionUpdate{
		TaskID: facts.task.TaskID, ExecutionVersion: facts.execution.ExecutionVersion,
		ExpectedStatus: facts.execution.Status, ExpectedWorkerID: facts.execution.WorkerID,
		Status: contracts.TaskExecutionStatusInterrupted, ObservedConfigHash: &observedHash,
		ErrorCode: &errorCode, StartedAt: facts.execution.StartedAt, EndedAt: &now,
	})
	if err != nil {
		return fmt.Errorf("interrupt config mismatch TaskExecution: %w", err)
	}
	if !updated {
		return errClaimConditionMiss
	}
	if decision := s.policy.CanTaskTransition(facts.task.Status, contracts.TaskStatusInterrupted); !decision.Allowed {
		return fmt.Errorf("validate config mismatch Task lifecycle: %s", decision.Reason)
	}
	updated, err = s.tasks.Update(ctx, tx, TaskUpdate{
		TaskID: facts.task.TaskID, ExpectedStatus: facts.task.Status,
		ExpectedCurrentExecutionVersion: facts.task.CurrentExecutionVersion,
		Status:                          contracts.TaskStatusInterrupted, CurrentExecutionVersion: facts.task.CurrentExecutionVersion,
		ResultSummary: facts.task.ResultSummary, ErrorCode: &errorCode,
		StartedAt: facts.task.StartedAt, EndedAt: facts.task.EndedAt,
	})
	if err != nil {
		return fmt.Errorf("interrupt config mismatch Task: %w", err)
	}
	if !updated {
		return errClaimConditionMiss
	}
	return s.ensurePendingReport(ctx, tx, facts, now)
}

func (s *ClaimTaskService) terminalizeClaim(
	ctx context.Context,
	tx contracts.RuntimeWriteTx,
	facts claimFacts,
	now time.Time,
	errorCode contracts.ErrorCode,
	invariantCode *contracts.InvariantCode,
	terminationReason *contracts.TerminationReason,
) error {
	if decision := s.policy.CanExecutionTransition(facts.execution.Status, contracts.TaskExecutionStatusFailed); !decision.Allowed {
		return fmt.Errorf("validate terminal Claim Execution lifecycle: %s", decision.Reason)
	}
	updated, err := s.executions.Update(ctx, tx, TaskExecutionUpdate{
		TaskID: facts.task.TaskID, ExecutionVersion: facts.execution.ExecutionVersion,
		ExpectedStatus: facts.execution.Status, ExpectedWorkerID: facts.execution.WorkerID,
		Status: contracts.TaskExecutionStatusFailed, ErrorCode: &errorCode,
		InvariantCode: invariantCode, TerminationReason: terminationReason,
		StartedAt: facts.execution.StartedAt, EndedAt: &now,
	})
	if err != nil {
		return fmt.Errorf("terminalize Claim TaskExecution: %w", err)
	}
	if !updated {
		return errClaimConditionMiss
	}
	if decision := s.policy.CanRunTransition(facts.run.Status, contracts.RunStatusFailed); !decision.Allowed {
		return fmt.Errorf("validate terminal Claim Run lifecycle: %s", decision.Reason)
	}
	updated, err = s.runs.Update(ctx, tx, RunUpdate{
		TaskID: facts.task.TaskID, RunID: facts.run.RunID,
		ExecutionVersion: facts.execution.ExecutionVersion, ExpectedStatus: facts.run.Status,
		Status: contracts.RunStatusFailed, PlanID: facts.run.PlanID,
		CurrentStepID: facts.run.CurrentStepID, Context: nonNilJSONObject(facts.run.Context),
		ErrorCode: &errorCode, StartedAt: facts.run.StartedAt, EndedAt: &now,
	})
	if err != nil {
		return fmt.Errorf("terminalize Claim Run: %w", err)
	}
	if !updated {
		return errClaimConditionMiss
	}
	if decision := s.policy.CanTaskTransition(facts.task.Status, contracts.TaskStatusFailed); !decision.Allowed {
		return fmt.Errorf("validate terminal Claim Task lifecycle: %s", decision.Reason)
	}
	updated, err = s.tasks.Update(ctx, tx, TaskUpdate{
		TaskID: facts.task.TaskID, ExpectedStatus: facts.task.Status,
		ExpectedCurrentExecutionVersion: facts.task.CurrentExecutionVersion,
		Status:                          contracts.TaskStatusFailed, CurrentExecutionVersion: facts.task.CurrentExecutionVersion,
		ResultSummary: facts.task.ResultSummary, ErrorCode: &errorCode,
		StartedAt: facts.task.StartedAt, EndedAt: &now,
	})
	if err != nil {
		return fmt.Errorf("terminalize Claim Task: %w", err)
	}
	if !updated {
		return errClaimConditionMiss
	}
	return s.ensurePendingReport(ctx, tx, facts, now)
}

func (s *ClaimTaskService) ensurePendingReport(
	ctx context.Context,
	tx contracts.RuntimeWriteTx,
	facts claimFacts,
	now time.Time,
) error {
	result, err := s.reports.EnsurePending(ctx, tx, contracts.EnsurePendingReportRequest{
		TaskID: facts.task.TaskID, RunID: facts.run.RunID, CreatedAt: now,
	})
	if err != nil {
		return fmt.Errorf("ensure pending Claim Report: %w", err)
	}
	switch result.(type) {
	case contracts.EnsurePendingReportCreated, contracts.EnsurePendingReportExisting:
		return nil
	default:
		return fmt.Errorf("unknown pending Report result: %w", ErrPersistenceInvariantViolation)
	}
}

func nonNilJSONObject(value json.RawMessage) json.RawMessage {
	if len(value) == 0 {
		return json.RawMessage(`{}`)
	}
	return value
}
