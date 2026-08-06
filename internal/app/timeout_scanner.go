package app

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/zhaohaip/agentops-go/internal/contracts"
	"github.com/zhaohaip/agentops-go/internal/taskruntime"
)

const maxTimeoutScanInterval = 5 * time.Second

// TimeoutCandidate 是 Scanner 观察到的过期候选版本。
type TimeoutCandidate struct {
	TaskID                   contracts.TaskID
	ObservedExecutionVersion contracts.ExecutionVersion
}

// TimeoutCandidateSource 只负责查询候选，不直接修改状态。
type TimeoutCandidateSource interface {
	ListTimeoutCandidates(context.Context) ([]TimeoutCandidate, error)
}

// TaskExpirer 是 Timeout Scanner 调用的窄化 Runtime Port。
type TaskExpirer interface {
	ExpireTask(context.Context, taskruntime.ExpireTaskRequest) (taskruntime.ExpireTaskResult, error)
}

// TimeoutScanner 按有限间隔提交候选，每个候选由 Runtime 使用独立事务处理。
type TimeoutScanner struct {
	source   TimeoutCandidateSource
	expirer  TaskExpirer
	interval time.Duration
}

// NewTimeoutScanner 创建扫描间隔不超过五秒的 Timeout Scanner。
func NewTimeoutScanner(source TimeoutCandidateSource, expirer TaskExpirer, interval time.Duration) (*TimeoutScanner, error) {
	if source == nil || expirer == nil {
		return nil, errors.New("create Timeout Scanner: dependencies are required")
	}
	if interval <= 0 || interval > maxTimeoutScanInterval {
		return nil, errors.New("create Timeout Scanner: interval must be within five seconds")
	}
	return &TimeoutScanner{source: source, expirer: expirer, interval: interval}, nil
}

// Run 立即扫描一次，之后按固定间隔扫描，直到 Context 结束或遇到系统错误。
func (s *TimeoutScanner) Run(ctx context.Context) error {
	if s == nil {
		return errors.New("run Timeout Scanner: scanner is not initialized")
	}
	if err := s.scan(ctx); err != nil {
		return err
	}
	ticker := time.NewTicker(s.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return context.Cause(ctx)
		case <-ticker.C:
			if err := s.scan(ctx); err != nil {
				return err
			}
		}
	}
}

func (s *TimeoutScanner) scan(ctx context.Context) error {
	candidates, err := s.source.ListTimeoutCandidates(ctx)
	if err != nil {
		return fmt.Errorf("list Timeout candidates: %w", err)
	}
	for _, candidate := range candidates {
		if candidate.TaskID == "" || !candidate.ObservedExecutionVersion.Valid() {
			return errors.New("list Timeout candidates: invalid candidate")
		}
		if _, err := s.expirer.ExpireTask(ctx, taskruntime.ExpireTaskRequest{
			TaskID: candidate.TaskID, ObservedExecutionVersion: candidate.ObservedExecutionVersion,
		}); err != nil {
			return fmt.Errorf("expire Timeout candidate: %w", err)
		}
	}
	return nil
}
