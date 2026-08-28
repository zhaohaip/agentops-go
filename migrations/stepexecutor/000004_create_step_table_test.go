package stepexecutor

import (
	"strings"
	"testing"
)

func TestMigrationOwnsOnlyStepTableAndFrozenRunForeignKey(t *testing.T) {
	t.Parallel()
	definitions := Migrations()
	if len(definitions) != 1 || definitions[0].Version != 4 || definitions[0].Name != "create_step_table" {
		t.Fatalf("Migrations() = %#v", definitions)
	}
	joined := strings.ToLower(strings.Join(definitions[0].Statements, "\n"))
	if strings.Count(joined, "create table ") != 1 || !strings.Contains(joined, "create table step ") {
		t.Fatalf("Step Executor Migration creates unexpected tables: %s", joined)
	}
	if strings.Contains(joined, "add column") || !strings.Contains(joined, "add constraint run_current_step_foreign_key") {
		t.Fatalf("Step Executor Migration changed Run columns or omitted frozen FK: %s", joined)
	}
}
