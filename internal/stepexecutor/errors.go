package stepexecutor

import (
	"errors"
	"fmt"

	"github.com/zhaohaip/agentops-go/internal/contracts"
)

// CauseCode 是 Step Executor 对外公开的稳定、安全原因码。
type CauseCode string

const (
	CauseStepExecutorContractBroken            CauseCode = "STEP_EXECUTOR_CONTRACT_BROKEN"
	CauseRuntimeInvalidModelClientRequest      CauseCode = "RUNTIME_INVALID_MODEL_CLIENT_REQUEST"
	CauseRuntimeStaticToolSnapshotInconsistent CauseCode = "RUNTIME_STATIC_TOOL_SNAPSHOT_INCONSISTENT"
	CausePersistenceInvariantViolation         CauseCode = "PersistenceInvariantViolation"
	CauseReferenceCountLimitExceeded           CauseCode = "REFERENCE_COUNT_LIMIT_EXCEEDED"
	CauseInputResolutionFailed                 CauseCode = "InputResolutionFailed"
	CauseTaskCancelled                         CauseCode = CauseCode(contracts.ExecutionCancellationCauseTaskCancelled)
	CauseTaskTimedOut                          CauseCode = CauseCode(contracts.ExecutionCancellationCauseTaskTimedOut)
	CauseActionTimeout                         CauseCode = CauseCode(contracts.ExecutionCancellationCauseActionTimeout)
	CauseRuntimeShutdown                       CauseCode = CauseCode(contracts.ExecutionCancellationCauseRuntimeShutdown)
	CauseLockLost                              CauseCode = CauseCode(contracts.ExecutionCancellationCauseLockLost)
	CauseStaleExecution                        CauseCode = "STALE_EXECUTION"
	CauseModelTimeout                          CauseCode = "MODEL_TIMEOUT"
	CauseModelAuthentication                   CauseCode = "MODEL_AUTHENTICATION"
	CauseModelNetwork                          CauseCode = "MODEL_NETWORK"
	CauseModelRateLimited                      CauseCode = "MODEL_RATE_LIMITED"
	CauseModelProviderError                    CauseCode = "MODEL_PROVIDER_ERROR"
	CauseModelResponseTooLarge                 CauseCode = "MODEL_RESPONSE_TOO_LARGE"
	CauseModelOutputInvalid                    CauseCode = "MODEL_OUTPUT_INVALID"
	CauseModelInputTooLarge                    CauseCode = "MODEL_INPUT_TOO_LARGE"
	CauseResultSanitizationFailed              CauseCode = CauseCode(contracts.CauseCodeResultSanitizationFailed)
	CauseStepOutputTooLarge                    CauseCode = CauseCode(contracts.CauseCodeStepOutputTooLarge)
)

// Valid 报告原因码是否属于冻结集合。
func (c CauseCode) Valid() bool {
	switch c {
	case CauseStepExecutorContractBroken, CauseRuntimeInvalidModelClientRequest,
		CauseRuntimeStaticToolSnapshotInconsistent, CausePersistenceInvariantViolation,
		CauseReferenceCountLimitExceeded,
		CauseInputResolutionFailed,
		CauseTaskCancelled, CauseTaskTimedOut, CauseActionTimeout, CauseRuntimeShutdown,
		CauseLockLost, CauseStaleExecution, CauseModelTimeout, CauseModelAuthentication,
		CauseModelNetwork, CauseModelRateLimited, CauseModelProviderError,
		CauseModelResponseTooLarge, CauseModelOutputInvalid, CauseModelInputTooLarge,
		CauseResultSanitizationFailed, CauseStepOutputTooLarge:
		return true
	default:
		return false
	}
}

// Stale 报告原因是否允许出现在 Stale Outcome。
func (c CauseCode) Stale() bool {
	switch c {
	case CauseTaskCancelled, CauseTaskTimedOut, CauseRuntimeShutdown, CauseLockLost, CauseStaleExecution:
		return true
	default:
		return false
	}
}

// RuntimeFatal 报告原因是否只能经独立系统错误通道返回。
func (c CauseCode) RuntimeFatal() bool {
	switch c {
	case CauseStepExecutorContractBroken, CauseRuntimeInvalidModelClientRequest,
		CauseRuntimeStaticToolSnapshotInconsistent, CausePersistenceInvariantViolation,
		CauseReferenceCountLimitExceeded:
		return true
	default:
		return false
	}
}

// ErrorKind 是依赖错误映射后的封闭作用域。
type ErrorKind string

const (
	ErrorKindFailed       ErrorKind = "Failed"
	ErrorKindStale        ErrorKind = "Stale"
	ErrorKindRuntimeFatal ErrorKind = "RuntimeFatal"
)

// Valid 报告错误作用域是否属于冻结集合。
func (k ErrorKind) Valid() bool {
	return k == ErrorKindFailed || k == ErrorKindStale || k == ErrorKindRuntimeFatal
}

// StepError 是依赖错误映射后的安全分类，不公开底层错误文本。
type StepError struct {
	Kind      ErrorKind
	ErrorCode contracts.ErrorCode
	CauseCode CauseCode
	cause     error
}

// Error 返回稳定且不包含底层 Provider 错误文本的描述。
func (e *StepError) Error() string {
	if e == nil {
		return "Step Executor error"
	}
	return fmt.Sprintf("Step Executor error: %s/%s/%s", e.Kind, e.ErrorCode, e.CauseCode)
}

// Unwrap 保留 context 取消和 deadline 的 errors.Is 语义。
func (e *StepError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.cause
}

// MapModelClientError 按 Model Client 的类型化类别生成稳定 StepError。
//
// Canceled 的业务取消原因由调用侧 context 决定，因此这里只保留为 Stale；具体原因在
// Model Step 执行器处理 context 时替换，不能解析错误字符串猜测。
func MapModelClientError(err error) *StepError {
	var typed *contracts.ModelClientError
	if !errors.As(err, &typed) || typed == nil || !typed.Kind.Valid() {
		return NewRuntimeFatalError(
			contracts.ErrorCodeStepExecutorContractBroken, CauseStepExecutorContractBroken, err,
		)
	}

	switch typed.Kind {
	case contracts.ModelClientErrorCanceled:
		return newStepError(ErrorKindStale, "", CauseStaleExecution, err)
	case contracts.ModelClientErrorTimeout:
		return newStepError(ErrorKindFailed, contracts.ErrorCodeModelCallFailed, CauseModelTimeout, err)
	case contracts.ModelClientErrorAuthentication:
		return newStepError(ErrorKindFailed, contracts.ErrorCodeModelCallFailed, CauseModelAuthentication, err)
	case contracts.ModelClientErrorNetwork:
		return newStepError(ErrorKindFailed, contracts.ErrorCodeModelCallFailed, CauseModelNetwork, err)
	case contracts.ModelClientErrorRateLimited:
		return newStepError(ErrorKindFailed, contracts.ErrorCodeModelCallFailed, CauseModelRateLimited, err)
	case contracts.ModelClientErrorProvider:
		return newStepError(ErrorKindFailed, contracts.ErrorCodeModelCallFailed, CauseModelProviderError, err)
	case contracts.ModelClientErrorResponseTooLarge:
		return newStepError(ErrorKindFailed, contracts.ErrorCodeModelCallFailed, CauseModelResponseTooLarge, err)
	case contracts.ModelClientErrorInvalidResponse:
		return newStepError(ErrorKindFailed, contracts.ErrorCodeModelOutputInvalid, CauseModelOutputInvalid, err)
	case contracts.ModelClientErrorContractViolation:
		return NewRuntimeFatalError(
			contracts.ErrorCodeStepExecutorContractBroken, CauseRuntimeInvalidModelClientRequest, err,
		)
	default:
		return NewRuntimeFatalError(
			contracts.ErrorCodeStepExecutorContractBroken, CauseStepExecutorContractBroken, err,
		)
	}
}

// NewRuntimeFatalError 把依赖的类型化 Runtime Fatal 映射到独立系统错误通道。
func NewRuntimeFatalError(errorCode contracts.ErrorCode, causeCode CauseCode, cause error) *StepError {
	if !validRuntimeFatalPair(errorCode, causeCode) {
		return newStepError(ErrorKindRuntimeFatal, contracts.ErrorCodeStepExecutorContractBroken,
			CauseStepExecutorContractBroken, cause)
	}
	return newStepError(ErrorKindRuntimeFatal, errorCode, causeCode, cause)
}

func validRuntimeFatalPair(errorCode contracts.ErrorCode, causeCode CauseCode) bool {
	switch errorCode {
	case contracts.ErrorCodeStepExecutorContractBroken:
		switch causeCode {
		case CauseStepExecutorContractBroken, CauseRuntimeInvalidModelClientRequest,
			CauseReferenceCountLimitExceeded:
			return true
		}
	case contracts.ErrorCodeRuntimeStaticToolSnapshotInconsistent:
		return causeCode == CauseRuntimeStaticToolSnapshotInconsistent
	case contracts.ErrorCodePersistenceInvariantViolation:
		return causeCode == CausePersistenceInvariantViolation
	}
	return false
}

func validFailedPair(errorCode contracts.ErrorCode, causeCode CauseCode) bool {
	if errorCode == contracts.ErrorCodeModelInputTooLarge || causeCode == CauseModelInputTooLarge {
		return errorCode == contracts.ErrorCodeModelInputTooLarge && causeCode == CauseModelInputTooLarge
	}
	return errorCode.Valid() && causeCode.Valid()
}

// MapToolRuntimeFatal 把 Tool Framework 的类型化 RuntimeFatal 映射到系统错误通道。
func MapToolRuntimeFatal(result contracts.ToolRuntimeFatal) *StepError {
	return NewRuntimeFatalError(result.ErrorCode, mapSharedRuntimeCause(result.SafeCauseCode), nil)
}

// MapApprovalRuntimeFatal 把 Approval 的类型化 RuntimeFatal 映射到系统错误通道。
func MapApprovalRuntimeFatal(result contracts.ApprovalRequestRuntimeFatal) *StepError {
	return NewRuntimeFatalError(result.ErrorCode, mapSharedRuntimeCause(result.CauseCode), nil)
}

func mapSharedRuntimeCause(cause contracts.CauseCode) CauseCode {
	switch cause {
	case contracts.CauseCodeStepExecutorContractBroken:
		return CauseStepExecutorContractBroken
	case contracts.CauseCodeRuntimeStaticToolSnapshotInconsistent:
		return CauseRuntimeStaticToolSnapshotInconsistent
	case contracts.CauseCodePersistenceInvariantViolation:
		return CausePersistenceInvariantViolation
	default:
		return CauseStepExecutorContractBroken
	}
}

func newStepError(kind ErrorKind, errorCode contracts.ErrorCode, causeCode CauseCode, cause error) *StepError {
	return &StepError{Kind: kind, ErrorCode: errorCode, CauseCode: causeCode, cause: cause}
}
