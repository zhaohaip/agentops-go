// Package migrations 是所有生产 Migration 的唯一显式装配入口。
package migrations

import "github.com/zhaohaip/agentops-go/internal/adapter/postgres/migration"

// All 返回当前 Runtime 必须认识的完整 Migration 历史。
//
// Phase 0 没有业务 Migration；后续领域模块按 Owner 分包，并在此显式追加。
func All() []migration.Migration {
	return nil
}
