package contracts

import "time"

// ExecutionScope 是 Task Runtime 构造的唯一进程内执行关联 DTO。
type ExecutionScope struct {
	TaskID              TaskID              `json:"task_id"`
	RunID               RunID               `json:"run_id"`
	ExecutionVersion    ExecutionVersion    `json:"execution_version"`
	ExecutionConfigHash ExecutionConfigHash `json:"execution_config_hash"`
	WorkerID            WorkerID            `json:"worker_id"`
	StepID              StepID              `json:"step_id"`
	DeadlineAt          time.Time           `json:"deadline_at"`
}

// CheckpointNextAction 表示 Checkpoint 冻结的下一动作。
type CheckpointNextAction string

const (
	CheckpointNextActionGeneratePlan        CheckpointNextAction = "GENERATE_PLAN"
	CheckpointNextActionExecuteStep         CheckpointNextAction = "EXECUTE_STEP"
	CheckpointNextActionRequestApproval     CheckpointNextAction = "REQUEST_APPROVAL"
	CheckpointNextActionExecuteApprovedTool CheckpointNextAction = "EXECUTE_APPROVED_TOOL"
	CheckpointNextActionFinalizeRun         CheckpointNextAction = "FINALIZE_RUN"
)

// Valid 报告 CheckpointNextAction 是否属于封闭集合。
func (a CheckpointNextAction) Valid() bool {
	switch a {
	case CheckpointNextActionGeneratePlan, CheckpointNextActionExecuteStep,
		CheckpointNextActionRequestApproval, CheckpointNextActionExecuteApprovedTool,
		CheckpointNextActionFinalizeRun:
		return true
	default:
		return false
	}
}

// StepOutcomeKind 表示 Step Executor 共享结果的封闭分支。
//
// 各分支的模块专用载荷仍由 Step Executor 拥有。
type StepOutcomeKind string

const (
	StepOutcomeCompleted       StepOutcomeKind = "Completed"
	StepOutcomeWaitingApproval StepOutcomeKind = "WaitingApproval"
	StepOutcomeTerminalized    StepOutcomeKind = "Terminalized"
	StepOutcomeFailed          StepOutcomeKind = "Failed"
	StepOutcomeStale           StepOutcomeKind = "Stale"
)

// Valid 报告 StepOutcomeKind 是否属于封闭集合。
func (k StepOutcomeKind) Valid() bool {
	switch k {
	case StepOutcomeCompleted, StepOutcomeWaitingApproval, StepOutcomeTerminalized,
		StepOutcomeFailed, StepOutcomeStale:
		return true
	default:
		return false
	}
}

// StepContinuationKind 表示成功 Step 的后续动作。
type StepContinuationKind string

const (
	StepContinuationNextStep    StepContinuationKind = "NEXT_STEP"
	StepContinuationFinalizeRun StepContinuationKind = "FINALIZE_RUN"
)

// Valid 报告 StepContinuationKind 是否属于封闭集合。
func (k StepContinuationKind) Valid() bool {
	return k == StepContinuationNextStep || k == StepContinuationFinalizeRun
}

// ExecutionClaim 表示 Task Runtime 已原子领取的执行尝试。
type ExecutionClaim struct {
	TaskID           TaskID           `json:"task_id"`
	RunID            RunID            `json:"run_id"`
	ExecutionVersion ExecutionVersion `json:"execution_version"`
	WorkerID         WorkerID         `json:"worker_id"`
	ClaimedAt        time.Time        `json:"claimed_at"`
}

// ClaimResult 是 Worker Claim 的封闭结果。
type ClaimResult interface {
	isClaimResult()
}

// ClaimResultClaimed 表示成功领取一个 ExecutionClaim。
type ClaimResultClaimed struct {
	Claim ExecutionClaim
}

func (ClaimResultClaimed) isClaimResult() {}

// ClaimResultNoWork 表示当前没有可领取工作。
type ClaimResultNoWork struct{}

func (ClaimResultNoWork) isClaimResult() {}

// ClaimResultConfigMismatchInterrupted 表示配置失配已原子中断并出队。
type ClaimResultConfigMismatchInterrupted struct{}

func (ClaimResultConfigMismatchInterrupted) isClaimResult() {}

// ClaimResultCheckpointInvalidTerminalized 表示 CheckpointInvalid 已原子终态化。
type ClaimResultCheckpointInvalidTerminalized struct{}

func (ClaimResultCheckpointInvalidTerminalized) isClaimResult() {}

// ClaimResultDataInconsistentTerminalized 表示数据不一致已原子终态化。
type ClaimResultDataInconsistentTerminalized struct{}

func (ClaimResultDataInconsistentTerminalized) isClaimResult() {}

// ClaimResultExpiredTerminalized 表示超时已原子终态化。
type ClaimResultExpiredTerminalized struct{}

func (ClaimResultExpiredTerminalized) isClaimResult() {}

// ExecuteResult 是 Worker Execute 的封闭结果。
type ExecuteResult interface {
	isExecuteResult()
}

// ExecuteResultWaitingApproval 表示执行已进入 WaitingApproval。
type ExecuteResultWaitingApproval struct{}

func (ExecuteResultWaitingApproval) isExecuteResult() {}

// ExecuteResultTerminal 表示本轮执行已进入终止结果。
type ExecuteResultTerminal struct{}

func (ExecuteResultTerminal) isExecuteResult() {}

// ExecuteResultStale 表示执行事实已因合法竞争过期。
type ExecuteResultStale struct{}

func (ExecuteResultStale) isExecuteResult() {}
