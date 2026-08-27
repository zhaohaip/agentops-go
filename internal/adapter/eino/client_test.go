package eino

import (
	"context"
	"errors"
	"testing"

	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"

	"github.com/zhaohaip/agentops-go/internal/contracts"
)

type contextKey string

type recordingChatModel struct {
	generateCalls int
	streamCalls   int
	contextValue  any
	options       *model.Options
}

func (m *recordingChatModel) Generate(
	ctx context.Context,
	_ []*schema.Message,
	opts ...model.Option,
) (*schema.Message, error) {
	m.generateCalls++
	m.contextValue = ctx.Value(contextKey("trace"))
	m.options = model.GetCommonOptions(nil, opts...)
	return schema.AssistantMessage(`{"goal":"ok"}`, nil), nil
}

func (m *recordingChatModel) Stream(
	context.Context,
	[]*schema.Message,
	...model.Option,
) (*schema.StreamReader[*schema.Message], error) {
	m.streamCalls++
	return nil, errors.New("stream must not be called")
}

func TestGenerateStructuredUsesOnlyBaseChatModelGenerate(t *testing.T) {
	t.Parallel()
	chatModel := &recordingChatModel{}
	client := newEinoDeepSeekModelClient(chatModel, nil)
	ctx := context.WithValue(context.Background(), contextKey("trace"), "trace-value")

	response, err := client.GenerateStructured(ctx, unitModelRequest())
	if err != nil {
		t.Fatalf("GenerateStructured: %v", err)
	}
	if response.AssistantContent != `{"goal":"ok"}` {
		t.Fatalf("unexpected response: %#v", response)
	}
	if chatModel.generateCalls != 1 || chatModel.streamCalls != 0 {
		t.Fatalf("unexpected calls: generate=%d stream=%d", chatModel.generateCalls, chatModel.streamCalls)
	}
	if chatModel.contextValue != "trace-value" {
		t.Fatalf("context value was not propagated: %#v", chatModel.contextValue)
	}
	if chatModel.options == nil || chatModel.options.Temperature == nil ||
		*chatModel.options.Temperature != 0.2 || chatModel.options.TopP == nil ||
		*chatModel.options.TopP != 1 || chatModel.options.MaxTokens == nil ||
		*chatModel.options.MaxTokens != 4096 {
		t.Fatalf("unexpected Eino options: %#v", chatModel.options)
	}
}

func TestMapModelClientErrorContextCancellationPrecedesResponseLimit(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	err := mapModelClientError(ctx, errResponseTooLarge)
	if err.Kind != contracts.ModelClientErrorCanceled {
		t.Fatalf("error kind: got %s want %s", err.Kind, contracts.ModelClientErrorCanceled)
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatal("cancellation sentinel was not preserved")
	}
	if errors.Is(err, errResponseTooLarge) {
		t.Fatal("response-limit error must not enter the public error chain")
	}
}

func unitModelRequest() contracts.ModelRequest {
	return contracts.ModelRequest{
		Model:          deepSeekModel,
		ResponseFormat: deepSeekResponseFormat,
		Messages: []contracts.ModelMessage{
			{Role: contracts.ModelMessageRoleSystem, Content: "prompt"},
		},
		GenerationParams: contracts.GenerationParams{
			Temperature:     contracts.NewCanonicalDecimalV1(2, 1),
			TopP:            contracts.NewCanonicalDecimalV1(1, 0),
			MaxOutputTokens: 4096,
		},
	}
}
