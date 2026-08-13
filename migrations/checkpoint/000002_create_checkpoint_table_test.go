package checkpoint

import (
	"strings"
	"testing"
)

func TestMigrationOwnsOnlyCheckpointTable(t *testing.T) {
	t.Parallel()
	definitions := Migrations()
	if len(definitions) != 1 || definitions[0].Version != 2 || definitions[0].Name != "create_checkpoint_table" {
		t.Fatalf("Migrations() = %#v", definitions)
	}
	joined := strings.ToLower(strings.Join(definitions[0].Statements, "\n"))
	if strings.Count(joined, "create table ") != 1 || !strings.Contains(joined, "create table checkpoint ") {
		t.Fatalf("Checkpoint migration creates unexpected tables: %s", joined)
	}
}
