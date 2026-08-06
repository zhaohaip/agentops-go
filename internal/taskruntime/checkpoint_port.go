package taskruntime

import (
	"context"
	"time"

	"github.com/zhaohaip/agentops-go/internal/contracts"
)

// AgentRuntimeConfig 是 Create、Claim 与 Execute 使用的已冻结 Agent 配置投影。
type AgentRuntimeConfig struct {
	TaskTimeout                 time.Duration
	ExecutionConfig             contracts.ExecutionConfigV1
	PlanningToolCatalogSelector contracts.PlanningToolCatalogSelector
}

// AgentConfigSource 只返回已经通过启动校验的静态 Agent 配置。
type AgentConfigSource interface {
	LookupAgent(contracts.AgentID) (AgentRuntimeConfig, bool)
}

// ClaimCheckpointSource 表示领取所要求的 Checkpoint 来源。
type ClaimCheckpointSource string

const (
	// ClaimCheckpointSourceInitial 表示首次领取要求 Initialization Checkpoint。
	ClaimCheckpointSourceInitial ClaimCheckpointSource = "InitialClaim"
	// ClaimCheckpointSourceContinuation 表示非首次领取要求当前版本最大 Checkpoint。
	ClaimCheckpointSourceContinuation ClaimCheckpointSource = "QueuedContinuation"
)

// SaveRuntimeCheckpointRequest 是 Task Runtime 在调用方事务内保存执行边界的最小请求。
type SaveRuntimeCheckpointRequest struct {
	TaskID              contracts.TaskID
	RunID               contracts.RunID
	ExecutionVersion    contracts.ExecutionVersion
	ExecutionConfigHash contracts.ExecutionConfigHash
	NextAction          contracts.CheckpointNextAction
	CreatedAt           time.Time
}

// RuntimeCheckpoint 是领取与执行派发门禁使用的已验证 Checkpoint 投影。
type RuntimeCheckpoint struct {
	CheckpointID           contracts.CheckpointID
	TaskID                 contracts.TaskID
	RunID                  contracts.RunID
	ExecutionVersion       contracts.ExecutionVersion
	ExecutionConfigHash    contracts.ExecutionConfigHash
	NextAction             contracts.CheckpointNextAction
	CheckpointSequence     int64
	ResolvedReferences     contracts.CanonicalResolvedReferences
	ApprovalContext        *contracts.ApprovalContext
	SourceExecutionVersion *contracts.ExecutionVersion
	SourceCheckpointID     *contracts.CheckpointID
}

// StartupCleanupCheckpointResult 是启动清理所需最大 Checkpoint 的封闭结果。
type StartupCleanupCheckpointResult interface {
	isStartupCleanupCheckpointResult()
}

// StartupCleanupCheckpointValid 表示遗留 Execution 的最大 Checkpoint 已通过持久化校验。
type StartupCleanupCheckpointValid struct {
	Checkpoint RuntimeCheckpoint
}

func (StartupCleanupCheckpointValid) isStartupCleanupCheckpointResult() {}

// StartupCleanupCheckpointInvalid 表示遗留 Execution 的最大 Checkpoint 缺失或损坏。
type StartupCleanupCheckpointInvalid struct {
	ReasonCode contracts.ReasonCode
}

func (StartupCleanupCheckpointInvalid) isStartupCleanupCheckpointResult() {}

// StartupCleanupCheckpointPort 在启动清理事务内验证遗留 Execution 的最大 Checkpoint。
type StartupCleanupCheckpointPort interface {
	LoadLatestForStartupCleanup(
		context.Context,
		contracts.RuntimeWriteTx,
		contracts.TaskID,
		contracts.RunID,
		contracts.ExecutionVersion,
	) (StartupCleanupCheckpointResult, error)
}

// ClaimCheckpointResult 是 Checkpoint Port 的封闭领取结果。
type ClaimCheckpointResult interface {
	isClaimCheckpointResult()
}

// ClaimCheckpointValid 表示领取所需的最大 Checkpoint 已通过结构校验。
type ClaimCheckpointValid struct {
	Checkpoint RuntimeCheckpoint
}

func (ClaimCheckpointValid) isClaimCheckpointResult() {}

// ClaimCheckpointInvalid 表示 Checkpoint 缺失或损坏。
type ClaimCheckpointInvalid struct {
	ReasonCode contracts.ReasonCode
}

func (ClaimCheckpointInvalid) isClaimCheckpointResult() {}

// RuntimeCheckpointPort 在 Task Runtime 所有的事务内保存和校验 Checkpoint。
type RuntimeCheckpointPort interface {
	SaveRuntimeCheckpoint(context.Context, contracts.RuntimeWriteTx, SaveRuntimeCheckpointRequest) error
	LoadLatestForClaim(
		context.Context,
		contracts.RuntimeWriteTx,
		contracts.TaskID,
		contracts.RunID,
		contracts.ExecutionVersion,
		ClaimCheckpointSource,
	) (ClaimCheckpointResult, error)
	LoadLatestForExecutionDispatch(
		context.Context,
		contracts.RuntimeWriteTx,
		contracts.TaskID,
		contracts.RunID,
		contracts.ExecutionVersion,
	) (ExecutionCheckpointResult, error)
}

// ExecutionCheckpointResult 是执行派发所需最大 Checkpoint 的封闭校验结果。
type ExecutionCheckpointResult interface {
	isExecutionCheckpointResult()
}

// ExecutionCheckpointValid 表示当前版本最大 Checkpoint 已通过持久化校验。
type ExecutionCheckpointValid struct {
	Checkpoint RuntimeCheckpoint
}

func (ExecutionCheckpointValid) isExecutionCheckpointResult() {}

// ExecutionCheckpointInvalid 表示最大 Checkpoint 缺失或损坏。
type ExecutionCheckpointInvalid struct {
	ReasonCode contracts.ReasonCode
}

func (ExecutionCheckpointInvalid) isExecutionCheckpointResult() {}
