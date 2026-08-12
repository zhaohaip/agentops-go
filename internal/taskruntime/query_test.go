package taskruntime_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/zhaohaip/agentops-go/internal/contracts"
	"github.com/zhaohaip/agentops-go/internal/taskruntime"
)

func TestTaskQueryDerivesRecoverableFromFrozenFacts(t *testing.T) {
	config := loadedAgentConfig(t)
	hash, err := taskruntime.HashExecutionConfigV1(config.ExecutionConfig)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 8, 7, 12, 0, 0, 0, time.UTC)
	tests := []struct {
		name   string
		mutate func(*taskruntime.Task, *taskruntime.TaskExecution, *taskruntime.RecoverabilityEvidence)
		want   bool
	}{
		{name: "recoverable", want: true},
		{name: "running Task is recoverable", want: true, mutate: func(task *taskruntime.Task, _ *taskruntime.TaskExecution, _ *taskruntime.RecoverabilityEvidence) {
			task.Status = contracts.TaskStatusRunning
		}},
		{name: "Pending Task is not recoverable", mutate: func(task *taskruntime.Task, _ *taskruntime.TaskExecution, _ *taskruntime.RecoverabilityEvidence) {
			task.Status = contracts.TaskStatusPending
		}},
		{name: "queued", mutate: func(task *taskruntime.Task, _ *taskruntime.TaskExecution, _ *taskruntime.RecoverabilityEvidence) {
			queued := now
			task.QueuedAt = &queued
		}},
		{name: "deadline reached", mutate: func(_ *taskruntime.Task, _ *taskruntime.TaskExecution, evidence *taskruntime.RecoverabilityEvidence) {
			evidence.DatabaseNow = now.Add(2 * time.Hour)
		}},
		{name: "execution running", mutate: func(_ *taskruntime.Task, execution *taskruntime.TaskExecution, _ *taskruntime.RecoverabilityEvidence) {
			execution.Status = contracts.TaskExecutionStatusRunning
		}},
		{name: "unsafe write Tool", mutate: func(_ *taskruntime.Task, _ *taskruntime.TaskExecution, evidence *taskruntime.RecoverabilityEvidence) {
			evidence.HasRunningOrUnknownWriteTool = true
		}},
		{name: "Checkpoint invalid", mutate: func(_ *taskruntime.Task, _ *taskruntime.TaskExecution, evidence *taskruntime.RecoverabilityEvidence) {
			evidence.CheckpointValid = false
		}},
		{name: "Checkpoint hash mismatch", mutate: func(_ *taskruntime.Task, _ *taskruntime.TaskExecution, evidence *taskruntime.RecoverabilityEvidence) {
			evidence.CheckpointExecutionConfigHash = contracts.ExecutionConfigHash("bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb")
		}},
		{name: "current config hash mismatch", mutate: func(_ *taskruntime.Task, execution *taskruntime.TaskExecution, evidence *taskruntime.RecoverabilityEvidence) {
			other := contracts.ExecutionConfigHash("bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb")
			execution.ExecutionConfigHash = other
			evidence.CheckpointExecutionConfigHash = other
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			task := taskruntime.Task{TaskID: "query-task", AgentID: config.ExecutionConfig.Agent.AgentID,
				Status: contracts.TaskStatusInterrupted, CurrentRunID: "query-run", CurrentExecutionVersion: 1,
				DeadlineAt: now.Add(time.Hour), CreatedAt: now}
			run := taskruntime.Run{RunID: task.CurrentRunID, TaskID: task.TaskID, Status: contracts.RunStatusRunning}
			execution := taskruntime.TaskExecution{TaskID: task.TaskID, ExecutionVersion: 1,
				Status: contracts.TaskExecutionStatusInterrupted, ExecutionConfigHash: hash}
			evidence := taskruntime.RecoverabilityEvidence{DatabaseNow: now, CheckpointValid: true,
				CheckpointExecutionConfigHash: hash}
			if test.mutate != nil {
				test.mutate(&task, &execution, &evidence)
			}
			snapshots := &fakeTaskQuerySnapshots{items: []taskruntime.TaskQuerySnapshot{{
				Task: task, Run: run, Execution: execution, Evidence: evidence,
			}}}
			service, err := taskruntime.NewTaskQueryService(snapshots,
				&fakeAgentConfigSource{agents: map[contracts.AgentID]taskruntime.AgentRuntimeConfig{task.AgentID: config}})
			if err != nil {
				t.Fatal(err)
			}
			view, err := service.GetTask(context.Background(), task.TaskID)
			if err != nil || view.Recoverable != test.want {
				t.Fatalf("GetTask() = %+v, %v; recoverable want %v", view, err, test.want)
			}
		})
	}
}

func TestTaskQueryListAndStatusValidation(t *testing.T) {
	snapshots := &fakeTaskQuerySnapshots{items: []taskruntime.TaskQuerySnapshot{{
		Task:      taskruntime.Task{TaskID: "failed", Status: contracts.TaskStatusFailed, CurrentRunID: "run", CurrentExecutionVersion: 1},
		Run:       taskruntime.Run{TaskID: "failed", RunID: "run", Status: contracts.RunStatusFailed},
		Execution: taskruntime.TaskExecution{TaskID: "failed", ExecutionVersion: 1, Status: contracts.TaskExecutionStatusFailed},
	}}}
	service, err := taskruntime.NewTaskQueryService(snapshots, &fakeAgentConfigSource{})
	if err != nil {
		t.Fatal(err)
	}
	status := contracts.TaskStatusFailed
	views, err := service.ListTasks(context.Background(), &status)
	if err != nil || len(views) != 1 || views[0].Recoverable {
		t.Fatalf("ListTasks() = %+v, %v", views, err)
	}
	invalid := contracts.TaskStatus("invalid")
	if _, err := service.ListTasks(context.Background(), &invalid); !errors.Is(err, taskruntime.ErrInvalidArgument) {
		t.Fatalf("ListTasks(invalid) error = %v", err)
	}
}

func TestTaskQueryPropagatesSnapshotFailure(t *testing.T) {
	wantErr := errors.New("query snapshot unavailable")
	service, err := taskruntime.NewTaskQueryService(&fakeTaskQuerySnapshots{err: wantErr}, &fakeAgentConfigSource{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.GetTask(context.Background(), "query-error"); !errors.Is(err, wantErr) {
		t.Fatalf("GetTask() error = %v, want snapshot error", err)
	}
}

type fakeTaskQuerySnapshots struct {
	items []taskruntime.TaskQuerySnapshot
	err   error
}

func (f *fakeTaskQuerySnapshots) FindSnapshot(
	_ context.Context,
	taskID contracts.TaskID,
) (taskruntime.TaskQuerySnapshot, error) {
	if f.err != nil {
		return taskruntime.TaskQuerySnapshot{}, f.err
	}
	for _, snapshot := range f.items {
		if snapshot.Task.TaskID == taskID {
			return snapshot, nil
		}
	}
	return taskruntime.TaskQuerySnapshot{}, taskruntime.ErrRepositoryNotFound
}

func (f *fakeTaskQuerySnapshots) ListSnapshots(
	_ context.Context,
	status *contracts.TaskStatus,
) ([]taskruntime.TaskQuerySnapshot, error) {
	if f.err != nil {
		return nil, f.err
	}
	result := make([]taskruntime.TaskQuerySnapshot, 0, len(f.items))
	for _, snapshot := range f.items {
		if status == nil || snapshot.Task.Status == *status {
			result = append(result, snapshot)
		}
	}
	return result, nil
}

var _ taskruntime.TaskQuerySnapshotRepository = (*fakeTaskQuerySnapshots)(nil)
