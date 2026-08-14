package taskruntime

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/zhaohaip/agentops-go/internal/contracts"
	"github.com/zhaohaip/agentops-go/internal/lifecycle"
	"github.com/zhaohaip/agentops-go/internal/taskruntime/activecall"
)

var (
	ErrRecoverStateConflict     = errors.New("RecoverStateConflict")
	ErrRecoverConfigMismatch    = errors.New(string(contracts.ErrorCodeConfigVersionMismatch))
	ErrRecoverCheckpointInvalid = errors.New(string(contracts.ErrorCodeCheckpointInvalid))
)

// RecoverTaskRequest 是幂等恢复命令。
type RecoverTaskRequest struct {
	CommandID  CommandID
	TaskID     contracts.TaskID
	OperatorID string
}

// TaskRecovered 是恢复命令已原子创建并排队的新执行版本。
type TaskRecovered struct {
	TaskID                 contracts.TaskID
	RunID                  contracts.RunID
	SourceExecutionVersion contracts.ExecutionVersion
	NewExecutionVersion    contracts.ExecutionVersion
	TaskStatus             contracts.TaskStatus
	RunStatus              contracts.RunStatus
	ExecutionStatus        contracts.TaskExecutionStatus
	QueuedAt               time.Time
	RecoveryCheckpointID   contracts.CheckpointID
}

// RecoverTaskService 编排 Receipt、恢复来源验证、版本切换和 Recovery Start。
type RecoverTaskService struct {
	executor    contracts.RuntimeWriteExecutor
	tasks       TaskRepository
	runs        RunRepository
	executions  TaskExecutionRepository
	recovery    RecoveryRepository
	receipts    CommandReceiptRepository
	reports     contracts.PendingReportWriter
	clock       DatabaseClock
	configs     AgentConfigSource
	checkpoints RecoveryCheckpointPort
	taskLogs    TaskLogRepository
	activeCalls *activecall.Registry
	policy      lifecycle.Policy
	newID       func(string) (string, error)
}

// RecoverTaskDependencies 声明 RecoverTask 的最小事务依赖。
type RecoverTaskDependencies struct {
	Executor    contracts.RuntimeWriteExecutor
	Tasks       TaskRepository
	Runs        RunRepository
	Executions  TaskExecutionRepository
	Recovery    RecoveryRepository
	Receipts    CommandReceiptRepository
	Reports     contracts.PendingReportWriter
	Clock       DatabaseClock
	Configs     AgentConfigSource
	Checkpoints RecoveryCheckpointPort
	TaskLogs    TaskLogRepository
	ActiveCalls *activecall.Registry
	Policy      lifecycle.Policy
}

// NewRecoverTaskService 创建 RecoverTask 服务。
func NewRecoverTaskService(dependencies RecoverTaskDependencies) (*RecoverTaskService, error) {
	if dependencies.Executor == nil || dependencies.Tasks == nil || dependencies.Runs == nil ||
		dependencies.Executions == nil || dependencies.Recovery == nil || dependencies.Receipts == nil ||
		dependencies.Reports == nil || dependencies.Clock == nil || dependencies.Configs == nil ||
		dependencies.Checkpoints == nil || dependencies.TaskLogs == nil || dependencies.ActiveCalls == nil {
		return nil, errors.New("create RecoverTask service: dependencies are required")
	}
	return &RecoverTaskService{
		executor: dependencies.Executor, tasks: dependencies.Tasks, runs: dependencies.Runs,
		executions: dependencies.Executions, recovery: dependencies.Recovery,
		receipts: dependencies.Receipts, reports: dependencies.Reports, clock: dependencies.Clock,
		configs: dependencies.Configs, checkpoints: dependencies.Checkpoints, taskLogs: dependencies.TaskLogs,
		activeCalls: dependencies.ActiveCalls, policy: dependencies.Policy, newID: randomID,
	}, nil
}

// RecoverTask 原子重放 Receipt，或从当前中断版本创建严格下一版本。
func (s *RecoverTaskService) RecoverTask(ctx context.Context, request RecoverTaskRequest) (TaskRecovered, error) {
	if s == nil {
		return TaskRecovered{}, errors.New("recover Task: service is not initialized")
	}
	fingerprint, err := recoverRequestFingerprint(request)
	if err != nil {
		return TaskRecovered{}, err
	}
	var response recoverReceiptResponse
	var restoredLog taskLogDraft
	created := false
	err = s.executor.Execute(ctx, func(ctx context.Context, tx contracts.RuntimeWriteTx) error {
		stored, lockErr := s.receipts.Lock(ctx, tx, request.CommandID)
		switch {
		case lockErr == nil:
			if stored.CommandType != CommandTypeRecover || stored.TargetID != string(request.TaskID) ||
				stored.RequestFingerprint != fingerprint {
				return ErrCommandConflict
			}
			if decodeErr := json.Unmarshal(stored.Response, &response); decodeErr != nil {
				return fmt.Errorf("decode Recover command receipt: %w", ErrPersistenceInvariantViolation)
			}
			return validateRecoverReceipt(response)
		case !errors.Is(lockErr, ErrRepositoryNotFound):
			return fmt.Errorf("lock Recover command receipt: %w", lockErr)
		}

		facts, lockErr := s.recovery.LockRecoveryFacts(ctx, tx, request.TaskID)
		if lockErr != nil {
			return fmt.Errorf("lock Recover facts: %w", lockErr)
		}
		now, clockErr := s.clock.Now(ctx, tx)
		if clockErr != nil {
			return fmt.Errorf("read Recover database clock: %w", clockErr)
		}
		// 终态优先于 deadline。Recover 不得把已经提交的业务结果重新解释为 Timeout。
		if facts.Task.Status.Terminal() {
			response = recoverRejected(facts, recoverReceiptErrorStateConflict, "")
			return s.insertRecoverReceipt(ctx, tx, request, fingerprint, response, now)
		}
		if !now.Before(facts.Task.DeadlineAt) {
			var failureErr error
			response, failureErr = s.terminalizeFailure(ctx, tx, request, fingerprint, facts, now,
				contracts.ErrorCodeTaskTimeout, contracts.ReasonCode(""), recoverReceiptErrorTimedOut)
			return failureErr
		}
		phase, phaseErr := recoveryPhase(facts)
		if phaseErr != nil {
			response = recoverRejected(facts, recoverReceiptErrorStateConflict, "")
			return s.insertRecoverReceipt(ctx, tx, request, fingerprint, response, now)
		}
		validated, validateErr := s.checkpoints.ValidateRecoverySource(ctx, tx, ValidateRecoveryCheckpointRequest{
			TaskID: facts.Task.TaskID, RunID: facts.Run.RunID,
			SourceExecutionVersion: facts.Execution.ExecutionVersion, Phase: phase,
		})
		if validateErr != nil {
			return fmt.Errorf("validate Recover source: %w", validateErr)
		}
		var source RecoveryCheckpointValid
		switch typed := validated.(type) {
		case RecoveryCheckpointValid:
			source = typed
		case RecoveryCheckpointInvalid:
			if !typed.ReasonCode.ValidForCheckpointInvalid() {
				return fmt.Errorf("invalid Recover Checkpoint reason: %w", ErrPersistenceInvariantViolation)
			}
			var failureErr error
			response, failureErr = s.terminalizeFailure(ctx, tx, request, fingerprint, facts, now,
				contracts.ErrorCodeCheckpointInvalid, typed.ReasonCode, recoverReceiptErrorCheckpointInvalid)
			return failureErr
		case RecoveryCheckpointInvariantViolation:
			return fmt.Errorf("Recover source invariant violation: %s: %w", typed.ReasonCode, ErrPersistenceInvariantViolation)
		default:
			return fmt.Errorf("unknown Recover source result: %w", ErrPersistenceInvariantViolation)
		}
		if source.Source == nil || source.CheckpointID == "" || !source.ExecutionConfigHash.Valid() || !source.NextAction.Valid() {
			return fmt.Errorf("invalid validated Recover source: %w", ErrPersistenceInvariantViolation)
		}
		// P2 只生产验收 GENERATE_PLAN。Step、Tool 和 Approval 来源必须等各 Owner 阶段
		// 提供真实数据库事实和静态 Tool 安全 Guard 后再接入，不能在此静默放行。
		if source.NextAction != contracts.CheckpointNextActionGeneratePlan {
			return fmt.Errorf("Recover source requires a later-phase provider: %w", ErrPersistenceInvariantViolation)
		}
		agent, exists := s.configs.LookupAgent(facts.Task.AgentID)
		if !exists || !agent.ExecutionConfig.Agent.Enabled {
			return errors.New("recover Task: validated Agent configuration is unavailable")
		}
		currentHash, hashErr := HashExecutionConfigV1(agent.ExecutionConfig)
		if hashErr != nil {
			return fmt.Errorf("hash Recover execution config: %w", hashErr)
		}
		if currentHash != facts.Execution.ExecutionConfigHash || currentHash != source.ExecutionConfigHash {
			response = recoverConfigMismatch(facts, source, currentHash)
			return s.insertRecoverReceipt(ctx, tx, request, fingerprint, response, now)
		}
		newVersion := facts.Execution.ExecutionVersion + 1
		if !newVersion.Valid() {
			return fmt.Errorf("advance Recover execution version: %w", ErrPersistenceInvariantViolation)
		}
		executionID, idErr := s.newID("execution")
		if idErr != nil {
			return fmt.Errorf("create Recover TaskExecution ID: %w", idErr)
		}
		if insertErr := s.executions.Insert(ctx, tx, TaskExecution{
			TaskExecutionID: TaskExecutionID(executionID), TaskID: facts.Task.TaskID,
			ExecutionVersion: newVersion, Status: contracts.TaskExecutionStatusQueued,
			ExecutionConfigHash: currentHash, CreatedAt: now,
		}); insertErr != nil {
			return fmt.Errorf("insert Recover TaskExecution: %w", insertErr)
		}
		taskStatus, runStatus := recoveryTargetStates(source.NextAction)
		if facts.Task.Status != taskStatus {
			if decision := s.policy.CanTaskTransition(facts.Task.Status, taskStatus); !decision.Allowed {
				return fmt.Errorf("validate Recover Task lifecycle: %s", decision.Reason)
			}
		}
		updated, updateErr := s.tasks.Update(ctx, tx, TaskUpdate{
			TaskID: facts.Task.TaskID, ExpectedStatus: facts.Task.Status,
			ExpectedCurrentExecutionVersion: facts.Execution.ExecutionVersion,
			Status:                          taskStatus, CurrentExecutionVersion: newVersion, ResultSummary: facts.Task.ResultSummary,
			QueuedAt: &now, StartedAt: facts.Task.StartedAt,
		})
		if updateErr != nil || !updated {
			return conditionalRecoverError("update Recover Task", updateErr)
		}
		updated, updateErr = s.runs.Update(ctx, tx, RunUpdate{
			TaskID: facts.Task.TaskID, RunID: facts.Run.RunID, ExecutionVersion: newVersion,
			ExpectedStatus: facts.Run.Status, Status: runStatus, PlanID: facts.Run.PlanID,
			CurrentStepID: facts.Run.CurrentStepID, Context: facts.Run.Context,
			StartedAt: facts.Run.StartedAt,
		})
		if updateErr != nil || !updated {
			return conditionalRecoverError("update Recover Run", updateErr)
		}
		checkpointID, checkpointErr := s.checkpoints.CreateRecoveryStart(ctx, tx, CreateRecoveryStartRequest{
			TaskID: facts.Task.TaskID, RunID: facts.Run.RunID, NewExecutionVersion: newVersion,
			ExecutionConfigHash: currentHash, ValidatedSource: source.Source,
		})
		if checkpointErr != nil {
			return fmt.Errorf("create Recover start Checkpoint: %w", checkpointErr)
		}
		response = recoverSucceeded(facts, newVersion, taskStatus, runStatus, now, checkpointID)
		if receiptErr := s.insertRecoverReceipt(ctx, tx, request, fingerprint, response, now); receiptErr != nil {
			return receiptErr
		}
		restoredLog = checkpointRestoredTaskLogDraft(
			facts.Task.TaskID,
			facts.Run.RunID,
			facts.Execution.ExecutionVersion,
			newVersion,
		)
		created = true
		return nil
	})
	if err != nil {
		return TaskRecovered{}, err
	}
	result, resultErr := response.result()
	if created {
		appendTaskLogBestEffort(ctx, s.executor, s.taskLogs, s.clock, restoredLog)
	}
	return result, resultErr
}

func recoveryPhase(facts TerminationFacts) (RecoverySourcePhase, error) {
	if facts.Task.CurrentExecutionVersion != facts.Execution.ExecutionVersion || facts.Task.CurrentRunID != facts.Run.RunID ||
		facts.Run.TaskID != facts.Task.TaskID || facts.Execution.TaskID != facts.Task.TaskID || facts.Task.QueuedAt != nil ||
		facts.Execution.Status != contracts.TaskExecutionStatusInterrupted || facts.Task.Status.Terminal() {
		return 0, ErrRecoverStateConflict
	}
	if facts.Execution.ErrorCode == nil {
		return 0, ErrRecoverStateConflict
	}
	if facts.Task.Status == contracts.TaskStatusInterrupted && facts.Run.Status == contracts.RunStatusPending &&
		*facts.Execution.ErrorCode == contracts.ErrorCodeConfigVersionMismatch && facts.Execution.StartedAt == nil &&
		facts.Run.PlanID == nil && facts.Run.CurrentStepID == nil && facts.Step == nil && facts.ToolExecution == nil {
		return RecoverySourceBeforeFirstExecution, nil
	}
	if facts.Run.Status == contracts.RunStatusRunning && (facts.Task.Status == contracts.TaskStatusRunning ||
		facts.Task.Status == contracts.TaskStatusInterrupted) {
		switch *facts.Execution.ErrorCode {
		case contracts.ErrorCodeWorkerInterrupted, contracts.ErrorCodeResultPersistenceFailed, contracts.ErrorCodeConfigVersionMismatch:
			return RecoverySourceStartedExecution, nil
		}
	}
	return 0, ErrRecoverStateConflict
}

func recoveryTargetStates(action contracts.CheckpointNextAction) (contracts.TaskStatus, contracts.RunStatus) {
	if action == contracts.CheckpointNextActionGeneratePlan {
		return contracts.TaskStatusPending, contracts.RunStatusPending
	}
	return contracts.TaskStatusRunning, contracts.RunStatusRunning
}

func conditionalRecoverError(operation string, err error) error {
	if err != nil {
		return fmt.Errorf("%s: %w", operation, err)
	}
	return fmt.Errorf("%s condition missed: %w", operation, ErrPersistenceInvariantViolation)
}

const (
	recoverReceiptOutcomeRecovered       = "Recovered"
	recoverReceiptOutcomeRejected        = "Rejected"
	recoverReceiptErrorStateConflict     = "RecoverStateConflict"
	recoverReceiptErrorConfigMismatch    = "CONFIG_VERSION_MISMATCH"
	recoverReceiptErrorCheckpointInvalid = "CheckpointInvalid"
	recoverReceiptErrorTimedOut          = "TaskTimeout"
)

type recoverReceiptResponse struct {
	Outcome                 string                        `json:"outcome"`
	TaskID                  contracts.TaskID              `json:"task_id"`
	RunID                   contracts.RunID               `json:"run_id"`
	SourceExecutionVersion  contracts.ExecutionVersion    `json:"source_execution_version"`
	NewExecutionVersion     contracts.ExecutionVersion    `json:"new_execution_version,omitempty"`
	TaskStatus              contracts.TaskStatus          `json:"task_status"`
	RunStatus               contracts.RunStatus           `json:"run_status"`
	ExecutionStatus         contracts.TaskExecutionStatus `json:"execution_status"`
	QueuedAt                time.Time                     `json:"queued_at,omitempty"`
	RecoveryCheckpointID    contracts.CheckpointID        `json:"recovery_checkpoint_id,omitempty"`
	ErrorCode               string                        `json:"error_code,omitempty"`
	ReasonCode              contracts.ReasonCode          `json:"reason_code,omitempty"`
	CurrentConfigHash       contracts.ExecutionConfigHash `json:"current_config_hash,omitempty"`
	TaskExecutionConfigHash contracts.ExecutionConfigHash `json:"task_execution_config_hash,omitempty"`
	CheckpointConfigHash    contracts.ExecutionConfigHash `json:"checkpoint_config_hash,omitempty"`
	SourceCheckpointID      contracts.CheckpointID        `json:"source_checkpoint_id,omitempty"`
}

func recoverSucceeded(facts TerminationFacts, newVersion contracts.ExecutionVersion, taskStatus contracts.TaskStatus,
	runStatus contracts.RunStatus, queuedAt time.Time, checkpointID contracts.CheckpointID) recoverReceiptResponse {
	return recoverReceiptResponse{Outcome: recoverReceiptOutcomeRecovered, TaskID: facts.Task.TaskID,
		RunID: facts.Run.RunID, SourceExecutionVersion: facts.Execution.ExecutionVersion,
		NewExecutionVersion: newVersion, TaskStatus: taskStatus, RunStatus: runStatus,
		ExecutionStatus: contracts.TaskExecutionStatusQueued, QueuedAt: queuedAt,
		RecoveryCheckpointID: checkpointID}
}

func recoverRejected(facts TerminationFacts, code string, reason contracts.ReasonCode) recoverReceiptResponse {
	return recoverReceiptResponse{Outcome: recoverReceiptOutcomeRejected, TaskID: facts.Task.TaskID,
		RunID: facts.Run.RunID, SourceExecutionVersion: facts.Execution.ExecutionVersion,
		TaskStatus: facts.Task.Status, RunStatus: facts.Run.Status, ExecutionStatus: facts.Execution.Status,
		ErrorCode: code, ReasonCode: reason}
}

func recoverConfigMismatch(facts TerminationFacts, source RecoveryCheckpointValid,
	current contracts.ExecutionConfigHash) recoverReceiptResponse {
	response := recoverRejected(facts, recoverReceiptErrorConfigMismatch, "")
	response.CurrentConfigHash = current
	response.TaskExecutionConfigHash = facts.Execution.ExecutionConfigHash
	response.CheckpointConfigHash = source.ExecutionConfigHash
	response.SourceCheckpointID = source.CheckpointID
	return response
}

func (s *RecoverTaskService) terminalizeFailure(ctx context.Context, tx contracts.RuntimeWriteTx,
	request RecoverTaskRequest, fingerprint string, facts TerminationFacts, now time.Time,
	errorCode contracts.ErrorCode, reason contracts.ReasonCode, responseCode string) (recoverReceiptResponse, error) {
	var terminationReason *contracts.TerminationReason
	if errorCode == contracts.ErrorCodeTaskTimeout {
		reason := contracts.TerminationReasonTimedOut
		terminationReason = &reason
	}
	if facts.Step != nil || facts.ToolExecution != nil {
		return recoverReceiptResponse{}, fmt.Errorf("Recover failure requires a later-phase terminalization provider: %w", ErrPersistenceInvariantViolation)
	}
	if decision := s.policy.CanTaskTransition(facts.Task.Status, contracts.TaskStatusFailed); !decision.Allowed {
		return recoverReceiptResponse{}, fmt.Errorf("validate Recover failure Task lifecycle: %s", decision.Reason)
	}
	if decision := s.policy.CanRunTransition(facts.Run.Status, contracts.RunStatusFailed); !decision.Allowed {
		return recoverReceiptResponse{}, fmt.Errorf("validate Recover failure Run lifecycle: %s", decision.Reason)
	}
	if decision := s.policy.CanExecutionTransition(facts.Execution.Status, contracts.TaskExecutionStatusFailed); !decision.Allowed {
		return recoverReceiptResponse{}, fmt.Errorf("validate Recover failure Execution lifecycle: %s", decision.Reason)
	}
	applied, err := s.recovery.ApplyRecoveryFailure(ctx, tx, ApplyRecoveryFailureRequest{
		TaskID: facts.Task.TaskID, ExpectedExecutionVersion: facts.Execution.ExecutionVersion,
		ExpectedTaskStatus: facts.Task.Status, ExpectedRunStatus: facts.Run.Status,
		ExpectedExecutionStatus: facts.Execution.Status, ErrorCode: errorCode,
		TerminationReason: terminationReason, EndedAt: now,
	})
	if err != nil {
		return recoverReceiptResponse{}, fmt.Errorf("apply Recover failure terminalization: %w", err)
	}
	if !applied {
		return recoverReceiptResponse{}, fmt.Errorf("Recover failure terminalization condition missed: %w", ErrPersistenceInvariantViolation)
	}
	if err := ensureTerminationReport(ctx, tx, s.reports, facts, now); err != nil {
		return recoverReceiptResponse{}, err
	}
	response := recoverRejected(facts, responseCode, reason)
	response.TaskStatus = contracts.TaskStatusFailed
	response.RunStatus = contracts.RunStatusFailed
	response.ExecutionStatus = contracts.TaskExecutionStatusFailed
	return response, s.insertRecoverReceipt(ctx, tx, request, fingerprint, response, now)
}

func (s *RecoverTaskService) insertRecoverReceipt(ctx context.Context, tx contracts.RuntimeWriteTx,
	request RecoverTaskRequest, fingerprint string, response recoverReceiptResponse, now time.Time) error {
	encoded, err := json.Marshal(response)
	if err != nil {
		return fmt.Errorf("encode Recover command receipt: %w", err)
	}
	if err := s.receipts.Insert(ctx, tx, CommandReceipt{CommandID: request.CommandID,
		CommandType: CommandTypeRecover, TargetID: string(request.TaskID), RequestFingerprint: fingerprint,
		Response: encoded, CreatedAt: now}); err != nil {
		return fmt.Errorf("insert Recover command receipt: %w", err)
	}
	return nil
}

func recoverRequestFingerprint(request RecoverTaskRequest) (string, error) {
	if request.CommandID == "" || request.TaskID == "" || strings.TrimSpace(request.OperatorID) == "" ||
		!utf8.ValidString(string(request.CommandID)) || !utf8.ValidString(string(request.TaskID)) ||
		!utf8.ValidString(request.OperatorID) {
		return "", ErrInvalidArgument
	}
	encoded, err := json.Marshal(struct {
		TaskID     contracts.TaskID `json:"task_id"`
		OperatorID string           `json:"operator_id"`
	}{TaskID: request.TaskID, OperatorID: request.OperatorID})
	if err != nil {
		return "", fmt.Errorf("encode Recover request fingerprint: %w", err)
	}
	digest := sha256.Sum256(encoded)
	return hex.EncodeToString(digest[:]), nil
}

func validateRecoverReceipt(response recoverReceiptResponse) error {
	if response.TaskID == "" || response.RunID == "" || !response.SourceExecutionVersion.Valid() {
		return ErrPersistenceInvariantViolation
	}
	if response.Outcome == recoverReceiptOutcomeRecovered {
		if response.NewExecutionVersion != response.SourceExecutionVersion+1 ||
			response.ExecutionStatus != contracts.TaskExecutionStatusQueued || response.QueuedAt.IsZero() ||
			response.RecoveryCheckpointID == "" {
			return ErrPersistenceInvariantViolation
		}
		return nil
	}
	if response.Outcome != recoverReceiptOutcomeRejected {
		return ErrPersistenceInvariantViolation
	}
	switch response.ErrorCode {
	case recoverReceiptErrorStateConflict, recoverReceiptErrorTimedOut:
		return nil
	case recoverReceiptErrorCheckpointInvalid:
		if !response.ReasonCode.ValidForCheckpointInvalid() {
			return ErrPersistenceInvariantViolation
		}
		return nil
	case recoverReceiptErrorConfigMismatch:
		if !response.CurrentConfigHash.Valid() || !response.TaskExecutionConfigHash.Valid() ||
			!response.CheckpointConfigHash.Valid() || response.SourceCheckpointID == "" {
			return ErrPersistenceInvariantViolation
		}
		return nil
	default:
		return ErrPersistenceInvariantViolation
	}
}

func (response recoverReceiptResponse) result() (TaskRecovered, error) {
	if err := validateRecoverReceipt(response); err != nil {
		return TaskRecovered{}, err
	}
	if response.Outcome == recoverReceiptOutcomeRecovered {
		return TaskRecovered{TaskID: response.TaskID, RunID: response.RunID,
			SourceExecutionVersion: response.SourceExecutionVersion, NewExecutionVersion: response.NewExecutionVersion,
			TaskStatus: response.TaskStatus, RunStatus: response.RunStatus, ExecutionStatus: response.ExecutionStatus,
			QueuedAt: response.QueuedAt.UTC(), RecoveryCheckpointID: response.RecoveryCheckpointID}, nil
	}
	switch response.ErrorCode {
	case recoverReceiptErrorStateConflict:
		return TaskRecovered{}, ErrRecoverStateConflict
	case recoverReceiptErrorConfigMismatch:
		return TaskRecovered{}, ErrRecoverConfigMismatch
	case recoverReceiptErrorCheckpointInvalid:
		return TaskRecovered{}, ErrRecoverCheckpointInvalid
	case recoverReceiptErrorTimedOut:
		return TaskRecovered{}, ErrTaskTimedOut
	default:
		return TaskRecovered{}, ErrPersistenceInvariantViolation
	}
}
