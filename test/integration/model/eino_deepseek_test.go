package model_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	einoadapter "github.com/zhaohaip/agentops-go/internal/adapter/eino"
	"github.com/zhaohaip/agentops-go/internal/contracts"
)

const maxModelResponseBodyBytes = 1 << 20

type chatCompletionRequest struct {
	Model    string `json:"model"`
	Stream   bool   `json:"stream"`
	Messages []struct {
		Role    string `json:"role"`
		Content string `json:"content"`
	} `json:"messages"`
	Temperature    float32 `json:"temperature"`
	TopP           float32 `json:"top_p"`
	MaxTokens      int     `json:"max_tokens"`
	ResponseFormat struct {
		Type string `json:"type"`
	} `json:"response_format"`
}

func TestEinoDeepSeekAdapterSendsFixedStructuredRequest(t *testing.T) {
	t.Parallel()
	requestSeen := make(chan chatCompletionRequest, 1)
	rawBodySeen := make(chan string, 1)
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/chat/completions" {
			t.Errorf("unexpected path: %s", request.URL.Path)
		}
		if got := request.Header.Get("Authorization"); got != "Bearer test-api-key" {
			t.Errorf("unexpected authorization header")
		}
		var body bytes.Buffer
		if _, err := body.ReadFrom(request.Body); err != nil {
			t.Errorf("read request: %v", err)
			return
		}
		rawBodySeen <- body.String()
		var decoded chatCompletionRequest
		if err := json.Unmarshal(body.Bytes(), &decoded); err != nil {
			t.Errorf("decode request: %v", err)
			return
		}
		requestSeen <- decoded
		writeJSONResponse(writer, "request-123", `{"goal":"safe"}`)
	}))
	defer server.Close()

	client := newTestClient(t, server.URL, nil)
	request := validModelRequest()
	response, err := client.GenerateStructured(context.Background(), request)
	if err != nil {
		t.Fatalf("GenerateStructured: %v", err)
	}
	if response.AssistantContent != `{"goal":"safe"}` {
		t.Fatalf("unexpected content: %q", response.AssistantContent)
	}
	if response.ProviderRequestID == nil || *response.ProviderRequestID != "request-123" {
		t.Fatalf("unexpected provider request id: %#v", response.ProviderRequestID)
	}

	actual := <-requestSeen
	if actual.Model != "deepseek-chat" || actual.Stream || actual.ResponseFormat.Type != "json_object" {
		t.Fatalf("fixed request fields mismatch: %#v", actual)
	}
	if len(actual.Messages) != 2 || actual.Messages[0].Role != "system" ||
		actual.Messages[0].Content != "system prompt" || actual.Messages[1].Role != "user" ||
		actual.Messages[1].Content != "user prompt" {
		t.Fatalf("messages mismatch: %#v", actual.Messages)
	}
	if actual.Temperature != 0.2 || actual.TopP != 1 || actual.MaxTokens != 4096 {
		t.Fatalf("generation params mismatch: %#v", actual)
	}
	rawBody := <-rawBodySeen
	for _, metadata := range []string{"task-log-123", "run-log-456", "INITIAL"} {
		if strings.Contains(rawBody, metadata) {
			t.Fatalf("metadata leaked into provider request: %s", metadata)
		}
	}
}

func TestEinoDeepSeekAdapterRejectsContractViolationsBeforeHTTP(t *testing.T) {
	t.Parallel()
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		writeJSONResponse(writer, "request-1", `{}`)
	}))
	defer server.Close()
	client := newTestClient(t, server.URL, nil)

	tests := []struct {
		name   string
		mutate func(*contracts.ModelRequest)
	}{
		{name: "model", mutate: func(request *contracts.ModelRequest) { request.Model = "other" }},
		{name: "stream", mutate: func(request *contracts.ModelRequest) { request.Stream = true }},
		{name: "response format", mutate: func(request *contracts.ModelRequest) { request.ResponseFormat = "text" }},
		{name: "messages", mutate: func(request *contracts.ModelRequest) { request.Messages = nil }},
		{name: "message role", mutate: func(request *contracts.ModelRequest) { request.Messages[0].Role = "assistant" }},
		{name: "temperature", mutate: func(request *contracts.ModelRequest) {
			request.GenerationParams.Temperature = contracts.NewCanonicalDecimalV1(21, 1)
		}},
		{name: "top p", mutate: func(request *contracts.ModelRequest) {
			request.GenerationParams.TopP = contracts.NewCanonicalDecimalV1(0, 0)
		}},
		{name: "max tokens", mutate: func(request *contracts.ModelRequest) {
			request.GenerationParams.MaxOutputTokens = 8193
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			request := validModelRequest()
			test.mutate(&request)
			_, err := client.GenerateStructured(context.Background(), request)
			assertModelError(t, err, contracts.ModelClientErrorContractViolation, nil)
		})
	}
	if calls.Load() != 0 {
		t.Fatalf("contract violations made %d HTTP calls", calls.Load())
	}
}

func TestEinoDeepSeekAdapterMapsProviderErrorsWithoutRetryOrLeak(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		status int
		kind   contracts.ModelClientErrorKind
	}{
		{name: "request timeout", status: http.StatusRequestTimeout, kind: contracts.ModelClientErrorTimeout},
		{name: "unauthorized", status: http.StatusUnauthorized, kind: contracts.ModelClientErrorAuthentication},
		{name: "forbidden", status: http.StatusForbidden, kind: contracts.ModelClientErrorAuthentication},
		{name: "rate limited", status: http.StatusTooManyRequests, kind: contracts.ModelClientErrorRateLimited},
		{name: "server error", status: http.StatusInternalServerError, kind: contracts.ModelClientErrorProvider},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var calls atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
				calls.Add(1)
				writer.Header().Set("Content-Type", "application/json")
				writer.WriteHeader(test.status)
				_, _ = writer.Write([]byte(`{"error":{"message":"provider secret","type":"provider_error"}}`))
			}))
			defer server.Close()
			client := newTestClient(t, server.URL, nil)

			_, err := client.GenerateStructured(context.Background(), validModelRequest())
			assertModelError(t, err, test.kind, nil)
			if calls.Load() != 1 {
				t.Fatalf("expected one provider call, got %d", calls.Load())
			}
			if strings.Contains(err.Error(), "provider secret") || errors.Unwrap(err) != nil {
				t.Fatal("provider error leaked through public error")
			}
		})
	}
}

func TestEinoDeepSeekAdapterMapsProtocolAndEmptyResponses(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		body string
		kind contracts.ModelClientErrorKind
	}{
		{name: "malformed provider response", body: `{`, kind: contracts.ModelClientErrorProvider},
		{name: "missing choice", body: `{"id":"request-1","choices":[]}`, kind: contracts.ModelClientErrorProvider},
		{name: "empty assistant content", body: chatResponse("request-1", ""), kind: contracts.ModelClientErrorInvalidResponse},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
				writer.Header().Set("Content-Type", "application/json")
				_, _ = writer.Write([]byte(test.body))
			}))
			defer server.Close()
			client := newTestClient(t, server.URL, nil)
			_, err := client.GenerateStructured(context.Background(), validModelRequest())
			assertModelError(t, err, test.kind, nil)
		})
	}
}

func TestEinoDeepSeekAdapterPropagatesCancelAndDeadline(t *testing.T) {
	t.Parallel()
	t.Run("cancel", func(t *testing.T) {
		entered := make(chan struct{})
		release := make(chan struct{})
		server := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, request *http.Request) {
			close(entered)
			select {
			case <-request.Context().Done():
			case <-release:
			}
		}))
		defer func() {
			close(release)
			server.CloseClientConnections()
			server.Close()
		}()
		client := newTestClient(t, server.URL, nil)
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan error, 1)
		go func() {
			_, err := client.GenerateStructured(ctx, validModelRequest())
			done <- err
		}()
		select {
		case <-entered:
		case <-time.After(2 * time.Second):
			t.Fatal("provider request did not enter handler")
		}
		cancel()
		select {
		case err := <-done:
			assertModelError(t, err, contracts.ModelClientErrorCanceled, context.Canceled)
		case <-time.After(2 * time.Second):
			t.Fatal("canceled call did not return")
		}
	})

	t.Run("deadline", func(t *testing.T) {
		entered := make(chan struct{})
		release := make(chan struct{})
		server := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, request *http.Request) {
			close(entered)
			select {
			case <-request.Context().Done():
			case <-release:
			}
		}))
		defer func() {
			close(release)
			server.CloseClientConnections()
			server.Close()
		}()
		client := newTestClient(t, server.URL, nil)
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		done := make(chan error, 1)
		go func() {
			_, err := client.GenerateStructured(ctx, validModelRequest())
			done <- err
		}()
		select {
		case <-entered:
		case <-time.After(2 * time.Second):
			t.Fatal("provider request did not enter handler")
		}
		select {
		case err := <-done:
			assertModelError(t, err, contracts.ModelClientErrorTimeout, context.DeadlineExceeded)
		case <-time.After(2 * time.Second):
			t.Fatal("deadline call did not return")
		}
	})
}

func TestEinoDeepSeekAdapterEnforcesResponseBodyLimit(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		size    int
		wantErr bool
	}{
		{name: "exactly one MiB", size: maxModelResponseBodyBytes},
		{name: "one byte too large", size: maxModelResponseBodyBytes + 1, wantErr: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			body := sizedChatResponse(t, test.size)
			server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
				writer.Header().Set("Content-Type", "application/json")
				_, _ = writer.Write(body)
			}))
			defer server.Close()
			client := newTestClient(t, server.URL, nil)
			response, err := client.GenerateStructured(context.Background(), validModelRequest())
			if test.wantErr {
				assertModelError(t, err, contracts.ModelClientErrorResponseTooLarge, nil)
				return
			}
			if err != nil || response.AssistantContent == "" {
				t.Fatalf("boundary response failed: response=%d err=%v", len(response.AssistantContent), err)
			}
		})
	}
}

func TestEinoDeepSeekAdapterMapsNetworkError(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	baseURL := server.URL
	server.Close()
	client := newTestClient(t, baseURL, nil)
	_, err := client.GenerateStructured(context.Background(), validModelRequest())
	assertModelError(t, err, contracts.ModelClientErrorNetwork, nil)
}

func TestEinoDeepSeekAdapterLogsOnlySafeMetadata(t *testing.T) {
	t.Parallel()
	var logs bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&logs, nil))
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writeJSONResponse(writer, "request-safe", "TOP_SECRET_RESPONSE")
	}))
	defer server.Close()
	client := newTestClient(t, server.URL, logger)
	request := validModelRequest()
	request.Messages[0].Content = "TOP_SECRET_PROMPT TOP_SECRET_TASK_INPUT TOP_SECRET_SCHEMA"
	response, err := client.GenerateStructured(context.Background(), request)
	if err != nil || response.AssistantContent != "TOP_SECRET_RESPONSE" {
		t.Fatalf("GenerateStructured: response=%#v err=%v", response, err)
	}
	text := logs.String()
	for _, forbidden := range []string{
		"TOP_SECRET_PROMPT", "TOP_SECRET_TASK_INPUT", "TOP_SECRET_SCHEMA",
		"TOP_SECRET_RESPONSE", "test-api-key",
	} {
		if strings.Contains(text, forbidden) {
			t.Fatalf("sensitive value leaked to logs: %s", forbidden)
		}
	}
	for _, required := range []string{
		`"provider":"deepseek"`, `"model":"deepseek-chat"`, `"phase":"INITIAL"`,
		`"repair":false`, `"task_id":"task-log-123"`, `"run_id":"run-log-456"`,
		`"execution_version":1`, `"success":true`,
	} {
		if !strings.Contains(text, required) {
			t.Fatalf("safe log field missing %s: %s", required, text)
		}
	}
}

func TestEinoDeepSeekAdapterErrorLogUsesSafeClassification(t *testing.T) {
	t.Parallel()
	var logs bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&logs, nil))
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		writer.WriteHeader(http.StatusInternalServerError)
		_, _ = writer.Write([]byte(`{"error":{"message":"RAW_PROVIDER_SECRET","type":"provider_error"}}`))
	}))
	defer server.Close()
	client := newTestClient(t, server.URL, logger)
	request := validModelRequest()
	request.Messages[0].Content = "RAW_PROMPT_SECRET"

	_, err := client.GenerateStructured(context.Background(), request)
	assertModelError(t, err, contracts.ModelClientErrorProvider, nil)
	text := logs.String()
	for _, forbidden := range []string{"RAW_PROVIDER_SECRET", "RAW_PROMPT_SECRET", "test-api-key"} {
		if strings.Contains(text, forbidden) {
			t.Fatalf("sensitive value leaked to error log: %s", forbidden)
		}
	}
	for _, required := range []string{
		`"task_id":"task-log-123"`, `"run_id":"run-log-456"`, `"execution_version":1`,
		`"success":false`, `"error_kind":"Provider"`, `"cause_code":"Provider"`,
	} {
		if !strings.Contains(text, required) {
			t.Fatalf("safe error log field missing %s: %s", required, text)
		}
	}
}

func newTestClient(t *testing.T, baseURL string, logger *slog.Logger) contracts.ModelClient {
	t.Helper()
	client, err := einoadapter.NewEinoDeepSeekModelClient(context.Background(), einoadapter.DeepSeekConfig{
		BaseURL: baseURL, APIKey: "test-api-key", HTTPClient: &http.Client{}, Logger: logger,
	})
	if err != nil {
		t.Fatalf("create client: %v", err)
	}
	return client
}

func validModelRequest() contracts.ModelRequest {
	executionVersion := contracts.ExecutionVersion(1)
	return contracts.ModelRequest{
		Model: "deepseek-chat", Stream: false, ResponseFormat: "json_object",
		Messages: []contracts.ModelMessage{
			{Role: contracts.ModelMessageRoleSystem, Content: "system prompt"},
			{Role: contracts.ModelMessageRoleUser, Content: "user prompt"},
		},
		GenerationParams: contracts.GenerationParams{
			Temperature: contracts.NewCanonicalDecimalV1(2, 1),
			TopP:        contracts.NewCanonicalDecimalV1(1, 0), MaxOutputTokens: 4096,
		},
		Metadata: contracts.ModelRequestMetadata{
			Operation: "GeneratePlan", Phase: "INITIAL", TaskID: "task-log-123",
			RunID: "run-log-456", ExecutionVersion: &executionVersion,
		},
	}
}

func writeJSONResponse(writer http.ResponseWriter, requestID, content string) {
	writer.Header().Set("Content-Type", "application/json")
	_, _ = writer.Write([]byte(chatResponse(requestID, content)))
}

func chatResponse(requestID, content string) string {
	encoded, _ := json.Marshal(struct {
		ID      string `json:"id"`
		Object  string `json:"object"`
		Created int64  `json:"created"`
		Model   string `json:"model"`
		Choices []struct {
			Index   int `json:"index"`
			Message struct {
				Role    string `json:"role"`
				Content string `json:"content"`
			} `json:"message"`
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
	}{
		ID: requestID, Object: "chat.completion", Created: 1, Model: "deepseek-chat",
		Choices: []struct {
			Index   int `json:"index"`
			Message struct {
				Role    string `json:"role"`
				Content string `json:"content"`
			} `json:"message"`
			FinishReason string `json:"finish_reason"`
		}{
			{Index: 0, Message: struct {
				Role    string `json:"role"`
				Content string `json:"content"`
			}{Role: "assistant", Content: content}, FinishReason: "stop"},
		},
	})
	return string(encoded)
}

func sizedChatResponse(t *testing.T, size int) []byte {
	t.Helper()
	empty := []byte(chatResponse("request-sized", ""))
	contentBytes := size - len(empty)
	if contentBytes <= 0 {
		t.Fatalf("target size %d is too small", size)
	}
	body := []byte(chatResponse("request-sized", strings.Repeat("a", contentBytes)))
	if len(body) != size {
		t.Fatalf("sized response: got %d want %d", len(body), size)
	}
	return body
}

func assertModelError(
	t *testing.T,
	err error,
	want contracts.ModelClientErrorKind,
	wantCause error,
) {
	t.Helper()
	var typed *contracts.ModelClientError
	if !errors.As(err, &typed) || typed == nil || typed.Kind != want {
		t.Fatalf("model error: got %T %v want %s", err, err, want)
	}
	if wantCause != nil && !errors.Is(err, wantCause) {
		t.Fatalf("model error does not preserve %v: %v", wantCause, err)
	}
	if wantCause == nil && errors.Unwrap(typed) != nil {
		t.Fatalf("unexpected public cause for %s: %v", want, errors.Unwrap(typed))
	}
	if strings.Contains(fmt.Sprint(err), "provider secret") {
		t.Fatal("provider text leaked through public error")
	}
}
