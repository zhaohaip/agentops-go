package postgres_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	postgrescheckpoint "github.com/zhaohaip/agentops-go/internal/adapter/postgres/checkpoint"
	postgresruntime "github.com/zhaohaip/agentops-go/internal/adapter/postgres/runtime"
	postgrestaskruntime "github.com/zhaohaip/agentops-go/internal/adapter/postgres/taskruntime"
	checkpoint "github.com/zhaohaip/agentops-go/internal/checkpoint"
	"github.com/zhaohaip/agentops-go/internal/contracts"
	domain "github.com/zhaohaip/agentops-go/internal/taskruntime"
	"github.com/zhaohaip/agentops-go/test/integration/postgres/postgrestest"
)

func TestCheckpointRepositoryContract(t *testing.T) {
	postgrestest.RunRepositoryContract(t, postgrestest.RepositoryContract{
		Name:       "Checkpoint",
		Migrations: checkpointMigrationSet(),
		Cases: []postgrestest.RepositoryCase{
			{Name: "sequence commit rollback reuse and latest", Run: testCheckpointSequenceAndLatest},
			{Name: "maximum damaged record never falls back", Run: testCheckpointMaximumDamaged},
			{Name: "hash attribution and transaction token errors", Run: testCheckpointGuards},
			{Name: "execution ValidationFacts projections", Run: testCheckpointExecutionValidationFacts},
		},
	})
}

func testCheckpointExecutionValidationFacts(t *testing.T, environment *postgrestest.RepositoryEnvironment) {
	installCheckpointValidationFactTables(t, environment)
	manager, checkpointRepository, taskRepositories := checkpointTestComponents(t, environment)

	tests := []struct {
		name      string
		action    contracts.CheckpointNextAction
		stepType  contracts.StepType
		stepState contracts.StepStatus
		withPrior bool
		mutate    func(context.Context, contracts.RuntimeWriteTx) error
		reason    contracts.ReasonCode
	}{
		{name: "EXECUTE_STEP", action: contracts.CheckpointNextActionExecuteStep, stepType: contracts.StepTypeModelCall, stepState: contracts.StepStatusRunning, withPrior: true},
		{name: "REQUEST_APPROVAL", action: contracts.CheckpointNextActionRequestApproval, stepType: contracts.StepTypeToolCall, stepState: contracts.StepStatusRunning, withPrior: true},
		{name: "FINALIZE_RUN with completed ToolExecution", action: contracts.CheckpointNextActionFinalizeRun, stepType: contracts.StepTypeToolCall, stepState: contracts.StepStatusCompleted,
			mutate: insertCheckpointToolExecution(contracts.ToolExecutionStatusCompleted, nil, false)},
		{name: "Plan cross Run", action: contracts.CheckpointNextActionExecuteStep, stepType: contracts.StepTypeModelCall, stepState: contracts.StepStatusRunning, withPrior: true,
			mutate: func(ctx context.Context, tx contracts.RuntimeWriteTx) error {
				return execCheckpointFixtureSQL(ctx, tx, `UPDATE plan SET run_id='run-other'`)
			}, reason: contracts.ReasonCodeCheckpointPlanReferenceInvalid},
		{name: "Step missing", action: contracts.CheckpointNextActionExecuteStep, stepType: contracts.StepTypeModelCall, stepState: contracts.StepStatusRunning, withPrior: true,
			mutate: func(ctx context.Context, tx contracts.RuntimeWriteTx) error {
				return execCheckpointFixtureSQL(ctx, tx, `DELETE FROM step WHERE step_id='step-current'`)
			}, reason: contracts.ReasonCodeCheckpointStepReferenceInvalid},
		{name: "reference mismatch", action: contracts.CheckpointNextActionExecuteStep, stepType: contracts.StepTypeModelCall, stepState: contracts.StepStatusRunning, withPrior: true,
			mutate: func(ctx context.Context, tx contracts.RuntimeWriteTx) error {
				return execCheckpointFixtureSQL(ctx, tx, `UPDATE step SET input='{"payload":"literal"}'::jsonb WHERE step_id='step-current'`)
			}, reason: contracts.ReasonCodeCheckpointReferenceExtra},
		{name: "ToolExecution UNKNOWN without error", action: contracts.CheckpointNextActionExecuteStep, stepType: contracts.StepTypeToolCall, stepState: contracts.StepStatusRunning,
			mutate: insertCheckpointToolExecution(contracts.ToolExecutionStatusUnknown, nil, true), reason: contracts.ReasonCodeCheckpointNextActionInvalid},
		{name: "ToolExecution cross attribution", action: contracts.CheckpointNextActionExecuteStep, stepType: contracts.StepTypeToolCall, stepState: contracts.StepStatusRunning,
			mutate: func(ctx context.Context, tx contracts.RuntimeWriteTx) error {
				if err := insertCheckpointToolExecution(contracts.ToolExecutionStatusRunning, nil, false)(ctx, tx); err != nil {
					return err
				}
				return execCheckpointFixtureSQL(ctx, tx, `UPDATE tool_execution SET run_id='run-other'`)
			}, reason: contracts.ReasonCodeCheckpointNextActionInvalid},
		{name: "ToolExecution FAILED without error", action: contracts.CheckpointNextActionExecuteStep, stepType: contracts.StepTypeToolCall, stepState: contracts.StepStatusRunning,
			mutate: insertCheckpointToolExecution(contracts.ToolExecutionStatusFailed, nil, false), reason: contracts.ReasonCodeCheckpointNextActionInvalid},
		{name: "FINALIZE_RUN rejects non-last Step", action: contracts.CheckpointNextActionFinalizeRun, stepType: contracts.StepTypeVerification, stepState: contracts.StepStatusCompleted,
			mutate: func(ctx context.Context, tx contracts.RuntimeWriteTx) error {
				return execCheckpointFixtureSQL(ctx, tx, `INSERT INTO step (step_id,run_id,plan_id,sequence,type,status,input,output_schema,output) SELECT 'step-later',run_id,plan_id,sequence+1,'Verification','Pending','{}','{}',NULL FROM step WHERE step_id='step-current'`)
			}, reason: contracts.ReasonCodeCheckpointNextActionInvalid},
	}

	for index, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			suffix := fmt.Sprintf("facts-%d", index)
			now := time.Now().UTC()
			graph := repositoryGraph(suffix, now, now)
			graph.task.Status = contracts.TaskStatusRunning
			graph.task.QueuedAt = nil
			graph.run.Status = contracts.RunStatusRunning
			worker := contracts.WorkerID("worker-facts")
			graph.execution.Status = contracts.TaskExecutionStatusRunning
			graph.execution.WorkerID = &worker
			graph.execution.StartedAt = &now
			insertRepositoryGraph(t, environment, taskRepositories, graph)

			planID := contracts.PlanID("plan-1")
			stepID := contracts.StepID("step-current")
			graph.run.PlanID, graph.run.CurrentStepID = &planID, &stepID
			err := environment.Runtime.WriteExecutor().Execute(context.Background(), func(ctx context.Context, tx contracts.RuntimeWriteTx) error {
				updated, err := taskRepositories.Runs.Update(ctx, tx, domain.RunUpdate{
					RunID: graph.run.RunID, TaskID: graph.task.TaskID, ExecutionVersion: 1,
					ExpectedStatus: contracts.RunStatusRunning, Status: contracts.RunStatusRunning,
					PlanID: &planID, CurrentStepID: &stepID, Context: json.RawMessage(`{}`), StartedAt: &now,
				})
				if err != nil || !updated {
					return fmt.Errorf("update Run location = %v: %w", updated, err)
				}
				if err := insertCheckpointValidationFacts(ctx, tx, graph, test.stepType, test.stepState, test.withPrior); err != nil {
					return err
				}
				if test.mutate != nil {
					return test.mutate(ctx, tx)
				}
				return nil
			})
			if err != nil {
				t.Fatalf("prepare ValidationFacts: %v", err)
			}

			references := contracts.CanonicalResolvedReferences{}
			if test.withPrior {
				key := "payload"
				references = contracts.CanonicalResolvedReferences{{TargetPath: []contracts.ReferencePathSegment{{Kind: contracts.ReferencePathSegmentKey, Key: &key}}, SourceStepID: "step-previous", SourceOutputField: "result"}}
			}
			request := checkpoint.RuntimeCheckpointSaveRequest{
				TaskID: graph.task.TaskID, RunID: graph.run.RunID, ExecutionVersion: 1,
				ExecutionConfigHash: graph.execution.ExecutionConfigHash,
				Draft: checkpoint.ExecutionDraft{PlanID: &planID, CurrentStepID: &stepID, NextAction: test.action,
					ResolvedReferences: references},
			}
			err = environment.Runtime.WriteExecutor().Execute(context.Background(), func(ctx context.Context, tx contracts.RuntimeWriteTx) error {
				if _, err := taskRepositories.Runs.LockByTask(ctx, tx, graph.task.TaskID); err != nil {
					return err
				}
				_, err := manager.SaveRuntimeCheckpoint(ctx, tx, request)
				return err
			})
			if test.reason == "" {
				if err != nil {
					t.Fatalf("SaveRuntimeCheckpoint() error = %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), string(test.reason)) {
				t.Fatalf("error = %v, want %s", err, test.reason)
			}
		})
	}
	testCheckpointApprovedFacts(t, environment, manager, checkpointRepository, taskRepositories)

	t.Run("Approval and ToolExecution projection", func(t *testing.T) {
		now := time.Now().UTC()
		graph := repositoryGraph("facts-projection", now, now)
		insertRepositoryGraph(t, environment, taskRepositories, graph)
		err := environment.Runtime.WriteExecutor().Execute(context.Background(), func(ctx context.Context, tx contracts.RuntimeWriteTx) error {
			if err := insertCheckpointValidationFacts(ctx, tx, graph, contracts.StepTypeToolCall, contracts.StepStatusRunning, false); err != nil {
				return err
			}
			if err := insertCheckpointApproval(ctx, tx, graph); err != nil {
				return err
			}
			if err := execCheckpointFixtureSQL(ctx, tx, `INSERT INTO tool_execution (tool_execution_id,task_id,run_id,step_id,execution_version,status,error_code,side_effect_unknown) VALUES ('tool-execution-1','`+string(graph.task.TaskID)+`','`+string(graph.run.RunID)+`','step-current',1,'UNKNOWN','WRITE_TOOL_INTERRUPTED',true)`); err != nil {
				return err
			}
			approvalID := contracts.ApprovalID("approval-1")
			planID := contracts.PlanID("plan-1")
			stepID := contracts.StepID("step-current")
			facts, err := checkpointRepository.LoadValidationFacts(ctx, tx, checkpoint.ValidationFactsRequest{
				TaskID: graph.task.TaskID, RunID: graph.run.RunID, ExecutionVersion: 1,
				PlanID: &planID, CurrentStepID: &stepID, ApprovalID: &approvalID,
			})
			if err != nil {
				return err
			}
			if facts.Approval == nil || facts.Approval.Status != contracts.ApprovalStatusApproved {
				return fmt.Errorf("Approval projection = %#v", facts.Approval)
			}
			if facts.Approval.ExecutionConfigHash != graph.execution.ExecutionConfigHash ||
				facts.Approval.OwnerExecutionConfigHash != graph.execution.ExecutionConfigHash ||
				facts.Approval.FrozenInputHash != checkpointApprovalContext().FrozenInputHash ||
				facts.Approval.ToolName != checkpointApprovalContext().ToolName {
				return fmt.Errorf("Approval frozen projection = %#v", facts.Approval)
			}
			if facts.ToolExecution == nil || facts.ToolExecution.Status != contracts.ToolExecutionStatusUnknown ||
				facts.ToolExecution.ErrorCode == nil || *facts.ToolExecution.ErrorCode != contracts.ErrorCodeWriteToolInterrupted ||
				!facts.ToolExecution.SideEffectUnknown {
				return fmt.Errorf("ToolExecution projection = %#v", facts.ToolExecution)
			}
			return nil
		})
		if err != nil {
			t.Fatalf("load Approval/ToolExecution projections: %v", err)
		}
	})
}

func testCheckpointApprovedFacts(t *testing.T, environment *postgrestest.RepositoryEnvironment, manager *checkpoint.Manager, repository *postgrescheckpoint.Repository, taskRepositories *postgrestaskruntime.Repositories) {
	t.Helper()
	tests := []struct {
		name        string
		mutate      func(context.Context, contracts.RuntimeWriteTx) error
		contextMut  func(*contracts.ApprovalContext)
		reason      contracts.ReasonCode
		invariant   bool
		oldApproval bool
	}{
		{name: "valid Approved continuation"},
		{name: "ordinary continuation rejects old Approval", oldApproval: true, reason: contracts.ReasonCodeCheckpointApprovalReferenceInvalid},
		{name: "Step tool differs from Approval and Context", mutate: func(ctx context.Context, tx contracts.RuntimeWriteTx) error {
			return execCheckpointFixtureSQL(ctx, tx, `UPDATE step SET tool_name='tool.other' WHERE step_id='step-current'`)
		}, reason: contracts.ReasonCodeCheckpointFrozenActionMismatch},
		{name: "cross Run Approval", mutate: func(ctx context.Context, tx contracts.RuntimeWriteTx) error {
			return execCheckpointFixtureSQL(ctx, tx, `UPDATE approval SET run_id='run-other'`)
		}, reason: contracts.ReasonCodeCheckpointApprovalReferenceInvalid},
		{name: "different frozen input", contextMut: func(value *contracts.ApprovalContext) {
			value.FrozenToolInput = contracts.FrozenToolInput(`{"replicas":3}`)
		}, reason: contracts.ReasonCodeCheckpointFrozenActionMismatch},
		{name: "different observed values", contextMut: func(value *contracts.ApprovalContext) {
			value.ObservedValues = contracts.ObservedValues(`{"replicas":9}`)
		}, reason: contracts.ReasonCodeCheckpointFrozenActionMismatch},
		{name: "different resource version", contextMut: func(value *contracts.ApprovalContext) {
			value.ResourceVersion = "43"
		}, reason: contracts.ReasonCodeCheckpointFrozenActionMismatch},
		{name: "different frozen hash", contextMut: func(value *contracts.ApprovalContext) {
			value.FrozenInputHash = contracts.FrozenInputHash(strings.Repeat("b", 64))
		}, reason: contracts.ReasonCodeCheckpointFrozenInputHashMismatch},
		{name: "Approval self frozen hash damaged", mutate: func(ctx context.Context, tx contracts.RuntimeWriteTx) error {
			return execCheckpointFixtureSQL(ctx, tx, `UPDATE approval SET frozen_input_hash='bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb'`)
		}, invariant: true},
		{name: "Approval owner execution hash damaged", mutate: func(ctx context.Context, tx contracts.RuntimeWriteTx) error {
			return execCheckpointFixtureSQL(ctx, tx, `UPDATE approval SET execution_config_hash='bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb'`)
		}, invariant: true},
		{name: "completed ToolExecution conflicts", mutate: insertCheckpointToolExecution(contracts.ToolExecutionStatusCompleted, nil, false), reason: contracts.ReasonCodeCheckpointApprovalReferenceInvalid},
		{name: "UNKNOWN ToolExecution invalid combination", mutate: insertCheckpointToolExecution(contracts.ToolExecutionStatusUnknown, nil, true), reason: contracts.ReasonCodeCheckpointApprovalReferenceInvalid},
	}
	for index, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			now := time.Now().UTC()
			graph := repositoryGraph(fmt.Sprintf("approval-facts-%d", index), now, now)
			graph.task.Status, graph.run.Status = contracts.TaskStatusRunning, contracts.RunStatusRunning
			graph.task.QueuedAt = nil
			worker := contracts.WorkerID("worker-approved")
			graph.execution.Status, graph.execution.WorkerID, graph.execution.StartedAt = contracts.TaskExecutionStatusRunning, &worker, &now
			insertRepositoryGraph(t, environment, taskRepositories, graph)
			planID, stepID := contracts.PlanID("plan-1"), contracts.StepID("step-current")
			approvalContext := checkpointApprovalContext()
			runtimeVersion := contracts.ExecutionVersion(1)
			if test.oldApproval {
				runtimeVersion = 2
			}
			if test.contextMut != nil {
				test.contextMut(&approvalContext)
			}
			err := environment.Runtime.WriteExecutor().Execute(context.Background(), func(ctx context.Context, tx contracts.RuntimeWriteTx) error {
				if test.oldApproval {
					if err := prepareSecondCheckpointExecution(ctx, tx, graph, now); err != nil {
						return err
					}
				}
				updated, err := taskRepositories.Runs.Update(ctx, tx, domain.RunUpdate{
					RunID: graph.run.RunID, TaskID: graph.task.TaskID, ExecutionVersion: runtimeVersion,
					ExpectedStatus: contracts.RunStatusRunning, Status: contracts.RunStatusRunning,
					PlanID: &planID, CurrentStepID: &stepID, Context: json.RawMessage(`{}`), StartedAt: &now,
				})
				if err != nil || !updated {
					return fmt.Errorf("update approved Run location = %v: %w", updated, err)
				}
				if err := insertCheckpointValidationFacts(ctx, tx, graph, contracts.StepTypeToolCall, contracts.StepStatusRunning, false); err != nil {
					return err
				}
				if err := insertCheckpointApproval(ctx, tx, graph); err != nil {
					return err
				}
				if test.mutate != nil {
					if err := test.mutate(ctx, tx); err != nil {
						return err
					}
				}
				codec, err := checkpoint.NewRuntimeContextCodec(checkpoint.RuntimeContextCodecLimits{MaxBytes: 64 * 1024, MaxDepth: 32})
				if err != nil {
					return err
				}
				raw, err := codec.Encode(contracts.RuntimeContextV1{
					SchemaVersion: 1, TaskID: graph.task.TaskID, RunID: graph.run.RunID, ExecutionVersion: runtimeVersion,
					PlanID: &planID, CurrentStepID: &stepID, NextAction: contracts.CheckpointNextActionExecuteApprovedTool,
					ResolvedReferences: contracts.CanonicalResolvedReferences{}, ApprovalContext: &approvalContext,
				})
				if err != nil {
					return err
				}
				sequence, err := repository.AllocateNextSequence(ctx, tx, graph.run.RunID)
				if err != nil {
					return err
				}
				_, err = repository.InsertCheckpoint(ctx, tx, checkpoint.Entity{
					CheckpointID: contracts.CheckpointID(fmt.Sprintf("checkpoint-approved-%d", index)), TaskID: graph.task.TaskID,
					RunID: graph.run.RunID, ExecutionVersion: runtimeVersion, CheckpointSequence: sequence,
					RuntimeContext: raw, ExecutionConfigHash: graph.execution.ExecutionConfigHash,
				})
				return err
			})
			if err != nil {
				t.Fatalf("prepare Approved facts: %v", err)
			}
			var result checkpoint.ValidationResult
			err = environment.Runtime.WriteExecutor().Execute(context.Background(), func(ctx context.Context, tx contracts.RuntimeWriteTx) error {
				var err error
				result, err = manager.LoadLatestForExecutionDispatch(ctx, tx, checkpoint.RuntimeCheckpointQuery{
					TaskID: graph.task.TaskID, RunID: graph.run.RunID, ExecutionVersion: runtimeVersion,
				})
				return err
			})
			if err != nil {
				t.Fatalf("load Approved Checkpoint: %v", err)
			}
			if test.reason == "" {
				if test.invariant {
					if _, ok := result.(checkpoint.PersistenceInvariantViolation); !ok {
						t.Fatalf("result = %#v, want PersistenceInvariantViolation", result)
					}
					return
				}
				if _, ok := result.(checkpoint.ValidCheckpoint); !ok {
					t.Fatalf("result = %#v, want ValidCheckpoint", result)
				}
				return
			}
			invalid, ok := result.(checkpoint.CheckpointInvalid)
			if !ok || invalid.ReasonCode != test.reason {
				t.Fatalf("result = %#v, want %s", result, test.reason)
			}
		})
	}
}

func testCheckpointSequenceAndLatest(t *testing.T, environment *postgrestest.RepositoryEnvironment) {
	manager, repository, taskRepositories := checkpointTestComponents(t, environment)
	now := time.Now().UTC()
	graph := repositoryGraph("checkpoint-sequence", now, now)
	insertRepositoryGraph(t, environment, taskRepositories, graph)
	first := saveCheckpoint(t, environment, taskRepositories, manager, checkpoint.RuntimeCheckpointSaveRequest{
		TaskID: graph.task.TaskID, RunID: graph.run.RunID, ExecutionVersion: 1,
		ExecutionConfigHash: graph.execution.ExecutionConfigHash, Draft: checkpoint.InitializationDraft{},
	})
	if first.CheckpointSequence != 1 || first.CreatedAt.IsZero() {
		t.Fatalf("saved ref = %#v", first)
	}

	rollback := errors.New("rollback checkpoint")
	err := environment.Runtime.WriteExecutor().Execute(context.Background(), func(ctx context.Context, tx contracts.RuntimeWriteTx) error {
		if _, err := taskRepositories.Runs.LockByTask(ctx, tx, graph.task.TaskID); err != nil {
			return err
		}
		sequence, err := repository.AllocateNextSequence(ctx, tx, graph.run.RunID)
		if err != nil {
			return err
		}
		if sequence != 2 {
			t.Fatalf("rolled-back sequence = %d", sequence)
		}
		return rollback
	})
	if !errors.Is(err, rollback) {
		t.Fatalf("rollback error = %v", err)
	}
	codec, err := checkpoint.NewRuntimeContextCodec(checkpoint.RuntimeContextCodecLimits{MaxBytes: 64 * 1024, MaxDepth: 32})
	if err != nil {
		t.Fatal(err)
	}
	raw, err := codec.Encode(checkpointRuntimeContext(graph.task.TaskID, graph.run.RunID, 1))
	if err != nil {
		t.Fatal(err)
	}
	secondID := contracts.CheckpointID("checkpoint-sequence-second")
	err = environment.Runtime.WriteExecutor().Execute(context.Background(), func(ctx context.Context, tx contracts.RuntimeWriteTx) error {
		if _, err := taskRepositories.Runs.LockByTask(ctx, tx, graph.task.TaskID); err != nil {
			return err
		}
		sequence, err := repository.AllocateNextSequence(ctx, tx, graph.run.RunID)
		if err != nil {
			return err
		}
		if sequence != 2 {
			t.Fatalf("reused sequence = %d, want 2", sequence)
		}
		_, err = repository.InsertCheckpoint(ctx, tx, checkpoint.Entity{
			CheckpointID: secondID, TaskID: graph.task.TaskID, RunID: graph.run.RunID,
			ExecutionVersion: 1, CheckpointSequence: sequence, RuntimeContext: raw,
			ExecutionConfigHash: graph.execution.ExecutionConfigHash,
		})
		return err
	})
	if err != nil {
		t.Fatalf("commit reused sequence: %v", err)
	}

	err = environment.Runtime.WriteExecutor().Execute(context.Background(), func(ctx context.Context, tx contracts.RuntimeWriteTx) error {
		latest, err := repository.FindLatestByExecutionVersion(ctx, tx, graph.task.TaskID, graph.run.RunID, 1)
		if err != nil {
			return err
		}
		if latest.CheckpointID != secondID || latest.CheckpointSequence != 2 {
			t.Fatalf("latest result = %#v", latest)
		}
		found, err := repository.FindByID(ctx, tx, first.CheckpointID)
		if err != nil {
			return err
		}
		if found.CheckpointSequence != 1 || found.TaskID != graph.task.TaskID {
			t.Fatalf("FindByID = %#v", found)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("load Checkpoints: %v", err)
	}
}

func testCheckpointMaximumDamaged(t *testing.T, environment *postgrestest.RepositoryEnvironment) {
	manager, repository, taskRepositories := checkpointTestComponents(t, environment)
	now := time.Now().UTC()
	graph := repositoryGraph("checkpoint-damaged", now, now)
	insertRepositoryGraph(t, environment, taskRepositories, graph)
	valid := saveCheckpoint(t, environment, taskRepositories, manager, checkpoint.RuntimeCheckpointSaveRequest{
		TaskID: graph.task.TaskID, RunID: graph.run.RunID, ExecutionVersion: 1,
		ExecutionConfigHash: graph.execution.ExecutionConfigHash, Draft: checkpoint.InitializationDraft{},
	})

	corruptID := contracts.CheckpointID("checkpoint-corrupt-max")
	err := environment.Runtime.WriteExecutor().Execute(context.Background(), func(ctx context.Context, tx contracts.RuntimeWriteTx) error {
		if _, err := taskRepositories.Runs.LockByTask(ctx, tx, graph.task.TaskID); err != nil {
			return err
		}
		sequence, err := repository.AllocateNextSequence(ctx, tx, graph.run.RunID)
		if err != nil {
			return err
		}
		_, err = repository.InsertCheckpoint(ctx, tx, checkpoint.Entity{
			CheckpointID: corruptID, TaskID: graph.task.TaskID, RunID: graph.run.RunID,
			ExecutionVersion: 1, CheckpointSequence: sequence, RuntimeContext: []byte(`{}`),
			ExecutionConfigHash: graph.execution.ExecutionConfigHash,
		})
		return err
	})
	if err != nil {
		t.Fatalf("insert corrupt maximum: %v", err)
	}

	err = environment.Runtime.WriteExecutor().Execute(context.Background(), func(ctx context.Context, tx contracts.RuntimeWriteTx) error {
		result, err := manager.LoadLatestForClaim(ctx, tx, checkpointQuery(graph), checkpoint.ClaimQueryInitial)
		if err != nil {
			return err
		}
		invalid, ok := result.(checkpoint.CheckpointInvalid)
		if !ok || invalid.CheckpointID != corruptID || invalid.ReasonCode != contracts.ReasonCodeRuntimeContextMalformed {
			t.Fatalf("maximum validation = %#v; valid lower ID was %s", result, valid.CheckpointID)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("validate corrupt maximum: %v", err)
	}
}

func testCheckpointGuards(t *testing.T, environment *postgrestest.RepositoryEnvironment) {
	manager, repository, taskRepositories := checkpointTestComponents(t, environment)
	now := time.Now().UTC()
	graph := repositoryGraph("checkpoint-guards", now, now)
	insertRepositoryGraph(t, environment, taskRepositories, graph)
	contextValue := checkpointRuntimeContext(graph.task.TaskID, graph.run.RunID, 1)
	for _, request := range []checkpoint.RuntimeCheckpointSaveRequest{
		{TaskID: graph.task.TaskID, RunID: graph.run.RunID, ExecutionVersion: 1, ExecutionConfigHash: contracts.ExecutionConfigHash(strings.Repeat("b", 64)), Draft: checkpoint.InitializationDraft{}},
		{TaskID: graph.task.TaskID, RunID: "run-other", ExecutionVersion: 1, ExecutionConfigHash: graph.execution.ExecutionConfigHash, Draft: checkpoint.InitializationDraft{}},
		{TaskID: graph.task.TaskID, RunID: graph.run.RunID, ExecutionVersion: 2, ExecutionConfigHash: graph.execution.ExecutionConfigHash, Draft: checkpoint.InitializationDraft{}},
	} {
		err := environment.Runtime.WriteExecutor().Execute(context.Background(), func(ctx context.Context, tx contracts.RuntimeWriteTx) error {
			_, err := manager.SaveRuntimeCheckpoint(ctx, tx, request)
			return err
		})
		if err == nil {
			t.Fatalf("Save(%#v) succeeded", request)
		}
	}
	var count int
	if err := environment.Runtime.ReadPool().QueryRow(context.Background(), `SELECT count(*) FROM checkpoint`).Scan(&count); err != nil || count != 0 {
		t.Fatalf("checkpoint count after rejected saves = %d, err=%v", count, err)
	}
	if _, err := repository.AllocateNextSequence(context.Background(), fakeCheckpointTx{}, graph.run.RunID); !errors.Is(err, postgresruntime.ErrInvalidRuntimeWriteTx) {
		t.Fatalf("invalid transaction token error = %v", err)
	}

	source := saveCheckpoint(t, environment, taskRepositories, manager, checkpoint.RuntimeCheckpointSaveRequest{
		TaskID: graph.task.TaskID, RunID: graph.run.RunID, ExecutionVersion: 1,
		ExecutionConfigHash: graph.execution.ExecutionConfigHash, Draft: checkpoint.InitializationDraft{},
	})
	secondExecution := graph.execution
	secondExecution.TaskExecutionID = "execution-checkpoint-guards-v2"
	secondExecution.ExecutionVersion = 2
	if err := environment.Runtime.WriteExecutor().Execute(context.Background(), func(ctx context.Context, tx contracts.RuntimeWriteTx) error {
		return taskRepositories.Executions.Insert(ctx, tx, secondExecution)
	}); err != nil {
		t.Fatalf("insert second TaskExecution: %v", err)
	}
	err := environment.Runtime.WriteExecutor().Execute(context.Background(), func(ctx context.Context, tx contracts.RuntimeWriteTx) error {
		if _, err := taskRepositories.Runs.LockByTask(ctx, tx, graph.task.TaskID); err != nil {
			return err
		}
		_, err := manager.SaveRuntimeCheckpoint(ctx, tx, checkpoint.RuntimeCheckpointSaveRequest{
			TaskID: graph.task.TaskID, RunID: graph.run.RunID, ExecutionVersion: 2,
			ExecutionConfigHash: graph.execution.ExecutionConfigHash,
			Draft:               checkpoint.ExecutionDraft{NextAction: contracts.CheckpointNextActionGeneratePlan},
		})
		return err
	})
	if err == nil {
		t.Fatal("version 2 generic save unexpectedly exposed Recovery path")
	}
	_ = source

	encoded, err := checkpoint.NewRuntimeContextCodec(checkpoint.RuntimeContextCodecLimits{MaxBytes: 64 * 1024, MaxDepth: 32})
	if err != nil {
		t.Fatal(err)
	}
	raw, err := encoded.Encode(contextValue)
	if err != nil {
		t.Fatal(err)
	}
	err = environment.Runtime.WriteExecutor().Execute(context.Background(), func(ctx context.Context, tx contracts.RuntimeWriteTx) error {
		if _, err := taskRepositories.Runs.LockByTask(ctx, tx, graph.task.TaskID); err != nil {
			return err
		}
		_, err := repository.InsertCheckpoint(ctx, tx, checkpoint.Entity{
			CheckpointID: "checkpoint-hash-mismatch", TaskID: graph.task.TaskID, RunID: graph.run.RunID,
			ExecutionVersion: 1, CheckpointSequence: 3, RuntimeContext: raw,
			ExecutionConfigHash: contracts.ExecutionConfigHash(strings.Repeat("b", 64)),
		})
		return err
	})
	if err != nil {
		t.Fatalf("insert hash mismatch fixture: %v", err)
	}
	err = environment.Runtime.WriteExecutor().Execute(context.Background(), func(ctx context.Context, tx contracts.RuntimeWriteTx) error {
		result, err := manager.LoadLatestForClaim(ctx, tx, checkpointQuery(graph), checkpoint.ClaimQueryInitial)
		if err != nil {
			return err
		}
		invalid, ok := result.(checkpoint.CheckpointInvalid)
		if !ok || invalid.ReasonCode != contracts.ReasonCodeCheckpointExecutionHashMismatch {
			t.Fatalf("hash result = %#v", result)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("validate hash mismatch: %v", err)
	}
}

func checkpointTestComponents(t *testing.T, environment *postgrestest.RepositoryEnvironment) (*checkpoint.Manager, *postgrescheckpoint.Repository, *postgrestaskruntime.Repositories) {
	t.Helper()
	codec, err := checkpoint.NewRuntimeContextCodec(checkpoint.RuntimeContextCodecLimits{MaxBytes: 64 * 1024, MaxDepth: 32})
	if err != nil {
		t.Fatal(err)
	}
	repository := postgrescheckpoint.New()
	manager, err := checkpoint.NewManager(repository, codec)
	if err != nil {
		t.Fatal(err)
	}
	return manager, repository, postgrestaskruntime.New(environment.Runtime.ReadPool())
}

func checkpointRuntimeContext(taskID contracts.TaskID, runID contracts.RunID, version contracts.ExecutionVersion) contracts.RuntimeContextV1 {
	return contracts.RuntimeContextV1{
		SchemaVersion: 1, TaskID: taskID, RunID: runID, ExecutionVersion: version,
		NextAction:         contracts.CheckpointNextActionGeneratePlan,
		ResolvedReferences: contracts.CanonicalResolvedReferences{},
	}
}

func saveCheckpoint(t *testing.T, environment *postgrestest.RepositoryEnvironment, taskRepositories *postgrestaskruntime.Repositories, manager *checkpoint.Manager, request checkpoint.RuntimeCheckpointSaveRequest) checkpoint.Ref {
	t.Helper()
	var result checkpoint.Ref
	err := environment.Runtime.WriteExecutor().Execute(context.Background(), func(ctx context.Context, tx contracts.RuntimeWriteTx) error {
		if _, err := taskRepositories.Runs.LockByTask(ctx, tx, request.TaskID); err != nil {
			return err
		}
		var err error
		result, err = manager.SaveRuntimeCheckpoint(ctx, tx, request)
		return err
	})
	if err != nil {
		t.Fatalf("save Checkpoint: %v", err)
	}
	return result
}

func checkpointQuery(graph repositoryGraphValues) checkpoint.RuntimeCheckpointQuery {
	return checkpoint.RuntimeCheckpointQuery{TaskID: graph.task.TaskID, RunID: graph.run.RunID, ExecutionVersion: graph.execution.ExecutionVersion}
}

func installCheckpointValidationFactTables(t *testing.T, environment *postgrestest.RepositoryEnvironment) {
	t.Helper()
	connection := postgrestest.Connect(t, environment.Identities.MigrationDSN)
	for _, statement := range []string{
		`CREATE TABLE plan (plan_id text PRIMARY KEY, run_id text NOT NULL)`,
		`CREATE TABLE step (step_id text PRIMARY KEY, run_id text NOT NULL, plan_id text NOT NULL, sequence bigint NOT NULL, type text NOT NULL, status text NOT NULL, input jsonb NOT NULL, output_schema jsonb NOT NULL, output jsonb, tool_name text NOT NULL DEFAULT '')`,
		`CREATE UNIQUE INDEX step_run_sequence_test_index ON step (run_id, sequence)`,
		`CREATE TABLE approval (approval_id text PRIMARY KEY, task_id text NOT NULL, run_id text NOT NULL, step_id text NOT NULL, execution_version bigint NOT NULL, execution_config_hash text NOT NULL, status text NOT NULL, tool_name text NOT NULL, frozen_tool_input jsonb NOT NULL, observed_values jsonb NOT NULL, resource_version text NOT NULL, frozen_input_hash text NOT NULL)`,
		`CREATE TABLE tool_execution (tool_execution_id text PRIMARY KEY, task_id text NOT NULL, run_id text NOT NULL, step_id text NOT NULL, execution_version bigint NOT NULL, status text NOT NULL, error_code text, side_effect_unknown boolean NOT NULL DEFAULT false)`,
	} {
		if _, err := connection.Exec(context.Background(), statement); err != nil {
			t.Fatalf("create Checkpoint fact fixture: %v", err)
		}
	}
	if err := connection.Close(context.Background()); err != nil {
		t.Fatalf("close fact fixture connection: %v", err)
	}
	environment.Identities.GrantRuntimePrivileges(t)
}

func insertCheckpointValidationFacts(ctx context.Context, tx contracts.RuntimeWriteTx, graph repositoryGraphValues, stepType contracts.StepType, stepState contracts.StepStatus, withPrior bool) error {
	return postgresruntime.WithPostgreSQLWriteTx(tx, func(databaseTx pgx.Tx) error {
		if _, err := databaseTx.Exec(ctx, `DELETE FROM tool_execution; DELETE FROM approval; DELETE FROM step; DELETE FROM plan`); err != nil {
			return err
		}
		if _, err := databaseTx.Exec(ctx, `INSERT INTO plan (plan_id,run_id) VALUES ('plan-1',$1)`, graph.run.RunID); err != nil {
			return err
		}
		if withPrior {
			if _, err := databaseTx.Exec(ctx, `INSERT INTO step (step_id,run_id,plan_id,sequence,type,status,input,output_schema,output) VALUES ('step-previous',$1,'plan-1',1,'Analysis','Completed','{}','{"result":{"type":"string"}}','{"result":"safe"}')`, graph.run.RunID); err != nil {
				return err
			}
		}
		sequence := 1
		input := `{}`
		if withPrior {
			sequence = 2
			input = `{"payload":"step.output.result"}`
		}
		_, err := databaseTx.Exec(ctx, `INSERT INTO step (step_id,run_id,plan_id,sequence,type,status,input,output_schema,output,tool_name) VALUES ('step-current',$1,'plan-1',$2,$3,$4,$5::jsonb,'{}','{}',$6)`, graph.run.RunID, sequence, stepType, stepState, input, map[bool]string{true: "tool.test", false: ""}[stepType == contracts.StepTypeToolCall])
		return err
	})
}

func checkpointApprovalContext() contracts.ApprovalContext {
	frozenInput := contracts.FrozenToolInput(`{"replicas":2}`)
	observedValues := contracts.ObservedValues(`{"replicas":1}`)
	resourceVersion := contracts.ResourceVersion("42")
	hash, err := contracts.ComputeFrozenInputHashV1(contracts.FrozenApprovedToolInputV1{
		Schema: contracts.FrozenApprovedToolInputSchemaV1, Version: contracts.FrozenApprovedToolInputVersionV1,
		ToolName: "tool.test", ToolInput: frozenInput, ObservedValues: observedValues, ResourceVersion: resourceVersion,
	})
	if err != nil {
		panic(err)
	}
	return contracts.ApprovalContext{
		ApprovalID: "approval-1", ApprovalExecutionVersion: 1, ToolName: "tool.test",
		FrozenToolInput: frozenInput, ObservedValues: observedValues, ResourceVersion: resourceVersion,
		FrozenInputHash: hash,
	}
}

func insertCheckpointApproval(ctx context.Context, tx contracts.RuntimeWriteTx, graph repositoryGraphValues) error {
	approval := checkpointApprovalContext()
	return postgresruntime.WithPostgreSQLWriteTx(tx, func(databaseTx pgx.Tx) error {
		_, err := databaseTx.Exec(ctx, `
INSERT INTO approval (
    approval_id,task_id,run_id,step_id,execution_version,execution_config_hash,status,
    tool_name,frozen_tool_input,observed_values,resource_version,frozen_input_hash
) VALUES ('approval-1',$1,$2,'step-current',1,$3,'Approved',$4,$5::jsonb,$6::jsonb,$7,$8)`,
			graph.task.TaskID, graph.run.RunID, graph.execution.ExecutionConfigHash, approval.ToolName,
			string(approval.FrozenToolInput), string(approval.ObservedValues), approval.ResourceVersion, approval.FrozenInputHash)
		return err
	})
}

func prepareSecondCheckpointExecution(ctx context.Context, tx contracts.RuntimeWriteTx, graph repositoryGraphValues, startedAt time.Time) error {
	return postgresruntime.WithPostgreSQLWriteTx(tx, func(databaseTx pgx.Tx) error {
		if _, err := databaseTx.Exec(ctx, `UPDATE task SET current_execution_version=2 WHERE task_id=$1`, graph.task.TaskID); err != nil {
			return err
		}
		_, err := databaseTx.Exec(ctx, `
INSERT INTO task_execution (
    task_execution_id,task_id,execution_version,worker_id,status,execution_config_hash,created_at,started_at
) VALUES ($1,$2,2,'worker-approved','RUNNING',$3,$4,$4)`,
			"execution-version-2-"+string(graph.task.TaskID), graph.task.TaskID,
			graph.execution.ExecutionConfigHash, startedAt)
		return err
	})
}

func insertCheckpointToolExecution(status contracts.ToolExecutionStatus, errorCode *contracts.ErrorCode, sideEffectUnknown bool) func(context.Context, contracts.RuntimeWriteTx) error {
	return func(ctx context.Context, tx contracts.RuntimeWriteTx) error {
		return postgresruntime.WithPostgreSQLWriteTx(tx, func(databaseTx pgx.Tx) error {
			_, err := databaseTx.Exec(ctx, `
INSERT INTO tool_execution (tool_execution_id,task_id,run_id,step_id,execution_version,status,error_code,side_effect_unknown)
SELECT 'tool-execution-action',r.task_id,s.run_id,s.step_id,1,$1,$2,$3
FROM step AS s JOIN run AS r ON r.run_id=s.run_id WHERE s.step_id='step-current'`, status, errorCode, sideEffectUnknown)
			return err
		})
	}
}

func execCheckpointFixtureSQL(ctx context.Context, tx contracts.RuntimeWriteTx, statement string) error {
	return postgresruntime.WithPostgreSQLWriteTx(tx, func(databaseTx pgx.Tx) error {
		_, err := databaseTx.Exec(ctx, statement)
		return err
	})
}

type fakeCheckpointTx struct{}

func (fakeCheckpointTx) AgentOpsRuntimeWriteTx() {}
