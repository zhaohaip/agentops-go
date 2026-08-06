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
	// ErrTaskAlreadyTerminal 表示 Cancel 目标已经处于业务终态。
	ErrTaskAlreadyTerminal = errors.New("TaskAlreadyTerminal")
	// ErrTaskTimedOut 表示 Cancel 取得写入顺序时目标已经到期并按 Timeout 收敛。
	ErrTaskTimedOut = errors.New(string(contracts.ErrorCodeTaskTimeout))
)

// CancelTaskRequest 是 API Layer 传入的幂等取消命令。
type CancelTaskRequest struct {
	CommandID  CommandID
	TaskID     contracts.TaskID
	OperatorID string
}

// TaskCancelled 是 CancelTask 已提交的确定结果。
type TaskCancelled struct {
	TaskID            contracts.TaskID
	TaskStatus        contracts.TaskStatus
	RunStatus         contracts.RunStatus
	ExecutionStatus   contracts.TaskExecutionStatus
	ExecutionVersion  contracts.ExecutionVersion
	TerminationReason contracts.TerminationReason
}

// CancelTaskService 编排幂等 Cancel 命令及原子终态。
type CancelTaskService struct {
	executor     contracts.RuntimeWriteExecutor
	terminations TerminationRepository
	receipts     CommandReceiptRepository
	reports      contracts.PendingReportWriter
	clock        DatabaseClock
	configs      AgentConfigSource
	taskLogs     TaskLogRepository
	activeCalls  *activecall.Registry
	policy       lifecycle.Policy
}

// CancelTaskDependencies 声明 CancelTask 的最小出站依赖。
type CancelTaskDependencies struct {
	Executor     contracts.RuntimeWriteExecutor
	Terminations TerminationRepository
	Receipts     CommandReceiptRepository
	Reports      contracts.PendingReportWriter
	Clock        DatabaseClock
	Configs      AgentConfigSource
	TaskLogs     TaskLogRepository
	ActiveCalls  *activecall.Registry
	Policy       lifecycle.Policy
}

// NewCancelTaskService 创建未接入生产组合根的 CancelTask 服务。
func NewCancelTaskService(dependencies CancelTaskDependencies) (*CancelTaskService, error) {
	if dependencies.Executor == nil || dependencies.Terminations == nil || dependencies.Receipts == nil ||
		dependencies.Reports == nil || dependencies.Clock == nil || dependencies.Configs == nil ||
		dependencies.TaskLogs == nil || dependencies.ActiveCalls == nil {
		return nil, errors.New("create CancelTask service: dependencies are required")
	}
	return &CancelTaskService{
		executor: dependencies.Executor, terminations: dependencies.Terminations,
		receipts: dependencies.Receipts, reports: dependencies.Reports, clock: dependencies.Clock,
		configs: dependencies.Configs, taskLogs: dependencies.TaskLogs,
		activeCalls: dependencies.ActiveCalls, policy: dependencies.Policy,
	}, nil
}

// CancelTask 原子取消当前执行，或重放不可变 Command Receipt。
func (s *CancelTaskService) CancelTask(ctx context.Context, request CancelTaskRequest) (TaskCancelled, error) {
	if s == nil {
		return TaskCancelled{}, errors.New("cancel Task: service is not initialized")
	}
	fingerprint, err := cancelRequestFingerprint(request)
	if err != nil {
		return TaskCancelled{}, err
	}
	var response cancelReceiptResponse
	var cancelKey *activecall.Key
	var terminalLog taskLogDraft
	var wroteTerminal bool
	err = s.executor.Execute(ctx, func(ctx context.Context, tx contracts.RuntimeWriteTx) error {
		stored, lockErr := s.receipts.Lock(ctx, tx, request.CommandID)
		switch {
		case lockErr == nil:
			if stored.CommandType != CommandTypeCancel || stored.TargetID != string(request.TaskID) ||
				stored.RequestFingerprint != fingerprint {
				return ErrCommandConflict
			}
			if decodeErr := json.Unmarshal(stored.Response, &response); decodeErr != nil {
				return fmt.Errorf("decode Cancel command receipt: %w", ErrPersistenceInvariantViolation)
			}
			return validateCancelReceipt(response)
		case !errors.Is(lockErr, ErrRepositoryNotFound):
			return fmt.Errorf("lock Cancel command receipt: %w", lockErr)
		}

		facts, lockErr := s.terminations.LockTerminationFacts(ctx, tx, request.TaskID)
		if lockErr != nil {
			return fmt.Errorf("lock Cancel termination facts: %w", lockErr)
		}
		now, clockErr := s.clock.Now(ctx, tx)
		if clockErr != nil {
			return fmt.Errorf("read Cancel database clock: %w", clockErr)
		}
		if facts.Task.Status.Terminal() {
			response = rejectedCancelReceipt(facts, cancelReceiptErrorAlreadyTerminal)
			return s.insertCancelReceipt(ctx, tx, request, fingerprint, response, now)
		}
		reason := contracts.TerminationReasonCancelled
		taskStatus := contracts.TaskStatusCancelled
		errorCode := contracts.ErrorCodeTaskCancelled
		responseError := ""
		if !now.Before(facts.Task.DeadlineAt) {
			reason = contracts.TerminationReasonTimedOut
			taskStatus = contracts.TaskStatusFailed
			errorCode = contracts.ErrorCodeTaskTimeout
			responseError = cancelReceiptErrorTimedOut
		}
		requestUpdate, updateErr := s.terminationRequest(facts, now, taskStatus, errorCode, reason)
		if updateErr != nil {
			return updateErr
		}
		applied, applyErr := s.terminations.ApplyTermination(ctx, tx, requestUpdate)
		if applyErr != nil {
			return fmt.Errorf("apply Cancel termination: %w", applyErr)
		}
		if !applied {
			return fmt.Errorf("Cancel termination condition missed: %w", ErrPersistenceInvariantViolation)
		}
		if ensureErr := ensureTerminationReport(ctx, tx, s.reports, facts, now); ensureErr != nil {
			return ensureErr
		}
		if responseError == "" {
			response = cancelledReceipt(facts, reason)
		} else {
			response = rejectedCancelReceipt(facts, responseError)
			response.TaskStatus = taskStatus
			response.RunStatus = contracts.RunStatusFailed
			response.ExecutionStatus = contracts.TaskExecutionStatusFailed
			response.TerminationReason = reason
		}
		if receiptErr := s.insertCancelReceipt(ctx, tx, request, fingerprint, response, now); receiptErr != nil {
			return receiptErr
		}
		if facts.Execution.WorkerID != nil {
			key := activecall.Key{TaskID: facts.Task.TaskID, ExecutionVersion: facts.Execution.ExecutionVersion, WorkerID: *facts.Execution.WorkerID}
			cancelKey = &key
		}
		terminalLog = terminalTaskLogDraft(QueueCandidate{TaskID: facts.Task.TaskID, RunID: facts.Run.RunID,
			ExecutionVersion: facts.Execution.ExecutionVersion}, errorCode, &reason)
		wroteTerminal = true
		return nil
	})
	if err != nil {
		return TaskCancelled{}, err
	}
	if wroteTerminal {
		cause := activecall.CauseTaskCancelled
		if response.TerminationReason == contracts.TerminationReasonTimedOut {
			cause = activecall.CauseTaskTimedOut
		}
		if cancelKey != nil {
			_, _ = s.activeCalls.Cancel(*cancelKey, cause)
		}
		appendTaskLogBestEffort(ctx, s.executor, s.taskLogs, s.clock, terminalLog)
	}
	return response.result()
}

func (s *CancelTaskService) terminationRequest(
	facts TerminationFacts,
	now time.Time,
	taskStatus contracts.TaskStatus,
	errorCode contracts.ErrorCode,
	reason contracts.TerminationReason,
) (ApplyTerminationRequest, error) {
	if facts.Task.CurrentExecutionVersion != facts.Execution.ExecutionVersion || facts.Run.TaskID != facts.Task.TaskID ||
		facts.Run.RunID != facts.Task.CurrentRunID || facts.Execution.TaskID != facts.Task.TaskID {
		return ApplyTerminationRequest{}, fmt.Errorf("termination facts attribution mismatch: %w", ErrPersistenceInvariantViolation)
	}
	if decision := s.policy.CanTaskTransition(facts.Task.Status, taskStatus); !decision.Allowed {
		return ApplyTerminationRequest{}, fmt.Errorf("validate termination Task lifecycle: %s", decision.Reason)
	}
	if decision := s.policy.CanRunTransition(facts.Run.Status, contracts.RunStatusFailed); !decision.Allowed {
		return ApplyTerminationRequest{}, fmt.Errorf("validate termination Run lifecycle: %s", decision.Reason)
	}
	if decision := s.policy.CanExecutionTransition(facts.Execution.Status, contracts.TaskExecutionStatusFailed); !decision.Allowed {
		return ApplyTerminationRequest{}, fmt.Errorf("validate termination Execution lifecycle: %s", decision.Reason)
	}
	request := ApplyTerminationRequest{
		TaskID: facts.Task.TaskID, ExpectedExecutionVersion: facts.Execution.ExecutionVersion,
		ExpectedTaskStatus: facts.Task.Status, ExpectedRunStatus: facts.Run.Status,
		ExpectedExecutionStatus: facts.Execution.Status, TaskStatus: taskStatus,
		TaskErrorCode: errorCode, RunErrorCode: errorCode, StepErrorCode: errorCode,
		TerminationReason: reason, EndedAt: now,
		PreserveExecutionEndedAt: facts.Execution.Status == contracts.TaskExecutionStatusInterrupted,
	}
	if facts.Execution.ErrorCode != nil && *facts.Execution.ErrorCode == contracts.ErrorCodeConfigVersionMismatch {
		preserved := *facts.Execution.ErrorCode
		request.ExecutionErrorCode = &preserved
	}
	if facts.Step != nil && !facts.Step.Status.Terminal() {
		if decision := s.policy.CanStepTransition(facts.Step.Status, contracts.StepStatusFailed); !decision.Allowed {
			return ApplyTerminationRequest{}, fmt.Errorf("validate termination Step lifecycle: %s", decision.Reason)
		}
		status := facts.Step.Status
		request.ExpectedStepStatus = &status
	}
	if facts.ToolExecution != nil && facts.ToolExecution.Status == contracts.ToolExecutionStatusRunning {
		status := facts.ToolExecution.Status
		request.ExpectedToolStatus = &status
		readOnly, classifyErr := s.toolReadOnly(facts.Task.AgentID, facts.ToolExecution.ToolName)
		if classifyErr != nil {
			return ApplyTerminationRequest{}, classifyErr
		}
		if readOnly {
			failed := contracts.ToolExecutionStatusFailed
			request.ToolStatus = &failed
			toolError := errorCode
			request.ToolErrorCode = &toolError
		} else {
			unknown := contracts.ToolExecutionStatusUnknown
			request.ToolStatus = &unknown
			request.ToolSideEffectUnknown = true
			if request.ExecutionErrorCode == nil {
				writeInterrupted := contracts.ErrorCodeWriteToolInterrupted
				request.ExecutionErrorCode = &writeInterrupted
			}
		}
	}
	return request, nil
}

func (s *CancelTaskService) toolReadOnly(agentID contracts.AgentID, toolName contracts.ToolName) (bool, error) {
	config, exists := s.configs.LookupAgent(agentID)
	if !exists || !config.ExecutionConfig.Agent.Enabled {
		return false, errors.New("termination validated Agent configuration is unavailable")
	}
	for _, tool := range config.ExecutionConfig.ToolFramework.Tools {
		if tool.Enabled && tool.Name == toolName {
			if tool.RiskLevel == contracts.RiskLevelLow && tool.ReadOnly {
				return true, nil
			}
			if tool.RiskLevel == contracts.RiskLevelHigh && !tool.ReadOnly {
				return false, nil
			}
			return false, fmt.Errorf("termination Tool capability is inconsistent: %w", ErrPersistenceInvariantViolation)
		}
	}
	return false, fmt.Errorf("termination Tool is unavailable: %w", ErrPersistenceInvariantViolation)
}

const (
	cancelReceiptOutcomeCancelled     = "Cancelled"
	cancelReceiptOutcomeRejected      = "Rejected"
	cancelReceiptErrorAlreadyTerminal = "TaskAlreadyTerminal"
	cancelReceiptErrorTimedOut        = "TaskTimeout"
)

type cancelReceiptResponse struct {
	Outcome           string                        `json:"outcome"`
	TaskID            contracts.TaskID              `json:"task_id"`
	TaskStatus        contracts.TaskStatus          `json:"task_status"`
	RunStatus         contracts.RunStatus           `json:"run_status"`
	ExecutionStatus   contracts.TaskExecutionStatus `json:"execution_status"`
	ExecutionVersion  contracts.ExecutionVersion    `json:"execution_version"`
	TerminationReason contracts.TerminationReason   `json:"termination_reason,omitempty"`
	ErrorCode         string                        `json:"error_code,omitempty"`
}

func cancelRequestFingerprint(request CancelTaskRequest) (string, error) {
	if request.CommandID == "" || request.TaskID == "" || strings.TrimSpace(request.OperatorID) == "" ||
		!utf8.ValidString(string(request.CommandID)) || !utf8.ValidString(string(request.TaskID)) || !utf8.ValidString(request.OperatorID) {
		return "", ErrInvalidArgument
	}
	encoded, err := json.Marshal(struct {
		TaskID     contracts.TaskID `json:"task_id"`
		OperatorID string           `json:"operator_id"`
	}{TaskID: request.TaskID, OperatorID: request.OperatorID})
	if err != nil {
		return "", fmt.Errorf("encode Cancel request fingerprint: %w", err)
	}
	digest := sha256.Sum256(encoded)
	return hex.EncodeToString(digest[:]), nil
}

func cancelledReceipt(facts TerminationFacts, reason contracts.TerminationReason) cancelReceiptResponse {
	return cancelReceiptResponse{Outcome: cancelReceiptOutcomeCancelled, TaskID: facts.Task.TaskID,
		TaskStatus: contracts.TaskStatusCancelled, RunStatus: contracts.RunStatusFailed,
		ExecutionStatus: contracts.TaskExecutionStatusFailed, ExecutionVersion: facts.Execution.ExecutionVersion,
		TerminationReason: reason}
}

func rejectedCancelReceipt(facts TerminationFacts, code string) cancelReceiptResponse {
	return cancelReceiptResponse{Outcome: cancelReceiptOutcomeRejected, TaskID: facts.Task.TaskID,
		TaskStatus: facts.Task.Status, RunStatus: facts.Run.Status, ExecutionStatus: facts.Execution.Status,
		ExecutionVersion: facts.Execution.ExecutionVersion, ErrorCode: code}
}

func (s *CancelTaskService) insertCancelReceipt(ctx context.Context, tx contracts.RuntimeWriteTx, request CancelTaskRequest,
	fingerprint string, response cancelReceiptResponse, now time.Time) error {
	encoded, err := json.Marshal(response)
	if err != nil {
		return fmt.Errorf("encode Cancel command receipt: %w", err)
	}
	if err := s.receipts.Insert(ctx, tx, CommandReceipt{CommandID: request.CommandID, CommandType: CommandTypeCancel,
		TargetID: string(request.TaskID), RequestFingerprint: fingerprint, Response: encoded, CreatedAt: now}); err != nil {
		return fmt.Errorf("insert Cancel command receipt: %w", err)
	}
	return nil
}

func validateCancelReceipt(response cancelReceiptResponse) error {
	if response.TaskID == "" || !response.ExecutionVersion.Valid() {
		return ErrPersistenceInvariantViolation
	}
	switch response.Outcome {
	case cancelReceiptOutcomeCancelled:
		if response.TaskStatus != contracts.TaskStatusCancelled || response.RunStatus != contracts.RunStatusFailed ||
			response.ExecutionStatus != contracts.TaskExecutionStatusFailed || response.TerminationReason != contracts.TerminationReasonCancelled {
			return ErrPersistenceInvariantViolation
		}
	case cancelReceiptOutcomeRejected:
		if response.ErrorCode != cancelReceiptErrorAlreadyTerminal && response.ErrorCode != cancelReceiptErrorTimedOut {
			return ErrPersistenceInvariantViolation
		}
	default:
		return ErrPersistenceInvariantViolation
	}
	return nil
}

func (response cancelReceiptResponse) result() (TaskCancelled, error) {
	if err := validateCancelReceipt(response); err != nil {
		return TaskCancelled{}, err
	}
	if response.Outcome == cancelReceiptOutcomeCancelled {
		return TaskCancelled{TaskID: response.TaskID, TaskStatus: response.TaskStatus,
			RunStatus: response.RunStatus, ExecutionStatus: response.ExecutionStatus,
			ExecutionVersion: response.ExecutionVersion, TerminationReason: response.TerminationReason}, nil
	}
	if response.ErrorCode == cancelReceiptErrorTimedOut {
		return TaskCancelled{}, ErrTaskTimedOut
	}
	return TaskCancelled{}, ErrTaskAlreadyTerminal
}

func ensureTerminationReport(ctx context.Context, tx contracts.RuntimeWriteTx, reports contracts.PendingReportWriter,
	facts TerminationFacts, now time.Time) error {
	result, err := reports.EnsurePending(ctx, tx, contracts.EnsurePendingReportRequest{TaskID: facts.Task.TaskID, RunID: facts.Run.RunID, CreatedAt: now})
	if err != nil {
		return fmt.Errorf("ensure pending termination Report: %w", err)
	}
	switch result.(type) {
	case contracts.EnsurePendingReportCreated, contracts.EnsurePendingReportExisting:
		return nil
	default:
		return fmt.Errorf("unknown pending Report result: %w", ErrPersistenceInvariantViolation)
	}
}
