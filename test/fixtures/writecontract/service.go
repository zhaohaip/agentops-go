// Package writecontract 提供只依赖共享事务 Port 的测试业务模块。
package writecontract

import (
	"context"
	"errors"

	"github.com/zhaohaip/agentops-go/internal/contracts"
)

// Repository 表示在调用方 Runtime 事务中执行一次测试写入的业务出站 Port。
type Repository interface {
	Store(context.Context, contracts.RuntimeWriteTx, string) error
}

// Service 验证应用服务只通过 contracts 组合事务 Owner 和多个 Repository。
type Service struct {
	executor contracts.RuntimeWriteExecutor
	first    Repository
	second   Repository
}

// NewService 创建测试业务服务。
func NewService(
	executor contracts.RuntimeWriteExecutor,
	first Repository,
	second Repository,
) *Service {
	return &Service{executor: executor, first: first, second: second}
}

// StorePair 在同一个不透明事务令牌中依次调用两个 Repository。
func (s *Service) StorePair(ctx context.Context, firstValue string, secondValue string) error {
	if s == nil || s.executor == nil || s.first == nil || s.second == nil {
		return errors.New("store pair: service dependencies are required")
	}
	return s.executor.Execute(ctx, func(ctx context.Context, tx contracts.RuntimeWriteTx) error {
		if err := s.first.Store(ctx, tx, firstValue); err != nil {
			return err
		}
		return s.second.Store(ctx, tx, secondValue)
	})
}
