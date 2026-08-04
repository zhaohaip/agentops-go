package infra

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"
)

const (
	testMigrationDSN = "postgres://agentops_migration:test-password@127.0.0.1:5432/agentops?sslmode=disable"
	testWriteDSN     = "postgres://agentops_write:test-password@127.0.0.1:5432/agentops?sslmode=disable"
	testReadDSN      = "postgres://agentops_read:test-password@127.0.0.1:5432/agentops?sslmode=disable"
)

func TestParseAppliesDefaults(t *testing.T) {
	t.Parallel()
	config := parseConfig(t, validPostgreSQLDocument())

	if config.Runtime.LockCheckInterval != 5*time.Second || config.Runtime.LockCheckTimeout != 2*time.Second {
		t.Fatalf("Runtime config = %+v, want default lock checks", config.Runtime)
	}
	if config.PostgreSQL.MigrationDSN.Value() != testMigrationDSN ||
		config.PostgreSQL.RuntimeWriteDSN.Value() != testWriteDSN ||
		config.PostgreSQL.RuntimeReadDSN.Value() != testReadDSN {
		t.Fatal("PostgreSQL identity DSNs were not loaded")
	}
	if config.HTTP.Address != "127.0.0.1:8080" || config.HTTP.ReadHeaderTimeout != 5*time.Second {
		t.Fatalf("HTTP config = %+v, want defaults", config.HTTP)
	}
	if config.Logger.Level != "info" || config.Logger.Format != "json" {
		t.Fatalf("Logger = %+v, want info/json", config.Logger)
	}
	if config.Shutdown.Timeout != 10*time.Second {
		t.Fatalf("Shutdown timeout = %s, want 10s", config.Shutdown.Timeout)
	}
}

func TestParseExplicitValues(t *testing.T) {
	t.Parallel()
	config := parseConfig(t, `
runtime:
  lock_check_interval: 30s
  lock_check_timeout: 3s
postgresql:
  migration_dsn: postgres://migration@localhost:5433/runtime
  runtime_write_dsn: postgres://writer@localhost:5433/runtime
  runtime_read_dsn: postgres://reader@localhost:5433/runtime
http:
  address: "[::1]:9090"
  read_header_timeout: 7s
logger:
  level: DEBUG
  format: TEXT
shutdown:
  timeout: 45s
`)

	if config.Runtime.LockCheckInterval != 30*time.Second || config.Runtime.LockCheckTimeout != 3*time.Second {
		t.Fatalf("Runtime config = %+v, want explicit values", config.Runtime)
	}
	if config.HTTP.Address != "[::1]:9090" || config.HTTP.ReadHeaderTimeout != 7*time.Second {
		t.Fatalf("HTTP config = %+v, want explicit values", config.HTTP)
	}
	if config.Logger.Level != "debug" || config.Logger.Format != "text" {
		t.Fatalf("Logger config = %+v, want normalized explicit values", config.Logger)
	}
	if config.Shutdown.Timeout != 45*time.Second {
		t.Fatalf("Shutdown timeout = %s, want 45s", config.Shutdown.Timeout)
	}
}

func TestParseRejectsUnknownFields(t *testing.T) {
	t.Parallel()
	tests := map[string]string{
		"root":              validPostgreSQLDocument() + "unknown: true\n",
		"nested":            strings.TrimSuffix(validPostgreSQLDocument(), "\n") + "\n  password: secret\n",
		"legacy single DSN": "postgresql:\n  dsn: " + testWriteDSN + "\n",
	}
	for name, document := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			_, err := Parse(strings.NewReader(document))
			if err == nil || !strings.Contains(err.Error(), "field") {
				t.Fatalf("Parse() error = %v, want unknown field diagnostic", err)
			}
		})
	}
}

func TestParseRejectsMissingAndInvalidConfiguration(t *testing.T) {
	t.Parallel()
	tests := map[string]string{
		"missing identities":    "logger:\n  level: info\n",
		"missing migration DSN": postgresDocument("", testWriteDSN, testReadDSN),
		"missing write DSN":     postgresDocument(testMigrationDSN, "", testReadDSN),
		"missing read DSN":      postgresDocument(testMigrationDSN, testWriteDSN, ""),
		"invalid migration DSN": postgresDocument("not-a-url", testWriteDSN, testReadDSN),
		"invalid write DSN":     postgresDocument(testMigrationDSN, "not-a-url", testReadDSN),
		"invalid read DSN":      postgresDocument(testMigrationDSN, testWriteDSN, "not-a-url"),
		"invalid lock interval": validPostgreSQLDocument() + "runtime:\n  lock_check_interval: 0s\n",
		"invalid duration":      validPostgreSQLDocument() + "shutdown:\n  timeout: soon\n",
		"non-loopback HTTP":     validPostgreSQLDocument() + "http:\n  address: 0.0.0.0:8080\n",
		"invalid HTTP port":     validPostgreSQLDocument() + "http:\n  address: 127.0.0.1:not-a-port\n",
		"invalid logger level":  validPostgreSQLDocument() + "logger:\n  level: verbose\n",
		"invalid logger format": validPostgreSQLDocument() + "logger:\n  format: xml\n",
		"invalid shutdown":      validPostgreSQLDocument() + "shutdown:\n  timeout: -1s\n",
	}
	for name, document := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if _, err := Parse(strings.NewReader(document)); err == nil {
				t.Fatal("Parse() error = nil, want validation error")
			}
		})
	}
}

func TestParseRejectsEmptyAndMultipleDocuments(t *testing.T) {
	t.Parallel()
	tests := map[string]string{
		"empty":    "",
		"multiple": validPostgreSQLDocument() + "---\n" + validPostgreSQLDocument(),
	}
	for name, document := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if _, err := Parse(strings.NewReader(document)); err == nil {
				t.Fatal("Parse() error = nil, want document error")
			}
		})
	}
}

func TestSensitiveConfigurationIsRedacted(t *testing.T) {
	t.Parallel()
	const password = "do-not-leak"
	config := parseConfig(t, postgresDocument(
		"postgres://migration:"+password+"@127.0.0.1:5432/agentops",
		"postgres://writer:"+password+"@127.0.0.1:5432/agentops",
		"postgres://reader:"+password+"@127.0.0.1:5432/agentops",
	))

	jsonValue, err := json.Marshal(config)
	if err != nil {
		t.Fatalf("json.Marshal() error = %v", err)
	}
	yamlValue, err := yaml.Marshal(config)
	if err != nil {
		t.Fatalf("yaml.Marshal() error = %v", err)
	}
	renderings := []string{
		fmt.Sprint(config), fmt.Sprintf("%+v", config), fmt.Sprintf("%#v", config),
		fmt.Sprint(config.PostgreSQL), fmt.Sprintf("%+v", config.PostgreSQL),
		fmt.Sprint(config.PostgreSQL.MigrationDSN), fmt.Sprint(config.PostgreSQL.RuntimeWriteDSN),
		fmt.Sprint(config.PostgreSQL.RuntimeReadDSN), string(jsonValue), string(yamlValue),
	}
	for _, rendering := range renderings {
		if strings.Contains(rendering, password) {
			t.Fatalf("sensitive value exposed by rendering %q", rendering)
		}
	}
}

func TestValidationErrorDoesNotExposeDSN(t *testing.T) {
	t.Parallel()
	const password = "do-not-leak"
	_, err := Parse(strings.NewReader(postgresDocument(
		"postgres://migration:"+password+"@",
		testWriteDSN,
		testReadDSN,
	)))
	if err == nil {
		t.Fatal("Parse() error = nil, want invalid DSN error")
	}
	if strings.Contains(err.Error(), password) {
		t.Fatalf("Parse() error exposed sensitive value: %q", err)
	}
}

func validPostgreSQLDocument() string {
	return postgresDocument(testMigrationDSN, testWriteDSN, testReadDSN)
}

func postgresDocument(migrationDSN string, writeDSN string, readDSN string) string {
	return fmt.Sprintf(`postgresql:
  migration_dsn: %s
  runtime_write_dsn: %s
  runtime_read_dsn: %s
`, migrationDSN, writeDSN, readDSN)
}

func parseConfig(t *testing.T, document string) Config {
	t.Helper()
	config, err := Parse(strings.NewReader(document))
	if err != nil {
		t.Fatalf("Parse() error = %v", err)
	}
	return config
}
