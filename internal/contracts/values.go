package contracts

import (
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// TaskID 表示 Task 标识。
type TaskID string

// RunID 表示 Run 标识。
type RunID string

// PlanID 表示 Plan 标识。
type PlanID string

// StepID 表示 Step 标识。
type StepID string

// ToolExecutionID 表示 ToolExecution 标识。
type ToolExecutionID string

// ApprovalID 表示 Approval 标识。
type ApprovalID string

// CheckpointID 表示 Checkpoint 标识。
type CheckpointID string

// ReportID 表示 Report 标识。
type ReportID string

// AgentID 表示静态 Agent 标识。
type AgentID string

// WorkerID 表示一次 Runtime Instance 的 Worker 标识。
type WorkerID string

// ToolName 表示 Tool 的稳定名称。
type ToolName string

// ResourceVersion 表示 Kubernetes resourceVersion。
type ResourceVersion string

// ExecutionVersion 表示持久化 TaskExecution 版本。
type ExecutionVersion int64

// Valid 报告 ExecutionVersion 是否是合法持久化值。
func (v ExecutionVersion) Valid() bool {
	return v > 0
}

// ExecutionConfigHash 表示完整 ExecutionConfigV1 的 SHA-256 摘要。
type ExecutionConfigHash string

// Valid 报告 ExecutionConfigHash 是否为 64 位小写十六进制。
func (h ExecutionConfigHash) Valid() bool {
	return validSHA256(string(h))
}

// CatalogSnapshotHash 表示 Planning Tool Catalog 的独立 SHA-256 摘要。
type CatalogSnapshotHash string

// Valid 报告 CatalogSnapshotHash 是否为 64 位小写十六进制。
func (h CatalogSnapshotHash) Valid() bool {
	return validSHA256(string(h))
}

// FrozenInputHash 表示 FrozenApprovedToolInputV1 的 SHA-256 摘要。
type FrozenInputHash string

// Valid 报告 FrozenInputHash 是否为 64 位小写十六进制。
func (h FrozenInputHash) Valid() bool {
	return validSHA256(string(h))
}

func validSHA256(value string) bool {
	if len(value) != 64 {
		return false
	}
	for _, character := range value {
		if (character < '0' || character > '9') && (character < 'a' || character > 'f') {
			return false
		}
	}
	return true
}

// CanonicalDecimalV1 表示不使用指数形式的规范有限十进制值。
type CanonicalDecimalV1 struct {
	coefficient int64
	scale       uint32
}

// NewCanonicalDecimalV1 构造并去除小数尾随零。
func NewCanonicalDecimalV1(coefficient int64, scale uint32) CanonicalDecimalV1 {
	if coefficient == 0 {
		return CanonicalDecimalV1{}
	}
	for scale > 0 && coefficient%10 == 0 {
		coefficient /= 10
		scale--
	}
	return CanonicalDecimalV1{
		coefficient: coefficient,
		scale:       scale,
	}
}

// Coefficient 返回规范十进制的系数。
func (d CanonicalDecimalV1) Coefficient() int64 {
	return d.coefficient
}

// Scale 返回规范十进制的非负小数位数。
func (d CanonicalDecimalV1) Scale() uint32 {
	return d.scale
}

// String 返回无指数、无多余尾随零的十进制表示。
func (d CanonicalDecimalV1) String() string {
	negative := d.coefficient < 0
	digits := strconv.FormatInt(d.coefficient, 10)
	if negative {
		digits = strings.TrimPrefix(digits, "-")
	}

	scale := int(d.scale)
	var value string
	switch {
	case scale == 0:
		value = digits
	case len(digits) > scale:
		split := len(digits) - scale
		value = digits[:split] + "." + digits[split:]
	default:
		value = "0." + strings.Repeat("0", scale-len(digits)) + digits
	}
	if negative {
		return "-" + value
	}
	return value
}

// MarshalJSON 将值编码为 JSON number。
func (d CanonicalDecimalV1) MarshalJSON() ([]byte, error) {
	return []byte(d.String()), nil
}

// UnmarshalJSON 只接受无指数且已经规范化的 JSON number。
func (d *CanonicalDecimalV1) UnmarshalJSON(data []byte) error {
	if d == nil {
		return errors.New("decode canonical decimal: target is nil")
	}

	value := string(data)
	coefficient, scale, err := parseCanonicalDecimal(value)
	if err != nil {
		return err
	}
	parsed := NewCanonicalDecimalV1(coefficient, scale)
	if parsed.String() != value {
		return errors.New("decode canonical decimal: value is not canonical")
	}
	*d = parsed
	return nil
}

func parseCanonicalDecimal(value string) (int64, uint32, error) {
	if value == "" || strings.ContainsAny(value, "eE+") {
		return 0, 0, errors.New("decode canonical decimal: invalid JSON number")
	}

	negative := strings.HasPrefix(value, "-")
	unsigned := strings.TrimPrefix(value, "-")
	if unsigned == "" || strings.Count(unsigned, ".") > 1 {
		return 0, 0, errors.New("decode canonical decimal: invalid JSON number")
	}

	parts := strings.SplitN(unsigned, ".", 2)
	integerPart := parts[0]
	if integerPart == "" || (len(integerPart) > 1 && integerPart[0] == '0') {
		return 0, 0, errors.New("decode canonical decimal: invalid integer part")
	}
	fractionPart := ""
	if len(parts) == 2 {
		fractionPart = parts[1]
		if fractionPart == "" {
			return 0, 0, errors.New("decode canonical decimal: invalid fraction")
		}
	}
	for _, character := range integerPart + fractionPart {
		if character < '0' || character > '9' {
			return 0, 0, errors.New("decode canonical decimal: invalid digit")
		}
	}

	digits := integerPart + fractionPart
	if negative {
		digits = "-" + digits
	}
	coefficient, err := strconv.ParseInt(digits, 10, 64)
	if err != nil {
		return 0, 0, fmt.Errorf("decode canonical decimal: coefficient out of range: %w", err)
	}
	if negative && coefficient == 0 {
		return 0, 0, errors.New("decode canonical decimal: negative zero is not allowed")
	}
	return coefficient, uint32(len(fractionPart)), nil
}

// ResolvedToolInput 表示已经解析并校验的规范 Tool 输入。
type ResolvedToolInput json.RawMessage

// MarshalJSON 输出规范 Tool 输入。
func (v ResolvedToolInput) MarshalJSON() ([]byte, error) {
	return marshalJSONValue(v)
}

// UnmarshalJSON 读取一个合法 JSON 值。
func (v *ResolvedToolInput) UnmarshalJSON(data []byte) error {
	return unmarshalJSONValue(data, (*json.RawMessage)(v))
}

// FrozenToolInput 表示审批冻结的规范 Tool 输入。
type FrozenToolInput json.RawMessage

// MarshalJSON 输出冻结 Tool 输入。
func (v FrozenToolInput) MarshalJSON() ([]byte, error) {
	return marshalJSONValue(v)
}

// UnmarshalJSON 读取一个合法 JSON 值。
func (v *FrozenToolInput) UnmarshalJSON(data []byte) error {
	return unmarshalJSONValue(data, (*json.RawMessage)(v))
}

// ObservedValues 表示审批准备时允许保留的旧值。
type ObservedValues json.RawMessage

// MarshalJSON 输出审批旧值。
func (v ObservedValues) MarshalJSON() ([]byte, error) {
	return marshalJSONValue(v)
}

// UnmarshalJSON 读取一个合法 JSON 值。
func (v *ObservedValues) UnmarshalJSON(data []byte) error {
	return unmarshalJSONValue(data, (*json.RawMessage)(v))
}

// SafeToolOutput 表示经过白名单、限长和脱敏的 Tool 输出。
type SafeToolOutput json.RawMessage

// MarshalJSON 输出安全 Tool 结果。
func (v SafeToolOutput) MarshalJSON() ([]byte, error) {
	return marshalJSONValue(v)
}

// UnmarshalJSON 读取一个合法 JSON 值。
func (v *SafeToolOutput) UnmarshalJSON(data []byte) error {
	return unmarshalJSONValue(data, (*json.RawMessage)(v))
}

func marshalJSONValue(value []byte) ([]byte, error) {
	if !json.Valid(value) {
		return nil, errors.New("encode JSON value: invalid value")
	}
	return value, nil
}

func unmarshalJSONValue(data []byte, target *json.RawMessage) error {
	if target == nil {
		return errors.New("decode JSON value: target is nil")
	}
	if !json.Valid(data) || string(data) == "null" {
		return errors.New("decode JSON value: non-null JSON value is required")
	}
	*target = append((*target)[:0], data...)
	return nil
}
