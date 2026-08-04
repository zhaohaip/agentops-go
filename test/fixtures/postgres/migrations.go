// Package postgres 提供稳定、无业务语义的 PostgreSQL 测试 Fixture。
package postgres

import "github.com/zhaohaip/agentops-go/internal/adapter/postgres/migration"

// InitialMigrations 返回 Repository 契约探针的初始 Migration。
func InitialMigrations() []migration.Migration {
	return []migration.Migration{
		{
			Version: 1,
			Name:    "create_repository_contract_probe",
			Statements: []string{`
CREATE TABLE repository_contract_probe (
    id BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    natural_key TEXT NOT NULL UNIQUE,
    value TEXT NOT NULL,
    version BIGINT NOT NULL DEFAULT 1,
    claimed BOOLEAN NOT NULL DEFAULT FALSE
)`},
		},
	}
}

// CurrentMigrations 返回包含增量版本的完整探针 Migration 集。
func CurrentMigrations() []migration.Migration {
	definitions := InitialMigrations()
	return append(definitions, migration.Migration{
		Version:    2,
		Name:       "add_repository_contract_note",
		Statements: []string{"ALTER TABLE repository_contract_probe ADD COLUMN note TEXT NOT NULL DEFAULT ''"},
	})
}
