package contracts

import (
	"errors"
	"fmt"
	"testing"
)

func TestExecutionCancellationCauseIsClosedAndExtractable(t *testing.T) {
	causes := []ExecutionCancellationCause{
		ExecutionCancellationCauseActionTimeout,
		ExecutionCancellationCauseTaskCancelled,
		ExecutionCancellationCauseTaskTimedOut,
		ExecutionCancellationCauseLockLost,
		ExecutionCancellationCauseRuntimeShutdown,
	}
	for _, cause := range causes {
		if !cause.Valid() {
			t.Fatalf("cause %q is invalid", cause)
		}
		wrapped := fmt.Errorf("wrapped cancellation: %w", cause)
		got, ok := ExecutionCancellationCauseFrom(wrapped)
		if !ok || got != cause {
			t.Fatalf("ExecutionCancellationCauseFrom() = (%q, %v), want (%q, true)", got, ok, cause)
		}
	}

	if ExecutionCancellationCause("UNKNOWN").Valid() {
		t.Fatal("unknown cancellation cause is valid")
	}
	if got, ok := ExecutionCancellationCauseFrom(errors.New("ACTION_TIMEOUT")); ok || got != "" {
		t.Fatalf("plain error text was classified as (%q, %v)", got, ok)
	}
}
