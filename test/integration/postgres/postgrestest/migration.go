package postgrestest

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/zhaohaip/agentops-go/internal/adapter/postgres/migration"
)

// MigrationHarness 在调用方提供的隔离连接上验证 Migration 场景。
type MigrationHarness struct {
	connection *pgx.Conn
}

// NewMigrationHarness 创建 Migration 测试入口。
func NewMigrationHarness(connection *pgx.Conn) *MigrationHarness {
	return &MigrationHarness{connection: connection}
}

// Apply 应用指定 Migration 集，并原样保留可供 errors.Is/As 判断的底层错误。
//
// 调用方可先 Apply 前序集合，再 Apply 完整集合，以验证增量升级；重复调用同一集合
// 可验证幂等性，传入 nil 可验证空 Migration。
func (h *MigrationHarness) Apply(ctx context.Context, definitions []migration.Migration) error {
	runner, err := migration.NewRunner(h.connection, definitions)
	if err != nil {
		return fmt.Errorf("create migration test runner: %w", err)
	}
	if err := runner.Migrate(ctx); err != nil {
		return fmt.Errorf("apply migration test set: %w", err)
	}
	return nil
}

// AppliedVersions 返回元数据表记录的版本顺序。
func (h *MigrationHarness) AppliedVersions(ctx context.Context) ([]int64, error) {
	rows, err := h.connection.Query(ctx, "SELECT version FROM agentops_schema_migrations ORDER BY version")
	if err != nil {
		return nil, fmt.Errorf("query applied migration versions: %w", err)
	}
	defer rows.Close()

	var versions []int64
	for rows.Next() {
		var version int64
		if err := rows.Scan(&version); err != nil {
			return nil, fmt.Errorf("scan applied migration version: %w", err)
		}
		versions = append(versions, version)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate applied migration versions: %w", err)
	}
	return versions, nil
}
