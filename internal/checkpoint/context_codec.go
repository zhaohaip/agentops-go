package checkpoint

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"unicode/utf8"

	"github.com/zhaohaip/agentops-go/internal/contracts"
)

var outputFieldNamePattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// RuntimeContextCodecErrorKind 是 Runtime Context 解码失败的可判定类别。
type RuntimeContextCodecErrorKind uint8

const (
	// RuntimeContextCodecMalformed 表示 JSON 或固定结构无效。
	RuntimeContextCodecMalformed RuntimeContextCodecErrorKind = iota + 1
	// RuntimeContextCodecVersionUnsupported 表示 schema_version 不是当前冻结版本。
	RuntimeContextCodecVersionUnsupported
)

// RuntimeContextCodecError 保留安全错误类别，不向调用方暴露原始持久化内容。
type RuntimeContextCodecError struct {
	Kind RuntimeContextCodecErrorKind
	Err  error
}

// Error 实现 error。
func (e *RuntimeContextCodecError) Error() string {
	if e == nil || e.Err == nil {
		return "runtime context codec error"
	}
	return e.Err.Error()
}

// Unwrap 返回底层安全诊断错误。
func (e *RuntimeContextCodecError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Err
}

const (
	runtimeContextSchemaVersion = 1
	maxResolvedReferences       = 256
	maxReferencePathDepth       = 16
)

// RuntimeContextCodecLimits defines transport limits for one RuntimeContextV1
// document. The limits are deployment policy rather than wire-protocol fields.
type RuntimeContextCodecLimits struct {
	MaxBytes int
	MaxDepth int
}

// RuntimeContextCodec strictly encodes and decodes RuntimeContextV1 documents.
type RuntimeContextCodec struct {
	limits RuntimeContextCodecLimits
}

// NewRuntimeContextCodec creates a codec with explicit, positive limits.
func NewRuntimeContextCodec(limits RuntimeContextCodecLimits) (RuntimeContextCodec, error) {
	if limits.MaxBytes <= 0 {
		return RuntimeContextCodec{}, errors.New("create runtime context codec: max bytes must be positive")
	}
	if limits.MaxDepth <= 0 {
		return RuntimeContextCodec{}, errors.New("create runtime context codec: max depth must be positive")
	}
	return RuntimeContextCodec{limits: limits}, nil
}

// Encode validates and deterministically encodes the fixed RuntimeContextV1
// field set. Arbitrary maps are confined to already-frozen JSON values.
func (c RuntimeContextCodec) Encode(value contracts.RuntimeContextV1) ([]byte, error) {
	if err := validateRuntimeContext(value); err != nil {
		if value.SchemaVersion != runtimeContextSchemaVersion {
			return nil, &RuntimeContextCodecError{
				Kind: RuntimeContextCodecVersionUnsupported,
				Err:  fmt.Errorf("encode runtime context: %w", err),
			}
		}
		return nil, malformedCodecError("encode runtime context", err)
	}

	encoded, err := json.Marshal(value)
	if err != nil {
		return nil, malformedCodecError("encode runtime context", err)
	}
	if err := c.validateJSONDocument(encoded); err != nil {
		return nil, malformedCodecError("encode runtime context", err)
	}
	return encoded, nil
}

// Decode strictly parses and validates one RuntimeContextV1 JSON object.
func (c RuntimeContextCodec) Decode(encoded []byte) (contracts.RuntimeContextV1, error) {
	if err := c.validateJSONDocument(encoded); err != nil {
		return contracts.RuntimeContextV1{}, malformedCodecError("decode runtime context", err)
	}
	if err := rejectNullRuntimeContextFields(encoded); err != nil {
		return contracts.RuntimeContextV1{}, malformedCodecError("decode runtime context", err)
	}

	decoder := json.NewDecoder(bytes.NewReader(encoded))
	decoder.DisallowUnknownFields()
	var value contracts.RuntimeContextV1
	if err := decoder.Decode(&value); err != nil {
		return contracts.RuntimeContextV1{}, malformedCodecError("decode runtime context", err)
	}
	if err := ensureJSONEOF(decoder); err != nil {
		return contracts.RuntimeContextV1{}, malformedCodecError("decode runtime context", err)
	}
	if err := validateRuntimeContext(value); err != nil {
		if value.SchemaVersion != 0 && value.SchemaVersion != runtimeContextSchemaVersion {
			return contracts.RuntimeContextV1{}, &RuntimeContextCodecError{
				Kind: RuntimeContextCodecVersionUnsupported,
				Err:  fmt.Errorf("decode runtime context: %w", err),
			}
		}
		return contracts.RuntimeContextV1{}, malformedCodecError("decode runtime context", err)
	}
	return value, nil
}

func malformedCodecError(operation string, err error) error {
	return &RuntimeContextCodecError{
		Kind: RuntimeContextCodecMalformed,
		Err:  fmt.Errorf("%s: %w", operation, err),
	}
}

func (c RuntimeContextCodec) validateJSONDocument(encoded []byte) error {
	if len(encoded) == 0 {
		return errors.New("JSON document is empty")
	}
	if len(encoded) > c.limits.MaxBytes {
		return fmt.Errorf("JSON document exceeds %d bytes", c.limits.MaxBytes)
	}
	if !utf8.Valid(encoded) {
		return errors.New("JSON document is not valid UTF-8")
	}

	decoder := json.NewDecoder(bytes.NewReader(encoded))
	token, err := decoder.Token()
	if err != nil {
		return fmt.Errorf("read JSON object: %w", err)
	}
	root, ok := token.(json.Delim)
	if !ok || root != '{' {
		return errors.New("JSON document must be an object")
	}
	if err := scanJSONObject(decoder, 1, c.limits.MaxDepth); err != nil {
		return err
	}
	return ensureJSONEOF(decoder)
}

func scanJSONObject(decoder *json.Decoder, depth, maxDepth int) error {
	if depth > maxDepth {
		return fmt.Errorf("JSON document exceeds depth %d", maxDepth)
	}
	seen := make(map[string]struct{})
	for decoder.More() {
		fieldToken, err := decoder.Token()
		if err != nil {
			return fmt.Errorf("read JSON object field: %w", err)
		}
		field, ok := fieldToken.(string)
		if !ok {
			return errors.New("JSON object field name must be a string")
		}
		if _, duplicate := seen[field]; duplicate {
			return fmt.Errorf("duplicate JSON field %q", field)
		}
		seen[field] = struct{}{}
		if err := scanJSONValue(decoder, depth, maxDepth); err != nil {
			return fmt.Errorf("field %q: %w", field, err)
		}
	}
	if _, err := decoder.Token(); err != nil {
		return fmt.Errorf("close JSON object: %w", err)
	}
	return nil
}

func scanJSONArray(decoder *json.Decoder, depth, maxDepth int) error {
	if depth > maxDepth {
		return fmt.Errorf("JSON document exceeds depth %d", maxDepth)
	}
	for decoder.More() {
		if err := scanJSONValue(decoder, depth, maxDepth); err != nil {
			return err
		}
	}
	if _, err := decoder.Token(); err != nil {
		return fmt.Errorf("close JSON array: %w", err)
	}
	return nil
}

func scanJSONValue(decoder *json.Decoder, parentDepth, maxDepth int) error {
	token, err := decoder.Token()
	if err != nil {
		return fmt.Errorf("read JSON value: %w", err)
	}
	delimiter, composite := token.(json.Delim)
	if !composite {
		return nil
	}
	switch delimiter {
	case '{':
		return scanJSONObject(decoder, parentDepth+1, maxDepth)
	case '[':
		return scanJSONArray(decoder, parentDepth+1, maxDepth)
	default:
		return fmt.Errorf("unexpected JSON delimiter %q", delimiter)
	}
}

func rejectNullRuntimeContextFields(encoded []byte) error {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(encoded, &fields); err != nil {
		return fmt.Errorf("inspect runtime context fields: %w", err)
	}
	for field, value := range fields {
		if bytes.Equal(bytes.TrimSpace(value), []byte("null")) {
			return fmt.Errorf("field %q must not be null", field)
		}
	}
	return nil
}

func ensureJSONEOF(decoder *json.Decoder) error {
	var trailing any
	err := decoder.Decode(&trailing)
	if errors.Is(err, io.EOF) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read trailing JSON data: %w", err)
	}
	return errors.New("multiple JSON values are not allowed")
}

func validateRuntimeContext(value contracts.RuntimeContextV1) error {
	if value.SchemaVersion != runtimeContextSchemaVersion {
		return fmt.Errorf("schema_version must be %d", runtimeContextSchemaVersion)
	}
	if err := requireString("task_id", string(value.TaskID)); err != nil {
		return err
	}
	if err := requireString("run_id", string(value.RunID)); err != nil {
		return err
	}
	if !value.ExecutionVersion.Valid() {
		return errors.New("execution_version must be positive")
	}
	if !value.NextAction.Valid() {
		return errors.New("next_action is not supported")
	}
	if value.ResolvedReferences == nil {
		return errors.New("resolved_references must be an array, not null or missing")
	}
	if len(value.ResolvedReferences) > maxResolvedReferences {
		return fmt.Errorf("resolved_references exceeds %d entries", maxResolvedReferences)
	}
	if value.PlanID != nil {
		if err := requireString("plan_id", string(*value.PlanID)); err != nil {
			return err
		}
	}
	if value.CurrentStepID != nil {
		if err := requireString("current_step_id", string(*value.CurrentStepID)); err != nil {
			return err
		}
	}
	for index, reference := range value.ResolvedReferences {
		if err := validateResolvedReference(reference); err != nil {
			return fmt.Errorf("resolved_references[%d]: %w", index, err)
		}
	}
	if value.ApprovalContext != nil {
		if err := validateApprovalContext(*value.ApprovalContext, value.ExecutionVersion); err != nil {
			return fmt.Errorf("approval_context: %w", err)
		}
	}
	return validateNextActionShape(value)
}

func validateNextActionShape(value contracts.RuntimeContextV1) error {
	hasPlan := value.PlanID != nil
	hasStep := value.CurrentStepID != nil
	hasApproval := value.ApprovalContext != nil
	hasReferences := len(value.ResolvedReferences) != 0

	switch value.NextAction {
	case contracts.CheckpointNextActionGeneratePlan:
		if hasPlan || hasStep || hasApproval || hasReferences {
			return errors.New("GENERATE_PLAN requires no plan, step, approval, or resolved references")
		}
	case contracts.CheckpointNextActionExecuteStep:
		if !hasPlan || !hasStep || hasApproval {
			return errors.New("EXECUTE_STEP requires plan and current step without approval context")
		}
	case contracts.CheckpointNextActionRequestApproval:
		if !hasPlan || !hasStep {
			return errors.New("REQUEST_APPROVAL requires plan and current step")
		}
		if hasApproval && value.ApprovalContext.ApprovalExecutionVersion != value.ExecutionVersion {
			return errors.New("REQUEST_APPROVAL approval must belong to the current execution version")
		}
	case contracts.CheckpointNextActionExecuteApprovedTool:
		if !hasPlan || !hasStep || !hasApproval {
			return errors.New("EXECUTE_APPROVED_TOOL requires plan, current step, and approval context")
		}
	case contracts.CheckpointNextActionFinalizeRun:
		if !hasPlan || !hasStep || hasApproval || hasReferences {
			return errors.New("FINALIZE_RUN requires plan and final current step without approval or resolved references")
		}
	}
	return nil
}

func validateResolvedReference(reference contracts.ResolvedReference) error {
	if len(reference.TargetPath) == 0 {
		return errors.New("target_path must not be empty")
	}
	if len(reference.TargetPath) > maxReferencePathDepth {
		return fmt.Errorf("target_path exceeds %d segments", maxReferencePathDepth)
	}
	if err := requireString("source_step_id", string(reference.SourceStepID)); err != nil {
		return err
	}
	if err := requireString("source_output_field", reference.SourceOutputField); err != nil {
		return err
	}
	if !outputFieldNamePattern.MatchString(reference.SourceOutputField) {
		return errors.New("source_output_field is not a valid OutputSchema field name")
	}
	for index, segment := range reference.TargetPath {
		switch segment.Kind {
		case contracts.ReferencePathSegmentKey:
			if segment.Key == nil || segment.Index != nil {
				return fmt.Errorf("target_path[%d] key segment must contain only key", index)
			}
			if err := requireString("key", *segment.Key); err != nil {
				return fmt.Errorf("target_path[%d]: %w", index, err)
			}
		case contracts.ReferencePathSegmentIndex:
			if segment.Index == nil || segment.Key != nil {
				return fmt.Errorf("target_path[%d] index segment must contain only index", index)
			}
		default:
			return fmt.Errorf("target_path[%d] kind is not supported", index)
		}
	}
	return nil
}

func validateApprovalContext(value contracts.ApprovalContext, executionVersion contracts.ExecutionVersion) error {
	if err := requireString("approval_id", string(value.ApprovalID)); err != nil {
		return err
	}
	if !value.ApprovalExecutionVersion.Valid() || value.ApprovalExecutionVersion > executionVersion {
		return errors.New("approval_execution_version must be positive and not exceed execution_version")
	}
	if err := requireString("tool_name", string(value.ToolName)); err != nil {
		return err
	}
	if err := requireJSONObject("frozen_tool_input", value.FrozenToolInput); err != nil {
		return err
	}
	if err := requireJSONObject("observed_values", value.ObservedValues); err != nil {
		return err
	}
	if err := requireString("resource_version", string(value.ResourceVersion)); err != nil {
		return err
	}
	if !value.FrozenInputHash.Valid() {
		return errors.New("frozen_input_hash must be a lowercase SHA-256 value")
	}
	return nil
}

func requireJSONObject(field string, value []byte) error {
	if len(value) == 0 || !json.Valid(value) {
		return fmt.Errorf("%s must be a valid JSON object", field)
	}
	decoder := json.NewDecoder(bytes.NewReader(value))
	token, err := decoder.Token()
	if err != nil {
		return fmt.Errorf("%s must be a valid JSON object", field)
	}
	delimiter, ok := token.(json.Delim)
	if !ok || delimiter != '{' {
		return fmt.Errorf("%s must be a JSON object", field)
	}
	return nil
}

func requireString(field, value string) error {
	if value == "" {
		return fmt.Errorf("%s must not be empty", field)
	}
	if !utf8.ValidString(value) {
		return fmt.Errorf("%s must be valid UTF-8", field)
	}
	return nil
}
