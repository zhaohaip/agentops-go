// Package checkpoint 声明 Checkpoint 模块拥有的 PostgreSQL Migration。
package checkpoint

import "github.com/zhaohaip/agentops-go/internal/adapter/postgres/migration"

// Migrations 返回 Checkpoint 已发布的完整 Migration 历史。
func Migrations() []migration.Migration {
	return []migration.Migration{{
		Version: 2,
		Name:    "create_checkpoint_table",
		Statements: []string{
			createCheckpointTableSQL,
			createCheckpointLatestIndexSQL,
			createCheckpointSourceIndexSQL,
		},
	}}
}

const createCheckpointTableSQL = `
CREATE TABLE checkpoint (
    checkpoint_id TEXT PRIMARY KEY CHECK (checkpoint_id <> ''),
    task_id TEXT NOT NULL CHECK (task_id <> ''),
    run_id TEXT NOT NULL CHECK (run_id <> ''),
    execution_version BIGINT NOT NULL CHECK (execution_version > 0),
    checkpoint_sequence BIGINT NOT NULL CHECK (checkpoint_sequence > 0),
    runtime_context JSONB NOT NULL CHECK (jsonb_typeof(runtime_context) = 'object'),
    execution_config_hash TEXT NOT NULL CHECK (execution_config_hash ~ '^[0-9a-f]{64}$'),
    source_execution_version BIGINT CHECK (source_execution_version > 0),
    source_checkpoint_id TEXT,
    created_at TIMESTAMPTZ NOT NULL,
    CONSTRAINT checkpoint_run_sequence_unique UNIQUE (run_id, checkpoint_sequence),
    CONSTRAINT checkpoint_run_foreign_key FOREIGN KEY (task_id, run_id)
        REFERENCES run (task_id, run_id),
    CONSTRAINT checkpoint_execution_foreign_key FOREIGN KEY (task_id, execution_version)
        REFERENCES task_execution (task_id, execution_version),
    CONSTRAINT checkpoint_source_foreign_key FOREIGN KEY (source_checkpoint_id)
        REFERENCES checkpoint (checkpoint_id),
    CONSTRAINT checkpoint_source_pair_check CHECK (
        (source_execution_version IS NULL) = (source_checkpoint_id IS NULL)
    ),
    CONSTRAINT checkpoint_source_version_check CHECK (
        source_execution_version IS NULL OR source_execution_version < execution_version
    )
)`

const createCheckpointLatestIndexSQL = `
CREATE INDEX checkpoint_latest_execution_index
ON checkpoint (task_id, run_id, execution_version, checkpoint_sequence DESC)`

const createCheckpointSourceIndexSQL = `
CREATE INDEX checkpoint_source_checkpoint_index
ON checkpoint (source_checkpoint_id)
WHERE source_checkpoint_id IS NOT NULL`
