package checkpoint

import (
	"context"
	"errors"
	"fmt"
	"reflect"

	"github.com/zhaohaip/agentops-go/internal/contracts"
)

// RecoverySourcePhase 是 Task Runtime 已完成前置安全 Guard 后选择的封闭来源阶段。
type RecoverySourcePhase uint8

const (
	// RecoverySourceBeforeFirstExecution 表示该版本尚未成功领取。
	RecoverySourceBeforeFirstExecution RecoverySourcePhase = iota + 1
	// RecoverySourceStartedExecution 表示该版本已经进入执行阶段。
	RecoverySourceStartedExecution
)

// RecoverySourceQuery 固定恢复来源的旧版本范围和阶段。
type RecoverySourceQuery struct {
	TaskID                 contracts.TaskID
	RunID                  contracts.RunID
	SourceExecutionVersion contracts.ExecutionVersion
	Phase                  RecoverySourcePhase
}

// RecoverySourceResult 是恢复来源校验的封闭结果。
type RecoverySourceResult interface {
	isRecoverySourceResult()
}

// ValidatedRecoverySource 是仅可由 Manager 在当前事务内产生的不可伪造恢复能力。
type ValidatedRecoverySource struct {
	capability *validatedRecoveryCapability
}

func (ValidatedRecoverySource) isRecoverySourceResult() {}

// SourcePhase 返回经持久化矩阵验证的来源阶段。
func (s ValidatedRecoverySource) SourcePhase() RecoverySourcePhase {
	if s.capability == nil {
		return 0
	}
	return s.capability.phase
}

// SourceNextAction 返回来源冻结动作。
func (s ValidatedRecoverySource) SourceNextAction() contracts.CheckpointNextAction {
	if s.capability == nil {
		return ""
	}
	return s.capability.view.Context.NextAction
}

// SourceCheckpointID 返回已经通过恢复矩阵验证的最大来源 Checkpoint。
func (s ValidatedRecoverySource) SourceCheckpointID() contracts.CheckpointID {
	if s.capability == nil {
		return ""
	}
	return s.capability.view.CheckpointID
}

// SourceExecutionConfigHash 返回最大来源 Checkpoint 的不可变执行配置 Hash。
func (s ValidatedRecoverySource) SourceExecutionConfigHash() contracts.ExecutionConfigHash {
	if s.capability == nil {
		return ""
	}
	return s.capability.view.ExecutionConfigHash
}

// RecoverySourceInvalid 表示来源可安全归属，但最大 Checkpoint 不可恢复。
type RecoverySourceInvalid struct {
	CheckpointInvalid
}

func (RecoverySourceInvalid) isRecoverySourceResult() {}

// RecoverySourceInvariantViolation 表示核心归属无法安全确定。
type RecoverySourceInvariantViolation struct {
	PersistenceInvariantViolation
}

func (RecoverySourceInvariantViolation) isRecoverySourceResult() {}

// RuntimeRecoveryStartRequest 只能携带 Manager 产生的能力，不能注入来源 Context。
type RuntimeRecoveryStartRequest struct {
	TaskID              contracts.TaskID
	RunID               contracts.RunID
	NewExecutionVersion contracts.ExecutionVersion
	ExecutionConfigHash contracts.ExecutionConfigHash
	ValidatedSource     ValidatedRecoverySource
}

type validatedRecoveryCapability struct {
	manager *Manager
	tx      contracts.RuntimeWriteTx
	view    View
	typeOf  InferredType
	phase   RecoverySourcePhase
}

// ValidateRecoverySource 只选择旧版本最大 Checkpoint，并按封闭恢复矩阵签发同事务能力。
func (m *Manager) ValidateRecoverySource(ctx context.Context, tx contracts.RuntimeWriteTx, query RecoverySourceQuery) (RecoverySourceResult, error) {
	if err := validateRecoveryQuery(query); err != nil {
		return nil, fmt.Errorf("validate Recovery source: %w", err)
	}
	if err := m.repository.VerifyRunAttribution(ctx, tx, query.TaskID, query.RunID); err != nil {
		if errors.Is(err, ErrPersistenceInvariantViolation) {
			return RecoverySourceInvariantViolation{persistenceInvariantResult()}, nil
		}
		return nil, fmt.Errorf("validate Recovery source attribution: %w", err)
	}
	entity, err := m.repository.FindLatestByExecutionVersion(ctx, tx, query.TaskID, query.RunID, query.SourceExecutionVersion)
	if errors.Is(err, ErrRepositoryNotFound) {
		return recoveryInvalid(Entity{TaskID: query.TaskID, RunID: query.RunID, ExecutionVersion: query.SourceExecutionVersion}, contracts.ReasonCodeCheckpointNotFound), nil
	}
	if err != nil {
		return nil, fmt.Errorf("select Recovery source: %w", err)
	}
	decoded, err := m.codec.Decode(entity.RuntimeContext)
	if err != nil {
		return recoveryInvalid(entity, codecReasonCode(err)), nil
	}
	if decoded.TaskID != entity.TaskID || decoded.RunID != entity.RunID || decoded.ExecutionVersion != entity.ExecutionVersion {
		return recoveryInvalid(entity, contracts.ReasonCodeCheckpointAttributionMismatch), nil
	}
	facts, err := m.loadFacts(ctx, tx, RuntimeCheckpointQuery{TaskID: query.TaskID, RunID: query.RunID, ExecutionVersion: query.SourceExecutionVersion}, decoded)
	if err != nil {
		if errors.Is(err, ErrPersistenceInvariantViolation) {
			return RecoverySourceInvariantViolation{persistenceInvariantResult()}, nil
		}
		return nil, fmt.Errorf("load Recovery ValidationFacts: %w", err)
	}
	if !coreAttributionValid(decoded, facts) || facts.Task.CurrentExecutionVersion != query.SourceExecutionVersion {
		return RecoverySourceInvariantViolation{persistenceInvariantResult()}, nil
	}
	if entity.ExecutionConfigHash != facts.Execution.ExecutionConfigHash {
		return recoveryInvalid(entity, contracts.ReasonCodeCheckpointExecutionHashMismatch), nil
	}
	inferredType, ok := inferCheckpointType(entity, decoded)
	if !ok {
		return recoveryInvalid(entity, contracts.ReasonCodeCheckpointTypeAmbiguous), nil
	}
	if inferredType == InferredTypeRecoveryStart {
		valid, err := m.directRecoverySourceValid(ctx, tx, entity)
		if err != nil {
			return nil, fmt.Errorf("validate direct Recovery source: %w", err)
		}
		if !valid {
			return recoveryInvalid(entity, contracts.ReasonCodeCheckpointSourceInvalid), nil
		}
	}
	reason, invariant := m.validateRecoveryContext(decoded, facts, inferredType, query.Phase)
	if invariant {
		return RecoverySourceInvariantViolation{persistenceInvariantResult()}, nil
	}
	if reason != "" {
		return recoveryInvalid(entity, reason), nil
	}
	return ValidatedRecoverySource{capability: &validatedRecoveryCapability{
		manager: m, tx: tx, view: View{Entity: entity, Context: decoded}, typeOf: inferredType, phase: query.Phase,
	}}, nil
}

// CreateRecoveryStart 从同一事务能力复制自包含 Runtime Context，并创建严格下一版本起点。
func (m *Manager) CreateRecoveryStart(ctx context.Context, tx contracts.RuntimeWriteTx, request RuntimeRecoveryStartRequest) (Ref, error) {
	capability := request.ValidatedSource.capability
	if capability == nil || capability.manager != m || !sameRuntimeWriteTx(capability.tx, tx) || request.TaskID == "" || request.RunID == "" ||
		!request.NewExecutionVersion.Valid() || !request.ExecutionConfigHash.Valid() {
		return Ref{}, fmt.Errorf("create Recovery Start: %w: invalid capability or request", ErrInvalidDraft)
	}
	source := capability.view
	if source.TaskID != request.TaskID || source.RunID != request.RunID ||
		request.NewExecutionVersion != source.ExecutionVersion+1 || request.ExecutionConfigHash != source.ExecutionConfigHash {
		return Ref{}, fmt.Errorf("create Recovery Start: %w: source identity, version, or hash mismatch", ErrInvalidDraft)
	}
	latest, err := m.repository.FindLatestByExecutionVersion(ctx, tx, source.TaskID, source.RunID, source.ExecutionVersion)
	if err != nil {
		return Ref{}, fmt.Errorf("create Recovery Start: reload source: %w", err)
	}
	if latest.CheckpointID != source.CheckpointID || latest.CheckpointSequence != source.CheckpointSequence {
		return Ref{}, fmt.Errorf("create Recovery Start: %w: source is no longer latest", ErrInvalidDraft)
	}
	newExecution, err := m.repository.LoadTaskExecution(ctx, tx, request.TaskID, request.NewExecutionVersion)
	if err != nil {
		return Ref{}, fmt.Errorf("create Recovery Start: load new TaskExecution: %w", err)
	}
	if newExecution.ExecutionConfigHash != request.ExecutionConfigHash {
		return Ref{}, fmt.Errorf("create Recovery Start: %w: new execution hash mismatch", ErrPersistenceInvariantViolation)
	}
	runtimeContext := source.Context
	runtimeContext.ExecutionVersion = request.NewExecutionVersion
	runtimeContext.ResolvedReferences = cloneReferences(source.Context.ResolvedReferences)
	if source.Context.ApprovalContext != nil {
		approval := *source.Context.ApprovalContext
		approval.FrozenToolInput = append(contracts.FrozenToolInput(nil), source.Context.ApprovalContext.FrozenToolInput...)
		approval.ObservedValues = append(contracts.ObservedValues(nil), source.Context.ApprovalContext.ObservedValues...)
		runtimeContext.ApprovalContext = &approval
	}
	newFacts, err := m.loadFacts(ctx, tx, RuntimeCheckpointQuery{
		TaskID: request.TaskID, RunID: request.RunID, ExecutionVersion: request.NewExecutionVersion,
	}, runtimeContext)
	if err != nil {
		return Ref{}, fmt.Errorf("create Recovery Start: load new ValidationFacts: %w", err)
	}
	if !coreAttributionValid(runtimeContext, newFacts) || newFacts.Task.CurrentExecutionVersion != request.NewExecutionVersion {
		return Ref{}, fmt.Errorf("create Recovery Start: %w: new version attribution mismatch", ErrPersistenceInvariantViolation)
	}
	if reason, invariant := m.validateContextWithApproval(runtimeContext, newFacts, usageClaimContinuation, true); invariant {
		return Ref{}, fmt.Errorf("create Recovery Start: %w", ErrPersistenceInvariantViolation)
	} else if reason != "" {
		return Ref{}, fmt.Errorf("create Recovery Start: %w: %s", ErrInvalidDraft, reason)
	}
	encoded, err := m.codec.Encode(runtimeContext)
	if err != nil {
		return Ref{}, fmt.Errorf("create Recovery Start: encode Context: %w", err)
	}
	sequence, err := m.repository.AllocateNextSequence(ctx, tx, request.RunID)
	if err != nil {
		return Ref{}, fmt.Errorf("create Recovery Start: allocate sequence: %w", err)
	}
	checkpointID, err := newCheckpointID()
	if err != nil {
		return Ref{}, fmt.Errorf("create Recovery Start: generate ID: %w", err)
	}
	sourceVersion := source.ExecutionVersion
	sourceCheckpointID := source.CheckpointID
	entity := Entity{
		CheckpointID: checkpointID, TaskID: request.TaskID, RunID: request.RunID,
		ExecutionVersion: request.NewExecutionVersion, CheckpointSequence: sequence,
		RuntimeContext: encoded, ExecutionConfigHash: request.ExecutionConfigHash,
		SourceExecutionVersion: &sourceVersion, SourceCheckpointID: &sourceCheckpointID,
	}
	createdAt, err := m.repository.InsertCheckpoint(ctx, tx, entity)
	if err != nil {
		return Ref{}, fmt.Errorf("create Recovery Start: insert: %w", err)
	}
	return Ref{CheckpointID: checkpointID, CheckpointSequence: sequence, CreatedAt: createdAt.UTC()}, nil
}

func (m *Manager) validateRecoveryContext(runtimeContext contracts.RuntimeContextV1, facts ValidationFacts, inferredType InferredType, phase RecoverySourcePhase) (contracts.ReasonCode, bool) {
	if reason := validatePlanAndStep(runtimeContext, facts); reason != "" {
		return reason, false
	}
	if reason := m.validateReferences(runtimeContext, facts); reason != "" {
		return reason, false
	}
	approvalValidator := validateApproval
	if inferredType == InferredTypeRecoveryStart {
		approvalValidator = validateApprovalForRecovery
	}
	if reason, invariant := approvalValidator(runtimeContext, facts); reason != "" || invariant {
		return reason, invariant
	}
	return validateRecoveryMatrix(runtimeContext, facts, inferredType, phase), false
}

func validateRecoveryMatrix(runtimeContext contracts.RuntimeContextV1, facts ValidationFacts, inferredType InferredType, phase RecoverySourcePhase) contracts.ReasonCode {
	if facts.Task.QueuedAt != nil || facts.Execution.Status != contracts.TaskExecutionStatusInterrupted || facts.Execution.ErrorCode == nil {
		return contracts.ReasonCodeCheckpointNextActionInvalid
	}
	errorCode := *facts.Execution.ErrorCode
	switch phase {
	case RecoverySourceBeforeFirstExecution:
		if errorCode != contracts.ErrorCodeConfigVersionMismatch || facts.Task.Status != contracts.TaskStatusInterrupted ||
			facts.Task.ErrorCode == nil || *facts.Task.ErrorCode != contracts.ErrorCodeConfigVersionMismatch ||
			facts.Execution.ObservedConfigHash == nil || facts.Execution.StartedAt != nil ||
			facts.Run.Status != contracts.RunStatusPending || runtimeContext.NextAction != contracts.CheckpointNextActionGeneratePlan ||
			facts.Plan != nil || facts.Step != nil || facts.Approval != nil || facts.ToolExecution != nil ||
			inferredType != InferredTypeInitialization && inferredType != InferredTypeRecoveryStart {
			return contracts.ReasonCodeCheckpointNextActionInvalid
		}
	case RecoverySourceStartedExecution:
		if facts.Run.Status != contracts.RunStatusRunning || inferredType == InferredTypeInitialization {
			return contracts.ReasonCodeCheckpointNextActionInvalid
		}
		if errorCode == contracts.ErrorCodeConfigVersionMismatch {
			if facts.Task.Status != contracts.TaskStatusInterrupted ||
				facts.Task.ErrorCode == nil || *facts.Task.ErrorCode != contracts.ErrorCodeConfigVersionMismatch ||
				facts.Execution.ObservedConfigHash == nil ||
				!validConfigMismatchStartedAt(runtimeContext, facts, inferredType) {
				return contracts.ReasonCodeCheckpointNextActionInvalid
			}
		} else if errorCode != contracts.ErrorCodeWorkerInterrupted && errorCode != contracts.ErrorCodeResultPersistenceFailed ||
			facts.Task.Status != contracts.TaskStatusRunning || facts.Execution.StartedAt == nil {
			return contracts.ReasonCodeCheckpointNextActionInvalid
		}
		if reason := validateRecoveryActionConsequence(runtimeContext, facts, errorCode); reason != "" {
			return reason
		}
	default:
		return contracts.ReasonCodeCheckpointNextActionInvalid
	}
	return ""
}

func validConfigMismatchStartedAt(runtimeContext contracts.RuntimeContextV1, facts ValidationFacts, inferredType InferredType) bool {
	switch {
	case inferredType == InferredTypeRecoveryStart:
		return facts.Execution.StartedAt == nil
	case inferredType == InferredTypeExecution && runtimeContext.NextAction == contracts.CheckpointNextActionExecuteApprovedTool:
		return facts.Execution.StartedAt != nil
	default:
		return false
	}
}

func validateRecoveryActionConsequence(runtimeContext contracts.RuntimeContextV1, facts ValidationFacts, errorCode contracts.ErrorCode) contracts.ReasonCode {
	tool := facts.ToolExecution
	if tool != nil && (!toolExecutionBelongs(runtimeContext, facts) || tool.Status == contracts.ToolExecutionStatusUnknown || tool.SideEffectUnknown || tool.Status == contracts.ToolExecutionStatusRunning) {
		return contracts.ReasonCodeCheckpointNextActionInvalid
	}
	if errorCode == contracts.ErrorCodeConfigVersionMismatch && tool != nil {
		return contracts.ReasonCodeCheckpointNextActionInvalid
	}
	switch runtimeContext.NextAction {
	case contracts.CheckpointNextActionGeneratePlan:
		if facts.Plan != nil || facts.Step != nil || facts.Approval != nil || tool != nil {
			return contracts.ReasonCodeCheckpointNextActionInvalid
		}
	case contracts.CheckpointNextActionExecuteStep:
		if facts.Step == nil || facts.Step.Status != contracts.StepStatusRunning || facts.Approval != nil {
			return contracts.ReasonCodeCheckpointNextActionInvalid
		}
		if tool != nil {
			if facts.Step.Type != contracts.StepTypeToolCall {
				return contracts.ReasonCodeCheckpointNextActionInvalid
			}
			if tool.Status != contracts.ToolExecutionStatusFailed || tool.ErrorCode == nil ||
				errorCode == contracts.ErrorCodeWorkerInterrupted && *tool.ErrorCode != contracts.ErrorCodeWorkerInterrupted ||
				errorCode == contracts.ErrorCodeResultPersistenceFailed && *tool.ErrorCode != contracts.ErrorCodeResultPersistenceFailed {
				return contracts.ReasonCodeCheckpointNextActionInvalid
			}
		} else if errorCode == contracts.ErrorCodeResultPersistenceFailed && facts.Step.Type == contracts.StepTypeToolCall {
			return contracts.ReasonCodeCheckpointNextActionInvalid
		}
	case contracts.CheckpointNextActionRequestApproval:
		if facts.Step == nil || facts.Step.Type != contracts.StepTypeToolCall || facts.Step.Status != contracts.StepStatusRunning ||
			runtimeContext.ApprovalContext != nil || facts.Approval != nil || tool != nil || errorCode != contracts.ErrorCodeWorkerInterrupted {
			return contracts.ReasonCodeCheckpointNextActionInvalid
		}
	case contracts.CheckpointNextActionExecuteApprovedTool:
		if facts.Step == nil || facts.Step.Status != contracts.StepStatusRunning || runtimeContext.ApprovalContext == nil ||
			facts.Approval == nil || facts.Approval.Status != contracts.ApprovalStatusApproved || tool != nil || errorCode != contracts.ErrorCodeWorkerInterrupted && errorCode != contracts.ErrorCodeConfigVersionMismatch {
			return contracts.ReasonCodeCheckpointNextActionInvalid
		}
	case contracts.CheckpointNextActionFinalizeRun:
		if facts.Step == nil || facts.Step.Status != contracts.StepStatusCompleted || facts.HasLaterStep || tool != nil || errorCode != contracts.ErrorCodeWorkerInterrupted {
			return contracts.ReasonCodeCheckpointNextActionInvalid
		}
	default:
		return contracts.ReasonCodeCheckpointNextActionInvalid
	}
	return ""
}

func sameRuntimeWriteTx(left, right contracts.RuntimeWriteTx) bool {
	if left == nil || right == nil || reflect.TypeOf(left) != reflect.TypeOf(right) ||
		reflect.TypeOf(left).Kind() != reflect.Pointer || !reflect.TypeOf(left).Comparable() {
		return false
	}
	return left == right
}

func validateApprovalForRecovery(runtimeContext contracts.RuntimeContextV1, facts ValidationFacts) (contracts.ReasonCode, bool) {
	if runtimeContext.ApprovalContext == nil {
		return validateApproval(runtimeContext, facts)
	}
	context := runtimeContext.ApprovalContext
	approval := facts.Approval
	if approval == nil || facts.Step == nil || approval.ApprovalID != context.ApprovalID || approval.TaskID != runtimeContext.TaskID ||
		approval.RunID != runtimeContext.RunID || approval.StepID != facts.Step.StepID || approval.ExecutionVersion != context.ApprovalExecutionVersion ||
		approval.ExecutionVersion >= runtimeContext.ExecutionVersion {
		return contracts.ReasonCodeCheckpointApprovalReferenceInvalid, false
	}
	currentVersion := runtimeContext.ExecutionVersion
	runtimeContext.ExecutionVersion = approval.ExecutionVersion
	reason, invariant := validateApproval(runtimeContext, facts)
	runtimeContext.ExecutionVersion = currentVersion
	return reason, invariant
}

func validateRecoveryQuery(query RecoverySourceQuery) error {
	if query.TaskID == "" || query.RunID == "" || !query.SourceExecutionVersion.Valid() ||
		query.Phase != RecoverySourceBeforeFirstExecution && query.Phase != RecoverySourceStartedExecution {
		return ErrInvalidDraft
	}
	return nil
}

func recoveryInvalid(entity Entity, reason contracts.ReasonCode) RecoverySourceInvalid {
	return RecoverySourceInvalid{CheckpointInvalid: invalidResult(entity, reason)}
}
