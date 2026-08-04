// Package migration 提供 AgentOps PostgreSQL Schema Migration 基础设施。
//
// 本包只管理版本元数据与顺序执行，不拥有业务表，也不负责 advisory lock、
// 数据库连接生命周期或 Runtime 写事务。
package migration
