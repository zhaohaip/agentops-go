package migration

import (
	"errors"
	"reflect"
	"strings"
	"testing"
)

func TestPrepareMigrationsSortsAndCopiesDefinitions(t *testing.T) {
	t.Parallel()

	definitions := []Migration{
		{Version: 30, Name: "third", Statements: []string{"SELECT 30"}},
		{Version: 10, Name: "first", Statements: []string{"SELECT 10"}},
		{Version: 20, Name: "second", Statements: []string{"SELECT 20"}},
	}
	prepared, err := prepareMigrations(definitions)
	if err != nil {
		t.Fatalf("prepareMigrations() error = %v", err)
	}

	var versions []int64
	for _, current := range prepared {
		versions = append(versions, current.version)
	}
	if want := []int64{10, 20, 30}; !reflect.DeepEqual(versions, want) {
		t.Fatalf("versions = %v, want %v", versions, want)
	}

	definitions[1].Statements[0] = "SELECT 999"
	if prepared[0].statements[0] != "SELECT 10" {
		t.Fatal("prepared migration retained caller-owned statement slice")
	}
}

func TestPrepareMigrationsRejectsInvalidDefinitions(t *testing.T) {
	t.Parallel()

	tests := map[string][]Migration{
		"non-positive version": {
			{Version: 0, Name: "zero", Statements: []string{"SELECT 1"}},
		},
		"empty name": {
			{Version: 1, Statements: []string{"SELECT 1"}},
		},
		"name whitespace": {
			{Version: 1, Name: " first ", Statements: []string{"SELECT 1"}},
		},
		"no statements": {
			{Version: 1, Name: "first"},
		},
		"empty statement": {
			{Version: 1, Name: "first", Statements: []string{" \n"}},
		},
		"duplicate version": {
			{Version: 1, Name: "first", Statements: []string{"SELECT 1"}},
			{Version: 1, Name: "second", Statements: []string{"SELECT 2"}},
		},
	}

	for name, definitions := range tests {
		name := name
		definitions := definitions
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			_, err := prepareMigrations(definitions)
			if !errors.Is(err, ErrInvalidMigration) {
				t.Fatalf("prepareMigrations() error = %v, want ErrInvalidMigration", err)
			}
		})
	}
}

func TestMigrationChecksumIncludesStatementBoundariesAndContent(t *testing.T) {
	t.Parallel()

	base := migrationChecksum([]string{"SELECT 1", "SELECT 2"})
	if len(base) != 64 || strings.ToLower(base) != base {
		t.Fatalf("checksum = %q, want 64 lowercase hexadecimal characters", base)
	}
	if base == migrationChecksum([]string{"SELECT 1SELECT 2"}) {
		t.Fatal("checksum did not preserve statement boundaries")
	}
	if base == migrationChecksum([]string{"SELECT 1", "SELECT 3"}) {
		t.Fatal("checksum did not detect statement change")
	}
}

func TestValidateAppliedHistory(t *testing.T) {
	t.Parallel()

	registered, err := prepareMigrations([]Migration{
		{Version: 10, Name: "first", Statements: []string{"SELECT 10"}},
		{Version: 20, Name: "second", Statements: []string{"SELECT 20"}},
	})
	if err != nil {
		t.Fatalf("prepareMigrations() error = %v", err)
	}

	tests := []struct {
		name    string
		applied []appliedMigration
		wantErr error
	}{
		{name: "empty"},
		{
			name: "valid prefix",
			applied: []appliedMigration{
				{version: 10, name: "first", checksum: registered[0].checksum},
			},
		},
		{
			name: "unknown version",
			applied: []appliedMigration{
				{version: 99, name: "unknown", checksum: strings.Repeat("a", 64)},
			},
			wantErr: ErrUnknownAppliedVersion,
		},
		{
			name: "missing earlier version",
			applied: []appliedMigration{
				{version: 20, name: "second", checksum: registered[1].checksum},
			},
			wantErr: ErrAppliedHistoryInconsistent,
		},
		{
			name: "name mismatch",
			applied: []appliedMigration{
				{version: 10, name: "changed", checksum: registered[0].checksum},
			},
			wantErr: ErrAppliedMigrationMismatch,
		},
		{
			name: "checksum mismatch",
			applied: []appliedMigration{
				{version: 10, name: "first", checksum: strings.Repeat("b", 64)},
			},
			wantErr: ErrAppliedMigrationMismatch,
		},
	}

	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			err := validateAppliedHistory(registered, test.applied)
			if !errors.Is(err, test.wantErr) {
				t.Fatalf("validateAppliedHistory() error = %v, want %v", err, test.wantErr)
			}
		})
	}
}
