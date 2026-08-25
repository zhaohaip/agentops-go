package planner

import (
	"sync"

	"github.com/zhaohaip/agentops-go/internal/contracts"
)

// SingleRepairGate 将一次 GeneratePlan 的 Repair 准入封闭为零次或一次。
// 它不调用模型；P3-T06 的 Application Service 只能在成功取得 Prompt 后发起 REPAIR 调用。
type SingleRepairGate struct {
	mu        sync.Mutex
	attempted bool
	builder   PromptBuilder
}

// RepairDecision 是候选校验后与一次 Repair 策略相关的封闭分支。
type RepairDecision string

const (
	RepairDecisionAccepted             RepairDecision = "Accepted"
	RepairDecisionRequired             RepairDecision = "RepairRequired"
	RepairDecisionPlanValidationFailed RepairDecision = "PlanValidationFailed"
)

func NewSingleRepairGate(builder PromptBuilder) *SingleRepairGate {
	return &SingleRepairGate{builder: builder}
}

// Decide 将候选问题映射为接受、唯一 Repair 或 Repair 后校验失败。
// P3-T06 只需把最后一支携带的稳定 issue 映射到 REPAIR_EXHAUSTED cause。
func (gate *SingleRepairGate) Decide(issues []ValidationIssue) RepairDecision {
	if len(issues) == 0 {
		return RepairDecisionAccepted
	}
	if gate == nil {
		return RepairDecisionPlanValidationFailed
	}
	gate.mu.Lock()
	attempted := gate.attempted
	gate.mu.Unlock()
	if attempted {
		return RepairDecisionPlanValidationFailed
	}
	return RepairDecisionRequired
}

// Build 唯一一次取得 Repair Prompt；准入在构造前即被消费，构造失败也不得再次尝试。
func (gate *SingleRepairGate) Build(request RepairPromptRequest) (
	[]contracts.ModelMessage,
	[]RepairIssue,
	error,
) {
	if gate == nil {
		return nil, nil, &PromptError{Code: PromptErrorRepairExhausted}
	}
	gate.mu.Lock()
	if gate.attempted {
		gate.mu.Unlock()
		return nil, nil, &PromptError{Code: PromptErrorRepairExhausted}
	}
	gate.attempted = true
	gate.mu.Unlock()
	return gate.builder.BuildRepair(request)
}

// Attempted 报告 Repair 准入是否已消费。
func (gate *SingleRepairGate) Attempted() bool {
	if gate == nil {
		return false
	}
	gate.mu.Lock()
	defer gate.mu.Unlock()
	return gate.attempted
}
