package planner

import (
	"strings"
	"testing"
)

func TestMigrationOwnsOnlyPlanTableAndFrozenRunForeignKey(t *testing.T) {
	t.Parallel()
	definitions := Migrations()
	if len(definitions) != 1 || definitions[0].Version != 3 || definitions[0].Name != "create_plan_table" {
		t.Fatalf("Migrations() = %#v", definitions)
	}
	joined := strings.ToLower(strings.Join(definitions[0].Statements, "\n"))
	if strings.Count(joined, "create table ") != 1 || !strings.Contains(joined, "create table plan ") {
		t.Fatalf("Planner Migration creates unexpected tables: %s", joined)
	}
	if strings.Contains(joined, "add column") || !strings.Contains(joined, "add constraint run_plan_foreign_key") {
		t.Fatalf("Planner Migration changed Run columns or omitted frozen FK: %s", joined)
	}
}
