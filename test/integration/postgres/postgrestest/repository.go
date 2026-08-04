package postgrestest

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/zhaohaip/agentops-go/internal/adapter/postgres/migration"
	postgresruntime "github.com/zhaohaip/agentops-go/internal/adapter/postgres/runtime"
	"github.com/zhaohaip/agentops-go/internal/config/infra"
)

// RepositoryEnvironment 是 Repository 契约用例可使用的真实 PostgreSQL 能力。
type RepositoryEnvironment struct {
	Database   *Database
	Identities *DatabaseIdentities
	Config     infra.Config
	Runtime    *postgresruntime.Runtime
}

// RepositoryCase 定义一个不依赖其他用例状态的 Repository 契约场景。
type RepositoryCase struct {
	Name string
	Run  func(*testing.T, *RepositoryEnvironment)
}

// RepositoryContract 定义模块提供给统一 runner 的 Migration 与契约场景。
type RepositoryContract struct {
	Name       string
	Migrations []migration.Migration
	Cases      []RepositoryCase
}

// RunRepositoryContract 在每个场景的独立 Database 中并行执行 Repository 契约。
//
// 后续模块应在 Cases 中覆盖自身的条件更新、唯一约束和竞争语义；runner 统一提供
// READ COMMITTED 写入口、只读池、Database Clock、提交/回滚和 advisory lock 隔离。
func RunRepositoryContract(t *testing.T, contract RepositoryContract) {
	t.Helper()
	if contract.Name == "" {
		t.Fatal("repository contract name is required")
	}
	if len(contract.Cases) == 0 {
		t.Fatal("repository contract cases are required")
	}

	for _, current := range contract.Cases {
		current := current
		t.Run(current.Name, func(t *testing.T) {
			t.Parallel()
			if current.Name == "" || current.Run == nil {
				t.Fatal("repository contract case name and runner are required")
			}
			environment := NewRepositoryEnvironment(t, contract.Migrations)
			current.Run(t, environment)
		})
	}
}

// NewRepositoryEnvironment 创建独立 Database、取得 advisory lock、执行 Migration
// 并启动持锁连接监控。
func NewRepositoryEnvironment(t testing.TB, definitions []migration.Migration) *RepositoryEnvironment {
	t.Helper()
	database := NewDatabase(t)
	identities := NewDatabaseIdentities(t, database)
	config := RuntimeConfig(t, identities, "127.0.0.1:0")
	runtime, err := postgresruntime.Open(context.Background(), config.PostgreSQL, config.Runtime, definitions)
	if err != nil {
		t.Fatalf("open repository contract runtime: %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		if err := runtime.Close(ctx); err != nil && !errors.Is(err, context.DeadlineExceeded) {
			t.Errorf("close repository contract runtime: %v", err)
		}
	})
	if err := runtime.Migrate(context.Background()); err != nil {
		t.Fatalf("migrate repository contract database: %v", err)
	}
	identities.GrantRuntimePrivileges(t)
	if err := runtime.StartMonitoring(); err != nil {
		t.Fatalf("monitor repository contract runtime: %v", err)
	}
	return &RepositoryEnvironment{Database: database, Identities: identities, Config: config, Runtime: runtime}
}

// ExecuteConcurrent 同时释放指定数量的竞争者，并返回每个竞争者的原始结果。
func ExecuteConcurrent(ctx context.Context, contenders int, work func(context.Context, int) error) []error {
	if contenders <= 0 {
		return []error{errors.New("concurrent contender count must be positive")}
	}
	if work == nil {
		return []error{errors.New("concurrent work is required")}
	}

	start := make(chan struct{})
	results := make(chan indexedError, contenders)
	for index := 0; index < contenders; index++ {
		go func(contender int) {
			select {
			case <-ctx.Done():
				results <- indexedError{index: contender, err: ctx.Err()}
			case <-start:
				results <- indexedError{index: contender, err: work(ctx, contender)}
			}
		}(index)
	}
	close(start)

	errorsByIndex := make([]error, contenders)
	for range contenders {
		result := <-results
		errorsByIndex[result.index] = result.err
	}
	return errorsByIndex
}

type indexedError struct {
	index int
	err   error
}

// FormatConcurrentErrors 为测试失败信息生成不丢失错误链的摘要。
func FormatConcurrentErrors(results []error) error {
	var failures []error
	for index, err := range results {
		if err != nil {
			failures = append(failures, fmt.Errorf("contender %d: %w", index, err))
		}
	}
	return errors.Join(failures...)
}
