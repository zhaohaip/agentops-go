package migrations

import "testing"

func TestPhaseZeroRegistryIsEmpty(t *testing.T) {
	t.Parallel()

	if got := All(); len(got) != 0 {
		t.Fatalf("All() returned %d migrations, want empty Phase 0 registry", len(got))
	}
}
