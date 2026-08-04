// Package infra loads and validates the Runtime's infrastructure configuration.
package infra

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

const redactedValue = "<redacted>"

// Config contains infrastructure-only Runtime configuration.
type Config struct {
	Runtime    RuntimeHostConfig
	PostgreSQL PostgreSQLConfig
	HTTP       HTTPServerConfig
	Logger     LoggerConfig
	Shutdown   ShutdownConfig
}

// RuntimeHostConfig controls Runtime-level infrastructure checks.
type RuntimeHostConfig struct {
	LockCheckInterval time.Duration
	LockCheckTimeout  time.Duration
}

// PostgreSQLConfig 包含同一 Database 的三种最小权限连接身份。
type PostgreSQLConfig struct {
	MigrationDSN    Secret
	RuntimeWriteDSN Secret
	RuntimeReadDSN  Secret
}

// HTTPServerConfig controls the infrastructure HTTP server.
type HTTPServerConfig struct {
	Address           string
	ReadHeaderTimeout time.Duration
}

// LoggerConfig controls the structured application logger.
type LoggerConfig struct {
	Level  string
	Format string
}

// ShutdownConfig controls the upper bound for graceful Runtime shutdown.
type ShutdownConfig struct {
	Timeout time.Duration
}

// Secret stores a sensitive configuration value and redacts all standard
// formatting and serialization paths.
type Secret struct {
	value string
}

// Value returns the sensitive value for the infrastructure adapter that owns
// its use. Callers must not log the returned string.
func (s Secret) Value() string {
	return s.value
}

// String returns a redacted representation.
func (Secret) String() string {
	return redactedValue
}

// GoString returns a redacted Go-syntax representation.
func (Secret) GoString() string {
	return redactedValue
}

// MarshalText prevents text-based encoders from exposing the value.
func (Secret) MarshalText() ([]byte, error) {
	return []byte(redactedValue), nil
}

// MarshalJSON prevents JSON encoders from exposing the value.
func (Secret) MarshalJSON() ([]byte, error) {
	return json.Marshal(redactedValue)
}

// String returns a safe summary that excludes all configuration values.
func (Config) String() string {
	return "infra.Config{PostgreSQL:" + redactedValue + "}"
}

// GoString returns a safe Go-syntax summary.
func (Config) GoString() string {
	return "infra.Config{PostgreSQL:" + redactedValue + "}"
}

// String returns a safe PostgreSQL configuration summary.
func (PostgreSQLConfig) String() string {
	return "infra.PostgreSQLConfig{MigrationDSN:" + redactedValue +
		",RuntimeWriteDSN:" + redactedValue + ",RuntimeReadDSN:" + redactedValue + "}"
}

// GoString returns a safe Go-syntax PostgreSQL configuration summary.
func (PostgreSQLConfig) GoString() string {
	return "infra.PostgreSQLConfig{MigrationDSN:" + redactedValue +
		",RuntimeWriteDSN:" + redactedValue + ",RuntimeReadDSN:" + redactedValue + "}"
}

type rawConfig struct {
	Runtime    rawRuntimeHostConfig `yaml:"runtime"`
	PostgreSQL rawPostgreSQLConfig  `yaml:"postgresql"`
	HTTP       rawHTTPServerConfig  `yaml:"http"`
	Logger     rawLoggerConfig      `yaml:"logger"`
	Shutdown   rawShutdownConfig    `yaml:"shutdown"`
}

type rawRuntimeHostConfig struct {
	LockCheckInterval string `yaml:"lock_check_interval"`
	LockCheckTimeout  string `yaml:"lock_check_timeout"`
}

type rawPostgreSQLConfig struct {
	MigrationDSN    string `yaml:"migration_dsn"`
	RuntimeWriteDSN string `yaml:"runtime_write_dsn"`
	RuntimeReadDSN  string `yaml:"runtime_read_dsn"`
}

type rawHTTPServerConfig struct {
	Address           string `yaml:"address"`
	ReadHeaderTimeout string `yaml:"read_header_timeout"`
}

type rawLoggerConfig struct {
	Level  string `yaml:"level"`
	Format string `yaml:"format"`
}

type rawShutdownConfig struct {
	Timeout string `yaml:"timeout"`
}

// Load reads infrastructure configuration from a single YAML file.
func Load(path string) (Config, error) {
	if strings.TrimSpace(path) == "" {
		return Config{}, errors.New("load infrastructure config: path is required")
	}

	file, err := os.Open(path)
	if err != nil {
		return Config{}, fmt.Errorf("open infrastructure config: %w", err)
	}
	defer file.Close()

	config, err := Parse(file)
	if err != nil {
		return Config{}, fmt.Errorf("load infrastructure config: %w", err)
	}

	return config, nil
}

// Parse strictly decodes and validates one YAML infrastructure document.
func Parse(reader io.Reader) (Config, error) {
	if reader == nil {
		return Config{}, errors.New("decode infrastructure config: reader is required")
	}

	raw := defaults()
	decoder := yaml.NewDecoder(reader)
	decoder.KnownFields(true)

	if err := decoder.Decode(&raw); err != nil {
		if errors.Is(err, io.EOF) {
			return Config{}, errors.New("decode infrastructure config: document is required")
		}
		return Config{}, fmt.Errorf("decode infrastructure config: %w", err)
	}

	var extra any
	if err := decoder.Decode(&extra); err == nil {
		return Config{}, errors.New("decode infrastructure config: multiple documents are not allowed")
	} else if !errors.Is(err, io.EOF) {
		return Config{}, fmt.Errorf("decode infrastructure config: %w", err)
	}

	config, err := convert(raw)
	if err != nil {
		return Config{}, err
	}
	if err := config.Validate(); err != nil {
		return Config{}, err
	}

	return config, nil
}

func defaults() rawConfig {
	return rawConfig{
		Runtime: rawRuntimeHostConfig{
			LockCheckInterval: "5s",
			LockCheckTimeout:  "2s",
		},
		HTTP: rawHTTPServerConfig{
			Address:           "127.0.0.1:8080",
			ReadHeaderTimeout: "5s",
		},
		Logger: rawLoggerConfig{
			Level:  "info",
			Format: "json",
		},
		Shutdown: rawShutdownConfig{
			Timeout: "10s",
		},
	}
}

// Validate checks all infrastructure configuration invariants.
func (c Config) Validate() error {
	if c.Runtime.LockCheckInterval <= 0 || c.Runtime.LockCheckInterval > 5*time.Minute {
		return errors.New("validate infrastructure config: runtime.lock_check_interval must be between 0 and 5m")
	}
	if c.Runtime.LockCheckTimeout <= 0 || c.Runtime.LockCheckTimeout > c.Runtime.LockCheckInterval {
		return errors.New("validate infrastructure config: runtime.lock_check_timeout must be between 0 and lock_check_interval")
	}
	postgresDSNs := []struct {
		field string
		value string
	}{
		{field: "postgresql.migration_dsn", value: c.PostgreSQL.MigrationDSN.Value()},
		{field: "postgresql.runtime_write_dsn", value: c.PostgreSQL.RuntimeWriteDSN.Value()},
		{field: "postgresql.runtime_read_dsn", value: c.PostgreSQL.RuntimeReadDSN.Value()},
	}
	for _, current := range postgresDSNs {
		if err := validatePostgreSQLDSN(current.field, current.value); err != nil {
			return err
		}
	}
	if err := validateHTTPAddress(c.HTTP.Address); err != nil {
		return err
	}
	if c.HTTP.ReadHeaderTimeout <= 0 || c.HTTP.ReadHeaderTimeout > time.Minute {
		return errors.New("validate infrastructure config: http.read_header_timeout must be between 0 and 1m")
	}
	if c.Logger.Level != "debug" && c.Logger.Level != "info" && c.Logger.Level != "warn" && c.Logger.Level != "error" {
		return errors.New("validate infrastructure config: logger.level must be one of debug, info, warn, error")
	}
	if c.Logger.Format != "json" && c.Logger.Format != "text" {
		return errors.New("validate infrastructure config: logger.format must be one of json, text")
	}
	if c.Shutdown.Timeout <= 0 || c.Shutdown.Timeout > 5*time.Minute {
		return errors.New("validate infrastructure config: shutdown.timeout must be between 0 and 5m")
	}

	return nil
}

func convert(raw rawConfig) (Config, error) {
	lockCheckInterval, err := parseDuration("runtime.lock_check_interval", raw.Runtime.LockCheckInterval)
	if err != nil {
		return Config{}, err
	}
	lockCheckTimeout, err := parseDuration("runtime.lock_check_timeout", raw.Runtime.LockCheckTimeout)
	if err != nil {
		return Config{}, err
	}
	readHeaderTimeout, err := parseDuration("http.read_header_timeout", raw.HTTP.ReadHeaderTimeout)
	if err != nil {
		return Config{}, err
	}
	shutdownTimeout, err := parseDuration("shutdown.timeout", raw.Shutdown.Timeout)
	if err != nil {
		return Config{}, err
	}

	return Config{
		Runtime: RuntimeHostConfig{
			LockCheckInterval: lockCheckInterval,
			LockCheckTimeout:  lockCheckTimeout,
		},
		PostgreSQL: PostgreSQLConfig{
			MigrationDSN:    Secret{value: raw.PostgreSQL.MigrationDSN},
			RuntimeWriteDSN: Secret{value: raw.PostgreSQL.RuntimeWriteDSN},
			RuntimeReadDSN:  Secret{value: raw.PostgreSQL.RuntimeReadDSN},
		},
		HTTP: HTTPServerConfig{
			Address:           raw.HTTP.Address,
			ReadHeaderTimeout: readHeaderTimeout,
		},
		Logger: LoggerConfig{
			Level:  strings.ToLower(raw.Logger.Level),
			Format: strings.ToLower(raw.Logger.Format),
		},
		Shutdown: ShutdownConfig{
			Timeout: shutdownTimeout,
		},
	}, nil
}

func parseDuration(fieldName string, value string) (time.Duration, error) {
	duration, err := time.ParseDuration(value)
	if err != nil {
		return 0, fmt.Errorf("validate infrastructure config: %s must be a valid duration", fieldName)
	}
	return duration, nil
}

func validatePostgreSQLDSN(field string, value string) error {
	if strings.TrimSpace(value) == "" {
		return fmt.Errorf("validate infrastructure config: %s is required", field)
	}

	parsed, err := url.Parse(value)
	if err != nil || (parsed.Scheme != "postgres" && parsed.Scheme != "postgresql") || parsed.Host == "" || strings.Trim(parsed.Path, "/") == "" {
		return fmt.Errorf("validate infrastructure config: %s must be a valid PostgreSQL URL", field)
	}

	return nil
}

func validateHTTPAddress(address string) error {
	host, portText, err := net.SplitHostPort(address)
	if err != nil || host == "" {
		return errors.New("validate infrastructure config: http.address must be a loopback host and TCP port")
	}
	if !strings.EqualFold(host, "localhost") {
		ip := net.ParseIP(host)
		if ip == nil || !ip.IsLoopback() {
			return errors.New("validate infrastructure config: http.address must use a loopback host")
		}
	}

	port, err := strconv.ParseUint(portText, 10, 16)
	if err != nil || port > 65535 {
		return errors.New("validate infrastructure config: http.address must contain a valid TCP port")
	}

	return nil
}
