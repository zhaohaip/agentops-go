package main

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"testing"
)

func TestParseConfigPath(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		args []string
		want string
	}{
		"default": {
			want: defaultConfigPath,
		},
		"explicit": {
			args: []string{"-config", "/tmp/agentops-infra.yaml"},
			want: "/tmp/agentops-infra.yaml",
		},
	}

	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			got, err := parseConfigPath(test.args)
			if err != nil {
				t.Fatalf("parseConfigPath() error = %v", err)
			}
			if got != test.want {
				t.Fatalf("parseConfigPath() = %q, want %q", got, test.want)
			}
		})
	}
}

func TestParseConfigPathRejectsInvalidArguments(t *testing.T) {
	t.Parallel()

	tests := map[string][]string{
		"unknown flag":     {"-unknown"},
		"missing path":     {"-config"},
		"empty path":       {"-config", ""},
		"positional value": {"infra.yaml"},
	}
	for name, args := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			if _, err := parseConfigPath(args); err == nil {
				t.Fatal("parseConfigPath() error = nil, want error")
			}
		})
	}
}

func TestRunLoadsConfigAndCreatesHost(t *testing.T) {
	t.Parallel()

	configPath := filepath.Join(t.TempDir(), "infra.yaml")
	document := []byte(`
postgresql:
  migration_dsn: postgres://migration:test@127.0.0.1:5432/agentops
  runtime_write_dsn: postgres://writer:test@127.0.0.1:5432/agentops
  runtime_read_dsn: postgres://reader:test@127.0.0.1:5432/agentops
http:
  address: 127.0.0.1:0
`)
	if err := os.WriteFile(configPath, document, 0o600); err != nil {
		t.Fatalf("write test config: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := run(ctx, []string{"-config", configPath}, io.Discard); err != nil {
		t.Fatalf("run() error = %v", err)
	}
}
