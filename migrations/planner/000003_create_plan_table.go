// Package planner 声明 Planner 模块拥有的 PostgreSQL Migration。
package planner

import "github.com/zhaohaip/agentops-go/internal/adapter/postgres/migration"

// Migrations 返回 Planner 已发布的完整 Migration 历史。
func Migrations() []migration.Migration {
	return []migration.Migration{{
		Version: 3,
		Name:    "create_plan_table",
		Statements: []string{
			createPlanTableSQL,
			addRunPlanForeignKeySQL,
		},
	}}
}

const createPlanTableSQL = `
CREATE TABLE plan (
    plan_id TEXT PRIMARY KEY CHECK (plan_id <> ''),
    run_id TEXT NOT NULL CHECK (run_id <> ''),
    goal TEXT NOT NULL CHECK (goal <> ''),
    created_at TIMESTAMPTZ NOT NULL,
    CONSTRAINT plan_run_unique UNIQUE (run_id),
    CONSTRAINT plan_run_identity_unique UNIQUE (run_id, plan_id),
    CONSTRAINT plan_run_foreign_key FOREIGN KEY (run_id)
        REFERENCES run (run_id)
)`

// 复合 FK 同时保证 run.plan_id 存在且只能指向该 Run 自己的 Plan。
// DEFERRABLE 允许 Task Runtime 在一个结果事务中创建 Plan 后再冻结 Run 指针。
const addRunPlanForeignKeySQL = `
ALTER TABLE run
ADD CONSTRAINT run_plan_foreign_key
FOREIGN KEY (run_id, plan_id)
REFERENCES plan (run_id, plan_id)
DEFERRABLE INITIALLY DEFERRED`
