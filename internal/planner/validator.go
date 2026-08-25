package planner

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"
	"sort"
	"strings"

	"github.com/zhaohaip/agentops-go/internal/contracts"
	"github.com/zhaohaip/agentops-go/internal/contracts/references"
)

const maxValidationIssues = 32

// ValidationIssueCode 表示 Plan 静态校验的稳定问题分类。
type ValidationIssueCode string

const (
	ValidationIssueGoalRequired                   ValidationIssueCode = "GOAL_REQUIRED"
	ValidationIssueStepCountInvalid               ValidationIssueCode = "STEP_COUNT_INVALID"
	ValidationIssueStepSequenceInvalid            ValidationIssueCode = "STEP_SEQUENCE_INVALID"
	ValidationIssueStepTypeInvalid                ValidationIssueCode = "STEP_TYPE_INVALID"
	ValidationIssueStepNameRequired               ValidationIssueCode = "STEP_NAME_REQUIRED"
	ValidationIssueFinalVerificationRequired      ValidationIssueCode = "FINAL_VERIFICATION_REQUIRED"
	ValidationIssueOutputSchemaInvalid            ValidationIssueCode = "OUTPUT_SCHEMA_INVALID"
	ValidationIssueOutputSchemaFieldLimitExceeded ValidationIssueCode = "OUTPUT_SCHEMA_FIELD_LIMIT_EXCEEDED"
	ValidationIssueOutputFieldNameTooLong         ValidationIssueCode = "OUTPUT_FIELD_NAME_TOO_LONG"
	ValidationIssueToolNameRequired               ValidationIssueCode = "TOOL_NAME_REQUIRED"
	ValidationIssueToolNameForbidden              ValidationIssueCode = "TOOL_NAME_FORBIDDEN"
	ValidationIssueToolNotFound                   ValidationIssueCode = "TOOL_NOT_FOUND"
	ValidationIssueToolDisabled                   ValidationIssueCode = "TOOL_DISABLED"
	ValidationIssueToolNotAllowed                 ValidationIssueCode = "TOOL_NOT_ALLOWED"
	ValidationIssueToolInputInvalid               ValidationIssueCode = "TOOL_INPUT_INVALID"
	ValidationIssueReferenceSyntaxInvalid         ValidationIssueCode = "REFERENCE_SYNTAX_INVALID"
	ValidationIssueReferenceNotAllowedOnFirstStep ValidationIssueCode = "REFERENCE_NOT_ALLOWED_ON_FIRST_STEP"
	ValidationIssueReferenceFieldNotFound         ValidationIssueCode = "REFERENCE_FIELD_NOT_FOUND"
	ValidationIssueReferenceTypeMismatch          ValidationIssueCode = "REFERENCE_TYPE_MISMATCH"
	ValidationIssueExpressionNotSupported         ValidationIssueCode = "EXPRESSION_NOT_SUPPORTED"
	ValidationIssueReferenceCountLimitExceeded    ValidationIssueCode = "REFERENCE_COUNT_LIMIT_EXCEEDED"
	ValidationIssueNonToolInputInvalid            ValidationIssueCode = "NON_TOOL_INPUT_INVALID"
	ValidationIssuePlanStepLimitExceeded          ValidationIssueCode = "PLAN_STEP_LIMIT_EXCEEDED"
	ValidationIssuePlanDraftTooLarge              ValidationIssueCode = "PLAN_DRAFT_TOO_LARGE"
	ValidationIssuePlanGoalTooLong                ValidationIssueCode = "PLAN_GOAL_TOO_LONG"
	ValidationIssueStepNameTooLong                ValidationIssueCode = "STEP_NAME_TOO_LONG"
	ValidationIssueStepInputTooLarge              ValidationIssueCode = "STEP_INPUT_TOO_LARGE"
	ValidationIssueJSONDepthExceeded              ValidationIssueCode = "JSON_DEPTH_EXCEEDED"
	ValidationIssueObjectFieldLimitExceeded       ValidationIssueCode = "OBJECT_FIELD_LIMIT_EXCEEDED"
	ValidationIssueValidationIssueLimitExceeded   ValidationIssueCode = "VALIDATION_ISSUE_LIMIT_EXCEEDED"
	ValidationIssueSensitiveContentDetected       ValidationIssueCode = "SENSITIVE_CONTENT_DETECTED"
	ValidationIssueUnsafePersistableContent       ValidationIssueCode = "UNSAFE_PERSISTABLE_CONTENT"
)

// Valid 报告错误码是否属于 P3-T03 静态 Validator 的封闭集合。
func (c ValidationIssueCode) Valid() bool {
	switch c {
	case ValidationIssueGoalRequired, ValidationIssueStepCountInvalid,
		ValidationIssueStepSequenceInvalid, ValidationIssueStepTypeInvalid,
		ValidationIssueStepNameRequired, ValidationIssueFinalVerificationRequired,
		ValidationIssueOutputSchemaInvalid, ValidationIssueOutputSchemaFieldLimitExceeded,
		ValidationIssueOutputFieldNameTooLong, ValidationIssueToolNameRequired,
		ValidationIssueToolNameForbidden, ValidationIssueToolNotFound,
		ValidationIssueToolDisabled, ValidationIssueToolNotAllowed,
		ValidationIssueToolInputInvalid, ValidationIssueReferenceSyntaxInvalid,
		ValidationIssueReferenceNotAllowedOnFirstStep, ValidationIssueReferenceFieldNotFound,
		ValidationIssueReferenceTypeMismatch, ValidationIssueExpressionNotSupported,
		ValidationIssueReferenceCountLimitExceeded, ValidationIssueNonToolInputInvalid,
		ValidationIssuePlanStepLimitExceeded, ValidationIssuePlanDraftTooLarge,
		ValidationIssuePlanGoalTooLong, ValidationIssueStepNameTooLong,
		ValidationIssueStepInputTooLarge, ValidationIssueJSONDepthExceeded,
		ValidationIssueObjectFieldLimitExceeded, ValidationIssueValidationIssueLimitExceeded,
		ValidationIssueSensitiveContentDetected, ValidationIssueUnsafePersistableContent:
		return true
	default:
		return false
	}
}

// ValidationIssue 是可进入 Repair 输入的安全、稳定校验问题。
type ValidationIssue struct {
	Code    ValidationIssueCode `json:"error_code"`
	Path    string              `json:"path"`
	Summary string              `json:"summary"`
}

// ValidatePlanRequest 是一次静态校验所需的不可变事实。
type ValidatePlanRequest struct {
	Draft        PlanDraft
	MaxSteps     uint32
	AllowedTools []string
	ToolSnapshot contracts.PlanningToolSnapshot
}

// Validator 对 PlanDraft 执行无外部副作用的确定性静态校验。
type Validator struct {
	referenceExtractor references.Extractor
}

// NewValidator 创建复用共享引用协议的 Plan Validator。
func NewValidator() Validator {
	return Validator{referenceExtractor: references.NewStepReferenceExtractor()}
}

type orderedValidationIssue struct {
	stepOrder uint32
	issue     ValidationIssue
}

// Validate 返回按 Step、路径和错误码稳定排序且数量有界的问题。
func (v Validator) Validate(request ValidatePlanRequest) []ValidationIssue {
	issues := make([]orderedValidationIssue, 0)
	add := func(stepOrder uint32, code ValidationIssueCode, path string) {
		issues = append(issues, orderedValidationIssue{
			stepOrder: stepOrder,
			issue:     ValidationIssue{Code: code, Path: path, Summary: validationIssueSummary(code)},
		})
	}

	validatePlanShape(request, add)
	tools := indexPlanningTools(request.ToolSnapshot.Tools)
	allowedTools := indexAllowedTools(request.AllowedTools)
	for index := range request.Draft.Steps {
		step := request.Draft.Steps[index]
		stepOrder := step.Sequence
		if stepOrder == 0 {
			stepOrder = uint32(index + 1)
		}
		validateStepShape(step, index, stepOrder, add)
		validateStepSemantics(v.referenceExtractor, request.Draft.Steps, index, stepOrder, tools, allowedTools, add)
	}
	validateDraftResourceLimits(request.Draft, add)
	for _, issue := range NewSafeResultProcessor().Validate(request.Draft) {
		issues = append(issues, orderedValidationIssue{issue: issue})
	}

	sort.SliceStable(issues, func(left, right int) bool {
		if issues[left].stepOrder != issues[right].stepOrder {
			return issues[left].stepOrder < issues[right].stepOrder
		}
		if issues[left].issue.Path != issues[right].issue.Path {
			return issues[left].issue.Path < issues[right].issue.Path
		}
		return issues[left].issue.Code < issues[right].issue.Code
	})
	if len(issues) > maxValidationIssues {
		issues = append(issues[:maxValidationIssues-1], orderedValidationIssue{
			issue: ValidationIssue{
				Code:    ValidationIssueValidationIssueLimitExceeded,
				Path:    "$",
				Summary: validationIssueSummary(ValidationIssueValidationIssueLimitExceeded),
			},
		})
	}
	result := make([]ValidationIssue, 0, len(issues))
	for _, issue := range issues {
		result = append(result, issue.issue)
	}
	return result
}

func validatePlanShape(request ValidatePlanRequest, add func(uint32, ValidationIssueCode, string)) {
	if strings.TrimSpace(request.Draft.Goal) == "" {
		add(0, ValidationIssueGoalRequired, "$.goal")
	}
	if len(request.Draft.Goal) > maxGoalBytes {
		add(0, ValidationIssuePlanGoalTooLong, "$.goal")
	}
	stepCount := len(request.Draft.Steps)
	if stepCount == 0 || request.MaxSteps == 0 || uint64(stepCount) > uint64(request.MaxSteps) {
		add(0, ValidationIssueStepCountInvalid, "$.steps")
	}
	if stepCount > maxPlanSteps {
		add(0, ValidationIssuePlanStepLimitExceeded, "$.steps")
	}
	if stepCount > 0 && request.Draft.Steps[stepCount-1].Type != contracts.StepTypeVerification {
		add(request.Draft.Steps[stepCount-1].Sequence, ValidationIssueFinalVerificationRequired,
			fmt.Sprintf("$.steps[%d].type", stepCount-1))
	}
}

func validateStepShape(
	step StepDraft,
	index int,
	stepOrder uint32,
	add func(uint32, ValidationIssueCode, string),
) {
	path := fmt.Sprintf("$.steps[%d]", index)
	if step.Sequence != uint32(index+1) {
		add(stepOrder, ValidationIssueStepSequenceInvalid, path+".sequence")
	}
	if !step.Type.Valid() {
		add(stepOrder, ValidationIssueStepTypeInvalid, path+".type")
	}
	if strings.TrimSpace(step.Name) == "" {
		add(stepOrder, ValidationIssueStepNameRequired, path+".name")
	}
	if len(step.Name) > maxStepNameBytes {
		add(stepOrder, ValidationIssueStepNameTooLong, path+".name")
	}
	input := step.Input.JSON()
	if len(input) > maxStepInputBytes {
		add(stepOrder, ValidationIssueStepInputTooLarge, path+".input")
	}
	validateOutputSchema(step.OutputSchema, path+".output_schema", stepOrder, add)
	if step.Type == contracts.StepTypeToolCall {
		if step.ToolName == nil || strings.TrimSpace(string(*step.ToolName)) == "" {
			add(stepOrder, ValidationIssueToolNameRequired, path+".tool_name")
		}
	} else if step.ToolName != nil {
		add(stepOrder, ValidationIssueToolNameForbidden, path+".tool_name")
	}
}

func validateOutputSchema(
	schema contracts.OutputSchema,
	path string,
	stepOrder uint32,
	add func(uint32, ValidationIssueCode, string),
) {
	if len(schema) == 0 {
		add(stepOrder, ValidationIssueOutputSchemaInvalid, path)
		return
	}
	if len(schema) > maxOutputFields {
		add(stepOrder, ValidationIssueOutputSchemaFieldLimitExceeded, path)
	}
	names := make([]string, 0, len(schema))
	for name := range schema {
		names = append(names, name)
	}
	slices.Sort(names)
	for _, name := range names {
		fieldPath := path + "." + dynamicJSONPathSegment
		if len(name) > maxOutputFieldNameBytes {
			add(stepOrder, ValidationIssueOutputFieldNameTooLong, fieldPath)
		}
		if !outputFieldNamePattern.MatchString(name) || !schema[name].Type.Valid() {
			add(stepOrder, ValidationIssueOutputSchemaInvalid, fieldPath)
		}
	}
}

func validateStepSemantics(
	extractor references.Extractor,
	steps []StepDraft,
	index int,
	stepOrder uint32,
	tools map[string]contracts.PlanningToolSpec,
	allowedTools map[string]struct{},
	add func(uint32, ValidationIssueCode, string),
) {
	step := steps[index]
	path := fmt.Sprintf("$.steps[%d].input", index)
	input := step.Input.JSON()
	if len(input) == 0 || !json.Valid(input) || bytes.TrimSpace(input)[0] != '{' {
		add(stepOrder, inputIssueCode(step.Type), path)
		return
	}

	var source *references.SourceStep
	var sourceOutputSchema contracts.OutputSchema
	if index > 0 {
		previous := steps[index-1]
		source = &references.SourceStep{Sequence: previous.Sequence, OutputSchema: previous.OutputSchema}
		sourceOutputSchema = previous.OutputSchema
	}
	result, extractErr := extractor.Extract(references.ExtractRequest{
		ActionMode:              contracts.ReferenceActionModeTargetStepInput,
		StepInput:               input,
		TargetStepSequence:      step.Sequence,
		SourceStep:              source,
		ValidatePersistedOutput: false,
	})
	if extractErr != nil {
		add(stepOrder, referenceIssueCode(extractErr, step.Sequence, step.Type), path)
	}

	if step.Type != contracts.StepTypeToolCall {
		validateNonToolDraftInput(step, path, stepOrder, sourceOutputSchema,
			result.StaticReferences, extractErr != nil, add)
		return
	}
	if step.ToolName == nil || strings.TrimSpace(string(*step.ToolName)) == "" {
		return
	}
	toolName := string(*step.ToolName)
	if _, allowed := allowedTools[toolName]; !allowed {
		add(stepOrder, ValidationIssueToolNotAllowed, fmt.Sprintf("$.steps[%d].tool_name", index))
		return
	}
	tool, exists := tools[toolName]
	if !exists {
		add(stepOrder, ValidationIssueToolNotFound, fmt.Sprintf("$.steps[%d].tool_name", index))
		return
	}
	if !tool.Enabled {
		add(stepOrder, ValidationIssueToolDisabled, fmt.Sprintf("$.steps[%d].tool_name", index))
	}
	validateToolInput(input, tool.InputSchema, path, stepOrder, sourceOutputSchema,
		result.StaticReferences, extractErr != nil, add)
}

func validateNonToolDraftInput(
	step StepDraft,
	path string,
	stepOrder uint32,
	sourceOutputSchema contracts.OutputSchema,
	staticReferences references.CanonicalStaticReferences,
	suppressStringTypeIssues bool,
	add func(uint32, ValidationIssueCode, string),
) {
	contract, ok := contracts.NonToolInputContract(step.Type)
	if !ok {
		return
	}
	fields, err := decodeObject(step.Input.JSON())
	if err != nil {
		add(stepOrder, ValidationIssueNonToolInputInvalid, path)
		return
	}
	byName := make(map[string]contracts.NonToolInputFieldContract, len(contract))
	for _, field := range contract {
		byName[field.Name] = field
		if field.Required {
			if _, exists := fields[field.Name]; !exists {
				add(stepOrder, ValidationIssueNonToolInputInvalid, path+"."+field.Name)
			}
		}
	}
	for _, name := range sortedFieldNames(fields) {
		field, exists := byName[name]
		if !exists {
			add(stepOrder, ValidationIssueNonToolInputInvalid, path+"."+dynamicJSONPathSegment)
			continue
		}
		fieldPath := path + "." + name
		staticReference, referenced := findStaticReference(staticReferences,
			[]contracts.ReferencePathSegment{referenceKey(name)})
		if referenced {
			if !field.ReferenceAllowed {
				add(stepOrder, ValidationIssueNonToolInputInvalid, fieldPath)
			} else if !referenceTypeCompatible(staticReference, sourceOutputSchema, field.Type) {
				add(stepOrder, ValidationIssueReferenceTypeMismatch, fieldPath)
			}
			continue
		}
		raw := fields[name]
		if suppressStringTypeIssues && isJSONString(raw) {
			continue
		}
		if !matchesNonToolFieldShape(raw, field) || containsNull(raw) {
			add(stepOrder, ValidationIssueNonToolInputInvalid, fieldPath)
		}
	}
	for _, reference := range staticReferences {
		if len(reference.TargetPath) != 1 || reference.TargetPath[0].Kind != contracts.ReferencePathSegmentKey {
			add(stepOrder, ValidationIssueNonToolInputInvalid, path)
		}
	}
}

func validateToolInput(
	input json.RawMessage,
	schema contracts.CanonicalJSONSchema,
	path string,
	stepOrder uint32,
	sourceOutputSchema contracts.OutputSchema,
	staticReferences references.CanonicalStaticReferences,
	suppressStringTypeIssues bool,
	add func(uint32, ValidationIssueCode, string),
) {
	if schema.Type != contracts.JSONSchemaTypeObject || schema.Nullable {
		add(stepOrder, ValidationIssueToolInputInvalid, path)
		return
	}
	validateToolSchemaValue(input, schema, nil, path, stepOrder, sourceOutputSchema,
		staticReferences, suppressStringTypeIssues, add)
}

func validateToolSchemaValue(
	raw json.RawMessage,
	schema contracts.CanonicalJSONSchema,
	targetPath []contracts.ReferencePathSegment,
	publicPath string,
	stepOrder uint32,
	sourceOutputSchema contracts.OutputSchema,
	staticReferences references.CanonicalStaticReferences,
	suppressStringTypeIssues bool,
	add func(uint32, ValidationIssueCode, string),
) {
	if reference, referenced := findStaticReference(staticReferences, targetPath); referenced {
		if !referenceTypeCompatible(reference, sourceOutputSchema, schema.Type) {
			add(stepOrder, ValidationIssueReferenceTypeMismatch, publicPath)
		}
		return
	}
	if suppressStringTypeIssues && isJSONString(raw) {
		return
	}
	if isNull(raw) {
		if !schema.Nullable {
			add(stepOrder, ValidationIssueToolInputInvalid, publicPath)
		}
		return
	}
	switch schema.Type {
	case contracts.JSONSchemaTypeObject:
		fields, err := decodeObject(raw)
		if err != nil {
			add(stepOrder, ValidationIssueToolInputInvalid, publicPath)
			return
		}
		for _, required := range schema.Required {
			if _, exists := fields[required]; !exists {
				add(stepOrder, ValidationIssueToolInputInvalid, publicPath+"."+required)
			}
		}
		for _, name := range sortedFieldNames(fields) {
			childSchema, exists := schema.Properties[name]
			if !exists {
				add(stepOrder, ValidationIssueToolInputInvalid, publicPath+"."+dynamicJSONPathSegment)
				continue
			}
			validateToolSchemaValue(fields[name], childSchema, appendReferenceKey(targetPath, name),
				publicPath+"."+name, stepOrder, sourceOutputSchema, staticReferences, suppressStringTypeIssues, add)
		}
	case contracts.JSONSchemaTypeArray:
		var elements []json.RawMessage
		if schema.Items == nil || json.Unmarshal(raw, &elements) != nil {
			add(stepOrder, ValidationIssueToolInputInvalid, publicPath)
			return
		}
		for index, element := range elements {
			validateToolSchemaValue(element, *schema.Items, appendReferenceIndex(targetPath, uint64(index)),
				fmt.Sprintf("%s[%d]", publicPath, index), stepOrder, sourceOutputSchema,
				staticReferences, suppressStringTypeIssues, add)
		}
	default:
		if !matchesJSONSchemaPrimitive(raw, schema.Type) {
			add(stepOrder, ValidationIssueToolInputInvalid, publicPath)
		}
	}
}

func validateDraftResourceLimits(draft PlanDraft, add func(uint32, ValidationIssueCode, string)) {
	encoded, err := marshalPlanDraft(draft)
	if err != nil {
		return
	}
	if len(encoded) > maxPlanDraftBytes {
		add(0, ValidationIssuePlanDraftTooLarge, "$")
	}
	if err := scanPlanJSON(encoded); err != nil {
		var parseErr *ParseError
		if !errors.As(err, &parseErr) {
			return
		}
		switch parseErr.Code {
		case ParseIssueJSONDepthExceeded:
			add(0, ValidationIssueJSONDepthExceeded, parseErr.Path)
		case ParseIssueObjectFieldLimitExceeded:
			add(0, ValidationIssueObjectFieldLimitExceeded, parseErr.Path)
		}
	}
}

func indexPlanningTools(tools []contracts.PlanningToolSpec) map[string]contracts.PlanningToolSpec {
	result := make(map[string]contracts.PlanningToolSpec, len(tools))
	for _, tool := range tools {
		result[tool.ToolName] = tool
	}
	return result
}

func indexAllowedTools(tools []string) map[string]struct{} {
	result := make(map[string]struct{}, len(tools))
	for _, tool := range tools {
		result[tool] = struct{}{}
	}
	return result
}

func inputIssueCode(stepType contracts.StepType) ValidationIssueCode {
	if stepType == contracts.StepTypeToolCall {
		return ValidationIssueToolInputInvalid
	}
	return ValidationIssueNonToolInputInvalid
}

func referenceIssueCode(err error, sequence uint32, stepType contracts.StepType) ValidationIssueCode {
	var issue *references.IssueError
	switch {
	case errors.As(err, &issue) && issue.Code == contracts.ReferenceIssueCodeCountLimitExceeded:
		return ValidationIssueReferenceCountLimitExceeded
	case errors.Is(err, references.ErrExpressionNotSupported):
		return ValidationIssueExpressionNotSupported
	case errors.Is(err, references.ErrReferenceSyntax):
		return ValidationIssueReferenceSyntaxInvalid
	case errors.Is(err, references.ErrSourceStep) && sequence <= 1:
		return ValidationIssueReferenceNotAllowedOnFirstStep
	case errors.Is(err, references.ErrSourceStep), errors.Is(err, references.ErrSourceOutput):
		return ValidationIssueReferenceFieldNotFound
	default:
		return inputIssueCode(stepType)
	}
}

func findStaticReference(
	referencesList references.CanonicalStaticReferences,
	path []contracts.ReferencePathSegment,
) (references.StaticReference, bool) {
	for _, reference := range referencesList {
		if referencePathsEqual(reference.TargetPath, path) {
			return reference, true
		}
	}
	return references.StaticReference{}, false
}

func referenceTypeCompatible(
	reference references.StaticReference,
	sourceSchema contracts.OutputSchema,
	target contracts.JSONSchemaType,
) bool {
	source, exists := sourceSchema[reference.SourceOutputField]
	if !exists {
		return false
	}
	if source.Type == contracts.OutputValueTypeInteger && target == contracts.JSONSchemaTypeNumber {
		return true
	}
	return contracts.JSONSchemaType(source.Type) == target
}

func referencePathsEqual(left, right []contracts.ReferencePathSegment) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index].Kind != right[index].Kind {
			return false
		}
		switch left[index].Kind {
		case contracts.ReferencePathSegmentKey:
			if left[index].Key == nil || right[index].Key == nil || *left[index].Key != *right[index].Key {
				return false
			}
		case contracts.ReferencePathSegmentIndex:
			if left[index].Index == nil || right[index].Index == nil || *left[index].Index != *right[index].Index {
				return false
			}
		default:
			return false
		}
	}
	return true
}

func referenceKey(key string) contracts.ReferencePathSegment {
	keyCopy := key
	return contracts.ReferencePathSegment{Kind: contracts.ReferencePathSegmentKey, Key: &keyCopy}
}

func appendReferenceKey(path []contracts.ReferencePathSegment, key string) []contracts.ReferencePathSegment {
	return append(append([]contracts.ReferencePathSegment(nil), path...), referenceKey(key))
}

func appendReferenceIndex(path []contracts.ReferencePathSegment, index uint64) []contracts.ReferencePathSegment {
	indexCopy := index
	return append(append([]contracts.ReferencePathSegment(nil), path...), contracts.ReferencePathSegment{
		Kind: contracts.ReferencePathSegmentIndex, Index: &indexCopy,
	})
}

func matchesJSONSchemaPrimitive(raw json.RawMessage, schemaType contracts.JSONSchemaType) bool {
	trimmed := bytes.TrimSpace(raw)
	switch schemaType {
	case contracts.JSONSchemaTypeString:
		var value string
		return json.Unmarshal(trimmed, &value) == nil
	case contracts.JSONSchemaTypeBoolean:
		var value bool
		return json.Unmarshal(trimmed, &value) == nil
	case contracts.JSONSchemaTypeInteger:
		value, ok := singleJSONNumberToken(trimmed)
		if !ok || strings.ContainsAny(string(value), ".eE") {
			return false
		}
		return true
	case contracts.JSONSchemaTypeNumber:
		_, ok := singleJSONNumberToken(trimmed)
		return ok
	default:
		return false
	}
}

func singleJSONNumberToken(raw []byte) (json.Number, bool) {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	token, err := decoder.Token()
	if err != nil {
		return "", false
	}
	number, ok := token.(json.Number)
	if !ok {
		return "", false
	}
	if _, err := decoder.Token(); !errors.Is(err, io.EOF) {
		return "", false
	}
	return number, true
}

func isJSONString(raw json.RawMessage) bool {
	var value string
	return json.Unmarshal(raw, &value) == nil
}

func validationIssueSummary(code ValidationIssueCode) string {
	return string(code)
}
