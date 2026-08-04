# PostgreSQL Migrations

生产 Migration 采用显式装配，不使用 `init()` 或运行时目录扫描：

```text
migrations/
├── registry.go
├── taskruntime/
│   └── 000001_create_task_runtime_tables.go
├── checkpoint/
├── planner/
├── stepexecutor/
├── toolframework/
├── approval/
└── report/
```

Phase 0 的注册集合为空，只由 Framework 创建
`agentops_schema_migrations` 元数据表。后续模块在自己的 Owner 目录声明
`migration.Migration`，再由 `registry.go` 显式汇总完整历史。

规则：

- `Version` 为全局唯一正整数；允许预留版本区间，但发布后不得修改或复用。
- `Name` 和 `Statements` 发布后不可变，Framework 会持久化内容摘要并在启动时复核。
- 一个 Migration 的 Statements 按声明顺序在同一事务中执行；不同版本使用不同事务。
- 不提供 down migration。失败版本修复后可重新启动，但已经成功记录的版本不得改写。
