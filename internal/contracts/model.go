package contracts

import (
	"context"
	"fmt"
)

// ModelClient 是 AgentOps 共享结构化模型调用 Port。
type ModelClient interface {
	GenerateStructured(
		ctx context.Context,
		request ModelRequest,
	) (ModelResponse, error)
}

// ModelMessageRole 表示 ModelRequest 消息角色。
type ModelMessageRole string

const (
	ModelMessageRoleSystem ModelMessageRole = "system"
	ModelMessageRoleUser   ModelMessageRole = "user"
)

// Valid 报告 ModelMessageRole 是否属于请求允许集合。
func (r ModelMessageRole) Valid() bool {
	return r == ModelMessageRoleSystem || r == ModelMessageRoleUser
}

// ModelMessage 表示发送给 Model Client 的 AgentOps 消息。
type ModelMessage struct {
	Role    ModelMessageRole `json:"role"`
	Content string           `json:"content"`
}

// ModelRequestMetadata 表示仅供进程内安全调用关联的元数据。
type ModelRequestMetadata struct {
	Operation        string            `json:"operation,omitempty"`
	Phase            string            `json:"phase,omitempty"`
	TaskID           TaskID            `json:"task_id"`
	RunID            RunID             `json:"run_id"`
	ExecutionVersion *ExecutionVersion `json:"execution_version,omitempty"`
	StepID           *StepID           `json:"step_id,omitempty"`
	StepType         *StepType         `json:"step_type,omitempty"`
	ReportID         *ReportID         `json:"report_id,omitempty"`
}

// ModelRequest 表示与 Provider SDK 隔离的结构化模型请求。
type ModelRequest struct {
	Model            string               `json:"model"`
	Stream           bool                 `json:"stream"`
	ResponseFormat   string               `json:"response_format"`
	Messages         []ModelMessage       `json:"messages"`
	GenerationParams GenerationParams     `json:"generation_params"`
	Metadata         ModelRequestMetadata `json:"metadata"`
}

// ModelResponse 表示 Model Client 允许公开的最小响应。
type ModelResponse struct {
	AssistantContent  string  `json:"assistant_content"`
	ProviderRequestID *string `json:"provider_request_id,omitempty"`
}

// ModelClientErrorKind 表示 Model Client 的封闭错误类别。
type ModelClientErrorKind string

const (
	ModelClientErrorCanceled          ModelClientErrorKind = "Canceled"
	ModelClientErrorTimeout           ModelClientErrorKind = "Timeout"
	ModelClientErrorAuthentication    ModelClientErrorKind = "Authentication"
	ModelClientErrorNetwork           ModelClientErrorKind = "Network"
	ModelClientErrorRateLimited       ModelClientErrorKind = "RateLimited"
	ModelClientErrorProvider          ModelClientErrorKind = "Provider"
	ModelClientErrorResponseTooLarge  ModelClientErrorKind = "ResponseTooLarge"
	ModelClientErrorInvalidResponse   ModelClientErrorKind = "InvalidResponse"
	ModelClientErrorContractViolation ModelClientErrorKind = "ContractViolation"
)

// Valid 报告 ModelClientErrorKind 是否属于封闭集合。
func (k ModelClientErrorKind) Valid() bool {
	switch k {
	case ModelClientErrorCanceled, ModelClientErrorTimeout, ModelClientErrorAuthentication,
		ModelClientErrorNetwork, ModelClientErrorRateLimited, ModelClientErrorProvider,
		ModelClientErrorResponseTooLarge, ModelClientErrorInvalidResponse, ModelClientErrorContractViolation:
		return true
	default:
		return false
	}
}

// ModelClientError 是可通过 errors.As 识别的 Model Client 错误。
type ModelClientError struct {
	Kind  ModelClientErrorKind
	cause error
}

// NewModelClientError 创建类型化 Model Client 错误。
func NewModelClientError(kind ModelClientErrorKind, cause error) *ModelClientError {
	return &ModelClientError{
		Kind:  kind,
		cause: cause,
	}
}

// Error 返回不包含 Provider 原始错误文本的稳定描述。
func (e *ModelClientError) Error() string {
	if e == nil {
		return "model client error"
	}
	return fmt.Sprintf("model client error: %s", e.Kind)
}

// Unwrap 保留 context 取消和 deadline 的 errors.Is 语义。
func (e *ModelClientError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.cause
}
