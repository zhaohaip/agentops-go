package stepexecutor

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"math"
	"math/big"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/zhaohaip/agentops-go/internal/contracts"
)

const (
	modelStepModelName         = "deepseek-chat"
	modelStepResponseFormat    = "json_object"
	modelStepOperation         = "ExecuteModelStep"
	modelStepCallTimeout       = 60 * time.Second
	maxModelStepPromptBytes    = 256 * 1024
	maxModelStepResponseBytes  = 1024 * 1024
	maxModelStepOutputBytes    = 1024 * 1024
	maxModelStepOutputDepth    = 16
	maxModelStepObjectFields   = 64
	maxModelStepOutputFields   = 32
	maxModelOutputFieldNameLen = 64
	modelRedactedValue         = "[REDACTED]"
)

var (
	// ErrModelStepContract 表示调用方交付的模型 Step 投影违反冻结契约。
	ErrModelStepContract = errors.New("model Step contract is invalid")
	// ErrModelStepInputTooLarge 表示构造后的完整模型 Prompt 超过冻结上限。
	ErrModelStepInputTooLarge = errors.New("model Step input exceeds size limit")
	// ErrModelStepOutputInvalid 表示模型响应不满足严格 JSON 或 OutputSchema 契约。
	ErrModelStepOutputInvalid = errors.New("model Step output is invalid")
	// ErrModelStepResponseTooLarge 表示 Model Client 返回的原始响应超过冻结上限。
	ErrModelStepResponseTooLarge = errors.New("model Step response exceeds size limit")
	// ErrModelStepSanitization 表示模型响应无法转换为安全输出。
	ErrModelStepSanitization = errors.New("model Step output sanitization failed")
	// ErrModelStepOutputTooLarge 表示安全规范化输出超过冻结上限。
	ErrModelStepOutputTooLarge = errors.New("model Step output exceeds size limit")
)

// ModelStepResult 是模型原始响应完成校验和脱敏后的内存结果。
type ModelStepResult struct {
	SafeOutput json.RawMessage
}

// ModelStepRunner 执行 Analysis、ModelCall 和 Verification 的单次模型调用。
//
// Runner 只持有 Model Client Port，不持有 Repository、事务或持久化能力。
type ModelStepRunner struct {
	model       contracts.ModelClient
	callTimeout time.Duration
}

// NewModelStepRunner 创建使用共享 Model Client 的无状态模型 Step Runner。
func NewModelStepRunner(model contracts.ModelClient) *ModelStepRunner {
	return &ModelStepRunner{model: model, callTimeout: modelStepCallTimeout}
}

// Execute 执行一次模型 Step，并且只返回经过校验和脱敏的安全输出。
func (r *ModelStepRunner) Execute(
	ctx context.Context,
	request StepExecutionRequest,
	resolved ResolvedStepInput,
) (ModelStepResult, error) {
	if err := validateModelStepRequest(ctx, request, resolved); err != nil {
		return ModelStepResult{}, contractResolutionError(err)
	}
	if r == nil || r.model == nil || r.callTimeout <= 0 {
		return ModelStepResult{}, contractResolutionError(ErrModelStepContract)
	}
	if canceled := modelStepCancellation(ctx); canceled != nil {
		return ModelStepResult{}, canceled
	}

	modelRequest, err := buildModelStepRequest(request, resolved)
	if err != nil {
		return ModelStepResult{}, contractResolutionError(err)
	}
	if modelPromptBytes(modelRequest.Messages) > maxModelStepPromptBytes {
		return ModelStepResult{}, newStepError(
			ErrorKindFailed, contracts.ErrorCodeModelInputTooLarge, CauseModelInputTooLarge,
			ErrModelStepInputTooLarge,
		)
	}

	callCtx, cancel := context.WithTimeout(ctx, r.callTimeout)
	defer cancel()
	response, callErr := r.model.GenerateStructured(callCtx, modelRequest)
	if canceled := modelStepCancellation(ctx); canceled != nil {
		return ModelStepResult{}, canceled
	}
	if callCtx.Err() != nil {
		return ModelStepResult{}, newStepError(
			ErrorKindFailed, contracts.ErrorCodeModelCallFailed, CauseModelTimeout,
			context.DeadlineExceeded,
		)
	}
	if callErr != nil {
		if response.AssistantContent != "" || response.ProviderRequestID != nil {
			return ModelStepResult{}, contractResolutionError(ErrModelStepContract)
		}
		return ModelStepResult{}, MapModelClientError(callErr)
	}
	if len(response.AssistantContent) > maxModelStepResponseBytes {
		return ModelStepResult{}, newStepError(
			ErrorKindFailed, contracts.ErrorCodeModelCallFailed, CauseModelResponseTooLarge,
			ErrModelStepResponseTooLarge,
		)
	}

	safeOutput, err := processModelStepOutput(response.AssistantContent, request.Step.OutputSchema)
	if err != nil {
		return ModelStepResult{}, err
	}
	return ModelStepResult{SafeOutput: append(json.RawMessage(nil), safeOutput...)}, nil
}

func validateModelStepRequest(
	ctx context.Context,
	request StepExecutionRequest,
	resolved ResolvedStepInput,
) error {
	if ctx == nil || request.NextAction != contracts.CheckpointNextActionExecuteStep ||
		request.Scope.TaskID == "" || request.Scope.RunID == "" ||
		!request.Scope.ExecutionVersion.Valid() || !request.Scope.ExecutionConfigHash.Valid() ||
		request.Scope.WorkerID == "" || request.Scope.StepID == "" || request.Scope.DeadlineAt.IsZero() ||
		request.Step.StepID == "" || request.Step.RunID != request.Scope.RunID ||
		request.Step.StepID != request.Scope.StepID || request.Step.PlanID == "" ||
		request.Step.Sequence == 0 || strings.TrimSpace(request.Step.Name) == "" ||
		!utf8.ValidString(request.Step.Name) || !modelStepType(request.Step.Type) ||
		(request.Step.Status != contracts.StepStatusPending && request.Step.Status != contracts.StepStatusRunning) ||
		request.Agent == nil || request.Agent.AgentID == "" ||
		strings.TrimSpace(request.Agent.SystemPrompt) == "" || !utf8.ValidString(request.Agent.SystemPrompt) ||
		request.Agent.ModelName != modelStepModelName ||
		!validModelStepGenerationParams(request.Agent.GenerationParams) ||
		resolved.StepID != request.Step.StepID || resolved.InputContractVersion != stepInputContractVersionV1 ||
		!validOutcomeJSONObject(resolved.Value) ||
		!resolvedReferencesEqual(resolved.ReferencedFields, request.ResolvedReferences) ||
		!validModelOutputSchema(request.Step.OutputSchema) {
		return ErrModelStepContract
	}
	return nil
}

func modelStepType(stepType contracts.StepType) bool {
	switch stepType {
	case contracts.StepTypeAnalysis, contracts.StepTypeModelCall, contracts.StepTypeVerification:
		return true
	default:
		return false
	}
}

func validModelStepGenerationParams(params contracts.GenerationParams) bool {
	temperature, ok := new(big.Rat).SetString(params.Temperature.String())
	if !ok || temperature.Sign() < 0 || temperature.Cmp(big.NewRat(2, 1)) > 0 {
		return false
	}
	topP, ok := new(big.Rat).SetString(params.TopP.String())
	return ok && topP.Sign() > 0 && topP.Cmp(big.NewRat(1, 1)) <= 0 &&
		params.MaxOutputTokens > 0 && params.MaxOutputTokens <= 8192
}

func validModelOutputSchema(schema contracts.OutputSchema) bool {
	if len(schema) == 0 || len(schema) > maxModelStepOutputFields {
		return false
	}
	for name, field := range schema {
		if len(name) > maxModelOutputFieldNameLen || !validOutputFieldName(name) || !field.Type.Valid() {
			return false
		}
	}
	return true
}

func buildModelStepRequest(
	request StepExecutionRequest,
	resolved ResolvedStepInput,
) (contracts.ModelRequest, error) {
	template, ok := modelStepTemplate(request.Step.Type)
	if !ok {
		return contracts.ModelRequest{}, ErrModelStepContract
	}
	type promptPayload struct {
		StepType      contracts.StepType     `json:"step_type"`
		StepName      string                 `json:"step_name"`
		Template      string                 `json:"template"`
		ResolvedInput json.RawMessage        `json:"resolved_input"`
		OutputSchema  contracts.OutputSchema `json:"output_schema"`
		OutputRule    string                 `json:"output_rule"`
	}
	payload, err := json.Marshal(promptPayload{
		StepType: request.Step.Type, StepName: request.Step.Name, Template: template,
		ResolvedInput: resolved.Value, OutputSchema: request.Step.OutputSchema,
		OutputRule: "Return exactly one JSON object matching output_schema; no extra fields or text.",
	})
	if err != nil {
		return contracts.ModelRequest{}, ErrModelStepContract
	}
	version := request.Scope.ExecutionVersion
	stepID := request.Step.StepID
	stepType := request.Step.Type
	return contracts.ModelRequest{
		Model: modelStepModelName, Stream: false, ResponseFormat: modelStepResponseFormat,
		Messages: []contracts.ModelMessage{
			{
				Role: contracts.ModelMessageRoleSystem,
				Content: request.Agent.SystemPrompt +
					"\nFollow AgentOps safety boundaries. Use only the supplied resolved input and output schema.",
			},
			{Role: contracts.ModelMessageRoleUser, Content: string(payload)},
		},
		GenerationParams: request.Agent.GenerationParams,
		Metadata: contracts.ModelRequestMetadata{
			Operation: modelStepOperation, Phase: string(request.Step.Type),
			TaskID: request.Scope.TaskID, RunID: request.Scope.RunID,
			ExecutionVersion: &version, StepID: &stepID, StepType: &stepType,
		},
	}, nil
}

func modelStepTemplate(stepType contracts.StepType) (string, bool) {
	switch stepType {
	case contracts.StepTypeModelCall:
		return "Execute prompt using only optional context.", true
	case contracts.StepTypeAnalysis:
		return "Analyze evidence according to instruction.", true
	case contracts.StepTypeVerification:
		return "Verify evidence only against criteria and report the declared fields.", true
	default:
		return "", false
	}
}

func modelPromptBytes(messages []contracts.ModelMessage) int {
	total := 0
	for _, message := range messages {
		total += len(message.Content)
	}
	return total
}

func modelStepCancellation(ctx context.Context) *StepError {
	if ctx == nil || ctx.Err() == nil {
		return nil
	}
	cause := modelStepCancellationCode(context.Cause(ctx), ctx.Err())
	switch cause {
	case CauseTaskCancelled, CauseTaskTimedOut, CauseRuntimeShutdown, CauseLockLost:
		return newStepError(ErrorKindStale, "", cause, ctx.Err())
	case CauseActionTimeout:
		return newStepError(
			ErrorKindFailed, contracts.ErrorCodeModelCallFailed, CauseModelTimeout, ctx.Err(),
		)
	default:
		return newStepError(ErrorKindStale, "", CauseStaleExecution, ctx.Err())
	}
}

func modelStepCancellationCode(cause error, sentinel error) CauseCode {
	if code, ok := contracts.ExecutionCancellationCauseFrom(cause); ok {
		return CauseCode(code)
	}
	if errors.Is(sentinel, context.DeadlineExceeded) {
		return CauseActionTimeout
	}
	return CauseRuntimeShutdown
}

func processModelStepOutput(
	content string,
	schema contracts.OutputSchema,
) (json.RawMessage, error) {
	value, err := decodeStrictModelOutput(content)
	if err != nil || !matchesModelOutputSchema(value, schema) {
		return nil, newStepError(
			ErrorKindFailed, contracts.ErrorCodeModelOutputInvalid, CauseModelOutputInvalid,
			ErrModelStepOutputInvalid,
		)
	}
	sanitized, ok := sanitizeModelOutput(value)
	if !ok || !matchesModelOutputSchema(sanitized, schema) {
		return nil, newStepError(
			ErrorKindFailed, contracts.ErrorCodeResultSanitizationFailed,
			CauseResultSanitizationFailed, ErrModelStepSanitization,
		)
	}
	encoded, err := json.Marshal(sanitized)
	if err != nil {
		return nil, newStepError(
			ErrorKindFailed, contracts.ErrorCodeResultSanitizationFailed,
			CauseResultSanitizationFailed, ErrModelStepSanitization,
		)
	}
	if len(encoded) > maxModelStepOutputBytes {
		return nil, newStepError(
			ErrorKindFailed, contracts.ErrorCodeStepOutputTooLarge,
			CauseStepOutputTooLarge, ErrModelStepOutputTooLarge,
		)
	}
	return append(json.RawMessage(nil), encoded...), nil
}

func decodeStrictModelOutput(content string) (map[string]any, error) {
	if !utf8.ValidString(content) || strings.TrimSpace(content) == "" {
		return nil, ErrModelStepOutputInvalid
	}
	decoder := json.NewDecoder(strings.NewReader(content))
	decoder.UseNumber()
	value, err := readStrictModelValue(decoder, 1)
	if err != nil {
		return nil, ErrModelStepOutputInvalid
	}
	if err := requireModelOutputEOF(decoder); err != nil {
		return nil, ErrModelStepOutputInvalid
	}
	object, ok := value.(map[string]any)
	if !ok {
		return nil, ErrModelStepOutputInvalid
	}
	return object, nil
}

func readStrictModelValue(decoder *json.Decoder, depth int) (any, error) {
	token, err := decoder.Token()
	if err != nil || token == nil {
		return nil, ErrModelStepOutputInvalid
	}
	if delimiter, ok := token.(json.Delim); ok {
		switch delimiter {
		case '{':
			return readStrictModelObject(decoder, depth)
		case '[':
			return readStrictModelArray(decoder, depth)
		default:
			return nil, ErrModelStepOutputInvalid
		}
	}
	if number, ok := token.(json.Number); ok {
		parsed, err := number.Float64()
		if err != nil || math.IsInf(parsed, 0) || math.IsNaN(parsed) {
			return nil, ErrModelStepOutputInvalid
		}
	}
	return token, nil
}

func readStrictModelObject(decoder *json.Decoder, depth int) (map[string]any, error) {
	if depth > maxModelStepOutputDepth {
		return nil, ErrModelStepOutputInvalid
	}
	result := make(map[string]any)
	for decoder.More() {
		fieldToken, err := decoder.Token()
		field, ok := fieldToken.(string)
		if err != nil || !ok || !utf8.ValidString(field) {
			return nil, ErrModelStepOutputInvalid
		}
		if _, duplicate := result[field]; duplicate {
			return nil, ErrModelStepOutputInvalid
		}
		if len(result) >= maxModelStepObjectFields {
			return nil, ErrModelStepOutputInvalid
		}
		value, err := readStrictModelValue(decoder, depth+1)
		if err != nil {
			return nil, err
		}
		result[field] = value
	}
	if token, err := decoder.Token(); err != nil || token != json.Delim('}') {
		return nil, ErrModelStepOutputInvalid
	}
	return result, nil
}

func readStrictModelArray(decoder *json.Decoder, depth int) ([]any, error) {
	if depth > maxModelStepOutputDepth {
		return nil, ErrModelStepOutputInvalid
	}
	result := make([]any, 0)
	for decoder.More() {
		value, err := readStrictModelValue(decoder, depth+1)
		if err != nil {
			return nil, err
		}
		result = append(result, value)
	}
	if token, err := decoder.Token(); err != nil || token != json.Delim(']') {
		return nil, ErrModelStepOutputInvalid
	}
	return result, nil
}

func requireModelOutputEOF(decoder *json.Decoder) error {
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return ErrModelStepOutputInvalid
	}
	return nil
}

func matchesModelOutputSchema(value map[string]any, schema contracts.OutputSchema) bool {
	if len(value) != len(schema) {
		return false
	}
	for name, field := range schema {
		actual, exists := value[name]
		if !exists || actual == nil || !matchesOutputType(actual, field.Type) {
			return false
		}
	}
	return true
}

func sanitizeModelOutput(value map[string]any) (map[string]any, bool) {
	sanitized, ok := sanitizeModelValue(value)
	if !ok {
		return nil, false
	}
	result, ok := sanitized.(map[string]any)
	return result, ok
}

func sanitizeModelValue(value any) (any, bool) {
	switch typed := value.(type) {
	case map[string]any:
		result := make(map[string]any, len(typed))
		for key, child := range typed {
			if containsUnsafeModelControl(key) {
				return nil, false
			}
			if sensitiveModelKey(key) {
				result[key] = modelRedactedValue
				continue
			}
			sanitized, ok := sanitizeModelValue(child)
			if !ok {
				return nil, false
			}
			result[key] = sanitized
		}
		return result, true
	case []any:
		result := make([]any, len(typed))
		for index, child := range typed {
			sanitized, ok := sanitizeModelValue(child)
			if !ok {
				return nil, false
			}
			result[index] = sanitized
		}
		return result, true
	case string:
		if containsUnsafeModelControl(typed) {
			return nil, false
		}
		if sensitiveModelString(typed) {
			return modelRedactedValue, true
		}
		return typed, true
	default:
		return typed, true
	}
}

func sensitiveModelKey(key string) bool {
	switch strings.ToLower(key) {
	case "password", "passwd", "secret", "token", "api_key", "apikey", "private_key",
		"client_secret", "authorization":
		return true
	default:
		return false
	}
}

func sensitiveModelString(value string) bool {
	for _, marker := range []string{
		"-----BEGIN PRIVATE KEY-----",
		"-----BEGIN RSA PRIVATE KEY-----",
		"-----BEGIN EC PRIVATE KEY-----",
		"-----BEGIN OPENSSH PRIVATE KEY-----",
	} {
		if strings.Contains(value, marker) {
			return true
		}
	}
	trimmed := strings.TrimLeft(value, " \t\r\n")
	lower := strings.ToLower(trimmed)
	return strings.HasPrefix(lower, "bearer ") || strings.HasPrefix(lower, "basic ")
}

func containsUnsafeModelControl(value string) bool {
	for _, character := range value {
		if character < 0x20 && character != '\n' && character != '\r' && character != '\t' {
			return true
		}
	}
	return false
}
