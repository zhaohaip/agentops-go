// Package lifecycle 实现无状态、无 I/O 的 Task Lifecycle Policy。
package lifecycle

import (
	"time"

	"github.com/zhaohaip/agentops-go/internal/contracts"
)

// RejectionReason 是 Lifecycle Policy 的稳定拒绝原因。
type RejectionReason string

const (
	RejectionNone               RejectionReason = ""
	RejectionInvalidState       RejectionReason = "INVALID_STATE"
	RejectionVersionMismatch    RejectionReason = "VERSION_MISMATCH"
	RejectionWorkerMismatch     RejectionReason = "WORKER_MISMATCH"
	RejectionDeadlineReached    RejectionReason = "DEADLINE_REACHED"
	RejectionApprovalNotPending RejectionReason = "APPROVAL_NOT_PENDING"
	RejectionInvalidSource      RejectionReason = "INVALID_SOURCE"
)

// Decision 表示一次纯规则判定。
type Decision struct {
	Allowed bool
	Reason  RejectionReason
}

// Policy 集中保存 Task Runtime 与 Approval 共用的生命周期规则。
type Policy struct{}

// New 创建一个无状态 Lifecycle Policy。
func New() Policy {
	return Policy{}
}

// GuardFacts 是所有会推进当前执行的共享版本、所有权和 deadline 事实。
type GuardFacts struct {
	CurrentExecutionVersion contracts.ExecutionVersion
	RequestExecutionVersion contracts.ExecutionVersion
	ExecutionWorkerID       *contracts.WorkerID
	RequestWorkerID         *contracts.WorkerID
	DeadlineAt              time.Time
	DatabaseNow             time.Time
}

// CheckGuard 原子事实加载完成后校验版本、worker ownership 和数据库 deadline。
func (Policy) CheckGuard(facts GuardFacts) Decision {
	if !facts.CurrentExecutionVersion.Valid() || facts.CurrentExecutionVersion != facts.RequestExecutionVersion {
		return rejected(RejectionVersionMismatch)
	}
	if facts.RequestWorkerID != nil {
		if facts.ExecutionWorkerID == nil || *facts.ExecutionWorkerID != *facts.RequestWorkerID || *facts.RequestWorkerID == "" {
			return rejected(RejectionWorkerMismatch)
		}
	}
	if facts.DeadlineAt.IsZero() || !facts.DatabaseNow.Before(facts.DeadlineAt) {
		return rejected(RejectionDeadlineReached)
	}
	return allowed()
}

// CanTaskTransition 校验冻结的 Task 状态机边。
func (Policy) CanTaskTransition(from, to contracts.TaskStatus) Decision {
	if !from.Valid() || !to.Valid() || from == to {
		return rejected(RejectionInvalidState)
	}
	valid := false
	switch from {
	case contracts.TaskStatusPending:
		valid = to == contracts.TaskStatusRunning || to == contracts.TaskStatusInterrupted ||
			to == contracts.TaskStatusCancelled || to == contracts.TaskStatusFailed
	case contracts.TaskStatusRunning:
		valid = to == contracts.TaskStatusWaitingApproval || to == contracts.TaskStatusInterrupted ||
			to == contracts.TaskStatusCompleted || to == contracts.TaskStatusFailed || to == contracts.TaskStatusCancelled
	case contracts.TaskStatusWaitingApproval:
		valid = to == contracts.TaskStatusRunning || to == contracts.TaskStatusCancelled || to == contracts.TaskStatusFailed
	case contracts.TaskStatusInterrupted:
		valid = to == contracts.TaskStatusPending || to == contracts.TaskStatusRunning ||
			to == contracts.TaskStatusCancelled || to == contracts.TaskStatusFailed
	}
	if !valid {
		return rejected(RejectionInvalidState)
	}
	return allowed()
}

// CanRunTransition 校验冻结的 Run 状态机边。
func (Policy) CanRunTransition(from, to contracts.RunStatus) Decision {
	if !from.Valid() || !to.Valid() || from == to {
		return rejected(RejectionInvalidState)
	}
	valid := false
	switch from {
	case contracts.RunStatusPending:
		valid = to == contracts.RunStatusRunning || to == contracts.RunStatusFailed
	case contracts.RunStatusRunning:
		valid = to == contracts.RunStatusWaitingApproval || to == contracts.RunStatusCompleted || to == contracts.RunStatusFailed
	case contracts.RunStatusWaitingApproval:
		valid = to == contracts.RunStatusRunning || to == contracts.RunStatusFailed
	}
	if !valid {
		return rejected(RejectionInvalidState)
	}
	return allowed()
}

// CanStepTransition 校验冻结的 Step 状态机边。
func (Policy) CanStepTransition(from, to contracts.StepStatus) Decision {
	if !from.Valid() || !to.Valid() || from == to {
		return rejected(RejectionInvalidState)
	}
	valid := false
	switch from {
	case contracts.StepStatusPending:
		valid = to == contracts.StepStatusRunning || to == contracts.StepStatusFailed
	case contracts.StepStatusRunning:
		valid = to == contracts.StepStatusWaitingApproval || to == contracts.StepStatusCompleted || to == contracts.StepStatusFailed
	case contracts.StepStatusWaitingApproval:
		valid = to == contracts.StepStatusRunning || to == contracts.StepStatusFailed
	}
	if !valid {
		return rejected(RejectionInvalidState)
	}
	return allowed()
}

// CanExecutionTransition 校验冻结的 TaskExecution 状态机边。
func (Policy) CanExecutionTransition(from, to contracts.TaskExecutionStatus) Decision {
	if !from.Valid() || !to.Valid() || from == to {
		return rejected(RejectionInvalidState)
	}
	valid := false
	switch from {
	case contracts.TaskExecutionStatusQueued:
		valid = to == contracts.TaskExecutionStatusRunning || to == contracts.TaskExecutionStatusInterrupted ||
			to == contracts.TaskExecutionStatusFailed
	case contracts.TaskExecutionStatusRunning:
		valid = to == contracts.TaskExecutionStatusWaitingApproval || to == contracts.TaskExecutionStatusCompleted ||
			to == contracts.TaskExecutionStatusFailed || to == contracts.TaskExecutionStatusInterrupted
	case contracts.TaskExecutionStatusWaitingApproval:
		valid = to == contracts.TaskExecutionStatusQueued || to == contracts.TaskExecutionStatusFailed
	case contracts.TaskExecutionStatusInterrupted:
		valid = to == contracts.TaskExecutionStatusFailed
	}
	if !valid {
		return rejected(RejectionInvalidState)
	}
	return allowed()
}

// ApprovalFacts 是审批转换已经锁定的最小跨对象事实。
type ApprovalFacts struct {
	TaskStatus              contracts.TaskStatus
	RunStatus               contracts.RunStatus
	StepStatus              contracts.StepStatus
	ExecutionStatus         contracts.TaskExecutionStatus
	ApprovalStatus          contracts.ApprovalStatus
	CurrentExecutionVersion contracts.ExecutionVersion
	RequestExecutionVersion contracts.ExecutionVersion
	ExecutionWorkerID       *contracts.WorkerID
	RequestWorkerID         *contracts.WorkerID
	DeadlineAt              time.Time
	DatabaseNow             time.Time
}

// CanEnterWaitingApproval 校验 Running 现场进入 WaitingApproval。
func (policy Policy) CanEnterWaitingApproval(facts ApprovalFacts) Decision {
	if facts.RequestWorkerID == nil {
		return rejected(RejectionWorkerMismatch)
	}
	if decision := policy.CheckGuard(guardFacts(facts)); !decision.Allowed {
		return decision
	}
	if facts.TaskStatus != contracts.TaskStatusRunning || facts.RunStatus != contracts.RunStatusRunning ||
		facts.StepStatus != contracts.StepStatusRunning || facts.ExecutionStatus != contracts.TaskExecutionStatusRunning {
		return rejected(RejectionInvalidState)
	}
	return allowed()
}

// CanApprove 校验 Pending Approval 的 WaitingApproval 现场重新进入运行队列。
func (policy Policy) CanApprove(facts ApprovalFacts) Decision {
	if decision := policy.checkWaitingApproval(facts); !decision.Allowed {
		return decision
	}
	return allowed()
}

// CanReject 校验 Pending Approval 的 WaitingApproval 现场进入取消终态。
func (policy Policy) CanReject(facts ApprovalFacts) Decision {
	if decision := policy.checkWaitingApproval(facts); !decision.Allowed {
		return decision
	}
	return allowed()
}

// CheckpointInvalidSource 表示审批入口发现 CheckpointInvalid 的来源。
type CheckpointInvalidSource string

const (
	CheckpointInvalidSourceRequestApproval CheckpointInvalidSource = "RequestApproval"
	CheckpointInvalidSourceApprove         CheckpointInvalidSource = "Approve"
	CheckpointInvalidSourceReject          CheckpointInvalidSource = "Reject"
)

// CanTerminalizeCheckpointInvalid 在调用方完成 Checkpoint 归属校验后授权审批入口终态化。
func (policy Policy) CanTerminalizeCheckpointInvalid(source CheckpointInvalidSource, facts ApprovalFacts) Decision {
	switch source {
	case CheckpointInvalidSourceRequestApproval:
		return policy.CanEnterWaitingApproval(facts)
	case CheckpointInvalidSourceApprove, CheckpointInvalidSourceReject:
		return policy.checkWaitingApproval(facts)
	default:
		return rejected(RejectionInvalidSource)
	}
}

func (policy Policy) checkWaitingApproval(facts ApprovalFacts) Decision {
	if facts.RequestWorkerID != nil {
		return rejected(RejectionWorkerMismatch)
	}
	withoutWorker := facts
	withoutWorker.RequestWorkerID = nil
	if decision := policy.CheckGuard(guardFacts(withoutWorker)); !decision.Allowed {
		return decision
	}
	if facts.ExecutionWorkerID != nil {
		return rejected(RejectionWorkerMismatch)
	}
	if facts.ApprovalStatus != contracts.ApprovalStatusPending {
		return rejected(RejectionApprovalNotPending)
	}
	if facts.TaskStatus != contracts.TaskStatusWaitingApproval || facts.RunStatus != contracts.RunStatusWaitingApproval ||
		facts.StepStatus != contracts.StepStatusWaitingApproval ||
		facts.ExecutionStatus != contracts.TaskExecutionStatusWaitingApproval {
		return rejected(RejectionInvalidState)
	}
	return allowed()
}

func guardFacts(facts ApprovalFacts) GuardFacts {
	return GuardFacts{
		CurrentExecutionVersion: facts.CurrentExecutionVersion,
		RequestExecutionVersion: facts.RequestExecutionVersion,
		ExecutionWorkerID:       facts.ExecutionWorkerID,
		RequestWorkerID:         facts.RequestWorkerID,
		DeadlineAt:              facts.DeadlineAt,
		DatabaseNow:             facts.DatabaseNow,
	}
}

func allowed() Decision {
	return Decision{Allowed: true}
}

func rejected(reason RejectionReason) Decision {
	return Decision{Reason: reason}
}
