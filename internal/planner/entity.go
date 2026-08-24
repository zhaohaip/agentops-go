// Package planner 定义 Planner 模块拥有的持久化契约。
package planner

import (
	"errors"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/zhaohaip/agentops-go/internal/contracts"
)

var (
	// ErrInvalidPlan 表示传入的持久化 Plan 事实不完整。
	ErrInvalidPlan = errors.New("invalid Plan entity")
	// ErrPersistenceInvariantViolation 表示数据库返回了不可能的 Plan 事实。
	ErrPersistenceInvariantViolation = errors.New("Plan persistence invariant violation")
)

// Entity 是 Task Runtime 结果事务创建后即不可变的安全 Plan 事实。
//
// 字段保持私有，构造后只能读取；模型原始响应、Prompt 和 Provider 元数据不属于该实体。
type Entity struct {
	planID    contracts.PlanID
	runID     contracts.RunID
	goal      string
	createdAt time.Time
}

// NewEntity 校验并创建不可变 Plan Entity。
func NewEntity(planID contracts.PlanID, runID contracts.RunID, goal string, createdAt time.Time) (Entity, error) {
	if strings.TrimSpace(string(planID)) == "" || strings.TrimSpace(string(runID)) == "" ||
		goal == "" || !utf8.ValidString(goal) || createdAt.IsZero() {
		return Entity{}, ErrInvalidPlan
	}
	return Entity{planID: planID, runID: runID, goal: goal, createdAt: createdAt.UTC()}, nil
}

// PlanID 返回 Plan 的稳定标识。
func (e Entity) PlanID() contracts.PlanID { return e.planID }

// RunID 返回拥有该 Plan 的唯一 Run。
func (e Entity) RunID() contracts.RunID { return e.runID }

// Goal 返回已通过 Planner 安全校验的目标。
func (e Entity) Goal() string { return e.goal }

// CreatedAt 返回数据库权威创建时间。
func (e Entity) CreatedAt() time.Time { return e.createdAt }
