package checkpoint

import (
	"context"
	"fmt"

	"github.com/zhaohaip/agentops-go/internal/contracts"
	domain "github.com/zhaohaip/agentops-go/internal/taskruntime"
)

// TaskRuntimeAdapter 将 Checkpoint 模块的强类型 Port 适配为 Task Runtime 的消费方 Port。
// purpose、usage 和 Draft 均由窄方法固定，Task Runtime 无法构造任意 Checkpoint 类型。
type TaskRuntimeAdapter struct {
	manager RuntimeCheckpointPort
}

// NewTaskRuntimeAdapter 创建 Create、Claim 与既有执行派发共用的 Checkpoint 适配器。
func NewTaskRuntimeAdapter(manager RuntimeCheckpointPort) (*TaskRuntimeAdapter, error) {
	if manager == nil {
		return nil, fmt.Errorf("create Task Runtime Checkpoint adapter: manager is required")
	}
	return &TaskRuntimeAdapter{manager: manager}, nil
}

// SaveInitializationCheckpoint 保存固定为 Initialization purpose 的 GENERATE_PLAN Checkpoint。
func (a *TaskRuntimeAdapter) SaveInitializationCheckpoint(
	ctx context.Context,
	tx contracts.RuntimeWriteTx,
	request domain.SaveRuntimeCheckpointRequest,
) error {
	_, err := a.manager.SaveRuntimeCheckpoint(ctx, tx, saveRequest(request, InitializationDraft{}))
	return err
}

// SaveGeneratePlanExecutionCheckpoint 保存固定为 Execution purpose 的 GENERATE_PLAN Checkpoint。
func (a *TaskRuntimeAdapter) SaveGeneratePlanExecutionCheckpoint(
	ctx context.Context,
	tx contracts.RuntimeWriteTx,
	request domain.SaveRuntimeCheckpointRequest,
) error {
	_, err := a.manager.SaveRuntimeCheckpoint(ctx, tx, saveRequest(request, ExecutionDraft{
		NextAction:         contracts.CheckpointNextActionGeneratePlan,
		ResolvedReferences: contracts.CanonicalResolvedReferences{},
	}))
	return err
}

func (a *TaskRuntimeAdapter) LoadLatestForClaim(
	ctx context.Context,
	tx contracts.RuntimeWriteTx,
	taskID contracts.TaskID,
	runID contracts.RunID,
	version contracts.ExecutionVersion,
	source domain.ClaimCheckpointSource,
) (domain.ClaimCheckpointResult, error) {
	kind := ClaimQueryKind(0)
	switch source {
	case domain.ClaimCheckpointSourceInitial:
		kind = ClaimQueryInitial
	case domain.ClaimCheckpointSourceContinuation:
		kind = ClaimQueryContinuation
	default:
		return nil, fmt.Errorf("load Claim Checkpoint: unknown source: %w", domain.ErrPersistenceInvariantViolation)
	}
	result, err := a.manager.LoadLatestForClaim(ctx, tx, query(taskID, runID, version), kind)
	if err != nil {
		return nil, err
	}
	return claimResult(result)
}

func (a *TaskRuntimeAdapter) LoadLatestForExecutionDispatch(
	ctx context.Context,
	tx contracts.RuntimeWriteTx,
	taskID contracts.TaskID,
	runID contracts.RunID,
	version contracts.ExecutionVersion,
) (domain.ExecutionCheckpointResult, error) {
	result, err := a.manager.LoadLatestForExecutionDispatch(ctx, tx, query(taskID, runID, version))
	if err != nil {
		return nil, err
	}
	switch typed := result.(type) {
	case ValidCheckpoint:
		return domain.ExecutionCheckpointValid{Checkpoint: runtimeCheckpoint(typed)}, nil
	case CheckpointInvalid:
		return domain.ExecutionCheckpointInvalid{ReasonCode: typed.ReasonCode}, nil
	case PersistenceInvariantViolation:
		return nil, fmt.Errorf("load Execution Checkpoint: %s: %w", typed.SafeReasonCode, domain.ErrPersistenceInvariantViolation)
	default:
		return nil, fmt.Errorf("load Execution Checkpoint: unknown validation result: %w", domain.ErrPersistenceInvariantViolation)
	}
}

func saveRequest(request domain.SaveRuntimeCheckpointRequest, draft RuntimeCheckpointDraft) RuntimeCheckpointSaveRequest {
	return RuntimeCheckpointSaveRequest{
		TaskID: request.TaskID, RunID: request.RunID, ExecutionVersion: request.ExecutionVersion,
		ExecutionConfigHash: request.ExecutionConfigHash, Draft: draft,
	}
}

func query(taskID contracts.TaskID, runID contracts.RunID, version contracts.ExecutionVersion) RuntimeCheckpointQuery {
	return RuntimeCheckpointQuery{TaskID: taskID, RunID: runID, ExecutionVersion: version}
}

func claimResult(result ValidationResult) (domain.ClaimCheckpointResult, error) {
	switch typed := result.(type) {
	case ValidCheckpoint:
		return domain.ClaimCheckpointValid{Checkpoint: runtimeCheckpoint(typed)}, nil
	case CheckpointInvalid:
		return domain.ClaimCheckpointInvalid{ReasonCode: typed.ReasonCode}, nil
	case PersistenceInvariantViolation:
		return nil, fmt.Errorf("load Claim Checkpoint: %s: %w", typed.SafeReasonCode, domain.ErrPersistenceInvariantViolation)
	default:
		return nil, fmt.Errorf("load Claim Checkpoint: unknown validation result: %w", domain.ErrPersistenceInvariantViolation)
	}
}

func runtimeCheckpoint(valid ValidCheckpoint) domain.RuntimeCheckpoint {
	view := valid.Checkpoint
	return domain.RuntimeCheckpoint{
		CheckpointID: view.CheckpointID, TaskID: view.TaskID, RunID: view.RunID,
		ExecutionVersion: view.ExecutionVersion, ExecutionConfigHash: valid.ExecutionConfigHash,
		NextAction: view.Context.NextAction, CheckpointSequence: view.CheckpointSequence,
		ResolvedReferences: view.Context.ResolvedReferences, ApprovalContext: view.Context.ApprovalContext,
		SourceExecutionVersion: view.SourceExecutionVersion, SourceCheckpointID: view.SourceCheckpointID,
	}
}

var _ domain.RuntimeCheckpointPort = (*TaskRuntimeAdapter)(nil)
