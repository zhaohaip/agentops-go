package taskruntime

import (
	"context"
	"time"

	"github.com/zhaohaip/agentops-go/internal/contracts"
)

// ApplyRecoveryFailureRequest 描述 P2 可归属 GENERATE_PLAN 来源的失败终态。
// Step/Tool 来源的真实数据库终态由其 Owner 阶段接入，不在此 Port 伪造。
type ApplyRecoveryFailureRequest struct {
	TaskID                   contracts.TaskID
	ExpectedExecutionVersion contracts.ExecutionVersion
	ExpectedTaskStatus       contracts.TaskStatus
	ExpectedRunStatus        contracts.RunStatus
	ExpectedExecutionStatus  contracts.TaskExecutionStatus
	ErrorCode                contracts.ErrorCode
	TerminationReason        *contracts.TerminationReason
	EndedAt                  time.Time
}

// RecoveryRepository 锁定 Recover 所需三对象，并提交 P2 范围内的确定失败终态。
type RecoveryRepository interface {
	LockRecoveryFacts(context.Context, contracts.RuntimeWriteTx, contracts.TaskID) (TerminationFacts, error)
	ApplyRecoveryFailure(context.Context, contracts.RuntimeWriteTx, ApplyRecoveryFailureRequest) (bool, error)
}
