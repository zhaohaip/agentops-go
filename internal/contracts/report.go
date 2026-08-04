package contracts

import (
	"context"
	"time"
)

// PendingReportWriter 在调用方已有事务中确保唯一的 Pending Report 占位存在。
type PendingReportWriter interface {
	EnsurePending(
		ctx context.Context,
		tx RuntimeWriteTx,
		request EnsurePendingReportRequest,
	) (EnsurePendingReportResult, error)
}

// EnsurePendingReportRequest 表示创建或幂等确认 Pending Report 的共享请求。
type EnsurePendingReportRequest struct {
	TaskID    TaskID    `json:"task_id"`
	RunID     RunID     `json:"run_id"`
	CreatedAt time.Time `json:"created_at"`
}

// EnsurePendingReportResult 是 EnsurePending 的封闭结果。
type EnsurePendingReportResult interface {
	isEnsurePendingReportResult()
}

// EnsurePendingReportCreated 表示本次调用创建了 Pending Report。
type EnsurePendingReportCreated struct{}

func (EnsurePendingReportCreated) isEnsurePendingReportResult() {}

// EnsurePendingReportExisting 表示合法 Report 已存在且未被重置。
type EnsurePendingReportExisting struct{}

func (EnsurePendingReportExisting) isEnsurePendingReportResult() {}

// ReportProcessingResult 是单次 Report Worker 处理的封闭结果。
type ReportProcessingResult interface {
	isReportProcessingResult()
}

// ReportProcessingCompleted 表示 Report 已成功生成。
type ReportProcessingCompleted struct{}

func (ReportProcessingCompleted) isReportProcessingResult() {}

// ReportProcessingFailed 表示 Report 已确定性失败并完成状态收敛。
type ReportProcessingFailed struct{}

func (ReportProcessingFailed) isReportProcessingResult() {}

// ReportProcessingNoWork 表示当前没有可处理的 Report。
type ReportProcessingNoWork struct{}

func (ReportProcessingNoWork) isReportProcessingResult() {}

// ReportProcessingInterrupted 表示处理因 Runtime 取消而中断。
type ReportProcessingInterrupted struct{}

func (ReportProcessingInterrupted) isReportProcessingResult() {}
