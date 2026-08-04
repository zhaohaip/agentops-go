# PostgreSQL 集成测试基础

## 运行

复制 `.env.test.example` 为 `.env.test`，配置 `AGENTOPS_TEST_POSTGRES_DSN`，然后执行：

```bash
make test-integration-postgres
make test-integration-postgres TEST_FLAGS=-race
make test-phase0
```

`.env.test` 已被 Git 忽略；该 DSN 是测试管理身份。testkit 会在每个随机 Database
中创建并清理独立的 Migration、Runtime 写和 Runtime 读角色，生产 Runtime 不使用
该管理身份。

## 隔离选择

- 普通 Migration 和只涉及表数据的测试使用 `postgrestest.NewSchema(t)`；每个测试得到
  随机 `search_path`，可以并行。
- advisory lock 等 Database 级行为使用 `postgrestest.NewDatabase(t)`；测试账号需要
  `CREATEDB` 权限。
- 两种环境都通过 `t.Cleanup` 自动删除；连接、Runtime 等资源应在创建环境后注册
  Cleanup，使其先于 Schema/Database 释放。

## Migration 测试

使用 `postgrestest.NewMigrationHarness(connection)`：

- `Apply(ctx, nil)` 验证空 Migration；
- `Apply(ctx, previous)` 后再 `Apply(ctx, current)` 验证增量升级；
- 重复 `Apply(ctx, current)` 验证幂等；
- 直接检查返回错误可验证失败回滚和 PostgreSQL SQL 错误链；
- `AppliedVersions` 用于断言元数据版本。

## Repository 契约测试

模块提供自己的测试 Migration 和互不依赖的 `RepositoryCase`，再调用：

```go
postgrestest.RunRepositoryContract(t, postgrestest.RepositoryContract{
    Name:       "module repository",
    Migrations: moduleTestMigrations,
    Cases: []postgrestest.RepositoryCase{
        {Name: "conditional update", Run: verifyConditionalUpdate},
        {Name: "unique constraint", Run: verifyUniqueConstraint},
    },
})
```

每个 Case 在独立 Database 中并行执行。`RepositoryEnvironment.Runtime` 提供真实
Write Executor、只读池和 Database Clock；并发场景可使用 `ExecuteConcurrent`。

`test/fixtures/` 只允许放稳定测试数据和测试专用 Migration，不得预置业务 Schema。
