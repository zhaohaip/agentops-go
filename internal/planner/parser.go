package planner

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/zhaohaip/agentops-go/internal/contracts"
)

// ParseIssueCode 是 Plan V1 严格解析失败的稳定分类。
type ParseIssueCode string

const (
	ParseIssueInvalidJSON              ParseIssueCode = "INVALID_JSON"
	ParseIssueDuplicateJSONKey         ParseIssueCode = "DUPLICATE_JSON_KEY"
	ParseIssueUnknownField             ParseIssueCode = "UNKNOWN_FIELD"
	ParseIssueRequiredFieldMissing     ParseIssueCode = "REQUIRED_FIELD_MISSING"
	ParseIssueNullNotAllowed           ParseIssueCode = "NULL_NOT_ALLOWED"
	ParseIssueGoalRequired             ParseIssueCode = "GOAL_REQUIRED"
	ParseIssueStepCountInvalid         ParseIssueCode = "STEP_COUNT_INVALID"
	ParseIssueStepSequenceInvalid      ParseIssueCode = "STEP_SEQUENCE_INVALID"
	ParseIssueStepTypeInvalid          ParseIssueCode = "STEP_TYPE_INVALID"
	ParseIssueStepNameRequired         ParseIssueCode = "STEP_NAME_REQUIRED"
	ParseIssueOutputSchemaInvalid      ParseIssueCode = "OUTPUT_SCHEMA_INVALID"
	ParseIssueOutputFieldLimitExceeded ParseIssueCode = "OUTPUT_SCHEMA_FIELD_LIMIT_EXCEEDED"
	ParseIssueOutputFieldNameTooLong   ParseIssueCode = "OUTPUT_FIELD_NAME_TOO_LONG"
	ParseIssueToolNameRequired         ParseIssueCode = "TOOL_NAME_REQUIRED"
	ParseIssueToolNameForbidden        ParseIssueCode = "TOOL_NAME_FORBIDDEN"
	ParseIssueToolInputInvalid         ParseIssueCode = "TOOL_INPUT_INVALID"
	ParseIssueNonToolInputInvalid      ParseIssueCode = "NON_TOOL_INPUT_INVALID"
	ParseIssuePlanStepLimitExceeded    ParseIssueCode = "PLAN_STEP_LIMIT_EXCEEDED"
	ParseIssuePlanDraftTooLarge        ParseIssueCode = "PLAN_DRAFT_TOO_LARGE"
	ParseIssuePlanGoalTooLong          ParseIssueCode = "PLAN_GOAL_TOO_LONG"
	ParseIssueStepNameTooLong          ParseIssueCode = "STEP_NAME_TOO_LONG"
	ParseIssueStepInputTooLarge        ParseIssueCode = "STEP_INPUT_TOO_LARGE"
	ParseIssueJSONDepthExceeded        ParseIssueCode = "JSON_DEPTH_EXCEEDED"
	ParseIssueObjectFieldLimitExceeded ParseIssueCode = "OBJECT_FIELD_LIMIT_EXCEEDED"
	ParseIssueModelResponseTooLarge    ParseIssueCode = "MODEL_RESPONSE_TOO_LARGE"
)

// ParseError 只包含稳定分类和安全路径，不回显候选值或原始解析错误。
type ParseError struct {
	Code ParseIssueCode
	Path string
}

// Valid 报告 issue code 是否属于当前 V1 Parser 的封闭集合。
func (c ParseIssueCode) Valid() bool {
	switch c {
	case ParseIssueInvalidJSON, ParseIssueDuplicateJSONKey, ParseIssueUnknownField,
		ParseIssueRequiredFieldMissing, ParseIssueNullNotAllowed, ParseIssueGoalRequired,
		ParseIssueStepCountInvalid, ParseIssueStepSequenceInvalid, ParseIssueStepTypeInvalid,
		ParseIssueStepNameRequired, ParseIssueOutputSchemaInvalid, ParseIssueOutputFieldLimitExceeded,
		ParseIssueOutputFieldNameTooLong, ParseIssueToolNameRequired, ParseIssueToolNameForbidden,
		ParseIssueToolInputInvalid, ParseIssueNonToolInputInvalid, ParseIssuePlanStepLimitExceeded,
		ParseIssuePlanDraftTooLarge, ParseIssuePlanGoalTooLong, ParseIssueStepNameTooLong,
		ParseIssueStepInputTooLarge, ParseIssueJSONDepthExceeded, ParseIssueObjectFieldLimitExceeded,
		ParseIssueModelResponseTooLarge:
		return true
	default:
		return false
	}
}

// Error 实现 error。
func (e *ParseError) Error() string {
	if e == nil {
		return "Plan parse error"
	}
	if e.Path == "" {
		return string(e.Code)
	}
	return string(e.Code) + " at " + e.Path
}

// Parser 严格解析当前唯一支持的 Plan V1 线协议。
type Parser struct{}

// NewParser 创建无可变状态的 Plan V1 Parser。
func NewParser() Parser { return Parser{} }

// ParseV1 解析一个完整、单值且严格符合白名单结构的 Plan V1 JSON object。
func (Parser) ParseV1(encoded []byte) (PlanDraft, error) {
	if len(encoded) > maxModelResponseBytes {
		return PlanDraft{}, parseError(ParseIssueModelResponseTooLarge, "$")
	}
	if len(encoded) == 0 || !utf8.Valid(encoded) {
		return PlanDraft{}, parseError(ParseIssueInvalidJSON, "$")
	}
	if err := scanPlanJSON(encoded); err != nil {
		return PlanDraft{}, err
	}
	fields, err := decodeObject(encoded)
	if err != nil {
		return PlanDraft{}, parseError(ParseIssueInvalidJSON, "$")
	}
	if err := rejectUnknownFields(fields, map[string]struct{}{"goal": {}, "steps": {}}, "$"); err != nil {
		return PlanDraft{}, err
	}
	goalRaw, err := requiredField(fields, "goal", "$")
	if err != nil {
		return PlanDraft{}, err
	}
	goal, err := decodeString(goalRaw, "$.goal", ParseIssueInvalidJSON)
	if err != nil {
		return PlanDraft{}, err
	}
	if strings.TrimSpace(goal) == "" {
		return PlanDraft{}, parseError(ParseIssueGoalRequired, "$.goal")
	}
	if len(goal) > maxGoalBytes {
		return PlanDraft{}, parseError(ParseIssuePlanGoalTooLong, "$.goal")
	}

	stepsRaw, err := requiredField(fields, "steps", "$")
	if err != nil {
		return PlanDraft{}, err
	}
	if isNull(stepsRaw) {
		return PlanDraft{}, parseError(ParseIssueNullNotAllowed, "$.steps")
	}
	var rawSteps []json.RawMessage
	if err := json.Unmarshal(stepsRaw, &rawSteps); err != nil {
		return PlanDraft{}, parseError(ParseIssueInvalidJSON, "$.steps")
	}
	if len(rawSteps) == 0 {
		return PlanDraft{}, parseError(ParseIssueStepCountInvalid, "$.steps")
	}
	if len(rawSteps) > maxPlanSteps {
		return PlanDraft{}, parseError(ParseIssuePlanStepLimitExceeded, "$.steps")
	}
	draft := PlanDraft{Goal: goal, Steps: make([]StepDraft, 0, len(rawSteps))}
	for index, rawStep := range rawSteps {
		step, err := parseStep(rawStep, index)
		if err != nil {
			return PlanDraft{}, err
		}
		draft.Steps = append(draft.Steps, step)
	}
	canonical, err := marshalPlanDraft(draft)
	if err != nil {
		return PlanDraft{}, parseError(ParseIssueInvalidJSON, "$")
	}
	if len(canonical) > maxPlanDraftBytes {
		return PlanDraft{}, parseError(ParseIssuePlanDraftTooLarge, "$")
	}
	return draft, nil
}

const dynamicJSONPathSegment = "<field>"

var (
	outputFieldNamePattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
	stepObjectPathPattern  = regexp.MustCompile(`^\$\.steps\[[0-9]+\]$`)
)

func parseStep(encoded []byte, index int) (StepDraft, error) {
	path := fmt.Sprintf("$.steps[%d]", index)
	fields, err := decodeObject(encoded)
	if err != nil {
		return StepDraft{}, parseError(ParseIssueInvalidJSON, path)
	}
	allowed := map[string]struct{}{
		"sequence": {}, "type": {}, "name": {}, "input": {}, "output_schema": {}, "tool_name": {},
	}
	if err := rejectUnknownFields(fields, allowed, path); err != nil {
		return StepDraft{}, err
	}
	sequenceRaw, err := requiredField(fields, "sequence", path)
	if err != nil {
		return StepDraft{}, err
	}
	sequence, err := decodeSequence(sequenceRaw, path+".sequence")
	if err != nil {
		return StepDraft{}, err
	}
	typeRaw, err := requiredField(fields, "type", path)
	if err != nil {
		return StepDraft{}, err
	}
	typeValue, err := decodeString(typeRaw, path+".type", ParseIssueStepTypeInvalid)
	if err != nil {
		return StepDraft{}, err
	}
	stepType := contracts.StepType(typeValue)
	if !stepType.Valid() {
		return StepDraft{}, parseError(ParseIssueStepTypeInvalid, path+".type")
	}
	nameRaw, err := requiredField(fields, "name", path)
	if err != nil {
		return StepDraft{}, err
	}
	name, err := decodeString(nameRaw, path+".name", ParseIssueInvalidJSON)
	if err != nil {
		return StepDraft{}, err
	}
	if strings.TrimSpace(name) == "" {
		return StepDraft{}, parseError(ParseIssueStepNameRequired, path+".name")
	}
	if len(name) > maxStepNameBytes {
		return StepDraft{}, parseError(ParseIssueStepNameTooLong, path+".name")
	}
	inputRaw, err := requiredField(fields, "input", path)
	if err != nil {
		return StepDraft{}, err
	}
	input, err := parseStepInput(inputRaw, stepType, path+".input")
	if err != nil {
		return StepDraft{}, err
	}
	outputRaw, err := requiredField(fields, "output_schema", path)
	if err != nil {
		return StepDraft{}, err
	}
	outputSchema, err := parseOutputSchema(outputRaw, path+".output_schema")
	if err != nil {
		return StepDraft{}, err
	}
	toolName, err := parseToolName(fields, stepType, path)
	if err != nil {
		return StepDraft{}, err
	}
	return StepDraft{
		Sequence: sequence, Type: stepType, Name: name, Input: input,
		OutputSchema: outputSchema, ToolName: toolName,
	}, nil
}

func parseStepInput(encoded []byte, stepType contracts.StepType, path string) (StepInput, error) {
	if isNull(encoded) {
		return StepInput{}, parseError(ParseIssueNullNotAllowed, path)
	}
	input, err := newStepInput(encoded)
	if err != nil {
		code := ParseIssueToolInputInvalid
		if stepType != contracts.StepTypeToolCall {
			code = ParseIssueNonToolInputInvalid
		}
		return StepInput{}, parseError(code, path)
	}
	if len(input.encoded) > maxStepInputBytes {
		return StepInput{}, parseError(ParseIssueStepInputTooLarge, path)
	}
	if stepType == contracts.StepTypeToolCall {
		return input, nil
	}
	if err := validateNonToolInput(input.encoded, stepType, path); err != nil {
		return StepInput{}, err
	}
	return input, nil
}

func validateNonToolInput(encoded []byte, stepType contracts.StepType, path string) error {
	contract, ok := contracts.NonToolInputContract(stepType)
	if !ok {
		return parseError(ParseIssueNonToolInputInvalid, path)
	}
	fields, err := decodeObject(encoded)
	if err != nil {
		return parseError(ParseIssueNonToolInputInvalid, path)
	}
	allowed := make(map[string]struct{}, len(contract))
	byName := make(map[string]contracts.NonToolInputFieldContract, len(contract))
	for _, field := range contract {
		allowed[field.Name] = struct{}{}
		byName[field.Name] = field
		if field.Required {
			if _, exists := fields[field.Name]; !exists {
				return parseError(ParseIssueNonToolInputInvalid, fixedJSONFieldPath(path, field.Name))
			}
		}
	}
	if err := rejectUnknownAs(fields, allowed, path, ParseIssueNonToolInputInvalid); err != nil {
		return err
	}
	for _, name := range sortedFieldNames(fields) {
		raw := fields[name]
		field := byName[name]
		if isNull(raw) || containsNull(raw) {
			return parseError(ParseIssueNonToolInputInvalid, fixedJSONFieldPath(path, name))
		}
		if !matchesNonToolFieldShape(raw, field) {
			return parseError(ParseIssueNonToolInputInvalid, fixedJSONFieldPath(path, name))
		}
	}
	return nil
}

func matchesNonToolFieldShape(raw []byte, field contracts.NonToolInputFieldContract) bool {
	trimmed := bytes.TrimSpace(raw)
	switch field.Type {
	case contracts.JSONSchemaTypeString:
		var value string
		return json.Unmarshal(trimmed, &value) == nil && strings.TrimSpace(value) != ""
	case contracts.JSONSchemaTypeObject:
		if len(trimmed) > 0 && trimmed[0] == '{' {
			return true
		}
		if field.ReferenceAllowed {
			var placeholder string
			return json.Unmarshal(trimmed, &placeholder) == nil && placeholder != ""
		}
	}
	return false
}

func parseOutputSchema(encoded []byte, path string) (contracts.OutputSchema, error) {
	if isNull(encoded) {
		return nil, parseError(ParseIssueOutputSchemaInvalid, path)
	}
	fields, err := decodeObject(encoded)
	if err != nil || len(fields) == 0 {
		return nil, parseError(ParseIssueOutputSchemaInvalid, path)
	}
	if len(fields) > maxOutputFields {
		return nil, parseError(ParseIssueOutputFieldLimitExceeded, path)
	}
	result := make(contracts.OutputSchema, len(fields))
	names := sortedFieldNames(fields)
	for _, name := range names {
		rawDescription := fields[name]
		fieldPath := dynamicJSONFieldPath(path)
		if len(name) > maxOutputFieldNameBytes {
			return nil, parseError(ParseIssueOutputFieldNameTooLong, fieldPath)
		}
		if !outputFieldNamePattern.MatchString(name) {
			return nil, parseError(ParseIssueOutputSchemaInvalid, fieldPath)
		}
		description, err := decodeObject(rawDescription)
		if err != nil || len(description) != 1 {
			return nil, parseError(ParseIssueOutputSchemaInvalid, fieldPath)
		}
		if err := rejectUnknownAs(description, map[string]struct{}{"type": {}}, fieldPath, ParseIssueOutputSchemaInvalid); err != nil {
			return nil, err
		}
		typeRaw, exists := description["type"]
		if !exists || isNull(typeRaw) {
			return nil, parseError(ParseIssueOutputSchemaInvalid, fixedJSONFieldPath(fieldPath, "type"))
		}
		var typeValue string
		if err := json.Unmarshal(typeRaw, &typeValue); err != nil {
			return nil, parseError(ParseIssueOutputSchemaInvalid, fixedJSONFieldPath(fieldPath, "type"))
		}
		valueType := contracts.OutputValueType(typeValue)
		if !valueType.Valid() {
			return nil, parseError(ParseIssueOutputSchemaInvalid, fixedJSONFieldPath(fieldPath, "type"))
		}
		result[name] = contracts.OutputFieldSchema{Type: valueType}
	}
	return result, nil
}

func parseToolName(fields map[string]json.RawMessage, stepType contracts.StepType, path string) (*contracts.ToolName, error) {
	raw, exists := fields["tool_name"]
	if stepType != contracts.StepTypeToolCall {
		if exists {
			return nil, parseError(ParseIssueToolNameForbidden, path+".tool_name")
		}
		return nil, nil
	}
	if !exists || isNull(raw) {
		return nil, parseError(ParseIssueToolNameRequired, path+".tool_name")
	}
	var value string
	if err := json.Unmarshal(raw, &value); err != nil || strings.TrimSpace(value) == "" {
		return nil, parseError(ParseIssueToolNameRequired, path+".tool_name")
	}
	toolName := contracts.ToolName(value)
	return &toolName, nil
}

func scanPlanJSON(encoded []byte) error {
	decoder := json.NewDecoder(bytes.NewReader(encoded))
	decoder.UseNumber()
	token, err := decoder.Token()
	if err != nil {
		return parseError(ParseIssueInvalidJSON, "$")
	}
	root, ok := token.(json.Delim)
	if !ok || root != '{' {
		return parseError(ParseIssueInvalidJSON, "$")
	}
	if err := scanObject(decoder, "$", 1); err != nil {
		return err
	}
	var trailing json.RawMessage
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return parseError(ParseIssueInvalidJSON, "$")
	}
	return nil
}

func scanObject(decoder *json.Decoder, path string, depth int) error {
	if depth > maxJSONDepth {
		return parseError(ParseIssueJSONDepthExceeded, path)
	}
	seen := make(map[string]struct{})
	count := 0
	for decoder.More() {
		fieldToken, err := decoder.Token()
		if err != nil {
			return parseError(ParseIssueInvalidJSON, path)
		}
		field, ok := fieldToken.(string)
		if !ok || !utf8.ValidString(field) {
			return parseError(ParseIssueInvalidJSON, path)
		}
		fieldPath := scannedJSONFieldPath(path, field)
		if _, duplicate := seen[field]; duplicate {
			return parseError(ParseIssueDuplicateJSONKey, dynamicJSONFieldPath(path))
		}
		seen[field] = struct{}{}
		count++
		if count > maxObjectFields {
			return parseError(ParseIssueObjectFieldLimitExceeded, path)
		}
		if err := scanValue(decoder, fieldPath, depth); err != nil {
			return err
		}
	}
	if _, err := decoder.Token(); err != nil {
		return parseError(ParseIssueInvalidJSON, path)
	}
	return nil
}

func scanArray(decoder *json.Decoder, path string, depth int) error {
	if depth > maxJSONDepth {
		return parseError(ParseIssueJSONDepthExceeded, path)
	}
	index := 0
	for decoder.More() {
		if err := scanValue(decoder, fmt.Sprintf("%s[%d]", path, index), depth); err != nil {
			return err
		}
		index++
	}
	if _, err := decoder.Token(); err != nil {
		return parseError(ParseIssueInvalidJSON, path)
	}
	return nil
}

func scanValue(decoder *json.Decoder, path string, parentDepth int) error {
	token, err := decoder.Token()
	if err != nil {
		return parseError(ParseIssueInvalidJSON, path)
	}
	delimiter, composite := token.(json.Delim)
	if !composite {
		return nil
	}
	switch delimiter {
	case '{':
		return scanObject(decoder, path, parentDepth+1)
	case '[':
		return scanArray(decoder, path, parentDepth+1)
	default:
		return parseError(ParseIssueInvalidJSON, path)
	}
}

func decodeObject(encoded []byte) (map[string]json.RawMessage, error) {
	if isNull(encoded) {
		return nil, errors.New("object must not be null")
	}
	trimmed := bytes.TrimSpace(encoded)
	if len(trimmed) == 0 || trimmed[0] != '{' {
		return nil, errors.New("JSON value is not an object")
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(trimmed, &fields); err != nil || fields == nil {
		return nil, errors.New("JSON value is not an object")
	}
	return fields, nil
}

func requiredField(fields map[string]json.RawMessage, name, path string) (json.RawMessage, error) {
	raw, exists := fields[name]
	if !exists {
		return nil, parseError(ParseIssueRequiredFieldMissing, fixedJSONFieldPath(path, name))
	}
	if isNull(raw) {
		return nil, parseError(ParseIssueNullNotAllowed, fixedJSONFieldPath(path, name))
	}
	return raw, nil
}

func decodeString(encoded []byte, path string, code ParseIssueCode) (string, error) {
	if isNull(encoded) {
		return "", parseError(ParseIssueNullNotAllowed, path)
	}
	var value string
	if err := json.Unmarshal(encoded, &value); err != nil {
		return "", parseError(code, path)
	}
	return value, nil
}

func decodeSequence(encoded []byte, path string) (uint32, error) {
	if isNull(encoded) {
		return 0, parseError(ParseIssueNullNotAllowed, path)
	}
	value := strings.TrimSpace(string(encoded))
	if value == "" || strings.ContainsAny(value, ".eE+-") {
		return 0, parseError(ParseIssueStepSequenceInvalid, path)
	}
	parsed, err := strconv.ParseUint(value, 10, 32)
	if err != nil || parsed == 0 {
		return 0, parseError(ParseIssueStepSequenceInvalid, path)
	}
	return uint32(parsed), nil
}

func rejectUnknownFields(fields map[string]json.RawMessage, allowed map[string]struct{}, path string) error {
	return rejectUnknownAs(fields, allowed, path, ParseIssueUnknownField)
}

func rejectUnknownAs(fields map[string]json.RawMessage, allowed map[string]struct{}, path string, code ParseIssueCode) error {
	for _, name := range sortedFieldNames(fields) {
		if _, exists := allowed[name]; !exists {
			return parseError(code, dynamicJSONFieldPath(path))
		}
	}
	return nil
}

func sortedFieldNames(fields map[string]json.RawMessage) []string {
	names := make([]string, 0, len(fields))
	for name := range fields {
		names = append(names, name)
	}
	slices.Sort(names)
	return names
}

func isNull(encoded []byte) bool {
	return bytes.Equal(bytes.TrimSpace(encoded), []byte("null"))
}

func containsNull(encoded []byte) bool {
	decoder := json.NewDecoder(bytes.NewReader(encoded))
	decoder.UseNumber()
	for {
		token, err := decoder.Token()
		if errors.Is(err, io.EOF) {
			return false
		}
		if err != nil {
			return false
		}
		if token == nil {
			return true
		}
	}
}

func marshalPlanDraft(draft PlanDraft) ([]byte, error) {
	var buffer bytes.Buffer
	encoder := json.NewEncoder(&buffer)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(draft); err != nil {
		return nil, err
	}
	return bytes.TrimSuffix(buffer.Bytes(), []byte{'\n'}), nil
}

// fixedJSONFieldPath 只接受协议或强类型契约定义的固定字段名。
func fixedJSONFieldPath(path, field string) string {
	return path + "." + field
}

// dynamicJSONFieldPath 隐藏由模型控制的字段名。公开错误、Repair 输入和日志只能传播该安全路径。
func dynamicJSONFieldPath(path string) string {
	return path + "." + dynamicJSONPathSegment
}

// scannedJSONFieldPath 只保留扫描器能够从冻结线协议位置确定的字段名。
// 其余对象（尤其 input 与 OutputSchema）的键均属于模型控制数据。
func scannedJSONFieldPath(path, field string) string {
	if path == "$" {
		switch field {
		case "goal", "steps":
			return fixedJSONFieldPath(path, field)
		}
	}
	if stepObjectPathPattern.MatchString(path) {
		switch field {
		case "sequence", "type", "name", "input", "output_schema", "tool_name":
			return fixedJSONFieldPath(path, field)
		}
	}
	return dynamicJSONFieldPath(path)
}

func parseError(code ParseIssueCode, path string) error {
	return &ParseError{Code: code, Path: path}
}
