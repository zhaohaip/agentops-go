// Package stepexecutor 定义 Step Executor 模块拥有的持久化契约。
package stepexecutor

import (
	"bytes"
	"encoding/json"
	"errors"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/zhaohaip/agentops-go/internal/contracts"
)

var (
	// ErrInvalidStep 表示传入的持久化 Step 事实不完整或自相矛盾。
	ErrInvalidStep = errors.New("invalid Step entity")
	// ErrPersistenceInvariantViolation 表示数据库返回了不可能的 Step 事实。
	ErrPersistenceInvariantViolation = errors.New("Step persistence invariant violation")
)

// EntityParams 是构造 Step 持久化实体所需的完整事实。
type EntityParams struct {
	StepID       contracts.StepID
	RunID        contracts.RunID
	PlanID       contracts.PlanID
	Sequence     uint32
	Type         contracts.StepType
	Name         string
	Input        json.RawMessage
	OutputSchema contracts.OutputSchema
	Output       json.RawMessage
	Status       contracts.StepStatus
	ToolName     contracts.ToolName
	ErrorCode    *contracts.ErrorCode
	StartedAt    *time.Time
	EndedAt      *time.Time
}

// Entity 是 Step Executor 模块拥有的持久化 Step 事实。
//
// 字段保持私有，JSON 与可空字段通过副本读出，避免调用方绕过 Repository 更新规则。
type Entity struct {
	stepID       contracts.StepID
	runID        contracts.RunID
	planID       contracts.PlanID
	sequence     uint32
	stepType     contracts.StepType
	name         string
	input        json.RawMessage
	outputSchema contracts.OutputSchema
	output       json.RawMessage
	status       contracts.StepStatus
	toolName     contracts.ToolName
	errorCode    *contracts.ErrorCode
	startedAt    *time.Time
	endedAt      *time.Time
}

// NewEntity 校验并创建 Step Entity。
func NewEntity(params EntityParams) (Entity, error) {
	if strings.TrimSpace(string(params.StepID)) == "" || strings.TrimSpace(string(params.RunID)) == "" ||
		strings.TrimSpace(string(params.PlanID)) == "" || params.Sequence == 0 || !params.Type.Valid() ||
		params.Name == "" || !utf8.ValidString(params.Name) || !params.Status.Valid() ||
		!validJSONObject(params.Input) || !validOutputSchema(params.OutputSchema) ||
		(params.Output != nil && !validJSONObject(params.Output)) || !validToolName(params.Type, params.ToolName) ||
		!validStepState(params) {
		return Entity{}, ErrInvalidStep
	}

	return Entity{
		stepID: params.StepID, runID: params.RunID, planID: params.PlanID,
		sequence: params.Sequence, stepType: params.Type, name: params.Name,
		input: cloneJSON(params.Input), outputSchema: cloneOutputSchema(params.OutputSchema),
		output: cloneJSON(params.Output), status: params.Status, toolName: params.ToolName,
		errorCode: clonePointer(params.ErrorCode), startedAt: cloneTime(params.StartedAt), endedAt: cloneTime(params.EndedAt),
	}, nil
}

// StepID 返回 Step 的稳定标识。
func (e Entity) StepID() contracts.StepID { return e.stepID }

// RunID 返回 Step 所属 Run。
func (e Entity) RunID() contracts.RunID { return e.runID }

// PlanID 返回 Step 所属的唯一 Plan。
func (e Entity) PlanID() contracts.PlanID { return e.planID }

// Sequence 返回从 1 开始的顺序号。
func (e Entity) Sequence() uint32 { return e.sequence }

// Type 返回 Step 类型。
func (e Entity) Type() contracts.StepType { return e.stepType }

// Name 返回 Planner 已校验的 Step 名称。
func (e Entity) Name() string { return e.name }

// Input 返回原始持久化输入的独立副本。
func (e Entity) Input() json.RawMessage { return cloneJSON(e.input) }

// OutputSchema 返回输出契约的独立副本。
func (e Entity) OutputSchema() contracts.OutputSchema { return cloneOutputSchema(e.outputSchema) }

// Output 返回安全持久化输出的独立副本；尚无输出时为 nil。
func (e Entity) Output() json.RawMessage { return cloneJSON(e.output) }

// Status 返回 Step 当前持久化状态。
func (e Entity) Status() contracts.StepStatus { return e.status }

// ToolName 返回 ToolCall 使用的 Tool；非 ToolCall 返回空值。
func (e Entity) ToolName() contracts.ToolName { return e.toolName }

// ErrorCode 返回失败原因的独立副本。
func (e Entity) ErrorCode() *contracts.ErrorCode { return clonePointer(e.errorCode) }

// StartedAt 返回开始时间的独立副本。
func (e Entity) StartedAt() *time.Time { return cloneTime(e.startedAt) }

// EndedAt 返回结束时间的独立副本。
func (e Entity) EndedAt() *time.Time { return cloneTime(e.endedAt) }

func validToolName(stepType contracts.StepType, toolName contracts.ToolName) bool {
	if stepType == contracts.StepTypeToolCall {
		return strings.TrimSpace(string(toolName)) != ""
	}
	return toolName == ""
}

func validStepState(params EntityParams) bool {
	if params.ErrorCode != nil && !params.ErrorCode.Valid() {
		return false
	}
	if params.StartedAt != nil && params.EndedAt != nil && params.EndedAt.Before(*params.StartedAt) {
		return false
	}
	switch params.Status {
	case contracts.StepStatusPending:
		return params.Output == nil && params.ErrorCode == nil && params.StartedAt == nil && params.EndedAt == nil
	case contracts.StepStatusRunning, contracts.StepStatusWaitingApproval:
		return params.Output == nil && params.ErrorCode == nil && params.StartedAt != nil && params.EndedAt == nil
	case contracts.StepStatusCompleted:
		return params.Output != nil && params.ErrorCode == nil && params.StartedAt != nil && params.EndedAt != nil
	case contracts.StepStatusFailed:
		return params.Output == nil && params.ErrorCode != nil && params.EndedAt != nil
	default:
		return false
	}
}

func validJSONObject(value json.RawMessage) bool {
	trimmed := bytes.TrimSpace(value)
	return len(trimmed) > 1 && trimmed[0] == '{' && json.Valid(trimmed)
}

func validOutputSchema(schema contracts.OutputSchema) bool {
	if len(schema) == 0 {
		return false
	}
	for name, field := range schema {
		if !validOutputFieldName(name) || !field.Type.Valid() {
			return false
		}
	}
	return true
}

func validOutputFieldName(name string) bool {
	if name == "" {
		return false
	}
	for index, character := range []byte(name) {
		if (character >= 'A' && character <= 'Z') || (character >= 'a' && character <= 'z') || character == '_' {
			continue
		}
		if index > 0 && character >= '0' && character <= '9' {
			continue
		}
		return false
	}
	return true
}

func cloneJSON(value json.RawMessage) json.RawMessage {
	if value == nil {
		return nil
	}
	return append(json.RawMessage(nil), value...)
}

func cloneOutputSchema(value contracts.OutputSchema) contracts.OutputSchema {
	if value == nil {
		return nil
	}
	copyValue := make(contracts.OutputSchema, len(value))
	for name, field := range value {
		copyValue[name] = field
	}
	return copyValue
}

func clonePointer[T any](value *T) *T {
	if value == nil {
		return nil
	}
	copyValue := *value
	return &copyValue
}

func cloneTime(value *time.Time) *time.Time {
	if value == nil {
		return nil
	}
	utc := value.UTC()
	return &utc
}
