// Package taskruntime 拥有 Task Runtime 的领域实体和应用层 Port。
package taskruntime

import (
	"encoding/json"
	"time"

	"github.com/zhaohaip/agentops-go/internal/contracts"
)

// CommandID 表示数据库内唯一的幂等命令标识。
type CommandID string

// CommandType 表示命令的稳定类型。
type CommandType string

const (
	CommandTypeCreate  CommandType = "Create"
	CommandTypeApprove CommandType = "Approve"
	CommandTypeReject  CommandType = "Reject"
	CommandTypeCancel  CommandType = "Cancel"
	CommandTypeRecover CommandType = "Recover"
)

// TaskExecutionID 表示一次执行尝试的唯一标识。
type TaskExecutionID string

// TaskLogID 表示一条附属日志的唯一标识。
type TaskLogID string

// TaskLogLevel 表示附属日志级别。
type TaskLogLevel string

const (
	TaskLogLevelInfo  TaskLogLevel = "Info"
	TaskLogLevelError TaskLogLevel = "Error"
)

// Task 是 Task Runtime 拥有的 Task 持久化实体。
type Task struct {
	TaskID                  contracts.TaskID
	AgentID                 contracts.AgentID
	CreatedBy               string
	Input                   string
	Status                  contracts.TaskStatus
	CurrentRunID            contracts.RunID
	CurrentExecutionVersion contracts.ExecutionVersion
	ResultSummary           *string
	ErrorCode               *contracts.ErrorCode
	DeadlineAt              time.Time
	QueuedAt                *time.Time
	CreatedAt               time.Time
	StartedAt               *time.Time
	EndedAt                 *time.Time
}

// Run 是 Task 唯一执行链的持久化实体。
type Run struct {
	RunID         contracts.RunID
	TaskID        contracts.TaskID
	Status        contracts.RunStatus
	PlanID        *contracts.PlanID
	CurrentStepID *contracts.StepID
	Context       json.RawMessage
	ErrorCode     *contracts.ErrorCode
	StartedAt     *time.Time
	EndedAt       *time.Time
}

// TaskExecution 是 Task 的一个有版本执行尝试。
type TaskExecution struct {
	TaskExecutionID     TaskExecutionID
	TaskID              contracts.TaskID
	ExecutionVersion    contracts.ExecutionVersion
	WorkerID            *contracts.WorkerID
	Status              contracts.TaskExecutionStatus
	ExecutionConfigHash contracts.ExecutionConfigHash
	ObservedConfigHash  *contracts.ExecutionConfigHash
	ErrorCode           *contracts.ErrorCode
	InvariantCode       *contracts.InvariantCode
	TerminationReason   *contracts.TerminationReason
	CreatedAt           time.Time
	StartedAt           *time.Time
	EndedAt             *time.Time
}

// CommandReceipt 是命令结果的不可变幂等记录。
type CommandReceipt struct {
	CommandID          CommandID
	CommandType        CommandType
	TargetID           string
	RequestFingerprint string
	Response           json.RawMessage
	CreatedAt          time.Time
}

// TaskLog 是不参与状态、恢复或幂等判断的附属日志。
type TaskLog struct {
	LogID            TaskLogID
	TaskID           contracts.TaskID
	RunID            contracts.RunID
	StepID           *contracts.StepID
	ExecutionVersion *contracts.ExecutionVersion
	Level            TaskLogLevel
	Event            string
	Message          string
	Operator         string
	CreatedAt        time.Time
}
