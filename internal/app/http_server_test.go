package app

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"testing"
	"time"

	"github.com/zhaohaip/agentops-go/internal/config/infra"
)

func TestHTTPServerLifecycle(t *testing.T) {
	t.Parallel()

	server := newTestHTTPServer(t, http.NotFoundHandler())
	if err := server.Start(); err != nil {
		t.Fatalf("Start() error = %v", err)
	}

	client := &http.Client{Timeout: time.Second}
	response, err := client.Get("http://" + server.Address() + "/")
	if err != nil {
		t.Fatalf("GET infrastructure server: %v", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want %d", response.StatusCode, http.StatusNotFound)
	}

	if err := server.Shutdown(context.Background()); err != nil {
		t.Fatalf("Shutdown() error = %v", err)
	}
	select {
	case err := <-server.doneSignal():
		if err != nil {
			t.Fatalf("server completion error = %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("server did not stop")
	}
}

func TestHTTPServerRejectsSecondStart(t *testing.T) {
	t.Parallel()

	server := newTestHTTPServer(t, nil)
	if err := server.Start(); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	t.Cleanup(func() {
		_ = server.Shutdown(context.Background())
	})

	if err := server.Start(); err == nil {
		t.Fatal("second Start() error = nil, want error")
	}
}

func TestHTTPServerShutdownTimeoutForcesActiveConnectionClosed(t *testing.T) {
	handlerEntered := make(chan struct{})
	handlerDone := make(chan struct{})
	releaseHandler := make(chan struct{})
	handler := http.HandlerFunc(func(_ http.ResponseWriter, request *http.Request) {
		close(handlerEntered)
		defer close(handlerDone)
		select {
		case <-request.Context().Done():
		case <-releaseHandler:
		}
	})
	server := newTestHTTPServer(t, handler)
	if err := server.Start(); err != nil {
		t.Fatalf("Start() error = %v", err)
	}

	clientResult := make(chan error, 1)
	go func() {
		response, err := http.Get("http://" + server.Address() + "/blocked")
		if response != nil {
			_ = response.Body.Close()
		}
		clientResult <- err
	}()
	select {
	case <-handlerEntered:
	case <-time.After(time.Second):
		close(releaseHandler)
		t.Fatal("HTTP handler did not start")
	}

	shutdownCtx, cancelShutdown := context.WithTimeout(context.Background(), 50*time.Millisecond)
	shutdownErr := server.Shutdown(shutdownCtx)
	cancelShutdown()
	if !errors.Is(shutdownErr, context.DeadlineExceeded) {
		close(releaseHandler)
		t.Fatalf("Shutdown() error = %v, want deadline exceeded", shutdownErr)
	}
	repeatedErr := server.Shutdown(context.Background())
	if repeatedErr == nil || repeatedErr.Error() != shutdownErr.Error() {
		close(releaseHandler)
		t.Fatalf("repeated Shutdown() error = %v, want %v", repeatedErr, shutdownErr)
	}

	connection, dialErr := net.DialTimeout("tcp", server.Address(), 100*time.Millisecond)
	if dialErr == nil {
		_ = connection.Close()
		close(releaseHandler)
		t.Fatal("HTTP listener still accepted connections after Shutdown timeout")
	}

	select {
	case <-handlerDone:
	case <-time.After(time.Second):
		close(releaseHandler)
		t.Fatal("active HTTP handler continued after forced shutdown")
	}
	select {
	case err := <-clientResult:
		if err == nil {
			t.Fatal("active HTTP client connection completed without a forced-close error")
		}
	case <-time.After(time.Second):
		t.Fatal("active HTTP client connection was not closed")
	}
}

func newTestHTTPServer(t *testing.T, handler http.Handler) *HTTPServer {
	t.Helper()

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	server, err := NewHTTPServer(infra.HTTPServerConfig{
		Address:           "127.0.0.1:0",
		ReadHeaderTimeout: time.Second,
	}, logger, handler)
	if err != nil {
		t.Fatalf("NewHTTPServer() error = %v", err)
	}
	return server
}
