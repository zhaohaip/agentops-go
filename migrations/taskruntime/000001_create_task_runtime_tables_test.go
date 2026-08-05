package taskruntime

import (
	"strings"
	"testing"
)

func TestMigrationsExposeStableTaskRuntimeVersion(t *testing.T) {
	t.Parallel()

	definitions := Migrations()
	if len(definitions) != 1 {
		t.Fatalf("Migrations() length = %d, want 1", len(definitions))
	}
	if definitions[0].Version != 1 || definitions[0].Name != "create_task_runtime_tables" {
		t.Fatalf("Migration identity = (%d, %q), want (1, create_task_runtime_tables)", definitions[0].Version, definitions[0].Name)
	}
	if len(definitions[0].Statements) != 10 {
		t.Fatalf("Migration statement count = %d, want 10", len(definitions[0].Statements))
	}
}

func TestMigrationDoesNotCreateLaterPhaseTables(t *testing.T) {
	t.Parallel()

	joined := strings.ToLower(strings.Join(Migrations()[0].Statements, "\n"))
	for _, table := range []string{"plan", "step", "checkpoint", "tool_execution", "approval", "report"} {
		if strings.Contains(joined, "create table "+table+" ") || strings.Contains(joined, "create table "+table+"(") {
			t.Fatalf("Task Runtime Migration creates later-phase table %q", table)
		}
	}
}
