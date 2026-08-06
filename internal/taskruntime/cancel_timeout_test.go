package taskruntime_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/zhaohaip/agentops-go/internal/contracts"
	"github.com/zhaohaip/agentops-go/internal/lifecycle"
	"github.com/zhaohaip/agentops-go/internal/taskruntime"
	"github.com/zhaohaip/agentops-go/internal/taskruntime/activecall"
)

func TestCancelTaskTerminalizesEveryNonTerminalState(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name            string
		taskStatus      contracts.TaskStatus
		runStatus       contracts.RunStatus
		executionStatus contracts.TaskExecutionStatus
		stepStatus      *contracts.StepStatus
	}{
		{name: "Pending", taskStatus: contracts.TaskStatusPending, runStatus: contracts.RunStatusPending,
			executionStatus: contracts.TaskExecutionStatusQueued},
		{name: "Running", taskStatus: contracts.TaskStatusRunning, runStatus: contracts.RunStatusRunning,
			executionStatus: contracts.TaskExecutionStatusRunning, stepStatus: stepStatus(contracts.StepStatusRunning)},
		{name: "Planner committed Pending Step", taskStatus: contracts.TaskStatusRunning, runStatus: contracts.RunStatusRunning,
			executionStatus: contracts.TaskExecutionStatusRunning, stepStatus: stepStatus(contracts.StepStatusPending)},
		{name: "WaitingApproval", taskStatus: contracts.TaskStatusWaitingApproval, runStatus: contracts.RunStatusWaitingApproval,
			executionStatus: contracts.TaskExecutionStatusWaitingApproval, stepStatus: stepStatus(contracts.StepStatusWaitingApproval)},
		{name: "Interrupted", taskStatus: contracts.TaskStatusInterrupted, runStatus: contracts.RunStatusRunning,
			executionStatus: contracts.TaskExecutionStatusInterrupted, stepStatus: stepStatus(contracts.StepStatusRunning)},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			harness := newTerminationHarness(t, test.taskStatus, test.runStatus, test.executionStatus, test.stepStatus)
			oldEndedAt := harness.executor.snapshot().executions[executionKey("task-terminate", 1)].EndedAt
			result, err := harness.cancel.CancelTask(context.Background(), cancelRequest(taskruntime.CommandID("cancel-"+test.name)))
			if err != nil {
				t.Fatalf("CancelTask() error = %v", err)
			}
			if result.TerminationReason != contracts.TerminationReasonCancelled {
				t.Fatalf("termination reason = %s", result.TerminationReason)
			}
			store := harness.executor.snapshot()
			wantWorkerID := (*contracts.WorkerID)(nil)
			if test.executionStatus == contracts.TaskExecutionStatusRunning {
				wantWorkerID = workerID("worker-1")
			}
			assertTerminationFacts(t, store, contracts.TaskStatusCancelled, contracts.ErrorCodeTaskCancelled,
				contracts.TerminationReasonCancelled, wantWorkerID)
			if test.stepStatus == nil {
				if _, exists := store.terminationSteps["task-terminate"]; exists {
					t.Fatal("Cancel created an absent Step")
				}
			}
			if test.executionStatus == contracts.TaskExecutionStatusInterrupted &&
				(store.executions[executionKey("task-terminate", 1)].EndedAt != oldEndedAt) {
				t.Fatal("Interrupted Execution ended_at was not preserved")
			}
			if len(store.reports) != 1 || len(store.receipts) != 1 {
				t.Fatalf("reports/receipts = %d/%d, want 1/1", len(store.reports), len(store.receipts))
			}
			if len(store.logs) != 1 || store.logs[0].Event != "TaskTerminalized" {
				t.Fatalf("TaskLogs = %+v, want one TaskTerminalized", store.logs)
			}
		})
	}
}

func TestCancelAndTimeoutClassifyRunningToolExecution(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name        string
		readOnly    bool
		timeout     bool
		wantStatus  contracts.ToolExecutionStatus
		wantError   *contracts.ErrorCode
		wantUnknown bool
	}{
		{name: "Cancel read Tool", readOnly: true, wantStatus: contracts.ToolExecutionStatusFailed,
			wantError: errorCode(contracts.ErrorCodeTaskCancelled)},
		{name: "Timeout read Tool", readOnly: true, timeout: true, wantStatus: contracts.ToolExecutionStatusFailed,
			wantError: errorCode(contracts.ErrorCodeTaskTimeout)},
		{name: "Cancel write Tool", wantStatus: contracts.ToolExecutionStatusUnknown, wantUnknown: true},
		{name: "Timeout write Tool", timeout: true, wantStatus: contracts.ToolExecutionStatusUnknown, wantUnknown: true},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			harness := newTerminationHarness(t, contracts.TaskStatusRunning, contracts.RunStatusRunning,
				contracts.TaskExecutionStatusRunning, stepStatus(contracts.StepStatusRunning))
			if test.timeout {
				harness.expireNow()
			}
			toolName := harness.setTool(t, test.readOnly)
			harness.executor.store.terminationTools["task-terminate"] = taskruntime.TerminationToolExecution{
				ToolExecutionID: "tool-execution-1", ToolName: toolName, Status: contracts.ToolExecutionStatusRunning,
			}
			if test.timeout {
				if _, err := harness.expire.ExpireTask(context.Background(), taskruntime.ExpireTaskRequest{
					TaskID: "task-terminate", ObservedExecutionVersion: 1,
				}); err != nil {
					t.Fatalf("ExpireTask() error = %v", err)
				}
			} else if _, err := harness.cancel.CancelTask(context.Background(), cancelRequest("cancel-tool")); err != nil {
				t.Fatalf("CancelTask() error = %v", err)
			}
			store := harness.executor.snapshot()
			tool := store.terminationTools["task-terminate"]
			if tool.Status != test.wantStatus || !equalErrorCode(tool.ErrorCode, test.wantError) ||
				tool.SideEffectUnknown != test.wantUnknown || tool.EndedAt == nil {
				t.Fatalf("Tool termination = %+v", tool)
			}
			execution := store.executions[executionKey("task-terminate", 1)]
			if test.wantUnknown && (execution.ErrorCode == nil || *execution.ErrorCode != contracts.ErrorCodeWriteToolInterrupted) {
				t.Fatalf("write Tool Execution error = %v", execution.ErrorCode)
			}
		})
	}
}

func TestCancelPreservesConfigurationMismatchEvidence(t *testing.T) {
	t.Parallel()
	harness := newTerminationHarness(t, contracts.TaskStatusInterrupted, contracts.RunStatusPending,
		contracts.TaskExecutionStatusInterrupted, nil)
	before := harness.executor.snapshot().executions[executionKey("task-terminate", 1)]
	if _, err := harness.cancel.CancelTask(context.Background(), cancelRequest("cancel-interrupted")); err != nil {
		t.Fatalf("CancelTask() error = %v", err)
	}
	after := harness.executor.snapshot().executions[executionKey("task-terminate", 1)]
	if !equalErrorCode(before.ErrorCode, after.ErrorCode) || before.ObservedConfigHash != after.ObservedConfigHash ||
		after.TerminationReason == nil || *after.TerminationReason != contracts.TerminationReasonCancelled ||
		before.EndedAt != after.EndedAt || after.WorkerID != nil {
		t.Fatalf("configuration mismatch evidence changed: before=%+v after=%+v", before, after)
	}
}

func TestCancelAndTimeoutPreserveStartedInterruptedWorkerID(t *testing.T) {
	t.Parallel()
	for _, timeout := range []bool{false, true} {
		timeout := timeout
		t.Run(map[bool]string{false: "Cancel", true: "Timeout"}[timeout], func(t *testing.T) {
			harness := newTerminationHarness(t, contracts.TaskStatusRunning, contracts.RunStatusRunning,
				contracts.TaskExecutionStatusInterrupted, stepStatus(contracts.StepStatusRunning))
			harness.executor.store.tasks["task-terminate"] = clearTaskError(harness.executor.store.tasks["task-terminate"])
			execution := harness.executor.store.executions[executionKey("task-terminate", 1)]
			execution.WorkerID = workerID("worker-interrupted")
			execution.ErrorCode = errorCode(contracts.ErrorCodeWorkerInterrupted)
			execution.ObservedConfigHash = nil
			startedAt := harness.now.Add(-2 * time.Minute)
			execution.StartedAt = &startedAt
			harness.executor.store.executions[executionKey("task-terminate", 1)] = execution
			oldEndedAt := execution.EndedAt
			if timeout {
				harness.expireNow()
				if _, err := harness.expire.ExpireTask(context.Background(), taskruntime.ExpireTaskRequest{
					TaskID: "task-terminate", ObservedExecutionVersion: 1,
				}); err != nil {
					t.Fatalf("ExpireTask() error = %v", err)
				}
			} else if _, err := harness.cancel.CancelTask(context.Background(), cancelRequest("cancel-started-interrupted")); err != nil {
				t.Fatalf("CancelTask() error = %v", err)
			}
			after := harness.executor.snapshot().executions[executionKey("task-terminate", 1)]
			if !equalWorkerID(after.WorkerID, workerID("worker-interrupted")) || after.EndedAt != oldEndedAt {
				t.Fatalf("started INTERRUPTED history = worker:%v ended_at:%v, want worker-interrupted/%v",
					after.WorkerID, after.EndedAt, oldEndedAt)
			}
		})
	}
}

func TestTimeoutKeepsNilWorkerForWaitingApprovalAndPreClaimConfigMismatch(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name            string
		taskStatus      contracts.TaskStatus
		runStatus       contracts.RunStatus
		executionStatus contracts.TaskExecutionStatus
		stepStatus      *contracts.StepStatus
	}{
		{name: "WaitingApproval", taskStatus: contracts.TaskStatusWaitingApproval,
			runStatus: contracts.RunStatusWaitingApproval, executionStatus: contracts.TaskExecutionStatusWaitingApproval,
			stepStatus: stepStatus(contracts.StepStatusWaitingApproval)},
		{name: "pre-claim configuration mismatch", taskStatus: contracts.TaskStatusInterrupted,
			runStatus: contracts.RunStatusPending, executionStatus: contracts.TaskExecutionStatusInterrupted},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			harness := newTerminationHarness(t, test.taskStatus, test.runStatus, test.executionStatus, test.stepStatus)
			harness.expireNow()
			if _, err := harness.expire.ExpireTask(context.Background(), taskruntime.ExpireTaskRequest{
				TaskID: "task-terminate", ObservedExecutionVersion: 1,
			}); err != nil {
				t.Fatalf("ExpireTask() error = %v", err)
			}
			if execution := harness.executor.snapshot().executions[executionKey("task-terminate", 1)]; execution.WorkerID != nil {
				t.Fatalf("worker_id = %v, want nil", execution.WorkerID)
			}
		})
	}
}

func TestCancelAtDeadlineUsesTimeoutSemanticsAndReceiptReplay(t *testing.T) {
	t.Parallel()
	harness := newTerminationHarness(t, contracts.TaskStatusRunning, contracts.RunStatusRunning,
		contracts.TaskExecutionStatusRunning, stepStatus(contracts.StepStatusRunning))
	harness.executor.store.tasks["task-terminate"] = taskWithDeadline(harness.executor.store.tasks["task-terminate"], harness.now)
	request := cancelRequest("cancel-at-deadline")
	if _, err := harness.cancel.CancelTask(context.Background(), request); !errors.Is(err, taskruntime.ErrTaskTimedOut) {
		t.Fatalf("CancelTask() error = %v, want TaskTimeout", err)
	}
	commits := harness.executor.commits
	if _, err := harness.cancel.CancelTask(context.Background(), request); !errors.Is(err, taskruntime.ErrTaskTimedOut) {
		t.Fatalf("CancelTask(replay) error = %v, want TaskTimeout", err)
	}
	if harness.executor.commits != commits+1 || len(harness.executor.snapshot().reports) != 1 {
		t.Fatalf("replay commits/reports = %d/%d", harness.executor.commits, len(harness.executor.snapshot().reports))
	}
	assertTerminationFacts(t, harness.executor.snapshot(), contracts.TaskStatusFailed,
		contracts.ErrorCodeTaskTimeout, contracts.TerminationReasonTimedOut, workerID("worker-1"))
	conflict := request
	conflict.OperatorID = "other"
	if _, err := harness.cancel.CancelTask(context.Background(), conflict); !errors.Is(err, taskruntime.ErrCommandConflict) {
		t.Fatalf("CancelTask(conflict) error = %v", err)
	}
}

func TestCancelAlreadyTerminalStoresAndReplaysDeterministicReceipt(t *testing.T) {
	t.Parallel()
	harness := newTerminationHarness(t, contracts.TaskStatusCompleted, contracts.RunStatusCompleted,
		contracts.TaskExecutionStatusCompleted, nil)
	request := cancelRequest("cancel-terminal")
	for attempt := 0; attempt < 2; attempt++ {
		if _, err := harness.cancel.CancelTask(context.Background(), request); !errors.Is(err, taskruntime.ErrTaskAlreadyTerminal) {
			t.Fatalf("CancelTask(attempt %d) error = %v", attempt+1, err)
		}
	}
	store := harness.executor.snapshot()
	if len(store.receipts) != 1 || len(store.reports) != 0 || len(store.logs) != 0 {
		t.Fatalf("receipts/reports/logs = %d/%d/%d, want 1/0/0", len(store.receipts), len(store.reports), len(store.logs))
	}
	if store.tasks["task-terminate"].Status != contracts.TaskStatusCompleted {
		t.Fatal("terminal Task was modified")
	}
}

func TestExpireTaskClosedResultsAndVersionGuard(t *testing.T) {
	t.Parallel()
	t.Run("Expired", func(t *testing.T) {
		harness := newTerminationHarness(t, contracts.TaskStatusRunning, contracts.RunStatusRunning,
			contracts.TaskExecutionStatusRunning, stepStatus(contracts.StepStatusPending))
		harness.expireNow()
		result, err := harness.expire.ExpireTask(context.Background(), taskruntime.ExpireTaskRequest{
			TaskID: "task-terminate", ObservedExecutionVersion: 1,
		})
		if err != nil {
			t.Fatalf("ExpireTask() error = %v", err)
		}
		if _, ok := result.(taskruntime.ExpireTaskExpired); !ok {
			t.Fatalf("result = %T", result)
		}
		assertTerminationFacts(t, harness.executor.snapshot(), contracts.TaskStatusFailed,
			contracts.ErrorCodeTaskTimeout, contracts.TerminationReasonTimedOut, workerID("worker-1"))
		if execution := harness.executor.snapshot().executions[executionKey("task-terminate", 1)]; execution.ErrorCode != nil {
			t.Fatalf("Timeout Execution error_code = %v, want nil; TIMED_OUT belongs only to termination_reason", execution.ErrorCode)
		}
	})
	t.Run("deadline not reached", func(t *testing.T) {
		harness := newTerminationHarness(t, contracts.TaskStatusRunning, contracts.RunStatusRunning,
			contracts.TaskExecutionStatusRunning, nil)
		task := harness.executor.store.tasks["task-terminate"]
		task.DeadlineAt = harness.now.Add(time.Second)
		harness.executor.store.tasks[task.TaskID] = task
		result, err := harness.expire.ExpireTask(context.Background(), taskruntime.ExpireTaskRequest{TaskID: task.TaskID, ObservedExecutionVersion: 1})
		if err != nil {
			t.Fatalf("ExpireTask() error = %v", err)
		}
		if _, ok := result.(taskruntime.ExpireTaskSkipped); !ok {
			t.Fatalf("result = %T", result)
		}
	})
	t.Run("observed version stale", func(t *testing.T) {
		harness := newTerminationHarness(t, contracts.TaskStatusRunning, contracts.RunStatusRunning,
			contracts.TaskExecutionStatusRunning, nil)
		result, err := harness.expire.ExpireTask(context.Background(), taskruntime.ExpireTaskRequest{TaskID: "task-terminate", ObservedExecutionVersion: 2})
		if err != nil {
			t.Fatalf("ExpireTask() error = %v", err)
		}
		if _, ok := result.(taskruntime.ExpireTaskSkipped); !ok {
			t.Fatalf("result = %T", result)
		}
		if len(harness.executor.snapshot().reports) != 0 {
			t.Fatal("stale version created Report")
		}
	})
	t.Run("already terminal", func(t *testing.T) {
		harness := newTerminationHarness(t, contracts.TaskStatusCompleted, contracts.RunStatusCompleted,
			contracts.TaskExecutionStatusCompleted, nil)
		result, err := harness.expire.ExpireTask(context.Background(), taskruntime.ExpireTaskRequest{TaskID: "task-terminate", ObservedExecutionVersion: 1})
		if err != nil {
			t.Fatalf("ExpireTask() error = %v", err)
		}
		if _, ok := result.(taskruntime.ExpireTaskAlreadyTerminal); !ok {
			t.Fatalf("result = %T", result)
		}
	})
}

func TestTerminationReportFailureRollsBackAllFacts(t *testing.T) {
	t.Parallel()
	harness := newTerminationHarness(t, contracts.TaskStatusRunning, contracts.RunStatusRunning,
		contracts.TaskExecutionStatusRunning, stepStatus(contracts.StepStatusRunning))
	harness.expireNow()
	harness.reports.fail = errors.New("report unavailable")
	before := harness.executor.snapshot()
	if _, err := harness.expire.ExpireTask(context.Background(), taskruntime.ExpireTaskRequest{
		TaskID: "task-terminate", ObservedExecutionVersion: 1,
	}); err == nil {
		t.Fatal("ExpireTask() succeeded")
	}
	after := harness.executor.snapshot()
	if after.tasks["task-terminate"].Status != before.tasks["task-terminate"].Status ||
		after.executions[executionKey("task-terminate", 1)].Status != before.executions[executionKey("task-terminate", 1)].Status ||
		len(after.reports) != 0 {
		t.Fatal("Report failure left partial termination facts")
	}
}

func TestTerminationCommitsBeforeCancellingPreparedAndActiveCalls(t *testing.T) {
	t.Parallel()
	for _, activate := range []bool{false, true} {
		activate := activate
		t.Run(map[bool]string{false: "PREPARED", true: "ACTIVE"}[activate], func(t *testing.T) {
			harness := newTerminationHarness(t, contracts.TaskStatusRunning, contracts.RunStatusRunning,
				contracts.TaskExecutionStatusRunning, stepStatus(contracts.StepStatusRunning))
			key := activecall.Key{TaskID: "task-terminate", ExecutionVersion: 1, WorkerID: "worker-1"}
			handle, err := harness.registry.Prepare(context.Background(), key,
				activecall.Metadata{ActionKind: contracts.CheckpointNextActionExecuteStep, StepID: "step-1"})
			if err != nil {
				t.Fatalf("Prepare() error = %v", err)
			}
			defer handle.Unregister()
			if activate {
				if err := handle.Activate(); err != nil {
					t.Fatalf("Activate() error = %v", err)
				}
			}
			harness.terminations.applyHook = func() {
				if handle.Context().Err() != nil {
					t.Fatal("Active Call cancelled before termination commit")
				}
			}
			if _, err := harness.cancel.CancelTask(context.Background(), cancelRequest("cancel-active")); err != nil {
				t.Fatalf("CancelTask() error = %v", err)
			}
			if !errors.Is(context.Cause(handle.Context()), activecall.CauseTaskCancelled) {
				t.Fatalf("cancel cause = %v", context.Cause(handle.Context()))
			}
		})
	}
}

func TestTimeoutCancelsActiveCallAndLateResultCannotCommit(t *testing.T) {
	t.Parallel()
	harness := newTerminationHarness(t, contracts.TaskStatusRunning, contracts.RunStatusRunning,
		contracts.TaskExecutionStatusRunning, stepStatus(contracts.StepStatusRunning))
	harness.expireNow()
	key := activecall.Key{TaskID: "task-terminate", ExecutionVersion: 1, WorkerID: "worker-1"}
	handle, err := harness.registry.Prepare(context.Background(), key,
		activecall.Metadata{ActionKind: contracts.CheckpointNextActionExecuteStep, StepID: "step-1"})
	if err != nil {
		t.Fatalf("Prepare() error = %v", err)
	}
	defer handle.Unregister()
	if err := handle.Activate(); err != nil {
		t.Fatalf("Activate() error = %v", err)
	}
	if _, err := harness.expire.ExpireTask(context.Background(), taskruntime.ExpireTaskRequest{
		TaskID: "task-terminate", ObservedExecutionVersion: 1,
	}); err != nil {
		t.Fatalf("ExpireTask() error = %v", err)
	}
	if !errors.Is(context.Cause(handle.Context()), activecall.CauseTaskTimedOut) {
		t.Fatalf("cancel cause = %v", context.Cause(handle.Context()))
	}
	ports := fakeExecutionRepository{harness.repositories}
	lateError := contracts.ErrorCodeModelCallFailed
	lateApplied := false
	if err := harness.executor.Execute(context.Background(), func(ctx context.Context, tx contracts.RuntimeWriteTx) error {
		var updateErr error
		lateApplied, updateErr = ports.Update(ctx, tx, taskruntime.TaskExecutionUpdate{
			TaskID: "task-terminate", ExecutionVersion: 1,
			ExpectedStatus: contracts.TaskExecutionStatusRunning, ExpectedWorkerID: workerID("worker-1"),
			Status: contracts.TaskExecutionStatusFailed, ErrorCode: &lateError,
		})
		return updateErr
	}); err != nil {
		t.Fatalf("late result transaction error = %v", err)
	}
	if lateApplied {
		t.Fatal("late result overwrote Timeout terminal facts")
	}
	assertTerminationFacts(t, harness.executor.snapshot(), contracts.TaskStatusFailed,
		contracts.ErrorCodeTaskTimeout, contracts.TerminationReasonTimedOut, workerID("worker-1"))
}

func TestTimeoutWinsSynchronizedRaceAgainstLateResult(t *testing.T) {
	t.Parallel()
	harness := newTerminationHarness(t, contracts.TaskStatusRunning, contracts.RunStatusRunning,
		contracts.TaskExecutionStatusRunning, stepStatus(contracts.StepStatusRunning))
	harness.expireNow()
	harness.executor.waiterStarted = make(chan struct{}, 1)
	terminationEntered := make(chan struct{})
	releaseTermination := make(chan struct{})
	harness.terminations.applyHook = func() {
		close(terminationEntered)
		<-releaseTermination
	}
	type expireResult struct {
		result taskruntime.ExpireTaskResult
		err    error
	}
	expired := make(chan expireResult, 1)
	go func() {
		result, err := harness.expire.ExpireTask(context.Background(), taskruntime.ExpireTaskRequest{
			TaskID: "task-terminate", ObservedExecutionVersion: 1,
		})
		expired <- expireResult{result: result, err: err}
	}()
	waitForSignal(t, terminationEntered, "Timeout termination transaction")

	late := make(chan struct {
		applied bool
		err     error
	}, 1)
	go func() {
		applied := false
		err := harness.executor.Execute(context.Background(), func(ctx context.Context, tx contracts.RuntimeWriteTx) error {
			var updateErr error
			applied, updateErr = (fakeExecutionRepository{harness.repositories}).Update(ctx, tx, taskruntime.TaskExecutionUpdate{
				TaskID: "task-terminate", ExecutionVersion: 1,
				ExpectedStatus: contracts.TaskExecutionStatusRunning, ExpectedWorkerID: workerID("worker-1"),
				Status: contracts.TaskExecutionStatusFailed,
			})
			return updateErr
		})
		late <- struct {
			applied bool
			err     error
		}{applied: applied, err: err}
	}()
	waitForSignal(t, harness.executor.waiterStarted, "late result waiting behind Timeout")
	close(releaseTermination)
	expireOutcome := waitForValue(t, expired, "ExpireTask result")
	if expireOutcome.err != nil {
		t.Fatalf("ExpireTask() error = %v", expireOutcome.err)
	}
	if _, ok := expireOutcome.result.(taskruntime.ExpireTaskExpired); !ok {
		t.Fatalf("ExpireTask result = %T", expireOutcome.result)
	}
	lateOutcome := waitForValue(t, late, "late result")
	if lateOutcome.err != nil || lateOutcome.applied {
		t.Fatalf("late result = applied:%v error:%v, want false/nil", lateOutcome.applied, lateOutcome.err)
	}
	assertTerminationFacts(t, harness.executor.snapshot(), contracts.TaskStatusFailed,
		contracts.ErrorCodeTaskTimeout, contracts.TerminationReasonTimedOut, workerID("worker-1"))
}

func TestPendingReportUsesTerminationTransactionToken(t *testing.T) {
	t.Parallel()
	harness := newTerminationHarness(t, contracts.TaskStatusRunning, contracts.RunStatusRunning,
		contracts.TaskExecutionStatusRunning, nil)
	harness.expireNow()
	if _, err := harness.expire.ExpireTask(context.Background(), taskruntime.ExpireTaskRequest{
		TaskID: "task-terminate", ObservedExecutionVersion: 1,
	}); err != nil {
		t.Fatalf("ExpireTask() error = %v", err)
	}
	harness.reports.mu.Lock()
	reportTx := harness.reports.seenTx[0]
	harness.reports.mu.Unlock()
	harness.repositories.mu.Lock()
	applyTx := harness.repositories.operationTx["termination.apply"][0]
	harness.repositories.mu.Unlock()
	if reportTx != applyTx {
		t.Fatalf("Report tx = %p, termination tx = %p", reportTx, applyTx)
	}
}

type terminationHarness struct {
	now          time.Time
	executor     *fakeExecutor
	repositories *fakeRepositories
	terminations *fakeTerminationRepository
	reports      *fakePendingReportWriter
	registry     *activecall.Registry
	configs      *fakeAgentConfigSource
	cancel       *taskruntime.CancelTaskService
	expire       *taskruntime.ExpireTaskService
}

func newTerminationHarness(t *testing.T, taskStatus contracts.TaskStatus, runStatus contracts.RunStatus,
	executionStatus contracts.TaskExecutionStatus, currentStep *contracts.StepStatus) *terminationHarness {
	t.Helper()
	now := time.Date(2026, 8, 5, 12, 0, 0, 0, time.UTC)
	executor := newFakeExecutor()
	repositories := newFakeRepositories(executor, now)
	reports := &fakePendingReportWriter{}
	registry := activecall.NewRegistry()
	config := loadedAgentConfig(t)
	configs := &fakeAgentConfigSource{agents: map[contracts.AgentID]taskruntime.AgentRuntimeConfig{"agent-default": config}}
	worker := workerID("worker-1")
	if executionStatus != contracts.TaskExecutionStatusRunning {
		worker = nil
	}
	queuedAt := now.Add(-time.Minute)
	if taskStatus != contracts.TaskStatusPending {
		queuedAt = time.Time{}
	}
	task := taskruntime.Task{TaskID: "task-terminate", AgentID: "agent-default", Status: taskStatus,
		CurrentRunID: "run-terminate", CurrentExecutionVersion: 1, DeadlineAt: now.Add(time.Minute)}
	if !queuedAt.IsZero() {
		task.QueuedAt = &queuedAt
	}
	run := taskruntime.Run{RunID: "run-terminate", TaskID: task.TaskID, Status: runStatus}
	execution := taskruntime.TaskExecution{TaskExecutionID: "execution-1", TaskID: task.TaskID,
		ExecutionVersion: 1, WorkerID: worker, Status: executionStatus,
		ExecutionConfigHash: contracts.ExecutionConfigHash(strings.Repeat("a", 64))}
	if executionStatus == contracts.TaskExecutionStatusInterrupted {
		endedAt := now.Add(-time.Minute)
		errorCode := contracts.ErrorCodeConfigVersionMismatch
		observed := contracts.ExecutionConfigHash(strings.Repeat("b", 64))
		execution.EndedAt, execution.ErrorCode, execution.ObservedConfigHash = &endedAt, &errorCode, &observed
		task.ErrorCode = &errorCode
	}
	executor.store.tasks[task.TaskID] = task
	executor.store.runs[task.TaskID] = run
	executor.store.executions[executionKey(task.TaskID, 1)] = execution
	if currentStep != nil {
		executor.store.terminationSteps[task.TaskID] = taskruntime.TerminationStep{StepID: "step-1", Status: *currentStep}
	}
	terminations := &fakeTerminationRepository{repositories: repositories}
	logs := fakeTaskLogRepository{repositories}
	cancel, err := taskruntime.NewCancelTaskService(taskruntime.CancelTaskDependencies{
		Executor: executor, Terminations: terminations, Receipts: fakeReceiptRepository{repositories},
		Reports: reports, Clock: repositories, Configs: configs, TaskLogs: logs,
		ActiveCalls: registry, Policy: lifecycle.New(),
	})
	if err != nil {
		t.Fatalf("NewCancelTaskService() error = %v", err)
	}
	expire, err := taskruntime.NewExpireTaskService(taskruntime.ExpireTaskDependencies{
		Executor: executor, Terminations: terminations, Reports: reports, Clock: repositories,
		Configs: configs, TaskLogs: logs, ActiveCalls: registry, Policy: lifecycle.New(),
	})
	if err != nil {
		t.Fatalf("NewExpireTaskService() error = %v", err)
	}
	return &terminationHarness{now: now, executor: executor, repositories: repositories,
		terminations: terminations, reports: reports, registry: registry, configs: configs,
		cancel: cancel, expire: expire}
}

func (h *terminationHarness) expireNow() {
	task := h.executor.store.tasks["task-terminate"]
	task.DeadlineAt = h.now
	h.executor.store.tasks[task.TaskID] = task
}

func (h *terminationHarness) setTool(t *testing.T, readOnly bool) contracts.ToolName {
	t.Helper()
	config := h.configs.agents["agent-default"]
	tool := config.ExecutionConfig.ToolFramework.Tools[0]
	if readOnly {
		tool.RiskLevel, tool.ReadOnly = contracts.RiskLevelLow, true
	} else {
		tool.RiskLevel, tool.ReadOnly = contracts.RiskLevelHigh, false
	}
	config.ExecutionConfig.ToolFramework.Tools[0] = tool
	h.configs.agents["agent-default"] = config
	return tool.Name
}

func cancelRequest(commandID taskruntime.CommandID) taskruntime.CancelTaskRequest {
	return taskruntime.CancelTaskRequest{CommandID: commandID, TaskID: "task-terminate", OperatorID: "operator-1"}
}

func assertTerminationFacts(t *testing.T, store *fakeStore, taskStatus contracts.TaskStatus,
	errorCode contracts.ErrorCode, reason contracts.TerminationReason, wantWorkerID *contracts.WorkerID) {
	t.Helper()
	task := store.tasks["task-terminate"]
	run := store.runs["task-terminate"]
	execution := store.executions[executionKey("task-terminate", 1)]
	if task.Status != taskStatus || task.ErrorCode == nil || *task.ErrorCode != errorCode || task.QueuedAt != nil || task.EndedAt == nil {
		t.Fatalf("Task facts = %+v", task)
	}
	if run.Status != contracts.RunStatusFailed || run.ErrorCode == nil || *run.ErrorCode != errorCode || run.EndedAt == nil {
		t.Fatalf("Run facts = %+v", run)
	}
	if execution.Status != contracts.TaskExecutionStatusFailed || execution.TerminationReason == nil ||
		*execution.TerminationReason != reason || !equalWorkerID(execution.WorkerID, wantWorkerID) || execution.EndedAt == nil {
		t.Fatalf("Execution facts = %+v", execution)
	}
	if step, exists := store.terminationSteps[task.TaskID]; exists && step.Status != contracts.StepStatusFailed {
		t.Fatalf("Step facts = %+v", step)
	}
}

func stepStatus(status contracts.StepStatus) *contracts.StepStatus { return &status }
func errorCode(code contracts.ErrorCode) *contracts.ErrorCode      { return &code }
func workerID(id contracts.WorkerID) *contracts.WorkerID           { return &id }
func equalErrorCode(left, right *contracts.ErrorCode) bool {
	if left == nil || right == nil {
		return left == nil && right == nil
	}
	return *left == *right
}
func taskWithDeadline(task taskruntime.Task, deadline time.Time) taskruntime.Task {
	task.DeadlineAt = deadline
	return task
}

func clearTaskError(task taskruntime.Task) taskruntime.Task {
	task.ErrorCode = nil
	return task
}

func waitForSignal(t *testing.T, signal <-chan struct{}, description string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	select {
	case <-signal:
	case <-ctx.Done():
		t.Fatalf("timed out waiting for %s", description)
	}
}

func waitForValue[T any](t *testing.T, values <-chan T, description string) T {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	select {
	case value := <-values:
		return value
	case <-ctx.Done():
		t.Fatalf("timed out waiting for %s", description)
		var zero T
		return zero
	}
}
