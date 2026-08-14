package taskruntime

import (
	"context"
	"fmt"
	"time"

	"github.com/zhaohaip/agentops-go/internal/contracts"
)

const (
	taskLogWriteTimeout = 50 * time.Millisecond
	taskLogOperator     = "System"

	taskLogEventTaskCreated          = "TaskCreated"
	taskLogEventExecutionClaimed     = "ExecutionClaimed"
	taskLogEventExecutionInterrupted = "ExecutionInterrupted"
	taskLogEventTaskTerminalized     = "TaskTerminalized"
	taskLogEventCheckpointRestored   = "CheckpointRestored"
)

type taskLogDraft struct {
	taskID           contracts.TaskID
	runID            contracts.RunID
	executionVersion contracts.ExecutionVersion
	level            TaskLogLevel
	event            string
	message          string
}

// appendTaskLogBestEffort 在核心事务提交后尝试一次独立的短日志事务。
// TaskLog 不是领域事实；连接繁忙、关闭或持久化失败时均允许丢弃。
func appendTaskLogBestEffort(
	parent context.Context,
	executor contracts.RuntimeWriteExecutor,
	logs TaskLogRepository,
	clock DatabaseClock,
	draft taskLogDraft,
) {
	if parent == nil || executor == nil || logs == nil || clock == nil ||
		draft.taskID == "" || draft.runID == "" || !draft.executionVersion.Valid() {
		return
	}
	logID, err := randomID("task-log")
	if err != nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(parent), taskLogWriteTimeout)
	defer cancel()

	_, err = executor.TryExecute(ctx, func(ctx context.Context, tx contracts.RuntimeWriteTx) error {
		createdAt, clockErr := clock.Now(ctx, tx)
		if clockErr != nil {
			return fmt.Errorf("read TaskLog database clock: %w", clockErr)
		}
		version := draft.executionVersion
		return logs.Append(ctx, tx, TaskLog{
			LogID: TaskLogID(logID), TaskID: draft.taskID, RunID: draft.runID,
			ExecutionVersion: &version, Level: draft.level, Event: draft.event,
			Message: draft.message, Operator: taskLogOperator, CreatedAt: createdAt,
		})
	})
	if err != nil {
		return
	}
}

func claimTaskLogDraft(result contracts.ClaimResult, candidate QueueCandidate) (taskLogDraft, bool) {
	switch typed := result.(type) {
	case contracts.ClaimResultClaimed:
		return taskLogDraft{
			taskID: typed.Claim.TaskID, runID: typed.Claim.RunID,
			executionVersion: typed.Claim.ExecutionVersion, level: TaskLogLevelInfo,
			event: taskLogEventExecutionClaimed, message: "execution claimed",
		}, true
	case contracts.ClaimResultConfigMismatchInterrupted:
		return taskLogDraft{
			taskID: candidate.TaskID, runID: candidate.RunID, executionVersion: candidate.ExecutionVersion,
			level: TaskLogLevelError, event: taskLogEventExecutionInterrupted,
			message: "execution interrupted: error_code=" + string(contracts.ErrorCodeConfigVersionMismatch),
		}, true
	case contracts.ClaimResultCheckpointInvalidTerminalized:
		return terminalTaskLogDraft(candidate, contracts.ErrorCodeCheckpointInvalid, nil), true
	case contracts.ClaimResultDataInconsistentTerminalized:
		return terminalTaskLogDraft(candidate, contracts.ErrorCodeDataInconsistent, nil), true
	case contracts.ClaimResultExpiredTerminalized:
		reason := contracts.TerminationReasonTimedOut
		return terminalTaskLogDraft(candidate, contracts.ErrorCodeTaskTimeout, &reason), true
	default:
		return taskLogDraft{}, false
	}
}

func terminalTaskLogDraft(
	candidate QueueCandidate,
	errorCode contracts.ErrorCode,
	terminationReason *contracts.TerminationReason,
) taskLogDraft {
	message := "task terminalized: status=" + string(contracts.TaskStatusFailed) +
		" error_code=" + string(errorCode)
	if terminationReason != nil {
		message += " termination_reason=" + string(*terminationReason)
	}
	return taskLogDraft{
		taskID: candidate.TaskID, runID: candidate.RunID, executionVersion: candidate.ExecutionVersion,
		level: TaskLogLevelError, event: taskLogEventTaskTerminalized, message: message,
	}
}

func checkpointRestoredTaskLogDraft(
	taskID contracts.TaskID,
	runID contracts.RunID,
	sourceExecutionVersion contracts.ExecutionVersion,
	newExecutionVersion contracts.ExecutionVersion,
) taskLogDraft {
	return taskLogDraft{
		taskID: taskID, runID: runID, executionVersion: newExecutionVersion,
		level: TaskLogLevelInfo, event: taskLogEventCheckpointRestored,
		message: fmt.Sprintf(
			"checkpoint restored: source_execution_version=%d new_execution_version=%d",
			sourceExecutionVersion,
			newExecutionVersion,
		),
	}
}
