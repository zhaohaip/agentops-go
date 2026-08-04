// Package postgrestest 提供 PostgreSQL 集成测试的隔离环境、Migration 场景和
// Repository 契约测试入口。
//
// 本包只允许被 test/ 下的测试代码使用，不包含生产配置或业务 Schema。
package postgrestest
