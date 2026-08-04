// Package app 装配并控制 AgentOps Runtime 进程生命周期。
package app

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sync"
	"time"

	postgresruntime "github.com/zhaohaip/agentops-go/internal/adapter/postgres/runtime"
	"github.com/zhaohaip/agentops-go/internal/config/infra"
	"github.com/zhaohaip/agentops-go/migrations"
)

type component interface {
	name() string
	Start() error
	doneSignal() <-chan error
	Shutdown(context.Context) error
}

type databaseRuntime interface {
	Migrate(context.Context) error
	StartMonitoring() error
	Done() <-chan error
	StopAcceptingWrites()
	Close(context.Context) error
}

type databaseFactory func(context.Context) (databaseRuntime, error)

// Host 是 Runtime 进程的组合根和生命周期控制器。
type Host struct {
	logger          *slog.Logger
	shutdownTimeout time.Duration
	databaseFactory databaseFactory
	components      []component

	mu  sync.Mutex
	ran bool
}

// NewHost 初始化 Logger 和 HTTP Server，并装配 PostgreSQL Runtime 工厂。
func NewHost(config infra.Config, logOutput io.Writer) (*Host, error) {
	if err := config.Validate(); err != nil {
		return nil, err
	}

	logger, err := NewLogger(config.Logger, logOutput)
	if err != nil {
		return nil, err
	}
	httpServer, err := NewHTTPServer(config.HTTP, logger, nil)
	if err != nil {
		return nil, err
	}

	factory := func(ctx context.Context) (databaseRuntime, error) {
		return postgresruntime.Open(ctx, config.PostgreSQL, config.Runtime, migrations.All())
	}
	return newHostWithDatabase(logger, config.Shutdown.Timeout, factory, httpServer), nil
}

func newHost(logger *slog.Logger, shutdownTimeout time.Duration, components ...component) *Host {
	return newHostWithDatabase(logger, shutdownTimeout, nil, components...)
}

func newHostWithDatabase(
	logger *slog.Logger,
	shutdownTimeout time.Duration,
	factory databaseFactory,
	components ...component,
) *Host {
	return &Host{
		logger:          logger,
		shutdownTimeout: shutdownTimeout,
		databaseFactory: factory,
		components:      components,
	}
}

// Run 依次取得数据库锁、执行 Migration、监控持锁连接并启动组件。
//
// Context 取消、组件失败或持锁连接失效时，Host 按组件、数据库的逆依赖顺序有界关闭。
func (h *Host) Run(ctx context.Context) error {
	if ctx == nil {
		return errors.New("run host: context is required")
	}
	if h.logger == nil {
		return errors.New("run host: logger is required")
	}
	if h.shutdownTimeout <= 0 {
		return errors.New("run host: shutdown timeout must be positive")
	}

	h.mu.Lock()
	if h.ran {
		h.mu.Unlock()
		return errors.New("run host: already run")
	}
	h.ran = true
	h.mu.Unlock()

	if ctx.Err() != nil {
		return nil
	}

	database, err := h.openDatabase(ctx)
	if err != nil {
		return err
	}
	if database != nil {
		if err := database.Migrate(ctx); err != nil {
			shutdownErr := h.shutdownRuntime(nil, database)
			return errors.Join(fmt.Errorf("run host migrations: %w", err), shutdownErr)
		}
		if err := database.StartMonitoring(); err != nil {
			shutdownErr := h.shutdownRuntime(nil, database)
			return errors.Join(fmt.Errorf("start PostgreSQL runtime monitoring: %w", err), shutdownErr)
		}
	}

	started, err := h.startComponents()
	if err != nil {
		shutdownErr := h.shutdownRuntime(started, database)
		return errors.Join(err, shutdownErr)
	}

	h.logger.Info("runtime host started")
	runtimeErr := h.wait(ctx, started, database)
	h.logger.Info("runtime host stopping")
	shutdownErr := h.shutdownRuntime(started, database)

	return errors.Join(runtimeErr, shutdownErr)
}

func (h *Host) openDatabase(ctx context.Context) (databaseRuntime, error) {
	if h.databaseFactory == nil {
		return nil, nil
	}
	database, err := h.databaseFactory(ctx)
	if err != nil {
		return nil, fmt.Errorf("open PostgreSQL runtime: %w", err)
	}
	if database == nil {
		return nil, errors.New("open PostgreSQL runtime: factory returned nil")
	}
	return database, nil
}

func (h *Host) startComponents() ([]component, error) {
	started := make([]component, 0, len(h.components))
	for _, current := range h.components {
		if current == nil {
			return started, errors.New("start host: component is required")
		}
		if err := current.Start(); err != nil {
			return started, fmt.Errorf("start host component %s: %w", current.name(), err)
		}
		started = append(started, current)
	}
	return started, nil
}

func (h *Host) wait(ctx context.Context, started []component, database databaseRuntime) error {
	if len(started) == 0 && database == nil {
		<-ctx.Done()
		return nil
	}

	type componentResult struct {
		name string
		err  error
	}
	results := make(chan componentResult, len(started))
	for _, current := range started {
		go func(running component) {
			err, ok := <-running.doneSignal()
			if !ok {
				err = nil
			}
			results <- componentResult{name: running.name(), err: err}
		}(current)
	}
	var databaseDone <-chan error
	if database != nil {
		databaseDone = database.Done()
	}

	select {
	case <-ctx.Done():
		return nil
	case err := <-databaseDone:
		if err == nil {
			return errors.New("PostgreSQL runtime stopped unexpectedly")
		}
		return fmt.Errorf("PostgreSQL runtime failed: %w", err)
	case result := <-results:
		if result.err == nil {
			return fmt.Errorf("host component %s stopped unexpectedly", result.name)
		}
		return fmt.Errorf("host component %s failed: %w", result.name, result.err)
	}
}

func (h *Host) shutdownRuntime(started []component, database databaseRuntime) error {
	if database != nil {
		database.StopAcceptingWrites()
	}

	ctx, cancel := context.WithTimeout(context.Background(), h.shutdownTimeout)
	defer cancel()

	var shutdownErrors []error
	for index := len(started) - 1; index >= 0; index-- {
		current := started[index]
		if err := current.Shutdown(ctx); err != nil {
			shutdownErrors = append(shutdownErrors, fmt.Errorf("shutdown host component %s: %w", current.name(), err))
		}
	}
	if database != nil {
		if err := database.Close(ctx); err != nil {
			shutdownErrors = append(shutdownErrors, fmt.Errorf("shutdown PostgreSQL runtime: %w", err))
		}
	}

	return errors.Join(shutdownErrors...)
}
