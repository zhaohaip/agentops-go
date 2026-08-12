package api_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/zhaohaip/agentops-go/internal/api"
	"github.com/zhaohaip/agentops-go/internal/app"
	"github.com/zhaohaip/agentops-go/internal/contracts"
	"github.com/zhaohaip/agentops-go/internal/taskruntime"
)

const testBearerToken = "phase-one-test-token"

func TestMain(testingMain *testing.M) {
	gin.SetMode(gin.TestMode)
	os.Exit(testingMain.Run())
}

func TestTaskHandlerCreateGetListAndCancel(t *testing.T) {
	now := time.Date(2026, 8, 7, 12, 0, 0, 0, time.UTC)
	application := &fakeTaskApplication{
		created: taskruntime.TaskCreated{TaskID: "task-1", RunID: "run-1", Status: contracts.TaskStatusPending,
			CurrentExecutionVersion: 1, DeadlineAt: now.Add(time.Hour), QueuedAt: now},
		cancelled: taskruntime.TaskCancelled{TaskID: "task-1", TaskStatus: contracts.TaskStatusCancelled,
			RunStatus: contracts.RunStatusFailed, ExecutionStatus: contracts.TaskExecutionStatusInterrupted,
			ExecutionVersion: 1, TerminationReason: contracts.TerminationReasonCancelled},
		view: taskruntime.TaskView{
			Task: taskruntime.Task{TaskID: "task-1", AgentID: "agent-1", Status: contracts.TaskStatusInterrupted,
				CurrentRunID: "run-1", CurrentExecutionVersion: 1, DeadlineAt: now.Add(time.Hour), CreatedAt: now},
			Run: taskruntime.Run{RunID: "run-1", TaskID: "task-1", Status: contracts.RunStatusRunning},
			Execution: taskruntime.TaskExecution{TaskID: "task-1", ExecutionVersion: 1,
				Status: contracts.TaskExecutionStatusInterrupted},
			Recoverable: true,
		},
	}
	router, logs := newTaskRouter(t, application)

	response := performRequest(router, http.MethodPost, "/v1/tasks",
		`{"command_id":"create-1","agent_id":"agent-1","input":"private-input"}`, testBearerToken)
	if response.Code != http.StatusCreated {
		t.Fatalf("Create status = %d, body = %s", response.Code, response.Body.String())
	}
	if response.Header().Get("Content-Type") != "application/json" {
		t.Fatalf("Create Content-Type = %q", response.Header().Get("Content-Type"))
	}
	if application.createRequest.OperatorID != "api-operator" || application.createRequest.TaskInput != "private-input" {
		t.Fatalf("Create request = %+v", application.createRequest)
	}

	response = performRequest(router, http.MethodGet, "/v1/tasks/task-1", "", testBearerToken)
	if response.Code != http.StatusOK {
		t.Fatalf("Get status = %d, body = %s", response.Code, response.Body.String())
	}
	var getBody struct {
		TaskID      string `json:"task_id"`
		Recoverable bool   `json:"recoverable"`
	}
	decodeResponse(t, response, &getBody)
	if getBody.TaskID != "task-1" || !getBody.Recoverable {
		t.Fatalf("Get body = %+v", getBody)
	}

	application.views = []taskruntime.TaskView{application.view}
	response = performRequest(router, http.MethodGet, "/v1/tasks?status=INTERRUPTED", "", testBearerToken)
	if response.Code != http.StatusOK || application.listStatus == nil || *application.listStatus != contracts.TaskStatusInterrupted {
		t.Fatalf("List status = %d, filter = %v, body = %s", response.Code, application.listStatus, response.Body.String())
	}

	response = performRequest(router, http.MethodPost, "/v1/tasks/task-1/cancel", `{"command_id":"cancel-1"}`, testBearerToken)
	if response.Code != http.StatusOK {
		t.Fatalf("Cancel status = %d, body = %s", response.Code, response.Body.String())
	}
	if application.cancelRequest.TaskID != "task-1" || application.cancelRequest.OperatorID != "api-operator" {
		t.Fatalf("Cancel request = %+v", application.cancelRequest)
	}

	logged := logs.String()
	for _, secret := range []string{testBearerToken, "private-input", "Authorization", "status=INTERRUPTED"} {
		if strings.Contains(logged, secret) {
			t.Fatalf("log contains sensitive or unconstrained request data %q: %s", secret, logged)
		}
	}
}

func TestTaskHandlerAuthenticationAndStrictInput(t *testing.T) {
	application := &fakeTaskApplication{}
	router, _ := newTaskRouter(t, application)

	for _, token := range []string{"", "wrong"} {
		response := performRequest(router, http.MethodPost, "/v1/tasks", `{}`, token)
		if response.Code != http.StatusUnauthorized {
			t.Fatalf("token %q status = %d", token, response.Code)
		}
	}
	if application.createCalls != 0 {
		t.Fatalf("unauthorized Create calls = %d", application.createCalls)
	}

	for _, body := range []string{
		`{}`,
		`{"command_id":"c","agent_id":"a","input":"i","unknown":true}`,
		`{"command_id":"c","agent_id":"a","input":"i"}{}`,
		`{"command_id":`,
	} {
		response := performRequest(router, http.MethodPost, "/v1/tasks", body, testBearerToken)
		if response.Code != http.StatusBadRequest {
			t.Fatalf("body %q status = %d, response = %s", body, response.Code, response.Body.String())
		}
	}
	response := performRequest(router, http.MethodGet, "/v1/tasks?status=NOT_A_STATUS", "", testBearerToken)
	if response.Code != http.StatusBadRequest {
		t.Fatalf("invalid status = %d, body = %s", response.Code, response.Body.String())
	}
}

func TestTaskHandlerRejectsRawInvalidUTF8BeforeService(t *testing.T) {
	tests := []struct {
		name       string
		method     string
		path       string
		body       []byte
		serviceHit func(*fakeTaskApplication) int
	}{
		{
			name:   "Create",
			method: http.MethodPost,
			path:   "/v1/tasks",
			body: append(append([]byte(`{"command_id":"create-invalid","agent_id":"agent-1","input":"`), 0xff),
				[]byte(`"}`)...),
			serviceHit: func(application *fakeTaskApplication) int { return application.createCalls },
		},
		{
			name:   "Cancel",
			method: http.MethodPost,
			path:   "/v1/tasks/task-1/cancel",
			body: append(append([]byte(`{"command_id":"cancel-`), 0xff),
				[]byte(`"}`)...),
			serviceHit: func(application *fakeTaskApplication) int { return application.cancelCalls },
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			application := &fakeTaskApplication{}
			router, _ := newTaskRouter(t, application)
			response := performRawRequest(router, test.method, test.path, test.body, testBearerToken)
			if response.Code != http.StatusBadRequest ||
				!strings.Contains(response.Body.String(), `"code":"InvalidArgument"`) {
				t.Fatalf("response = %d %s", response.Code, response.Body.String())
			}
			if calls := test.serviceHit(application); calls != 0 {
				t.Fatalf("Service calls = %d, want 0", calls)
			}
		})
	}
}

func TestTaskRouterPreservesNotFoundAndMethodBehavior(t *testing.T) {
	application := &fakeTaskApplication{}
	router, _ := newTaskRouter(t, application)
	tests := []struct {
		name   string
		method string
		path   string
		token  string
		want   int
	}{
		{name: "unknown route", method: http.MethodGet, path: "/unknown", token: testBearerToken, want: http.StatusNotFound},
		{name: "wrong method", method: http.MethodPut, path: "/v1/tasks", token: testBearerToken, want: http.StatusNotFound},
		{name: "trailing slash", method: http.MethodGet, path: "/v1/tasks/", token: testBearerToken, want: http.StatusNotFound},
		{name: "authentication precedes routing", method: http.MethodGet, path: "/unknown", want: http.StatusUnauthorized},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			response := performRequest(router, test.method, test.path, "", test.token)
			if response.Code != test.want {
				t.Fatalf("status = %d, want %d, body = %s", response.Code, test.want, response.Body.String())
			}
			if response.Header().Get("Content-Type") != "application/json" {
				t.Fatalf("Content-Type = %q", response.Header().Get("Content-Type"))
			}
		})
	}
}

func TestTaskRouterPassesHTTPRequestContextToApplication(t *testing.T) {
	type contextKey string
	const key contextKey = "request-marker"
	application := &fakeTaskApplication{created: taskruntime.TaskCreated{
		TaskID: "task-context", RunID: "run-context", Status: contracts.TaskStatusPending,
		CurrentExecutionVersion: 1, DeadlineAt: time.Now().Add(time.Hour), QueuedAt: time.Now(),
	}}
	router, _ := newTaskRouter(t, application)
	request := httptest.NewRequest(http.MethodPost, "/v1/tasks",
		strings.NewReader(`{"command_id":"create-context","agent_id":"agent-1","input":"task"}`))
	request.Header.Set("Authorization", "Bearer "+testBearerToken)
	request = request.WithContext(context.WithValue(request.Context(), key, "from-net-http"))
	response := httptest.NewRecorder()
	router.ServeHTTP(response, request)
	if response.Code != http.StatusCreated {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
	}
	if application.requestContext == nil || application.requestContext.Value(key) != "from-net-http" {
		t.Fatal("application did not receive the HTTP request context")
	}
}

func TestTaskHandlerMapsApplicationErrorsWithoutDisclosure(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want int
		code string
	}{
		{name: "invalid", err: taskruntime.ErrInvalidArgument, want: http.StatusBadRequest, code: "InvalidArgument"},
		{name: "not found", err: taskruntime.ErrRepositoryNotFound, want: http.StatusNotFound, code: "NotFound"},
		{name: "conflict", err: taskruntime.ErrCommandConflict, want: http.StatusConflict, code: "CommandConflict"},
		{name: "terminal", err: taskruntime.ErrTaskAlreadyTerminal, want: http.StatusConflict, code: "TaskAlreadyTerminal"},
		{name: "timeout", err: taskruntime.ErrTaskTimedOut, want: http.StatusConflict, code: string(contracts.ErrorCodeTaskTimeout)},
		{name: "unavailable", err: taskruntime.ErrAgentUnavailable, want: http.StatusUnprocessableEntity, code: "AgentUnavailable"},
		{name: "canceled", err: context.Canceled, want: http.StatusRequestTimeout, code: "RequestCanceled"},
		{name: "system", err: errors.New("postgres password=never-log-this"), want: http.StatusInternalServerError, code: "InternalError"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			application := &fakeTaskApplication{queryErr: test.err}
			router, logs := newTaskRouter(t, application)
			response := performRequest(router, http.MethodGet, "/v1/tasks/task-1", "", testBearerToken)
			if response.Code != test.want || !strings.Contains(response.Body.String(), `"code":"`+test.code+`"`) {
				t.Fatalf("response = %d %s", response.Code, response.Body.String())
			}
			if strings.Contains(response.Body.String(), "never-log-this") || strings.Contains(logs.String(), "never-log-this") {
				t.Fatal("internal error detail was disclosed")
			}
		})
	}
}

func newTaskRouter(t *testing.T, application *fakeTaskApplication) (http.Handler, *bytes.Buffer) {
	t.Helper()
	var logs bytes.Buffer
	router, err := app.NewTaskRouter(api.TaskHandlerDependencies{
		Creator: application, Canceller: application, Querier: application,
		BearerToken: testBearerToken, OperatorID: "api-operator",
		Logger: slog.New(slog.NewJSONHandler(&logs, nil)),
	})
	if err != nil {
		t.Fatal(err)
	}
	return router, &logs
}

func performRequest(handler http.Handler, method, target, body, token string) *httptest.ResponseRecorder {
	return performRawRequest(handler, method, target, []byte(body), token)
}

func performRawRequest(handler http.Handler, method, target string, body []byte, token string) *httptest.ResponseRecorder {
	request := httptest.NewRequest(method, target, bytes.NewReader(body))
	if token != "" {
		request.Header.Set("Authorization", "Bearer "+token)
	}
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	return response
}

func decodeResponse(t *testing.T, response *httptest.ResponseRecorder, target any) {
	t.Helper()
	if err := json.Unmarshal(response.Body.Bytes(), target); err != nil {
		t.Fatal(err)
	}
}

type fakeTaskApplication struct {
	created        taskruntime.TaskCreated
	createErr      error
	createRequest  taskruntime.CreateTaskRequest
	createCalls    int
	cancelled      taskruntime.TaskCancelled
	cancelErr      error
	cancelRequest  taskruntime.CancelTaskRequest
	cancelCalls    int
	view           taskruntime.TaskView
	views          []taskruntime.TaskView
	queryErr       error
	listStatus     *contracts.TaskStatus
	requestContext context.Context
}

func (f *fakeTaskApplication) CreateTask(ctx context.Context, request taskruntime.CreateTaskRequest) (taskruntime.TaskCreated, error) {
	f.createCalls++
	f.createRequest = request
	f.requestContext = ctx
	return f.created, f.createErr
}

func (f *fakeTaskApplication) CancelTask(_ context.Context, request taskruntime.CancelTaskRequest) (taskruntime.TaskCancelled, error) {
	f.cancelCalls++
	f.cancelRequest = request
	return f.cancelled, f.cancelErr
}

func (f *fakeTaskApplication) GetTask(context.Context, contracts.TaskID) (taskruntime.TaskView, error) {
	return f.view, f.queryErr
}

func (f *fakeTaskApplication) ListTasks(_ context.Context, status *contracts.TaskStatus) ([]taskruntime.TaskView, error) {
	if status != nil {
		copy := *status
		f.listStatus = &copy
	}
	return f.views, f.queryErr
}
