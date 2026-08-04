package migration

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

const (
	metadataTableName = "agentops_schema_migrations"
	rollbackTimeout   = 5 * time.Second
)

const createMetadataTableSQL = `
CREATE TABLE IF NOT EXISTS agentops_schema_migrations (
    version BIGINT PRIMARY KEY CHECK (version > 0),
    name TEXT NOT NULL CHECK (btrim(name) <> ''),
    checksum TEXT NOT NULL CHECK (checksum ~ '^[0-9a-f]{64}$'),
    applied_at TIMESTAMPTZ NOT NULL DEFAULT CURRENT_TIMESTAMP
)`

const selectAppliedMigrationsSQL = `
SELECT version, name, checksum, applied_at
FROM agentops_schema_migrations
ORDER BY version`

const insertAppliedMigrationSQL = `
INSERT INTO agentops_schema_migrations (version, name, checksum)
VALUES ($1, $2, $3)`

// TransactionBeginner 是 Runner 对 PostgreSQL 连接所需的最小能力。
//
// P0-T04 的持锁专用连接将实现该接口；本包不创建或缓存连接。
type TransactionBeginner interface {
	Begin(context.Context) (pgx.Tx, error)
}

// Runner 负责初始化版本元数据并顺序执行尚未应用的 Migration。
type Runner struct {
	beginner   TransactionBeginner
	migrations []preparedMigration
}

// NewRunner 校验并复制 Migration 注册集合。
func NewRunner(beginner TransactionBeginner, migrations []Migration) (*Runner, error) {
	if beginner == nil {
		return nil, errors.New("create migration runner: PostgreSQL connection is required")
	}

	prepared, err := prepareMigrations(migrations)
	if err != nil {
		return nil, fmt.Errorf("create migration runner: %w", err)
	}

	return &Runner{
		beginner:   beginner,
		migrations: prepared,
	}, nil
}

// Migrate 初始化元数据、校验历史，并按版本升序应用所有待执行 Migration。
func (r *Runner) Migrate(ctx context.Context) error {
	if ctx == nil {
		return errors.New("run migrations: context is required")
	}
	if r == nil || r.beginner == nil {
		return errors.New("run migrations: runner is not initialized")
	}

	applied, err := r.initializeAndLoadHistory(ctx)
	if err != nil {
		return err
	}
	if err := validateAppliedHistory(r.migrations, applied); err != nil {
		return fmt.Errorf("run migrations: %w", err)
	}

	for index := len(applied); index < len(r.migrations); index++ {
		if err := r.applyMigration(ctx, r.migrations[index]); err != nil {
			return err
		}
	}

	return nil
}

type appliedMigration struct {
	version   int64
	name      string
	checksum  string
	appliedAt time.Time
}

func (r *Runner) initializeAndLoadHistory(ctx context.Context) ([]appliedMigration, error) {
	var applied []appliedMigration
	err := r.withTransaction(ctx, "initialize migration metadata", func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, createMetadataTableSQL); err != nil {
			return fmt.Errorf("create metadata table %s: %w", metadataTableName, err)
		}

		rows, err := tx.Query(ctx, selectAppliedMigrationsSQL)
		if err != nil {
			return fmt.Errorf("query applied migrations: %w", err)
		}
		defer rows.Close()

		for rows.Next() {
			var current appliedMigration
			if err := rows.Scan(&current.version, &current.name, &current.checksum, &current.appliedAt); err != nil {
				return fmt.Errorf("scan applied migration: %w", err)
			}
			applied = append(applied, current)
		}
		if err := rows.Err(); err != nil {
			return fmt.Errorf("iterate applied migrations: %w", err)
		}

		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("run migrations: %w", err)
	}

	return applied, nil
}

func validateAppliedHistory(registered []preparedMigration, applied []appliedMigration) error {
	registeredByVersion := make(map[int64]int, len(registered))
	for index, current := range registered {
		registeredByVersion[current.version] = index
	}

	for index, current := range applied {
		registeredIndex, known := registeredByVersion[current.version]
		if !known {
			return fmt.Errorf("%w: version %d", ErrUnknownAppliedVersion, current.version)
		}
		if registeredIndex != index {
			return fmt.Errorf(
				"%w: applied version %d is at position %d, expected position %d",
				ErrAppliedHistoryInconsistent,
				current.version,
				index,
				registeredIndex,
			)
		}

		expected := registered[registeredIndex]
		if current.name != expected.name || current.checksum != expected.checksum {
			return fmt.Errorf("%w: version %d", ErrAppliedMigrationMismatch, current.version)
		}
	}

	return nil
}

func (r *Runner) applyMigration(ctx context.Context, current preparedMigration) error {
	err := r.withTransaction(ctx, fmt.Sprintf("apply migration version %d", current.version), func(tx pgx.Tx) error {
		for index, statement := range current.statements {
			if _, err := tx.Exec(ctx, statement); err != nil {
				return fmt.Errorf("execute statement %d: %w", index, err)
			}
		}

		if _, err := tx.Exec(
			ctx,
			insertAppliedMigrationSQL,
			current.version,
			current.name,
			current.checksum,
		); err != nil {
			return fmt.Errorf("record applied version: %w", err)
		}

		return nil
	})
	if err != nil {
		return fmt.Errorf("run migrations: version %d (%s): %w", current.version, current.name, err)
	}

	return nil
}

func (r *Runner) withTransaction(
	ctx context.Context,
	operation string,
	work func(pgx.Tx) error,
) (resultErr error) {
	tx, err := r.beginner.Begin(ctx)
	if err != nil {
		return fmt.Errorf("%s: begin transaction: %w", operation, err)
	}

	committed := false
	defer func() {
		if committed {
			return
		}

		rollbackCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), rollbackTimeout)
		defer cancel()
		rollbackErr := tx.Rollback(rollbackCtx)
		if rollbackErr != nil && !errors.Is(rollbackErr, pgx.ErrTxClosed) {
			resultErr = errors.Join(resultErr, fmt.Errorf("%s: rollback transaction: %w", operation, rollbackErr))
		}
	}()

	if err := work(tx); err != nil {
		return fmt.Errorf("%s: %w", operation, err)
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("%s: commit transaction: %w", operation, err)
	}
	committed = true

	return nil
}
