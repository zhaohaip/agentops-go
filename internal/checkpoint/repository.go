package checkpoint

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"

	"github.com/zhaohaip/agentops-go/internal/contracts"
	"github.com/zhaohaip/agentops-go/internal/contracts/references"
)

var (
	// ErrRepositoryNotFound 表示指定持久化事实不存在。
	ErrRepositoryNotFound = errors.New("checkpoint repository fact not found")
	// ErrPersistenceInvariantViolation 表示核心归属无法安全关联。
	ErrPersistenceInvariantViolation = errors.New("checkpoint persistence invariant violation")
	// ErrInvalidDraft 表示调用方构造了不允许写入的 Checkpoint Draft。
	ErrInvalidDraft = errors.New("checkpoint draft is invalid")
)

// RuntimeCheckpointDraft 是 Task Runtime 可保存的封闭 Draft 联合。
type RuntimeCheckpointDraft interface {
	isRuntimeCheckpointDraft()
}

// InitializationDraft 固定创建无 Plan、GENERATE_PLAN 的初始化 Checkpoint。
type InitializationDraft struct{}

func (InitializationDraft) isRuntimeCheckpointDraft() {}

// ExecutionDraft 表示非恢复执行边界；不允许携带 Recovery source。
type ExecutionDraft struct {
	PlanID             *contracts.PlanID
	CurrentStepID      *contracts.StepID
	NextAction         contracts.CheckpointNextAction
	ResolvedReferences contracts.CanonicalResolvedReferences
}

func (ExecutionDraft) isRuntimeCheckpointDraft() {}

// RuntimeCheckpointSaveRequest 是 Task Runtime 唯一公开保存请求。
type RuntimeCheckpointSaveRequest struct {
	TaskID              contracts.TaskID
	RunID               contracts.RunID
	ExecutionVersion    contracts.ExecutionVersion
	ExecutionConfigHash contracts.ExecutionConfigHash
	Draft               RuntimeCheckpointDraft
}

// RuntimeCheckpointQuery 是三个读取 usage 共享的身份参数；usage 由方法固定。
type RuntimeCheckpointQuery struct {
	TaskID           contracts.TaskID
	RunID            contracts.RunID
	ExecutionVersion contracts.ExecutionVersion
}

// ClaimQueryKind 固定领取用例的两种状态矩阵。
type ClaimQueryKind uint8

const (
	// ClaimQueryInitial 要求 Initialization Checkpoint。
	ClaimQueryInitial ClaimQueryKind = iota + 1
	// ClaimQueryContinuation 要求已排队的 Execution Checkpoint。
	ClaimQueryContinuation
)

// RuntimeCheckpointPort 是 Checkpoint 对 Task Runtime 暴露的窄 Port。
// Recovery 与 Approval 方法将在对应 Owner 任务完成后单独加入。
type RuntimeCheckpointPort interface {
	SaveRuntimeCheckpoint(context.Context, contracts.RuntimeWriteTx, RuntimeCheckpointSaveRequest) (Ref, error)
	LoadLatestForClaim(context.Context, contracts.RuntimeWriteTx, RuntimeCheckpointQuery, ClaimQueryKind) (ValidationResult, error)
	LoadLatestForExecutionDispatch(context.Context, contracts.RuntimeWriteTx, RuntimeCheckpointQuery) (ValidationResult, error)
	LoadLatestForStartupCleanup(context.Context, contracts.RuntimeWriteTx, RuntimeCheckpointQuery) (ValidationResult, error)
	ValidateRecoverySource(context.Context, contracts.RuntimeWriteTx, RecoverySourceQuery) (RecoverySourceResult, error)
	CreateRecoveryStart(context.Context, contracts.RuntimeWriteTx, RuntimeRecoveryStartRequest) (Ref, error)
}

// ValidationResult 是最新记录的封闭验证结果。
type ValidationResult interface {
	isCheckpointValidationResult()
}

// ValidCheckpoint 表示最大 sequence 记录通过指定 usage 的完整校验。
type ValidCheckpoint struct {
	Checkpoint          View
	InferredType        InferredType
	ExecutionConfigHash contracts.ExecutionConfigHash
}

func (ValidCheckpoint) isCheckpointValidationResult() {}

// CheckpointInvalid 表示记录可以安全归属，但内容无效或缺失。
type CheckpointInvalid struct {
	CheckpointID     contracts.CheckpointID
	TaskID           contracts.TaskID
	RunID            contracts.RunID
	ExecutionVersion contracts.ExecutionVersion
	ReasonCode       contracts.ReasonCode
}

func (CheckpointInvalid) isCheckpointValidationResult() {}

// PersistenceInvariantViolation 表示核心持久化事实无法安全归属。
type PersistenceInvariantViolation struct {
	SafeReasonCode contracts.CauseCode
}

func (PersistenceInvariantViolation) isCheckpointValidationResult() {}

type checkpointPurpose uint8

const (
	purposeInitialization checkpointPurpose = iota + 1
	purposeExecution
)

type checkpointUsage uint8

const (
	usageClaimInitial checkpointUsage = iota + 1
	usageClaimContinuation
	usageExecutionDispatch
	usageStartupCleanup
)

type saveCheckpointRequest struct {
	purpose checkpointPurpose
	query   RuntimeCheckpointQuery
	hash    contracts.ExecutionConfigHash
	context contracts.RuntimeContextV1
}

// Manager 只负责 Checkpoint 保存、选择和校验，不推进任何领域生命周期。
type Manager struct {
	repository Repository
	codec      RuntimeContextCodec
	extractor  references.Extractor
}

// NewManager 创建 Checkpoint 持久化 Manager。
func NewManager(repository Repository, codec RuntimeContextCodec) (*Manager, error) {
	if repository == nil {
		return nil, errors.New("create Checkpoint manager: repository is required")
	}
	return &Manager{repository: repository, codec: codec, extractor: references.NewStepReferenceExtractor()}, nil
}

// SaveRuntimeCheckpoint 将封闭 Draft 映射为固定 purpose 后保存。
func (m *Manager) SaveRuntimeCheckpoint(ctx context.Context, tx contracts.RuntimeWriteTx, request RuntimeCheckpointSaveRequest) (Ref, error) {
	query := RuntimeCheckpointQuery{TaskID: request.TaskID, RunID: request.RunID, ExecutionVersion: request.ExecutionVersion}
	runtimeContext := contracts.RuntimeContextV1{
		SchemaVersion: 1, TaskID: request.TaskID, RunID: request.RunID,
		ExecutionVersion: request.ExecutionVersion, ResolvedReferences: contracts.CanonicalResolvedReferences{},
	}
	purpose := purposeExecution
	switch draft := request.Draft.(type) {
	case InitializationDraft:
		purpose = purposeInitialization
		runtimeContext.NextAction = contracts.CheckpointNextActionGeneratePlan
	case ExecutionDraft:
		runtimeContext.PlanID = draft.PlanID
		runtimeContext.CurrentStepID = draft.CurrentStepID
		runtimeContext.NextAction = draft.NextAction
		runtimeContext.ResolvedReferences = cloneReferences(draft.ResolvedReferences)
	default:
		return Ref{}, fmt.Errorf("save Runtime Checkpoint: %w: unsupported draft", ErrInvalidDraft)
	}
	return m.saveCheckpoint(ctx, tx, saveCheckpointRequest{purpose: purpose, query: query, hash: request.ExecutionConfigHash, context: runtimeContext})
}

// LoadLatestForClaim 固定使用 CLAIM 状态矩阵。
func (m *Manager) LoadLatestForClaim(ctx context.Context, tx contracts.RuntimeWriteTx, query RuntimeCheckpointQuery, kind ClaimQueryKind) (ValidationResult, error) {
	switch kind {
	case ClaimQueryInitial:
		return m.loadAndValidateLatest(ctx, tx, query, usageClaimInitial)
	case ClaimQueryContinuation:
		return m.loadAndValidateLatest(ctx, tx, query, usageClaimContinuation)
	default:
		return nil, fmt.Errorf("load latest Checkpoint for Claim: %w: unknown Claim kind", ErrInvalidDraft)
	}
}

// LoadLatestForExecutionDispatch 固定使用 EXECUTION_DISPATCH 状态矩阵。
func (m *Manager) LoadLatestForExecutionDispatch(ctx context.Context, tx contracts.RuntimeWriteTx, query RuntimeCheckpointQuery) (ValidationResult, error) {
	return m.loadAndValidateLatest(ctx, tx, query, usageExecutionDispatch)
}

// LoadLatestForStartupCleanup 固定使用 STARTUP_CLEANUP 状态矩阵。
func (m *Manager) LoadLatestForStartupCleanup(ctx context.Context, tx contracts.RuntimeWriteTx, query RuntimeCheckpointQuery) (ValidationResult, error) {
	return m.loadAndValidateLatest(ctx, tx, query, usageStartupCleanup)
}

func (m *Manager) saveCheckpoint(ctx context.Context, tx contracts.RuntimeWriteTx, request saveCheckpointRequest) (Ref, error) {
	if err := validateQuery(request.query); err != nil || !request.hash.Valid() {
		return Ref{}, fmt.Errorf("save Checkpoint: %w", ErrInvalidDraft)
	}
	if request.purpose == purposeInitialization && request.query.ExecutionVersion != 1 {
		return Ref{}, fmt.Errorf("save Checkpoint: %w: Initialization requires version 1", ErrInvalidDraft)
	}
	encoded, err := m.codec.Encode(request.context)
	if err != nil {
		return Ref{}, fmt.Errorf("save Checkpoint: %w: %v", ErrInvalidDraft, err)
	}
	facts, err := m.loadFacts(ctx, tx, request.query, request.context)
	if err != nil {
		return Ref{}, fmt.Errorf("save Checkpoint: load ValidationFacts: %w", err)
	}
	if facts.Execution.ExecutionConfigHash != request.hash {
		return Ref{}, fmt.Errorf("save Checkpoint: %w: execution config hash mismatch", ErrPersistenceInvariantViolation)
	}
	reason, invariant := m.validateContext(request.context, facts, saveUsage(request.purpose))
	if invariant {
		return Ref{}, fmt.Errorf("save Checkpoint: %w", ErrPersistenceInvariantViolation)
	}
	if reason != "" {
		return Ref{}, fmt.Errorf("save Checkpoint: %w: %s", ErrInvalidDraft, reason)
	}
	sequence, err := m.repository.AllocateNextSequence(ctx, tx, request.query.RunID)
	if err != nil {
		return Ref{}, fmt.Errorf("save Checkpoint: allocate sequence: %w", err)
	}
	if request.purpose == purposeInitialization && sequence != 1 {
		return Ref{}, fmt.Errorf("save Checkpoint: %w: Initialization requires sequence 1", ErrInvalidDraft)
	}
	checkpointID, err := newCheckpointID()
	if err != nil {
		return Ref{}, fmt.Errorf("save Checkpoint: generate ID: %w", err)
	}
	entity := Entity{
		CheckpointID: checkpointID, TaskID: request.query.TaskID, RunID: request.query.RunID,
		ExecutionVersion: request.query.ExecutionVersion, CheckpointSequence: sequence,
		RuntimeContext: encoded, ExecutionConfigHash: request.hash,
	}
	createdAt, err := m.repository.InsertCheckpoint(ctx, tx, entity)
	if err != nil {
		return Ref{}, fmt.Errorf("save Checkpoint: insert: %w", err)
	}
	return Ref{CheckpointID: checkpointID, CheckpointSequence: sequence, CreatedAt: createdAt.UTC()}, nil
}

func (m *Manager) loadAndValidateLatest(ctx context.Context, tx contracts.RuntimeWriteTx, query RuntimeCheckpointQuery, usage checkpointUsage) (ValidationResult, error) {
	if err := validateQuery(query); err != nil {
		return nil, fmt.Errorf("load latest Checkpoint: %w", err)
	}
	if err := m.repository.VerifyRunAttribution(ctx, tx, query.TaskID, query.RunID); err != nil {
		if errors.Is(err, ErrPersistenceInvariantViolation) {
			return persistenceInvariantResult(), nil
		}
		return nil, fmt.Errorf("load latest Checkpoint: verify Run attribution: %w", err)
	}
	entity, err := m.repository.FindLatestByExecutionVersion(ctx, tx, query.TaskID, query.RunID, query.ExecutionVersion)
	if errors.Is(err, ErrRepositoryNotFound) {
		return CheckpointInvalid{TaskID: query.TaskID, RunID: query.RunID, ExecutionVersion: query.ExecutionVersion, ReasonCode: contracts.ReasonCodeCheckpointNotFound}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("load latest Checkpoint: select: %w", err)
	}
	decoded, err := m.codec.Decode(entity.RuntimeContext)
	if err != nil {
		return invalidResult(entity, codecReasonCode(err)), nil
	}
	if decoded.TaskID != entity.TaskID || decoded.RunID != entity.RunID || decoded.ExecutionVersion != entity.ExecutionVersion {
		return invalidResult(entity, contracts.ReasonCodeCheckpointAttributionMismatch), nil
	}
	facts, err := m.loadFacts(ctx, tx, query, decoded)
	if err != nil {
		if errors.Is(err, ErrPersistenceInvariantViolation) {
			return persistenceInvariantResult(), nil
		}
		return nil, fmt.Errorf("load latest Checkpoint: load ValidationFacts: %w", err)
	}
	if !coreAttributionValid(decoded, facts) {
		return persistenceInvariantResult(), nil
	}
	if entity.ExecutionConfigHash != facts.Execution.ExecutionConfigHash {
		return invalidResult(entity, contracts.ReasonCodeCheckpointExecutionHashMismatch), nil
	}
	inferredType, ok := inferCheckpointType(entity, decoded)
	if !ok {
		return invalidResult(entity, contracts.ReasonCodeCheckpointTypeAmbiguous), nil
	}
	if inferredType == InferredTypeRecoveryStart {
		if usage == usageClaimInitial {
			return invalidResult(entity, contracts.ReasonCodeCheckpointSourceInvalid), nil
		}
		valid, err := m.directRecoverySourceValid(ctx, tx, entity)
		if err != nil {
			return nil, fmt.Errorf("load latest Checkpoint: validate Recovery source: %w", err)
		}
		if !valid {
			return invalidResult(entity, contracts.ReasonCodeCheckpointSourceInvalid), nil
		}
	}
	reason, invariant := m.validateContextWithApproval(
		decoded,
		facts,
		usage,
		inferredType == InferredTypeRecoveryStart,
	)
	if invariant {
		return persistenceInvariantResult(), nil
	}
	if reason != "" {
		return invalidResult(entity, reason), nil
	}
	if usage == usageClaimInitial && inferredType != InferredTypeInitialization ||
		usage == usageClaimContinuation && inferredType == InferredTypeInitialization {
		return invalidResult(entity, contracts.ReasonCodeCheckpointSourceInvalid), nil
	}
	return ValidCheckpoint{
		Checkpoint: View{Entity: entity, Context: decoded}, InferredType: inferredType,
		ExecutionConfigHash: entity.ExecutionConfigHash,
	}, nil
}

// directRecoverySourceValid 只核对 Recovery Start 的直接不可变来源，不沿来源链递归。
func (m *Manager) directRecoverySourceValid(ctx context.Context, tx contracts.RuntimeWriteTx, entity Entity) (bool, error) {
	if entity.SourceExecutionVersion == nil || entity.SourceCheckpointID == nil ||
		*entity.SourceExecutionVersion+1 != entity.ExecutionVersion {
		return false, nil
	}
	source, err := m.repository.FindByID(ctx, tx, *entity.SourceCheckpointID)
	if errors.Is(err, ErrRepositoryNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if source.CheckpointID != *entity.SourceCheckpointID ||
		source.TaskID != entity.TaskID || source.RunID != entity.RunID ||
		source.ExecutionVersion != *entity.SourceExecutionVersion ||
		source.CheckpointSequence >= entity.CheckpointSequence ||
		source.ExecutionConfigHash != entity.ExecutionConfigHash {
		return false, nil
	}
	sourceExecution, err := m.repository.LoadTaskExecution(ctx, tx, entity.TaskID, *entity.SourceExecutionVersion)
	if errors.Is(err, ErrPersistenceInvariantViolation) || errors.Is(err, ErrRepositoryNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return sourceExecution.TaskID == entity.TaskID &&
		sourceExecution.ExecutionVersion == *entity.SourceExecutionVersion &&
		sourceExecution.ExecutionConfigHash == source.ExecutionConfigHash, nil
}

func (m *Manager) loadFacts(ctx context.Context, tx contracts.RuntimeWriteTx, query RuntimeCheckpointQuery, runtimeContext contracts.RuntimeContextV1) (ValidationFacts, error) {
	if err := m.repository.VerifyRunAttribution(ctx, tx, query.TaskID, query.RunID); err != nil {
		return ValidationFacts{}, err
	}
	return m.repository.LoadValidationFacts(ctx, tx, ValidationFactsRequest{
		TaskID: query.TaskID, RunID: query.RunID, ExecutionVersion: query.ExecutionVersion,
		PlanID: runtimeContext.PlanID, CurrentStepID: runtimeContext.CurrentStepID,
		ApprovalID: approvalID(runtimeContext.ApprovalContext),
	})
}

func (m *Manager) validateContext(runtimeContext contracts.RuntimeContextV1, facts ValidationFacts, usage checkpointUsage) (contracts.ReasonCode, bool) {
	return m.validateContextWithApproval(runtimeContext, facts, usage, false)
}

func (m *Manager) validateContextWithApproval(runtimeContext contracts.RuntimeContextV1, facts ValidationFacts, usage checkpointUsage, recovery bool) (contracts.ReasonCode, bool) {
	if reason := validatePlanAndStep(runtimeContext, facts); reason != "" {
		return reason, false
	}
	if reason := m.validateReferences(runtimeContext, facts); reason != "" {
		return reason, false
	}
	approvalValidator := validateApproval
	if recovery {
		approvalValidator = validateApprovalForRecovery
	}
	if reason, invariant := approvalValidator(runtimeContext, facts); reason != "" || invariant {
		return reason, invariant
	}
	if reason := validateAction(runtimeContext, facts, usage); reason != "" {
		return reason, false
	}
	return validateUsage(runtimeContext, facts, usage), false
}

func coreAttributionValid(runtimeContext contracts.RuntimeContextV1, facts ValidationFacts) bool {
	return facts.Task.TaskID == runtimeContext.TaskID && facts.Run.TaskID == runtimeContext.TaskID &&
		facts.Run.RunID == runtimeContext.RunID && facts.Execution.TaskID == runtimeContext.TaskID &&
		facts.Execution.ExecutionVersion == runtimeContext.ExecutionVersion &&
		facts.Task.CurrentRunID == runtimeContext.RunID
}

func validatePlanAndStep(runtimeContext contracts.RuntimeContextV1, facts ValidationFacts) contracts.ReasonCode {
	if runtimeContext.PlanID == nil {
		if facts.Plan != nil || facts.Run.PlanID != nil {
			return contracts.ReasonCodeCheckpointPlanReferenceInvalid
		}
	} else if facts.Plan == nil || facts.Run.PlanID == nil || *facts.Run.PlanID != *runtimeContext.PlanID ||
		facts.Plan.PlanID != *runtimeContext.PlanID || facts.Plan.RunID != runtimeContext.RunID {
		return contracts.ReasonCodeCheckpointPlanReferenceInvalid
	}
	if runtimeContext.CurrentStepID == nil {
		if facts.Step != nil || facts.Run.CurrentStepID != nil {
			return contracts.ReasonCodeCheckpointStepReferenceInvalid
		}
	} else if facts.Step == nil || facts.Run.CurrentStepID == nil || *facts.Run.CurrentStepID != *runtimeContext.CurrentStepID ||
		facts.Step.StepID != *runtimeContext.CurrentStepID || facts.Step.RunID != runtimeContext.RunID ||
		runtimeContext.PlanID == nil || facts.Step.PlanID != *runtimeContext.PlanID {
		return contracts.ReasonCodeCheckpointStepReferenceInvalid
	}
	return ""
}

func (m *Manager) validateReferences(runtimeContext contracts.RuntimeContextV1, facts ValidationFacts) contracts.ReasonCode {
	mode, err := references.ActionModeForNextAction(runtimeContext.NextAction)
	if err != nil {
		return contracts.ReasonCodeCheckpointNextActionInvalid
	}
	if mode == contracts.ReferenceActionModeNoStepInput {
		if len(runtimeContext.ResolvedReferences) != 0 {
			return contracts.ReasonCodeCheckpointReferenceExtra
		}
		return ""
	}
	if facts.Step == nil {
		return contracts.ReasonCodeCheckpointStepReferenceInvalid
	}
	var source *references.SourceStep
	if facts.Previous != nil {
		source = &references.SourceStep{
			StepID: facts.Previous.StepID, Sequence: facts.Previous.Sequence, Status: facts.Previous.Status,
			OutputSchema: facts.Previous.OutputSchema, SafeOutput: facts.Previous.SafeOutput,
		}
	}
	result, err := m.extractor.Extract(references.ExtractRequest{
		ActionMode: mode, StepInput: facts.Step.Input, TargetStepSequence: facts.Step.Sequence,
		SourceStep: source, ValidatePersistedOutput: true,
	})
	if err != nil {
		return mapReferenceError(err)
	}
	if !reflect.DeepEqual(result.ResolvedReferences, runtimeContext.ResolvedReferences) {
		return classifyReferenceDifference(result.ResolvedReferences, runtimeContext.ResolvedReferences)
	}
	return ""
}

func validateApproval(runtimeContext contracts.RuntimeContextV1, facts ValidationFacts) (contracts.ReasonCode, bool) {
	context := runtimeContext.ApprovalContext
	approval := facts.Approval
	if context == nil {
		if approval != nil {
			return contracts.ReasonCodeCheckpointApprovalReferenceInvalid, false
		}
		return "", false
	}
	if approval == nil || facts.Step == nil || approval.ApprovalID != context.ApprovalID ||
		approval.TaskID != runtimeContext.TaskID || approval.RunID != runtimeContext.RunID ||
		approval.StepID != facts.Step.StepID || approval.ExecutionVersion != context.ApprovalExecutionVersion ||
		approval.ExecutionVersion != runtimeContext.ExecutionVersion {
		return contracts.ReasonCodeCheckpointApprovalReferenceInvalid, false
	}
	if !approval.ExecutionConfigHash.Valid() || !approval.OwnerExecutionConfigHash.Valid() ||
		!approval.FrozenInputHash.Valid() {
		return "", true
	}
	expectedHash, err := contracts.ComputeFrozenInputHashV1(contracts.FrozenApprovedToolInputV1{
		Schema: contracts.FrozenApprovedToolInputSchemaV1, Version: contracts.FrozenApprovedToolInputVersionV1,
		ToolName: approval.ToolName, ToolInput: approval.FrozenToolInput,
		ObservedValues: approval.ObservedValues, ResourceVersion: approval.ResourceVersion,
	})
	if err != nil || expectedHash != approval.FrozenInputHash || approval.ExecutionConfigHash != approval.OwnerExecutionConfigHash {
		return "", true
	}
	if approval.ExecutionConfigHash != facts.Execution.ExecutionConfigHash {
		return contracts.ReasonCodeCheckpointExecutionHashMismatch, false
	}
	if context.FrozenInputHash != approval.FrozenInputHash {
		return contracts.ReasonCodeCheckpointFrozenInputHashMismatch, false
	}
	if context.ToolName != approval.ToolName || approval.ToolName != facts.Step.ToolName ||
		!jsonEqual(context.FrozenToolInput, approval.FrozenToolInput) ||
		!jsonEqual(context.ObservedValues, approval.ObservedValues) || context.ResourceVersion != approval.ResourceVersion {
		return contracts.ReasonCodeCheckpointFrozenActionMismatch, false
	}
	return "", false
}

func validateAction(runtimeContext contracts.RuntimeContextV1, facts ValidationFacts, usage checkpointUsage) contracts.ReasonCode {
	switch runtimeContext.NextAction {
	case contracts.CheckpointNextActionGeneratePlan:
		if runtimeContext.PlanID != nil || runtimeContext.CurrentStepID != nil || runtimeContext.ApprovalContext != nil {
			return contracts.ReasonCodeCheckpointNextActionInvalid
		}
	case contracts.CheckpointNextActionExecuteStep:
		if facts.Plan == nil || facts.Step == nil || facts.Step.Status != contracts.StepStatusPending && facts.Step.Status != contracts.StepStatusRunning || runtimeContext.ApprovalContext != nil {
			return contracts.ReasonCodeCheckpointNextActionInvalid
		}
		if facts.Approval != nil || !toolExecutionAllowed(runtimeContext, facts, usage) {
			return contracts.ReasonCodeCheckpointNextActionInvalid
		}
	case contracts.CheckpointNextActionRequestApproval:
		if facts.Plan == nil || facts.Step == nil || facts.Step.Type != contracts.StepTypeToolCall ||
			facts.Step.Status != contracts.StepStatusPending && facts.Step.Status != contracts.StepStatusRunning ||
			runtimeContext.ApprovalContext != nil || facts.Approval != nil || facts.ToolExecution != nil {
			return contracts.ReasonCodeCheckpointNextActionInvalid
		}
	case contracts.CheckpointNextActionExecuteApprovedTool:
		if facts.Step == nil || facts.Step.Type != contracts.StepTypeToolCall || facts.Step.Status != contracts.StepStatusRunning ||
			runtimeContext.ApprovalContext == nil || facts.Approval == nil || facts.Approval.Status != contracts.ApprovalStatusApproved ||
			!toolExecutionAllowed(runtimeContext, facts, usage) {
			return contracts.ReasonCodeCheckpointApprovalReferenceInvalid
		}
	case contracts.CheckpointNextActionFinalizeRun:
		if facts.Plan == nil || facts.Step == nil || facts.Step.Status != contracts.StepStatusCompleted ||
			runtimeContext.CurrentStepID == nil || facts.Run.CurrentStepID == nil ||
			*runtimeContext.CurrentStepID != facts.Step.StepID || *facts.Run.CurrentStepID != facts.Step.StepID ||
			facts.HasLaterStep || runtimeContext.ApprovalContext != nil || !toolExecutionAllowed(runtimeContext, facts, usage) {
			return contracts.ReasonCodeCheckpointNextActionInvalid
		}
	default:
		return contracts.ReasonCodeCheckpointNextActionInvalid
	}
	return ""
}

func toolExecutionAllowed(runtimeContext contracts.RuntimeContextV1, facts ValidationFacts, usage checkpointUsage) bool {
	tool := facts.ToolExecution
	if tool == nil {
		return true
	}
	if !toolExecutionBelongs(runtimeContext, facts) {
		return false
	}
	switch runtimeContext.NextAction {
	case contracts.CheckpointNextActionExecuteStep, contracts.CheckpointNextActionExecuteApprovedTool:
		return usage == usageStartupCleanup && facts.Step != nil && facts.Step.Type == contracts.StepTypeToolCall &&
			tool.Status == contracts.ToolExecutionStatusRunning
	case contracts.CheckpointNextActionFinalizeRun:
		return facts.Step != nil && facts.Step.Type == contracts.StepTypeToolCall &&
			tool.Status == contracts.ToolExecutionStatusCompleted
	default:
		return false
	}
}

func jsonEqual(left, right []byte) bool {
	var leftValue any
	var rightValue any
	return json.Unmarshal(left, &leftValue) == nil && json.Unmarshal(right, &rightValue) == nil &&
		reflect.DeepEqual(leftValue, rightValue)
}

func toolExecutionBelongs(runtimeContext contracts.RuntimeContextV1, facts ValidationFacts) bool {
	tool := facts.ToolExecution
	if tool == nil || facts.Step == nil || tool.TaskID != runtimeContext.TaskID ||
		tool.RunID != runtimeContext.RunID || tool.StepID != facts.Step.StepID ||
		tool.ExecutionVersion != runtimeContext.ExecutionVersion || !tool.Status.Valid() ||
		(tool.Status == contracts.ToolExecutionStatusUnknown) != tool.SideEffectUnknown {
		return false
	}
	switch tool.Status {
	case contracts.ToolExecutionStatusRunning, contracts.ToolExecutionStatusCompleted:
		return tool.ErrorCode == nil
	case contracts.ToolExecutionStatusFailed, contracts.ToolExecutionStatusUnknown:
		return tool.ErrorCode != nil
	default:
		return false
	}
}

func validateUsage(runtimeContext contracts.RuntimeContextV1, facts ValidationFacts, usage checkpointUsage) contracts.ReasonCode {
	switch usage {
	case usageClaimInitial:
		if runtimeContext.NextAction != contracts.CheckpointNextActionGeneratePlan || runtimeContext.ExecutionVersion != 1 ||
			facts.Task.Status != contracts.TaskStatusPending || facts.Run.Status != contracts.RunStatusPending ||
			facts.Execution.Status != contracts.TaskExecutionStatusQueued || facts.Execution.WorkerID != nil || facts.Task.QueuedAt == nil {
			return contracts.ReasonCodeCheckpointNextActionInvalid
		}
	case usageClaimContinuation:
		if facts.Execution.Status != contracts.TaskExecutionStatusQueued || facts.Execution.WorkerID != nil || facts.Task.QueuedAt == nil {
			return contracts.ReasonCodeCheckpointNextActionInvalid
		}
		if runtimeContext.NextAction == contracts.CheckpointNextActionGeneratePlan {
			if facts.Task.Status != contracts.TaskStatusPending || facts.Run.Status != contracts.RunStatusPending {
				return contracts.ReasonCodeCheckpointNextActionInvalid
			}
		} else if facts.Task.Status != contracts.TaskStatusRunning || facts.Run.Status != contracts.RunStatusRunning {
			return contracts.ReasonCodeCheckpointNextActionInvalid
		}
	case usageExecutionDispatch, usageStartupCleanup:
		if facts.Task.Status != contracts.TaskStatusRunning || facts.Run.Status != contracts.RunStatusRunning || facts.Execution.Status != contracts.TaskExecutionStatusRunning {
			return contracts.ReasonCodeCheckpointNextActionInvalid
		}
		if facts.Execution.WorkerID == nil {
			return contracts.ReasonCodeCheckpointNextActionInvalid
		}
	}
	return ""
}

func saveUsage(purpose checkpointPurpose) checkpointUsage {
	if purpose == purposeInitialization {
		return usageClaimInitial
	}
	return usageExecutionDispatch
}

func validateQuery(query RuntimeCheckpointQuery) error {
	if query.TaskID == "" || query.RunID == "" || !query.ExecutionVersion.Valid() {
		return ErrInvalidDraft
	}
	return nil
}

func approvalID(value *contracts.ApprovalContext) *contracts.ApprovalID {
	if value == nil {
		return nil
	}
	return &value.ApprovalID
}

func mapReferenceError(err error) contracts.ReasonCode {
	var issue *references.IssueError
	switch {
	case errors.As(err, &issue) && issue.Code == contracts.ReferenceIssueCodeCountLimitExceeded:
		return contracts.ReasonCodeCheckpointReferenceLimitExceeded
	case errors.Is(err, references.ErrReferenceSyntax):
		return contracts.ReasonCodeCheckpointReferenceSyntaxInvalid
	case errors.Is(err, references.ErrReferencePath):
		return contracts.ReasonCodeCheckpointReferencePathInvalid
	case errors.Is(err, references.ErrDuplicateTarget):
		return contracts.ReasonCodeCheckpointReferenceDuplicateTarget
	case errors.Is(err, references.ErrSourceStep):
		return contracts.ReasonCodeCheckpointReferenceSourceInvalid
	case errors.Is(err, references.ErrSourceOutput), errors.Is(err, references.ErrInvalidStepInput):
		return contracts.ReasonCodeCheckpointStepOutputReferenceInvalid
	default:
		return contracts.ReasonCodeCheckpointNextActionInvalid
	}
}

func classifyReferenceDifference(expected, actual contracts.CanonicalResolvedReferences) contracts.ReasonCode {
	if len(actual) < len(expected) {
		return contracts.ReasonCodeCheckpointReferenceMissing
	}
	if len(actual) > len(expected) {
		return contracts.ReasonCodeCheckpointReferenceExtra
	}
	return contracts.ReasonCodeCheckpointReferenceOrderInvalid
}

func cloneReferences(value contracts.CanonicalResolvedReferences) contracts.CanonicalResolvedReferences {
	if value == nil {
		return contracts.CanonicalResolvedReferences{}
	}
	cloned := make(contracts.CanonicalResolvedReferences, len(value))
	for index := range value {
		cloned[index] = value[index]
		cloned[index].TargetPath = append([]contracts.ReferencePathSegment(nil), value[index].TargetPath...)
	}
	return cloned
}

func newCheckpointID() (contracts.CheckpointID, error) {
	var value [16]byte
	if _, err := rand.Read(value[:]); err != nil {
		return "", err
	}
	return contracts.CheckpointID("checkpoint-" + hex.EncodeToString(value[:])), nil
}

func invalidResult(entity Entity, reason contracts.ReasonCode) CheckpointInvalid {
	return CheckpointInvalid{CheckpointID: entity.CheckpointID, TaskID: entity.TaskID, RunID: entity.RunID, ExecutionVersion: entity.ExecutionVersion, ReasonCode: reason}
}

func persistenceInvariantResult() PersistenceInvariantViolation {
	return PersistenceInvariantViolation{SafeReasonCode: contracts.CauseCodePersistenceInvariantViolation}
}

func codecReasonCode(err error) contracts.ReasonCode {
	var codecError *RuntimeContextCodecError
	if errors.As(err, &codecError) && codecError.Kind == RuntimeContextCodecVersionUnsupported {
		return contracts.ReasonCodeRuntimeContextVersionUnsupported
	}
	return contracts.ReasonCodeRuntimeContextMalformed
}

func inferCheckpointType(entity Entity, runtimeContext contracts.RuntimeContextV1) (InferredType, bool) {
	hasSourceVersion := entity.SourceExecutionVersion != nil
	hasSourceCheckpoint := entity.SourceCheckpointID != nil
	if hasSourceVersion != hasSourceCheckpoint {
		return "", false
	}
	if hasSourceVersion {
		return InferredTypeRecoveryStart, true
	}
	if entity.ExecutionVersion == 1 && entity.CheckpointSequence == 1 &&
		runtimeContext.NextAction == contracts.CheckpointNextActionGeneratePlan &&
		runtimeContext.PlanID == nil && runtimeContext.CurrentStepID == nil && runtimeContext.ApprovalContext == nil {
		return InferredTypeInitialization, true
	}
	return InferredTypeExecution, true
}

var _ RuntimeCheckpointPort = (*Manager)(nil)
