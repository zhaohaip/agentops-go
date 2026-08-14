// Package api 提供 AgentOps Runtime 的 Gin HTTP 协议适配器。
package api

import (
	"bytes"
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"time"
	"unicode/utf8"

	"github.com/gin-gonic/gin"
	"github.com/gin-gonic/gin/binding"
	"github.com/zhaohaip/agentops-go/internal/contracts"
	"github.com/zhaohaip/agentops-go/internal/taskruntime"
)

const maxTaskRequestBytes = 1 << 20

type taskCreator interface {
	CreateTask(context.Context, taskruntime.CreateTaskRequest) (taskruntime.TaskCreated, error)
}

type taskCanceller interface {
	CancelTask(context.Context, taskruntime.CancelTaskRequest) (taskruntime.TaskCancelled, error)
}

type taskRecoverer interface {
	RecoverTask(context.Context, taskruntime.RecoverTaskRequest) (taskruntime.TaskRecovered, error)
}

type taskQuerier interface {
	GetTask(context.Context, contracts.TaskID) (taskruntime.TaskView, error)
	ListTasks(context.Context, *contracts.TaskStatus) ([]taskruntime.TaskView, error)
}

// TaskHandlerDependencies 声明 Task HTTP Handler 的应用层依赖和静态身份。
type TaskHandlerDependencies struct {
	Creator     taskCreator
	Canceller   taskCanceller
	Recoverer   taskRecoverer
	Querier     taskQuerier
	BearerToken string
	OperatorID  string
	Logger      *slog.Logger
}

// TaskHandler 只负责 Gin 请求校验和应用结果转换。
type TaskHandler struct {
	creator     taskCreator
	canceller   taskCanceller
	recoverer   taskRecoverer
	querier     taskQuerier
	bearerToken string
	operatorID  string
	logger      *slog.Logger
}

// NewTaskHandler 创建尚未装配到生产 HTTP Server 的 Task Handler。
func NewTaskHandler(dependencies TaskHandlerDependencies) (*TaskHandler, error) {
	if dependencies.Creator == nil || dependencies.Canceller == nil || dependencies.Recoverer == nil || dependencies.Querier == nil ||
		dependencies.BearerToken == "" || dependencies.OperatorID == "" || dependencies.Logger == nil {
		return nil, errors.New("create Task Handler: dependencies and static identities are required")
	}
	return &TaskHandler{creator: dependencies.Creator, canceller: dependencies.Canceller, recoverer: dependencies.Recoverer,
		querier: dependencies.Querier, bearerToken: dependencies.BearerToken,
		operatorID: dependencies.OperatorID, logger: dependencies.Logger}, nil
}

type recoverTaskRequest struct {
	CommandID string `json:"command_id" binding:"required"`
}

type recoverTaskResponse struct {
	TaskID                 contracts.TaskID              `json:"task_id"`
	RunID                  contracts.RunID               `json:"run_id"`
	SourceExecutionVersion contracts.ExecutionVersion    `json:"source_execution_version"`
	NewExecutionVersion    contracts.ExecutionVersion    `json:"new_execution_version"`
	TaskStatus             contracts.TaskStatus          `json:"task_status"`
	RunStatus              contracts.RunStatus           `json:"run_status"`
	ExecutionStatus        contracts.TaskExecutionStatus `json:"execution_status"`
	QueuedAt               time.Time                     `json:"queued_at"`
	RecoveryCheckpointID   contracts.CheckpointID        `json:"recovery_checkpoint_id"`
}

// Recover 绑定并处理 POST /v1/tasks/:task_id/recover。
func (h *TaskHandler) Recover(ginContext *gin.Context) {
	taskID, ok := bindTaskID(ginContext)
	if !ok {
		return
	}
	var body recoverTaskRequest
	if err := bindJSON(ginContext, &body); err != nil {
		writeError(ginContext, http.StatusBadRequest, "InvalidArgument", "invalid JSON request")
		return
	}
	result, err := h.recoverer.RecoverTask(ginContext.Request.Context(), taskruntime.RecoverTaskRequest{
		CommandID: taskruntime.CommandID(body.CommandID), TaskID: taskID, OperatorID: h.operatorID,
	})
	if err != nil {
		writeApplicationError(ginContext, err)
		return
	}
	respondJSON(ginContext, http.StatusOK, recoverTaskResponse{
		TaskID: result.TaskID, RunID: result.RunID, SourceExecutionVersion: result.SourceExecutionVersion,
		NewExecutionVersion: result.NewExecutionVersion, TaskStatus: result.TaskStatus, RunStatus: result.RunStatus,
		ExecutionStatus: result.ExecutionStatus, QueuedAt: result.QueuedAt,
		RecoveryCheckpointID: result.RecoveryCheckpointID,
	})
}

// Authenticate 校验静态 Bearer Token，并在失败时终止 Gin Handler 链。
func (h *TaskHandler) Authenticate(ginContext *gin.Context) {
	value := ginContext.GetHeader("Authorization")
	expected := "Bearer " + h.bearerToken
	if len(value) == len(expected) && subtle.ConstantTimeCompare([]byte(value), []byte(expected)) == 1 {
		ginContext.Next()
		return
	}
	writeError(ginContext, http.StatusUnauthorized, "Unauthorized", "authentication required")
	ginContext.Abort()
}

// LogRequest 仅记录受约束的请求元数据，不记录 Header、Query 或 Body。
func (h *TaskHandler) LogRequest(ginContext *gin.Context) {
	ginContext.Next()
	h.logger.InfoContext(ginContext.Request.Context(), "Task API request completed",
		"method", ginContext.Request.Method,
		"path", ginContext.Request.URL.Path,
		"status", ginContext.Writer.Status(),
	)
}

type createTaskRequest struct {
	CommandID string `json:"command_id" binding:"required"`
	AgentID   string `json:"agent_id" binding:"required"`
	Input     string `json:"input" binding:"required"`
}

type createTaskResponse struct {
	TaskID                  contracts.TaskID           `json:"task_id"`
	RunID                   contracts.RunID            `json:"run_id"`
	Status                  contracts.TaskStatus       `json:"status"`
	CurrentExecutionVersion contracts.ExecutionVersion `json:"current_execution_version"`
	DeadlineAt              time.Time                  `json:"deadline_at"`
	QueuedAt                time.Time                  `json:"queued_at"`
}

// Create 绑定并处理 POST /v1/tasks。
func (h *TaskHandler) Create(ginContext *gin.Context) {
	var body createTaskRequest
	if err := bindJSON(ginContext, &body); err != nil {
		writeError(ginContext, http.StatusBadRequest, "InvalidArgument", "invalid JSON request")
		return
	}
	created, err := h.creator.CreateTask(ginContext.Request.Context(), taskruntime.CreateTaskRequest{
		CommandID: taskruntime.CommandID(body.CommandID), AgentID: contracts.AgentID(body.AgentID),
		TaskInput: body.Input, OperatorID: h.operatorID,
	})
	if err != nil {
		writeApplicationError(ginContext, err)
		return
	}
	respondJSON(ginContext, http.StatusCreated, createTaskResponse{
		TaskID: created.TaskID, RunID: created.RunID, Status: created.Status,
		CurrentExecutionVersion: created.CurrentExecutionVersion,
		DeadlineAt:              created.DeadlineAt, QueuedAt: created.QueuedAt,
	})
}

// Get 处理 GET /v1/tasks/:task_id。
func (h *TaskHandler) Get(ginContext *gin.Context) {
	taskID, ok := bindTaskID(ginContext)
	if !ok {
		return
	}
	view, err := h.querier.GetTask(ginContext.Request.Context(), taskID)
	if err != nil {
		writeApplicationError(ginContext, err)
		return
	}
	respondJSON(ginContext, http.StatusOK, newTaskViewResponse(view))
}

// List 处理 GET /v1/tasks，并校验可选状态过滤条件。
func (h *TaskHandler) List(ginContext *gin.Context) {
	var status *contracts.TaskStatus
	if raw := ginContext.Query("status"); raw != "" {
		parsed := contracts.TaskStatus(raw)
		if !parsed.Valid() {
			writeError(ginContext, http.StatusBadRequest, "InvalidArgument", "invalid Task status")
			return
		}
		status = &parsed
	}
	views, err := h.querier.ListTasks(ginContext.Request.Context(), status)
	if err != nil {
		writeApplicationError(ginContext, err)
		return
	}
	items := make([]taskViewResponse, 0, len(views))
	for _, view := range views {
		items = append(items, newTaskViewResponse(view))
	}
	respondJSON(ginContext, http.StatusOK, taskListResponse{Tasks: items})
}

type cancelTaskRequest struct {
	CommandID string `json:"command_id" binding:"required"`
}

type cancelTaskResponse struct {
	TaskID            contracts.TaskID              `json:"task_id"`
	TaskStatus        contracts.TaskStatus          `json:"task_status"`
	RunStatus         contracts.RunStatus           `json:"run_status"`
	ExecutionStatus   contracts.TaskExecutionStatus `json:"execution_status"`
	ExecutionVersion  contracts.ExecutionVersion    `json:"execution_version"`
	TerminationReason contracts.TerminationReason   `json:"termination_reason"`
}

// Cancel 绑定并处理 POST /v1/tasks/:task_id/cancel。
func (h *TaskHandler) Cancel(ginContext *gin.Context) {
	taskID, ok := bindTaskID(ginContext)
	if !ok {
		return
	}
	var body cancelTaskRequest
	if err := bindJSON(ginContext, &body); err != nil {
		writeError(ginContext, http.StatusBadRequest, "InvalidArgument", "invalid JSON request")
		return
	}
	cancelled, err := h.canceller.CancelTask(ginContext.Request.Context(), taskruntime.CancelTaskRequest{
		CommandID:  taskruntime.CommandID(body.CommandID),
		TaskID:     taskID,
		OperatorID: h.operatorID,
	})
	if err != nil {
		writeApplicationError(ginContext, err)
		return
	}
	respondJSON(ginContext, http.StatusOK, cancelTaskResponse{
		TaskID: cancelled.TaskID, TaskStatus: cancelled.TaskStatus, RunStatus: cancelled.RunStatus,
		ExecutionStatus: cancelled.ExecutionStatus, ExecutionVersion: cancelled.ExecutionVersion,
		TerminationReason: cancelled.TerminationReason,
	})
}

type taskURI struct {
	TaskID string `uri:"task_id" binding:"required"`
}

func bindTaskID(ginContext *gin.Context) (contracts.TaskID, bool) {
	var parameters taskURI
	if err := ginContext.ShouldBindUri(&parameters); err != nil {
		writeError(ginContext, http.StatusBadRequest, "InvalidArgument", "invalid Task ID")
		return "", false
	}
	return contracts.TaskID(parameters.TaskID), true
}

type taskViewResponse struct {
	TaskID                  contracts.TaskID              `json:"task_id"`
	AgentID                 contracts.AgentID             `json:"agent_id"`
	Status                  contracts.TaskStatus          `json:"status"`
	CurrentRunID            contracts.RunID               `json:"current_run_id"`
	CurrentExecutionVersion contracts.ExecutionVersion    `json:"current_execution_version"`
	RunStatus               contracts.RunStatus           `json:"run_status"`
	CurrentStepID           *contracts.StepID             `json:"current_step_id"`
	ExecutionStatus         contracts.TaskExecutionStatus `json:"execution_status"`
	Recoverable             bool                          `json:"recoverable"`
	ResultSummary           *string                       `json:"result_summary"`
	ErrorCode               *contracts.ErrorCode          `json:"error_code"`
	DeadlineAt              time.Time                     `json:"deadline_at"`
	QueuedAt                *time.Time                    `json:"queued_at"`
	CreatedAt               time.Time                     `json:"created_at"`
	StartedAt               *time.Time                    `json:"started_at"`
	EndedAt                 *time.Time                    `json:"ended_at"`
}

type taskListResponse struct {
	Tasks []taskViewResponse `json:"tasks"`
}

func newTaskViewResponse(view taskruntime.TaskView) taskViewResponse {
	return taskViewResponse{
		TaskID: view.Task.TaskID, AgentID: view.Task.AgentID, Status: view.Task.Status,
		CurrentRunID: view.Task.CurrentRunID, CurrentExecutionVersion: view.Task.CurrentExecutionVersion,
		RunStatus: view.Run.Status, CurrentStepID: view.Run.CurrentStepID,
		ExecutionStatus: view.Execution.Status, Recoverable: view.Recoverable,
		ResultSummary: view.Task.ResultSummary, ErrorCode: view.Task.ErrorCode,
		DeadlineAt: view.Task.DeadlineAt, QueuedAt: view.Task.QueuedAt, CreatedAt: view.Task.CreatedAt,
		StartedAt: view.Task.StartedAt, EndedAt: view.Task.EndedAt,
	}
}

func bindJSON(ginContext *gin.Context, target any) error {
	ginContext.Request.Body = http.MaxBytesReader(ginContext.Writer, ginContext.Request.Body, maxTaskRequestBytes)
	return ginContext.ShouldBindWith(target, strictJSONBinding{})
}

type strictJSONBinding struct{}

func (strictJSONBinding) Name() string {
	return "strict-json"
}

func (strictJSONBinding) Bind(request *http.Request, target any) error {
	rawBody, err := io.ReadAll(request.Body)
	if err != nil {
		return err
	}
	if !utf8.Valid(rawBody) {
		return errors.New("request body is not valid UTF-8")
	}

	decoder := json.NewDecoder(bytes.NewReader(rawBody))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return errors.New("multiple JSON values")
	}
	return binding.Validator.ValidateStruct(target)
}

func writeApplicationError(ginContext *gin.Context, err error) {
	status, code, message := http.StatusInternalServerError, "InternalError", "internal server error"
	switch {
	case errors.Is(err, taskruntime.ErrInvalidArgument):
		status, code, message = http.StatusBadRequest, "InvalidArgument", "invalid request"
	case errors.Is(err, taskruntime.ErrRepositoryNotFound):
		status, code, message = http.StatusNotFound, "NotFound", "Task not found"
	case errors.Is(err, taskruntime.ErrCommandConflict):
		status, code, message = http.StatusConflict, "CommandConflict", "command conflicts with an existing request"
	case errors.Is(err, taskruntime.ErrTaskAlreadyTerminal):
		status, code, message = http.StatusConflict, "TaskAlreadyTerminal", "Task is already terminal"
	case errors.Is(err, taskruntime.ErrTaskTimedOut):
		status, code, message = http.StatusConflict, string(contracts.ErrorCodeTaskTimeout), "Task has timed out"
	case errors.Is(err, taskruntime.ErrAgentUnavailable):
		status, code, message = http.StatusUnprocessableEntity, "AgentUnavailable", "Agent is unavailable"
	case errors.Is(err, taskruntime.ErrRecoverStateConflict):
		status, code, message = http.StatusConflict, "RecoverStateConflict", "Task is not recoverable"
	case errors.Is(err, taskruntime.ErrRecoverConfigMismatch):
		status, code, message = http.StatusConflict, string(contracts.ErrorCodeConfigVersionMismatch), "execution configuration does not match"
	case errors.Is(err, taskruntime.ErrRecoverCheckpointInvalid):
		status, code, message = http.StatusConflict, string(contracts.ErrorCodeCheckpointInvalid), "recovery checkpoint is invalid"
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		status, code, message = http.StatusRequestTimeout, "RequestCanceled", "request canceled"
	}
	writeError(ginContext, status, code, message)
}

type errorResponse struct {
	Error errorDetails `json:"error"`
}

type errorDetails struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

func writeError(ginContext *gin.Context, status int, code, message string) {
	respondJSON(ginContext, status, errorResponse{Error: errorDetails{Code: code, Message: message}})
}

func respondJSON(ginContext *gin.Context, status int, response any) {
	ginContext.Header("Content-Type", "application/json")
	ginContext.JSON(status, response)
}
