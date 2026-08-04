package contracts

// ErrorCode 表示持久化领域对象或确定性命令结果的稳定分类。
type ErrorCode string

const (
	ErrorCodeTaskCancelled                         ErrorCode = "TaskCancelled"
	ErrorCodeTaskTimeout                           ErrorCode = "TaskTimeout"
	ErrorCodeConfigVersionMismatch                 ErrorCode = "CONFIG_VERSION_MISMATCH"
	ErrorCodeDataInconsistent                      ErrorCode = "DATA_INCONSISTENT"
	ErrorCodeCheckpointInvalid                     ErrorCode = "CheckpointInvalid"
	ErrorCodePlanGenerationFailed                  ErrorCode = "PlanGenerationFailed"
	ErrorCodePlanValidationFailed                  ErrorCode = "PlanValidationFailed"
	ErrorCodeInputResolutionFailed                 ErrorCode = "InputResolutionFailed"
	ErrorCodeModelCallFailed                       ErrorCode = "ModelCallFailed"
	ErrorCodeModelOutputInvalid                    ErrorCode = "ModelOutputInvalid"
	ErrorCodeResultSanitizationFailed              ErrorCode = "ResultSanitizationFailed"
	ErrorCodeStepOutputInvalid                     ErrorCode = "StepOutputInvalid"
	ErrorCodeStepOutputTooLarge                    ErrorCode = "StepOutputTooLarge"
	ErrorCodeToolNotFound                          ErrorCode = "ToolNotFound"
	ErrorCodeToolDisabled                          ErrorCode = "ToolDisabled"
	ErrorCodeToolNotAuthorized                     ErrorCode = "ToolNotAuthorized"
	ErrorCodeToolInputInvalid                      ErrorCode = "ToolInputInvalid"
	ErrorCodeToolAccessDenied                      ErrorCode = "ToolAccessDenied"
	ErrorCodeToolTimeout                           ErrorCode = "ToolTimeout"
	ErrorCodeToolConnectionLost                    ErrorCode = "ToolConnectionLost"
	ErrorCodeToolCallFailed                        ErrorCode = "ToolCallFailed"
	ErrorCodeApprovalContextChanged                ErrorCode = "ApprovalContextChanged"
	ErrorCodeApprovalRejected                      ErrorCode = "ApprovalRejected"
	ErrorCodeWriteToolInterrupted                  ErrorCode = "WRITE_TOOL_INTERRUPTED"
	ErrorCodeWorkerInterrupted                     ErrorCode = "WORKER_INTERRUPTED"
	ErrorCodeResultPersistenceFailed               ErrorCode = "RESULT_PERSISTENCE_FAILED"
	ErrorCodeReportGenerationFailed                ErrorCode = "ReportGenerationFailed"
	ErrorCodeStepExecutorContractBroken            ErrorCode = "STEP_EXECUTOR_CONTRACT_BROKEN"
	ErrorCodeRuntimeStaticToolSnapshotInconsistent ErrorCode = "RUNTIME_STATIC_TOOL_SNAPSHOT_INCONSISTENT"
	ErrorCodePersistenceInvariantViolation         ErrorCode = "PersistenceInvariantViolation"
)

// Valid 报告 ErrorCode 是否属于共享封闭集合。
func (c ErrorCode) Valid() bool {
	switch c {
	case ErrorCodeTaskCancelled, ErrorCodeTaskTimeout, ErrorCodeConfigVersionMismatch, ErrorCodeDataInconsistent,
		ErrorCodeCheckpointInvalid, ErrorCodePlanGenerationFailed, ErrorCodePlanValidationFailed,
		ErrorCodeInputResolutionFailed, ErrorCodeModelCallFailed, ErrorCodeModelOutputInvalid,
		ErrorCodeResultSanitizationFailed, ErrorCodeStepOutputInvalid, ErrorCodeStepOutputTooLarge,
		ErrorCodeToolNotFound, ErrorCodeToolDisabled, ErrorCodeToolNotAuthorized, ErrorCodeToolInputInvalid,
		ErrorCodeToolAccessDenied, ErrorCodeToolTimeout, ErrorCodeToolConnectionLost, ErrorCodeToolCallFailed,
		ErrorCodeApprovalContextChanged, ErrorCodeApprovalRejected, ErrorCodeWriteToolInterrupted,
		ErrorCodeWorkerInterrupted, ErrorCodeResultPersistenceFailed, ErrorCodeReportGenerationFailed,
		ErrorCodeStepExecutorContractBroken, ErrorCodeRuntimeStaticToolSnapshotInconsistent,
		ErrorCodePersistenceInvariantViolation:
		return true
	default:
		return false
	}
}

// CauseCode 表示 Port 结果、日志或安全诊断中的稳定原因分类。
type CauseCode string

const (
	CauseCodeTaskCancelled                         CauseCode = "TaskCancelled"
	CauseCodeTaskTimeout                           CauseCode = "TaskTimeout"
	CauseCodeConfigVersionMismatch                 CauseCode = "CONFIG_VERSION_MISMATCH"
	CauseCodeDataInconsistent                      CauseCode = "DATA_INCONSISTENT"
	CauseCodeCheckpointInvalid                     CauseCode = "CheckpointInvalid"
	CauseCodePlanGenerationFailed                  CauseCode = "PlanGenerationFailed"
	CauseCodePlanValidationFailed                  CauseCode = "PlanValidationFailed"
	CauseCodeInputResolutionFailed                 CauseCode = "InputResolutionFailed"
	CauseCodeModelCallFailed                       CauseCode = "ModelCallFailed"
	CauseCodeModelOutputInvalid                    CauseCode = "ModelOutputInvalid"
	CauseCodeResultSanitizationFailed              CauseCode = "ResultSanitizationFailed"
	CauseCodeStepOutputInvalid                     CauseCode = "StepOutputInvalid"
	CauseCodeStepOutputTooLarge                    CauseCode = "StepOutputTooLarge"
	CauseCodeToolNotFound                          CauseCode = "ToolNotFound"
	CauseCodeToolDisabled                          CauseCode = "ToolDisabled"
	CauseCodeToolNotAuthorized                     CauseCode = "ToolNotAuthorized"
	CauseCodeToolInputInvalid                      CauseCode = "ToolInputInvalid"
	CauseCodeToolAccessDenied                      CauseCode = "ToolAccessDenied"
	CauseCodeToolTimeout                           CauseCode = "ToolTimeout"
	CauseCodeToolConnectionLost                    CauseCode = "ToolConnectionLost"
	CauseCodeToolCallFailed                        CauseCode = "ToolCallFailed"
	CauseCodeApprovalContextChanged                CauseCode = "ApprovalContextChanged"
	CauseCodeApprovalRejected                      CauseCode = "ApprovalRejected"
	CauseCodeWriteToolInterrupted                  CauseCode = "WRITE_TOOL_INTERRUPTED"
	CauseCodeWorkerInterrupted                     CauseCode = "WORKER_INTERRUPTED"
	CauseCodeResultPersistenceFailed               CauseCode = "RESULT_PERSISTENCE_FAILED"
	CauseCodeReportGenerationFailed                CauseCode = "ReportGenerationFailed"
	CauseCodeStepExecutorContractBroken            CauseCode = "STEP_EXECUTOR_CONTRACT_BROKEN"
	CauseCodeRuntimeStaticToolSnapshotInconsistent CauseCode = "RUNTIME_STATIC_TOOL_SNAPSHOT_INCONSISTENT"
	CauseCodePersistenceInvariantViolation         CauseCode = "PersistenceInvariantViolation"
)

// Valid 报告 CauseCode 是否属于共享封闭集合。
func (c CauseCode) Valid() bool {
	return ErrorCode(c).Valid()
}

// TerminationReason 表示 TaskExecution 被外部生命周期命令终止的来源。
type TerminationReason string

const (
	TerminationReasonCancelled TerminationReason = "CANCELLED"
	TerminationReasonTimedOut  TerminationReason = "TIMED_OUT"
)

// Valid 报告 TerminationReason 是否属于封闭集合。
func (r TerminationReason) Valid() bool {
	return r == TerminationReasonCancelled || r == TerminationReasonTimedOut
}

// ReasonCode 表示既定 ErrorCode 下的稳定细分原因。
type ReasonCode string

const (
	ReasonCodeCheckpointNotFound                   ReasonCode = "CHECKPOINT_NOT_FOUND"
	ReasonCodeRuntimeContextMalformed              ReasonCode = "RUNTIME_CONTEXT_MALFORMED"
	ReasonCodeRuntimeContextVersionUnsupported     ReasonCode = "RUNTIME_CONTEXT_VERSION_UNSUPPORTED"
	ReasonCodeCheckpointAttributionMismatch        ReasonCode = "CHECKPOINT_ATTRIBUTION_MISMATCH"
	ReasonCodeCheckpointExecutionHashMismatch      ReasonCode = "CHECKPOINT_EXECUTION_HASH_MISMATCH"
	ReasonCodeCheckpointTypeAmbiguous              ReasonCode = "CHECKPOINT_TYPE_AMBIGUOUS"
	ReasonCodeCheckpointSourceInvalid              ReasonCode = "CHECKPOINT_SOURCE_INVALID"
	ReasonCodeCheckpointPlanReferenceInvalid       ReasonCode = "CHECKPOINT_PLAN_REFERENCE_INVALID"
	ReasonCodeCheckpointStepReferenceInvalid       ReasonCode = "CHECKPOINT_STEP_REFERENCE_INVALID"
	ReasonCodeCheckpointStepOutputReferenceInvalid ReasonCode = "CHECKPOINT_STEP_OUTPUT_REFERENCE_INVALID"
	ReasonCodeCheckpointReferenceSyntaxInvalid     ReasonCode = "CHECKPOINT_REFERENCE_SYNTAX_INVALID"
	ReasonCodeCheckpointReferencePathInvalid       ReasonCode = "CHECKPOINT_REFERENCE_PATH_INVALID"
	ReasonCodeCheckpointReferencePathTooDeep       ReasonCode = "CHECKPOINT_REFERENCE_PATH_TOO_DEEP"
	ReasonCodeCheckpointReferenceLimitExceeded     ReasonCode = "CHECKPOINT_REFERENCE_LIMIT_EXCEEDED"
	ReasonCodeCheckpointReferenceDuplicateTarget   ReasonCode = "CHECKPOINT_REFERENCE_DUPLICATE_TARGET"
	ReasonCodeCheckpointReferenceOrderInvalid      ReasonCode = "CHECKPOINT_REFERENCE_ORDER_INVALID"
	ReasonCodeCheckpointReferenceMissing           ReasonCode = "CHECKPOINT_REFERENCE_MISSING"
	ReasonCodeCheckpointReferenceExtra             ReasonCode = "CHECKPOINT_REFERENCE_EXTRA"
	ReasonCodeCheckpointReferenceSourceInvalid     ReasonCode = "CHECKPOINT_REFERENCE_SOURCE_INVALID"
	ReasonCodeCheckpointApprovalReferenceInvalid   ReasonCode = "CHECKPOINT_APPROVAL_REFERENCE_INVALID"
	ReasonCodeCheckpointFrozenActionMismatch       ReasonCode = "CHECKPOINT_FROZEN_ACTION_MISMATCH"
	ReasonCodeCheckpointFrozenInputHashMismatch    ReasonCode = "CHECKPOINT_FROZEN_INPUT_HASH_MISMATCH"
	ReasonCodeCheckpointNextActionInvalid          ReasonCode = "CHECKPOINT_NEXT_ACTION_INVALID"
)

// ValidForCheckpointInvalid 报告 ReasonCode 是否属于 CheckpointInvalid 的封闭集合。
//
// Tool Stale 的 reason_code 集合尚未在共享契约中冻结，因此本方法不声称校验全部
// reason_code 使用场景。
func (c ReasonCode) ValidForCheckpointInvalid() bool {
	switch c {
	case ReasonCodeCheckpointNotFound, ReasonCodeRuntimeContextMalformed, ReasonCodeRuntimeContextVersionUnsupported,
		ReasonCodeCheckpointAttributionMismatch, ReasonCodeCheckpointExecutionHashMismatch,
		ReasonCodeCheckpointTypeAmbiguous, ReasonCodeCheckpointSourceInvalid, ReasonCodeCheckpointPlanReferenceInvalid,
		ReasonCodeCheckpointStepReferenceInvalid, ReasonCodeCheckpointStepOutputReferenceInvalid,
		ReasonCodeCheckpointReferenceSyntaxInvalid, ReasonCodeCheckpointReferencePathInvalid,
		ReasonCodeCheckpointReferencePathTooDeep, ReasonCodeCheckpointReferenceLimitExceeded,
		ReasonCodeCheckpointReferenceDuplicateTarget, ReasonCodeCheckpointReferenceOrderInvalid,
		ReasonCodeCheckpointReferenceMissing, ReasonCodeCheckpointReferenceExtra,
		ReasonCodeCheckpointReferenceSourceInvalid, ReasonCodeCheckpointApprovalReferenceInvalid,
		ReasonCodeCheckpointFrozenActionMismatch, ReasonCodeCheckpointFrozenInputHashMismatch,
		ReasonCodeCheckpointNextActionInvalid:
		return true
	default:
		return false
	}
}

// InvariantCode 表示 DATA_INCONSISTENT 下的持久化不变量分类。
type InvariantCode string

const (
	InvariantCodeCurrentExecutionInvalid InvariantCode = "CURRENT_EXECUTION_INVALID"
	InvariantCodeQueueStateInvalid       InvariantCode = "QUEUE_STATE_INVALID"
	InvariantCodeClaimSourceAmbiguous    InvariantCode = "CLAIM_SOURCE_AMBIGUOUS"
	InvariantCodeCrossObjectStateInvalid InvariantCode = "CROSS_OBJECT_STATE_INVALID"
)

// Valid 报告 InvariantCode 是否属于封闭集合。
func (c InvariantCode) Valid() bool {
	switch c {
	case InvariantCodeCurrentExecutionInvalid, InvariantCodeQueueStateInvalid,
		InvariantCodeClaimSourceAmbiguous, InvariantCodeCrossObjectStateInvalid:
		return true
	default:
		return false
	}
}
