package taskruntime

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/zhaohaip/agentops-go/internal/contracts"
	"github.com/zhaohaip/agentops-go/internal/lifecycle"
)

// StartupCleanupStep 是遗留执行分类需要的当前 Step 投影。
type StartupCleanupStep struct {
	StepID    contracts.StepID
	Type      contracts.StepType
	Status    contracts.StepStatus
	ToolName  contracts.ToolName
	ErrorCode *contracts.ErrorCode
	EndedAt   *time.Time
}

// StartupCleanupToolExecution 是遗留执行分类需要的 ToolExecution 投影。
// read_only 必须从冻结配置取得，不能由该投影提供。
type StartupCleanupToolExecution struct {
	ToolExecutionID   contracts.ToolExecutionID
	TaskID            contracts.TaskID
	StepID            contracts.StepID
	ExecutionVersion  contracts.ExecutionVersion
	ToolName          contracts.ToolName
	Status            contracts.ToolExecutionStatus
	ErrorCode         *contracts.ErrorCode
	SideEffectUnknown bool
	EndedAt           *time.Time
}

// StartupCleanupApprovedRecovery 是直接 Approved Approval 及其来源 Execution 的最小投影。
type StartupCleanupApprovedRecovery struct {
	ApprovalID                contracts.ApprovalID
	TaskID                    contracts.TaskID
	Status                    contracts.ApprovalStatus
	ApprovalExecutionVersion  contracts.ExecutionVersion
	ApprovalConfigHash        contracts.ExecutionConfigHash
	SourceExecutionConfigHash contracts.ExecutionConfigHash
	ToolName                  contracts.ToolName
	FrozenToolInput           contracts.FrozenToolInput
	ObservedValues            contracts.ObservedValues
	ResourceVersion           contracts.ResourceVersion
	FrozenInputHash           contracts.FrozenInputHash
}

// StartupCleanupFacts 是一次启动事务锁定的旧 Worker RUNNING 现场。
type StartupCleanupFacts struct {
	Task             Task
	Run              Run
	Execution        TaskExecution
	Step             *StartupCleanupStep
	ToolExecution    *StartupCleanupToolExecution
	ApprovedRecovery *StartupCleanupApprovedRecovery
}

// StartupCleanupDisposition 是 Repository 应用的封闭清理类别。
type StartupCleanupDisposition string

const (
	StartupCleanupInterrupt StartupCleanupDisposition = "INTERRUPT"
	StartupCleanupTerminal  StartupCleanupDisposition = "TERMINAL"
)

// ApplyStartupCleanupRequest 描述 Runtime 已分类完成的原子清理写入。
type ApplyStartupCleanupRequest struct {
	TaskID                  contracts.TaskID
	ExecutionVersion        contracts.ExecutionVersion
	ExpectedWorkerID        contracts.WorkerID
	ExpectedTaskStatus      contracts.TaskStatus
	ExpectedRunStatus       contracts.RunStatus
	ExpectedExecutionStatus contracts.TaskExecutionStatus
	ExpectedStepStatus      *contracts.StepStatus
	ExpectedToolStatus      *contracts.ToolExecutionStatus
	Disposition             StartupCleanupDisposition
	TaskErrorCode           *contracts.ErrorCode
	ExecutionErrorCode      contracts.ErrorCode
	TerminationReason       *contracts.TerminationReason
	StepErrorCode           *contracts.ErrorCode
	ToolStatus              *contracts.ToolExecutionStatus
	ToolErrorCode           *contracts.ErrorCode
	ToolSideEffectUnknown   bool
	EndedAt                 time.Time
	CheckpointReasonCode    *contracts.ReasonCode
}

// StartupCleanupRepository 在启动清理事务内锁定候选并应用已分类结果。
type StartupCleanupRepository interface {
	LockLegacyRunningExecutions(
		context.Context,
		contracts.RuntimeWriteTx,
		contracts.WorkerID,
	) ([]StartupCleanupFacts, error)
	ApplyStartupCleanup(context.Context, contracts.RuntimeWriteTx, ApplyStartupCleanupRequest) (bool, error)
}

// StartupCleanupSummary 汇总本次启动事务实际提交的清理结果。
type StartupCleanupSummary struct {
	Inspected    int
	Interrupted  int
	Terminalized int
}

// StartupCleanupService 在任何业务驱动启动前分类旧 Worker 遗留执行。
type StartupCleanupService struct {
	executor    contracts.RuntimeWriteExecutor
	repository  StartupCleanupRepository
	checkpoints StartupCleanupCheckpointPort
	reports     contracts.PendingReportWriter
	clock       DatabaseClock
	configs     AgentConfigSource
	policy      lifecycle.Policy
}

// StartupCleanupDependencies 声明启动清理的最小出站依赖。
type StartupCleanupDependencies struct {
	Executor    contracts.RuntimeWriteExecutor
	Repository  StartupCleanupRepository
	Checkpoints StartupCleanupCheckpointPort
	Reports     contracts.PendingReportWriter
	Clock       DatabaseClock
	Configs     AgentConfigSource
	Policy      lifecycle.Policy
}

// NewStartupCleanupService 创建仅供启动门禁调用的清理服务。
func NewStartupCleanupService(dependencies StartupCleanupDependencies) (*StartupCleanupService, error) {
	if dependencies.Executor == nil || dependencies.Repository == nil || dependencies.Checkpoints == nil ||
		dependencies.Reports == nil || dependencies.Clock == nil || dependencies.Configs == nil {
		return nil, errors.New("create StartupCleanup service: dependencies are required")
	}
	return &StartupCleanupService{
		executor: dependencies.Executor, repository: dependencies.Repository,
		checkpoints: dependencies.Checkpoints, reports: dependencies.Reports,
		clock: dependencies.Clock, configs: dependencies.Configs, policy: dependencies.Policy,
	}, nil
}

// StartupCleanup 在一个持锁写事务内完成全部遗留现场分类和必要 Report 写入。
func (s *StartupCleanupService) StartupCleanup(
	ctx context.Context,
	currentWorkerID contracts.WorkerID,
) (StartupCleanupSummary, error) {
	if s == nil {
		return StartupCleanupSummary{}, errors.New("run StartupCleanup: service is not initialized")
	}
	if currentWorkerID == "" {
		return StartupCleanupSummary{}, ErrInvalidArgument
	}
	var summary StartupCleanupSummary
	err := s.executor.Execute(ctx, func(ctx context.Context, tx contracts.RuntimeWriteTx) error {
		now, err := s.clock.Now(ctx, tx)
		if err != nil {
			return fmt.Errorf("read StartupCleanup database clock: %w", err)
		}
		candidates, err := s.repository.LockLegacyRunningExecutions(ctx, tx, currentWorkerID)
		if err != nil {
			return fmt.Errorf("lock StartupCleanup candidates: %w", err)
		}
		for _, facts := range candidates {
			summary.Inspected++
			request, terminal, classifyErr := s.classify(ctx, tx, facts, currentWorkerID, now)
			if classifyErr != nil {
				return classifyErr
			}
			if transitionErr := s.validateTransitions(facts, request); transitionErr != nil {
				return transitionErr
			}
			applied, applyErr := s.repository.ApplyStartupCleanup(ctx, tx, request)
			if applyErr != nil {
				return fmt.Errorf("apply StartupCleanup for Task %s: %w", facts.Task.TaskID, applyErr)
			}
			if !applied {
				return fmt.Errorf("StartupCleanup condition missed for Task %s: %w", facts.Task.TaskID, ErrPersistenceInvariantViolation)
			}
			if terminal {
				if reportErr := ensureTerminationReport(ctx, tx, s.reports, TerminationFacts{
					Task: facts.Task, Run: facts.Run, Execution: facts.Execution,
				}, now); reportErr != nil {
					return reportErr
				}
				summary.Terminalized++
			} else {
				summary.Interrupted++
			}
		}
		return nil
	})
	if err != nil {
		return StartupCleanupSummary{}, err
	}
	return summary, nil
}

func (s *StartupCleanupService) validateTransitions(
	facts StartupCleanupFacts,
	request ApplyStartupCleanupRequest,
) error {
	if request.Disposition == StartupCleanupInterrupt {
		if decision := s.policy.CanExecutionTransition(
			facts.Execution.Status, contracts.TaskExecutionStatusInterrupted,
		); !decision.Allowed {
			return fmt.Errorf("validate StartupCleanup Execution interrupt: %s", decision.Reason)
		}
		return nil
	}
	if request.Disposition != StartupCleanupTerminal {
		return fmt.Errorf("invalid StartupCleanup disposition: %w", ErrPersistenceInvariantViolation)
	}
	if decision := s.policy.CanTaskTransition(facts.Task.Status, contracts.TaskStatusFailed); !decision.Allowed {
		return fmt.Errorf("validate StartupCleanup Task terminal: %s", decision.Reason)
	}
	if decision := s.policy.CanRunTransition(facts.Run.Status, contracts.RunStatusFailed); !decision.Allowed {
		return fmt.Errorf("validate StartupCleanup Run terminal: %s", decision.Reason)
	}
	if decision := s.policy.CanExecutionTransition(
		facts.Execution.Status, contracts.TaskExecutionStatusFailed,
	); !decision.Allowed {
		return fmt.Errorf("validate StartupCleanup Execution terminal: %s", decision.Reason)
	}
	if facts.Step != nil && !facts.Step.Status.Terminal() {
		if decision := s.policy.CanStepTransition(facts.Step.Status, contracts.StepStatusFailed); !decision.Allowed {
			return fmt.Errorf("validate StartupCleanup Step terminal: %s", decision.Reason)
		}
	}
	return nil
}

func (s *StartupCleanupService) classify(
	ctx context.Context,
	tx contracts.RuntimeWriteTx,
	facts StartupCleanupFacts,
	currentWorkerID contracts.WorkerID,
	now time.Time,
) (ApplyStartupCleanupRequest, bool, error) {
	if err := validateStartupFacts(facts, currentWorkerID); err != nil {
		return ApplyStartupCleanupRequest{}, false, err
	}
	config, exists := s.configs.LookupAgent(facts.Task.AgentID)
	if !exists || !config.ExecutionConfig.Agent.Enabled || config.ExecutionConfig.Agent.AgentID != facts.Task.AgentID {
		return ApplyStartupCleanupRequest{}, false, errors.New("StartupCleanup validated Agent configuration is unavailable")
	}
	currentHash, err := HashExecutionConfigV1(config.ExecutionConfig)
	if err != nil {
		return ApplyStartupCleanupRequest{}, false, fmt.Errorf("hash StartupCleanup execution config: %w", err)
	}
	if currentHash != facts.Execution.ExecutionConfigHash {
		return ApplyStartupCleanupRequest{}, false, fmt.Errorf("StartupCleanup static config does not match running Execution: %w", ErrPersistenceInvariantViolation)
	}

	base := ApplyStartupCleanupRequest{
		TaskID: facts.Task.TaskID, ExecutionVersion: facts.Execution.ExecutionVersion,
		ExpectedWorkerID:   *facts.Execution.WorkerID,
		ExpectedTaskStatus: facts.Task.Status, ExpectedRunStatus: facts.Run.Status,
		ExpectedExecutionStatus: facts.Execution.Status, EndedAt: now,
	}
	if facts.Step != nil {
		status := facts.Step.Status
		base.ExpectedStepStatus = &status
	}
	if facts.ToolExecution != nil {
		status := facts.ToolExecution.Status
		base.ExpectedToolStatus = &status
	}
	toolKind, err := startupToolKind(facts, config.ExecutionConfig)
	if err != nil {
		return ApplyStartupCleanupRequest{}, false, err
	}
	if !now.Before(facts.Task.DeadlineAt) {
		return timeoutStartupRequest(base, facts, toolKind), true, nil
	}

	checkpointResult, err := s.checkpoints.LoadLatestForStartupCleanup(
		ctx, tx, facts.Task.TaskID, facts.Run.RunID, facts.Execution.ExecutionVersion,
	)
	if err != nil {
		return ApplyStartupCleanupRequest{}, false, fmt.Errorf("load StartupCleanup Checkpoint: %w", err)
	}
	switch result := checkpointResult.(type) {
	case StartupCleanupCheckpointInvalid:
		if !result.ReasonCode.ValidForCheckpointInvalid() {
			return ApplyStartupCleanupRequest{}, false, fmt.Errorf("invalid StartupCleanup Checkpoint reason: %w", ErrPersistenceInvariantViolation)
		}
		return checkpointInvalidStartupRequest(base, result.ReasonCode), true, nil
	case StartupCleanupCheckpointValid:
		if err := validateStartupCheckpoint(facts, result.Checkpoint); err != nil {
			return ApplyStartupCleanupRequest{}, false, err
		}
		reason, err := validateStartupScene(facts, result.Checkpoint, toolKind)
		if err != nil {
			return ApplyStartupCleanupRequest{}, false, err
		}
		if reason != "" {
			return checkpointInvalidStartupRequest(base, reason), true, nil
		}
	default:
		return ApplyStartupCleanupRequest{}, false, fmt.Errorf("unknown StartupCleanup Checkpoint result %T: %w",
			checkpointResult, ErrPersistenceInvariantViolation)
	}
	if toolKind == startupToolWriteRunning {
		return writeToolStartupRequest(base), true, nil
	}
	return interruptStartupRequest(base, toolKind), false, nil
}

type startupToolClassification uint8

const (
	startupToolNone startupToolClassification = iota
	startupToolReadBoundaryBeforeExecution
	startupToolWriteBoundaryBeforeExecution
	startupToolReadRunning
	startupToolWriteRunning
)

func startupToolKind(facts StartupCleanupFacts, config contracts.ExecutionConfigV1) (startupToolClassification, error) {
	if facts.Step == nil || facts.Step.Type != contracts.StepTypeToolCall {
		if facts.ToolExecution != nil {
			return 0, fmt.Errorf("ToolExecution exists without Tool Step: %w", ErrPersistenceInvariantViolation)
		}
		return startupToolNone, nil
	}
	if facts.Step.ToolName == "" {
		return 0, fmt.Errorf("Tool Step has no Tool name: %w", ErrPersistenceInvariantViolation)
	}
	definition, err := startupToolDefinition(config, facts.Step.ToolName)
	if err != nil {
		return 0, err
	}
	readTool := definition.RiskLevel == contracts.RiskLevelLow && definition.ReadOnly
	writeTool := definition.RiskLevel == contracts.RiskLevelHigh && !definition.ReadOnly
	if !readTool && !writeTool {
		return 0, fmt.Errorf("StartupCleanup Tool capability is inconsistent: %w", ErrPersistenceInvariantViolation)
	}
	if facts.ToolExecution == nil {
		if readTool {
			return startupToolReadBoundaryBeforeExecution, nil
		}
		return startupToolWriteBoundaryBeforeExecution, nil
	}
	tool := facts.ToolExecution
	if tool.TaskID != facts.Task.TaskID || tool.StepID != facts.Step.StepID ||
		tool.ExecutionVersion != facts.Execution.ExecutionVersion || tool.ToolName != facts.Step.ToolName ||
		tool.Status != contracts.ToolExecutionStatusRunning || tool.SideEffectUnknown || tool.EndedAt != nil {
		return 0, fmt.Errorf("StartupCleanup ToolExecution facts are inconsistent: %w", ErrPersistenceInvariantViolation)
	}
	if readTool {
		return startupToolReadRunning, nil
	}
	if writeTool {
		return startupToolWriteRunning, nil
	}
	return 0, fmt.Errorf("unreachable StartupCleanup Tool capability: %w", ErrPersistenceInvariantViolation)
}

func startupToolDefinition(config contracts.ExecutionConfigV1, toolName contracts.ToolName) (contracts.ToolDefinitionV1, error) {
	var match *contracts.ToolDefinitionV1
	for index := range config.ToolFramework.Tools {
		tool := &config.ToolFramework.Tools[index]
		if tool.Enabled && tool.Name == toolName {
			if match != nil {
				return contracts.ToolDefinitionV1{}, fmt.Errorf("duplicate StartupCleanup Tool definition: %w", ErrPersistenceInvariantViolation)
			}
			match = tool
		}
	}
	if match == nil {
		return contracts.ToolDefinitionV1{}, fmt.Errorf("StartupCleanup Tool is unavailable: %w", ErrPersistenceInvariantViolation)
	}
	return *match, nil
}

func validateStartupFacts(facts StartupCleanupFacts, currentWorkerID contracts.WorkerID) error {
	if facts.Execution.Status != contracts.TaskExecutionStatusRunning || facts.Execution.WorkerID == nil ||
		*facts.Execution.WorkerID == "" || *facts.Execution.WorkerID == currentWorkerID ||
		facts.Task.TaskID == "" || facts.Task.CurrentExecutionVersion != facts.Execution.ExecutionVersion ||
		facts.Execution.TaskID != facts.Task.TaskID || facts.Run.TaskID != facts.Task.TaskID ||
		facts.Run.RunID != facts.Task.CurrentRunID || facts.Task.Status != contracts.TaskStatusRunning ||
		facts.Task.QueuedAt != nil || facts.Run.Status != contracts.RunStatusRunning || facts.Task.DeadlineAt.IsZero() {
		return fmt.Errorf("StartupCleanup legacy execution facts are inconsistent: %w", ErrPersistenceInvariantViolation)
	}
	if facts.Step != nil && (facts.Step.StepID == "" || facts.Step.Status != contracts.StepStatusRunning ||
		facts.Run.CurrentStepID == nil || *facts.Run.CurrentStepID != facts.Step.StepID) {
		return fmt.Errorf("StartupCleanup current Step facts are inconsistent: %w", ErrPersistenceInvariantViolation)
	}
	if facts.Step == nil && facts.Run.CurrentStepID != nil {
		return fmt.Errorf("StartupCleanup current Step is missing: %w", ErrPersistenceInvariantViolation)
	}
	return nil
}

func validateStartupCheckpoint(facts StartupCleanupFacts, checkpoint RuntimeCheckpoint) error {
	if checkpoint.CheckpointID == "" || checkpoint.TaskID != facts.Task.TaskID || checkpoint.RunID != facts.Run.RunID ||
		checkpoint.ExecutionVersion != facts.Execution.ExecutionVersion || checkpoint.CheckpointSequence <= 0 ||
		checkpoint.ExecutionConfigHash != facts.Execution.ExecutionConfigHash || !checkpoint.NextAction.Valid() {
		return fmt.Errorf("StartupCleanup Checkpoint attribution mismatch: %w", ErrPersistenceInvariantViolation)
	}
	return nil
}

func validateStartupScene(
	facts StartupCleanupFacts,
	checkpoint RuntimeCheckpoint,
	toolKind startupToolClassification,
) (contracts.ReasonCode, error) {
	switch checkpoint.NextAction {
	case contracts.CheckpointNextActionGeneratePlan:
		if facts.Step != nil || facts.ToolExecution != nil || facts.ApprovedRecovery != nil {
			return contracts.ReasonCodeCheckpointFrozenActionMismatch, nil
		}
	case contracts.CheckpointNextActionExecuteStep:
		if facts.Step == nil {
			return "", fmt.Errorf("Step cleanup has no current Step: %w", ErrPersistenceInvariantViolation)
		}
		if checkpoint.ApprovalContext != nil || facts.ApprovedRecovery != nil ||
			(facts.Step.Type == contracts.StepTypeToolCall && toolKind != startupToolReadBoundaryBeforeExecution &&
				toolKind != startupToolReadRunning) {
			return contracts.ReasonCodeCheckpointFrozenActionMismatch, nil
		}
	case contracts.CheckpointNextActionRequestApproval:
		if facts.Step == nil {
			return "", fmt.Errorf("RequestApproval cleanup has no current Step: %w", ErrPersistenceInvariantViolation)
		}
		if facts.Step.Type != contracts.StepTypeToolCall || toolKind != startupToolWriteBoundaryBeforeExecution ||
			facts.ToolExecution != nil || checkpoint.ApprovalContext != nil || facts.ApprovedRecovery != nil {
			return contracts.ReasonCodeCheckpointFrozenActionMismatch, nil
		}
	case contracts.CheckpointNextActionExecuteApprovedTool:
		if facts.Step == nil {
			return "", fmt.Errorf("approved Tool cleanup has no current Step: %w", ErrPersistenceInvariantViolation)
		}
		if facts.Step.Type != contracts.StepTypeToolCall ||
			(toolKind != startupToolWriteBoundaryBeforeExecution && toolKind != startupToolWriteRunning) {
			return contracts.ReasonCodeCheckpointFrozenActionMismatch, nil
		}
		if checkpoint.ApprovalContext == nil || facts.ApprovedRecovery == nil {
			return contracts.ReasonCodeCheckpointApprovalReferenceInvalid, nil
		}
		approval := facts.ApprovedRecovery
		if approval.TaskID != facts.Task.TaskID {
			return "", fmt.Errorf("approved recovery object attribution is inconsistent: %w", ErrPersistenceInvariantViolation)
		}
		actualContext := &contracts.ApprovalContext{
			ApprovalID: approval.ApprovalID, ApprovalExecutionVersion: approval.ApprovalExecutionVersion,
			ToolName: approval.ToolName, FrozenToolInput: approval.FrozenToolInput,
			ObservedValues: approval.ObservedValues, ResourceVersion: approval.ResourceVersion,
			FrozenInputHash: approval.FrozenInputHash,
		}
		if !validApprovalContext(actualContext, facts.Step.ToolName, facts.Execution.ExecutionVersion) {
			return "", fmt.Errorf("approved recovery object is internally inconsistent: %w", ErrPersistenceInvariantViolation)
		}
		if approval.Status != contracts.ApprovalStatusApproved {
			return contracts.ReasonCodeCheckpointApprovalReferenceInvalid, nil
		}
		if !validCheckpointApprovalContext(checkpoint, facts.Step.ToolName) {
			return contracts.ReasonCodeCheckpointApprovalReferenceInvalid, nil
		}
		if checkpoint.ApprovalContext.ApprovalID != approval.ApprovalID ||
			checkpoint.ApprovalContext.ApprovalExecutionVersion != approval.ApprovalExecutionVersion ||
			checkpoint.ApprovalContext.ToolName != approval.ToolName || facts.Step.ToolName != approval.ToolName {
			return contracts.ReasonCodeCheckpointApprovalReferenceInvalid, nil
		}
		if checkpoint.ApprovalContext.FrozenInputHash != approval.FrozenInputHash {
			return contracts.ReasonCodeCheckpointFrozenInputHashMismatch, nil
		}
		if checkpoint.ApprovalContext.ResourceVersion != approval.ResourceVersion ||
			!bytes.Equal(checkpoint.ApprovalContext.FrozenToolInput, approval.FrozenToolInput) ||
			!bytes.Equal(checkpoint.ApprovalContext.ObservedValues, approval.ObservedValues) {
			return contracts.ReasonCodeCheckpointApprovalReferenceInvalid, nil
		}
		if approval.ApprovalConfigHash != approval.SourceExecutionConfigHash ||
			approval.SourceExecutionConfigHash != facts.Execution.ExecutionConfigHash {
			return "", fmt.Errorf("approved recovery static hash is inconsistent: %w", ErrPersistenceInvariantViolation)
		}
	case contracts.CheckpointNextActionFinalizeRun:
		return contracts.ReasonCodeCheckpointFrozenActionMismatch, nil
	default:
		return "", fmt.Errorf("unknown StartupCleanup action: %w", ErrPersistenceInvariantViolation)
	}
	if facts.Step != nil && facts.Step.Type != contracts.StepTypeToolCall && toolKind != startupToolNone {
		return "", fmt.Errorf("non-Tool cleanup classified as Tool: %w", ErrPersistenceInvariantViolation)
	}
	return "", nil
}

func interruptStartupRequest(base ApplyStartupCleanupRequest, toolKind startupToolClassification) ApplyStartupCleanupRequest {
	base.Disposition = StartupCleanupInterrupt
	base.ExecutionErrorCode = contracts.ErrorCodeWorkerInterrupted
	if toolKind == startupToolReadRunning {
		failed := contracts.ToolExecutionStatusFailed
		errorCode := contracts.ErrorCodeWorkerInterrupted
		base.ToolStatus, base.ToolErrorCode = &failed, &errorCode
	}
	return base
}

func writeToolStartupRequest(base ApplyStartupCleanupRequest) ApplyStartupCleanupRequest {
	base.Disposition = StartupCleanupTerminal
	errorCode := contracts.ErrorCodeWriteToolInterrupted
	base.TaskErrorCode, base.StepErrorCode = &errorCode, &errorCode
	base.ExecutionErrorCode = errorCode
	unknown := contracts.ToolExecutionStatusUnknown
	base.ToolStatus, base.ToolSideEffectUnknown = &unknown, true
	return base
}

func timeoutStartupRequest(base ApplyStartupCleanupRequest, facts StartupCleanupFacts,
	toolKind startupToolClassification) ApplyStartupCleanupRequest {
	base.Disposition = StartupCleanupTerminal
	errorCode := contracts.ErrorCodeTaskTimeout
	base.TaskErrorCode, base.StepErrorCode = &errorCode, &errorCode
	reason := contracts.TerminationReasonTimedOut
	base.TerminationReason = &reason
	if toolKind == startupToolWriteRunning {
		base.ExecutionErrorCode = contracts.ErrorCodeWriteToolInterrupted
		unknown := contracts.ToolExecutionStatusUnknown
		base.ToolStatus, base.ToolSideEffectUnknown = &unknown, true
	} else {
		if facts.ToolExecution != nil && toolKind == startupToolReadRunning {
			failed := contracts.ToolExecutionStatusFailed
			base.ToolStatus, base.ToolErrorCode = &failed, &errorCode
		}
	}
	return base
}

func checkpointInvalidStartupRequest(base ApplyStartupCleanupRequest, reason contracts.ReasonCode) ApplyStartupCleanupRequest {
	base.Disposition = StartupCleanupTerminal
	errorCode := contracts.ErrorCodeCheckpointInvalid
	base.TaskErrorCode, base.StepErrorCode = &errorCode, &errorCode
	base.ExecutionErrorCode = errorCode
	base.CheckpointReasonCode = &reason
	return base
}
