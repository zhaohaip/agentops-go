package contracts

import "context"

// RuntimeWriteTx 表示由 Runtime Write Executor 持有的不透明 AgentOps 短事务令牌。
//
// 该令牌不暴露数据库、提交或回滚能力；Repository 只能在当前 Execute 回调中使用，
// 不得缓存或跨调用保存。
type RuntimeWriteTx interface {
	AgentOpsRuntimeWriteTx()
}

// RuntimeWriteExecutor 串行执行一个由调用方拥有业务步骤的 Runtime 短事务。
//
// Executor 独占事务的创建、提交和回滚；work 只通过 RuntimeWriteTx 把同一事务
// 传给所需 Repository Port。
type RuntimeWriteExecutor interface {
	Execute(
		ctx context.Context,
		work func(context.Context, RuntimeWriteTx) error,
	) error
}
