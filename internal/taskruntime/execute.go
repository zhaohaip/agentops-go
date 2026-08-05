package taskruntime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/zhaohaip/agentops-go/internal/contracts"
	"github.com/zhaohaip/agentops-go/internal/lifecycle"
	"github.com/zhaohaip/agentops-go/internal/taskruntime/activecall"
)

var errExecutionStale = errors.New("execution dispatch is stale")

// ExecuteTaskService 顺序编排一个已领取 Execution，直到它释放 Worker 执行槽。
type ExecuteTaskService struct {
	executor    contracts.RuntimeWriteExecutor
	dispatch    ExecutionDispatchRepository
	checkpoints RuntimeCheckpointPort
	clock       DatabaseClock
	configs     AgentConfigSource
	planner     PlannerPort
	steps       StepExecutorPort
	activeCalls *activecall.Registry
	policy      lifecycle.Policy
}

// ExecuteTaskDependencies 声明 ExecuteClaimedExecution 的最小出站依赖。
type ExecuteTaskDependencies struct {
	Executor    contracts.RuntimeWriteExecutor
	Dispatch    ExecutionDispatchRepository
	Checkpoints RuntimeCheckpointPort
	Clock       DatabaseClock
	Configs     AgentConfigSource
	Planner     PlannerPort
	Steps       StepExecutorPort
	ActiveCalls *activecall.Registry
	Policy      lifecycle.Policy
}

// NewExecuteTaskService 创建未接入生产组合根的执行编排服务。
func NewExecuteTaskService(dependencies ExecuteTaskDependencies) (*ExecuteTaskService, error) {
	if dependencies.Executor == nil || dependencies.Dispatch == nil || dependencies.Checkpoints == nil ||
		dependencies.Clock == nil || dependencies.Configs == nil || dependencies.Planner == nil ||
		dependencies.Steps == nil || dependencies.ActiveCalls == nil {
		return nil, errors.New("create ExecuteClaimedExecution service: dependencies are required")
	}
	return &ExecuteTaskService{
		executor: dependencies.Executor, dispatch: dependencies.Dispatch, checkpoints: dependencies.Checkpoints,
		clock: dependencies.Clock, configs: dependencies.Configs, planner: dependencies.Planner,
		steps: dependencies.Steps, activeCalls: dependencies.ActiveCalls, policy: dependencies.Policy,
	}, nil
}

// ExecuteClaimedExecution 每轮重载数据库事实，并只消费最大 Checkpoint 冻结的 next_action。
func (s *ExecuteTaskService) ExecuteClaimedExecution(
	ctx context.Context,
	claim contracts.ExecutionClaim,
) (contracts.ExecuteResult, error) {
	if s == nil {
		return nil, errors.New("execute claimed execution: service is not initialized")
	}
	if err := validateExecutionClaim(claim); err != nil {
		return nil, err
	}

	for {
		dispatch, result, err := s.loadDispatch(ctx, claim)
		if err != nil || result != nil {
			return result, err
		}
		switch dispatch.checkpoint.NextAction {
		case contracts.CheckpointNextActionGeneratePlan:
			result, err = s.executePlanner(ctx, claim, dispatch)
		case contracts.CheckpointNextActionExecuteStep,
			contracts.CheckpointNextActionRequestApproval,
			contracts.CheckpointNextActionExecuteApprovedTool:
			result, err = s.executeStep(ctx, claim, dispatch)
		case contracts.CheckpointNextActionFinalizeRun:
			result, err = s.finalize(ctx, claim, dispatch)
		default:
			return nil, fmt.Errorf("dispatch unknown next action %q: %w", dispatch.checkpoint.NextAction, ErrPersistenceInvariantViolation)
		}
		if err != nil || result != nil {
			return result, err
		}
	}
}

type executionDispatch struct {
	facts      ExecutionDispatchFacts
	checkpoint RuntimeCheckpoint
	config     AgentRuntimeConfig
}

func (s *ExecuteTaskService) loadDispatch(
	ctx context.Context,
	claim contracts.ExecutionClaim,
) (executionDispatch, contracts.ExecuteResult, error) {
	var loaded executionDispatch
	var stale bool
	var checkpointInvalid bool
	err := s.executor.Execute(ctx, func(ctx context.Context, tx contracts.RuntimeWriteTx) error {
		facts, err := s.dispatch.LockExecutionDispatch(ctx, tx, claim)
		if err != nil {
			return fmt.Errorf("lock execution dispatch facts: %w", err)
		}
		now, err := s.clock.Now(ctx, tx)
		if err != nil {
			return fmt.Errorf("read execution dispatch database clock: %w", err)
		}
		if !executionClaimOwnsFacts(s.policy, claim, facts, now) {
			stale = true
			return nil
		}
		checkpointResult, err := s.checkpoints.LoadLatestForExecutionDispatch(
			ctx, tx, claim.TaskID, claim.RunID, claim.ExecutionVersion,
		)
		if err != nil {
			return fmt.Errorf("load execution dispatch Checkpoint: %w", err)
		}
		switch result := checkpointResult.(type) {
		case ExecutionCheckpointValid:
			loaded.checkpoint = result.Checkpoint
		case ExecutionCheckpointInvalid:
			if !result.ReasonCode.ValidForCheckpointInvalid() {
				return fmt.Errorf("invalid Checkpoint reason %q: %w", result.ReasonCode, ErrPersistenceInvariantViolation)
			}
			checkpointInvalid = true
			request, requestErr := checkpointInvalidRequest(claim, facts, result.ReasonCode)
			if requestErr != nil {
				return requestErr
			}
			applied, terminalErr := s.dispatch.TerminalizeCheckpointInvalid(
				ctx, tx, request,
			)
			if terminalErr != nil {
				return fmt.Errorf("terminalize invalid execution Checkpoint: %w", terminalErr)
			}
			if !applied {
				stale = true
			}
			return nil
		default:
			return fmt.Errorf("unknown execution Checkpoint result %T: %w", checkpointResult, ErrPersistenceInvariantViolation)
		}
		if err := validateCheckpointAttribution(claim, facts, loaded.checkpoint); err != nil {
			return err
		}
		config, exists := s.configs.LookupAgent(facts.Task.AgentID)
		if !exists || !config.ExecutionConfig.Agent.Enabled || config.ExecutionConfig.Agent.AgentID != facts.Task.AgentID {
			return errors.New("execute claimed execution: validated runtime Agent configuration is unavailable")
		}
		if err := validatePlanningToolCatalogSelector(config); err != nil {
			return err
		}
		currentHash, err := HashExecutionConfigV1(config.ExecutionConfig)
		if err != nil {
			return fmt.Errorf("hash execution dispatch config: %w", err)
		}
		if !facts.Execution.ExecutionConfigHash.Valid() || facts.Execution.ExecutionConfigHash != currentHash ||
			loaded.checkpoint.ExecutionConfigHash != currentHash {
			return fmt.Errorf("execution dispatch configuration hash mismatch: %w", ErrPersistenceInvariantViolation)
		}
		reasonCode, err := validateFrozenDispatch(facts, loaded.checkpoint, config.ExecutionConfig)
		if err != nil {
			return err
		}
		if reasonCode != "" {
			checkpointInvalid = true
			request, requestErr := checkpointInvalidRequest(claim, facts, reasonCode)
			if requestErr != nil {
				return requestErr
			}
			applied, terminalErr := s.dispatch.TerminalizeCheckpointInvalid(ctx, tx, request)
			if terminalErr != nil {
				return fmt.Errorf("terminalize inconsistent frozen dispatch: %w", terminalErr)
			}
			if !applied {
				stale = true
			}
			return nil
		}
		loaded.facts = facts
		loaded.config = config
		return nil
	})
	if err != nil {
		return executionDispatch{}, nil, err
	}
	if stale {
		return executionDispatch{}, contracts.ExecuteResultStale{}, nil
	}
	if checkpointInvalid {
		return executionDispatch{}, contracts.ExecuteResultTerminal{}, nil
	}
	return loaded, nil, nil
}

func (s *ExecuteTaskService) executePlanner(
	ctx context.Context,
	claim contracts.ExecutionClaim,
	dispatch executionDispatch,
) (contracts.ExecuteResult, error) {
	handle, err := s.activeCalls.Prepare(ctx, activeCallKey(claim), activecall.Metadata{
		ActionKind: contracts.CheckpointNextActionGeneratePlan,
	})
	if err != nil {
		return nil, err
	}
	defer handle.Unregister()
	guard := actionGuard(claim, dispatch)
	if admitted, err := s.startAction(ctx, guard); err != nil {
		return nil, err
	} else if !admitted {
		return contracts.ExecuteResultStale{}, nil
	}
	if err := handle.Activate(); err != nil {
		return nil, err
	}
	if handle.Context().Err() != nil {
		return contracts.ExecuteResultStale{}, nil
	}
	outcome, err := s.planner.GeneratePlan(handle.Context(), PlannerRequest{
		TaskID: claim.TaskID, RunID: claim.RunID, ExecutionVersion: claim.ExecutionVersion,
		WorkerID: claim.WorkerID, TaskInput: dispatch.facts.Task.Input, DeadlineAt: dispatch.facts.Task.DeadlineAt,
		ExecutionConfigHash: dispatch.facts.Execution.ExecutionConfigHash,
		ExecutionConfig:     dispatch.config.ExecutionConfig,
		ToolCatalogSelector: dispatch.config.PlanningToolCatalogSelector,
	})
	callCanceled := handle.Context().Err() != nil
	handle.Unregister()
	if err != nil {
		if callCanceled {
			return contracts.ExecuteResultStale{}, nil
		}
		return nil, err
	}
	switch result := outcome.(type) {
	case PlannerOutcomeCompleted:
		if err := validatePlanDraft(result.Draft); err != nil {
			return nil, err
		}
		nextAction, err := nextActionForStep(result.Draft.Steps[0], dispatch.config.ExecutionConfig)
		if err != nil {
			return nil, err
		}
		commitResult, err := s.applyPlanner(ctx, claim, guard, result.Draft, nextAction)
		if err != nil {
			return nil, err
		}
		if commitResult != nil {
			return commitResult, nil
		}
		return nil, nil
	case PlannerOutcomeFailed:
		if !result.ErrorCode.Valid() {
			return nil, fmt.Errorf("Planner returned invalid error code: %w", ErrPersistenceInvariantViolation)
		}
		return s.terminalize(ctx, guard, result.ErrorCode)
	default:
		return nil, fmt.Errorf("Planner returned unknown outcome %T: %w", outcome, ErrPersistenceInvariantViolation)
	}
}

func (s *ExecuteTaskService) executeStep(
	ctx context.Context,
	claim contracts.ExecutionClaim,
	dispatch executionDispatch,
) (contracts.ExecuteResult, error) {
	step := *dispatch.facts.Step
	handle, err := s.activeCalls.Prepare(ctx, activeCallKey(claim), activecall.Metadata{
		ActionKind: dispatch.checkpoint.NextAction, StepID: step.StepID,
	})
	if err != nil {
		return nil, err
	}
	defer handle.Unregister()
	guard := actionGuard(claim, dispatch)
	if admitted, err := s.startAction(ctx, guard); err != nil {
		return nil, err
	} else if !admitted {
		return contracts.ExecuteResultStale{}, nil
	}
	if err := handle.Activate(); err != nil {
		return nil, err
	}
	if handle.Context().Err() != nil {
		return contracts.ExecuteResultStale{}, nil
	}
	scope, err := newExecutionScope(claim, dispatch.facts)
	if err != nil {
		return nil, err
	}
	outcome, err := s.steps.ExecuteStep(handle.Context(), StepExecutionRequest{
		Scope:      scope,
		NextAction: dispatch.checkpoint.NextAction, Step: step,
		ResolvedReferences: dispatch.checkpoint.ResolvedReferences,
		ApprovalContext:    dispatch.checkpoint.ApprovalContext, ExecutionConfig: dispatch.config.ExecutionConfig,
	})
	callCanceled := handle.Context().Err() != nil
	handle.Unregister()
	if err != nil {
		if callCanceled {
			return contracts.ExecuteResultStale{}, nil
		}
		return nil, err
	}
	if err := validateStepOutcome(dispatch.checkpoint.NextAction, outcome); err != nil {
		return nil, err
	}
	switch result := outcome.(type) {
	case StepOutcomeCompleted:
		return s.commitCompletedStep(ctx, claim, guard, result, dispatch.config.ExecutionConfig)
	case StepOutcomeWaitingApproval:
		var confirmed bool
		err := s.executor.Execute(ctx, func(ctx context.Context, tx contracts.RuntimeWriteTx) error {
			var confirmErr error
			confirmed, confirmErr = s.dispatch.ConfirmExecutionWaitingApproval(ctx, tx, claim, step.StepID)
			return confirmErr
		})
		if err != nil {
			return nil, fmt.Errorf("confirm Step waiting-approval result: %w", err)
		}
		if !confirmed {
			return nil, fmt.Errorf("Step Executor reported WaitingApproval without complete database facts: %w", ErrPersistenceInvariantViolation)
		}
		return contracts.ExecuteResultWaitingApproval{}, nil
	case StepOutcomeTerminalized:
		var confirmed bool
		err := s.executor.Execute(ctx, func(ctx context.Context, tx contracts.RuntimeWriteTx) error {
			var confirmErr error
			confirmed, confirmErr = s.dispatch.ConfirmExecutionTerminal(ctx, tx, claim)
			return confirmErr
		})
		if err != nil {
			return nil, fmt.Errorf("confirm Step terminal result: %w", err)
		}
		if !confirmed {
			return nil, fmt.Errorf("Step Executor reported Terminalized without terminal database facts: %w", ErrPersistenceInvariantViolation)
		}
		return contracts.ExecuteResultTerminal{}, nil
	case StepOutcomeFailed:
		if !result.ErrorCode.Valid() {
			return nil, fmt.Errorf("Step Executor returned invalid error code: %w", ErrPersistenceInvariantViolation)
		}
		return s.terminalize(ctx, guard, result.ErrorCode)
	case StepOutcomeStale:
		return contracts.ExecuteResultStale{}, nil
	default:
		return nil, fmt.Errorf("Step Executor returned unknown outcome %T: %w", outcome, ErrPersistenceInvariantViolation)
	}
}

func (s *ExecuteTaskService) commitCompletedStep(
	ctx context.Context,
	claim contracts.ExecutionClaim,
	guard ExecutionActionGuard,
	outcome StepOutcomeCompleted,
	config contracts.ExecutionConfigV1,
) (contracts.ExecuteResult, error) {
	if !outcome.Continuation.Valid() {
		return nil, fmt.Errorf("Step Executor returned invalid continuation: %w", ErrPersistenceInvariantViolation)
	}
	var applied bool
	var dispatchResult contracts.ExecuteResult
	err := s.executor.Execute(ctx, func(ctx context.Context, tx contracts.RuntimeWriteTx) error {
		current, result, err := s.reloadDispatchInTransaction(ctx, tx, claim)
		if err != nil {
			return err
		}
		if result != nil {
			dispatchResult = result
			return nil
		}
		if actionGuard(claim, current) != guard {
			return errExecutionStale
		}
		nextAction := contracts.CheckpointNextActionFinalizeRun
		if outcome.Continuation == contracts.StepContinuationNextStep {
			if outcome.NextStepID == "" {
				return fmt.Errorf("Step continuation is missing next step ID: %w", ErrPersistenceInvariantViolation)
			}
			nextStep, err := s.dispatch.LockStep(ctx, tx, claim, outcome.NextStepID)
			if err != nil {
				return fmt.Errorf("lock next execution Step: %w", err)
			}
			nextAction, err = nextActionForStep(PlanStepDraft{
				StepID: nextStep.StepID, Sequence: nextStep.Sequence, Type: nextStep.Type,
				Input: nextStep.Input, ToolName: nextStep.ToolName,
			}, config)
			if err != nil {
				return err
			}
		} else if outcome.NextStepID != "" {
			return fmt.Errorf("FINALIZE_RUN continuation contains next step ID: %w", ErrPersistenceInvariantViolation)
		}
		request := ApplyStepCompletedRequest{Guard: actionGuard(claim, current), Outcome: outcome, NextAction: nextAction}
		applied, err = s.dispatch.ApplyStepCompleted(ctx, tx, request)
		return err
	})
	if errors.Is(err, errExecutionStale) {
		return contracts.ExecuteResultStale{}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("commit Step result: %w", err)
	}
	if dispatchResult != nil {
		return dispatchResult, nil
	}
	if !applied {
		return contracts.ExecuteResultStale{}, nil
	}
	return nil, nil
}

func (s *ExecuteTaskService) startAction(ctx context.Context, guard ExecutionActionGuard) (bool, error) {
	var admitted bool
	err := s.executor.Execute(ctx, func(ctx context.Context, tx contracts.RuntimeWriteTx) error {
		var err error
		admitted, err = s.dispatch.StartExecutionAction(ctx, tx, guard)
		return err
	})
	if err != nil {
		return false, fmt.Errorf("start execution action: %w", err)
	}
	return admitted, nil
}

func (s *ExecuteTaskService) applyPlanner(
	ctx context.Context,
	claim contracts.ExecutionClaim,
	guard ExecutionActionGuard,
	draft ValidatedPlanDraft,
	nextAction contracts.CheckpointNextAction,
) (contracts.ExecuteResult, error) {
	var applied bool
	var dispatchResult contracts.ExecuteResult
	err := s.executor.Execute(ctx, func(ctx context.Context, tx contracts.RuntimeWriteTx) error {
		current, result, err := s.reloadDispatchInTransaction(ctx, tx, claim)
		if err != nil {
			return err
		}
		if result != nil {
			dispatchResult = result
			return nil
		}
		if actionGuard(claim, current) != guard {
			return errExecutionStale
		}
		applied, err = s.dispatch.ApplyPlannerCompleted(ctx, tx, ApplyPlannerCompletedRequest{
			Guard: guard, Draft: draft, NextAction: nextAction,
		})
		return err
	})
	if errors.Is(err, errExecutionStale) {
		return contracts.ExecuteResultStale{}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("commit Planner result: %w", err)
	}
	if dispatchResult != nil {
		return dispatchResult, nil
	}
	if !applied {
		return contracts.ExecuteResultStale{}, nil
	}
	return nil, nil
}

func (s *ExecuteTaskService) terminalize(
	ctx context.Context,
	guard ExecutionActionGuard,
	errorCode contracts.ErrorCode,
) (contracts.ExecuteResult, error) {
	var applied bool
	err := s.executor.Execute(ctx, func(ctx context.Context, tx contracts.RuntimeWriteTx) error {
		var err error
		applied, err = s.dispatch.TerminalizeExecution(ctx, tx, guard, errorCode)
		return err
	})
	if err != nil {
		return nil, fmt.Errorf("terminalize execution: %w", err)
	}
	if !applied {
		return contracts.ExecuteResultStale{}, nil
	}
	return contracts.ExecuteResultTerminal{}, nil
}

func (s *ExecuteTaskService) finalize(
	ctx context.Context,
	claim contracts.ExecutionClaim,
	dispatch executionDispatch,
) (contracts.ExecuteResult, error) {
	var applied bool
	var dispatchResult contracts.ExecuteResult
	err := s.executor.Execute(ctx, func(ctx context.Context, tx contracts.RuntimeWriteTx) error {
		current, result, err := s.reloadDispatchInTransaction(ctx, tx, claim)
		if err != nil {
			return err
		}
		if result != nil {
			dispatchResult = result
			return nil
		}
		applied, err = s.dispatch.FinalizeExecution(ctx, tx, actionGuard(claim, current))
		return err
	})
	if errors.Is(err, errExecutionStale) {
		return contracts.ExecuteResultStale{}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("finalize execution: %w", err)
	}
	if dispatchResult != nil {
		return dispatchResult, nil
	}
	if !applied {
		return contracts.ExecuteResultStale{}, nil
	}
	return contracts.ExecuteResultTerminal{}, nil
}

func (s *ExecuteTaskService) reloadDispatchInTransaction(
	ctx context.Context,
	tx contracts.RuntimeWriteTx,
	claim contracts.ExecutionClaim,
) (executionDispatch, contracts.ExecuteResult, error) {
	facts, err := s.dispatch.LockExecutionDispatch(ctx, tx, claim)
	if err != nil {
		return executionDispatch{}, nil, fmt.Errorf("reload execution dispatch facts: %w", err)
	}
	now, err := s.clock.Now(ctx, tx)
	if err != nil {
		return executionDispatch{}, nil, fmt.Errorf("reload execution dispatch clock: %w", err)
	}
	if !executionClaimOwnsFacts(s.policy, claim, facts, now) {
		return executionDispatch{}, contracts.ExecuteResultStale{}, nil
	}
	checkpointResult, err := s.checkpoints.LoadLatestForExecutionDispatch(
		ctx, tx, claim.TaskID, claim.RunID, claim.ExecutionVersion,
	)
	if err != nil {
		return executionDispatch{}, nil, fmt.Errorf("reload execution dispatch Checkpoint: %w", err)
	}
	switch result := checkpointResult.(type) {
	case ExecutionCheckpointValid:
		if err := validateCheckpointAttribution(claim, facts, result.Checkpoint); err != nil {
			return executionDispatch{}, nil, err
		}
		return executionDispatch{facts: facts, checkpoint: result.Checkpoint}, nil, nil
	case ExecutionCheckpointInvalid:
		if !result.ReasonCode.ValidForCheckpointInvalid() {
			return executionDispatch{}, nil, fmt.Errorf("invalid Checkpoint reason %q: %w", result.ReasonCode, ErrPersistenceInvariantViolation)
		}
		request, requestErr := checkpointInvalidRequest(claim, facts, result.ReasonCode)
		if requestErr != nil {
			return executionDispatch{}, nil, requestErr
		}
		applied, terminalErr := s.dispatch.TerminalizeCheckpointInvalid(
			ctx, tx, request,
		)
		if terminalErr != nil {
			return executionDispatch{}, nil, fmt.Errorf("terminalize invalid result Checkpoint: %w", terminalErr)
		}
		if !applied {
			return executionDispatch{}, contracts.ExecuteResultStale{}, nil
		}
		return executionDispatch{}, contracts.ExecuteResultTerminal{}, nil
	default:
		return executionDispatch{}, nil, fmt.Errorf("unknown execution Checkpoint result %T: %w", checkpointResult, ErrPersistenceInvariantViolation)
	}
}

func validateExecutionClaim(claim contracts.ExecutionClaim) error {
	if claim.TaskID == "" || claim.RunID == "" || !claim.ExecutionVersion.Valid() || claim.WorkerID == "" {
		return ErrInvalidArgument
	}
	return nil
}

func executionClaimOwnsFacts(
	policy lifecycle.Policy,
	claim contracts.ExecutionClaim,
	facts ExecutionDispatchFacts,
	now time.Time,
) bool {
	return facts.Task.TaskID == claim.TaskID && facts.Task.CurrentRunID == claim.RunID &&
		facts.Run.TaskID == claim.TaskID && facts.Run.RunID == claim.RunID &&
		facts.Execution.TaskID == claim.TaskID && facts.Execution.ExecutionVersion == claim.ExecutionVersion &&
		facts.Task.QueuedAt == nil && facts.Task.Status == contracts.TaskStatusRunning &&
		facts.Run.Status == contracts.RunStatusRunning && facts.Execution.Status == contracts.TaskExecutionStatusRunning &&
		policy.CheckGuard(lifecycle.GuardFacts{
			CurrentExecutionVersion: facts.Task.CurrentExecutionVersion,
			RequestExecutionVersion: claim.ExecutionVersion,
			ExecutionWorkerID:       facts.Execution.WorkerID, RequestWorkerID: &claim.WorkerID,
			DeadlineAt: facts.Task.DeadlineAt, DatabaseNow: now,
		}).Allowed
}

func validateCheckpointAttribution(
	claim contracts.ExecutionClaim,
	facts ExecutionDispatchFacts,
	checkpoint RuntimeCheckpoint,
) error {
	if checkpoint.CheckpointID == "" || checkpoint.TaskID != claim.TaskID || checkpoint.RunID != claim.RunID ||
		checkpoint.ExecutionVersion != claim.ExecutionVersion || checkpoint.CheckpointSequence <= 0 ||
		!checkpoint.NextAction.Valid() || checkpoint.ExecutionConfigHash != facts.Execution.ExecutionConfigHash {
		return fmt.Errorf("execution Checkpoint attribution mismatch: %w", ErrPersistenceInvariantViolation)
	}
	return nil
}

func validateFrozenDispatch(
	facts ExecutionDispatchFacts,
	checkpoint RuntimeCheckpoint,
	config contracts.ExecutionConfigV1,
) (contracts.ReasonCode, error) {
	if facts.Plan == nil {
		if checkpoint.NextAction != contracts.CheckpointNextActionGeneratePlan || facts.Step != nil || checkpoint.ApprovalContext != nil {
			return contracts.ReasonCodeCheckpointFrozenActionMismatch, nil
		}
		return "", nil
	}
	if facts.Run.PlanID == nil || *facts.Run.PlanID != facts.Plan.PlanID || checkpoint.NextAction == contracts.CheckpointNextActionGeneratePlan {
		return contracts.ReasonCodeCheckpointFrozenActionMismatch, nil
	}
	if checkpoint.NextAction == contracts.CheckpointNextActionFinalizeRun {
		if checkpoint.ApprovalContext != nil {
			return contracts.ReasonCodeCheckpointApprovalReferenceInvalid, nil
		}
		return "", nil
	}
	if facts.Step == nil || facts.Run.CurrentStepID == nil || *facts.Run.CurrentStepID != facts.Step.StepID {
		return contracts.ReasonCodeCheckpointStepReferenceInvalid, nil
	}
	step := *facts.Step
	if step.Type != contracts.StepTypeToolCall {
		if checkpoint.NextAction != contracts.CheckpointNextActionExecuteStep || step.ToolName != "" {
			return contracts.ReasonCodeCheckpointFrozenActionMismatch, nil
		}
		if checkpoint.ApprovalContext != nil {
			return contracts.ReasonCodeCheckpointApprovalReferenceInvalid, nil
		}
		return "", nil
	}
	var definition *contracts.ToolDefinitionV1
	for index := range config.ToolFramework.Tools {
		tool := &config.ToolFramework.Tools[index]
		if tool.Name == step.ToolName && tool.Enabled {
			if definition != nil {
				return "", fmt.Errorf("duplicate enabled Tool definition %q: %w", step.ToolName, ErrPersistenceInvariantViolation)
			}
			definition = tool
		}
	}
	if definition == nil {
		return contracts.ReasonCodeCheckpointFrozenActionMismatch, nil
	}
	switch {
	case definition.RiskLevel == contracts.RiskLevelLow && definition.ReadOnly:
		if checkpoint.NextAction != contracts.CheckpointNextActionExecuteStep {
			return contracts.ReasonCodeCheckpointFrozenActionMismatch, nil
		}
		if checkpoint.ApprovalContext != nil {
			return contracts.ReasonCodeCheckpointApprovalReferenceInvalid, nil
		}
		return "", nil
	case definition.RiskLevel == contracts.RiskLevelHigh && !definition.ReadOnly:
		switch checkpoint.NextAction {
		case contracts.CheckpointNextActionRequestApproval:
			if checkpoint.ApprovalContext != nil {
				return contracts.ReasonCodeCheckpointApprovalReferenceInvalid, nil
			}
			return "", nil
		case contracts.CheckpointNextActionExecuteApprovedTool:
			if !validApprovalContext(checkpoint.ApprovalContext, step.ToolName, facts.Execution.ExecutionVersion) {
				return contracts.ReasonCodeCheckpointApprovalReferenceInvalid, nil
			}
			return "", nil
		default:
			return contracts.ReasonCodeCheckpointFrozenActionMismatch, nil
		}
	default:
		return "", fmt.Errorf("Tool capability %q has no supported frozen action: %w", step.ToolName, ErrPersistenceInvariantViolation)
	}
}

func validApprovalContext(
	approval *contracts.ApprovalContext,
	toolName contracts.ToolName,
	executionVersion contracts.ExecutionVersion,
) bool {
	return approval != nil && approval.ApprovalID != "" && approval.ApprovalExecutionVersion.Valid() &&
		approval.ApprovalExecutionVersion <= executionVersion && approval.ToolName == toolName &&
		approval.ResourceVersion != "" && approval.FrozenInputHash.Valid() &&
		json.Valid(approval.FrozenToolInput) && json.Valid(approval.ObservedValues)
}

func validateStepOutcome(action contracts.CheckpointNextAction, outcome StepOutcome) error {
	valid := false
	switch action {
	case contracts.CheckpointNextActionExecuteStep, contracts.CheckpointNextActionExecuteApprovedTool:
		switch outcome.(type) {
		case StepOutcomeCompleted, StepOutcomeFailed, StepOutcomeStale:
			valid = true
		}
	case contracts.CheckpointNextActionRequestApproval:
		switch outcome.(type) {
		case StepOutcomeWaitingApproval, StepOutcomeTerminalized, StepOutcomeFailed, StepOutcomeStale:
			valid = true
		}
	}
	if !valid {
		return fmt.Errorf("Step Executor outcome %T is invalid for frozen action %s: %w", outcome, action, ErrPersistenceInvariantViolation)
	}
	return nil
}

func validatePlanDraft(draft ValidatedPlanDraft) error {
	if draft.PlanID == "" || len(draft.Steps) == 0 {
		return fmt.Errorf("Planner returned incomplete PlanDraft: %w", ErrPersistenceInvariantViolation)
	}
	for index, step := range draft.Steps {
		if step.StepID == "" || step.Sequence != uint32(index+1) || !step.Type.Valid() {
			return fmt.Errorf("Planner returned invalid Plan Step: %w", ErrPersistenceInvariantViolation)
		}
	}
	return nil
}

func validatePlanningToolCatalogSelector(config AgentRuntimeConfig) error {
	selector := config.PlanningToolCatalogSelector
	if selector.CatalogID == "" || selector.ExpectedRegistryVersion == "" ||
		!selector.ExpectedSnapshotHash.Valid() || selector.AllowedTools == nil ||
		len(selector.AllowedTools) != len(config.ExecutionConfig.Agent.AllowedTools) {
		return fmt.Errorf("invalid frozen Planning Tool Catalog selector: %w", ErrPersistenceInvariantViolation)
	}
	for index, tool := range config.ExecutionConfig.Agent.AllowedTools {
		if selector.AllowedTools[index] != string(tool) {
			return fmt.Errorf("Planning Tool Catalog selector does not match Agent allowed tools: %w", ErrPersistenceInvariantViolation)
		}
	}
	return nil
}

func newExecutionScope(
	claim contracts.ExecutionClaim,
	facts ExecutionDispatchFacts,
) (contracts.ExecutionScope, error) {
	if facts.Step == nil || facts.Step.StepID == "" || !facts.Execution.ExecutionConfigHash.Valid() ||
		facts.Task.DeadlineAt.IsZero() || facts.Task.TaskID != claim.TaskID || facts.Run.RunID != claim.RunID ||
		facts.Execution.ExecutionVersion != claim.ExecutionVersion || facts.Execution.WorkerID == nil ||
		*facts.Execution.WorkerID != claim.WorkerID {
		return contracts.ExecutionScope{}, fmt.Errorf("construct ExecutionScope from invalid facts: %w", ErrPersistenceInvariantViolation)
	}
	return contracts.ExecutionScope{
		TaskID: claim.TaskID, RunID: claim.RunID, ExecutionVersion: claim.ExecutionVersion,
		ExecutionConfigHash: facts.Execution.ExecutionConfigHash, WorkerID: claim.WorkerID,
		StepID: facts.Step.StepID, DeadlineAt: facts.Task.DeadlineAt,
	}, nil
}

func nextActionForStep(step PlanStepDraft, config contracts.ExecutionConfigV1) (contracts.CheckpointNextAction, error) {
	if step.Type != contracts.StepTypeToolCall {
		return contracts.CheckpointNextActionExecuteStep, nil
	}
	for _, tool := range config.ToolFramework.Tools {
		if tool.Name != step.ToolName || !tool.Enabled {
			continue
		}
		switch {
		case tool.RiskLevel == contracts.RiskLevelLow && tool.ReadOnly:
			return contracts.CheckpointNextActionExecuteStep, nil
		case tool.RiskLevel == contracts.RiskLevelHigh && !tool.ReadOnly:
			return contracts.CheckpointNextActionRequestApproval, nil
		default:
			return "", fmt.Errorf("Tool capability has no frozen next action: %w", ErrPersistenceInvariantViolation)
		}
	}
	return "", fmt.Errorf("Plan Step references unavailable Tool: %w", ErrPersistenceInvariantViolation)
}

func actionGuard(claim contracts.ExecutionClaim, dispatch executionDispatch) ExecutionActionGuard {
	guard := ExecutionActionGuard{
		Claim: claim, CheckpointID: dispatch.checkpoint.CheckpointID, NextAction: dispatch.checkpoint.NextAction,
	}
	if dispatch.facts.Step != nil {
		guard.StepID = dispatch.facts.Step.StepID
	}
	return guard
}

func activeCallKey(claim contracts.ExecutionClaim) activecall.Key {
	return activecall.Key{TaskID: claim.TaskID, ExecutionVersion: claim.ExecutionVersion, WorkerID: claim.WorkerID}
}

func checkpointInvalidRequest(
	claim contracts.ExecutionClaim,
	facts ExecutionDispatchFacts,
	reasonCode contracts.ReasonCode,
) (TerminalizeCheckpointInvalidRequest, error) {
	request := TerminalizeCheckpointInvalidRequest{Claim: claim, ReasonCode: reasonCode}
	if facts.Run.CurrentStepID == nil {
		if facts.Step != nil {
			return TerminalizeCheckpointInvalidRequest{}, fmt.Errorf(
				"CheckpointInvalid Step exists without Run current Step: %w", ErrPersistenceInvariantViolation,
			)
		}
		return request, nil
	}
	if facts.Step == nil || facts.Step.StepID != *facts.Run.CurrentStepID {
		return TerminalizeCheckpointInvalidRequest{}, fmt.Errorf(
			"CheckpointInvalid active Step cannot be determined: %w", ErrPersistenceInvariantViolation,
		)
	}
	if facts.Step.Status == contracts.StepStatusRunning {
		stepID := facts.Step.StepID
		request.ActiveStepID = &stepID
	}
	return request, nil
}
