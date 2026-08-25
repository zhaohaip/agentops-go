package planner

import (
	"errors"
	"sync"
	"sync/atomic"
	"testing"
)

func TestSingleRepairGateAllowsZeroOrOneRepair(t *testing.T) {
	t.Parallel()

	gate := NewSingleRepairGate(NewPromptBuilder())
	if gate.Attempted() {
		t.Fatal("new gate reports a Repair attempt")
	}
	if decision := gate.Decide(nil); decision != RepairDecisionAccepted {
		t.Fatalf("zero-issue decision = %s", decision)
	}

	request := RepairPromptRequest{InitialPromptRequest: promptTestRequest()}
	messages, _, err := gate.Build(request)
	if err != nil || len(messages) != 1 {
		t.Fatalf("first Build() = (%#v, %v)", messages, err)
	}
	if !gate.Attempted() {
		t.Fatal("gate did not record the Repair attempt")
	}
	_, _, err = gate.Build(request)
	assertPromptError(t, err, PromptErrorRepairExhausted)
}

func TestSingleRepairGateConsumesFailedAttempt(t *testing.T) {
	t.Parallel()

	gate := NewSingleRepairGate(NewPromptBuilder())
	request := RepairPromptRequest{InitialPromptRequest: promptTestRequest()}
	request.AgentSystemPrompt = ""
	_, _, err := gate.Build(request)
	assertPromptError(t, err, PromptErrorRuntimeInvariantBroken)

	request.InitialPromptRequest = promptTestRequest()
	_, _, err = gate.Build(request)
	assertPromptError(t, err, PromptErrorRepairExhausted)
}

func TestSingleRepairGateIsAtomicForConcurrentCallers(t *testing.T) {
	t.Parallel()

	gate := NewSingleRepairGate(NewPromptBuilder())
	request := RepairPromptRequest{InitialPromptRequest: promptTestRequest()}
	var successes atomic.Int32
	var exhausted atomic.Int32
	var wait sync.WaitGroup
	for range 16 {
		wait.Add(1)
		go func() {
			defer wait.Done()
			_, _, err := gate.Build(request)
			if err == nil {
				successes.Add(1)
				return
			}
			var promptErr *PromptError
			if errors.As(err, &promptErr) && promptErr.Code == PromptErrorRepairExhausted {
				exhausted.Add(1)
			}
		}()
	}
	wait.Wait()
	if successes.Load() != 1 || exhausted.Load() != 15 {
		t.Fatalf("successes = %d, exhausted = %d", successes.Load(), exhausted.Load())
	}
}
