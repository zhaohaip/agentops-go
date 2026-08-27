// Package eino 实现基于 Eino 的模型基础设施适配器。
package eino

import (
	"context"
	"errors"
	"log/slog"
	"math/big"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	openaiadapter "github.com/cloudwego/eino-ext/components/model/openai"
	openaiprotocol "github.com/cloudwego/eino-ext/libs/acl/openai"
	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"

	"github.com/zhaohaip/agentops-go/internal/contracts"
)

const (
	deepSeekProvider       = "deepseek"
	deepSeekModel          = "deepseek-chat"
	deepSeekResponseFormat = "json_object"
	maxResponseBodyBytes   = 1 << 20
)

// DeepSeekConfig 是创建不可变 DeepSeek ChatModel 所需的基础设施配置。
type DeepSeekConfig struct {
	BaseURL    string
	APIKey     string
	HTTPClient *http.Client
	Logger     *slog.Logger
}

// EinoDeepSeekModelClient 将 AgentOps Model Client Port 适配到 Eino ChatModel。
type EinoDeepSeekModelClient struct {
	chatModel model.BaseChatModel
	logger    *slog.Logger
}

var _ contracts.ModelClient = (*EinoDeepSeekModelClient)(nil)

// NewEinoDeepSeekModelClient 创建固定使用 deepseek-chat 和 JSON Object 输出的客户端。
func NewEinoDeepSeekModelClient(ctx context.Context, config DeepSeekConfig) (*EinoDeepSeekModelClient, error) {
	if ctx == nil || strings.TrimSpace(config.BaseURL) == "" || strings.TrimSpace(config.APIKey) == "" {
		return nil, errors.New("create Eino DeepSeek model client: invalid configuration")
	}

	httpClient := cloneHTTPClientWithResponseLimit(config.HTTPClient, maxResponseBodyBytes)
	chatModel, err := openaiadapter.NewChatModel(ctx, &openaiadapter.ChatModelConfig{
		BaseURL:    config.BaseURL,
		APIKey:     config.APIKey,
		HTTPClient: httpClient,
		Model:      deepSeekModel,
		ResponseFormat: &openaiadapter.ChatCompletionResponseFormat{
			Type: openaiadapter.ChatCompletionResponseFormatTypeJSONObject,
		},
	})
	if err != nil {
		return nil, errors.New("create Eino DeepSeek model client: adapter initialization failed")
	}

	return newEinoDeepSeekModelClient(chatModel, config.Logger), nil
}

func newEinoDeepSeekModelClient(chatModel model.BaseChatModel, logger *slog.Logger) *EinoDeepSeekModelClient {
	return &EinoDeepSeekModelClient{
		chatModel: chatModel,
		logger:    logger,
	}
}

// GenerateStructured 执行一次非流式结构化模型调用。
func (c *EinoDeepSeekModelClient) GenerateStructured(
	ctx context.Context,
	request contracts.ModelRequest,
) (contracts.ModelResponse, error) {
	startedAt := time.Now()
	if c == nil || c.chatModel == nil || !validModelRequest(ctx, request) {
		err := contracts.NewModelClientError(contracts.ModelClientErrorContractViolation, nil)
		c.logCall(ctx, request, startedAt, nil, err.Kind)
		return contracts.ModelResponse{}, err
	}

	messages := make([]*schema.Message, len(request.Messages))
	for index, message := range request.Messages {
		switch message.Role {
		case contracts.ModelMessageRoleSystem:
			messages[index] = schema.SystemMessage(message.Content)
		case contracts.ModelMessageRoleUser:
			messages[index] = schema.UserMessage(message.Content)
		}
	}

	temperature, _ := strconv.ParseFloat(request.GenerationParams.Temperature.String(), 32)
	topP, _ := strconv.ParseFloat(request.GenerationParams.TopP.String(), 32)
	result, err := c.chatModel.Generate(ctx, messages,
		model.WithTemperature(float32(temperature)),
		model.WithTopP(float32(topP)),
		model.WithMaxTokens(int(request.GenerationParams.MaxOutputTokens)),
	)
	if err != nil {
		mapped := mapModelClientError(ctx, err)
		c.logCall(ctx, request, startedAt, nil, mapped.Kind)
		return contracts.ModelResponse{}, mapped
	}
	if result == nil || result.Role != schema.Assistant || strings.TrimSpace(result.Content) == "" ||
		!utf8.ValidString(result.Content) {
		mapped := contracts.NewModelClientError(contracts.ModelClientErrorInvalidResponse, nil)
		c.logCall(ctx, request, startedAt, nil, mapped.Kind)
		return contracts.ModelResponse{}, mapped
	}

	var providerRequestID *string
	if requestID := openaiprotocol.GetRequestID(result); requestID != "" && utf8.ValidString(requestID) {
		providerRequestID = &requestID
	}
	response := contracts.ModelResponse{
		AssistantContent:  result.Content,
		ProviderRequestID: providerRequestID,
	}
	c.logCall(ctx, request, startedAt, providerRequestID, "")
	return response, nil
}

func validModelRequest(ctx context.Context, request contracts.ModelRequest) bool {
	if ctx == nil || request.Model != deepSeekModel || request.Stream ||
		request.ResponseFormat != deepSeekResponseFormat || len(request.Messages) == 0 ||
		!validGenerationParams(request.GenerationParams) {
		return false
	}
	for _, message := range request.Messages {
		if !message.Role.Valid() || strings.TrimSpace(message.Content) == "" || !utf8.ValidString(message.Content) {
			return false
		}
	}
	return true
}

func validGenerationParams(params contracts.GenerationParams) bool {
	temperature, ok := new(big.Rat).SetString(params.Temperature.String())
	if !ok || temperature.Sign() < 0 || temperature.Cmp(big.NewRat(2, 1)) > 0 {
		return false
	}
	topP, ok := new(big.Rat).SetString(params.TopP.String())
	return ok && topP.Sign() > 0 && topP.Cmp(big.NewRat(1, 1)) <= 0 &&
		params.MaxOutputTokens > 0 && params.MaxOutputTokens <= 8192
}

func mapModelClientError(ctx context.Context, err error) *contracts.ModelClientError {
	if ctx != nil && ctx.Err() != nil {
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return contracts.NewModelClientError(contracts.ModelClientErrorTimeout, context.DeadlineExceeded)
		}
		return contracts.NewModelClientError(contracts.ModelClientErrorCanceled, context.Canceled)
	}
	if errors.Is(err, errResponseTooLarge) {
		return contracts.NewModelClientError(contracts.ModelClientErrorResponseTooLarge, nil)
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return contracts.NewModelClientError(contracts.ModelClientErrorTimeout, context.DeadlineExceeded)
	}
	if errors.Is(err, context.Canceled) {
		return contracts.NewModelClientError(contracts.ModelClientErrorCanceled, context.Canceled)
	}

	var apiError *openaiadapter.APIError
	if errors.As(err, &apiError) {
		switch apiError.HTTPStatusCode {
		case http.StatusRequestTimeout, http.StatusGatewayTimeout:
			return contracts.NewModelClientError(contracts.ModelClientErrorTimeout, nil)
		case http.StatusUnauthorized, http.StatusForbidden:
			return contracts.NewModelClientError(contracts.ModelClientErrorAuthentication, nil)
		case http.StatusTooManyRequests:
			return contracts.NewModelClientError(contracts.ModelClientErrorRateLimited, nil)
		default:
			return contracts.NewModelClientError(contracts.ModelClientErrorProvider, nil)
		}
	}

	var networkError net.Error
	if errors.As(err, &networkError) {
		if networkError.Timeout() {
			return contracts.NewModelClientError(contracts.ModelClientErrorTimeout, context.DeadlineExceeded)
		}
		return contracts.NewModelClientError(contracts.ModelClientErrorNetwork, nil)
	}
	return contracts.NewModelClientError(contracts.ModelClientErrorProvider, nil)
}

func (c *EinoDeepSeekModelClient) logCall(
	ctx context.Context,
	request contracts.ModelRequest,
	startedAt time.Time,
	providerRequestID *string,
	errorKind contracts.ModelClientErrorKind,
) {
	if c == nil || c.logger == nil {
		return
	}
	attributes := []any{
		"provider", deepSeekProvider,
		"model", deepSeekModel,
		"phase", request.Metadata.Phase,
		"repair", request.Metadata.Phase == "REPAIR",
		"task_id", string(request.Metadata.TaskID),
		"run_id", string(request.Metadata.RunID),
		"duration_ms", time.Since(startedAt).Milliseconds(),
		"success", errorKind == "",
	}
	if request.Metadata.ExecutionVersion != nil {
		attributes = append(attributes, "execution_version", int64(*request.Metadata.ExecutionVersion))
	}
	if errorKind != "" {
		attributes = append(attributes,
			"error_kind", string(errorKind),
			"cause_code", string(errorKind),
		)
	}
	if providerRequestID != nil {
		attributes = append(attributes, "provider_request_id", *providerRequestID)
	}
	c.logger.InfoContext(ctx, "model call completed", attributes...)
}
