package stepexecutor

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/zhaohaip/agentops-go/internal/contracts"
	"github.com/zhaohaip/agentops-go/internal/contracts/references"
)

const (
	stepInputContractVersionV1   = "step-input-v1"
	maxResolvedStepInputBytes    = 1024 * 1024
	maxResolvedStepInputDepth    = 16
	maxResolvedInputObjectFields = 64
)

var (
	// ErrResolvedReferenceBinding 表示 Checkpoint 绑定与共享提取器结果不完全相等。
	ErrResolvedReferenceBinding = errors.New("resolved reference binding is inconsistent")
	// ErrResolvedInputSchema 表示引用替换后的完整输入不满足冻结 Schema。
	ErrResolvedInputSchema = errors.New("resolved Step input does not satisfy schema")
	// ErrResolvedInputStructure 表示引用替换后的完整输入超过冻结结构限制。
	ErrResolvedInputStructure = errors.New("resolved Step input exceeds structural limits")
	// ErrResolvedInputTooLarge 表示规范化后的完整输入超过冻结大小限制。
	ErrResolvedInputTooLarge = errors.New("resolved Step input exceeds size limit")
)

// ResolvedStepInput 是引用替换并完成运行期 Schema 校验的内存值。
type ResolvedStepInput struct {
	StepID               contracts.StepID
	Value                json.RawMessage
	ReferencedFields     contracts.CanonicalResolvedReferences
	InputContractVersion string
}

// InputResolver 使用共享引用提取器解析当前 Step 的紧邻前序输出。
type InputResolver struct {
	extractor references.Extractor
}

// NewInputResolver 创建无跨调用状态的 Input Resolver。
func NewInputResolver() InputResolver {
	return InputResolver{extractor: references.NewStepReferenceExtractor()}
}

// Resolve 从当前 Step 与可选的紧邻前序 Step 构造已校验输入。
func (r InputResolver) Resolve(request StepExecutionRequest) (ResolvedStepInput, error) {
	if len(request.ResolvedReferences) > references.MaxResolvedReferencesPerStep {
		return ResolvedStepInput{}, referenceLimitError(nil)
	}

	extracted, err := r.extractor.Extract(references.ExtractRequest{
		ActionMode:              contracts.ReferenceActionModeTargetStepInput,
		StepInput:               request.Step.Input,
		TargetStepSequence:      request.Step.Sequence,
		SourceStep:              resolverSourceStep(request.PreviousStep),
		ValidatePersistedOutput: true,
	})
	if err != nil {
		return ResolvedStepInput{}, mapExtractError(err)
	}
	if !resolvedReferencesEqual(extracted.ResolvedReferences, request.ResolvedReferences) {
		return ResolvedStepInput{}, contractResolutionError(ErrResolvedReferenceBinding)
	}

	resolvedValue, err := decodeResolverJSON(request.Step.Input)
	if err != nil {
		return ResolvedStepInput{}, contractResolutionError(err)
	}
	root, ok := resolvedValue.(map[string]any)
	if !ok {
		return ResolvedStepInput{}, contractResolutionError(ErrResolvedInputSchema)
	}

	outputFields, err := resolverOutputFields(request.PreviousStep, len(extracted.ResolvedReferences) > 0)
	if err != nil {
		return ResolvedStepInput{}, inputResolutionError(err)
	}
	for _, reference := range extracted.ResolvedReferences {
		if err := resolveReference(root, request, reference, outputFields); err != nil {
			if errors.Is(err, ErrResolvedReferenceBinding) {
				return ResolvedStepInput{}, contractResolutionError(err)
			}
			return ResolvedStepInput{}, inputResolutionError(err)
		}
	}
	if !validateResolvedInputStructure(root, 1) {
		return ResolvedStepInput{}, inputResolutionError(ErrResolvedInputStructure)
	}
	if !validateResolvedInput(request, root) {
		return ResolvedStepInput{}, inputResolutionError(ErrResolvedInputSchema)
	}

	encoded, err := json.Marshal(root)
	if err != nil {
		return ResolvedStepInput{}, contractResolutionError(err)
	}
	if len(encoded) > maxResolvedStepInputBytes {
		return ResolvedStepInput{}, inputResolutionError(ErrResolvedInputTooLarge)
	}
	return ResolvedStepInput{
		StepID:               request.Step.StepID,
		Value:                append(json.RawMessage(nil), encoded...),
		ReferencedFields:     cloneResolvedReferences(extracted.ResolvedReferences),
		InputContractVersion: stepInputContractVersionV1,
	}, nil
}

func validateResolvedInputStructure(value any, depth int) bool {
	switch typed := value.(type) {
	case map[string]any:
		if depth > maxResolvedStepInputDepth || len(typed) > maxResolvedInputObjectFields {
			return false
		}
		for _, child := range typed {
			if !validateResolvedInputStructure(child, depth+1) {
				return false
			}
		}
	case []any:
		if depth > maxResolvedStepInputDepth {
			return false
		}
		for _, child := range typed {
			if !validateResolvedInputStructure(child, depth+1) {
				return false
			}
		}
	}
	return true
}

func resolverSourceStep(previous *PreviousStepProjection) *references.SourceStep {
	if previous == nil {
		return nil
	}
	return &references.SourceStep{
		StepID: previous.StepID, Sequence: previous.Sequence, Status: previous.Status,
		OutputSchema: cloneOutputSchema(previous.OutputSchema), SafeOutput: cloneJSON(previous.SafeOutput),
	}
}

func mapExtractError(err error) error {
	var issue *references.IssueError
	switch {
	case errors.As(err, &issue) && issue.Code == contracts.ReferenceIssueCodeCountLimitExceeded:
		return referenceLimitError(err)
	case errors.Is(err, references.ErrExpressionNotSupported):
		return inputResolutionError(err)
	case errors.Is(err, references.ErrReferenceSyntax):
		return inputResolutionError(err)
	case errors.Is(err, references.ErrSourceStep):
		return inputResolutionError(err)
	case errors.Is(err, references.ErrSourceOutput):
		return inputResolutionError(err)
	case errors.Is(err, references.ErrInvalidActionMode),
		errors.Is(err, references.ErrInvalidStepInput),
		errors.Is(err, references.ErrReferencePath),
		errors.Is(err, references.ErrDuplicateTarget):
		return contractResolutionError(err)
	default:
		return contractResolutionError(err)
	}
}

func referenceLimitError(cause error) error {
	return NewRuntimeFatalError(
		contracts.ErrorCodeStepExecutorContractBroken, CauseReferenceCountLimitExceeded, cause,
	)
}

func contractResolutionError(cause error) error {
	return NewRuntimeFatalError(
		contracts.ErrorCodeStepExecutorContractBroken, CauseStepExecutorContractBroken, cause,
	)
}

func inputResolutionError(cause error) error {
	return newStepError(
		ErrorKindFailed, contracts.ErrorCodeInputResolutionFailed, CauseInputResolutionFailed, cause,
	)
}

func resolverOutputFields(
	previous *PreviousStepProjection,
	required bool,
) (map[string]json.RawMessage, error) {
	if !required {
		return nil, nil
	}
	if previous == nil {
		return nil, references.ErrSourceStep
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(previous.SafeOutput, &fields); err != nil || fields == nil {
		return nil, references.ErrSourceOutput
	}
	return fields, nil
}

func resolveReference(
	root map[string]any,
	request StepExecutionRequest,
	reference contracts.ResolvedReference,
	outputFields map[string]json.RawMessage,
) error {
	previous := request.PreviousStep
	if previous == nil {
		return references.ErrSourceStep
	}
	sourceSchema, exists := previous.OutputSchema[reference.SourceOutputField]
	if !exists {
		return references.ErrSourceOutput
	}
	raw, exists := outputFields[reference.SourceOutputField]
	if !exists {
		return references.ErrSourceOutput
	}
	value, err := decodeResolverJSON(raw)
	if err != nil || value == nil || !matchesOutputType(value, sourceSchema.Type) {
		return references.ErrSourceOutput
	}
	targetType, allowed, err := referenceTargetType(request, reference.TargetPath)
	if err != nil || !allowed || !referenceTypesCompatible(sourceSchema.Type, targetType) {
		return ErrResolvedInputSchema
	}
	if err := replaceResolverValue(root, reference.TargetPath, value); err != nil {
		return fmt.Errorf("replace resolved reference: %w", err)
	}
	return nil
}

func referenceTargetType(
	request StepExecutionRequest,
	path []contracts.ReferencePathSegment,
) (contracts.JSONSchemaType, bool, error) {
	if request.Step.Type == contracts.StepTypeToolCall {
		if request.ToolCapability == nil {
			return "", false, ErrResolvedInputSchema
		}
		schema, err := toolSchemaAtPath(request.ToolCapability.InputSchema, path)
		return schema.Type, true, err
	}
	contract, ok := contracts.NonToolInputContract(request.Step.Type)
	if !ok || len(path) != 1 || path[0].Kind != contracts.ReferencePathSegmentKey || path[0].Key == nil {
		return "", false, ErrResolvedInputSchema
	}
	for _, field := range contract {
		if field.Name == *path[0].Key {
			return field.Type, field.ReferenceAllowed, nil
		}
	}
	return "", false, ErrResolvedInputSchema
}

func toolSchemaAtPath(
	schema contracts.CanonicalJSONSchema,
	path []contracts.ReferencePathSegment,
) (contracts.CanonicalJSONSchema, error) {
	if len(path) == 0 {
		return contracts.CanonicalJSONSchema{}, ErrResolvedInputSchema
	}
	current := schema
	for _, segment := range path {
		switch segment.Kind {
		case contracts.ReferencePathSegmentKey:
			if segment.Key == nil || segment.Index != nil || current.Type != contracts.JSONSchemaTypeObject {
				return contracts.CanonicalJSONSchema{}, ErrResolvedInputSchema
			}
			child, exists := current.Properties[*segment.Key]
			if !exists {
				return contracts.CanonicalJSONSchema{}, ErrResolvedInputSchema
			}
			current = child
		case contracts.ReferencePathSegmentIndex:
			if segment.Index == nil || segment.Key != nil || current.Type != contracts.JSONSchemaTypeArray ||
				current.Items == nil {
				return contracts.CanonicalJSONSchema{}, ErrResolvedInputSchema
			}
			current = *current.Items
		default:
			return contracts.CanonicalJSONSchema{}, ErrResolvedInputSchema
		}
	}
	return current, nil
}

func referenceTypesCompatible(source contracts.OutputValueType, target contracts.JSONSchemaType) bool {
	return contracts.JSONSchemaType(source) == target ||
		(source == contracts.OutputValueTypeInteger && target == contracts.JSONSchemaTypeNumber)
}

func replaceResolverValue(root map[string]any, path []contracts.ReferencePathSegment, value any) error {
	var current any = root
	for index, segment := range path {
		last := index == len(path)-1
		switch segment.Kind {
		case contracts.ReferencePathSegmentKey:
			object, ok := current.(map[string]any)
			if !ok || segment.Key == nil || segment.Index != nil {
				return ErrResolvedReferenceBinding
			}
			if _, exists := object[*segment.Key]; !exists {
				return ErrResolvedReferenceBinding
			}
			if last {
				object[*segment.Key] = value
				return nil
			}
			current = object[*segment.Key]
		case contracts.ReferencePathSegmentIndex:
			array, ok := current.([]any)
			if !ok || segment.Index == nil || segment.Key != nil || *segment.Index >= uint64(len(array)) {
				return ErrResolvedReferenceBinding
			}
			if last {
				array[*segment.Index] = value
				return nil
			}
			current = array[*segment.Index]
		default:
			return ErrResolvedReferenceBinding
		}
	}
	return ErrResolvedReferenceBinding
}

func validateResolvedInput(request StepExecutionRequest, root map[string]any) bool {
	if request.Step.Type == contracts.StepTypeToolCall {
		return request.ToolCapability != nil &&
			request.ToolCapability.InputSchema.Type == contracts.JSONSchemaTypeObject &&
			!request.ToolCapability.InputSchema.Nullable &&
			validateToolSchemaValue(root, request.ToolCapability.InputSchema)
	}
	return validateNonToolInputValue(root, request.Step.Type)
}

func validateNonToolInputValue(root map[string]any, stepType contracts.StepType) bool {
	contract, ok := contracts.NonToolInputContract(stepType)
	if !ok {
		return false
	}
	byName := make(map[string]contracts.NonToolInputFieldContract, len(contract))
	for _, field := range contract {
		byName[field.Name] = field
		if field.Required {
			if _, exists := root[field.Name]; !exists {
				return false
			}
		}
	}
	for name, value := range root {
		field, exists := byName[name]
		if !exists || value == nil || containsResolverNull(value) {
			return false
		}
		switch field.Type {
		case contracts.JSONSchemaTypeString:
			text, ok := value.(string)
			if !ok || strings.TrimSpace(text) == "" {
				return false
			}
		case contracts.JSONSchemaTypeObject:
			if _, ok := value.(map[string]any); !ok {
				return false
			}
		default:
			return false
		}
	}
	return true
}

func validateToolSchemaValue(value any, schema contracts.CanonicalJSONSchema) bool {
	if value == nil {
		return schema.Nullable
	}
	switch schema.Type {
	case contracts.JSONSchemaTypeObject:
		object, ok := value.(map[string]any)
		if !ok {
			return false
		}
		for _, name := range schema.Required {
			if _, exists := object[name]; !exists {
				return false
			}
		}
		for name, childValue := range object {
			childSchema, exists := schema.Properties[name]
			if !exists || !validateToolSchemaValue(childValue, childSchema) {
				return false
			}
		}
		return true
	case contracts.JSONSchemaTypeArray:
		array, ok := value.([]any)
		if !ok || schema.Items == nil {
			return false
		}
		for _, element := range array {
			if !validateToolSchemaValue(element, *schema.Items) {
				return false
			}
		}
		return true
	default:
		return matchesJSONSchemaType(value, schema.Type)
	}
}

func matchesOutputType(value any, outputType contracts.OutputValueType) bool {
	return matchesJSONSchemaType(value, contracts.JSONSchemaType(outputType))
}

func matchesJSONSchemaType(value any, schemaType contracts.JSONSchemaType) bool {
	switch schemaType {
	case contracts.JSONSchemaTypeString:
		_, ok := value.(string)
		return ok
	case contracts.JSONSchemaTypeBoolean:
		_, ok := value.(bool)
		return ok
	case contracts.JSONSchemaTypeInteger:
		number, ok := value.(json.Number)
		return ok && !strings.ContainsAny(number.String(), ".eE")
	case contracts.JSONSchemaTypeNumber:
		_, ok := value.(json.Number)
		return ok
	case contracts.JSONSchemaTypeObject:
		_, ok := value.(map[string]any)
		return ok
	case contracts.JSONSchemaTypeArray:
		_, ok := value.([]any)
		return ok
	default:
		return false
	}
}

func containsResolverNull(value any) bool {
	if value == nil {
		return true
	}
	switch typed := value.(type) {
	case map[string]any:
		for _, child := range typed {
			if containsResolverNull(child) {
				return true
			}
		}
	case []any:
		for _, child := range typed {
			if containsResolverNull(child) {
				return true
			}
		}
	}
	return false
}

func decodeResolverJSON(raw []byte) (any, error) {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var value any
	if err := decoder.Decode(&value); err != nil {
		return nil, err
	}
	if err := decoder.Decode(new(any)); !errors.Is(err, io.EOF) {
		return nil, errors.New("expected exactly one JSON value")
	}
	return value, nil
}

func resolvedReferencesEqual(
	left contracts.CanonicalResolvedReferences,
	right contracts.CanonicalResolvedReferences,
) bool {
	if left == nil || right == nil || len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index].SourceStepID != right[index].SourceStepID ||
			left[index].SourceOutputField != right[index].SourceOutputField ||
			!resolvedPathsEqual(left[index].TargetPath, right[index].TargetPath) {
			return false
		}
	}
	return true
}

func resolvedPathsEqual(left, right []contracts.ReferencePathSegment) bool {
	if len(left) == 0 || len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index].Kind != right[index].Kind {
			return false
		}
		switch left[index].Kind {
		case contracts.ReferencePathSegmentKey:
			if left[index].Key == nil || right[index].Key == nil ||
				left[index].Index != nil || right[index].Index != nil ||
				*left[index].Key != *right[index].Key {
				return false
			}
		case contracts.ReferencePathSegmentIndex:
			if left[index].Index == nil || right[index].Index == nil ||
				left[index].Key != nil || right[index].Key != nil ||
				*left[index].Index != *right[index].Index {
				return false
			}
		default:
			return false
		}
	}
	return true
}

func cloneResolvedReferences(value contracts.CanonicalResolvedReferences) contracts.CanonicalResolvedReferences {
	cloned := make(contracts.CanonicalResolvedReferences, len(value))
	for index := range value {
		cloned[index] = value[index]
		cloned[index].TargetPath = make([]contracts.ReferencePathSegment, len(value[index].TargetPath))
		for pathIndex := range value[index].TargetPath {
			cloned[index].TargetPath[pathIndex] = value[index].TargetPath[pathIndex]
			cloned[index].TargetPath[pathIndex].Key = clonePointer(value[index].TargetPath[pathIndex].Key)
			cloned[index].TargetPath[pathIndex].Index = clonePointer(value[index].TargetPath[pathIndex].Index)
		}
	}
	return cloned
}
