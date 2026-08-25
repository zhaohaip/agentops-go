package planner

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/zhaohaip/agentops-go/internal/contracts"
)

var (
	privateKeyMarkerPattern = regexp.MustCompile(`-----BEGIN (?:[A-Z0-9]+ )*(?:ENCRYPTED )?PRIVATE KEY-----`)
	pgpPrivateKeyPattern    = regexp.MustCompile(`-----BEGIN PGP PRIVATE KEY BLOCK-----`)
	authorizationPattern    = regexp.MustCompile(`(?i)(?:^|[\t\r\n ,;])authorization[ \t]*[:=][ \t]*\S+`)
	bearerPattern           = regexp.MustCompile(`(?i)(?:^|[\t\r\n ,;:])bearer[ \t]+[A-Za-z0-9._~+/=-]+(?:$|[\t\r\n ,;])`)
	basicPattern            = regexp.MustCompile(`(?i)(?:^|[\t\r\n ,;:])basic[ \t]+([A-Za-z0-9+/]+={0,2})(?:$|[\t\r\n ,;])`)
)

// SafeResultProcessor 是 Plan 候选返回和 Repair 摘要共用的唯一安全门禁。
// 它只检查并拒绝，不截断、脱敏或改写候选。
type SafeResultProcessor struct{}

// NewSafeResultProcessor 创建无状态的安全结果处理器。
func NewSafeResultProcessor() SafeResultProcessor { return SafeResultProcessor{} }

// Validate 检查 PlanDraft 中所有可能持久化的字符串、JSON key 和字符串值。
func (SafeResultProcessor) Validate(draft PlanDraft) []ValidationIssue {
	collector := newSafeIssueCollector()
	inspectPlanSafety(draft, collector.add)
	return collector.result()
}

// SafeCandidateSummary 仅为通过同一安全门禁的候选生成规范化摘要。
// 不安全候选只返回稳定 issue，不返回任何候选 bytes。
func (processor SafeResultProcessor) SafeCandidateSummary(draft PlanDraft) (
	[]byte,
	[]ValidationIssue,
	error,
) {
	issues := processor.Validate(draft)
	if len(issues) != 0 {
		return nil, issues, nil
	}
	summary, err := canonicalCandidateSummary(draft)
	if err != nil {
		return nil, []ValidationIssue{newSafetyIssue(
			ValidationIssueUnsafePersistableContent,
			"$",
		)}, nil
	}
	return summary, nil, nil
}

type safeIssueCollector struct {
	issues []ValidationIssue
	seen   map[string]struct{}
}

func newSafeIssueCollector() *safeIssueCollector {
	return &safeIssueCollector{seen: make(map[string]struct{})}
}

func (collector *safeIssueCollector) add(code ValidationIssueCode, path string) {
	key := string(code) + "\x00" + path
	if _, exists := collector.seen[key]; exists {
		return
	}
	collector.seen[key] = struct{}{}
	collector.issues = append(collector.issues, newSafetyIssue(code, path))
}

func (collector *safeIssueCollector) result() []ValidationIssue {
	slices.SortFunc(collector.issues, func(left, right ValidationIssue) int {
		if comparison := strings.Compare(left.Path, right.Path); comparison != 0 {
			return comparison
		}
		return strings.Compare(string(left.Code), string(right.Code))
	})
	if len(collector.issues) > maxValidationIssues {
		collector.issues = append(collector.issues[:maxValidationIssues-1], newSafetyIssue(
			ValidationIssueValidationIssueLimitExceeded,
			"$",
		))
	}
	return slices.Clone(collector.issues)
}

func newSafetyIssue(code ValidationIssueCode, path string) ValidationIssue {
	return ValidationIssue{Code: code, Path: path, Summary: validationIssueSummary(code)}
}

func inspectPlanSafety(draft PlanDraft, add func(ValidationIssueCode, string)) {
	inspectPersistableString(draft.Goal, "$.goal", add)
	for index, step := range draft.Steps {
		path := "$.steps[" + jsonIndex(index) + "]"
		inspectPersistableString(string(step.Type), path+".type", add)
		inspectPersistableString(step.Name, path+".name", add)
		if step.ToolName != nil {
			inspectPersistableString(string(*step.ToolName), path+".tool_name", add)
		}
		inspectOutputSchemaSafety(step.OutputSchema, path+".output_schema", add)
		inspectInputSafety(step.Input.JSON(), path+".input", add)
	}
}

func inspectOutputSchemaSafety(
	schema contracts.OutputSchema,
	path string,
	add func(ValidationIssueCode, string),
) {
	names := make([]string, 0, len(schema))
	for name := range schema {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		if sensitivePersistableKey(name) {
			add(ValidationIssueSensitiveContentDetected, path+"."+dynamicJSONPathSegment)
		} else {
			inspectPersistableString(name, path+"."+dynamicJSONPathSegment, add)
		}
		inspectPersistableString(string(schema[name].Type), path+"."+dynamicJSONPathSegment+".type", add)
	}
}

func inspectInputSafety(encoded []byte, path string, add func(ValidationIssueCode, string)) {
	if len(encoded) == 0 || !utf8.Valid(encoded) || !json.Valid(encoded) {
		add(ValidationIssueUnsafePersistableContent, path)
		return
	}
	decoder := json.NewDecoder(bytes.NewReader(encoded))
	decoder.UseNumber()
	var value any
	if err := decoder.Decode(&value); err != nil {
		add(ValidationIssueUnsafePersistableContent, path)
		return
	}
	inspectJSONSafety(value, path, add)
}

func inspectJSONSafety(value any, path string, add func(ValidationIssueCode, string)) {
	switch typed := value.(type) {
	case map[string]any:
		names := make([]string, 0, len(typed))
		for name := range typed {
			names = append(names, name)
		}
		sort.Strings(names)
		for _, name := range names {
			childPath := path + "." + dynamicJSONPathSegment
			if sensitivePersistableKey(name) {
				add(ValidationIssueSensitiveContentDetected, childPath)
			} else {
				inspectPersistableString(name, childPath, add)
			}
			inspectJSONSafety(typed[name], childPath, add)
		}
	case []any:
		for index, child := range typed {
			inspectJSONSafety(child, path+"["+jsonIndex(index)+"]", add)
		}
	case string:
		inspectPersistableString(typed, path, add)
	}
}

func inspectPersistableString(value, path string, add func(ValidationIssueCode, string)) {
	if !utf8.ValidString(value) {
		add(ValidationIssueUnsafePersistableContent, path)
		return
	}
	if containsControlCharacter(value) {
		add(ValidationIssueUnsafePersistableContent, path)
	}
	if containsCredentialPattern(value) {
		add(ValidationIssueSensitiveContentDetected, path)
	}
}

func containsControlCharacter(value string) bool {
	for _, character := range value {
		if unicode.IsControl(character) {
			return true
		}
	}
	return false
}

func containsCredentialPattern(value string) bool {
	upper := strings.ToUpper(value)
	return privateKeyMarkerPattern.MatchString(upper) || pgpPrivateKeyPattern.MatchString(upper) ||
		authorizationPattern.MatchString(value) || bearerPattern.MatchString(value) || containsBasicCredential(value)
}

func containsBasicCredential(value string) bool {
	for _, match := range basicPattern.FindAllStringSubmatch(value, -1) {
		if len(match) != 2 {
			continue
		}
		decoded, err := base64.StdEncoding.DecodeString(match[1])
		if err != nil {
			decoded, err = base64.RawStdEncoding.DecodeString(match[1])
		}
		if err == nil && bytes.Contains(decoded, []byte{':'}) {
			return true
		}
	}
	return false
}

func sensitivePersistableKey(key string) bool {
	switch strings.ToLower(key) {
	case "password", "passwd", "secret", "token", "api_key", "apikey", "private_key",
		"client_secret", "authorization":
		return true
	default:
		return false
	}
}

func jsonIndex(index int) string {
	return strconv.Itoa(index)
}
