// Package references 实现跨 Planner、Step Executor 与 Checkpoint 的唯一 Step 引用协议。
package references

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/zhaohaip/agentops-go/internal/contracts"
)

const (
	// MaxResolvedReferencesPerStep 是冻结协议允许的单 Step 引用上限。
	MaxResolvedReferencesPerStep = 256
	// MaxTargetPathDepth 是冻结协议允许的 target_path 深度上限。
	MaxTargetPathDepth = 16
)

var (
	// ErrInvalidActionMode 表示调用方传入了未知 action mode。
	ErrInvalidActionMode = errors.New("reference action mode is invalid")
	// ErrInvalidStepInput 表示 Step input 不是可遍历的严格 JSON object。
	ErrInvalidStepInput = errors.New("step input is invalid")
	// ErrReferenceSyntax 表示保留引用前缀被用于非法模板、拼接或多级字段。
	ErrReferenceSyntax = errors.New("step reference syntax is invalid")
	// ErrExpressionNotSupported 表示引用被包裹在模板、函数或条件表达式中。
	// 它保留 ErrReferenceSyntax 的 errors.Is 兼容性，供需要更细稳定分类的调用方使用。
	ErrExpressionNotSupported = fmt.Errorf("%w: expression is not supported", ErrReferenceSyntax)
	// ErrReferencePath 表示引用 target_path 为空或超过冻结深度。
	ErrReferencePath = errors.New("step reference target path is invalid")
	// ErrDuplicateTarget 表示同一 target_path 出现重复引用。
	ErrDuplicateTarget = errors.New("step reference target is duplicated")
	// ErrSourceStep 表示引用来源不是已完成的紧邻前序 Step。
	ErrSourceStep = errors.New("step reference source step is invalid")
	// ErrSourceOutput 表示引用字段未同时出现在来源 Schema 和安全输出中。
	ErrSourceOutput = errors.New("step reference source output is invalid")
)

var (
	outputFieldPattern       = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
	referenceTokenPattern    = regexp.MustCompile(`step\.output\.[A-Za-z_][A-Za-z0-9_]*`)
	functionExpression       = regexp.MustCompile(`(?s)^[A-Za-z_][A-Za-z0-9_]*(?:(?:\.|::|:)[A-Za-z_][A-Za-z0-9_]*)*[ \t]*\(.*\)$`)
	conditionalExpression    = regexp.MustCompile(`(?s)^if[ \t]+.+[ \t]+then(?:[ \t]+.+)?(?:[ \t]+else[ \t]+.+)?$`)
	dollarTemplateFragment   = regexp.MustCompile(`(?s)\$\{[^}]*step\.output\.[A-Za-z_][A-Za-z0-9_]*[^}]*\}`)
	mustacheTemplateFragment = regexp.MustCompile(`(?s)\{\{.*?step\.output\.[A-Za-z_][A-Za-z0-9_]*.*?\}\}`)
)

type referenceStringClass uint8

const (
	referenceStringPlainText referenceStringClass = iota
	referenceStringLegal
	referenceStringReservedPrefixInvalid
	referenceStringExpression
)

// SourceStep 是提取器所需的最小前序 Step 投影。
type SourceStep struct {
	StepID       contracts.StepID
	Sequence     uint32
	Status       contracts.StepStatus
	OutputSchema contracts.OutputSchema
	SafeOutput   json.RawMessage
}

// ExtractRequest 是一次共享引用提取请求。
//
// ValidatePersistedOutput=false 用于 Planner 静态校验；true 用于 Step 与
// Checkpoint 的运行期绑定，并要求 SafeOutput 中已经存在被引用字段。
type ExtractRequest struct {
	ActionMode              contracts.ReferenceActionMode
	StepInput               json.RawMessage
	TargetStepSequence      uint32
	SourceStep              *SourceStep
	ValidatePersistedOutput bool
}

// StaticReference 是 Planner 持久化 Step 前可验证的引用事实，不包含数据库 StepID。
type StaticReference struct {
	TargetPath        []contracts.ReferencePathSegment
	SourceOutputField string
}

// CanonicalStaticReferences 是按共享路径规则排序的静态引用事实。
type CanonicalStaticReferences []StaticReference

// ExtractResult 明确区分 Planner 静态事实与运行期持久化绑定。
// 两个分支互斥，未使用的分支保持 nil。
type ExtractResult struct {
	StaticReferences   CanonicalStaticReferences
	ResolvedReferences contracts.CanonicalResolvedReferences
}

// IssueError 携带共享契约冻结的稳定引用 issue code。
type IssueError struct {
	Code contracts.ReferenceIssueCode
}

// Error 实现 error。
func (e *IssueError) Error() string {
	return string(e.Code)
}

// Extractor 持有唯一的 Step 引用遍历、绑定和规范化算法。
type Extractor struct{}

// NewStepReferenceExtractor 创建共享 Step 引用提取器。
func NewStepReferenceExtractor() Extractor {
	return Extractor{}
}

// ActionModeForNextAction 返回五种冻结动作唯一对应的引用模式。
func ActionModeForNextAction(action contracts.CheckpointNextAction) (contracts.ReferenceActionMode, error) {
	switch action {
	case contracts.CheckpointNextActionExecuteStep,
		contracts.CheckpointNextActionRequestApproval,
		contracts.CheckpointNextActionExecuteApprovedTool:
		return contracts.ReferenceActionModeTargetStepInput, nil
	case contracts.CheckpointNextActionGeneratePlan,
		contracts.CheckpointNextActionFinalizeRun:
		return contracts.ReferenceActionModeNoStepInput, nil
	default:
		return "", ErrInvalidActionMode
	}
}

// Extract 提取并按冻结 target_path 规则排序引用。
func (Extractor) Extract(request ExtractRequest) (ExtractResult, error) {
	switch request.ActionMode {
	case contracts.ReferenceActionModeNoStepInput:
		return emptyResult(request.ValidatePersistedOutput), nil
	case contracts.ReferenceActionModeTargetStepInput:
		return extractTargetStepInput(request)
	default:
		return ExtractResult{}, ErrInvalidActionMode
	}
}

type referenceFact struct {
	TargetPath        []contracts.ReferencePathSegment
	SourceOutputField string
}

func extractTargetStepInput(request ExtractRequest) (ExtractResult, error) {
	if !utf8.Valid(request.StepInput) {
		return ExtractResult{}, fmt.Errorf("%w: input is not valid UTF-8", ErrInvalidStepInput)
	}
	decoder := json.NewDecoder(bytes.NewReader(request.StepInput))
	first, err := decoder.Token()
	if err != nil {
		return ExtractResult{}, fmt.Errorf("%w: %v", ErrInvalidStepInput, err)
	}
	if delimiter, ok := first.(json.Delim); !ok || delimiter != '{' {
		return ExtractResult{}, fmt.Errorf("%w: top-level value must be an object", ErrInvalidStepInput)
	}

	facts := make([]referenceFact, 0)
	targets := make(map[string]struct{})
	if err := walkObject(decoder, nil, request, &facts, targets); err != nil {
		return ExtractResult{}, err
	}
	if err := requireEOF(decoder); err != nil {
		return ExtractResult{}, fmt.Errorf("%w: %v", ErrInvalidStepInput, err)
	}
	sort.Slice(facts, func(left, right int) bool {
		return compareFacts(facts[left], facts[right]) < 0
	})
	return projectResult(request, facts), nil
}

func walkObject(
	decoder *json.Decoder,
	path []contracts.ReferencePathSegment,
	request ExtractRequest,
	facts *[]referenceFact,
	targets map[string]struct{},
) error {
	seen := make(map[string]struct{})
	for decoder.More() {
		fieldToken, err := decoder.Token()
		if err != nil {
			return fmt.Errorf("%w: read object field: %v", ErrInvalidStepInput, err)
		}
		field, ok := fieldToken.(string)
		if !ok || !utf8.ValidString(field) {
			return fmt.Errorf("%w: object field is invalid", ErrInvalidStepInput)
		}
		if _, duplicate := seen[field]; duplicate {
			return fmt.Errorf("%w: %s", ErrDuplicateTarget, formatPath(pathWithKey(path, field)))
		}
		seen[field] = struct{}{}
		if err := walkValue(decoder, pathWithKey(path, field), request, facts, targets); err != nil {
			return err
		}
	}
	if _, err := decoder.Token(); err != nil {
		return fmt.Errorf("%w: close object: %v", ErrInvalidStepInput, err)
	}
	return nil
}

func walkArray(
	decoder *json.Decoder,
	path []contracts.ReferencePathSegment,
	request ExtractRequest,
	facts *[]referenceFact,
	targets map[string]struct{},
) error {
	var index uint64
	for decoder.More() {
		if err := walkValue(decoder, pathWithIndex(path, index), request, facts, targets); err != nil {
			return err
		}
		index++
	}
	if _, err := decoder.Token(); err != nil {
		return fmt.Errorf("%w: close array: %v", ErrInvalidStepInput, err)
	}
	return nil
}

func walkValue(
	decoder *json.Decoder,
	path []contracts.ReferencePathSegment,
	request ExtractRequest,
	facts *[]referenceFact,
	targets map[string]struct{},
) error {
	token, err := decoder.Token()
	if err != nil {
		return fmt.Errorf("%w: read value: %v", ErrInvalidStepInput, err)
	}
	if delimiter, ok := token.(json.Delim); ok {
		switch delimiter {
		case '{':
			return walkObject(decoder, path, request, facts, targets)
		case '[':
			return walkArray(decoder, path, request, facts, targets)
		default:
			return fmt.Errorf("%w: unexpected delimiter %q", ErrInvalidStepInput, delimiter)
		}
	}
	value, ok := token.(string)
	if !ok {
		return nil
	}
	field, referenced, err := parseReference(value)
	if err != nil {
		return fmt.Errorf("%w at %s", err, formatPath(path))
	}
	if !referenced {
		return nil
	}
	if len(path) == 0 {
		return ErrReferencePath
	}
	if len(path) > MaxTargetPathDepth {
		return fmt.Errorf("%w: %s exceeds %d segments", ErrReferencePath, formatPath(path), MaxTargetPathDepth)
	}
	if err := validateSource(request, field); err != nil {
		return err
	}
	targetKey := encodePath(path)
	if _, duplicate := targets[targetKey]; duplicate {
		return fmt.Errorf("%w: %s", ErrDuplicateTarget, formatPath(path))
	}
	targets[targetKey] = struct{}{}
	*facts = append(*facts, referenceFact{
		TargetPath:        clonePath(path),
		SourceOutputField: field,
	})
	if len(*facts) > MaxResolvedReferencesPerStep {
		return &IssueError{Code: contracts.ReferenceIssueCodeCountLimitExceeded}
	}
	return nil
}

func parseReference(value string) (string, bool, error) {
	class, field := classifyReferenceString(value)
	switch class {
	case referenceStringLegal:
		return field, true, nil
	case referenceStringReservedPrefixInvalid:
		return "", false, ErrReferenceSyntax
	case referenceStringExpression:
		return "", false, ErrExpressionNotSupported
	default:
		return "", false, nil
	}
}

// classifyReferenceString 将字符串封闭划分为纯引用、非法保留前缀、
// 明确表达式和普通文本。分类只依赖固定词法外壳，不枚举函数或操作符。
func classifyReferenceString(value string) (referenceStringClass, string) {
	const prefix = "step.output."
	if strings.HasPrefix(value, prefix) {
		field := strings.TrimPrefix(value, prefix)
		if outputFieldPattern.MatchString(field) {
			return referenceStringLegal, field
		}
		return referenceStringReservedPrefixInvalid, ""
	}
	if !referenceTokenPattern.MatchString(value) {
		return referenceStringPlainText, ""
	}

	trimmed := strings.TrimSpace(value)
	if dollarTemplateFragment.MatchString(trimmed) || mustacheTemplateFragment.MatchString(trimmed) ||
		functionExpression.MatchString(trimmed) || conditionalExpression.MatchString(trimmed) {
		return referenceStringExpression, ""
	}
	return referenceStringPlainText, ""
}

func validateSource(request ExtractRequest, field string) error {
	source := request.SourceStep
	if source == nil || request.TargetStepSequence <= 1 ||
		source.Sequence+1 != request.TargetStepSequence {
		return ErrSourceStep
	}
	if _, exists := source.OutputSchema[field]; !exists {
		return fmt.Errorf("%w: field %q is absent from output schema", ErrSourceOutput, field)
	}
	if !request.ValidatePersistedOutput {
		return nil
	}
	if source.StepID == "" || source.Status != contracts.StepStatusCompleted {
		return ErrSourceStep
	}
	if !utf8.Valid(source.SafeOutput) {
		return fmt.Errorf("%w: safe output is not valid UTF-8", ErrSourceOutput)
	}
	var output map[string]json.RawMessage
	if err := json.Unmarshal(source.SafeOutput, &output); err != nil || output == nil {
		return fmt.Errorf("%w: safe output must be an object", ErrSourceOutput)
	}
	if _, exists := output[field]; !exists {
		return fmt.Errorf("%w: field %q is absent from safe output", ErrSourceOutput, field)
	}
	return nil
}

func compareFacts(left, right referenceFact) int {
	if compared := comparePaths(left.TargetPath, right.TargetPath); compared != 0 {
		return compared
	}
	return bytes.Compare([]byte(left.SourceOutputField), []byte(right.SourceOutputField))
}

func emptyResult(runtime bool) ExtractResult {
	if runtime {
		return ExtractResult{ResolvedReferences: contracts.CanonicalResolvedReferences{}}
	}
	return ExtractResult{StaticReferences: CanonicalStaticReferences{}}
}

func projectResult(request ExtractRequest, facts []referenceFact) ExtractResult {
	if !request.ValidatePersistedOutput {
		static := make(CanonicalStaticReferences, 0, len(facts))
		for _, fact := range facts {
			static = append(static, StaticReference{
				TargetPath:        clonePath(fact.TargetPath),
				SourceOutputField: fact.SourceOutputField,
			})
		}
		return ExtractResult{StaticReferences: static}
	}

	resolved := make(contracts.CanonicalResolvedReferences, 0, len(facts))
	for _, fact := range facts {
		resolved = append(resolved, contracts.ResolvedReference{
			TargetPath:        clonePath(fact.TargetPath),
			SourceStepID:      request.SourceStep.StepID,
			SourceOutputField: fact.SourceOutputField,
		})
	}
	return ExtractResult{ResolvedReferences: resolved}
}

func comparePaths(left, right []contracts.ReferencePathSegment) int {
	limit := min(len(left), len(right))
	for index := 0; index < limit; index++ {
		leftSegment, rightSegment := left[index], right[index]
		if leftSegment.Kind != rightSegment.Kind {
			if leftSegment.Kind == contracts.ReferencePathSegmentKey {
				return -1
			}
			return 1
		}
		if leftSegment.Kind == contracts.ReferencePathSegmentKey {
			if compared := bytes.Compare([]byte(*leftSegment.Key), []byte(*rightSegment.Key)); compared != 0 {
				return compared
			}
			continue
		}
		if *leftSegment.Index < *rightSegment.Index {
			return -1
		}
		if *leftSegment.Index > *rightSegment.Index {
			return 1
		}
	}
	if len(left) < len(right) {
		return -1
	}
	if len(left) > len(right) {
		return 1
	}
	return 0
}

func pathWithKey(path []contracts.ReferencePathSegment, key string) []contracts.ReferencePathSegment {
	keyCopy := key
	return append(clonePath(path), contracts.ReferencePathSegment{Kind: contracts.ReferencePathSegmentKey, Key: &keyCopy})
}

func pathWithIndex(path []contracts.ReferencePathSegment, index uint64) []contracts.ReferencePathSegment {
	indexCopy := index
	return append(clonePath(path), contracts.ReferencePathSegment{Kind: contracts.ReferencePathSegmentIndex, Index: &indexCopy})
}

func clonePath(path []contracts.ReferencePathSegment) []contracts.ReferencePathSegment {
	return append([]contracts.ReferencePathSegment(nil), path...)
}

func encodePath(path []contracts.ReferencePathSegment) string {
	var encoded strings.Builder
	for _, segment := range path {
		if segment.Kind == contracts.ReferencePathSegmentKey {
			fmt.Fprintf(&encoded, "k%d:%s;", len(*segment.Key), *segment.Key)
		} else {
			fmt.Fprintf(&encoded, "i%d;", *segment.Index)
		}
	}
	return encoded.String()
}

func formatPath(path []contracts.ReferencePathSegment) string {
	return encodePath(path)
}

func requireEOF(decoder *json.Decoder) error {
	var trailing any
	err := decoder.Decode(&trailing)
	if errors.Is(err, io.EOF) {
		return nil
	}
	if err != nil {
		return err
	}
	return errors.New("multiple JSON values are not allowed")
}
