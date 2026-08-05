package taskruntime_test

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/zhaohaip/agentops-go/internal/contracts"
	"github.com/zhaohaip/agentops-go/internal/lifecycle"
	"github.com/zhaohaip/agentops-go/internal/taskruntime"
	"github.com/zhaohaip/agentops-go/internal/taskruntime/activecall"
)

func TestExecuteDispatchesAllFrozenNextActions(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name       string
		action     contracts.CheckpointNextAction
		planner    taskruntime.PlannerOutcome
		step       taskruntime.StepOutcome
		wantResult any
	}{
		{name: "GENERATE_PLAN", action: contracts.CheckpointNextActionGeneratePlan,
			planner:    taskruntime.PlannerOutcomeFailed{ErrorCode: contracts.ErrorCodePlanGenerationFailed},
			wantResult: contracts.ExecuteResultTerminal{}},
		{name: "EXECUTE_STEP", action: contracts.CheckpointNextActionExecuteStep,
			step: taskruntime.StepOutcomeStale{}, wantResult: contracts.ExecuteResultStale{}},
		{name: "REQUEST_APPROVAL", action: contracts.CheckpointNextActionRequestApproval,
			step: taskruntime.StepOutcomeWaitingApproval{}, wantResult: contracts.ExecuteResultWaitingApproval{}},
		{name: "EXECUTE_APPROVED_TOOL", action: contracts.CheckpointNextActionExecuteApprovedTool,
			step: taskruntime.StepOutcomeStale{}, wantResult: contracts.ExecuteResultStale{}},
		{name: "FINALIZE_RUN", action: contracts.CheckpointNextActionFinalizeRun,
			wantResult: contracts.ExecuteResultTerminal{}},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			harness := newExecuteHarness(t, test.action)
			if test.action == contracts.CheckpointNextActionRequestApproval {
				harness.steps.call = harness.waitingApprovalCall()
			}
			harness.planner.outcomes = []taskruntime.PlannerOutcome{test.planner}
			harness.steps.outcomes = []taskruntime.StepOutcome{test.step}
			result, err := harness.service.ExecuteClaimedExecution(context.Background(), harness.claim)
			if err != nil {
				t.Fatalf("ExecuteClaimedExecution() error = %v", err)
			}
			if resultType(result) != resultType(test.wantResult) {
				t.Fatalf("result = %T, want %T", result, test.wantResult)
			}
			if harness.planner.called.Load() != boolCount(test.action == contracts.CheckpointNextActionGeneratePlan) {
				t.Fatalf("Planner calls = %d", harness.planner.called.Load())
			}
			wantStep := test.action == contracts.CheckpointNextActionExecuteStep ||
				test.action == contracts.CheckpointNextActionRequestApproval ||
				test.action == contracts.CheckpointNextActionExecuteApprovedTool
			if harness.steps.called.Load() != boolCount(wantStep) {
				t.Fatalf("Step calls = %d", harness.steps.called.Load())
			}
		})
	}
}

func TestExecuteReloadsFactsAndRunsExternalCallsOutsideTransactions(t *testing.T) {
	t.Parallel()
	harness := newExecuteHarness(t, contracts.CheckpointNextActionGeneratePlan)
	config := harness.config.ExecutionConfig
	readTool := config.ToolFramework.Tools[0]
	readTool.Enabled = true
	readTool.RiskLevel = contracts.RiskLevelLow
	readTool.ReadOnly = true
	harness.config.ExecutionConfig.ToolFramework.Tools[0] = readTool
	harness.resetHash(t)
	harness.planner.outcomes = []taskruntime.PlannerOutcome{taskruntime.PlannerOutcomeCompleted{Draft: taskruntime.ValidatedPlanDraft{
		PlanID: "plan-1", Goal: "goal", Steps: []taskruntime.PlanStepDraft{
			{StepID: "step-1", Sequence: 1, Type: contracts.StepTypeModelCall},
			{StepID: "step-2", Sequence: 2, Type: contracts.StepTypeToolCall, ToolName: readTool.Name},
		},
	}}}
	harness.steps.outcomes = []taskruntime.StepOutcome{
		taskruntime.StepOutcomeCompleted{
			Continuation: contracts.StepContinuationNextStep, NextStepID: "step-2",
		},
		taskruntime.StepOutcomeCompleted{Continuation: contracts.StepContinuationFinalizeRun},
	}
	result, err := harness.service.ExecuteClaimedExecution(context.Background(), harness.claim)
	if err != nil {
		t.Fatalf("ExecuteClaimedExecution() error = %v", err)
	}
	if _, ok := result.(contracts.ExecuteResultTerminal); !ok {
		t.Fatalf("result = %T, want Terminal", result)
	}
	if harness.executor.externalInsideTransaction.Load() {
		t.Fatal("Planner or Step Executor was called inside a write transaction")
	}
	if harness.dispatch.lockCalls.Load() < 7 {
		t.Fatalf("dispatch reload calls = %d, want at least one before every action and result", harness.dispatch.lockCalls.Load())
	}
	if harness.planner.called.Load() != 1 || harness.steps.called.Load() != 2 {
		t.Fatalf("external calls = Planner:%d Step:%d, want 1/2", harness.planner.called.Load(), harness.steps.called.Load())
	}
	state := harness.dispatch.snapshot()
	if !state.facts.Task.Status.Terminal() || !state.facts.Run.Status.Terminal() || !state.facts.Execution.Status.Ended() {
		t.Fatalf("final facts are not terminal: Task=%s Run=%s Execution=%s",
			state.facts.Task.Status, state.facts.Run.Status, state.facts.Execution.Status)
	}
}

func TestExecuteWaitingApprovalReleasesSlotWithoutRuntimeWrite(t *testing.T) {
	t.Parallel()
	harness := newExecuteHarness(t, contracts.CheckpointNextActionRequestApproval)
	harness.steps.call = harness.waitingApprovalCall()
	result, err := harness.service.ExecuteClaimedExecution(context.Background(), harness.claim)
	if err != nil {
		t.Fatalf("ExecuteClaimedExecution() error = %v", err)
	}
	if _, ok := result.(contracts.ExecuteResultWaitingApproval); !ok {
		t.Fatalf("result = %T, want WaitingApproval", result)
	}
	if harness.dispatch.applyStepCalls.Load() != 0 || harness.dispatch.terminalizeCalls.Load() != 0 {
		t.Fatal("Task Runtime duplicated Approval Manager persistence")
	}
	if _, exists := harness.registry.State(activeKey(harness.claim)); exists {
		t.Fatal("WaitingApproval retained Active Call slot")
	}
}

func TestExecuteValidatesFrozenActionStepToolAndApprovalContext(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name       string
		action     contracts.CheckpointNextAction
		configure  func(*testing.T, *executeHarness)
		wantReason contracts.ReasonCode
		wantError  bool
	}{
		{
			name: "EXECUTE_STEP rejects approval context", action: contracts.CheckpointNextActionExecuteStep,
			configure: func(_ *testing.T, h *executeHarness) {
				h.dispatch.mutate(func(state *executeState) {
					state.checkpoint.ApprovalContext = validApprovalContext(h.config.ExecutionConfig.ToolFramework.Tools[0].Name)
				})
			},
			wantReason: contracts.ReasonCodeCheckpointApprovalReferenceInvalid,
		},
		{
			name: "EXECUTE_STEP rejects high write Tool", action: contracts.CheckpointNextActionExecuteStep,
			configure: func(t *testing.T, h *executeHarness) {
				tool := h.config.ExecutionConfig.ToolFramework.Tools[0]
				tool.Enabled, tool.RiskLevel, tool.ReadOnly = true, contracts.RiskLevelHigh, false
				h.config.ExecutionConfig.ToolFramework.Tools[0] = tool
				h.dispatch.mutate(func(state *executeState) {
					state.facts.Step.Type, state.facts.Step.ToolName = contracts.StepTypeToolCall, tool.Name
				})
				h.resetHash(t)
			},
			wantReason: contracts.ReasonCodeCheckpointFrozenActionMismatch,
		},
		{
			name: "REQUEST_APPROVAL rejects Model Step", action: contracts.CheckpointNextActionRequestApproval,
			configure: func(_ *testing.T, h *executeHarness) {
				h.dispatch.mutate(func(state *executeState) {
					state.facts.Step.Type, state.facts.Step.ToolName = contracts.StepTypeModelCall, ""
				})
			},
			wantReason: contracts.ReasonCodeCheckpointFrozenActionMismatch,
		},
		{
			name: "REQUEST_APPROVAL rejects low read Tool", action: contracts.CheckpointNextActionRequestApproval,
			configure: func(t *testing.T, h *executeHarness) {
				tool := h.config.ExecutionConfig.ToolFramework.Tools[0]
				tool.RiskLevel, tool.ReadOnly = contracts.RiskLevelLow, true
				h.config.ExecutionConfig.ToolFramework.Tools[0] = tool
				h.resetHash(t)
			},
			wantReason: contracts.ReasonCodeCheckpointFrozenActionMismatch,
		},
		{
			name: "REQUEST_APPROVAL rejects existing approval", action: contracts.CheckpointNextActionRequestApproval,
			configure: func(_ *testing.T, h *executeHarness) {
				h.dispatch.mutate(func(state *executeState) {
					state.checkpoint.ApprovalContext = validApprovalContext(state.facts.Step.ToolName)
				})
			},
			wantReason: contracts.ReasonCodeCheckpointApprovalReferenceInvalid,
		},
		{
			name: "EXECUTE_APPROVED_TOOL rejects Model Step", action: contracts.CheckpointNextActionExecuteApprovedTool,
			configure: func(_ *testing.T, h *executeHarness) {
				h.dispatch.mutate(func(state *executeState) {
					state.facts.Step.Type, state.facts.Step.ToolName = contracts.StepTypeModelCall, ""
				})
			},
			wantReason: contracts.ReasonCodeCheckpointFrozenActionMismatch,
		},
		{
			name: "EXECUTE_APPROVED_TOOL rejects low read Tool", action: contracts.CheckpointNextActionExecuteApprovedTool,
			configure: func(t *testing.T, h *executeHarness) {
				tool := h.config.ExecutionConfig.ToolFramework.Tools[0]
				tool.RiskLevel, tool.ReadOnly = contracts.RiskLevelLow, true
				h.config.ExecutionConfig.ToolFramework.Tools[0] = tool
				h.resetHash(t)
			},
			wantReason: contracts.ReasonCodeCheckpointFrozenActionMismatch,
		},
		{
			name: "EXECUTE_APPROVED_TOOL rejects missing approval", action: contracts.CheckpointNextActionExecuteApprovedTool,
			configure: func(_ *testing.T, h *executeHarness) {
				h.dispatch.mutate(func(state *executeState) { state.checkpoint.ApprovalContext = nil })
			},
			wantReason: contracts.ReasonCodeCheckpointApprovalReferenceInvalid,
		},
		{
			name: "EXECUTE_APPROVED_TOOL rejects mismatched approval Tool", action: contracts.CheckpointNextActionExecuteApprovedTool,
			configure: func(_ *testing.T, h *executeHarness) {
				h.dispatch.mutate(func(state *executeState) { state.checkpoint.ApprovalContext.ToolName = "other-tool" })
			},
			wantReason: contracts.ReasonCodeCheckpointApprovalReferenceInvalid,
		},
		{
			name: "unsupported static Tool capability is fatal", action: contracts.CheckpointNextActionExecuteStep,
			configure: func(t *testing.T, h *executeHarness) {
				tool := h.config.ExecutionConfig.ToolFramework.Tools[0]
				tool.Enabled, tool.RiskLevel, tool.ReadOnly = true, contracts.RiskLevelHigh, true
				h.config.ExecutionConfig.ToolFramework.Tools[0] = tool
				h.dispatch.mutate(func(state *executeState) {
					state.facts.Step.Type, state.facts.Step.ToolName = contracts.StepTypeToolCall, tool.Name
				})
				h.resetHash(t)
			},
			wantError: true,
		},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			harness := newExecuteHarness(t, test.action)
			test.configure(t, harness)
			result, err := harness.service.ExecuteClaimedExecution(context.Background(), harness.claim)
			if test.wantError {
				if !errors.Is(err, taskruntime.ErrPersistenceInvariantViolation) {
					t.Fatalf("error = %v, want persistence invariant violation", err)
				}
				if result != nil {
					t.Fatalf("result = %T, want nil", result)
				}
			} else {
				if err != nil {
					t.Fatalf("ExecuteClaimedExecution() error = %v", err)
				}
				if _, ok := result.(contracts.ExecuteResultTerminal); !ok {
					t.Fatalf("result = %T, want Terminal", result)
				}
				state := harness.dispatch.snapshot()
				if state.checkpointInvalidReason != test.wantReason || state.pendingReports != 1 {
					t.Fatalf("reason/report = %s/%d, want %s/1", state.checkpointInvalidReason, state.pendingReports, test.wantReason)
				}
			}
			if harness.steps.called.Load() != 0 {
				t.Fatalf("Step calls = %d, want 0", harness.steps.called.Load())
			}
		})
	}
}

func TestExecuteAcceptsFrozenStepActionCombinations(t *testing.T) {
	t.Parallel()
	for _, stepType := range []contracts.StepType{
		contracts.StepTypeAnalysis, contracts.StepTypeModelCall, contracts.StepTypeVerification, contracts.StepTypeToolCall,
	} {
		stepType := stepType
		t.Run(string(stepType), func(t *testing.T) {
			t.Parallel()
			harness := newExecuteHarness(t, contracts.CheckpointNextActionExecuteStep)
			if stepType == contracts.StepTypeToolCall {
				tool := harness.config.ExecutionConfig.ToolFramework.Tools[0]
				harness.dispatch.mutate(func(state *executeState) {
					state.facts.Step.Type, state.facts.Step.ToolName = stepType, tool.Name
				})
			} else {
				harness.dispatch.mutate(func(state *executeState) { state.facts.Step.Type = stepType })
			}
			harness.steps.outcomes = []taskruntime.StepOutcome{taskruntime.StepOutcomeStale{}}
			result, err := harness.service.ExecuteClaimedExecution(context.Background(), harness.claim)
			if err != nil {
				t.Fatalf("ExecuteClaimedExecution() error = %v", err)
			}
			if _, ok := result.(contracts.ExecuteResultStale); !ok || harness.steps.called.Load() != 1 {
				t.Fatalf("result/calls = %T/%d, want Stale/1", result, harness.steps.called.Load())
			}
		})
	}
}

func TestExecuteApprovedToolRejectsMalformedApprovalContext(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		mutate func(*contracts.ApprovalContext)
	}{
		{name: "empty approval id", mutate: func(value *contracts.ApprovalContext) { value.ApprovalID = "" }},
		{name: "invalid approval version", mutate: func(value *contracts.ApprovalContext) { value.ApprovalExecutionVersion = 0 }},
		{name: "future approval version", mutate: func(value *contracts.ApprovalContext) { value.ApprovalExecutionVersion = 2 }},
		{name: "empty resource version", mutate: func(value *contracts.ApprovalContext) { value.ResourceVersion = "" }},
		{name: "invalid frozen input hash", mutate: func(value *contracts.ApprovalContext) { value.FrozenInputHash = "bad" }},
		{name: "invalid frozen Tool input", mutate: func(value *contracts.ApprovalContext) { value.FrozenToolInput = []byte("{") }},
		{name: "invalid observed values", mutate: func(value *contracts.ApprovalContext) { value.ObservedValues = []byte("{") }},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			harness := newExecuteHarness(t, contracts.CheckpointNextActionExecuteApprovedTool)
			harness.dispatch.mutate(func(state *executeState) { test.mutate(state.checkpoint.ApprovalContext) })
			result, err := harness.service.ExecuteClaimedExecution(context.Background(), harness.claim)
			if err != nil {
				t.Fatalf("ExecuteClaimedExecution() error = %v", err)
			}
			if _, ok := result.(contracts.ExecuteResultTerminal); !ok {
				t.Fatalf("result = %T, want Terminal", result)
			}
			state := harness.dispatch.snapshot()
			if state.checkpointInvalidReason != contracts.ReasonCodeCheckpointApprovalReferenceInvalid ||
				harness.steps.called.Load() != 0 {
				t.Fatalf("reason/calls = %s/%d, want approval-reference-invalid/0",
					state.checkpointInvalidReason, harness.steps.called.Load())
			}
		})
	}
}

func TestExecuteRejectsOutcomeInconsistentWithFrozenAction(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		action  contracts.CheckpointNextAction
		outcome taskruntime.StepOutcome
	}{
		{name: "EXECUTE_STEP WaitingApproval", action: contracts.CheckpointNextActionExecuteStep, outcome: taskruntime.StepOutcomeWaitingApproval{}},
		{name: "EXECUTE_STEP Terminalized", action: contracts.CheckpointNextActionExecuteStep, outcome: taskruntime.StepOutcomeTerminalized{}},
		{name: "REQUEST_APPROVAL Completed", action: contracts.CheckpointNextActionRequestApproval, outcome: taskruntime.StepOutcomeCompleted{}},
		{name: "EXECUTE_APPROVED_TOOL WaitingApproval", action: contracts.CheckpointNextActionExecuteApprovedTool, outcome: taskruntime.StepOutcomeWaitingApproval{}},
		{name: "EXECUTE_APPROVED_TOOL Terminalized", action: contracts.CheckpointNextActionExecuteApprovedTool, outcome: taskruntime.StepOutcomeTerminalized{}},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			harness := newExecuteHarness(t, test.action)
			harness.steps.outcomes = []taskruntime.StepOutcome{test.outcome}
			result, err := harness.service.ExecuteClaimedExecution(context.Background(), harness.claim)
			if !errors.Is(err, taskruntime.ErrPersistenceInvariantViolation) {
				t.Fatalf("error = %v, want persistence invariant violation", err)
			}
			if result != nil || harness.dispatch.applyStepCalls.Load() != 0 || harness.dispatch.terminalizeCalls.Load() != 0 {
				t.Fatalf("result/apply/terminalize = %T/%d/%d, want nil/0/0", result,
					harness.dispatch.applyStepCalls.Load(), harness.dispatch.terminalizeCalls.Load())
			}
		})
	}
}

func TestExecuteWaitingApprovalRequiresCompletePersistedScene(t *testing.T) {
	t.Parallel()
	harness := newExecuteHarness(t, contracts.CheckpointNextActionRequestApproval)
	harness.steps.outcomes = []taskruntime.StepOutcome{taskruntime.StepOutcomeWaitingApproval{}}
	result, err := harness.service.ExecuteClaimedExecution(context.Background(), harness.claim)
	if !errors.Is(err, taskruntime.ErrPersistenceInvariantViolation) {
		t.Fatalf("error = %v, want persistence invariant violation", err)
	}
	if result != nil {
		t.Fatalf("result = %T, want nil", result)
	}
	state := harness.dispatch.snapshot()
	if state.facts.Task.Status != contracts.TaskStatusRunning || state.approvalPending || state.waitingCheckpoint {
		t.Fatalf("incomplete waiting scene was accepted: %+v", state)
	}
}

func TestExecuteWaitingApprovalRejectsPartialPersistedScene(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		mutate func(*executeState)
	}{
		{name: "missing Pending Approval", mutate: func(state *executeState) { state.approvalPending = false }},
		{name: "missing waiting Checkpoint", mutate: func(state *executeState) { state.waitingCheckpoint = false }},
		{name: "worker retained", mutate: func(state *executeState) {
			workerID := contracts.WorkerID("worker-1")
			state.facts.Execution.WorkerID = &workerID
		}},
		{name: "Task not waiting", mutate: func(state *executeState) { state.facts.Task.Status = contracts.TaskStatusRunning }},
		{name: "Run not waiting", mutate: func(state *executeState) { state.facts.Run.Status = contracts.RunStatusRunning }},
		{name: "Execution not waiting", mutate: func(state *executeState) {
			state.facts.Execution.Status = contracts.TaskExecutionStatusRunning
		}},
		{name: "Step not waiting", mutate: func(state *executeState) { state.facts.Step.Status = contracts.StepStatusRunning }},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			harness := newExecuteHarness(t, contracts.CheckpointNextActionRequestApproval)
			harness.steps.call = func(context.Context, taskruntime.StepExecutionRequest) (taskruntime.StepOutcome, error) {
				harness.commitWaitingApproval()
				harness.dispatch.mutate(test.mutate)
				return taskruntime.StepOutcomeWaitingApproval{}, nil
			}
			result, err := harness.service.ExecuteClaimedExecution(context.Background(), harness.claim)
			if !errors.Is(err, taskruntime.ErrPersistenceInvariantViolation) {
				t.Fatalf("error = %v, want persistence invariant violation", err)
			}
			if result != nil {
				t.Fatalf("result = %T, want nil", result)
			}
		})
	}
}

func TestExecuteDropsLatePlannerResultAfterVersionGuardChanges(t *testing.T) {
	t.Parallel()
	harness := newExecuteHarness(t, contracts.CheckpointNextActionGeneratePlan)
	entered := make(chan struct{})
	release := make(chan struct{})
	harness.planner.call = func(context.Context, taskruntime.PlannerRequest) (taskruntime.PlannerOutcome, error) {
		close(entered)
		<-release
		return taskruntime.PlannerOutcomeCompleted{Draft: validPlanDraft()}, nil
	}
	done := make(chan struct {
		result contracts.ExecuteResult
		err    error
	}, 1)
	go func() {
		result, err := harness.service.ExecuteClaimedExecution(context.Background(), harness.claim)
		done <- struct {
			result contracts.ExecuteResult
			err    error
		}{result, err}
	}()
	<-entered
	harness.dispatch.mutate(func(state *executeState) {
		state.facts.Task.CurrentExecutionVersion = 2
	})
	close(release)
	outcome := <-done
	if outcome.err != nil {
		t.Fatalf("ExecuteClaimedExecution() error = %v", outcome.err)
	}
	if _, ok := outcome.result.(contracts.ExecuteResultStale); !ok {
		t.Fatalf("result = %T, want Stale", outcome.result)
	}
	if harness.dispatch.applyPlannerCalls.Load() != 0 {
		t.Fatalf("late Planner apply calls = %d, want 0", harness.dispatch.applyPlannerCalls.Load())
	}
	if harness.dispatch.snapshot().facts.Plan != nil {
		t.Fatal("late Planner result persisted a Plan")
	}
	if harness.dispatch.checkpointInvalidCalls.Load() != 0 ||
		harness.dispatch.snapshot().facts.Task.Status != contracts.TaskStatusRunning {
		t.Fatal("Version Guard competition was incorrectly terminalized as CheckpointInvalid")
	}
}

func TestExecutePreparedCancellationPreventsExternalCall(t *testing.T) {
	t.Parallel()
	harness := newExecuteHarness(t, contracts.CheckpointNextActionExecuteStep)
	startEntered := make(chan struct{})
	startRelease := make(chan struct{})
	harness.dispatch.startHook = func() {
		close(startEntered)
		<-startRelease
	}
	done := make(chan struct {
		result contracts.ExecuteResult
		err    error
	}, 1)
	go func() {
		result, err := harness.service.ExecuteClaimedExecution(context.Background(), harness.claim)
		done <- struct {
			result contracts.ExecuteResult
			err    error
		}{result, err}
	}()
	<-startEntered
	if state, ok := harness.registry.State(activeKey(harness.claim)); !ok || state != activecall.StatePrepared {
		t.Fatalf("Active Call state = %q, %v; want PREPARED", state, ok)
	}
	if cancelled, err := harness.registry.Cancel(activeKey(harness.claim), activecall.CauseTaskCancelled); err != nil || !cancelled {
		t.Fatalf("Cancel() = %v, %v", cancelled, err)
	}
	close(startRelease)
	outcome := <-done
	if outcome.err != nil {
		t.Fatalf("ExecuteClaimedExecution() error = %v", outcome.err)
	}
	if _, ok := outcome.result.(contracts.ExecuteResultStale); !ok {
		t.Fatalf("result = %T, want Stale", outcome.result)
	}
	if harness.steps.called.Load() != 0 {
		t.Fatal("Step Executor called after PREPARED cancellation")
	}
}

func TestExecuteActionGuardMissPreventsExternalCall(t *testing.T) {
	t.Parallel()
	harness := newExecuteHarness(t, contracts.CheckpointNextActionExecuteApprovedTool)
	harness.dispatch.startMiss.Store(true)
	result, err := harness.service.ExecuteClaimedExecution(context.Background(), harness.claim)
	if err != nil {
		t.Fatalf("ExecuteClaimedExecution() error = %v", err)
	}
	if _, ok := result.(contracts.ExecuteResultStale); !ok {
		t.Fatalf("result = %T, want Stale", result)
	}
	if harness.steps.called.Load() != 0 {
		t.Fatal("Step Executor called after action Guard missed")
	}
}

func TestExecutePropagatesSystemErrorAndRegistryCancellation(t *testing.T) {
	t.Parallel()
	systemErr := errors.New("step infrastructure unavailable")
	t.Run("system error", func(t *testing.T) {
		t.Parallel()
		harness := newExecuteHarness(t, contracts.CheckpointNextActionExecuteStep)
		harness.steps.call = func(context.Context, taskruntime.StepExecutionRequest) (taskruntime.StepOutcome, error) {
			return nil, systemErr
		}
		result, err := harness.service.ExecuteClaimedExecution(context.Background(), harness.claim)
		if result != nil || !errors.Is(err, systemErr) {
			t.Fatalf("ExecuteClaimedExecution() = %T, %v; want system error", result, err)
		}
	})
	t.Run("active cancellation", func(t *testing.T) {
		t.Parallel()
		harness := newExecuteHarness(t, contracts.CheckpointNextActionExecuteStep)
		entered := make(chan context.Context, 1)
		harness.steps.call = func(ctx context.Context, _ taskruntime.StepExecutionRequest) (taskruntime.StepOutcome, error) {
			entered <- ctx
			<-ctx.Done()
			return nil, ctx.Err()
		}
		done := make(chan struct {
			result contracts.ExecuteResult
			err    error
		}, 1)
		go func() {
			result, err := harness.service.ExecuteClaimedExecution(context.Background(), harness.claim)
			done <- struct {
				result contracts.ExecuteResult
				err    error
			}{result, err}
		}()
		callContext := <-entered
		if err := harness.registry.CancelAll(activecall.CauseRuntimeShutdown); err != nil {
			t.Fatalf("CancelAll() error = %v", err)
		}
		outcome := <-done
		if outcome.err != nil {
			t.Fatalf("ExecuteClaimedExecution() error = %v", outcome.err)
		}
		if _, ok := outcome.result.(contracts.ExecuteResultStale); !ok {
			t.Fatalf("result = %T, want Stale", outcome.result)
		}
		if !errors.Is(context.Cause(callContext), activecall.CauseRuntimeShutdown) {
			t.Fatalf("external context cause = %v", context.Cause(callContext))
		}
	})
}

func TestExecuteBuildsScopeFromVerifiedPersistentHash(t *testing.T) {
	t.Parallel()
	harness := newExecuteHarness(t, contracts.CheckpointNextActionExecuteStep)
	harness.steps.call = func(_ context.Context, request taskruntime.StepExecutionRequest) (taskruntime.StepOutcome, error) {
		state := harness.dispatch.snapshot()
		if request.Scope.ExecutionConfigHash != state.facts.Execution.ExecutionConfigHash ||
			request.Scope.TaskID != harness.claim.TaskID || request.Scope.RunID != harness.claim.RunID ||
			request.Scope.ExecutionVersion != harness.claim.ExecutionVersion || request.Scope.WorkerID != harness.claim.WorkerID ||
			request.Scope.StepID != state.facts.Step.StepID || request.Scope.DeadlineAt != state.facts.Task.DeadlineAt {
			t.Fatalf("ExecutionScope = %+v, want verified persisted facts", request.Scope)
		}
		return taskruntime.StepOutcomeStale{}, nil
	}
	result, err := harness.service.ExecuteClaimedExecution(context.Background(), harness.claim)
	if err != nil {
		t.Fatalf("ExecuteClaimedExecution() error = %v", err)
	}
	if _, ok := result.(contracts.ExecuteResultStale); !ok {
		t.Fatalf("result = %T, want Stale", result)
	}
}

func TestExecutePlannerReceivesFrozenCatalogSelector(t *testing.T) {
	t.Parallel()
	harness := newExecuteHarness(t, contracts.CheckpointNextActionGeneratePlan)
	harness.planner.call = func(_ context.Context, request taskruntime.PlannerRequest) (taskruntime.PlannerOutcome, error) {
		want := harness.config.PlanningToolCatalogSelector
		if request.ToolCatalogSelector.CatalogID != want.CatalogID ||
			request.ToolCatalogSelector.ExpectedRegistryVersion != want.ExpectedRegistryVersion ||
			request.ToolCatalogSelector.ExpectedSnapshotHash != want.ExpectedSnapshotHash ||
			len(request.ToolCatalogSelector.AllowedTools) != len(want.AllowedTools) {
			t.Fatalf("Planner selector = %+v, want %+v", request.ToolCatalogSelector, want)
		}
		return taskruntime.PlannerOutcomeFailed{ErrorCode: contracts.ErrorCodePlanGenerationFailed}, nil
	}
	result, err := harness.service.ExecuteClaimedExecution(context.Background(), harness.claim)
	if err != nil {
		t.Fatalf("ExecuteClaimedExecution() error = %v", err)
	}
	if _, ok := result.(contracts.ExecuteResultTerminal); !ok {
		t.Fatalf("result = %T, want Terminal", result)
	}
}

func TestExecutePersistsHighWriteToolAsRequestApproval(t *testing.T) {
	t.Parallel()
	for _, source := range []string{"Planner first Step", "completed Step successor"} {
		source := source
		t.Run(source, func(t *testing.T) {
			t.Parallel()
			action := contracts.CheckpointNextActionGeneratePlan
			if source == "completed Step successor" {
				action = contracts.CheckpointNextActionExecuteStep
			}
			harness := newExecuteHarness(t, action)
			tool := harness.config.ExecutionConfig.ToolFramework.Tools[0]
			tool.Enabled = true
			tool.RiskLevel = contracts.RiskLevelHigh
			tool.ReadOnly = false
			harness.config.ExecutionConfig.ToolFramework.Tools[0] = tool
			harness.resetHash(t)

			if source == "Planner first Step" {
				harness.planner.outcomes = []taskruntime.PlannerOutcome{taskruntime.PlannerOutcomeCompleted{Draft: taskruntime.ValidatedPlanDraft{
					PlanID: "plan-high", Goal: "approve",
					Steps: []taskruntime.PlanStepDraft{{
						StepID: "step-high", Sequence: 1, Type: contracts.StepTypeToolCall, ToolName: tool.Name,
					}},
				}}}
			} else {
				harness.dispatch.mutate(func(state *executeState) {
					state.steps = map[contracts.StepID]taskruntime.ExecutionStep{
						"step-high": {
							StepID: "step-high", Sequence: 2, Type: contracts.StepTypeToolCall,
							Status: contracts.StepStatusPending, ToolName: tool.Name,
						},
					}
				})
			}
			var stepCalls atomic.Int64
			harness.steps.call = func(_ context.Context, request taskruntime.StepExecutionRequest) (taskruntime.StepOutcome, error) {
				call := stepCalls.Add(1)
				if source == "completed Step successor" && call == 1 {
					return taskruntime.StepOutcomeCompleted{
						Continuation: contracts.StepContinuationNextStep, NextStepID: "step-high",
					}, nil
				}
				if request.NextAction != contracts.CheckpointNextActionRequestApproval {
					t.Fatalf("next action = %s, want REQUEST_APPROVAL", request.NextAction)
				}
				harness.commitWaitingApproval()
				return taskruntime.StepOutcomeWaitingApproval{}, nil
			}
			result, err := harness.service.ExecuteClaimedExecution(context.Background(), harness.claim)
			if err != nil {
				t.Fatalf("ExecuteClaimedExecution() error = %v", err)
			}
			if _, ok := result.(contracts.ExecuteResultWaitingApproval); !ok {
				t.Fatalf("result = %T, want WaitingApproval", result)
			}
			if harness.dispatch.snapshot().checkpoint.NextAction != contracts.CheckpointNextActionRequestApproval {
				t.Fatalf("persisted next action = %s", harness.dispatch.snapshot().checkpoint.NextAction)
			}
		})
	}
}

func TestExecuteTerminalizedOutcomeRequiresPersistedTerminalFacts(t *testing.T) {
	t.Parallel()
	for _, terminal := range []bool{true, false} {
		terminal := terminal
		t.Run(map[bool]string{true: "confirmed", false: "not-confirmed"}[terminal], func(t *testing.T) {
			t.Parallel()
			harness := newExecuteHarness(t, contracts.CheckpointNextActionRequestApproval)
			harness.steps.call = func(context.Context, taskruntime.StepExecutionRequest) (taskruntime.StepOutcome, error) {
				if terminal {
					harness.dispatch.mutate(func(state *executeState) {
						state.facts.Task.Status = contracts.TaskStatusFailed
						state.facts.Run.Status = contracts.RunStatusFailed
						state.facts.Execution.Status = contracts.TaskExecutionStatusFailed
					})
				}
				return taskruntime.StepOutcomeTerminalized{}, nil
			}
			result, err := harness.service.ExecuteClaimedExecution(context.Background(), harness.claim)
			if terminal {
				if err != nil {
					t.Fatalf("ExecuteClaimedExecution() error = %v", err)
				}
				if _, ok := result.(contracts.ExecuteResultTerminal); !ok {
					t.Fatalf("result = %T, want Terminal", result)
				}
			} else if !errors.Is(err, taskruntime.ErrPersistenceInvariantViolation) {
				t.Fatalf("error = %v, want persistence invariant violation", err)
			}
			if harness.dispatch.terminalizeCalls.Load() != 0 {
				t.Fatal("Task Runtime duplicated Terminalized persistence")
			}
		})
	}
}

func TestExecuteResultCheckpointInvalidTerminalizesCurrentExecution(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name           string
		action         contracts.CheckpointNextAction
		expectStepFail bool
	}{
		{name: "Planner result", action: contracts.CheckpointNextActionGeneratePlan},
		{name: "Step result", action: contracts.CheckpointNextActionExecuteStep, expectStepFail: true},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			harness := newExecuteHarness(t, test.action)
			entered := make(chan struct{})
			release := make(chan struct{})
			if test.action == contracts.CheckpointNextActionGeneratePlan {
				harness.planner.call = func(context.Context, taskruntime.PlannerRequest) (taskruntime.PlannerOutcome, error) {
					close(entered)
					<-release
					return taskruntime.PlannerOutcomeCompleted{Draft: validPlanDraft()}, nil
				}
			} else {
				harness.steps.call = func(context.Context, taskruntime.StepExecutionRequest) (taskruntime.StepOutcome, error) {
					close(entered)
					<-release
					return taskruntime.StepOutcomeCompleted{
						Continuation: contracts.StepContinuationFinalizeRun,
					}, nil
				}
			}

			done := make(chan struct {
				result contracts.ExecuteResult
				err    error
			}, 1)
			go func() {
				result, err := harness.service.ExecuteClaimedExecution(context.Background(), harness.claim)
				done <- struct {
					result contracts.ExecuteResult
					err    error
				}{result: result, err: err}
			}()
			<-entered
			harness.checkpoints.reason = contracts.ReasonCodeCheckpointNextActionInvalid
			harness.checkpoints.invalid.Store(true)
			close(release)
			outcome := <-done
			if outcome.err != nil {
				t.Fatalf("ExecuteClaimedExecution() error = %v", outcome.err)
			}
			if _, ok := outcome.result.(contracts.ExecuteResultTerminal); !ok {
				t.Fatalf("result = %T, want Terminal", outcome.result)
			}
			assertCheckpointInvalidTerminalState(
				t, harness, contracts.ReasonCodeCheckpointNextActionInvalid, test.expectStepFail,
			)
			if harness.dispatch.applyPlannerCalls.Load() != 0 || harness.dispatch.applyStepCalls.Load() != 0 {
				t.Fatalf("invalid result was applied: Planner=%d Step=%d",
					harness.dispatch.applyPlannerCalls.Load(), harness.dispatch.applyStepCalls.Load())
			}
		})
	}
}

func TestExecuteCheckpointInvalidTerminalizesWithoutExternalCall(t *testing.T) {
	t.Parallel()
	harness := newExecuteHarness(t, contracts.CheckpointNextActionExecuteStep)
	harness.checkpoints.invalid.Store(true)
	result, err := harness.service.ExecuteClaimedExecution(context.Background(), harness.claim)
	if err != nil {
		t.Fatalf("ExecuteClaimedExecution() error = %v", err)
	}
	if _, ok := result.(contracts.ExecuteResultTerminal); !ok {
		t.Fatalf("result = %T, want Terminal", result)
	}
	if harness.planner.called.Load() != 0 || harness.steps.called.Load() != 0 {
		t.Fatal("external call occurred with an invalid Checkpoint")
	}
	assertCheckpointInvalidTerminalState(t, harness, contracts.ReasonCodeCheckpointNotFound, true)
}

type executeHarness struct {
	claim       contracts.ExecutionClaim
	config      taskruntime.AgentRuntimeConfig
	executor    *executeExecutor
	dispatch    *fakeExecutionDispatch
	checkpoints *fakeExecutionCheckpoint
	planner     *fakePlanner
	steps       *fakeStepExecutor
	registry    *activecall.Registry
	service     *taskruntime.ExecuteTaskService
}

func newExecuteHarness(t *testing.T, action contracts.CheckpointNextAction) *executeHarness {
	t.Helper()
	config := loadedAgentConfig(t)
	if action == contracts.CheckpointNextActionRequestApproval || action == contracts.CheckpointNextActionExecuteApprovedTool {
		tool := config.ExecutionConfig.ToolFramework.Tools[0]
		tool.Enabled = true
		tool.RiskLevel = contracts.RiskLevelHigh
		tool.ReadOnly = false
		config.ExecutionConfig.ToolFramework.Tools[0] = tool
	}
	hash, err := taskruntime.HashExecutionConfigV1(config.ExecutionConfig)
	if err != nil {
		t.Fatalf("HashExecutionConfigV1() error = %v", err)
	}
	workerID := contracts.WorkerID("worker-1")
	planID := contracts.PlanID("plan-1")
	stepID := contracts.StepID("step-1")
	now := time.Date(2026, 8, 5, 10, 0, 0, 0, time.UTC)
	state := executeState{
		facts: taskruntime.ExecutionDispatchFacts{
			Task: taskruntime.Task{
				TaskID: "task-1", AgentID: config.ExecutionConfig.Agent.AgentID, Input: "diagnose",
				Status: contracts.TaskStatusRunning, CurrentRunID: "run-1", CurrentExecutionVersion: 1,
				DeadlineAt: now.Add(time.Hour),
			},
			Run: taskruntime.Run{RunID: "run-1", TaskID: "task-1", Status: contracts.RunStatusRunning},
			Execution: taskruntime.TaskExecution{
				TaskID: "task-1", ExecutionVersion: 1, WorkerID: &workerID,
				Status: contracts.TaskExecutionStatusRunning, ExecutionConfigHash: hash,
			},
		},
		checkpoint: taskruntime.RuntimeCheckpoint{
			CheckpointID: "checkpoint-1", TaskID: "task-1", RunID: "run-1", ExecutionVersion: 1,
			ExecutionConfigHash: hash, NextAction: action, CheckpointSequence: 1,
		},
	}
	if action != contracts.CheckpointNextActionGeneratePlan {
		state.facts.Plan = &taskruntime.ExecutionPlan{PlanID: planID}
		state.facts.Run.PlanID = &planID
	}
	if action != contracts.CheckpointNextActionGeneratePlan && action != contracts.CheckpointNextActionFinalizeRun {
		stepType := contracts.StepTypeModelCall
		var toolName contracts.ToolName
		if action == contracts.CheckpointNextActionRequestApproval || action == contracts.CheckpointNextActionExecuteApprovedTool {
			stepType = contracts.StepTypeToolCall
			toolName = config.ExecutionConfig.ToolFramework.Tools[0].Name
		}
		state.facts.Step = &taskruntime.ExecutionStep{
			StepID: stepID, Sequence: 1, Type: stepType, Status: contracts.StepStatusRunning, ToolName: toolName,
		}
		state.facts.Run.CurrentStepID = &stepID
	}
	if action == contracts.CheckpointNextActionExecuteApprovedTool {
		state.checkpoint.ApprovalContext = validApprovalContext(state.facts.Step.ToolName)
	}
	executor := &executeExecutor{}
	dispatch := &fakeExecutionDispatch{state: state}
	checkpoints := &fakeExecutionCheckpoint{dispatch: dispatch}
	planner := &fakePlanner{executor: executor}
	steps := &fakeStepExecutor{executor: executor}
	registry := activecall.NewRegistry()
	service, err := taskruntime.NewExecuteTaskService(taskruntime.ExecuteTaskDependencies{
		Executor: executor, Dispatch: dispatch, Checkpoints: checkpoints,
		Clock: executeClock{now: now}, Configs: &executeConfigSource{config: config},
		Planner: planner, Steps: steps, ActiveCalls: registry, Policy: lifecycle.New(),
	})
	if err != nil {
		t.Fatalf("NewExecuteTaskService() error = %v", err)
	}
	return &executeHarness{
		claim:  contracts.ExecutionClaim{TaskID: "task-1", RunID: "run-1", ExecutionVersion: 1, WorkerID: workerID},
		config: config, executor: executor, dispatch: dispatch, checkpoints: checkpoints,
		planner: planner, steps: steps, registry: registry, service: service,
	}
}

func (h *executeHarness) waitingApprovalCall() func(context.Context, taskruntime.StepExecutionRequest) (taskruntime.StepOutcome, error) {
	return func(context.Context, taskruntime.StepExecutionRequest) (taskruntime.StepOutcome, error) {
		h.commitWaitingApproval()
		return taskruntime.StepOutcomeWaitingApproval{}, nil
	}
}

func (h *executeHarness) commitWaitingApproval() {
	h.dispatch.mutate(func(state *executeState) {
		state.facts.Task.Status = contracts.TaskStatusWaitingApproval
		state.facts.Run.Status = contracts.RunStatusWaitingApproval
		step := *state.facts.Step
		step.Status = contracts.StepStatusWaitingApproval
		state.facts.Step = &step
		state.facts.Execution.Status = contracts.TaskExecutionStatusWaitingApproval
		state.facts.Execution.WorkerID = nil
		state.facts.Task.QueuedAt = nil
		state.approvalPending = true
		state.waitingCheckpoint = true
	})
}

func validApprovalContext(toolName contracts.ToolName) *contracts.ApprovalContext {
	return &contracts.ApprovalContext{
		ApprovalID: "approval-1", ApprovalExecutionVersion: 1, ToolName: toolName,
		FrozenToolInput: []byte(`{"resource":"deployment/app"}`), ObservedValues: []byte(`{"replicas":1}`),
		ResourceVersion: "42", FrozenInputHash: contracts.FrozenInputHash("aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"),
	}
}

func (h *executeHarness) resetHash(t *testing.T) {
	t.Helper()
	hash, err := taskruntime.HashExecutionConfigV1(h.config.ExecutionConfig)
	if err != nil {
		t.Fatalf("HashExecutionConfigV1() error = %v", err)
	}
	h.dispatch.mutate(func(state *executeState) {
		state.facts.Execution.ExecutionConfigHash = hash
		state.checkpoint.ExecutionConfigHash = hash
	})
	config := h.config
	service, err := taskruntime.NewExecuteTaskService(taskruntime.ExecuteTaskDependencies{
		Executor: h.executor, Dispatch: h.dispatch, Checkpoints: h.checkpoints,
		Clock:   executeClock{now: time.Date(2026, 8, 5, 10, 0, 0, 0, time.UTC)},
		Configs: &executeConfigSource{config: config}, Planner: h.planner, Steps: h.steps,
		ActiveCalls: h.registry, Policy: lifecycle.New(),
	})
	if err != nil {
		t.Fatalf("NewExecuteTaskService() error = %v", err)
	}
	h.service = service
}

type executeTx struct{ id uint64 }

func (*executeTx) AgentOpsRuntimeWriteTx() {}

type executeExecutor struct {
	mu                        sync.Mutex
	nextTxID                  atomic.Uint64
	inTransaction             atomic.Bool
	externalInsideTransaction atomic.Bool
}

func (e *executeExecutor) Execute(ctx context.Context, work func(context.Context, contracts.RuntimeWriteTx) error) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.inTransaction.Store(true)
	defer e.inTransaction.Store(false)
	return work(ctx, &executeTx{id: e.nextTxID.Add(1)})
}

func (e *executeExecutor) TryExecute(ctx context.Context, work func(context.Context, contracts.RuntimeWriteTx) error) (bool, error) {
	if !e.mu.TryLock() {
		return false, nil
	}
	defer e.mu.Unlock()
	e.inTransaction.Store(true)
	defer e.inTransaction.Store(false)
	return true, work(ctx, &executeTx{id: e.nextTxID.Add(1)})
}

type executeState struct {
	facts                   taskruntime.ExecutionDispatchFacts
	checkpoint              taskruntime.RuntimeCheckpoint
	steps                   map[contracts.StepID]taskruntime.ExecutionStep
	checkpointInvalidReason contracts.ReasonCode
	pendingReports          int
	checkpointInvalidTx     contracts.RuntimeWriteTx
	approvalPending         bool
	waitingCheckpoint       bool
}

type fakeExecutionDispatch struct {
	mu                     sync.Mutex
	state                  executeState
	startMiss              atomic.Bool
	startHook              func()
	lockCalls              atomic.Int64
	applyPlannerCalls      atomic.Int64
	applyStepCalls         atomic.Int64
	terminalizeCalls       atomic.Int64
	checkpointInvalidCalls atomic.Int64
}

func (d *fakeExecutionDispatch) LockExecutionDispatch(
	_ context.Context, tx contracts.RuntimeWriteTx, _ contracts.ExecutionClaim,
) (taskruntime.ExecutionDispatchFacts, error) {
	if _, ok := tx.(*executeTx); !ok {
		return taskruntime.ExecutionDispatchFacts{}, errors.New("invalid transaction")
	}
	d.lockCalls.Add(1)
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.state.facts, nil
}

func (d *fakeExecutionDispatch) LockStep(
	_ context.Context, tx contracts.RuntimeWriteTx, _ contracts.ExecutionClaim, stepID contracts.StepID,
) (taskruntime.ExecutionStep, error) {
	if _, ok := tx.(*executeTx); !ok {
		return taskruntime.ExecutionStep{}, errors.New("invalid transaction")
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	step, ok := d.state.steps[stepID]
	if !ok {
		return taskruntime.ExecutionStep{}, taskruntime.ErrRepositoryNotFound
	}
	return step, nil
}

func (d *fakeExecutionDispatch) StartExecutionAction(
	_ context.Context, _ contracts.RuntimeWriteTx, guard taskruntime.ExecutionActionGuard,
) (bool, error) {
	if d.startHook != nil {
		d.startHook()
	}
	if d.startMiss.Load() {
		return false, nil
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if !d.guardMatches(guard) {
		return false, nil
	}
	if d.state.facts.Step != nil && d.state.facts.Step.Status == contracts.StepStatusPending {
		step := *d.state.facts.Step
		step.Status = contracts.StepStatusRunning
		d.state.facts.Step = &step
	}
	return true, nil
}

func (d *fakeExecutionDispatch) ApplyPlannerCompleted(
	_ context.Context, _ contracts.RuntimeWriteTx, request taskruntime.ApplyPlannerCompletedRequest,
) (bool, error) {
	d.applyPlannerCalls.Add(1)
	d.mu.Lock()
	defer d.mu.Unlock()
	if !d.guardMatches(request.Guard) {
		return false, nil
	}
	planID := request.Draft.PlanID
	d.state.facts.Plan = &taskruntime.ExecutionPlan{PlanID: planID}
	d.state.facts.Run.PlanID = &planID
	first := request.Draft.Steps[0]
	step := taskruntime.ExecutionStep{
		StepID: first.StepID, Sequence: first.Sequence, Type: first.Type,
		Status: contracts.StepStatusPending, Input: first.Input, ToolName: first.ToolName,
	}
	d.state.facts.Step = &step
	d.state.facts.Run.CurrentStepID = &step.StepID
	d.state.steps = make(map[contracts.StepID]taskruntime.ExecutionStep, len(request.Draft.Steps))
	for _, draft := range request.Draft.Steps {
		d.state.steps[draft.StepID] = taskruntime.ExecutionStep{
			StepID: draft.StepID, Sequence: draft.Sequence, Type: draft.Type,
			Status: contracts.StepStatusPending, Input: draft.Input, ToolName: draft.ToolName,
		}
	}
	d.advanceCheckpoint(request.NextAction)
	return true, nil
}

func (d *fakeExecutionDispatch) ApplyStepCompleted(
	_ context.Context, _ contracts.RuntimeWriteTx, request taskruntime.ApplyStepCompletedRequest,
) (bool, error) {
	d.applyStepCalls.Add(1)
	d.mu.Lock()
	defer d.mu.Unlock()
	if !d.guardMatches(request.Guard) {
		return false, nil
	}
	if request.Outcome.Continuation == contracts.StepContinuationNextStep {
		next := d.state.steps[request.Outcome.NextStepID]
		d.state.facts.Step = &next
		d.state.facts.Run.CurrentStepID = &next.StepID
	} else if d.state.facts.Step != nil {
		step := *d.state.facts.Step
		step.Status = contracts.StepStatusCompleted
		d.state.facts.Step = &step
	}
	d.advanceCheckpoint(request.NextAction)
	return true, nil
}

func (d *fakeExecutionDispatch) TerminalizeExecution(
	_ context.Context, _ contracts.RuntimeWriteTx, guard taskruntime.ExecutionActionGuard, _ contracts.ErrorCode,
) (bool, error) {
	d.terminalizeCalls.Add(1)
	d.mu.Lock()
	defer d.mu.Unlock()
	if guard.CheckpointID != "" && !d.guardMatches(guard) {
		return false, nil
	}
	d.state.facts.Task.Status = contracts.TaskStatusFailed
	d.state.facts.Run.Status = contracts.RunStatusFailed
	d.state.facts.Execution.Status = contracts.TaskExecutionStatusFailed
	return true, nil
}

func (d *fakeExecutionDispatch) TerminalizeCheckpointInvalid(
	_ context.Context,
	tx contracts.RuntimeWriteTx,
	request taskruntime.TerminalizeCheckpointInvalidRequest,
) (bool, error) {
	if _, ok := tx.(*executeTx); !ok {
		return false, errors.New("invalid transaction")
	}
	d.checkpointInvalidCalls.Add(1)
	d.mu.Lock()
	defer d.mu.Unlock()
	workerID := d.state.facts.Execution.WorkerID
	if d.state.facts.Task.TaskID != request.Claim.TaskID ||
		d.state.facts.Task.CurrentRunID != request.Claim.RunID ||
		d.state.facts.Task.CurrentExecutionVersion != request.Claim.ExecutionVersion ||
		d.state.facts.Execution.ExecutionVersion != request.Claim.ExecutionVersion ||
		workerID == nil || *workerID != request.Claim.WorkerID ||
		d.state.facts.Task.Status != contracts.TaskStatusRunning ||
		d.state.facts.Run.Status != contracts.RunStatusRunning ||
		d.state.facts.Execution.Status != contracts.TaskExecutionStatusRunning {
		return false, nil
	}
	if !request.ReasonCode.ValidForCheckpointInvalid() {
		return false, taskruntime.ErrPersistenceInvariantViolation
	}
	if request.ActiveStepID != nil {
		if d.state.facts.Step == nil || d.state.facts.Step.StepID != *request.ActiveStepID {
			return false, taskruntime.ErrPersistenceInvariantViolation
		}
		step := *d.state.facts.Step
		step.Status = contracts.StepStatusFailed
		d.state.facts.Step = &step
	}
	errorCode := contracts.ErrorCodeCheckpointInvalid
	d.state.facts.Task.Status = contracts.TaskStatusFailed
	d.state.facts.Task.ErrorCode = &errorCode
	d.state.facts.Task.QueuedAt = nil
	d.state.facts.Run.Status = contracts.RunStatusFailed
	d.state.facts.Run.ErrorCode = &errorCode
	d.state.facts.Execution.Status = contracts.TaskExecutionStatusFailed
	d.state.facts.Execution.ErrorCode = &errorCode
	d.state.checkpointInvalidReason = request.ReasonCode
	d.state.checkpointInvalidTx = tx
	if d.state.pendingReports == 0 {
		d.state.pendingReports = 1
	}
	return true, nil
}

func (d *fakeExecutionDispatch) FinalizeExecution(
	_ context.Context, _ contracts.RuntimeWriteTx, guard taskruntime.ExecutionActionGuard,
) (bool, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if !d.guardMatches(guard) {
		return false, nil
	}
	d.state.facts.Task.Status = contracts.TaskStatusCompleted
	d.state.facts.Run.Status = contracts.RunStatusCompleted
	d.state.facts.Execution.Status = contracts.TaskExecutionStatusCompleted
	return true, nil
}

func (d *fakeExecutionDispatch) ConfirmExecutionTerminal(
	_ context.Context, _ contracts.RuntimeWriteTx, claim contracts.ExecutionClaim,
) (bool, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.state.facts.Task.TaskID == claim.TaskID && d.state.facts.Task.CurrentExecutionVersion == claim.ExecutionVersion &&
		d.state.facts.Task.Status.Terminal() && d.state.facts.Run.Status.Terminal() &&
		d.state.facts.Execution.ExecutionVersion == claim.ExecutionVersion && d.state.facts.Execution.Status.Ended(), nil
}

func (d *fakeExecutionDispatch) ConfirmExecutionWaitingApproval(
	_ context.Context, _ contracts.RuntimeWriteTx, claim contracts.ExecutionClaim, stepID contracts.StepID,
) (bool, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.state.facts.Task.TaskID == claim.TaskID &&
		d.state.facts.Task.CurrentExecutionVersion == claim.ExecutionVersion &&
		d.state.facts.Task.Status == contracts.TaskStatusWaitingApproval && d.state.facts.Task.QueuedAt == nil &&
		d.state.facts.Run.RunID == claim.RunID && d.state.facts.Run.Status == contracts.RunStatusWaitingApproval &&
		d.state.facts.Execution.ExecutionVersion == claim.ExecutionVersion &&
		d.state.facts.Execution.Status == contracts.TaskExecutionStatusWaitingApproval &&
		d.state.facts.Execution.WorkerID == nil && d.state.facts.Step != nil &&
		d.state.facts.Step.StepID == stepID && d.state.facts.Step.Status == contracts.StepStatusWaitingApproval &&
		d.state.approvalPending && d.state.waitingCheckpoint, nil
}

func (d *fakeExecutionDispatch) guardMatches(guard taskruntime.ExecutionActionGuard) bool {
	return d.state.facts.Task.CurrentExecutionVersion == guard.Claim.ExecutionVersion &&
		d.state.facts.Execution.WorkerID != nil && *d.state.facts.Execution.WorkerID == guard.Claim.WorkerID &&
		d.state.checkpoint.CheckpointID == guard.CheckpointID && d.state.checkpoint.NextAction == guard.NextAction
}

func (d *fakeExecutionDispatch) advanceCheckpoint(action contracts.CheckpointNextAction) {
	d.state.checkpoint.CheckpointID = contracts.CheckpointID(string(d.state.checkpoint.CheckpointID) + "-next")
	d.state.checkpoint.CheckpointSequence++
	d.state.checkpoint.NextAction = action
}

func (d *fakeExecutionDispatch) snapshot() executeState {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.state
}

func (d *fakeExecutionDispatch) mutate(change func(*executeState)) {
	d.mu.Lock()
	defer d.mu.Unlock()
	change(&d.state)
}

type fakeExecutionCheckpoint struct {
	dispatch  *fakeExecutionDispatch
	invalid   atomic.Bool
	reason    contracts.ReasonCode
	mu        sync.Mutex
	invalidTx contracts.RuntimeWriteTx
}

func (p *fakeExecutionCheckpoint) SaveRuntimeCheckpoint(
	context.Context, contracts.RuntimeWriteTx, taskruntime.SaveRuntimeCheckpointRequest,
) error {
	return errors.New("SaveRuntimeCheckpoint is outside Execute test scope")
}

func (p *fakeExecutionCheckpoint) LoadLatestForClaim(
	context.Context,
	contracts.RuntimeWriteTx,
	contracts.TaskID,
	contracts.RunID,
	contracts.ExecutionVersion,
	taskruntime.ClaimCheckpointSource,
) (taskruntime.ClaimCheckpointResult, error) {
	return nil, errors.New("LoadLatestForClaim is outside Execute test scope")
}

func (p *fakeExecutionCheckpoint) LoadLatestForExecutionDispatch(
	_ context.Context, tx contracts.RuntimeWriteTx, _ contracts.TaskID, _ contracts.RunID, _ contracts.ExecutionVersion,
) (taskruntime.ExecutionCheckpointResult, error) {
	if p.invalid.Load() {
		reason := p.reason
		if reason == "" {
			reason = contracts.ReasonCodeCheckpointNotFound
		}
		p.mu.Lock()
		p.invalidTx = tx
		p.mu.Unlock()
		return taskruntime.ExecutionCheckpointInvalid{ReasonCode: reason}, nil
	}
	return taskruntime.ExecutionCheckpointValid{Checkpoint: p.dispatch.snapshot().checkpoint}, nil
}

type fakePlanner struct {
	executor *executeExecutor
	outcomes []taskruntime.PlannerOutcome
	call     func(context.Context, taskruntime.PlannerRequest) (taskruntime.PlannerOutcome, error)
	called   atomic.Int64
}

func (p *fakePlanner) GeneratePlan(ctx context.Context, request taskruntime.PlannerRequest) (taskruntime.PlannerOutcome, error) {
	p.called.Add(1)
	if p.executor.inTransaction.Load() {
		p.executor.externalInsideTransaction.Store(true)
	}
	if p.call != nil {
		return p.call(ctx, request)
	}
	result := p.outcomes[0]
	p.outcomes = p.outcomes[1:]
	return result, nil
}

type fakeStepExecutor struct {
	executor *executeExecutor
	outcomes []taskruntime.StepOutcome
	call     func(context.Context, taskruntime.StepExecutionRequest) (taskruntime.StepOutcome, error)
	called   atomic.Int64
}

func (s *fakeStepExecutor) ExecuteStep(ctx context.Context, request taskruntime.StepExecutionRequest) (taskruntime.StepOutcome, error) {
	s.called.Add(1)
	if s.executor.inTransaction.Load() {
		s.executor.externalInsideTransaction.Store(true)
	}
	if s.call != nil {
		return s.call(ctx, request)
	}
	result := s.outcomes[0]
	s.outcomes = s.outcomes[1:]
	return result, nil
}

type executeClock struct{ now time.Time }

func (c executeClock) Now(context.Context, contracts.RuntimeWriteTx) (time.Time, error) {
	return c.now, nil
}

type executeConfigSource struct {
	config taskruntime.AgentRuntimeConfig
}

func (s *executeConfigSource) LookupAgent(agentID contracts.AgentID) (taskruntime.AgentRuntimeConfig, bool) {
	return s.config, s.config.ExecutionConfig.Agent.AgentID == agentID
}

func validPlanDraft() taskruntime.ValidatedPlanDraft {
	return taskruntime.ValidatedPlanDraft{
		PlanID: "plan-1", Goal: "goal",
		Steps: []taskruntime.PlanStepDraft{{StepID: "step-1", Sequence: 1, Type: contracts.StepTypeModelCall}},
	}
}

func activeKey(claim contracts.ExecutionClaim) activecall.Key {
	return activecall.Key{TaskID: claim.TaskID, ExecutionVersion: claim.ExecutionVersion, WorkerID: claim.WorkerID}
}

func resultType(value any) string { return fmt.Sprintf("%T", value) }

func boolCount(value bool) int64 {
	if value {
		return 1
	}
	return 0
}

func assertCheckpointInvalidTerminalState(
	t *testing.T,
	harness *executeHarness,
	wantReason contracts.ReasonCode,
	wantStepFailed bool,
) {
	t.Helper()
	state := harness.dispatch.snapshot()
	if state.facts.Task.Status != contracts.TaskStatusFailed || state.facts.Run.Status != contracts.RunStatusFailed ||
		state.facts.Execution.Status != contracts.TaskExecutionStatusFailed {
		t.Fatalf("terminal states = Task:%s Run:%s Execution:%s",
			state.facts.Task.Status, state.facts.Run.Status, state.facts.Execution.Status)
	}
	if state.facts.Task.ErrorCode == nil || *state.facts.Task.ErrorCode != contracts.ErrorCodeCheckpointInvalid ||
		state.facts.Run.ErrorCode == nil || *state.facts.Run.ErrorCode != contracts.ErrorCodeCheckpointInvalid ||
		state.facts.Execution.ErrorCode == nil || *state.facts.Execution.ErrorCode != contracts.ErrorCodeCheckpointInvalid {
		t.Fatalf("terminal error codes = Task:%v Run:%v Execution:%v",
			state.facts.Task.ErrorCode, state.facts.Run.ErrorCode, state.facts.Execution.ErrorCode)
	}
	if state.facts.Task.QueuedAt != nil {
		t.Fatalf("queued_at = %v, want nil", state.facts.Task.QueuedAt)
	}
	if state.checkpointInvalidReason != wantReason || state.pendingReports != 1 {
		t.Fatalf("reason/report = %s/%d, want %s/1",
			state.checkpointInvalidReason, state.pendingReports, wantReason)
	}
	if wantStepFailed {
		if state.facts.Step == nil || state.facts.Step.Status != contracts.StepStatusFailed {
			t.Fatalf("active Step = %+v, want Failed", state.facts.Step)
		}
	} else if state.facts.Step != nil {
		t.Fatalf("Planner terminalization unexpectedly has Step %+v", state.facts.Step)
	}
	if harness.dispatch.checkpointInvalidCalls.Load() != 1 {
		t.Fatalf("CheckpointInvalid terminalization calls = %d, want 1", harness.dispatch.checkpointInvalidCalls.Load())
	}
	harness.checkpoints.mu.Lock()
	loadTx := harness.checkpoints.invalidTx
	harness.checkpoints.mu.Unlock()
	if loadTx == nil || loadTx != state.checkpointInvalidTx {
		t.Fatalf("Checkpoint load tx = %p, terminalization tx = %p; want same transaction", loadTx, state.checkpointInvalidTx)
	}
}

var (
	_ contracts.RuntimeWriteExecutor          = (*executeExecutor)(nil)
	_ taskruntime.ExecutionDispatchRepository = (*fakeExecutionDispatch)(nil)
	_ taskruntime.RuntimeCheckpointPort       = (*fakeExecutionCheckpoint)(nil)
	_ taskruntime.PlannerPort                 = (*fakePlanner)(nil)
	_ taskruntime.StepExecutorPort            = (*fakeStepExecutor)(nil)
)
