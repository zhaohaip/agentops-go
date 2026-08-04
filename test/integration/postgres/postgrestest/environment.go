package postgrestest

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/zhaohaip/agentops-go/internal/config/infra"
)

const (
	// DSNEnvironment 是 PostgreSQL 集成测试唯一读取的连接配置环境变量。
	DSNEnvironment = "AGENTOPS_TEST_POSTGRES_DSN"
	cleanupTimeout = 5 * time.Second
)

// Schema 表示一个测试独占并自动清理的 PostgreSQL Schema。
type Schema struct {
	Name string
	DSN  string

	admin     *pgx.Conn
	closeOnce sync.Once
	closeErr  error
}

// NewSchema 在环境变量指定的 Database 中创建随机 Schema，并注册自动清理。
func NewSchema(t testing.TB) *Schema {
	t.Helper()

	admin := Connect(t, BaseDSN(t))
	name := uniqueName(t, "agentops_test_schema")
	if _, err := admin.Exec(context.Background(), "CREATE SCHEMA "+pgx.Identifier{name}.Sanitize()); err != nil {
		t.Fatalf("create isolated PostgreSQL schema: %v", err)
	}

	parsed := parseURL(t, BaseDSN(t))
	query := parsed.Query()
	query.Set("search_path", name)
	parsed.RawQuery = query.Encode()

	schema := &Schema{Name: name, DSN: parsed.String(), admin: admin}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), cleanupTimeout)
		defer cancel()
		if err := schema.Close(ctx); err != nil {
			t.Errorf("clean isolated PostgreSQL schema: %v", err)
		}
	})
	return schema
}

// Close 删除测试 Schema；重复调用是安全的。
func (s *Schema) Close(ctx context.Context) error {
	if s == nil {
		return nil
	}
	s.closeOnce.Do(func() {
		if _, err := s.admin.Exec(ctx, "DROP SCHEMA "+pgx.Identifier{s.Name}.Sanitize()+" CASCADE"); err != nil {
			s.closeErr = fmt.Errorf("drop schema %s: %w", s.Name, err)
		}
		if err := s.admin.Close(ctx); err != nil {
			s.closeErr = errors.Join(s.closeErr, fmt.Errorf("close schema admin connection: %w", err))
		}
	})
	return s.closeErr
}

// Tables 返回该 Schema 当前包含的普通表名。
func (s *Schema) Tables(ctx context.Context) ([]string, error) {
	rows, err := s.admin.Query(ctx, `
SELECT table_name
FROM information_schema.tables
WHERE table_schema = $1 AND table_type = 'BASE TABLE'
ORDER BY table_name`, s.Name)
	if err != nil {
		return nil, fmt.Errorf("query schema tables: %w", err)
	}
	defer rows.Close()

	var tables []string
	for rows.Next() {
		var table string
		if err := rows.Scan(&table); err != nil {
			return nil, fmt.Errorf("scan schema table: %w", err)
		}
		tables = append(tables, table)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate schema tables: %w", err)
	}
	return tables, nil
}

// Database 表示一个测试独占并自动清理的 PostgreSQL Database。
//
// Database 级隔离用于 advisory lock 或其他不能由 search_path 隔离的测试。
type Database struct {
	Name string
	DSN  string

	admin     *pgx.Conn
	roles     []string
	closeOnce sync.Once
	closeErr  error
}

// NewDatabase 创建随机 Database，并注册强制清理。
func NewDatabase(t testing.TB) *Database {
	t.Helper()

	baseDSN := BaseDSN(t)
	admin := Connect(t, baseDSN)
	name := uniqueName(t, "agentops_test_db")
	if _, err := admin.Exec(context.Background(), "CREATE DATABASE "+pgx.Identifier{name}.Sanitize()+" TEMPLATE template0"); err != nil {
		t.Fatalf("create isolated PostgreSQL database: %v", err)
	}

	parsed := parseURL(t, baseDSN)
	parsed.Path = "/" + name
	query := parsed.Query()
	query.Del("search_path")
	parsed.RawQuery = query.Encode()

	database := &Database{Name: name, DSN: parsed.String(), admin: admin}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), cleanupTimeout)
		defer cancel()
		if err := database.Close(ctx); err != nil {
			t.Errorf("clean isolated PostgreSQL database: %v", err)
		}
	})
	return database
}

// Close 强制断开残留会话并删除测试 Database；重复调用是安全的。
func (d *Database) Close(ctx context.Context) error {
	if d == nil {
		return nil
	}
	d.closeOnce.Do(func() {
		if _, err := d.admin.Exec(ctx, "DROP DATABASE "+pgx.Identifier{d.Name}.Sanitize()+" WITH (FORCE)"); err != nil {
			d.closeErr = fmt.Errorf("drop database %s: %w", d.Name, err)
		}
		for _, role := range d.roles {
			if _, err := d.admin.Exec(ctx, "DROP ROLE IF EXISTS "+pgx.Identifier{role}.Sanitize()); err != nil {
				d.closeErr = errors.Join(d.closeErr, fmt.Errorf("drop test role %s: %w", role, err))
			}
		}
		if err := d.admin.Close(ctx); err != nil {
			d.closeErr = errors.Join(d.closeErr, fmt.Errorf("close database admin connection: %w", err))
		}
	})
	return d.closeErr
}

// DatabaseIdentities 是测试 Database 的 Migration、Runtime 写和 Runtime 读身份。
type DatabaseIdentities struct {
	MigrationDSN    string
	RuntimeWriteDSN string
	RuntimeReadDSN  string

	migrationRole string
	writeRole     string
	readRole      string
}

// RuntimeReadRole 返回测试专用只读角色名，用于构造权限异常 Migration。
func (i *DatabaseIdentities) RuntimeReadRole() string {
	if i == nil {
		return ""
	}
	return i.readRole
}

// NewDatabaseIdentities 创建三个互不继承的最小权限登录角色。
func NewDatabaseIdentities(t testing.TB, database *Database) *DatabaseIdentities {
	t.Helper()
	if database == nil {
		t.Fatal("test database is required for PostgreSQL identities")
	}

	migrationRole, migrationPassword := createLoginRole(t, database, "agentops_migration")
	writeRole, writePassword := createLoginRole(t, database, "agentops_write")
	readRole, readPassword := createLoginRole(t, database, "agentops_read")
	if _, err := database.admin.Exec(
		context.Background(),
		"ALTER DATABASE "+pgx.Identifier{database.Name}.Sanitize()+" OWNER TO "+pgx.Identifier{migrationRole}.Sanitize(),
	); err != nil {
		t.Fatalf("assign test database Migration owner: %v", err)
	}
	if _, err := database.admin.Exec(
		context.Background(),
		"REVOKE CONNECT ON DATABASE "+pgx.Identifier{database.Name}.Sanitize()+" FROM PUBLIC",
	); err != nil {
		t.Fatalf("revoke public test database connect: %v", err)
	}
	if _, err := database.admin.Exec(
		context.Background(),
		"GRANT CONNECT ON DATABASE "+pgx.Identifier{database.Name}.Sanitize()+" TO "+
			pgx.Identifier{migrationRole}.Sanitize()+", "+pgx.Identifier{writeRole}.Sanitize()+", "+pgx.Identifier{readRole}.Sanitize(),
	); err != nil {
		t.Fatalf("grant test database identity connections: %v", err)
	}

	migrationDSN := dsnWithIdentity(t, database.DSN, migrationRole, migrationPassword)
	writeDSN := dsnWithIdentity(t, database.DSN, writeRole, writePassword)
	readDSN := dsnWithIdentity(t, database.DSN, readRole, readPassword)
	migrationConnection := Connect(t, migrationDSN)
	if _, err := migrationConnection.Exec(
		context.Background(),
		"GRANT USAGE ON SCHEMA public TO "+pgx.Identifier{writeRole}.Sanitize()+", "+pgx.Identifier{readRole}.Sanitize(),
	); err != nil {
		t.Fatalf("grant test Runtime schema usage: %v", err)
	}

	return &DatabaseIdentities{
		MigrationDSN:    migrationDSN,
		RuntimeWriteDSN: writeDSN,
		RuntimeReadDSN:  readDSN,
		migrationRole:   migrationRole,
		writeRole:       writeRole,
		readRole:        readRole,
	}
}

// GrantRuntimePrivileges 在测试 Migration 完成后授予探针表的运行期最小权限。
func (i *DatabaseIdentities) GrantRuntimePrivileges(t testing.TB) {
	t.Helper()
	connection := Connect(t, i.MigrationDSN)
	statements := []string{
		"GRANT SELECT, INSERT, UPDATE, DELETE ON ALL TABLES IN SCHEMA public TO " + pgx.Identifier{i.writeRole}.Sanitize(),
		"GRANT USAGE, SELECT, UPDATE ON ALL SEQUENCES IN SCHEMA public TO " + pgx.Identifier{i.writeRole}.Sanitize(),
		"GRANT SELECT ON ALL TABLES IN SCHEMA public TO " + pgx.Identifier{i.readRole}.Sanitize(),
		"GRANT SELECT ON ALL SEQUENCES IN SCHEMA public TO " + pgx.Identifier{i.readRole}.Sanitize(),
		"REVOKE INSERT, UPDATE, DELETE, TRUNCATE, REFERENCES, TRIGGER ON agentops_schema_migrations FROM " + pgx.Identifier{i.writeRole}.Sanitize(),
	}
	for _, statement := range statements {
		if _, err := connection.Exec(context.Background(), statement); err != nil {
			t.Fatalf("grant test Runtime privileges: %v", err)
		}
	}
}

// BaseDSN 返回真实 PostgreSQL 测试 DSN；未配置时跳过当前测试。
func BaseDSN(t testing.TB) string {
	t.Helper()
	dsn := strings.TrimSpace(os.Getenv(DSNEnvironment))
	if dsn == "" {
		t.Skipf("%s is not set", DSNEnvironment)
	}
	return dsn
}

// Connect 建立测试连接并注册自动关闭。
func Connect(t testing.TB, dsn string) *pgx.Conn {
	t.Helper()
	conn, err := pgx.Connect(context.Background(), dsn)
	if err != nil {
		t.Fatalf("connect test PostgreSQL: %v", err)
	}
	t.Cleanup(func() {
		if err := conn.Close(context.Background()); err != nil {
			t.Errorf("close test PostgreSQL connection: %v", err)
		}
	})
	return conn
}

// RuntimeConfig 将隔离 DSN 映射到唯一的生产基础设施配置结构。
func RuntimeConfig(t testing.TB, identities *DatabaseIdentities, httpAddress string) infra.Config {
	t.Helper()
	if identities == nil {
		t.Fatal("PostgreSQL test identities are required")
	}
	return RuntimeConfigForDSNs(
		t,
		identities.MigrationDSN,
		identities.RuntimeWriteDSN,
		identities.RuntimeReadDSN,
		httpAddress,
	)
}

// RuntimeConfigForDSNs 构造需要显式身份 DSN 的测试配置。
func RuntimeConfigForDSNs(
	t testing.TB,
	migrationDSN string,
	writeDSN string,
	readDSN string,
	httpAddress string,
) infra.Config {
	t.Helper()
	document := fmt.Sprintf(`
runtime:
  lock_check_interval: 50ms
  lock_check_timeout: 25ms
postgresql:
  migration_dsn: %s
  runtime_write_dsn: %s
  runtime_read_dsn: %s
http:
  address: %s
  read_header_timeout: 1s
logger:
  level: error
  format: json
shutdown:
  timeout: 3s
`, strconv.Quote(migrationDSN), strconv.Quote(writeDSN), strconv.Quote(readDSN), strconv.Quote(httpAddress))
	config, err := infra.Parse(strings.NewReader(document))
	if err != nil {
		t.Fatalf("parse test infrastructure config: %v", err)
	}
	return config
}

func createLoginRole(t testing.TB, database *Database, prefix string) (string, string) {
	t.Helper()
	role := uniqueName(t, prefix)
	password := uniqueName(t, "password")
	statement := "CREATE ROLE " + pgx.Identifier{role}.Sanitize() + " LOGIN PASSWORD '" + password +
		"' NOSUPERUSER NOCREATEDB NOCREATEROLE NOINHERIT NOREPLICATION NOBYPASSRLS"
	if _, err := database.admin.Exec(context.Background(), statement); err != nil {
		t.Fatalf("create PostgreSQL test role: %v", err)
	}
	database.roles = append(database.roles, role)
	return role, password
}

func dsnWithIdentity(t testing.TB, dsn string, username string, password string) string {
	t.Helper()
	parsed := parseURL(t, dsn)
	parsed.User = url.UserPassword(username, password)
	return parsed.String()
}

func uniqueName(t testing.TB, prefix string) string {
	t.Helper()
	var suffix [8]byte
	if _, err := rand.Read(suffix[:]); err != nil {
		t.Fatalf("generate PostgreSQL test isolation name: %v", err)
	}
	return prefix + "_" + hex.EncodeToString(suffix[:])
}

func parseURL(t testing.TB, dsn string) *url.URL {
	t.Helper()
	parsed, err := url.Parse(dsn)
	if err != nil {
		t.Fatalf("parse test PostgreSQL DSN: %v", err)
	}
	return parsed
}
