package app

import (
	"bytes"
	"io"
	"strings"
	"testing"

	"github.com/zhaohaip/agentops-go/internal/config/infra"
)

func TestNewLoggerHonorsLevelAndFormat(t *testing.T) {
	t.Parallel()

	var output bytes.Buffer
	logger, err := NewLogger(infra.LoggerConfig{Level: "info", Format: "json"}, &output)
	if err != nil {
		t.Fatalf("NewLogger() error = %v", err)
	}

	logger.Debug("hidden")
	logger.Info("visible", "component", "test")

	logged := output.String()
	if strings.Contains(logged, "hidden") {
		t.Fatalf("debug message was logged at info level: %q", logged)
	}
	if !strings.Contains(logged, `"msg":"visible"`) || !strings.Contains(logged, `"component":"test"`) {
		t.Fatalf("structured info message missing from %q", logged)
	}
}

func TestNewLoggerRejectsInvalidInput(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		config infra.LoggerConfig
		output io.Writer
	}{
		"nil output": {
			config: infra.LoggerConfig{Level: "info", Format: "json"},
		},
		"invalid level": {
			config: infra.LoggerConfig{Level: "verbose", Format: "json"},
			output: &bytes.Buffer{},
		},
		"invalid format": {
			config: infra.LoggerConfig{Level: "info", Format: "xml"},
			output: &bytes.Buffer{},
		},
	}

	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			if _, err := NewLogger(test.config, test.output); err == nil {
				t.Fatal("NewLogger() error = nil, want error")
			}
		})
	}
}

func TestLoggerDoesNotExposeSensitiveConfig(t *testing.T) {
	t.Parallel()

	const password = "do-not-log"
	config, err := infra.Parse(strings.NewReader(
		"postgresql:\n" +
			"  migration_dsn: postgres://migration:" + password + "@127.0.0.1:5432/agentops\n" +
			"  runtime_write_dsn: postgres://writer:" + password + "@127.0.0.1:5432/agentops\n" +
			"  runtime_read_dsn: postgres://reader:" + password + "@127.0.0.1:5432/agentops\n",
	))
	if err != nil {
		t.Fatalf("Parse() error = %v", err)
	}

	var output bytes.Buffer
	logger, err := NewLogger(config.Logger, &output)
	if err != nil {
		t.Fatalf("NewLogger() error = %v", err)
	}
	logger.Info("configuration loaded", "config", config)

	if strings.Contains(output.String(), password) {
		t.Fatalf("logger exposed sensitive configuration: %q", output.String())
	}
}
