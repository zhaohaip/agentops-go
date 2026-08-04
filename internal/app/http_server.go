package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"sync"

	"github.com/zhaohaip/agentops-go/internal/config/infra"
)

// HTTPServer owns the infrastructure HTTP listener lifecycle.
type HTTPServer struct {
	config        infra.HTTPServerConfig
	logger        *slog.Logger
	server        *http.Server
	done          chan error
	handlerCancel context.CancelFunc

	mu           sync.RWMutex
	listener     net.Listener
	started      bool
	shutdownOnce sync.Once
	shutdownErr  error
}

// NewHTTPServer creates an HTTP server without opening its listener.
func NewHTTPServer(config infra.HTTPServerConfig, logger *slog.Logger, handler http.Handler) (*HTTPServer, error) {
	if logger == nil {
		return nil, errors.New("create HTTP server: logger is required")
	}
	if handler == nil {
		handler = http.NotFoundHandler()
	}

	handlerContext, cancelHandlers := context.WithCancel(context.Background())
	return &HTTPServer{
		config: config,
		logger: logger,
		server: &http.Server{
			Addr:              config.Address,
			Handler:           handler,
			ReadHeaderTimeout: config.ReadHeaderTimeout,
			BaseContext: func(net.Listener) context.Context {
				return handlerContext
			},
		},
		done:          make(chan error, 1),
		handlerCancel: cancelHandlers,
	}, nil
}

func (s *HTTPServer) name() string {
	return "http_server"
}

// Start opens the configured listener and begins serving in the background.
func (s *HTTPServer) Start() error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.started {
		return errors.New("start HTTP server: already started")
	}

	listener, err := net.Listen("tcp", s.config.Address)
	if err != nil {
		return fmt.Errorf("start HTTP server: %w", err)
	}

	s.listener = listener
	s.started = true
	s.logger.Info("HTTP server started", "address", listener.Addr().String())
	go s.serve(listener)

	return nil
}

func (s *HTTPServer) serve(listener net.Listener) {
	err := s.server.Serve(listener)
	if errors.Is(err, http.ErrServerClosed) {
		err = nil
	}
	s.done <- err
	close(s.done)
}

func (s *HTTPServer) doneSignal() <-chan error {
	return s.done
}

// Shutdown 优雅关闭 HTTP Server；Context 超时后会取消活动 Handler 并强制关闭连接。
//
// 第一次调用决定关闭宽限期和最终结果；并发及后续调用返回同一个最终结果。
func (s *HTTPServer) Shutdown(ctx context.Context) error {
	if ctx == nil {
		return errors.New("shutdown HTTP server: context is required")
	}
	s.mu.RLock()
	started := s.started
	s.mu.RUnlock()
	if !started {
		return nil
	}

	s.shutdownOnce.Do(func() {
		s.shutdownErr = s.shutdown(ctx)
	})
	return s.shutdownErr
}

func (s *HTTPServer) shutdown(ctx context.Context) error {
	shutdownErr := s.server.Shutdown(ctx)
	s.handlerCancel()
	if shutdownErr == nil {
		return nil
	}

	forceErr := s.server.Close()
	if forceErr != nil && !errors.Is(forceErr, http.ErrServerClosed) {
		forceErr = fmt.Errorf("force close HTTP server: %w", forceErr)
	} else {
		forceErr = nil
	}
	return errors.Join(fmt.Errorf("shutdown HTTP server: %w", shutdownErr), forceErr)
}

// Address returns the configured address, or the bound address after Start.
func (s *HTTPServer) Address() string {
	s.mu.RLock()
	defer s.mu.RUnlock()

	if s.listener == nil {
		return s.config.Address
	}
	return s.listener.Addr().String()
}
