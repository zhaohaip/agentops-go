package taskruntime

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/zhaohaip/agentops-go/internal/contracts"
	"github.com/zhaohaip/agentops-go/internal/lifecycle"
)

var (
	// ErrInvalidArgument 表示命令无法形成稳定、合法的应用请求。
	ErrInvalidArgument = errors.New("InvalidArgument")
	// ErrCommandConflict 表示 command_id 已被不同请求使用。
	ErrCommandConflict = errors.New("CommandConflict")
	// ErrAgentUnavailable 表示静态 Agent 不存在或已禁用。
	ErrAgentUnavailable = errors.New("AgentUnavailable")
)

// CreateTaskRequest 是 CreateTask 入站请求。
type CreateTaskRequest struct {
	CommandID  CommandID
	AgentID    contracts.AgentID
	TaskInput  string
	OperatorID string
}

// TaskCreated 是 CreateTask 的成功结果。
type TaskCreated struct {
	TaskID                  contracts.TaskID
	RunID                   contracts.RunID
	Status                  contracts.TaskStatus
	CurrentExecutionVersion contracts.ExecutionVersion
	DeadlineAt              time.Time
	QueuedAt                time.Time
}

// CreateTaskService 编排 Task、Run、首个 Execution、Receipt 与初始 Checkpoint。
type CreateTaskService struct {
	executor    contracts.RuntimeWriteExecutor
	tasks       TaskRepository
	runs        RunRepository
	executions  TaskExecutionRepository
	receipts    CommandReceiptRepository
	taskLogs    TaskLogRepository
	clock       DatabaseClock
	configs     AgentConfigSource
	checkpoints RuntimeCheckpointPort
	policy      lifecycle.Policy
	newID       func(string) (string, error)
}

// CreateTaskDependencies 声明 CreateTask 的最小出站依赖。
type CreateTaskDependencies struct {
	Executor    contracts.RuntimeWriteExecutor
	Tasks       TaskRepository
	Runs        RunRepository
	Executions  TaskExecutionRepository
	Receipts    CommandReceiptRepository
	TaskLogs    TaskLogRepository
	Clock       DatabaseClock
	Configs     AgentConfigSource
	Checkpoints RuntimeCheckpointPort
	Policy      lifecycle.Policy
}

// NewCreateTaskService 创建未接入生产组合根的 CreateTask 应用服务。
func NewCreateTaskService(dependencies CreateTaskDependencies) (*CreateTaskService, error) {
	if dependencies.Executor == nil || dependencies.Tasks == nil || dependencies.Runs == nil ||
		dependencies.Executions == nil || dependencies.Receipts == nil || dependencies.Clock == nil ||
		dependencies.TaskLogs == nil || dependencies.Configs == nil || dependencies.Checkpoints == nil {
		return nil, errors.New("create CreateTask service: dependencies are required")
	}
	return &CreateTaskService{
		executor: dependencies.Executor, tasks: dependencies.Tasks, runs: dependencies.Runs,
		executions: dependencies.Executions, receipts: dependencies.Receipts,
		taskLogs: dependencies.TaskLogs,
		clock:    dependencies.Clock, configs: dependencies.Configs, checkpoints: dependencies.Checkpoints,
		policy: dependencies.Policy, newID: randomID,
	}, nil
}

// CreateTask 原子创建一个待领取 Task，或重放不可变 Command Receipt。
func (s *CreateTaskService) CreateTask(ctx context.Context, request CreateTaskRequest) (TaskCreated, error) {
	if s == nil {
		return TaskCreated{}, errors.New("create Task: service is not initialized")
	}
	fingerprint, err := createRequestFingerprint(request)
	if err != nil {
		return TaskCreated{}, err
	}

	var outcome createReceiptResponse
	createdInTransaction := false
	err = s.executor.Execute(ctx, func(ctx context.Context, tx contracts.RuntimeWriteTx) error {
		stored, lockErr := s.receipts.Lock(ctx, tx, request.CommandID)
		switch {
		case lockErr == nil:
			if stored.CommandType != CommandTypeCreate || stored.TargetID != string(request.AgentID) ||
				stored.RequestFingerprint != fingerprint {
				return ErrCommandConflict
			}
			if decodeErr := json.Unmarshal(stored.Response, &outcome); decodeErr != nil {
				return fmt.Errorf("decode Create command receipt: %w", ErrPersistenceInvariantViolation)
			}
			return validateCreateReceiptResponse(outcome)
		case !errors.Is(lockErr, ErrRepositoryNotFound):
			return fmt.Errorf("lock Create command receipt: %w", lockErr)
		}

		agent, exists := s.configs.LookupAgent(request.AgentID)
		if !exists || !agent.ExecutionConfig.Agent.Enabled {
			outcome = rejectedCreateReceipt(createReceiptErrorAgentUnavailable)
			return s.insertCreateReceipt(ctx, tx, request, fingerprint, outcome)
		}
		if uint64(len(request.TaskInput)) > agent.ExecutionConfig.Planner.Limits.MaxTaskInputBytes {
			outcome = rejectedCreateReceipt(createReceiptErrorInvalidArgument)
			return s.insertCreateReceipt(ctx, tx, request, fingerprint, outcome)
		}
		configHash, hashErr := HashExecutionConfigV1(agent.ExecutionConfig)
		if hashErr != nil {
			return fmt.Errorf("hash Create execution config: %w", hashErr)
		}
		now, clockErr := s.clock.Now(ctx, tx)
		if clockErr != nil {
			return fmt.Errorf("read Create database clock: %w", clockErr)
		}
		if agent.TaskTimeout <= 0 {
			return errors.New("create Task: validated Agent task timeout is invalid")
		}
		if decision := s.policy.CanCreateTask(
			contracts.TaskStatusPending,
			contracts.RunStatusPending,
			contracts.TaskExecutionStatusQueued,
		); !decision.Allowed {
			return fmt.Errorf("validate Create lifecycle: %s", decision.Reason)
		}

		taskID, runID, executionID, idErr := s.createIDs()
		if idErr != nil {
			return idErr
		}
		version := contracts.ExecutionVersion(1)
		deadline := now.Add(agent.TaskTimeout)
		queuedAt := now
		if insertErr := s.tasks.Insert(ctx, tx, Task{
			TaskID: taskID, AgentID: request.AgentID, CreatedBy: request.OperatorID,
			Input: request.TaskInput, Status: contracts.TaskStatusPending, CurrentRunID: runID,
			CurrentExecutionVersion: version, DeadlineAt: deadline, QueuedAt: &queuedAt, CreatedAt: now,
		}); insertErr != nil {
			return fmt.Errorf("insert Create Task: %w", insertErr)
		}
		if insertErr := s.runs.Insert(ctx, tx, Run{
			RunID: runID, TaskID: taskID, Status: contracts.RunStatusPending, Context: json.RawMessage(`{}`),
		}); insertErr != nil {
			return fmt.Errorf("insert Create Run: %w", insertErr)
		}
		if insertErr := s.executions.Insert(ctx, tx, TaskExecution{
			TaskExecutionID: executionID, TaskID: taskID, ExecutionVersion: version,
			Status: contracts.TaskExecutionStatusQueued, ExecutionConfigHash: configHash, CreatedAt: now,
		}); insertErr != nil {
			return fmt.Errorf("insert Create TaskExecution: %w", insertErr)
		}
		if checkpointErr := s.checkpoints.SaveRuntimeCheckpoint(ctx, tx, SaveRuntimeCheckpointRequest{
			TaskID: taskID, RunID: runID, ExecutionVersion: version, ExecutionConfigHash: configHash,
			NextAction: contracts.CheckpointNextActionGeneratePlan, CreatedAt: now,
		}); checkpointErr != nil {
			return fmt.Errorf("save Create initialization Checkpoint: %w", checkpointErr)
		}

		outcome = createReceiptResponse{
			Outcome: createReceiptOutcomeCreated, TaskID: taskID, RunID: runID,
			Status: contracts.TaskStatusPending, CurrentExecutionVersion: version,
			DeadlineAt: deadline, QueuedAt: queuedAt,
		}
		if err := s.insertCreateReceipt(ctx, tx, request, fingerprint, outcome); err != nil {
			return err
		}
		createdInTransaction = true
		return nil
	})
	if err != nil {
		return TaskCreated{}, err
	}
	result, err := outcome.result()
	if err != nil {
		return TaskCreated{}, err
	}
	if createdInTransaction {
		appendTaskLogBestEffort(ctx, s.executor, s.taskLogs, s.clock, taskLogDraft{
			taskID: result.TaskID, runID: result.RunID, executionVersion: result.CurrentExecutionVersion,
			level: TaskLogLevelInfo, event: taskLogEventTaskCreated, message: "task created",
		})
	}
	return result, nil
}

const (
	createReceiptOutcomeCreated  = "Created"
	createReceiptOutcomeRejected = "Rejected"

	createReceiptErrorAgentUnavailable = "AgentUnavailable"
	createReceiptErrorInvalidArgument  = "InvalidArgument"
)

type createReceiptResponse struct {
	Outcome                 string                     `json:"outcome"`
	TaskID                  contracts.TaskID           `json:"task_id,omitempty"`
	RunID                   contracts.RunID            `json:"run_id,omitempty"`
	Status                  contracts.TaskStatus       `json:"status,omitempty"`
	CurrentExecutionVersion contracts.ExecutionVersion `json:"current_execution_version,omitempty"`
	DeadlineAt              time.Time                  `json:"deadline_at,omitempty"`
	QueuedAt                time.Time                  `json:"queued_at,omitempty"`
	ErrorCode               string                     `json:"error_code,omitempty"`
	ErrorSummary            string                     `json:"error_summary,omitempty"`
}

func (s *CreateTaskService) insertCreateReceipt(
	ctx context.Context,
	tx contracts.RuntimeWriteTx,
	request CreateTaskRequest,
	fingerprint string,
	response createReceiptResponse,
) error {
	encoded, err := json.Marshal(response)
	if err != nil {
		return fmt.Errorf("encode Create command receipt: %w", err)
	}
	now, err := s.clock.Now(ctx, tx)
	if err != nil {
		return fmt.Errorf("read Create receipt database clock: %w", err)
	}
	if err := s.receipts.Insert(ctx, tx, CommandReceipt{
		CommandID: request.CommandID, CommandType: CommandTypeCreate, TargetID: string(request.AgentID),
		RequestFingerprint: fingerprint, Response: encoded, CreatedAt: now,
	}); err != nil {
		return fmt.Errorf("insert Create command receipt: %w", err)
	}
	return nil
}

func (s *CreateTaskService) createIDs() (contracts.TaskID, contracts.RunID, TaskExecutionID, error) {
	taskID, err := s.newID("task")
	if err != nil {
		return "", "", "", fmt.Errorf("create Task ID: %w", err)
	}
	runID, err := s.newID("run")
	if err != nil {
		return "", "", "", fmt.Errorf("create Run ID: %w", err)
	}
	executionID, err := s.newID("execution")
	if err != nil {
		return "", "", "", fmt.Errorf("create TaskExecution ID: %w", err)
	}
	return contracts.TaskID(taskID), contracts.RunID(runID), TaskExecutionID(executionID), nil
}

func createRequestFingerprint(request CreateTaskRequest) (string, error) {
	if request.CommandID == "" || request.AgentID == "" || strings.TrimSpace(request.TaskInput) == "" ||
		strings.TrimSpace(request.OperatorID) == "" || !utf8.ValidString(string(request.CommandID)) ||
		!utf8.ValidString(string(request.AgentID)) || !utf8.ValidString(request.TaskInput) ||
		!utf8.ValidString(request.OperatorID) {
		return "", ErrInvalidArgument
	}
	normalized := struct {
		AgentID    contracts.AgentID `json:"agent_id"`
		TaskInput  string            `json:"task_input"`
		OperatorID string            `json:"operator_id"`
	}{AgentID: request.AgentID, TaskInput: request.TaskInput, OperatorID: request.OperatorID}
	encoded, err := json.Marshal(normalized)
	if err != nil {
		return "", fmt.Errorf("encode Create request fingerprint: %w", err)
	}
	digest := sha256.Sum256(encoded)
	return hex.EncodeToString(digest[:]), nil
}

func randomID(prefix string) (string, error) {
	var value [16]byte
	if _, err := rand.Read(value[:]); err != nil {
		return "", err
	}
	return prefix + "-" + hex.EncodeToString(value[:]), nil
}

func rejectedCreateReceipt(errorCode string) createReceiptResponse {
	summary := "Create request was rejected"
	switch errorCode {
	case createReceiptErrorAgentUnavailable:
		summary = "Agent is unavailable"
	case createReceiptErrorInvalidArgument:
		summary = "Task input exceeds the configured limit"
	}
	return createReceiptResponse{
		Outcome: createReceiptOutcomeRejected, ErrorCode: errorCode, ErrorSummary: summary,
	}
}

func validateCreateReceiptResponse(response createReceiptResponse) error {
	switch response.Outcome {
	case createReceiptOutcomeCreated:
		if response.TaskID == "" || response.RunID == "" || response.Status != contracts.TaskStatusPending ||
			!response.CurrentExecutionVersion.Valid() || response.DeadlineAt.IsZero() || response.QueuedAt.IsZero() {
			return fmt.Errorf("validate Create command receipt: %w", ErrPersistenceInvariantViolation)
		}
		return nil
	case createReceiptOutcomeRejected:
		if response.ErrorCode != createReceiptErrorAgentUnavailable && response.ErrorCode != createReceiptErrorInvalidArgument {
			return fmt.Errorf("validate Create command receipt: %w", ErrPersistenceInvariantViolation)
		}
		if response.ErrorSummary == "" {
			return fmt.Errorf("validate Create command receipt summary: %w", ErrPersistenceInvariantViolation)
		}
		return nil
	default:
		return fmt.Errorf("validate Create command receipt: %w", ErrPersistenceInvariantViolation)
	}
}

func (response createReceiptResponse) result() (TaskCreated, error) {
	if err := validateCreateReceiptResponse(response); err != nil {
		return TaskCreated{}, err
	}
	switch response.Outcome {
	case createReceiptOutcomeCreated:
		return TaskCreated{
			TaskID: response.TaskID, RunID: response.RunID, Status: response.Status,
			CurrentExecutionVersion: response.CurrentExecutionVersion,
			DeadlineAt:              response.DeadlineAt.UTC(), QueuedAt: response.QueuedAt.UTC(),
		}, nil
	case createReceiptOutcomeRejected:
		if response.ErrorCode == createReceiptErrorAgentUnavailable {
			return TaskCreated{}, ErrAgentUnavailable
		}
		return TaskCreated{}, ErrInvalidArgument
	default:
		return TaskCreated{}, ErrPersistenceInvariantViolation
	}
}
