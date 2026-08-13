package checkpoint

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/zhaohaip/agentops-go/internal/contracts"
)

func TestRecoverySourceMatrix(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name       string
		phase      RecoverySourcePhase
		typeOf     InferredType
		action     contracts.CheckpointNextAction
		errorCode  contracts.ErrorCode
		configure  func(*ValidationFacts, *contracts.RuntimeContextV1)
		wantReason bool
	}{
		{name: "Initialization config mismatch", phase: RecoverySourceBeforeFirstExecution, typeOf: InferredTypeInitialization, action: contracts.CheckpointNextActionGeneratePlan, errorCode: contracts.ErrorCodeConfigVersionMismatch},
		{name: "Recovery config mismatch before first Claim", phase: RecoverySourceBeforeFirstExecution, typeOf: InferredTypeRecoveryStart, action: contracts.CheckpointNextActionGeneratePlan, errorCode: contracts.ErrorCodeConfigVersionMismatch},
		{name: "Execution Planner worker interruption", phase: RecoverySourceStartedExecution, typeOf: InferredTypeExecution, action: contracts.CheckpointNextActionGeneratePlan, errorCode: contracts.ErrorCodeWorkerInterrupted},
		{name: "Recovery Planner persistence failure", phase: RecoverySourceStartedExecution, typeOf: InferredTypeRecoveryStart, action: contracts.CheckpointNextActionGeneratePlan, errorCode: contracts.ErrorCodeResultPersistenceFailed},
		{name: "Execution Step worker interruption", phase: RecoverySourceStartedExecution, typeOf: InferredTypeExecution, action: contracts.CheckpointNextActionExecuteStep, errorCode: contracts.ErrorCodeWorkerInterrupted, configure: configureRecoveryStep},
		{name: "Recovery Step persistence failure", phase: RecoverySourceStartedExecution, typeOf: InferredTypeRecoveryStart, action: contracts.CheckpointNextActionExecuteStep, errorCode: contracts.ErrorCodeResultPersistenceFailed, configure: func(f *ValidationFacts, c *contracts.RuntimeContextV1) {
			configureRecoveryStep(f, c)
			f.Step.Type = contracts.StepTypeToolCall
			errorCode := contracts.ErrorCodeResultPersistenceFailed
			f.ToolExecution = recoveryToolExecution(f, contracts.ToolExecutionStatusFailed, &errorCode, false)
		}},
		{name: "Request Approval", phase: RecoverySourceStartedExecution, typeOf: InferredTypeExecution, action: contracts.CheckpointNextActionRequestApproval, errorCode: contracts.ErrorCodeWorkerInterrupted, configure: configureRecoveryApprovalRequest},
		{name: "Execute Approved Tool", phase: RecoverySourceStartedExecution, typeOf: InferredTypeExecution, action: contracts.CheckpointNextActionExecuteApprovedTool, errorCode: contracts.ErrorCodeWorkerInterrupted, configure: configureRecoveryApprovedTool},
		{name: "Finalize Run", phase: RecoverySourceStartedExecution, typeOf: InferredTypeExecution, action: contracts.CheckpointNextActionFinalizeRun, errorCode: contracts.ErrorCodeWorkerInterrupted, configure: configureRecoveryFinalize},
		{name: "started Recovery config mismatch", phase: RecoverySourceStartedExecution, typeOf: InferredTypeRecoveryStart, action: contracts.CheckpointNextActionExecuteStep, errorCode: contracts.ErrorCodeConfigVersionMismatch, configure: configureRecoveryStep},
		{name: "Approved Execution config mismatch", phase: RecoverySourceStartedExecution, typeOf: InferredTypeExecution, action: contracts.CheckpointNextActionExecuteApprovedTool, errorCode: contracts.ErrorCodeConfigVersionMismatch, configure: func(f *ValidationFacts, c *contracts.RuntimeContextV1) {
			configureRecoveryApprovedTool(f, c)
			startedAt := time.Now().UTC()
			f.Execution.StartedAt = &startedAt
		}},
		{name: "Recovery config mismatch rejects started Execution", phase: RecoverySourceStartedExecution, typeOf: InferredTypeRecoveryStart, action: contracts.CheckpointNextActionExecuteStep, errorCode: contracts.ErrorCodeConfigVersionMismatch, configure: func(f *ValidationFacts, c *contracts.RuntimeContextV1) {
			configureRecoveryStep(f, c)
			startedAt := time.Now().UTC()
			f.Execution.StartedAt = &startedAt
		}, wantReason: true},
		{name: "Worker interruption requires started Execution", phase: RecoverySourceStartedExecution, typeOf: InferredTypeExecution, action: contracts.CheckpointNextActionGeneratePlan, errorCode: contracts.ErrorCodeWorkerInterrupted, configure: func(f *ValidationFacts, _ *contracts.RuntimeContextV1) {
			f.Execution.StartedAt = nil
		}, wantReason: true},
		{name: "Result persistence failure requires started Execution", phase: RecoverySourceStartedExecution, typeOf: InferredTypeExecution, action: contracts.CheckpointNextActionGeneratePlan, errorCode: contracts.ErrorCodeResultPersistenceFailed, configure: func(f *ValidationFacts, _ *contracts.RuntimeContextV1) {
			f.Execution.StartedAt = nil
		}, wantReason: true},
		{name: "Initialization cannot be started source", phase: RecoverySourceStartedExecution, typeOf: InferredTypeInitialization, action: contracts.CheckpointNextActionGeneratePlan, errorCode: contracts.ErrorCodeWorkerInterrupted, wantReason: true},
		{name: "Execution cannot be before first source", phase: RecoverySourceBeforeFirstExecution, typeOf: InferredTypeExecution, action: contracts.CheckpointNextActionGeneratePlan, errorCode: contracts.ErrorCodeConfigVersionMismatch, wantReason: true},
		{name: "result persistence cannot request Approval", phase: RecoverySourceStartedExecution, typeOf: InferredTypeExecution, action: contracts.CheckpointNextActionRequestApproval, errorCode: contracts.ErrorCodeResultPersistenceFailed, configure: configureRecoveryApprovalRequest, wantReason: true},
		{name: "Tool persistence failure requires ToolExecution", phase: RecoverySourceStartedExecution, typeOf: InferredTypeExecution, action: contracts.CheckpointNextActionExecuteStep, errorCode: contracts.ErrorCodeResultPersistenceFailed, configure: func(f *ValidationFacts, c *contracts.RuntimeContextV1) {
			configureRecoveryStep(f, c)
			f.Step.Type = contracts.StepTypeToolCall
		}, wantReason: true},
		{name: "Model cannot carry ToolExecution", phase: RecoverySourceStartedExecution, typeOf: InferredTypeExecution, action: contracts.CheckpointNextActionExecuteStep, errorCode: contracts.ErrorCodeWorkerInterrupted, configure: func(f *ValidationFacts, c *contracts.RuntimeContextV1) {
			configureRecoveryStep(f, c)
			errorCode := contracts.ErrorCodeWorkerInterrupted
			f.ToolExecution = recoveryToolExecution(f, contracts.ToolExecutionStatusFailed, &errorCode, false)
		}, wantReason: true},
		{name: "UNKNOWN forbidden", phase: RecoverySourceStartedExecution, typeOf: InferredTypeExecution, action: contracts.CheckpointNextActionExecuteStep, errorCode: contracts.ErrorCodeWorkerInterrupted, configure: func(f *ValidationFacts, c *contracts.RuntimeContextV1) {
			configureRecoveryStep(f, c)
			errorCode := contracts.ErrorCodeWriteToolInterrupted
			f.ToolExecution = recoveryToolExecution(f, contracts.ToolExecutionStatusUnknown, &errorCode, true)
		}, wantReason: true},
		{name: "running Tool forbidden", phase: RecoverySourceStartedExecution, typeOf: InferredTypeExecution, action: contracts.CheckpointNextActionExecuteStep, errorCode: contracts.ErrorCodeWorkerInterrupted, configure: func(f *ValidationFacts, c *contracts.RuntimeContextV1) {
			configureRecoveryStep(f, c)
			f.ToolExecution = recoveryToolExecution(f, contracts.ToolExecutionStatusRunning, nil, false)
		}, wantReason: true},
		{name: "side effect unknown forbidden", phase: RecoverySourceStartedExecution, typeOf: InferredTypeExecution, action: contracts.CheckpointNextActionExecuteStep, errorCode: contracts.ErrorCodeWorkerInterrupted, configure: func(f *ValidationFacts, c *contracts.RuntimeContextV1) {
			configureRecoveryStep(f, c)
			errorCode := contracts.ErrorCodeWorkerInterrupted
			f.ToolExecution = recoveryToolExecution(f, contracts.ToolExecutionStatusFailed, &errorCode, true)
		}, wantReason: true},
	}

	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			facts, runtimeContext := recoveryFacts(test.phase, test.action, test.errorCode)
			if test.configure != nil {
				test.configure(&facts, &runtimeContext)
			}
			reason := validateRecoveryMatrix(runtimeContext, facts, test.typeOf, test.phase)
			if (reason != "") != test.wantReason {
				t.Fatalf("validateRecoveryMatrix() reason = %s, want invalid=%v", reason, test.wantReason)
			}
		})
	}
}

func TestClaimConfigMismatchRecoverySourceStartedAtSemantics(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name        string
		typeOf      InferredType
		facts       func() (ValidationFacts, contracts.RuntimeContextV1)
		wantStarted bool
	}{
		{name: "Recovery Start before Claim", typeOf: InferredTypeRecoveryStart, facts: recoveryStartConfigMismatchFacts},
		{name: "Approved Continuation after requeue", typeOf: InferredTypeExecution, facts: approvedContinuationConfigMismatchFacts, wantStarted: true},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			facts, runtimeContext := test.facts()
			sourceVersion := contracts.ExecutionVersion(1)
			if test.typeOf == InferredTypeRecoveryStart {
				facts.Task.CurrentExecutionVersion = 2
				facts.Execution.ExecutionVersion = 2
				runtimeContext.ExecutionVersion = 2
				sourceVersion = 2
			}
			if (facts.Execution.StartedAt != nil) != test.wantStarted {
				t.Fatalf("fixture started_at = %v, want nonnil=%v", facts.Execution.StartedAt, test.wantStarted)
			}
			manager, _ := recoveryManagerHarness(t, facts, runtimeContext, test.typeOf)
			result, err := manager.ValidateRecoverySource(context.Background(), &pointerCheckpointTestTx{}, RecoverySourceQuery{
				TaskID: "task-1", RunID: "run-1", SourceExecutionVersion: sourceVersion, Phase: RecoverySourceStartedExecution,
			})
			if _, ok := result.(ValidatedRecoverySource); err != nil || !ok {
				t.Fatalf("ValidateRecoverySource() = %#v, %v", result, err)
			}
		})
	}
}

func TestClaimConfigMismatchRejectsWrongStartedAtForSourceType(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		typeOf  InferredType
		facts   func() (ValidationFacts, contracts.RuntimeContextV1)
		mutate  func(*ValidationFacts)
		version contracts.ExecutionVersion
	}{
		{name: "started Recovery Start", typeOf: InferredTypeRecoveryStart, facts: recoveryStartConfigMismatchFacts, version: 2, mutate: func(f *ValidationFacts) {
			startedAt := time.Now().UTC()
			f.Execution.StartedAt = &startedAt
		}},
		{name: "Approved Continuation lost started_at", typeOf: InferredTypeExecution, facts: approvedContinuationConfigMismatchFacts, version: 1, mutate: func(f *ValidationFacts) {
			f.Execution.StartedAt = nil
		}},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			facts, runtimeContext := test.facts()
			if test.typeOf == InferredTypeRecoveryStart {
				facts.Task.CurrentExecutionVersion = 2
				facts.Execution.ExecutionVersion = 2
				runtimeContext.ExecutionVersion = 2
			}
			test.mutate(&facts)
			manager, _ := recoveryManagerHarness(t, facts, runtimeContext, test.typeOf)
			result, err := manager.ValidateRecoverySource(context.Background(), &pointerCheckpointTestTx{}, RecoverySourceQuery{
				TaskID: "task-1", RunID: "run-1", SourceExecutionVersion: test.version, Phase: RecoverySourceStartedExecution,
			})
			invalid, ok := result.(RecoverySourceInvalid)
			if err != nil || !ok || invalid.ReasonCode != contracts.ReasonCodeCheckpointNextActionInvalid {
				t.Fatalf("ValidateRecoverySource() = %#v, %v", result, err)
			}
		})
	}
}

func TestRecoverySourcePreservesApprovalInvariantClassification(t *testing.T) {
	t.Parallel()
	otherConfigHash := contracts.ExecutionConfigHash("bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb")
	otherFrozenHash := contracts.FrozenInputHash("bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb")
	tests := []struct {
		name       string
		mutate     func(*ValidationFacts, *contracts.RuntimeContextV1)
		invariant  bool
		wantReason contracts.ReasonCode
	}{
		{name: "Approval config hash format is damaged", mutate: func(f *ValidationFacts, _ *contracts.RuntimeContextV1) {
			f.Approval.ExecutionConfigHash = "bad"
		}, invariant: true},
		{name: "Approval frozen hash format is damaged", mutate: func(f *ValidationFacts, _ *contracts.RuntimeContextV1) {
			f.Approval.FrozenInputHash = "bad"
		}, invariant: true},
		{name: "Approval frozen hash cannot be recomputed", mutate: func(f *ValidationFacts, _ *contracts.RuntimeContextV1) {
			f.Approval.FrozenToolInput = []byte("{")
		}, invariant: true},
		{name: "Approval frozen hash differs from recomputed evidence", mutate: func(f *ValidationFacts, _ *contracts.RuntimeContextV1) {
			f.Approval.FrozenInputHash = otherFrozenHash
		}, invariant: true},
		{name: "Approval hash differs from owner TaskExecution", mutate: func(f *ValidationFacts, _ *contracts.RuntimeContextV1) {
			f.Approval.OwnerExecutionConfigHash = otherConfigHash
		}, invariant: true},
		{name: "Approval reference mismatch remains safely invalid", mutate: func(f *ValidationFacts, _ *contracts.RuntimeContextV1) {
			f.Approval.ApprovalID = "approval-other"
		}, wantReason: contracts.ReasonCodeCheckpointApprovalReferenceInvalid},
		{name: "frozen content mismatch remains safely invalid", mutate: func(_ *ValidationFacts, c *contracts.RuntimeContextV1) {
			c.ApprovalContext.FrozenToolInput = []byte(`{"replicas":4}`)
		}, wantReason: contracts.ReasonCodeCheckpointFrozenActionMismatch},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			facts, runtimeContext := recoveryFacts(
				RecoverySourceStartedExecution,
				contracts.CheckpointNextActionExecuteApprovedTool,
				contracts.ErrorCodeWorkerInterrupted,
			)
			configureRecoveryApprovedTool(&facts, &runtimeContext)
			test.mutate(&facts, &runtimeContext)
			manager, _ := recoveryManagerHarness(t, facts, runtimeContext, InferredTypeExecution)

			result, err := manager.ValidateRecoverySource(context.Background(), &pointerCheckpointTestTx{}, RecoverySourceQuery{
				TaskID: "task-1", RunID: "run-1", SourceExecutionVersion: 1, Phase: RecoverySourceStartedExecution,
			})
			if err != nil {
				t.Fatal(err)
			}
			if test.invariant {
				violation, ok := result.(RecoverySourceInvariantViolation)
				if !ok || violation.SafeReasonCode != contracts.CauseCodePersistenceInvariantViolation {
					t.Fatalf("result = %#v, want RecoverySourceInvariantViolation", result)
				}
				return
			}
			invalid, ok := result.(RecoverySourceInvalid)
			if !ok || invalid.ReasonCode != test.wantReason {
				t.Fatalf("result = %#v, want RecoverySourceInvalid/%s", result, test.wantReason)
			}
		})
	}
}

func TestRecoverySourceInfrastructureFailureUsesErrorChannel(t *testing.T) {
	t.Parallel()
	facts, runtimeContext := recoveryFacts(
		RecoverySourceStartedExecution,
		contracts.CheckpointNextActionExecuteApprovedTool,
		contracts.ErrorCodeWorkerInterrupted,
	)
	configureRecoveryApprovedTool(&facts, &runtimeContext)
	manager, repository := recoveryManagerHarness(t, facts, runtimeContext, InferredTypeExecution)
	repository.latestErr = errors.New("database unavailable")

	result, err := manager.ValidateRecoverySource(context.Background(), &pointerCheckpointTestTx{}, RecoverySourceQuery{
		TaskID: "task-1", RunID: "run-1", SourceExecutionVersion: 1, Phase: RecoverySourceStartedExecution,
	})
	if err == nil || result != nil {
		t.Fatalf("result/error = %#v/%v, want nil infrastructure error", result, err)
	}
}

func TestRecoveryCapabilityCreatesDirectSelfContainedStart(t *testing.T) {
	t.Parallel()
	tx := &pointerCheckpointTestTx{}
	facts, runtimeContext := recoveryFacts(RecoverySourceStartedExecution, contracts.CheckpointNextActionExecuteApprovedTool, contracts.ErrorCodeWorkerInterrupted)
	configureRecoveryApprovedTool(&facts, &runtimeContext)
	manager, repository := recoveryManagerHarness(t, facts, runtimeContext, InferredTypeExecution)

	result, err := manager.ValidateRecoverySource(context.Background(), tx, RecoverySourceQuery{
		TaskID: "task-1", RunID: "run-1", SourceExecutionVersion: 1, Phase: RecoverySourceStartedExecution,
	})
	validated, ok := result.(ValidatedRecoverySource)
	if err != nil || !ok || validated.SourceNextAction() != contracts.CheckpointNextActionExecuteApprovedTool {
		t.Fatalf("ValidateRecoverySource() = %#v, %v", result, err)
	}

	repository.facts.Task.CurrentExecutionVersion = 2
	repository.facts.Task.Status = contracts.TaskStatusRunning
	repository.facts.Execution.ExecutionVersion = 2
	repository.facts.Execution.Status = contracts.TaskExecutionStatusQueued
	repository.facts.Execution.WorkerID = nil
	repository.facts.Execution.ErrorCode = nil
	repository.facts.Execution.ObservedConfigHash = nil
	repository.facts.Execution.StartedAt = nil
	queuedAt := time.Now().UTC()
	repository.facts.Task.QueuedAt = &queuedAt
	repository.executionHashes[2] = checkpointTestHash

	ref, err := manager.CreateRecoveryStart(context.Background(), tx, RuntimeRecoveryStartRequest{
		TaskID: "task-1", RunID: "run-1", NewExecutionVersion: 2,
		ExecutionConfigHash: checkpointTestHash, ValidatedSource: validated,
	})
	if err != nil || ref.CheckpointSequence != 3 || len(repository.inserted) != 1 {
		t.Fatalf("CreateRecoveryStart() = %#v, %v; inserted=%d", ref, err, len(repository.inserted))
	}
	inserted := repository.inserted[0]
	if inserted.SourceExecutionVersion == nil || *inserted.SourceExecutionVersion != 1 ||
		inserted.SourceCheckpointID == nil || *inserted.SourceCheckpointID != "checkpoint-source" {
		t.Fatalf("direct source = %+v", inserted)
	}
	decoded, err := manager.codec.Decode(inserted.RuntimeContext)
	if err != nil || decoded.ExecutionVersion != 2 || decoded.ApprovalContext == nil ||
		decoded.ApprovalContext.ApprovalID != runtimeContext.ApprovalContext.ApprovalID ||
		decoded.ApprovalContext.ApprovalExecutionVersion != 1 ||
		decoded.ApprovalContext.FrozenInputHash != runtimeContext.ApprovalContext.FrozenInputHash {
		t.Fatalf("self-contained Context = %+v, %v", decoded, err)
	}

	query := RuntimeCheckpointQuery{TaskID: "task-1", RunID: "run-1", ExecutionVersion: 2}
	claimResult, err := manager.LoadLatestForClaim(context.Background(), tx, query, ClaimQueryContinuation)
	assertValidApprovedRecoveryStart(t, claimResult, err)

	workerID := contracts.WorkerID("worker-2")
	repository.facts.Task.QueuedAt = nil
	repository.facts.Execution.Status = contracts.TaskExecutionStatusRunning
	repository.facts.Execution.WorkerID = &workerID
	dispatchResult, err := manager.LoadLatestForExecutionDispatch(context.Background(), tx, query)
	assertValidApprovedRecoveryStart(t, dispatchResult, err)
	cleanupResult, err := manager.LoadLatestForStartupCleanup(context.Background(), tx, query)
	assertValidApprovedRecoveryStart(t, cleanupResult, err)
}

func TestApprovedRecoveryStartRejectsDamagedApprovalAttributionVersionAndHashes(t *testing.T) {
	t.Parallel()
	otherHash := contracts.ExecutionConfigHash("bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb")
	tests := []struct {
		name       string
		mutate     func(*recoveryFakeRepository)
		wantKind   string
		wantReason contracts.ReasonCode
	}{
		{name: "Approval belongs to another Task", mutate: func(r *recoveryFakeRepository) {
			r.facts.Approval.TaskID = "task-other"
		}, wantKind: "invalid", wantReason: contracts.ReasonCodeCheckpointApprovalReferenceInvalid},
		{name: "Approval version differs from frozen Context", mutate: func(r *recoveryFakeRepository) {
			r.facts.Approval.ExecutionVersion = 2
		}, wantKind: "invalid", wantReason: contracts.ReasonCodeCheckpointApprovalReferenceInvalid},
		{name: "Approval owner TaskExecution hash is damaged", mutate: func(r *recoveryFakeRepository) {
			r.facts.Approval.OwnerExecutionConfigHash = otherHash
		}, wantKind: "invariant"},
		{name: "Approval hash differs from current Recovery Execution", mutate: func(r *recoveryFakeRepository) {
			r.facts.Approval.ExecutionConfigHash = otherHash
			r.facts.Approval.OwnerExecutionConfigHash = otherHash
		}, wantKind: "invalid", wantReason: contracts.ReasonCodeCheckpointExecutionHashMismatch},
		{name: "Recovery Start hash differs from current TaskExecution", mutate: func(r *recoveryFakeRepository) {
			r.facts.Execution.ExecutionConfigHash = otherHash
		}, wantKind: "invalid", wantReason: contracts.ReasonCodeCheckpointExecutionHashMismatch},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			manager, repository := approvedRecoveryStartValidationHarness(t)
			test.mutate(repository)
			result, err := manager.LoadLatestForClaim(context.Background(), &pointerCheckpointTestTx{}, RuntimeCheckpointQuery{
				TaskID: "task-1", RunID: "run-1", ExecutionVersion: 2,
			}, ClaimQueryContinuation)
			if err != nil {
				t.Fatal(err)
			}
			switch test.wantKind {
			case "invalid":
				invalid, ok := result.(CheckpointInvalid)
				if !ok || invalid.ReasonCode != test.wantReason {
					t.Fatalf("result = %#v, want CheckpointInvalid/%s", result, test.wantReason)
				}
			case "invariant":
				if _, ok := result.(PersistenceInvariantViolation); !ok {
					t.Fatalf("result = %#v, want PersistenceInvariantViolation", result)
				}
			}
		})
	}
}

func TestOrdinaryApprovedContinuationStillRequiresCurrentApprovalVersion(t *testing.T) {
	t.Parallel()
	manager, _ := approvedRecoveryStartValidationHarness(t)
	managerRepository := manager.repository.(*recoveryFakeRepository)
	managerRepository.latest.SourceExecutionVersion = nil
	managerRepository.latest.SourceCheckpointID = nil

	result, err := manager.LoadLatestForClaim(context.Background(), &pointerCheckpointTestTx{}, RuntimeCheckpointQuery{
		TaskID: "task-1", RunID: "run-1", ExecutionVersion: 2,
	}, ClaimQueryContinuation)
	invalid, ok := result.(CheckpointInvalid)
	if err != nil || !ok || invalid.ReasonCode != contracts.ReasonCodeCheckpointApprovalReferenceInvalid {
		t.Fatalf("ordinary cross-version Approval result = %#v, %v", result, err)
	}
}

func TestRecoveryCapabilityRejectsCrossTransactionAndStaleSource(t *testing.T) {
	t.Parallel()
	tx := &pointerCheckpointTestTx{}
	facts, runtimeContext := recoveryFacts(RecoverySourceBeforeFirstExecution, contracts.CheckpointNextActionGeneratePlan, contracts.ErrorCodeConfigVersionMismatch)
	manager, repository := recoveryManagerHarness(t, facts, runtimeContext, InferredTypeInitialization)
	result, err := manager.ValidateRecoverySource(context.Background(), tx, RecoverySourceQuery{
		TaskID: "task-1", RunID: "run-1", SourceExecutionVersion: 1, Phase: RecoverySourceBeforeFirstExecution,
	})
	validated, ok := result.(ValidatedRecoverySource)
	if err != nil || !ok {
		t.Fatalf("ValidateRecoverySource() = %#v, %v", result, err)
	}

	request := RuntimeRecoveryStartRequest{TaskID: "task-1", RunID: "run-1", NewExecutionVersion: 2, ExecutionConfigHash: checkpointTestHash, ValidatedSource: validated}
	if _, err := manager.CreateRecoveryStart(context.Background(), otherCheckpointTestTx{}, request); !errors.Is(err, ErrInvalidDraft) {
		t.Fatalf("cross transaction error = %v", err)
	}
	repository.latest.CheckpointID = "checkpoint-newer"
	if _, err := manager.CreateRecoveryStart(context.Background(), tx, request); !errors.Is(err, ErrInvalidDraft) {
		t.Fatalf("stale source error = %v", err)
	}
}

func TestRecoverySourceUsesDamagedMaximumWithoutFallback(t *testing.T) {
	t.Parallel()
	tx := &pointerCheckpointTestTx{}
	facts, runtimeContext := recoveryFacts(RecoverySourceStartedExecution, contracts.CheckpointNextActionGeneratePlan, contracts.ErrorCodeWorkerInterrupted)
	manager, repository := recoveryManagerHarness(t, facts, runtimeContext, InferredTypeExecution)
	repository.latest.CheckpointID = "checkpoint-damaged-maximum"
	repository.latest.CheckpointSequence = 3
	repository.latest.RuntimeContext = []byte(`{}`)

	result, err := manager.ValidateRecoverySource(context.Background(), tx, RecoverySourceQuery{
		TaskID: "task-1", RunID: "run-1", SourceExecutionVersion: 1, Phase: RecoverySourceStartedExecution,
	})
	invalid, ok := result.(RecoverySourceInvalid)
	if err != nil || !ok || invalid.ReasonCode != contracts.ReasonCodeRuntimeContextMalformed || repository.findLatestCalls != 1 {
		t.Fatalf("damaged maximum result=%#v err=%v calls=%d", result, err, repository.findLatestCalls)
	}
}

func TestConsecutiveRecoveryUsesOnlyDirectLatestSource(t *testing.T) {
	t.Parallel()
	tx := &pointerCheckpointTestTx{}
	facts, runtimeContext := recoveryFacts(RecoverySourceBeforeFirstExecution, contracts.CheckpointNextActionGeneratePlan, contracts.ErrorCodeConfigVersionMismatch)
	facts.Task.CurrentExecutionVersion = 2
	facts.Execution.ExecutionVersion = 2
	runtimeContext.ExecutionVersion = 2
	manager, repository := recoveryManagerHarness(t, facts, runtimeContext, InferredTypeRecoveryStart)
	oldSource := repository.latest
	oldSource.CheckpointID = "checkpoint-v1"
	oldSource.ExecutionVersion = 1
	oldSource.CheckpointSequence = 1
	sourceVersion := contracts.ExecutionVersion(1)
	sourceID := oldSource.CheckpointID
	repository.latest.CheckpointID = "checkpoint-v2"
	repository.latest.ExecutionVersion = 2
	repository.latest.CheckpointSequence = 2
	repository.latest.SourceExecutionVersion = &sourceVersion
	repository.latest.SourceCheckpointID = &sourceID
	repository.byID[sourceID] = oldSource
	repository.executionHashes[1] = checkpointTestHash
	repository.executionHashes[2] = checkpointTestHash

	result, err := manager.ValidateRecoverySource(context.Background(), tx, RecoverySourceQuery{
		TaskID: "task-1", RunID: "run-1", SourceExecutionVersion: 2, Phase: RecoverySourceBeforeFirstExecution,
	})
	validated, ok := result.(ValidatedRecoverySource)
	if err != nil || !ok {
		t.Fatalf("second ValidateRecoverySource() = %#v, %v", result, err)
	}
	repository.facts.Task.CurrentExecutionVersion = 3
	repository.facts.Task.Status = contracts.TaskStatusPending
	repository.facts.Task.ErrorCode = nil
	queuedAt := time.Now().UTC()
	repository.facts.Task.QueuedAt = &queuedAt
	repository.facts.Execution.ExecutionVersion = 3
	repository.facts.Execution.Status = contracts.TaskExecutionStatusQueued
	repository.facts.Execution.ErrorCode = nil
	repository.facts.Execution.ObservedConfigHash = nil
	repository.executionHashes[3] = checkpointTestHash
	if _, err := manager.CreateRecoveryStart(context.Background(), tx, RuntimeRecoveryStartRequest{
		TaskID: "task-1", RunID: "run-1", NewExecutionVersion: 3, ExecutionConfigHash: checkpointTestHash, ValidatedSource: validated,
	}); err != nil {
		t.Fatal(err)
	}
	inserted := repository.inserted[0]
	if inserted.SourceExecutionVersion == nil || *inserted.SourceExecutionVersion != 2 ||
		inserted.SourceCheckpointID == nil || *inserted.SourceCheckpointID != "checkpoint-v2" {
		t.Fatalf("second Recovery source = %+v", inserted)
	}
}

func TestDirectRecoverySourceValidation(t *testing.T) {
	t.Parallel()
	source := Entity{CheckpointID: "checkpoint-v1", TaskID: "task-1", RunID: "run-1", ExecutionVersion: 1, CheckpointSequence: 2, ExecutionConfigHash: checkpointTestHash}
	sourceVersion := contracts.ExecutionVersion(1)
	sourceID := source.CheckpointID
	recovery := Entity{CheckpointID: "checkpoint-v2", TaskID: "task-1", RunID: "run-1", ExecutionVersion: 2, CheckpointSequence: 3, ExecutionConfigHash: checkpointTestHash, SourceExecutionVersion: &sourceVersion, SourceCheckpointID: &sourceID}
	tests := []struct {
		name   string
		mutate func(*recoveryFakeRepository, *Entity)
	}{
		{name: "source missing", mutate: func(r *recoveryFakeRepository, _ *Entity) { delete(r.byID, "checkpoint-v1") }},
		{name: "source cross Task", mutate: func(r *recoveryFakeRepository, _ *Entity) {
			value := r.byID["checkpoint-v1"]
			value.TaskID = "task-other"
			r.byID["checkpoint-v1"] = value
		}},
		{name: "source cross Run", mutate: func(r *recoveryFakeRepository, _ *Entity) {
			value := r.byID["checkpoint-v1"]
			value.RunID = "run-other"
			r.byID["checkpoint-v1"] = value
		}},
		{name: "source hash mismatch", mutate: func(r *recoveryFakeRepository, _ *Entity) {
			value := r.byID["checkpoint-v1"]
			value.ExecutionConfigHash = contracts.ExecutionConfigHash("bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb")
			r.byID["checkpoint-v1"] = value
		}},
		{name: "source is not earlier", mutate: func(r *recoveryFakeRepository, _ *Entity) {
			value := r.byID["checkpoint-v1"]
			value.CheckpointSequence = 3
			r.byID["checkpoint-v1"] = value
		}},
		{name: "version not direct predecessor", mutate: func(_ *recoveryFakeRepository, value *Entity) {
			version := contracts.ExecutionVersion(0)
			value.SourceExecutionVersion = &version
		}},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			base := &fakeCheckpointRepository{}
			repository := &recoveryFakeRepository{fakeCheckpointRepository: base, byID: map[contracts.CheckpointID]Entity{"checkpoint-v1": source}, executionHashes: map[contracts.ExecutionVersion]contracts.ExecutionConfigHash{1: checkpointTestHash}}
			candidate := recovery
			test.mutate(repository, &candidate)
			manager := &Manager{repository: repository}
			valid, err := manager.directRecoverySourceValid(context.Background(), &pointerCheckpointTestTx{}, candidate)
			if err != nil || valid {
				t.Fatalf("directRecoverySourceValid() = %v, %v", valid, err)
			}
		})
	}
}

type otherCheckpointTestTx struct{}

func (otherCheckpointTestTx) AgentOpsRuntimeWriteTx() {}

type pointerCheckpointTestTx struct{}

func (*pointerCheckpointTestTx) AgentOpsRuntimeWriteTx() {}

type recoveryFakeRepository struct {
	*fakeCheckpointRepository
	byID            map[contracts.CheckpointID]Entity
	executionHashes map[contracts.ExecutionVersion]contracts.ExecutionConfigHash
}

func (r *recoveryFakeRepository) InsertCheckpoint(ctx context.Context, tx contracts.RuntimeWriteTx, entity Entity) (time.Time, error) {
	if r.latest.CheckpointID != "" {
		r.byID[r.latest.CheckpointID] = r.latest
	}
	return r.fakeCheckpointRepository.InsertCheckpoint(ctx, tx, entity)
}

func (r *recoveryFakeRepository) AllocateNextSequence(context.Context, contracts.RuntimeWriteTx, contracts.RunID) (int64, error) {
	return r.latest.CheckpointSequence + 1, nil
}

func (r *recoveryFakeRepository) FindByID(_ context.Context, _ contracts.RuntimeWriteTx, checkpointID contracts.CheckpointID) (Entity, error) {
	entity, ok := r.byID[checkpointID]
	if !ok {
		return Entity{}, ErrRepositoryNotFound
	}
	return entity, nil
}

func (r *recoveryFakeRepository) LoadTaskExecution(_ context.Context, _ contracts.RuntimeWriteTx, taskID contracts.TaskID, version contracts.ExecutionVersion) (TaskExecutionHash, error) {
	hash, ok := r.executionHashes[version]
	if !ok {
		return TaskExecutionHash{}, ErrPersistenceInvariantViolation
	}
	return TaskExecutionHash{TaskID: taskID, ExecutionVersion: version, ExecutionConfigHash: hash}, nil
}

func recoveryManagerHarness(t *testing.T, facts ValidationFacts, runtimeContext contracts.RuntimeContextV1, typeOf InferredType) (*Manager, *recoveryFakeRepository) {
	t.Helper()
	codec, err := NewRuntimeContextCodec(RuntimeContextCodecLimits{MaxBytes: 64 * 1024, MaxDepth: 32})
	if err != nil {
		t.Fatal(err)
	}
	document, err := codec.Encode(runtimeContext)
	if err != nil {
		t.Fatal(err)
	}
	entity := Entity{CheckpointID: "checkpoint-source", TaskID: "task-1", RunID: "run-1", ExecutionVersion: runtimeContext.ExecutionVersion, CheckpointSequence: 1, RuntimeContext: document, ExecutionConfigHash: checkpointTestHash}
	if typeOf == InferredTypeExecution {
		entity.CheckpointSequence = 2
	}
	byID := map[contracts.CheckpointID]Entity{}
	executionHashes := map[contracts.ExecutionVersion]contracts.ExecutionConfigHash{runtimeContext.ExecutionVersion: checkpointTestHash}
	if typeOf == InferredTypeRecoveryStart {
		sourceVersion := runtimeContext.ExecutionVersion - 1
		sourceID := contracts.CheckpointID("checkpoint-direct-source")
		entity.CheckpointSequence = 2
		entity.SourceExecutionVersion = &sourceVersion
		entity.SourceCheckpointID = &sourceID
		byID[sourceID] = Entity{CheckpointID: sourceID, TaskID: entity.TaskID, RunID: entity.RunID, ExecutionVersion: sourceVersion, CheckpointSequence: 1, ExecutionConfigHash: checkpointTestHash}
		executionHashes[sourceVersion] = checkpointTestHash
	}
	base := &fakeCheckpointRepository{facts: facts, latest: entity}
	repository := &recoveryFakeRepository{fakeCheckpointRepository: base, byID: byID, executionHashes: executionHashes}
	manager, err := NewManager(repository, codec)
	if err != nil {
		t.Fatal(err)
	}
	return manager, repository
}

func approvedRecoveryStartValidationHarness(t *testing.T) (*Manager, *recoveryFakeRepository) {
	t.Helper()
	facts, runtimeContext := recoveryFacts(RecoverySourceStartedExecution, contracts.CheckpointNextActionExecuteApprovedTool, contracts.ErrorCodeWorkerInterrupted)
	configureRecoveryApprovedTool(&facts, &runtimeContext)
	facts.Task.CurrentExecutionVersion = 2
	facts.Task.Status = contracts.TaskStatusRunning
	queuedAt := time.Now().UTC()
	facts.Task.QueuedAt = &queuedAt
	facts.Execution.ExecutionVersion = 2
	facts.Execution.Status = contracts.TaskExecutionStatusQueued
	facts.Execution.WorkerID = nil
	facts.Execution.ErrorCode = nil
	facts.Execution.StartedAt = nil
	runtimeContext.ExecutionVersion = 2
	return recoveryManagerHarness(t, facts, runtimeContext, InferredTypeRecoveryStart)
}

func assertValidApprovedRecoveryStart(t *testing.T, result ValidationResult, err error) {
	t.Helper()
	valid, ok := result.(ValidCheckpoint)
	if err != nil || !ok || valid.InferredType != InferredTypeRecoveryStart || valid.Checkpoint.Context.ApprovalContext == nil ||
		valid.Checkpoint.Context.ApprovalContext.ApprovalExecutionVersion != 1 {
		t.Fatalf("Recovery Start result = %#v, %v", result, err)
	}
}

func recoveryFacts(phase RecoverySourcePhase, action contracts.CheckpointNextAction, errorCode contracts.ErrorCode) (ValidationFacts, contracts.RuntimeContextV1) {
	startedAt := time.Now().UTC()
	observedHash := checkpointTestHash
	taskError := errorCode
	facts := ValidationFacts{
		Task:      TaskFact{TaskID: "task-1", Status: contracts.TaskStatusRunning, CurrentRunID: "run-1", CurrentExecutionVersion: 1},
		Run:       RunFact{RunID: "run-1", TaskID: "task-1", Status: contracts.RunStatusRunning},
		Execution: ExecutionFact{TaskID: "task-1", ExecutionVersion: 1, Status: contracts.TaskExecutionStatusInterrupted, ExecutionConfigHash: checkpointTestHash, ErrorCode: &errorCode, StartedAt: &startedAt},
	}
	if phase == RecoverySourceBeforeFirstExecution {
		facts.Task.Status = contracts.TaskStatusInterrupted
		facts.Task.ErrorCode = &taskError
		facts.Run.Status = contracts.RunStatusPending
		facts.Execution.StartedAt = nil
		facts.Execution.ObservedConfigHash = &observedHash
	} else if errorCode == contracts.ErrorCodeConfigVersionMismatch {
		facts.Task.Status = contracts.TaskStatusInterrupted
		facts.Task.ErrorCode = &taskError
		facts.Execution.ObservedConfigHash = &observedHash
		facts.Execution.StartedAt = nil
	}
	return facts, contracts.RuntimeContextV1{SchemaVersion: 1, TaskID: "task-1", RunID: "run-1", ExecutionVersion: 1, NextAction: action, ResolvedReferences: contracts.CanonicalResolvedReferences{}}
}

func recoveryStartConfigMismatchFacts() (ValidationFacts, contracts.RuntimeContextV1) {
	facts, runtimeContext := recoveryFacts(
		RecoverySourceStartedExecution,
		contracts.CheckpointNextActionExecuteStep,
		contracts.ErrorCodeConfigVersionMismatch,
	)
	configureRecoveryStep(&facts, &runtimeContext)
	return facts, runtimeContext
}

func approvedContinuationConfigMismatchFacts() (ValidationFacts, contracts.RuntimeContextV1) {
	// Claim 首次启动 Execution，写入 started_at 并进入高风险 Tool 的审批边界。
	startedAt := time.Now().UTC()
	facts := ValidationFacts{
		Task: TaskFact{
			TaskID: "task-1", Status: contracts.TaskStatusRunning,
			CurrentRunID: "run-1", CurrentExecutionVersion: 1,
		},
		Run: RunFact{RunID: "run-1", TaskID: "task-1", Status: contracts.RunStatusRunning},
		Execution: ExecutionFact{
			TaskID: "task-1", ExecutionVersion: 1, Status: contracts.TaskExecutionStatusRunning,
			ExecutionConfigHash: checkpointTestHash, StartedAt: &startedAt,
		},
	}
	runtimeContext := contracts.RuntimeContextV1{
		SchemaVersion: 1, TaskID: "task-1", RunID: "run-1", ExecutionVersion: 1,
		NextAction:         contracts.CheckpointNextActionRequestApproval,
		ResolvedReferences: contracts.CanonicalResolvedReferences{},
	}
	configureRecoveryApprovalRequest(&facts, &runtimeContext)

	// Step Executor 提交完整 WaitingApproval 现场。
	facts.Task.Status = contracts.TaskStatusWaitingApproval
	facts.Run.Status = contracts.RunStatusWaitingApproval
	facts.Execution.Status = contracts.TaskExecutionStatusWaitingApproval
	facts.Step.Status = contracts.StepStatusWaitingApproval

	// Approval Manager 批准后保存同版本 Approved Continuation 并重新排队，不覆盖 started_at。
	configureRecoveryApprovedTool(&facts, &runtimeContext)
	facts.Task.Status = contracts.TaskStatusRunning
	facts.Run.Status = contracts.RunStatusRunning
	facts.Execution.Status = contracts.TaskExecutionStatusQueued
	facts.Step.Status = contracts.StepStatusRunning
	runtimeContext.NextAction = contracts.CheckpointNextActionExecuteApprovedTool

	// Approve 后的重新领取因配置变化中断，必须保留首次 Claim 写入的 started_at。
	errorCode := contracts.ErrorCodeConfigVersionMismatch
	observedHash := contracts.ExecutionConfigHash("bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb")
	facts.Task.Status = contracts.TaskStatusInterrupted
	facts.Task.ErrorCode = &errorCode
	facts.Task.QueuedAt = nil
	facts.Execution.Status = contracts.TaskExecutionStatusInterrupted
	facts.Execution.ErrorCode = &errorCode
	facts.Execution.ObservedConfigHash = &observedHash
	return facts, runtimeContext
}

func configureRecoveryStep(facts *ValidationFacts, runtimeContext *contracts.RuntimeContextV1) {
	planID := contracts.PlanID("plan-1")
	stepID := contracts.StepID("step-1")
	facts.Run.PlanID, facts.Run.CurrentStepID = &planID, &stepID
	facts.Plan = &PlanFact{PlanID: planID, RunID: "run-1"}
	facts.Step = &StepFact{StepID: stepID, RunID: "run-1", PlanID: planID, Sequence: 1, Type: contracts.StepTypeModelCall, Status: contracts.StepStatusRunning, Input: json.RawMessage(`{}`)}
	runtimeContext.PlanID, runtimeContext.CurrentStepID = &planID, &stepID
}

func configureRecoveryApprovalRequest(facts *ValidationFacts, runtimeContext *contracts.RuntimeContextV1) {
	configureRecoveryStep(facts, runtimeContext)
	facts.Step.Type = contracts.StepTypeToolCall
	facts.Step.ToolName = "kubernetes.patch_deployment"
}

func configureRecoveryApprovedTool(facts *ValidationFacts, runtimeContext *contracts.RuntimeContextV1) {
	configureRecoveryApprovalRequest(facts, runtimeContext)
	input := contracts.FrozenToolInput(`{"replicas":3}`)
	observed := contracts.ObservedValues(`{"replicas":2}`)
	resourceVersion := contracts.ResourceVersion("42")
	frozenHash, _ := contracts.ComputeFrozenInputHashV1(contracts.FrozenApprovedToolInputV1{
		Schema: contracts.FrozenApprovedToolInputSchemaV1, Version: contracts.FrozenApprovedToolInputVersionV1,
		ToolName: facts.Step.ToolName, ToolInput: input, ObservedValues: observed, ResourceVersion: resourceVersion,
	})
	runtimeContext.ApprovalContext = &contracts.ApprovalContext{ApprovalID: "approval-1", ApprovalExecutionVersion: 1, ToolName: facts.Step.ToolName, FrozenToolInput: input, ObservedValues: observed, ResourceVersion: resourceVersion, FrozenInputHash: frozenHash}
	facts.Approval = &ApprovalFact{ApprovalID: "approval-1", TaskID: "task-1", RunID: "run-1", StepID: facts.Step.StepID, ExecutionVersion: 1, ExecutionConfigHash: checkpointTestHash, OwnerExecutionConfigHash: checkpointTestHash, Status: contracts.ApprovalStatusApproved, ToolName: facts.Step.ToolName, FrozenToolInput: input, ObservedValues: observed, ResourceVersion: resourceVersion, FrozenInputHash: frozenHash}
}

func configureRecoveryFinalize(facts *ValidationFacts, runtimeContext *contracts.RuntimeContextV1) {
	configureRecoveryStep(facts, runtimeContext)
	facts.Step.Status = contracts.StepStatusCompleted
}

func recoveryToolExecution(facts *ValidationFacts, status contracts.ToolExecutionStatus, errorCode *contracts.ErrorCode, unknown bool) *ToolExecutionFact {
	return &ToolExecutionFact{ToolExecutionID: "tool-execution-1", TaskID: "task-1", RunID: "run-1", StepID: facts.Step.StepID, ExecutionVersion: 1, Status: status, ErrorCode: errorCode, SideEffectUnknown: unknown}
}
