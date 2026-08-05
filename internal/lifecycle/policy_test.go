package lifecycle

import (
	"testing"
	"time"

	"github.com/zhaohaip/agentops-go/internal/contracts"
)

func TestTaskTransitions(t *testing.T) {
	t.Parallel()
	policy := New()
	legal := [][2]contracts.TaskStatus{
		{contracts.TaskStatusPending, contracts.TaskStatusRunning},
		{contracts.TaskStatusPending, contracts.TaskStatusInterrupted},
		{contracts.TaskStatusPending, contracts.TaskStatusCancelled},
		{contracts.TaskStatusPending, contracts.TaskStatusFailed},
		{contracts.TaskStatusRunning, contracts.TaskStatusWaitingApproval},
		{contracts.TaskStatusRunning, contracts.TaskStatusInterrupted},
		{contracts.TaskStatusRunning, contracts.TaskStatusCompleted},
		{contracts.TaskStatusRunning, contracts.TaskStatusFailed},
		{contracts.TaskStatusRunning, contracts.TaskStatusCancelled},
		{contracts.TaskStatusWaitingApproval, contracts.TaskStatusRunning},
		{contracts.TaskStatusWaitingApproval, contracts.TaskStatusCancelled},
		{contracts.TaskStatusWaitingApproval, contracts.TaskStatusFailed},
		{contracts.TaskStatusInterrupted, contracts.TaskStatusPending},
		{contracts.TaskStatusInterrupted, contracts.TaskStatusRunning},
		{contracts.TaskStatusInterrupted, contracts.TaskStatusCancelled},
		{contracts.TaskStatusInterrupted, contracts.TaskStatusFailed},
	}
	for _, transition := range legal {
		if decision := policy.CanTaskTransition(transition[0], transition[1]); !decision.Allowed {
			t.Errorf("CanTaskTransition(%s,%s) = %+v, want allowed", transition[0], transition[1], decision)
		}
	}
	for _, terminal := range []contracts.TaskStatus{
		contracts.TaskStatusCompleted, contracts.TaskStatusFailed, contracts.TaskStatusCancelled,
	} {
		for _, target := range []contracts.TaskStatus{
			contracts.TaskStatusPending, contracts.TaskStatusRunning, contracts.TaskStatusInterrupted,
		} {
			if decision := policy.CanTaskTransition(terminal, target); decision.Allowed || decision.Reason != RejectionInvalidState {
				t.Errorf("terminal transition %s->%s = %+v, want invalid state", terminal, target, decision)
			}
		}
	}
}

func TestExecutionTransitions(t *testing.T) {
	t.Parallel()
	policy := New()
	legal := [][2]contracts.TaskExecutionStatus{
		{contracts.TaskExecutionStatusQueued, contracts.TaskExecutionStatusRunning},
		{contracts.TaskExecutionStatusQueued, contracts.TaskExecutionStatusInterrupted},
		{contracts.TaskExecutionStatusQueued, contracts.TaskExecutionStatusFailed},
		{contracts.TaskExecutionStatusRunning, contracts.TaskExecutionStatusWaitingApproval},
		{contracts.TaskExecutionStatusRunning, contracts.TaskExecutionStatusCompleted},
		{contracts.TaskExecutionStatusRunning, contracts.TaskExecutionStatusFailed},
		{contracts.TaskExecutionStatusRunning, contracts.TaskExecutionStatusInterrupted},
		{contracts.TaskExecutionStatusWaitingApproval, contracts.TaskExecutionStatusQueued},
		{contracts.TaskExecutionStatusWaitingApproval, contracts.TaskExecutionStatusFailed},
		{contracts.TaskExecutionStatusInterrupted, contracts.TaskExecutionStatusFailed},
	}
	for _, transition := range legal {
		if decision := policy.CanExecutionTransition(transition[0], transition[1]); !decision.Allowed {
			t.Errorf("CanExecutionTransition(%s,%s) = %+v, want allowed", transition[0], transition[1], decision)
		}
	}
	illegal := [][2]contracts.TaskExecutionStatus{
		{contracts.TaskExecutionStatusQueued, contracts.TaskExecutionStatusCompleted},
		{contracts.TaskExecutionStatusWaitingApproval, contracts.TaskExecutionStatusRunning},
		{contracts.TaskExecutionStatusInterrupted, contracts.TaskExecutionStatusQueued},
		{contracts.TaskExecutionStatusCompleted, contracts.TaskExecutionStatusRunning},
	}
	for _, transition := range illegal {
		if decision := policy.CanExecutionTransition(transition[0], transition[1]); decision.Allowed || decision.Reason != RejectionInvalidState {
			t.Errorf("CanExecutionTransition(%s,%s) = %+v, want invalid state", transition[0], transition[1], decision)
		}
	}
}

func TestRunAndStepTransitions(t *testing.T) {
	t.Parallel()
	policy := New()
	runCases := []struct {
		from    contracts.RunStatus
		to      contracts.RunStatus
		allowed bool
	}{
		{contracts.RunStatusPending, contracts.RunStatusRunning, true},
		{contracts.RunStatusPending, contracts.RunStatusFailed, true},
		{contracts.RunStatusRunning, contracts.RunStatusWaitingApproval, true},
		{contracts.RunStatusRunning, contracts.RunStatusCompleted, true},
		{contracts.RunStatusWaitingApproval, contracts.RunStatusRunning, true},
		{contracts.RunStatusCompleted, contracts.RunStatusRunning, false},
		{contracts.RunStatusPending, contracts.RunStatusCompleted, false},
	}
	for _, test := range runCases {
		if got := policy.CanRunTransition(test.from, test.to); got.Allowed != test.allowed {
			t.Errorf("CanRunTransition(%s,%s) = %+v, allowed=%t", test.from, test.to, got, test.allowed)
		}
	}
	stepCases := []struct {
		from    contracts.StepStatus
		to      contracts.StepStatus
		allowed bool
	}{
		{contracts.StepStatusPending, contracts.StepStatusRunning, true},
		{contracts.StepStatusPending, contracts.StepStatusFailed, true},
		{contracts.StepStatusRunning, contracts.StepStatusWaitingApproval, true},
		{contracts.StepStatusRunning, contracts.StepStatusCompleted, true},
		{contracts.StepStatusRunning, contracts.StepStatusFailed, true},
		{contracts.StepStatusWaitingApproval, contracts.StepStatusRunning, true},
		{contracts.StepStatusWaitingApproval, contracts.StepStatusFailed, true},
		{contracts.StepStatusPending, contracts.StepStatusWaitingApproval, false},
		{contracts.StepStatusPending, contracts.StepStatusCompleted, false},
		{contracts.StepStatusCompleted, contracts.StepStatusRunning, false},
		{contracts.StepStatusCompleted, contracts.StepStatusFailed, false},
	}
	for _, test := range stepCases {
		if got := policy.CanStepTransition(test.from, test.to); got.Allowed != test.allowed {
			t.Errorf("CanStepTransition(%s,%s) = %+v, allowed=%t", test.from, test.to, got, test.allowed)
		}
	}
}

func TestPlannerCommittedPendingStepCanConvergeAtDeadline(t *testing.T) {
	t.Parallel()
	policy := New()
	deadline := time.Date(2026, time.January, 2, 3, 4, 5, 0, time.UTC)

	decision := policy.CheckGuard(GuardFacts{
		CurrentExecutionVersion: 1,
		RequestExecutionVersion: 1,
		DeadlineAt:              deadline,
		DatabaseNow:             deadline,
	})
	if decision.Allowed {
		t.Fatal("deadline guard unexpectedly allowed execution to continue")
	}
	if decision.Reason != RejectionDeadlineReached {
		t.Fatalf("deadline rejection reason = %q, want %q", decision.Reason, RejectionDeadlineReached)
	}

	transitions := []struct {
		name     string
		decision Decision
	}{
		{name: "task", decision: policy.CanTaskTransition(contracts.TaskStatusRunning, contracts.TaskStatusFailed)},
		{name: "run", decision: policy.CanRunTransition(contracts.RunStatusRunning, contracts.RunStatusFailed)},
		{name: "pending step", decision: policy.CanStepTransition(contracts.StepStatusPending, contracts.StepStatusFailed)},
		{name: "execution", decision: policy.CanExecutionTransition(contracts.TaskExecutionStatusRunning, contracts.TaskExecutionStatusFailed)},
	}
	for _, transition := range transitions {
		if !transition.decision.Allowed {
			t.Errorf("%s deadline convergence = %+v, want allowed", transition.name, transition.decision)
		}
	}
}

func TestGuardVersionWorkerAndDeadline(t *testing.T) {
	t.Parallel()
	policy := New()
	now := time.Date(2026, 8, 4, 10, 0, 0, 0, time.UTC)
	worker := contracts.WorkerID("worker-1")
	otherWorker := contracts.WorkerID("worker-2")
	base := GuardFacts{
		CurrentExecutionVersion: 2,
		RequestExecutionVersion: 2,
		ExecutionWorkerID:       &worker,
		RequestWorkerID:         &worker,
		DeadlineAt:              now.Add(time.Minute),
		DatabaseNow:             now,
	}
	if decision := policy.CheckGuard(base); !decision.Allowed {
		t.Fatalf("CheckGuard(valid) = %+v, want allowed", decision)
	}
	tests := map[string]struct {
		mutate func(*GuardFacts)
		reason RejectionReason
	}{
		"old version":       {func(facts *GuardFacts) { facts.RequestExecutionVersion = 1 }, RejectionVersionMismatch},
		"invalid version":   {func(facts *GuardFacts) { facts.CurrentExecutionVersion = 0 }, RejectionVersionMismatch},
		"worker mismatch":   {func(facts *GuardFacts) { facts.RequestWorkerID = &otherWorker }, RejectionWorkerMismatch},
		"missing ownership": {func(facts *GuardFacts) { facts.ExecutionWorkerID = nil }, RejectionWorkerMismatch},
		"at deadline":       {func(facts *GuardFacts) { facts.DatabaseNow = facts.DeadlineAt }, RejectionDeadlineReached},
		"after deadline":    {func(facts *GuardFacts) { facts.DatabaseNow = facts.DeadlineAt.Add(time.Nanosecond) }, RejectionDeadlineReached},
	}
	for name, test := range tests {
		name, test := name, test
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			facts := base
			test.mutate(&facts)
			if decision := policy.CheckGuard(facts); decision.Allowed || decision.Reason != test.reason {
				t.Fatalf("CheckGuard() = %+v, want %s", decision, test.reason)
			}
		})
	}
}

func TestApprovalLifecycleRules(t *testing.T) {
	t.Parallel()
	policy := New()
	now := time.Date(2026, 8, 4, 10, 0, 0, 0, time.UTC)
	worker := contracts.WorkerID("worker-1")
	running := ApprovalFacts{
		TaskStatus:              contracts.TaskStatusRunning,
		RunStatus:               contracts.RunStatusRunning,
		StepStatus:              contracts.StepStatusRunning,
		ExecutionStatus:         contracts.TaskExecutionStatusRunning,
		CurrentExecutionVersion: 3,
		RequestExecutionVersion: 3,
		ExecutionWorkerID:       &worker,
		RequestWorkerID:         &worker,
		DeadlineAt:              now.Add(time.Minute),
		DatabaseNow:             now,
	}
	if decision := policy.CanEnterWaitingApproval(running); !decision.Allowed {
		t.Fatalf("CanEnterWaitingApproval() = %+v, want allowed", decision)
	}
	if decision := policy.CanTerminalizeCheckpointInvalid(CheckpointInvalidSourceRequestApproval, running); !decision.Allowed {
		t.Fatalf("CanTerminalizeCheckpointInvalid(RequestApproval) = %+v, want allowed", decision)
	}

	waiting := running
	waiting.TaskStatus = contracts.TaskStatusWaitingApproval
	waiting.RunStatus = contracts.RunStatusWaitingApproval
	waiting.StepStatus = contracts.StepStatusWaitingApproval
	waiting.ExecutionStatus = contracts.TaskExecutionStatusWaitingApproval
	waiting.ApprovalStatus = contracts.ApprovalStatusPending
	waiting.ExecutionWorkerID = nil
	waiting.RequestWorkerID = nil
	if decision := policy.CanApprove(waiting); !decision.Allowed {
		t.Fatalf("CanApprove() = %+v, want allowed", decision)
	}
	if decision := policy.CanReject(waiting); !decision.Allowed {
		t.Fatalf("CanReject() = %+v, want allowed", decision)
	}
	for _, source := range []CheckpointInvalidSource{CheckpointInvalidSourceApprove, CheckpointInvalidSourceReject} {
		if decision := policy.CanTerminalizeCheckpointInvalid(source, waiting); !decision.Allowed {
			t.Errorf("CanTerminalizeCheckpointInvalid(%s) = %+v, want allowed", source, decision)
		}
	}
}

func TestApprovalLifecycleRejectsInvalidFacts(t *testing.T) {
	t.Parallel()
	policy := New()
	now := time.Date(2026, 8, 4, 10, 0, 0, 0, time.UTC)
	waiting := ApprovalFacts{
		TaskStatus:              contracts.TaskStatusWaitingApproval,
		RunStatus:               contracts.RunStatusWaitingApproval,
		StepStatus:              contracts.StepStatusWaitingApproval,
		ExecutionStatus:         contracts.TaskExecutionStatusWaitingApproval,
		ApprovalStatus:          contracts.ApprovalStatusPending,
		CurrentExecutionVersion: 2,
		RequestExecutionVersion: 2,
		DeadlineAt:              now.Add(time.Minute),
		DatabaseNow:             now,
	}
	tests := map[string]struct {
		mutate func(*ApprovalFacts)
		reason RejectionReason
	}{
		"state":    {func(facts *ApprovalFacts) { facts.StepStatus = contracts.StepStatusRunning }, RejectionInvalidState},
		"approval": {func(facts *ApprovalFacts) { facts.ApprovalStatus = contracts.ApprovalStatusApproved }, RejectionApprovalNotPending},
		"version":  {func(facts *ApprovalFacts) { facts.RequestExecutionVersion = 1 }, RejectionVersionMismatch},
		"deadline": {func(facts *ApprovalFacts) { facts.DatabaseNow = facts.DeadlineAt }, RejectionDeadlineReached},
		"worker retained": {func(facts *ApprovalFacts) {
			worker := contracts.WorkerID("worker")
			facts.ExecutionWorkerID = &worker
		}, RejectionWorkerMismatch},
	}
	for name, test := range tests {
		name, test := name, test
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			facts := waiting
			test.mutate(&facts)
			if decision := policy.CanApprove(facts); decision.Allowed || decision.Reason != test.reason {
				t.Fatalf("CanApprove() = %+v, want %s", decision, test.reason)
			}
		})
	}
	if decision := policy.CanTerminalizeCheckpointInvalid("Unknown", waiting); decision.Allowed || decision.Reason != RejectionInvalidSource {
		t.Fatalf("invalid source decision = %+v, want invalid source", decision)
	}
	runningWithoutWorker := waiting
	runningWithoutWorker.TaskStatus = contracts.TaskStatusRunning
	runningWithoutWorker.RunStatus = contracts.RunStatusRunning
	runningWithoutWorker.StepStatus = contracts.StepStatusRunning
	runningWithoutWorker.ExecutionStatus = contracts.TaskExecutionStatusRunning
	if decision := policy.CanEnterWaitingApproval(runningWithoutWorker); decision.Allowed || decision.Reason != RejectionWorkerMismatch {
		t.Fatalf("missing request worker decision = %+v, want worker mismatch", decision)
	}
}
