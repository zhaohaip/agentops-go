package contracts

// TaskStatus 表示 Task 的共享状态。
type TaskStatus string

const (
	TaskStatusPending         TaskStatus = "Pending"
	TaskStatusRunning         TaskStatus = "Running"
	TaskStatusWaitingApproval TaskStatus = "WaitingApproval"
	TaskStatusInterrupted     TaskStatus = "INTERRUPTED"
	TaskStatusCompleted       TaskStatus = "Completed"
	TaskStatusFailed          TaskStatus = "Failed"
	TaskStatusCancelled       TaskStatus = "Cancelled"
)

// Valid 报告 TaskStatus 是否属于封闭集合。
func (s TaskStatus) Valid() bool {
	switch s {
	case TaskStatusPending, TaskStatusRunning, TaskStatusWaitingApproval, TaskStatusInterrupted,
		TaskStatusCompleted, TaskStatusFailed, TaskStatusCancelled:
		return true
	default:
		return false
	}
}

// Terminal 报告 Task 是否处于业务终态。
func (s TaskStatus) Terminal() bool {
	return s == TaskStatusCompleted || s == TaskStatusFailed || s == TaskStatusCancelled
}

// RunStatus 表示 Run 的共享状态。
type RunStatus string

const (
	RunStatusPending         RunStatus = "Pending"
	RunStatusRunning         RunStatus = "Running"
	RunStatusWaitingApproval RunStatus = "WaitingApproval"
	RunStatusCompleted       RunStatus = "Completed"
	RunStatusFailed          RunStatus = "Failed"
)

// Valid 报告 RunStatus 是否属于封闭集合。
func (s RunStatus) Valid() bool {
	switch s {
	case RunStatusPending, RunStatusRunning, RunStatusWaitingApproval, RunStatusCompleted, RunStatusFailed:
		return true
	default:
		return false
	}
}

// Terminal 报告 Run 是否处于终态。
func (s RunStatus) Terminal() bool {
	return s == RunStatusCompleted || s == RunStatusFailed
}

// StepStatus 表示 Step 的共享状态。
type StepStatus string

const (
	StepStatusPending         StepStatus = "Pending"
	StepStatusRunning         StepStatus = "Running"
	StepStatusWaitingApproval StepStatus = "WaitingApproval"
	StepStatusCompleted       StepStatus = "Completed"
	StepStatusFailed          StepStatus = "Failed"
)

// Valid 报告 StepStatus 是否属于封闭集合。
func (s StepStatus) Valid() bool {
	switch s {
	case StepStatusPending, StepStatusRunning, StepStatusWaitingApproval, StepStatusCompleted, StepStatusFailed:
		return true
	default:
		return false
	}
}

// Terminal 报告 Step 是否处于终态。
func (s StepStatus) Terminal() bool {
	return s == StepStatusCompleted || s == StepStatusFailed
}

// TaskExecutionStatus 表示一次 TaskExecution 尝试的状态。
type TaskExecutionStatus string

const (
	TaskExecutionStatusQueued          TaskExecutionStatus = "QUEUED"
	TaskExecutionStatusRunning         TaskExecutionStatus = "RUNNING"
	TaskExecutionStatusWaitingApproval TaskExecutionStatus = "WAITING_APPROVAL"
	TaskExecutionStatusCompleted       TaskExecutionStatus = "COMPLETED"
	TaskExecutionStatusFailed          TaskExecutionStatus = "FAILED"
	TaskExecutionStatusInterrupted     TaskExecutionStatus = "INTERRUPTED"
)

// Valid 报告 TaskExecutionStatus 是否属于封闭集合。
func (s TaskExecutionStatus) Valid() bool {
	switch s {
	case TaskExecutionStatusQueued, TaskExecutionStatusRunning, TaskExecutionStatusWaitingApproval,
		TaskExecutionStatusCompleted, TaskExecutionStatusFailed, TaskExecutionStatusInterrupted:
		return true
	default:
		return false
	}
}

// Ended 报告当前执行尝试是否结束。
func (s TaskExecutionStatus) Ended() bool {
	return s == TaskExecutionStatusCompleted || s == TaskExecutionStatusFailed || s == TaskExecutionStatusInterrupted
}

// ToolExecutionStatus 表示 ToolExecution 的共享状态。
type ToolExecutionStatus string

const (
	ToolExecutionStatusRunning   ToolExecutionStatus = "RUNNING"
	ToolExecutionStatusCompleted ToolExecutionStatus = "COMPLETED"
	ToolExecutionStatusFailed    ToolExecutionStatus = "FAILED"
	ToolExecutionStatusUnknown   ToolExecutionStatus = "UNKNOWN"
)

// Valid 报告 ToolExecutionStatus 是否属于封闭集合。
func (s ToolExecutionStatus) Valid() bool {
	switch s {
	case ToolExecutionStatusRunning, ToolExecutionStatusCompleted, ToolExecutionStatusFailed, ToolExecutionStatusUnknown:
		return true
	default:
		return false
	}
}

// Terminal 报告 ToolExecution 是否处于终态。
func (s ToolExecutionStatus) Terminal() bool {
	return s == ToolExecutionStatusCompleted || s == ToolExecutionStatusFailed || s == ToolExecutionStatusUnknown
}

// ApprovalStatus 表示 Approval 的共享状态。
type ApprovalStatus string

const (
	ApprovalStatusPending  ApprovalStatus = "Pending"
	ApprovalStatusApproved ApprovalStatus = "Approved"
	ApprovalStatusRejected ApprovalStatus = "Rejected"
)

// Valid 报告 ApprovalStatus 是否属于封闭集合。
func (s ApprovalStatus) Valid() bool {
	return s == ApprovalStatusPending || s == ApprovalStatusApproved || s == ApprovalStatusRejected
}

// Terminal 报告 Approval 是否处于不可变终态。
func (s ApprovalStatus) Terminal() bool {
	return s == ApprovalStatusApproved || s == ApprovalStatusRejected
}

// ReportStatus 表示 Report 的共享状态。
type ReportStatus string

const (
	ReportStatusPending    ReportStatus = "Pending"
	ReportStatusGenerating ReportStatus = "Generating"
	ReportStatusCompleted  ReportStatus = "Completed"
	ReportStatusFailed     ReportStatus = "Failed"
)

// Valid 报告 ReportStatus 是否属于封闭集合。
func (s ReportStatus) Valid() bool {
	switch s {
	case ReportStatusPending, ReportStatusGenerating, ReportStatusCompleted, ReportStatusFailed:
		return true
	default:
		return false
	}
}

// Terminal 报告 Report 是否处于终态。
func (s ReportStatus) Terminal() bool {
	return s == ReportStatusCompleted || s == ReportStatusFailed
}
