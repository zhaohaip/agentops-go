package contracts

import "errors"

// ExecutionCancellationCause 是外部执行调用允许传播的封闭取消原因。
type ExecutionCancellationCause string

const (
	ExecutionCancellationCauseTaskCancelled   ExecutionCancellationCause = "TASK_CANCELLED"
	ExecutionCancellationCauseTaskTimedOut    ExecutionCancellationCause = "TASK_TIMED_OUT"
	ExecutionCancellationCauseActionTimeout   ExecutionCancellationCause = "ACTION_TIMEOUT"
	ExecutionCancellationCauseRuntimeShutdown ExecutionCancellationCause = "RUNTIME_SHUTDOWN"
	ExecutionCancellationCauseLockLost        ExecutionCancellationCause = "LOCK_LOST"
)

// Error 使取消原因能够通过 context.WithCancelCause 传播。
func (c ExecutionCancellationCause) Error() string { return string(c) }

// Valid 报告取消原因是否属于冻结集合。
func (c ExecutionCancellationCause) Valid() bool {
	switch c {
	case ExecutionCancellationCauseTaskCancelled, ExecutionCancellationCauseTaskTimedOut,
		ExecutionCancellationCauseActionTimeout, ExecutionCancellationCauseRuntimeShutdown,
		ExecutionCancellationCauseLockLost:
		return true
	default:
		return false
	}
}

// ExecutionCancellationCauseFrom 从错误链中读取共享类型化取消原因。
func ExecutionCancellationCauseFrom(err error) (ExecutionCancellationCause, bool) {
	var cause ExecutionCancellationCause
	if !errors.As(err, &cause) || !cause.Valid() {
		return "", false
	}
	return cause, true
}
