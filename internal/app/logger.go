package app

import (
	"errors"
	"io"
	"log/slog"

	"github.com/zhaohaip/agentops-go/internal/config/infra"
)

// NewLogger creates the Runtime's structured logger.
func NewLogger(config infra.LoggerConfig, output io.Writer) (*slog.Logger, error) {
	if output == nil {
		return nil, errors.New("initialize logger: output is required")
	}

	var level slog.Level
	switch config.Level {
	case "debug":
		level = slog.LevelDebug
	case "info":
		level = slog.LevelInfo
	case "warn":
		level = slog.LevelWarn
	case "error":
		level = slog.LevelError
	default:
		return nil, errors.New("initialize logger: unsupported level")
	}

	options := &slog.HandlerOptions{Level: level}
	var handler slog.Handler
	switch config.Format {
	case "json":
		handler = slog.NewJSONHandler(output, options)
	case "text":
		handler = slog.NewTextHandler(output, options)
	default:
		return nil, errors.New("initialize logger: unsupported format")
	}

	return slog.New(handler), nil
}
