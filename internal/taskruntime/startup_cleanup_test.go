package taskruntime_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/zhaohaip/agentops-go/internal/contracts"
	"github.com/zhaohaip/agentops-go/internal/lifecycle"
	"github.com/zhaohaip/agentops-go/internal/taskruntime"
)

func TestStartupCleanupClassifiesLegacyExternalCalls(t *testing.T) {
	tests := []struct {
		name       string
		configure  func(*startupCleanupHarness)
		terminal   bool
		wantTool   contracts.ToolExecutionStatus
		wantError  contracts.ErrorCode
		wantReport bool
	}{
		{name: "Planner"},
		{name: "Model", configure: func(h *startupCleanupHarness) { h.withModelStep() }},
		{name: "before ToolExecution", configure: func(h *startupCleanupHarness) { h.withToolBoundary(false) }},
		{name: "approved write Tool before ToolExecution", configure: func(h *startupCleanupHarness) { h.withToolBoundary(true) }},
		{name: "running read Tool", configure: func(h *startupCleanupHarness) { h.withRunningTool(false) },
			wantTool: contracts.ToolExecutionStatusFailed, wantError: contracts.ErrorCodeWorkerInterrupted},
		{name: "running write Tool", configure: func(h *startupCleanupHarness) { h.withRunningTool(true) }, terminal: true,
			wantTool: contracts.ToolExecutionStatusUnknown, wantError: contracts.ErrorCodeWriteToolInterrupted, wantReport: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			h := newStartupCleanupHarness(t)
			if test.configure != nil {
				test.configure(h)
			}
			summary, err := h.service.StartupCleanup(context.Background(), h.currentWorker)
			if err != nil {
				t.Fatalf("StartupCleanup() error = %v", err)
			}
			if summary.Inspected != 1 || summary.Terminalized != boolInt(test.terminal) ||
				summary.Interrupted != boolInt(!test.terminal) {
				t.Fatalf("summary = %+v", summary)
			}
			facts := h.snapshotFacts()
			if test.terminal {
				assertStartupTerminal(t, facts, test.wantError)
			} else {
				if facts.Execution.Status != contracts.TaskExecutionStatusInterrupted ||
					facts.Task.Status != contracts.TaskStatusRunning || facts.Run.Status != contracts.RunStatusRunning {
					t.Fatalf("interrupt facts = %+v", facts)
				}
			}
			if facts.Execution.WorkerID == nil || *facts.Execution.WorkerID != h.oldWorker {
				t.Fatalf("worker_id = %v, want preserved %q", facts.Execution.WorkerID, h.oldWorker)
			}
			if test.wantTool != "" && (facts.ToolExecution == nil || facts.ToolExecution.Status != test.wantTool) {
				t.Fatalf("ToolExecution = %+v, want status %s", facts.ToolExecution, test.wantTool)
			}
			if got := len(h.executor.snapshot().reports); got != boolInt(test.wantReport) {
				t.Fatalf("reports = %d, want %d", got, boolInt(test.wantReport))
			}
		})
	}
}

func TestStartupCleanupFrozenToolActionMismatchTerminalizesCheckpointInvalid(t *testing.T) {
	tests := []struct {
		name       string
		configure  func(*startupCleanupHarness)
		wantReason contracts.ReasonCode
	}{
		{name: "High write with EXECUTE_STEP", configure: func(h *startupCleanupHarness) {
			h.withToolBoundary(true)
			h.mutateFacts(func(f *taskruntime.StartupCleanupFacts) { f.ApprovedRecovery = nil })
			h.checkpoints.startupOverrides[h.taskID] = h.validCheckpoint(contracts.CheckpointNextActionExecuteStep)
		}, wantReason: contracts.ReasonCodeCheckpointFrozenActionMismatch},
		{name: "Low read with REQUEST_APPROVAL", configure: func(h *startupCleanupHarness) {
			h.withToolBoundary(false)
			h.checkpoints.startupOverrides[h.taskID] = h.validCheckpoint(contracts.CheckpointNextActionRequestApproval)
		}, wantReason: contracts.ReasonCodeCheckpointFrozenActionMismatch},
		{name: "EXECUTE_APPROVED_TOOL missing direct Approval", configure: func(h *startupCleanupHarness) {
			h.withToolBoundary(true)
			h.mutateFacts(func(f *taskruntime.StartupCleanupFacts) { f.ApprovedRecovery = nil })
		}, wantReason: contracts.ReasonCodeCheckpointApprovalReferenceInvalid},
		{name: "EXECUTE_APPROVED_TOOL wrong direct Approval", configure: func(h *startupCleanupHarness) {
			h.withToolBoundary(true)
			h.mutateFacts(func(f *taskruntime.StartupCleanupFacts) { f.ApprovedRecovery.ApprovalID = "other-approval" })
		}, wantReason: contracts.ReasonCodeCheckpointApprovalReferenceInvalid},
		{name: "EXECUTE_APPROVED_TOOL Approval is not Approved", configure: func(h *startupCleanupHarness) {
			h.withToolBoundary(true)
			h.mutateFacts(func(f *taskruntime.StartupCleanupFacts) {
				f.ApprovedRecovery.Status = contracts.ApprovalStatusPending
			})
		}, wantReason: contracts.ReasonCodeCheckpointApprovalReferenceInvalid},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			h := newStartupCleanupHarness(t)
			test.configure(h)

			summary, err := h.service.StartupCleanup(context.Background(), h.currentWorker)
			if err != nil || summary.Terminalized != 1 {
				t.Fatalf("StartupCleanup() = %+v, %v", summary, err)
			}
			facts := h.snapshotFacts()
			assertStartupTerminal(t, facts, contracts.ErrorCodeCheckpointInvalid)
			applications := h.executor.snapshot().startupApplications
			if len(applications) != 1 || applications[0].CheckpointReasonCode == nil ||
				*applications[0].CheckpointReasonCode != test.wantReason {
				t.Fatalf("Checkpoint reason applications = %+v, want %s", applications, test.wantReason)
			}
			if got := len(h.executor.snapshot().reports); got != 1 {
				t.Fatalf("reports = %d, want 1", got)
			}
		})
	}
}

func TestStartupCleanupApprovalContextDamageTerminalizesWithStableReason(t *testing.T) {
	validOtherHash := contracts.FrozenInputHash("bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb")
	tests := []struct {
		name       string
		configure  func(*startupCleanupHarness)
		wantReason contracts.ReasonCode
	}{
		{name: "zero Approval execution version", configure: func(h *startupCleanupHarness) {
			h.mutateStartupCheckpoint(func(c *taskruntime.RuntimeCheckpoint) { c.ApprovalContext.ApprovalExecutionVersion = 0 })
		}, wantReason: contracts.ReasonCodeCheckpointApprovalReferenceInvalid},
		{name: "future Approval execution version", configure: func(h *startupCleanupHarness) {
			h.mutateStartupCheckpoint(func(c *taskruntime.RuntimeCheckpoint) {
				c.ApprovalContext.ApprovalExecutionVersion = c.ExecutionVersion + 1
			})
		}, wantReason: contracts.ReasonCodeCheckpointApprovalReferenceInvalid},
		{name: "Approval execution version differs from actual Approval", configure: func(h *startupCleanupHarness) {
			h.mutateFacts(func(f *taskruntime.StartupCleanupFacts) {
				f.Task.CurrentExecutionVersion = 2
				f.Execution.ExecutionVersion = 2
			})
			h.mutateStartupCheckpoint(func(c *taskruntime.RuntimeCheckpoint) {
				c.ExecutionVersion = 2
				c.ApprovalContext.ApprovalExecutionVersion = 2
				sourceVersion := contracts.ExecutionVersion(2)
				c.SourceExecutionVersion = &sourceVersion
			})
		}, wantReason: contracts.ReasonCodeCheckpointApprovalReferenceInvalid},
		{name: "empty ResourceVersion", configure: func(h *startupCleanupHarness) {
			h.mutateStartupCheckpoint(func(c *taskruntime.RuntimeCheckpoint) { c.ApprovalContext.ResourceVersion = "" })
		}, wantReason: contracts.ReasonCodeCheckpointApprovalReferenceInvalid},
		{name: "malformed input Hash", configure: func(h *startupCleanupHarness) {
			h.mutateStartupCheckpoint(func(c *taskruntime.RuntimeCheckpoint) { c.ApprovalContext.FrozenInputHash = "bad" })
		}, wantReason: contracts.ReasonCodeCheckpointApprovalReferenceInvalid},
		{name: "different valid input Hash", configure: func(h *startupCleanupHarness) {
			h.mutateStartupCheckpoint(func(c *taskruntime.RuntimeCheckpoint) { c.ApprovalContext.FrozenInputHash = validOtherHash })
		}, wantReason: contracts.ReasonCodeCheckpointFrozenInputHashMismatch},
		{name: "malformed frozen input", configure: func(h *startupCleanupHarness) {
			h.mutateStartupCheckpoint(func(c *taskruntime.RuntimeCheckpoint) { c.ApprovalContext.FrozenToolInput = []byte("{") })
		}, wantReason: contracts.ReasonCodeCheckpointApprovalReferenceInvalid},
		{name: "different frozen input", configure: func(h *startupCleanupHarness) {
			h.mutateStartupCheckpoint(func(c *taskruntime.RuntimeCheckpoint) {
				c.ApprovalContext.FrozenToolInput = []byte(`{"resource":"deployment/other"}`)
			})
		}, wantReason: contracts.ReasonCodeCheckpointApprovalReferenceInvalid},
		{name: "malformed ObservedValues", configure: func(h *startupCleanupHarness) {
			h.mutateStartupCheckpoint(func(c *taskruntime.RuntimeCheckpoint) { c.ApprovalContext.ObservedValues = []byte("{") })
		}, wantReason: contracts.ReasonCodeCheckpointApprovalReferenceInvalid},
		{name: "different ObservedValues", configure: func(h *startupCleanupHarness) {
			h.mutateStartupCheckpoint(func(c *taskruntime.RuntimeCheckpoint) { c.ApprovalContext.ObservedValues = []byte(`{"replicas":2}`) })
		}, wantReason: contracts.ReasonCodeCheckpointApprovalReferenceInvalid},
		{name: "ResourceVersion differs from Approval", configure: func(h *startupCleanupHarness) {
			h.mutateFacts(func(f *taskruntime.StartupCleanupFacts) { f.ApprovedRecovery.ResourceVersion = "43" })
		}, wantReason: contracts.ReasonCodeCheckpointApprovalReferenceInvalid},
		{name: "actual Approval input Hash differs", configure: func(h *startupCleanupHarness) {
			h.mutateFacts(func(f *taskruntime.StartupCleanupFacts) { f.ApprovedRecovery.FrozenInputHash = validOtherHash })
		}, wantReason: contracts.ReasonCodeCheckpointFrozenInputHashMismatch},
		{name: "actual Approval frozen input differs", configure: func(h *startupCleanupHarness) {
			h.mutateFacts(func(f *taskruntime.StartupCleanupFacts) {
				f.ApprovedRecovery.FrozenToolInput = []byte(`{"resource":"deployment/other"}`)
			})
		}, wantReason: contracts.ReasonCodeCheckpointApprovalReferenceInvalid},
		{name: "actual Approval ObservedValues differ", configure: func(h *startupCleanupHarness) {
			h.mutateFacts(func(f *taskruntime.StartupCleanupFacts) { f.ApprovedRecovery.ObservedValues = []byte(`{"replicas":2}`) })
		}, wantReason: contracts.ReasonCodeCheckpointApprovalReferenceInvalid},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			h := newStartupCleanupHarness(t)
			h.withToolBoundary(true)
			test.configure(h)
			summary, err := h.service.StartupCleanup(context.Background(), h.currentWorker)
			if err != nil || summary.Terminalized != 1 {
				t.Fatalf("StartupCleanup() = %+v, %v", summary, err)
			}
			assertStartupTerminal(t, h.snapshotFacts(), contracts.ErrorCodeCheckpointInvalid)
			applications := h.executor.snapshot().startupApplications
			if len(applications) != 1 || applications[0].CheckpointReasonCode == nil ||
				*applications[0].CheckpointReasonCode != test.wantReason {
				t.Fatalf("applications = %+v, want reason %s", applications, test.wantReason)
			}
		})
	}
}

func TestStartupCleanupStaticToolContradictionAndUnownedObjectAreRuntimeFatal(t *testing.T) {
	tests := []struct {
		name      string
		configure func(*startupCleanupHarness)
	}{
		{name: "Low non-read-only static capability", configure: func(h *startupCleanupHarness) {
			h.withToolBoundary(false)
			h.config.ExecutionConfig.ToolFramework.Tools[0].ReadOnly = false
			h.syncConfigHash()
		}},
		{name: "ToolExecution belongs to another Task", configure: func(h *startupCleanupHarness) {
			h.withRunningTool(false)
			h.mutateFacts(func(f *taskruntime.StartupCleanupFacts) { f.ToolExecution.TaskID = "other-task" })
		}},
		{name: "direct Approval belongs to another Task", configure: func(h *startupCleanupHarness) {
			h.withToolBoundary(true)
			h.mutateFacts(func(f *taskruntime.StartupCleanupFacts) { f.ApprovedRecovery.TaskID = "other-task" })
		}},
		{name: "actual Approval version is out of bounds", configure: func(h *startupCleanupHarness) {
			h.withToolBoundary(true)
			h.mutateFacts(func(f *taskruntime.StartupCleanupFacts) {
				f.ApprovedRecovery.ApprovalExecutionVersion = f.Execution.ExecutionVersion + 1
			})
		}},
		{name: "actual Approval frozen input is malformed", configure: func(h *startupCleanupHarness) {
			h.withToolBoundary(true)
			h.mutateFacts(func(f *taskruntime.StartupCleanupFacts) { f.ApprovedRecovery.FrozenToolInput = []byte("{") })
		}},
		{name: "actual Approval ObservedValues are malformed", configure: func(h *startupCleanupHarness) {
			h.withToolBoundary(true)
			h.mutateFacts(func(f *taskruntime.StartupCleanupFacts) { f.ApprovedRecovery.ObservedValues = []byte("{") })
		}},
		{name: "actual Approval ResourceVersion is empty", configure: func(h *startupCleanupHarness) {
			h.withToolBoundary(true)
			h.mutateFacts(func(f *taskruntime.StartupCleanupFacts) { f.ApprovedRecovery.ResourceVersion = "" })
		}},
		{name: "actual Approval input Hash is malformed", configure: func(h *startupCleanupHarness) {
			h.withToolBoundary(true)
			h.mutateFacts(func(f *taskruntime.StartupCleanupFacts) { f.ApprovedRecovery.FrozenInputHash = "bad" })
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			h := newStartupCleanupHarness(t)
			test.configure(h)
			if _, err := h.service.StartupCleanup(context.Background(), h.currentWorker); err == nil {
				t.Fatal("StartupCleanup() error = nil, want Runtime Fatal")
			}
			store := h.executor.snapshot()
			if store.startupFacts[0].Execution.Status != contracts.TaskExecutionStatusRunning ||
				len(store.startupApplications) != 0 || len(store.reports) != 0 {
				t.Fatalf("Runtime Fatal path partially committed: %+v", store.startupFacts[0])
			}
		})
	}
}

func TestStartupCleanupDeadlineHasPriorityAndPreservesWriteToolUnknown(t *testing.T) {
	h := newStartupCleanupHarness(t)
	h.withRunningTool(true)
	h.mutateFacts(func(facts *taskruntime.StartupCleanupFacts) { facts.Task.DeadlineAt = h.now })
	h.checkpoints.failStartup = errors.New("checkpoint must not be loaded")

	summary, err := h.service.StartupCleanup(context.Background(), h.currentWorker)
	if err != nil {
		t.Fatalf("StartupCleanup() error = %v", err)
	}
	if summary.Terminalized != 1 {
		t.Fatalf("summary = %+v", summary)
	}
	facts := h.snapshotFacts()
	assertStartupTerminal(t, facts, contracts.ErrorCodeTaskTimeout)
	if facts.Execution.ErrorCode == nil || *facts.Execution.ErrorCode != contracts.ErrorCodeWriteToolInterrupted ||
		facts.Execution.TerminationReason == nil || *facts.Execution.TerminationReason != contracts.TerminationReasonTimedOut ||
		facts.ToolExecution == nil || facts.ToolExecution.Status != contracts.ToolExecutionStatusUnknown ||
		!facts.ToolExecution.SideEffectUnknown {
		t.Fatalf("expired write facts = %+v", facts)
	}
	h.checkpoints.mu.Lock()
	defer h.checkpoints.mu.Unlock()
	if len(h.checkpoints.seenTx) != 0 {
		t.Fatal("deadline path loaded Checkpoint")
	}
}

func TestStartupCleanupExpiredNonWriteActionUsesTimeoutAndIdempotentReport(t *testing.T) {
	h := newStartupCleanupHarness(t)
	h.withRunningTool(false)
	h.mutateFacts(func(facts *taskruntime.StartupCleanupFacts) { facts.Task.DeadlineAt = h.now.Add(-time.Nanosecond) })
	h.executor.store.reports = []contracts.EnsurePendingReportRequest{{
		TaskID: h.taskID, RunID: h.snapshotFacts().Run.RunID, CreatedAt: h.now.Add(-time.Minute),
	}}

	summary, err := h.service.StartupCleanup(context.Background(), h.currentWorker)
	if err != nil || summary.Terminalized != 1 {
		t.Fatalf("StartupCleanup() = %+v, %v", summary, err)
	}
	facts := h.snapshotFacts()
	assertStartupTerminal(t, facts, contracts.ErrorCodeTaskTimeout)
	if facts.Execution.ErrorCode != nil || facts.Execution.TerminationReason == nil ||
		*facts.Execution.TerminationReason != contracts.TerminationReasonTimedOut || facts.ToolExecution == nil ||
		facts.ToolExecution.Status != contracts.ToolExecutionStatusFailed || facts.ToolExecution.ErrorCode == nil ||
		*facts.ToolExecution.ErrorCode != contracts.ErrorCodeTaskTimeout || facts.ToolExecution.SideEffectUnknown {
		t.Fatalf("expired read facts = %+v", facts)
	}
	if got := len(h.executor.snapshot().reports); got != 1 {
		t.Fatalf("reports = %d, want idempotent 1", got)
	}
}

func TestStartupCleanupCheckpointInvalidTerminalizesAtomically(t *testing.T) {
	h := newStartupCleanupHarness(t)
	h.withModelStep()
	reason := contracts.ReasonCodeCheckpointReferenceMissing
	h.checkpoints.startupOverrides[h.taskID] = taskruntime.StartupCleanupCheckpointInvalid{ReasonCode: reason}

	summary, err := h.service.StartupCleanup(context.Background(), h.currentWorker)
	if err != nil || summary.Terminalized != 1 {
		t.Fatalf("StartupCleanup() = %+v, %v", summary, err)
	}
	facts := h.snapshotFacts()
	assertStartupTerminal(t, facts, contracts.ErrorCodeCheckpointInvalid)
	applications := h.executor.snapshot().startupApplications
	if len(applications) != 1 || applications[0].CheckpointReasonCode == nil ||
		*applications[0].CheckpointReasonCode != reason {
		t.Fatalf("applications = %+v", applications)
	}
	h.repositories.mu.Lock()
	clockTx := h.repositories.operationTx["clock.now"][0]
	lockTx := h.repositories.operationTx["startup_cleanup.lock"][0]
	applyTx := h.repositories.operationTx["startup_cleanup.apply"][0]
	h.repositories.mu.Unlock()
	h.checkpoints.mu.Lock()
	checkpointTx := h.checkpoints.seenTx[0]
	h.checkpoints.mu.Unlock()
	h.reports.mu.Lock()
	reportTx := h.reports.seenTx[0]
	h.reports.mu.Unlock()
	if clockTx != lockTx || lockTx != applyTx || applyTx != checkpointTx || checkpointTx != reportTx {
		t.Fatal("StartupCleanup did not use one RuntimeWriteTx for facts, Checkpoint, cleanup and Report")
	}
}

func TestStartupCleanupImpossibleFactsAndFailuresRollbackWholeTransaction(t *testing.T) {
	tests := []struct {
		name      string
		configure func(*startupCleanupHarness)
	}{
		{name: "nil legacy worker", configure: func(h *startupCleanupHarness) {
			h.mutateFacts(func(f *taskruntime.StartupCleanupFacts) { f.Execution.WorkerID = nil })
		}},
		{name: "current version mismatch", configure: func(h *startupCleanupHarness) {
			h.mutateFacts(func(f *taskruntime.StartupCleanupFacts) { f.Task.CurrentExecutionVersion++ })
		}},
		{name: "checkpoint attribution mismatch", configure: func(h *startupCleanupHarness) {
			result := h.validCheckpoint(contracts.CheckpointNextActionGeneratePlan)
			result.Checkpoint.RunID = "other-run"
			h.checkpoints.startupOverrides[h.taskID] = result
		}},
		{name: "checkpoint provider failure", configure: func(h *startupCleanupHarness) {
			h.checkpoints.failStartup = errors.New("checkpoint unavailable")
		}},
		{name: "approved source hash mismatch", configure: func(h *startupCleanupHarness) {
			h.withToolBoundary(true)
			h.mutateFacts(func(f *taskruntime.StartupCleanupFacts) {
				f.ApprovedRecovery.SourceExecutionConfigHash = contracts.ExecutionConfigHash("mismatched-hash")
			})
		}},
		{name: "conditional apply miss", configure: func(h *startupCleanupHarness) {
			h.repositories.missOperation["startup_cleanup.apply"] = true
		}},
		{name: "pending report failure", configure: func(h *startupCleanupHarness) {
			h.withRunningTool(true)
			h.reports.fail = errors.New("report unavailable")
		}},
		{name: "later impossible candidate rolls back earlier cleanup", configure: func(h *startupCleanupHarness) {
			second := h.snapshotFacts()
			second.Task.TaskID = "startup-task-2"
			second.Execution.TaskID = second.Task.TaskID
			second.Run.TaskID = second.Task.TaskID
			second.Execution.WorkerID = nil
			h.executor.store.startupFacts = append(h.executor.store.startupFacts, second)
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			h := newStartupCleanupHarness(t)
			test.configure(h)
			before := h.snapshotFacts()
			if _, err := h.service.StartupCleanup(context.Background(), h.currentWorker); err == nil {
				t.Fatal("StartupCleanup() error = nil")
			}
			after := h.snapshotFacts()
			if after.Execution.Status != before.Execution.Status || after.Task.Status != before.Task.Status ||
				len(h.executor.snapshot().startupApplications) != 0 || len(h.executor.snapshot().reports) != 0 {
				t.Fatalf("rollback failed: before=%+v after=%+v", before, after)
			}
		})
	}
}

func TestStartupCleanupQueuedRunningTaskRollsBackPlannerAndModelCandidates(t *testing.T) {
	tests := []struct {
		name      string
		withModel bool
	}{
		{name: "Planner"},
		{name: "Model", withModel: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			h := newStartupCleanupHarness(t)
			if test.withModel {
				h.withModelStep()
			}

			invalid := h.snapshotFacts()
			invalid.Task.TaskID = "queued-running-task"
			invalid.Task.CurrentRunID = "queued-running-run"
			queuedAt := h.now.Add(-time.Minute)
			invalid.Task.QueuedAt = &queuedAt
			invalid.Run.TaskID = invalid.Task.TaskID
			invalid.Run.RunID = invalid.Task.CurrentRunID
			invalid.Execution.TaskExecutionID = "queued-running-execution"
			invalid.Execution.TaskID = invalid.Task.TaskID
			if invalid.Step != nil {
				stepID := contracts.StepID("queued-running-model-step")
				invalid.Step.StepID = stepID
				invalid.Run.CurrentStepID = &stepID
			}
			h.executor.store.startupFacts = append(h.executor.store.startupFacts, invalid)
			action := contracts.CheckpointNextActionGeneratePlan
			if test.withModel {
				action = contracts.CheckpointNextActionExecuteStep
			}
			h.checkpoints.startupOverrides[invalid.Task.TaskID] = taskruntime.StartupCleanupCheckpointValid{
				Checkpoint: taskruntime.RuntimeCheckpoint{
					CheckpointID: "queued-running-checkpoint", TaskID: invalid.Task.TaskID, RunID: invalid.Run.RunID,
					ExecutionVersion:    invalid.Execution.ExecutionVersion,
					ExecutionConfigHash: invalid.Execution.ExecutionConfigHash,
					NextAction:          action, CheckpointSequence: 1,
				},
			}

			if _, err := h.service.StartupCleanup(context.Background(), h.currentWorker); err == nil {
				t.Fatal("StartupCleanup() error = nil")
			}
			store := h.executor.snapshot()
			if len(store.startupFacts) != 2 ||
				store.startupFacts[0].Execution.Status != contracts.TaskExecutionStatusRunning ||
				store.startupFacts[1].Execution.Status != contracts.TaskExecutionStatusRunning ||
				store.startupFacts[1].Task.QueuedAt == nil || !store.startupFacts[1].Task.QueuedAt.Equal(queuedAt) ||
				len(store.startupApplications) != 0 || len(store.reports) != 0 {
				t.Fatalf("StartupCleanup partially committed or repaired queued_at: %+v", store.startupFacts)
			}
		})
	}
}

func TestStartupCleanupIgnoresNonRunningExecutions(t *testing.T) {
	h := newStartupCleanupHarness(t)
	h.mutateFacts(func(f *taskruntime.StartupCleanupFacts) {
		f.Execution.Status = contracts.TaskExecutionStatusWaitingApproval
		f.Execution.WorkerID = nil
	})
	summary, err := h.service.StartupCleanup(context.Background(), h.currentWorker)
	if err != nil || summary != (taskruntime.StartupCleanupSummary{}) {
		t.Fatalf("StartupCleanup() = %+v, %v", summary, err)
	}
}

type startupCleanupHarness struct {
	t             *testing.T
	now           time.Time
	taskID        contracts.TaskID
	oldWorker     contracts.WorkerID
	currentWorker contracts.WorkerID
	config        taskruntime.AgentRuntimeConfig
	executor      *fakeExecutor
	repositories  *fakeRepositories
	checkpoints   *fakeCheckpointPort
	reports       *fakePendingReportWriter
	configs       *fakeAgentConfigSource
	service       *taskruntime.StartupCleanupService
}

func newStartupCleanupHarness(t *testing.T) *startupCleanupHarness {
	t.Helper()
	now := time.Date(2026, 8, 6, 12, 0, 0, 0, time.UTC)
	config := loadedAgentConfig(t)
	hash, err := taskruntime.HashExecutionConfigV1(config.ExecutionConfig)
	if err != nil {
		t.Fatalf("HashExecutionConfigV1() error = %v", err)
	}
	taskID := contracts.TaskID("startup-task")
	runID := contracts.RunID("startup-run")
	oldWorker := contracts.WorkerID("old-worker")
	executor := newFakeExecutor()
	repositories := newFakeRepositories(executor, now)
	facts := taskruntime.StartupCleanupFacts{
		Task: taskruntime.Task{TaskID: taskID, AgentID: config.ExecutionConfig.Agent.AgentID, Status: contracts.TaskStatusRunning,
			CurrentRunID: runID, CurrentExecutionVersion: 1, DeadlineAt: now.Add(time.Hour)},
		Run: taskruntime.Run{RunID: runID, TaskID: taskID, Status: contracts.RunStatusRunning},
		Execution: taskruntime.TaskExecution{TaskExecutionID: "startup-execution", TaskID: taskID, ExecutionVersion: 1,
			WorkerID: &oldWorker, Status: contracts.TaskExecutionStatusRunning, ExecutionConfigHash: hash},
	}
	executor.store.startupFacts = []taskruntime.StartupCleanupFacts{facts}
	checkpoints := &fakeCheckpointPort{startupOverrides: make(map[contracts.TaskID]taskruntime.StartupCleanupCheckpointResult)}
	reports := &fakePendingReportWriter{}
	configs := &fakeAgentConfigSource{agents: map[contracts.AgentID]taskruntime.AgentRuntimeConfig{config.ExecutionConfig.Agent.AgentID: config}}
	service, err := taskruntime.NewStartupCleanupService(taskruntime.StartupCleanupDependencies{
		Executor: executor, Repository: &fakeStartupCleanupRepository{repositories: repositories}, Checkpoints: checkpoints,
		Reports: reports, Clock: repositories, Configs: configs, Policy: lifecycle.New(),
	})
	if err != nil {
		t.Fatalf("NewStartupCleanupService() error = %v", err)
	}
	h := &startupCleanupHarness{t: t, now: now, taskID: taskID, oldWorker: oldWorker, currentWorker: "current-worker",
		config: config, executor: executor, repositories: repositories, checkpoints: checkpoints, reports: reports,
		configs: configs, service: service}
	checkpoints.startupOverrides[taskID] = h.validCheckpoint(contracts.CheckpointNextActionGeneratePlan)
	return h
}

func (h *startupCleanupHarness) validCheckpoint(action contracts.CheckpointNextAction) taskruntime.StartupCleanupCheckpointValid {
	facts := h.snapshotFacts()
	return taskruntime.StartupCleanupCheckpointValid{Checkpoint: taskruntime.RuntimeCheckpoint{
		CheckpointID: "startup-checkpoint", TaskID: facts.Task.TaskID, RunID: facts.Run.RunID,
		ExecutionVersion: facts.Execution.ExecutionVersion, ExecutionConfigHash: facts.Execution.ExecutionConfigHash,
		NextAction: action, CheckpointSequence: 1,
	}}
}

func (h *startupCleanupHarness) mutateStartupCheckpoint(mutate func(*taskruntime.RuntimeCheckpoint)) {
	result, ok := h.checkpoints.startupOverrides[h.taskID].(taskruntime.StartupCleanupCheckpointValid)
	if !ok {
		h.t.Fatal("StartupCleanup Checkpoint is not valid")
	}
	mutate(&result.Checkpoint)
	h.checkpoints.startupOverrides[h.taskID] = result
}

func (h *startupCleanupHarness) mutateFacts(mutate func(*taskruntime.StartupCleanupFacts)) {
	mutate(&h.executor.store.startupFacts[0])
}

func (h *startupCleanupHarness) snapshotFacts() taskruntime.StartupCleanupFacts {
	return h.executor.snapshot().startupFacts[0]
}

func (h *startupCleanupHarness) withModelStep() {
	stepID := contracts.StepID("model-step")
	h.mutateFacts(func(f *taskruntime.StartupCleanupFacts) {
		f.Run.CurrentStepID = &stepID
		f.Step = &taskruntime.StartupCleanupStep{StepID: stepID, Type: contracts.StepTypeModelCall, Status: contracts.StepStatusRunning}
	})
	h.checkpoints.startupOverrides[h.taskID] = h.validCheckpoint(contracts.CheckpointNextActionExecuteStep)
}

func (h *startupCleanupHarness) withToolBoundary(approved bool) {
	tool := h.config.ExecutionConfig.ToolFramework.Tools[0]
	if approved {
		h.makeToolWrite()
		tool = h.config.ExecutionConfig.ToolFramework.Tools[0]
		h.mutateFacts(func(f *taskruntime.StartupCleanupFacts) {
			f.Task.CurrentExecutionVersion = 2
			f.Execution.ExecutionVersion = 2
		})
	}
	stepID := contracts.StepID("tool-step")
	h.mutateFacts(func(f *taskruntime.StartupCleanupFacts) {
		f.Run.CurrentStepID = &stepID
		f.Step = &taskruntime.StartupCleanupStep{StepID: stepID, Type: contracts.StepTypeToolCall, Status: contracts.StepStatusRunning, ToolName: tool.Name}
	})
	action := contracts.CheckpointNextActionExecuteStep
	checkpoint := h.validCheckpoint(action)
	if approved {
		approvalContext := validApprovalContext(tool.Name)
		sourceVersion := approvalContext.ApprovalExecutionVersion
		sourceCheckpointID := contracts.CheckpointID("source-checkpoint")
		checkpoint.Checkpoint.NextAction = contracts.CheckpointNextActionExecuteApprovedTool
		checkpoint.Checkpoint.ApprovalContext = approvalContext
		checkpoint.Checkpoint.SourceExecutionVersion = &sourceVersion
		checkpoint.Checkpoint.SourceCheckpointID = &sourceCheckpointID
		h.mutateFacts(func(f *taskruntime.StartupCleanupFacts) {
			f.ApprovedRecovery = &taskruntime.StartupCleanupApprovedRecovery{
				ApprovalID: approvalContext.ApprovalID, TaskID: f.Task.TaskID, Status: contracts.ApprovalStatusApproved,
				ApprovalExecutionVersion:  sourceVersion,
				ApprovalConfigHash:        f.Execution.ExecutionConfigHash,
				SourceExecutionConfigHash: f.Execution.ExecutionConfigHash, ToolName: tool.Name,
				FrozenToolInput: append(contracts.FrozenToolInput(nil), approvalContext.FrozenToolInput...),
				ObservedValues:  append(contracts.ObservedValues(nil), approvalContext.ObservedValues...),
				ResourceVersion: approvalContext.ResourceVersion, FrozenInputHash: approvalContext.FrozenInputHash,
			}
		})
	}
	h.checkpoints.startupOverrides[h.taskID] = checkpoint
}

func (h *startupCleanupHarness) withRunningTool(write bool) {
	h.withToolBoundary(write)
	if !write {
		h.checkpoints.startupOverrides[h.taskID] = h.validCheckpoint(contracts.CheckpointNextActionExecuteStep)
	}
	h.mutateFacts(func(f *taskruntime.StartupCleanupFacts) {
		f.ToolExecution = &taskruntime.StartupCleanupToolExecution{ToolExecutionID: "tool-execution", TaskID: f.Task.TaskID,
			StepID: f.Step.StepID, ExecutionVersion: f.Execution.ExecutionVersion, ToolName: f.Step.ToolName,
			Status: contracts.ToolExecutionStatusRunning}
	})
}

func (h *startupCleanupHarness) makeToolWrite() {
	h.config.ExecutionConfig.ToolFramework.Tools[0].RiskLevel = contracts.RiskLevelHigh
	h.config.ExecutionConfig.ToolFramework.Tools[0].ReadOnly = false
	h.syncConfigHash()
}

func (h *startupCleanupHarness) syncConfigHash() {
	hash, err := taskruntime.HashExecutionConfigV1(h.config.ExecutionConfig)
	if err != nil {
		h.t.Fatalf("HashExecutionConfigV1() error = %v", err)
	}
	h.configs.agents[h.config.ExecutionConfig.Agent.AgentID] = h.config
	h.mutateFacts(func(f *taskruntime.StartupCleanupFacts) { f.Execution.ExecutionConfigHash = hash })
}

func assertStartupTerminal(t *testing.T, facts taskruntime.StartupCleanupFacts, taskError contracts.ErrorCode) {
	t.Helper()
	if facts.Task.Status != contracts.TaskStatusFailed || facts.Run.Status != contracts.RunStatusFailed ||
		facts.Execution.Status != contracts.TaskExecutionStatusFailed || facts.Task.ErrorCode == nil || *facts.Task.ErrorCode != taskError {
		t.Fatalf("terminal facts = %+v", facts)
	}
}

func boolInt(value bool) int {
	if value {
		return 1
	}
	return 0
}
