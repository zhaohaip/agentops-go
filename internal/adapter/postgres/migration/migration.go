package migration

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"
	"strings"
)

var (
	// ErrInvalidMigration 表示注册的 Migration 定义不合法。
	ErrInvalidMigration = errors.New("invalid migration")
	// ErrUnknownAppliedVersion 表示数据库记录了当前程序不认识的版本。
	ErrUnknownAppliedVersion = errors.New("unknown applied migration version")
	// ErrAppliedMigrationMismatch 表示已执行版本的名称或内容发生漂移。
	ErrAppliedMigrationMismatch = errors.New("applied migration mismatch")
	// ErrAppliedHistoryInconsistent 表示已执行版本不是当前注册集合的严格前缀。
	ErrAppliedHistoryInconsistent = errors.New("applied migration history inconsistent")
)

// Migration 定义一个版本化且必须在单独事务中执行的 Schema 变更。
//
// Statements 按声明顺序执行；Version 和已执行内容一旦发布便不得修改或复用。
type Migration struct {
	Version    int64
	Name       string
	Statements []string
}

type preparedMigration struct {
	version    int64
	name       string
	statements []string
	checksum   string
}

func prepareMigrations(migrations []Migration) ([]preparedMigration, error) {
	prepared := make([]preparedMigration, 0, len(migrations))
	for index, current := range migrations {
		if current.Version <= 0 {
			return nil, fmt.Errorf("%w: migration at index %d has non-positive version", ErrInvalidMigration, index)
		}
		if current.Name == "" || strings.TrimSpace(current.Name) != current.Name {
			return nil, fmt.Errorf("%w: migration version %d has invalid name", ErrInvalidMigration, current.Version)
		}
		if len(current.Statements) == 0 {
			return nil, fmt.Errorf("%w: migration version %d has no statements", ErrInvalidMigration, current.Version)
		}

		statements := append([]string(nil), current.Statements...)
		for statementIndex, statement := range statements {
			if strings.TrimSpace(statement) == "" {
				return nil, fmt.Errorf(
					"%w: migration version %d has empty statement at index %d",
					ErrInvalidMigration,
					current.Version,
					statementIndex,
				)
			}
		}

		prepared = append(prepared, preparedMigration{
			version:    current.Version,
			name:       current.Name,
			statements: statements,
			checksum:   migrationChecksum(statements),
		})
	}

	sort.Slice(prepared, func(left, right int) bool {
		return prepared[left].version < prepared[right].version
	})
	for index := 1; index < len(prepared); index++ {
		previous := prepared[index-1]
		current := prepared[index]
		if previous.version == current.version {
			return nil, fmt.Errorf("%w: duplicate version %d", ErrInvalidMigration, current.version)
		}
	}

	return prepared, nil
}

func migrationChecksum(statements []string) string {
	var payload bytes.Buffer
	var size [8]byte
	binary.BigEndian.PutUint64(size[:], uint64(len(statements)))
	payload.Write(size[:])
	for _, statement := range statements {
		binary.BigEndian.PutUint64(size[:], uint64(len(statement)))
		payload.Write(size[:])
		payload.WriteString(statement)
	}

	sum := sha256.Sum256(payload.Bytes())
	return hex.EncodeToString(sum[:])
}
