package planner

import (
	"bytes"
	"encoding/json"
	"errors"

	"github.com/zhaohaip/agentops-go/internal/contracts"
)

const (
	// PlanSchemaVersionV1 是当前唯一支持的 AgentOps Plan 线协议版本。
	PlanSchemaVersionV1 uint32 = 1

	maxModelResponseBytes   = 1024 * 1024
	maxPlanDraftBytes       = 256 * 1024
	maxPlanSteps            = 20
	maxStepNameBytes        = 128
	maxGoalBytes            = 2 * 1024
	maxStepInputBytes       = 32 * 1024
	maxOutputFields         = 32
	maxOutputFieldNameBytes = 64
	maxJSONDepth            = 16
	maxObjectFields         = 64
)

// StepInput 是已通过严格 JSON 结构解析的 Step input object。
type StepInput struct {
	encoded json.RawMessage
}

// JSON 返回输入 JSON 的独立副本。
func (i StepInput) JSON() json.RawMessage {
	return append(json.RawMessage(nil), i.encoded...)
}

// MarshalJSON 实现 Plan V1 线协议编码。
func (i StepInput) MarshalJSON() ([]byte, error) {
	if len(i.encoded) == 0 || !json.Valid(i.encoded) {
		return nil, errors.New("encode Step input: valid JSON object is required")
	}
	return append([]byte(nil), i.encoded...), nil
}

func newStepInput(encoded []byte) (StepInput, error) {
	trimmed := bytes.TrimSpace(encoded)
	if len(trimmed) == 0 || trimmed[0] != '{' || !json.Valid(trimmed) {
		return StepInput{}, errors.New("Step input must be a JSON object")
	}
	var compact bytes.Buffer
	if err := json.Compact(&compact, trimmed); err != nil {
		return StepInput{}, err
	}
	return StepInput{encoded: append(json.RawMessage(nil), compact.Bytes()...)}, nil
}

// PlanDraft 是尚未持久化、未分配 Plan ID 的 V1 Plan 候选。
type PlanDraft struct {
	Goal  string      `json:"goal"`
	Steps []StepDraft `json:"steps"`
}

// StepDraft 是尚未持久化、未分配 Step ID 的 V1 顺序 Step 候选。
type StepDraft struct {
	Sequence     uint32                 `json:"sequence"`
	Type         contracts.StepType     `json:"type"`
	Name         string                 `json:"name"`
	Input        StepInput              `json:"input"`
	OutputSchema contracts.OutputSchema `json:"output_schema"`
	ToolName     *contracts.ToolName    `json:"tool_name,omitempty"`
}
