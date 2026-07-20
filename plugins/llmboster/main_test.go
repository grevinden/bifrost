package llmboster

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/grevinden/bifrost/core/schemas"
)

type noopLogger struct{}

func (noopLogger) Debug(msg string, args ...any)          {}
func (noopLogger) Info(msg string, args ...any)           {}
func (noopLogger) Warn(msg string, args ...any)           {}
func (noopLogger) Error(msg string, args ...any)          {}
func (noopLogger) Fatal(msg string, args ...any)          {}
func (noopLogger) SetLevel(schemas.LogLevel)              {}
func (noopLogger) SetOutputType(schemas.LoggerOutputType) {}
func (noopLogger) LogHTTPRequest(schemas.LogLevel, string) schemas.LogEventBuilder {
	return schemas.NoopLogEvent
}

// safeStr safely dereferences a ChatMessageContent for test error messages.
func safeStr(c *schemas.ChatMessageContent) string {
	if c == nil || c.ContentStr == nil {
		return "<nil>"
	}
	return *c.ContentStr
}

func newTestPlugin(t *testing.T) *Plugin {
	t.Helper()
	p, err := Init(&Config{}, noopLogger{})
	if err != nil {
		t.Fatalf("Init: %v", err)
	}
	t.Cleanup(func() { _ = p.Cleanup() })
	return p
}

func newBifrostContext() *schemas.BifrostContext {
	return schemas.NewBifrostContext(context.Background(), time.Now().Add(30*time.Second))
}

// ---------------------------------------------------------------------------
// Init / GetName / Cleanup
// ---------------------------------------------------------------------------

func TestInit(t *testing.T) {
	p, err := Init(&Config{}, noopLogger{})
	if err != nil {
		t.Fatalf("Init: %v", err)
	}
	if p.GetName() != PluginName {
		t.Errorf("GetName() = %q, want %q", p.GetName(), PluginName)
	}
	_ = p.Cleanup()
}

func TestCleanupIdempotent(t *testing.T) {
	p := newTestPlugin(t)
	if err := p.Cleanup(); err != nil {
		t.Errorf("first cleanup: %v", err)
	}
	if err := p.Cleanup(); err != nil {
		t.Errorf("second cleanup: %v", err)
	}
}

// ---------------------------------------------------------------------------
// Embedded prompt
// ---------------------------------------------------------------------------

func TestEmbeddedPromptNotEmpty(t *testing.T) {
	if len(strings.TrimSpace(embeddedSystemPrompt)) == 0 {
		t.Error("embeddedSystemPrompt is empty")
	}
}

// ---------------------------------------------------------------------------
// isEmptyMessage
// ---------------------------------------------------------------------------

func TestIsEmptyMessage(t *testing.T) {
	tests := []struct {
		name string
		msg  schemas.ChatMessage
		want bool
	}{
		{"nil content", schemas.ChatMessage{Content: nil}, true},
		{"empty ContentStr", schemas.ChatMessage{
			Content: &schemas.ChatMessageContent{ContentStr: new("")},
		}, true},
		{"whitespace-only ContentStr", schemas.ChatMessage{
			Content: &schemas.ChatMessageContent{ContentStr: new("   \t\n")},
		}, true},
		{"non-empty ContentStr", schemas.ChatMessage{
			Content: &schemas.ChatMessageContent{ContentStr: new("hello")},
		}, false},
		{"ContentBlocks present", schemas.ChatMessage{
			Content: &schemas.ChatMessageContent{ContentBlocks: []schemas.ChatContentBlock{
				{Type: schemas.ChatContentBlockTypeText, Text: new("hi")},
			}},
		}, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := isEmptyMessage(tc.msg); got != tc.want {
				t.Errorf("isEmptyMessage() = %v, want %v", got, tc.want)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// PreLLMHook
// ---------------------------------------------------------------------------

func TestPreLLMHook_NilRequest(t *testing.T) {
	p := newTestPlugin(t)
	ctx := newBifrostContext()

	req, sc, err := p.PreLLMHook(ctx, nil)
	if err != nil {
		t.Errorf("error: %v", err)
	}
	if req != nil {
		t.Error("expected nil request")
	}
	if sc != nil {
		t.Error("expected nil short-circuit")
	}
}

func TestPreLLMHook_NoChatRequest(t *testing.T) {
	p := newTestPlugin(t)
	ctx := newBifrostContext()
	req := &schemas.BifrostRequest{RequestType: schemas.EmbeddingRequest}

	out, sc, err := p.PreLLMHook(ctx, req)
	if err != nil {
		t.Errorf("error: %v", err)
	}
	if out != req {
		t.Error("expected same request back")
	}
	if sc != nil {
		t.Error("expected nil short-circuit")
	}
}

func TestPreLLMHook_EmptyInput(t *testing.T) {
	p := newTestPlugin(t)
	ctx := newBifrostContext()
	req := &schemas.BifrostRequest{
		RequestType: schemas.ChatCompletionRequest,
		ChatRequest: &schemas.BifrostChatRequest{
			Input: []schemas.ChatMessage{},
		},
	}

	out, sc, err := p.PreLLMHook(ctx, req)
	if err != nil {
		t.Errorf("error: %v", err)
	}
	if out != req {
		t.Error("expected same request when input empty")
	}
	if sc != nil {
		t.Error("expected nil short-circuit")
	}
}

func TestPreLLMHook_AllEmptyMessages(t *testing.T) {
	p := newTestPlugin(t)
	ctx := newBifrostContext()
	req := &schemas.BifrostRequest{
		RequestType: schemas.ChatCompletionRequest,
		ChatRequest: &schemas.BifrostChatRequest{
			Input: []schemas.ChatMessage{
				{Content: nil},
				{Content: &schemas.ChatMessageContent{ContentStr: new("")}},
			},
		},
	}

	out, sc, err := p.PreLLMHook(ctx, req)
	if err != nil {
		t.Errorf("error: %v", err)
	}
	if out != req {
		t.Error("expected same request when all messages empty")
	}
	if sc != nil {
		t.Error("expected nil short-circuit")
	}
}

func TestPreLLMHook_LastMessageNotUser(t *testing.T) {
	p := newTestPlugin(t)
	ctx := newBifrostContext()
	req := &schemas.BifrostRequest{
		RequestType: schemas.ChatCompletionRequest,
		ChatRequest: &schemas.BifrostChatRequest{
			Input: []schemas.ChatMessage{
				{Role: schemas.ChatMessageRoleUser, Content: &schemas.ChatMessageContent{ContentStr: new("hi")}},
				{Role: schemas.ChatMessageRoleAssistant, Content: &schemas.ChatMessageContent{ContentStr: new("hello")}},
			},
		},
	}

	out, sc, err := p.PreLLMHook(ctx, req)
	if err != nil {
		t.Errorf("error: %v", err)
	}
	if out != req {
		t.Error("expected same request when last message is assistant")
	}
	if sc != nil {
		t.Error("expected nil short-circuit")
	}
}

func TestPreLLMHook_LastMessageUser_PreservesAllMessages(t *testing.T) {
	p := newTestPlugin(t)
	ctx := newBifrostContext()
	req := &schemas.BifrostRequest{
		RequestType: schemas.ChatCompletionRequest,
		ChatRequest: &schemas.BifrostChatRequest{
			Input: []schemas.ChatMessage{
				{Role: schemas.ChatMessageRoleSystem, Content: &schemas.ChatMessageContent{ContentStr: new("old system")}},
				{Role: schemas.ChatMessageRoleUser, Content: &schemas.ChatMessageContent{ContentStr: new("hi")}},
			},
		},
	}

	out, _, err := p.PreLLMHook(ctx, req)
	if err != nil {
		t.Errorf("error: %v", err)
	}
	if out == nil || out.ChatRequest == nil {
		t.Fatal("out is nil")
	}

	messages := out.ChatRequest.Input
	// No client set → no boost → normalized messages preserved as-is.
	// System message stays at position 0 (already canonical).
	if len(messages) != 2 {
		t.Fatalf("got %d messages, want 2", len(messages))
	}

	if messages[0].Role != schemas.ChatMessageRoleSystem {
		t.Errorf("first msg role = %q, want system", messages[0].Role)
	}
	if messages[0].Content == nil || messages[0].Content.ContentStr == nil {
		t.Fatal("system content is nil")
	}
	if *messages[0].Content.ContentStr != "old system" {
		t.Errorf("system content = %q, want %q", *messages[0].Content.ContentStr, "old system")
	}

	if messages[1].Role != schemas.ChatMessageRoleUser {
		t.Errorf("second msg role = %q, want user", messages[1].Role)
	}
}

func TestPreLLMHook_NoSystemMessage(t *testing.T) {
	p := newTestPlugin(t)
	ctx := newBifrostContext()
	req := &schemas.BifrostRequest{
		RequestType: schemas.ChatCompletionRequest,
		ChatRequest: &schemas.BifrostChatRequest{
			Input: []schemas.ChatMessage{
				{Role: schemas.ChatMessageRoleUser, Content: &schemas.ChatMessageContent{ContentStr: new("hi")}},
			},
		},
	}

	out, _, err := p.PreLLMHook(ctx, req)
	if err != nil {
		t.Errorf("error: %v", err)
	}
	if out == nil || out.ChatRequest == nil {
		t.Fatal("out is nil")
	}

	messages := out.ChatRequest.Input
	// No client set → no boost → original user message preserved as-is.
	if len(messages) != 1 {
		t.Fatalf("got %d messages, want 1", len(messages))
	}
	if messages[0].Role != schemas.ChatMessageRoleUser {
		t.Errorf("msg role = %q, want user", messages[0].Role)
	}
}

func TestPreLLMHook_MultipleSystemsRemoved(t *testing.T) {
	p := newTestPlugin(t)
	ctx := newBifrostContext()
	req := &schemas.BifrostRequest{
		RequestType: schemas.ChatCompletionRequest,
		ChatRequest: &schemas.BifrostChatRequest{
			Input: []schemas.ChatMessage{
				{Role: schemas.ChatMessageRoleSystem, Content: &schemas.ChatMessageContent{ContentStr: new("system 1")}},
				{Role: schemas.ChatMessageRoleDeveloper, Content: &schemas.ChatMessageContent{ContentStr: new("developer msg")}},
				{Role: schemas.ChatMessageRoleSystem, Content: &schemas.ChatMessageContent{ContentStr: new("system 2")}},
				{Role: schemas.ChatMessageRoleUser, Content: &schemas.ChatMessageContent{ContentStr: new("hi")}},
			},
		},
	}

	out, _, err := p.PreLLMHook(ctx, req)
	if err != nil {
		t.Errorf("error: %v", err)
	}
	if out == nil || out.ChatRequest == nil {
		t.Fatal("out is nil")
	}

	messages := out.ChatRequest.Input
	// No client set → no boost → normalized: combined system + developer + user = 3 messages.
	if len(messages) != 3 {
		t.Fatalf("got %d messages, want 3 (combined system + developer + user)", len(messages))
	}

	if messages[0].Role != schemas.ChatMessageRoleSystem {
		t.Errorf("first msg role = %q, want system", messages[0].Role)
	}
	if *messages[0].Content.ContentStr != "system 1\n\nsystem 2" {
		t.Errorf("combined system = %q, want %q", *messages[0].Content.ContentStr, "system 1\n\nsystem 2")
	}
	if messages[1].Role != schemas.ChatMessageRoleDeveloper || *messages[1].Content.ContentStr != "developer msg" {
		t.Errorf("second msg: role=%q content=%q, want developer/developer msg", messages[1].Role, safeStr(messages[1].Content))
	}
	if messages[2].Role != schemas.ChatMessageRoleUser || *messages[2].Content.ContentStr != "hi" {
		t.Errorf("third message should be user hi")
	}
}

func TestPreLLMHook_EmptyMessageFiltering(t *testing.T) {
	p := newTestPlugin(t)
	ctx := newBifrostContext()
	req := &schemas.BifrostRequest{
		RequestType: schemas.ChatCompletionRequest,
		ChatRequest: &schemas.BifrostChatRequest{
			Input: []schemas.ChatMessage{
				{Content: nil},
				{Role: schemas.ChatMessageRoleUser, Content: &schemas.ChatMessageContent{ContentStr: new("hello")}},
			},
		},
	}

	out, _, err := p.PreLLMHook(ctx, req)
	if err != nil {
		t.Errorf("error: %v", err)
	}
	if out == nil || out.ChatRequest == nil {
		t.Fatal("out.ChatRequest is nil")
	}

	messages := out.ChatRequest.Input
	// Empty message filtered out, user message preserved.
	if len(messages) != 1 {
		t.Fatalf("got %d messages, want 1", len(messages))
	}
	if messages[0].Role != schemas.ChatMessageRoleUser {
		t.Errorf("msg role = %q, want user", messages[0].Role)
	}
}

// ---------------------------------------------------------------------------
// Normalization: system messages in various positions
// ---------------------------------------------------------------------------

func TestNormalizeInput_SystemsInMiddle(t *testing.T) {
	p := newTestPlugin(t)
	ctx := newBifrostContext()
	req := &schemas.BifrostRequest{
		RequestType: schemas.ChatCompletionRequest,
		ChatRequest: &schemas.BifrostChatRequest{
			Input: []schemas.ChatMessage{
				{Role: schemas.ChatMessageRoleUser, Content: &schemas.ChatMessageContent{ContentStr: new("h1")}},
				{Role: schemas.ChatMessageRoleSystem, Content: &schemas.ChatMessageContent{ContentStr: new("ctx")}},
				{Role: schemas.ChatMessageRoleAssistant, Content: &schemas.ChatMessageContent{ContentStr: new("r")}},
				{Role: schemas.ChatMessageRoleUser, Content: &schemas.ChatMessageContent{ContentStr: new("h2")}},
			},
		},
	}

	out, _, err := p.PreLLMHook(ctx, req)
	if err != nil {
		t.Fatalf("PreLLMHook: %v", err)
	}

	messages := out.ChatRequest.Input
	if len(messages) != 4 {
		t.Fatalf("got %d messages, want 4 (system + user + assistant + user)", len(messages))
	}
	// System extracted to position 0
	if messages[0].Role != schemas.ChatMessageRoleSystem || *messages[0].Content.ContentStr != "ctx" {
		t.Errorf("first msg: role=%q content=%q, want system/ctx", messages[0].Role, safeStr(messages[0].Content))
	}
	if messages[1].Role != schemas.ChatMessageRoleUser || *messages[1].Content.ContentStr != "h1" {
		t.Errorf("second msg: role=%q content=%q, want user/h1", messages[1].Role, safeStr(messages[1].Content))
	}
	if messages[2].Role != schemas.ChatMessageRoleAssistant {
		t.Errorf("third msg role = %q, want assistant", messages[2].Role)
	}
	if messages[3].Role != schemas.ChatMessageRoleUser || *messages[3].Content.ContentStr != "h2" {
		t.Errorf("fourth msg: role=%q content=%q, want user/h2", messages[3].Role, safeStr(messages[3].Content))
	}
}

func TestNormalizeInput_SystemAtEnd(t *testing.T) {
	p := newTestPlugin(t)
	ctx := newBifrostContext()
	req := &schemas.BifrostRequest{
		RequestType: schemas.ChatCompletionRequest,
		ChatRequest: &schemas.BifrostChatRequest{
			Input: []schemas.ChatMessage{
				{Role: schemas.ChatMessageRoleUser, Content: &schemas.ChatMessageContent{ContentStr: new("h1")}},
				{Role: schemas.ChatMessageRoleAssistant, Content: &schemas.ChatMessageContent{ContentStr: new("r")}},
				{Role: schemas.ChatMessageRoleSystem, Content: &schemas.ChatMessageContent{ContentStr: new("late sys")}},
			},
		},
	}

	out, _, err := p.PreLLMHook(ctx, req)
	if err != nil {
		t.Fatalf("PreLLMHook: %v", err)
	}

	messages := out.ChatRequest.Input
	// System moved to first position, last is assistant → no boost
	if len(messages) != 3 {
		t.Fatalf("got %d messages, want 3", len(messages))
	}
	if messages[0].Role != schemas.ChatMessageRoleSystem || *messages[0].Content.ContentStr != "late sys" {
		t.Errorf("first msg should be system 'late sys'")
	}
	// Last message is assistant → no boost, no developer appended
	if messages[2].Role != schemas.ChatMessageRoleAssistant {
		t.Errorf("last msg role = %q, want assistant (no boost)", messages[2].Role)
	}
}

func TestNormalizeInput_EmptySystemSkipped(t *testing.T) {
	p := newTestPlugin(t)
	ctx := newBifrostContext()
	req := &schemas.BifrostRequest{
		RequestType: schemas.ChatCompletionRequest,
		ChatRequest: &schemas.BifrostChatRequest{
			Input: []schemas.ChatMessage{
				{Role: schemas.ChatMessageRoleSystem, Content: &schemas.ChatMessageContent{ContentStr: new("")}},
				{Role: schemas.ChatMessageRoleSystem, Content: &schemas.ChatMessageContent{ContentStr: new("real")}},
				{Role: schemas.ChatMessageRoleUser, Content: &schemas.ChatMessageContent{ContentStr: new("hi")}},
			},
		},
	}

	out, _, err := p.PreLLMHook(ctx, req)
	if err != nil {
		t.Fatalf("PreLLMHook: %v", err)
	}

	messages := out.ChatRequest.Input
	if len(messages) != 2 {
		t.Fatalf("got %d messages, want 2", len(messages))
	}
	if *messages[0].Content.ContentStr != "real" {
		t.Errorf("system = %q, want %q", *messages[0].Content.ContentStr, "real")
	}
}

func TestNormalizeInput_WhonlySystemSkipped(t *testing.T) {
	p := newTestPlugin(t)
	ctx := newBifrostContext()
	req := &schemas.BifrostRequest{
		RequestType: schemas.ChatCompletionRequest,
		ChatRequest: &schemas.BifrostChatRequest{
			Input: []schemas.ChatMessage{
				{Role: schemas.ChatMessageRoleSystem, Content: &schemas.ChatMessageContent{ContentStr: new("   \t\n")}},
				{Role: schemas.ChatMessageRoleUser, Content: &schemas.ChatMessageContent{ContentStr: new("hi")}},
			},
		},
	}

	out, _, err := p.PreLLMHook(ctx, req)
	if err != nil {
		t.Fatalf("PreLLMHook: %v", err)
	}

	messages := out.ChatRequest.Input
	// Whitespace-only system message should be skipped, no system message in output
	if len(messages) != 1 {
		t.Fatalf("got %d messages, want 1", len(messages))
	}
	if messages[0].Role != schemas.ChatMessageRoleUser {
		t.Errorf("msg role = %q, want user", messages[0].Role)
	}
}

// ---------------------------------------------------------------------------
// Multimodal messages
// ---------------------------------------------------------------------------

func TestPreLLMHook_MultimodalOnly_NoClient(t *testing.T) {
	p := newTestPlugin(t)
	ctx := newBifrostContext()
	req := &schemas.BifrostRequest{
		RequestType: schemas.ChatCompletionRequest,
		ChatRequest: &schemas.BifrostChatRequest{
			Input: []schemas.ChatMessage{
				{Role: schemas.ChatMessageRoleUser, Content: &schemas.ChatMessageContent{
					ContentBlocks: []schemas.ChatContentBlock{
						{Type: schemas.ChatContentBlockTypeImage, ImageURLStruct: &schemas.ChatInputImage{URL: "https://example.com/img.png"}},
					},
				}},
			},
		},
	}

	out, _, err := p.PreLLMHook(ctx, req)
	if err != nil {
		t.Fatalf("PreLLMHook: %v", err)
	}

	messages := out.ChatRequest.Input
	// No client → no boost. Message preserved as-is (multimodal is not empty).
	if len(messages) != 1 {
		t.Fatalf("got %d messages, want 1", len(messages))
	}
	if messages[0].Role != schemas.ChatMessageRoleUser {
		t.Errorf("msg role = %q, want user", messages[0].Role)
	}
}

func TestPreLLMHook_MultimodalWithText_BoostsDeveloperMessage(t *testing.T) {
	p := newTestPlugin(t)

	improved := "describe this image in detail"
	mockResp := &schemas.BifrostChatResponse{
		Choices: []schemas.BifrostResponseChoice{{
			ChatNonStreamResponseChoice: &schemas.ChatNonStreamResponseChoice{
				Message: &schemas.ChatMessage{
					Role:    schemas.ChatMessageRoleAssistant,
					Content: &schemas.ChatMessageContent{ContentStr: &improved},
				},
			},
		}},
	}
	setMockClient(t, p, &mockChatClient{resp: mockResp})

	ctx := newBifrostContext()
	req := &schemas.BifrostRequest{
		RequestType: schemas.ChatCompletionRequest,
		ChatRequest: &schemas.BifrostChatRequest{
			Input: []schemas.ChatMessage{
				{Role: schemas.ChatMessageRoleUser, Content: &schemas.ChatMessageContent{
					ContentBlocks: []schemas.ChatContentBlock{
						{Type: schemas.ChatContentBlockTypeImage, ImageURLStruct: &schemas.ChatInputImage{URL: "https://example.com/img.png"}},
					},
				}},
			},
		},
	}

	out, _, err := p.PreLLMHook(ctx, req)
	if err != nil {
		t.Fatalf("PreLLMHook: %v", err)
	}

	messages := out.ChatRequest.Input
	// user (original) + developer (improved) = 2
	if len(messages) != 2 {
		t.Fatalf("got %d messages, want 2", len(messages))
	}
	if messages[1].Role != schemas.ChatMessageRoleDeveloper {
		t.Errorf("second msg role = %q, want developer", messages[1].Role)
	}
	if *messages[1].Content.ContentStr != improved {
		t.Errorf("improved = %q, want %q", *messages[1].Content.ContentStr, improved)
	}
}

// ---------------------------------------------------------------------------
// Reasoning fallback
// ---------------------------------------------------------------------------

func TestBoostMessage_ReasoningFallback(t *testing.T) {
	p := newTestPlugin(t)

	// Model returns only reasoning (thinking), no text content
	reasoningText := "Let me think about how to improve this prompt..."
	mockResp := &schemas.BifrostChatResponse{
		Choices: []schemas.BifrostResponseChoice{{
			ChatNonStreamResponseChoice: &schemas.ChatNonStreamResponseChoice{
				Message: &schemas.ChatMessage{
					Role: schemas.ChatMessageRoleAssistant,
					Content: &schemas.ChatMessageContent{
						ContentStr: nil,
					},
					ChatAssistantMessage: &schemas.ChatAssistantMessage{
						Reasoning: &reasoningText,
					},
				},
			},
		}},
	}
	setMockClient(t, p, &mockChatClient{resp: mockResp})

	ctx := newBifrostContext()
	req := &schemas.BifrostRequest{
		RequestType: schemas.ChatCompletionRequest,
		ChatRequest: &schemas.BifrostChatRequest{
			Input: []schemas.ChatMessage{
				{Role: schemas.ChatMessageRoleUser, Content: &schemas.ChatMessageContent{ContentStr: new("help me")}},
			},
		},
	}

	out, _, err := p.PreLLMHook(ctx, req)
	if err != nil {
		t.Fatalf("PreLLMHook: %v", err)
	}

	messages := out.ChatRequest.Input
	if len(messages) != 2 {
		t.Fatalf("got %d messages, want 2", len(messages))
	}
	if messages[1].Role != schemas.ChatMessageRoleDeveloper {
		t.Errorf("second msg role = %q, want developer", messages[1].Role)
	}
	if *messages[1].Content.ContentStr != reasoningText {
		t.Errorf("improved = %q, want reasoning text %q", *messages[1].Content.ContentStr, reasoningText)
	}
}

func TestBoostMessage_TextPreferredOverReasoning(t *testing.T) {
	p := newTestPlugin(t)

	textContent := "refined prompt"
	reasoningText := "some reasoning"
	mockResp := &schemas.BifrostChatResponse{
		Choices: []schemas.BifrostResponseChoice{{
			ChatNonStreamResponseChoice: &schemas.ChatNonStreamResponseChoice{
				Message: &schemas.ChatMessage{
					Role:    schemas.ChatMessageRoleAssistant,
					Content: &schemas.ChatMessageContent{ContentStr: &textContent},
					ChatAssistantMessage: &schemas.ChatAssistantMessage{
						Reasoning: &reasoningText,
					},
				},
			},
		}},
	}
	setMockClient(t, p, &mockChatClient{resp: mockResp})

	ctx := newBifrostContext()
	req := &schemas.BifrostRequest{
		RequestType: schemas.ChatCompletionRequest,
		ChatRequest: &schemas.BifrostChatRequest{
			Input: []schemas.ChatMessage{
				{Role: schemas.ChatMessageRoleUser, Content: &schemas.ChatMessageContent{ContentStr: new("help me")}},
			},
		},
	}

	out, _, err := p.PreLLMHook(ctx, req)
	if err != nil {
		t.Fatalf("PreLLMHook: %v", err)
	}

	messages := out.ChatRequest.Input
	if len(messages) != 2 {
		t.Fatalf("got %d messages, want 2", len(messages))
	}
	// Text should be preferred over reasoning
	if *messages[1].Content.ContentStr != textContent {
		t.Errorf("improved = %q, want text %q (not reasoning)", *messages[1].Content.ContentStr, textContent)
	}
}

func TestBoostMessage_EmptyReasoningAndContent(t *testing.T) {
	p := newTestPlugin(t)

	// Both content and reasoning are empty
	mockResp := &schemas.BifrostChatResponse{
		Choices: []schemas.BifrostResponseChoice{{
			ChatNonStreamResponseChoice: &schemas.ChatNonStreamResponseChoice{
				Message: &schemas.ChatMessage{
					Role:    schemas.ChatMessageRoleAssistant,
					Content: &schemas.ChatMessageContent{ContentStr: new("")},
				},
			},
		}},
	}
	setMockClient(t, p, &mockChatClient{resp: mockResp})

	ctx := newBifrostContext()
	req := &schemas.BifrostRequest{
		RequestType: schemas.ChatCompletionRequest,
		ChatRequest: &schemas.BifrostChatRequest{
			Input: []schemas.ChatMessage{
				{Role: schemas.ChatMessageRoleUser, Content: &schemas.ChatMessageContent{ContentStr: new("help me")}},
			},
		},
	}

	out, _, err := p.PreLLMHook(ctx, req)
	if err != nil {
		t.Fatalf("PreLLMHook: %v", err)
	}

	messages := out.ChatRequest.Input
	// No developer message appended when boost returns empty
	if len(messages) != 1 {
		t.Fatalf("got %d messages, want 1 (no developer appended)", len(messages))
	}
}

// ---------------------------------------------------------------------------
// PreLLMHook — boostMessage success path (client set)
// ---------------------------------------------------------------------------

// mockChatClient satisfies the chatCompleter interface used by Plugin.
type mockChatClient struct {
	resp *schemas.BifrostChatResponse
	err  *schemas.BifrostError
}

func (m *mockChatClient) ChatCompletionRequest(
	ctx *schemas.BifrostContext, req *schemas.BifrostChatRequest,
) (*schemas.BifrostChatResponse, *schemas.BifrostError) {
	return m.resp, m.err
}

// mockChatClientCapture captures the last request sent to ChatCompletionRequest.
type mockChatClientCapture struct {
	resp   *schemas.BifrostChatResponse
	err    *schemas.BifrostError
	called bool
	last   *schemas.BifrostChatRequest
}

func (m *mockChatClientCapture) ChatCompletionRequest(
	ctx *schemas.BifrostContext, req *schemas.BifrostChatRequest,
) (*schemas.BifrostChatResponse, *schemas.BifrostError) {
	m.called = true
	m.last = req
	return m.resp, m.err
}

// setMockClient injects a mock client into the Plugin via SetBifrostClient.
func setMockClient(t *testing.T, p *Plugin, mock chatCompleter) {
	t.Helper()
	p.SetBifrostClient(mock)
}

func TestPreLLMHook_BoostSucceeds_AppendsDeveloperMessage(t *testing.T) {
	p := newTestPlugin(t)

	// Build a mock response with an improved message
	improvedContent := "refined user message"
	mockResp := &schemas.BifrostChatResponse{
		Choices: []schemas.BifrostResponseChoice{{
			ChatNonStreamResponseChoice: &schemas.ChatNonStreamResponseChoice{
				Message: &schemas.ChatMessage{
					Role:    schemas.ChatMessageRoleAssistant,
					Content: &schemas.ChatMessageContent{ContentStr: &improvedContent},
				},
			},
		}},
	}

	mockClient := &mockChatClient{resp: mockResp}
	setMockClient(t, p, mockClient)

	ctx := newBifrostContext()
	req := &schemas.BifrostRequest{
		RequestType: schemas.ChatCompletionRequest,
		ChatRequest: &schemas.BifrostChatRequest{
			Input: []schemas.ChatMessage{
				{Role: schemas.ChatMessageRoleSystem, Content: &schemas.ChatMessageContent{ContentStr: new("old system")}},
				{Role: schemas.ChatMessageRoleUser, Content: &schemas.ChatMessageContent{ContentStr: new("original user message")}},
			},
		},
	}

	out, _, err := p.PreLLMHook(ctx, req)
	if err != nil {
		t.Fatalf("PreLLMHook: %v", err)
	}

	messages := out.ChatRequest.Input
	// With boost succeeding: 1 system + 1 user (original) + 1 developer (improved) = 3 messages.
	if len(messages) != 3 {
		t.Fatalf("got %d messages, want 3 (system + user + improved developer)", len(messages))
	}

	if messages[0].Role != schemas.ChatMessageRoleSystem {
		t.Errorf("first msg role = %q, want system", messages[0].Role)
	}

	// The original user message should still be present.
	if messages[1].Role != schemas.ChatMessageRoleUser {
		t.Fatalf("second msg role = %q, want user (original preserved)", messages[1].Role)
	}
	if messages[1].Content.ContentStr == nil || *messages[1].Content.ContentStr != "original user message" {
		t.Error("original user message should be preserved")
	}

	// The last message should be a developer message with the improved content.
	if messages[2].Role != schemas.ChatMessageRoleDeveloper {
		t.Fatalf("third msg role = %q, want developer", messages[2].Role)
	}
	if messages[2].Content.ContentStr == nil || *messages[2].Content.ContentStr != improvedContent {
		t.Errorf("third msg content = %q, want %q",
			func() string {
				if messages[2].Content.ContentStr != nil {
					return *messages[2].Content.ContentStr
				}
				return ""
			}(),
			improvedContent)
	}
}

func TestPreLLMHook_BoostFails_UserMessagePreserved(t *testing.T) {
	p := newTestPlugin(t)

	// Mock client that returns an error (simulating boost failure).
	mockClient := &mockChatClient{err: &schemas.BifrostError{IsBifrostError: true}}
	setMockClient(t, p, mockClient)

	ctx := newBifrostContext()
	req := &schemas.BifrostRequest{
		RequestType: schemas.ChatCompletionRequest,
		ChatRequest: &schemas.BifrostChatRequest{
			Input: []schemas.ChatMessage{
				{Role: schemas.ChatMessageRoleSystem, Content: &schemas.ChatMessageContent{ContentStr: new("old system")}},
				{Role: schemas.ChatMessageRoleUser, Content: &schemas.ChatMessageContent{ContentStr: new("original user message")}},
			},
		},
	}

	out, _, err := p.PreLLMHook(ctx, req)
	if err != nil {
		t.Fatalf("PreLLMHook: %v", err)
	}

	messages := out.ChatRequest.Input
	// Normalized: system + user = 2, boost failed → no developer appended
	if len(messages) != 2 {
		t.Fatalf("got %d messages, want 2", len(messages))
	}

	// When boost fails, the original user message should pass through unchanged.
	if messages[1].Role != schemas.ChatMessageRoleUser {
		t.Errorf("second msg role = %q, want user (unchanged on boost failure)", messages[1].Role)
	}
	if messages[1].Content.ContentStr == nil || *messages[1].Content.ContentStr != "original user message" {
		t.Error("user message should be unchanged when boost fails")
	}
}

// ---------------------------------------------------------------------------
// PreLLMHook — boostMessage filters user system messages from sub-request
// ---------------------------------------------------------------------------

func TestBoostMessage_ExcludesUserSystemFromSubRequest(t *testing.T) {
	p := newTestPlugin(t)

	improved := "better prompt"
	mock := &mockChatClientCapture{
		resp: &schemas.BifrostChatResponse{
			Choices: []schemas.BifrostResponseChoice{{
				ChatNonStreamResponseChoice: &schemas.ChatNonStreamResponseChoice{
					Message: &schemas.ChatMessage{
						Role:    schemas.ChatMessageRoleAssistant,
						Content: &schemas.ChatMessageContent{ContentStr: &improved},
					},
				},
			}},
		},
	}
	p.SetBifrostClient(mock)

	ctx := newBifrostContext()
	req := &schemas.BifrostRequest{
		RequestType: schemas.ChatCompletionRequest,
		ChatRequest: &schemas.BifrostChatRequest{
			Input: []schemas.ChatMessage{
				{Role: schemas.ChatMessageRoleSystem, Content: &schemas.ChatMessageContent{ContentStr: new("client system prompt")}},
				{Role: schemas.ChatMessageRoleUser, Content: &schemas.ChatMessageContent{ContentStr: new("hello")}},
			},
		},
	}

	out, _, err := p.PreLLMHook(ctx, req)
	if err != nil {
		t.Fatalf("PreLLMHook: %v", err)
	}

	// Final output should still have system + user + developer = 3
	messages := out.ChatRequest.Input
	if len(messages) != 3 {
		t.Fatalf("output: got %d messages, want 3", len(messages))
	}

	// Sub-request should NOT contain the user's system message.
	if !mock.called {
		t.Fatal("mock was not called")
	}
	sub := mock.last
	for _, msg := range sub.Input {
		if msg.Role == schemas.ChatMessageRoleSystem && msg.Content.ContentStr != nil && *msg.Content.ContentStr == "client system prompt" {
			t.Error("sub-request should not contain user's system message")
		}
	}

	// Sub-request should have: plugin system + user + developer = 3
	if len(sub.Input) != 3 {
		roles := make([]string, len(sub.Input))
		for i, m := range sub.Input {
			roles[i] = string(m.Role)
		}
		t.Fatalf("sub-request: got %d messages (%v), want 3 (system+user+developer)", len(sub.Input), roles)
	}
	if sub.Input[0].Role != schemas.ChatMessageRoleSystem {
		t.Errorf("sub-request first msg role = %q, want system", sub.Input[0].Role)
	}
	if sub.Input[1].Role != schemas.ChatMessageRoleUser {
		t.Errorf("sub-request second msg role = %q, want user", sub.Input[1].Role)
	}
	if sub.Input[2].Role != schemas.ChatMessageRoleDeveloper {
		t.Errorf("sub-request third msg role = %q, want developer", sub.Input[2].Role)
	}
}

func TestBoostMessage_MultipleSystemsFiltered(t *testing.T) {
	p := newTestPlugin(t)

	improved := "better"
	mock := &mockChatClientCapture{
		resp: &schemas.BifrostChatResponse{
			Choices: []schemas.BifrostResponseChoice{{
				ChatNonStreamResponseChoice: &schemas.ChatNonStreamResponseChoice{
					Message: &schemas.ChatMessage{
						Role:    schemas.ChatMessageRoleAssistant,
						Content: &schemas.ChatMessageContent{ContentStr: &improved},
					},
				},
			}},
		},
	}
	p.SetBifrostClient(mock)

	ctx := newBifrostContext()
	req := &schemas.BifrostRequest{
		RequestType: schemas.ChatCompletionRequest,
		ChatRequest: &schemas.BifrostChatRequest{
			Input: []schemas.ChatMessage{
				{Role: schemas.ChatMessageRoleSystem, Content: &schemas.ChatMessageContent{ContentStr: new("sys1")}},
				{Role: schemas.ChatMessageRoleUser, Content: &schemas.ChatMessageContent{ContentStr: new("h1")}},
				{Role: schemas.ChatMessageRoleAssistant, Content: &schemas.ChatMessageContent{ContentStr: new("a1")}},
				{Role: schemas.ChatMessageRoleSystem, Content: &schemas.ChatMessageContent{ContentStr: new("sys2")}},
				{Role: schemas.ChatMessageRoleUser, Content: &schemas.ChatMessageContent{ContentStr: new("h2")}},
			},
		},
	}

	_, _, err := p.PreLLMHook(ctx, req)
	if err != nil {
		t.Fatalf("PreLLMHook: %v", err)
	}

	// Sub-request should contain only: plugin sys + user + assistant + user + developer = 5
	sub := mock.last
	for _, msg := range sub.Input {
		if msg.Role == schemas.ChatMessageRoleSystem && msg.Content.ContentStr != nil {
			txt := *msg.Content.ContentStr
			if txt == "sys1" || txt == "sys2" {
				t.Errorf("sub-request should not contain user system %q", txt)
			}
		}
	}
	if len(sub.Input) != 5 {
		roles := make([]string, len(sub.Input))
		for i, m := range sub.Input {
			roles[i] = string(m.Role)
		}
		t.Fatalf("sub-request: got %d messages (%v), want 5", len(sub.Input), roles)
	}
}

// ---------------------------------------------------------------------------
// PreLLMHook — recursion guard (recursive sub-request from boost)
// ---------------------------------------------------------------------------

func TestPreLLMHook_RecursiveSubRequest_ReturnsUnchanged(t *testing.T) {
	p := newTestPlugin(t)

	// Simulate a recursive call: the context has the recursion guard flag set,
	// which is what boostMessage sets before making a sub-request.
	ctx := newBifrostContext()
	ctx.SetValue(llmbosterRecursionGuard{}, true)

	req := &schemas.BifrostRequest{
		RequestType: schemas.ChatCompletionRequest,
		ChatRequest: &schemas.BifrostChatRequest{
			Input: []schemas.ChatMessage{
				{Role: schemas.ChatMessageRoleSystem, Content: &schemas.ChatMessageContent{ContentStr: new("old system")}},
				{Role: schemas.ChatMessageRoleUser, Content: &schemas.ChatMessageContent{ContentStr: new("user message")}},
			},
		},
	}

	out, _, err := p.PreLLMHook(ctx, req)
	if err != nil {
		t.Fatalf("PreLLMHook: %v", err)
	}

	// When recursion guard is set, return the request completely unchanged (no normalization).
	if out != req {
		t.Fatal("recursive call should return the same req pointer")
	}

	messages := out.ChatRequest.Input
	// Should be exactly the original input — no normalization, no boosting.
	if len(messages) != 2 {
		t.Fatalf("got %d messages, want 2 (original unchanged)", len(messages))
	}

	if messages[0].Role != schemas.ChatMessageRoleSystem || *messages[0].Content.ContentStr != "old system" {
		t.Error("first message should be the original system message")
	}
	if messages[1].Role != schemas.ChatMessageRoleUser || *messages[1].Content.ContentStr != "user message" {
		t.Error("second message should be the original user message")
	}
}

// ---------------------------------------------------------------------------
// Interface compliance
// ---------------------------------------------------------------------------

func TestInterfaceCompliance(t *testing.T) {
	var _ schemas.LLMPlugin = (*Plugin)(nil)
	var _ schemas.HTTPTransportPlugin = (*Plugin)(nil)
	var _ schemas.BasePlugin = (*Plugin)(nil)
}

// ---------------------------------------------------------------------------
// HTTPTransport hooks — passthrough
// ---------------------------------------------------------------------------

func TestHTTPTransportPreHook_Passthrough(t *testing.T) {
	p := newTestPlugin(t)
	ctx := newBifrostContext()
	resp, err := p.HTTPTransportPreHook(ctx, nil)
	if err != nil {
		t.Errorf("error: %v", err)
	}
	if resp != nil {
		t.Error("expected nil response")
	}
}

func TestHTTPTransportPostHook_Passthrough(t *testing.T) {
	p := newTestPlugin(t)
	ctx := newBifrostContext()
	if err := p.HTTPTransportPostHook(ctx, nil, nil); err != nil {
		t.Errorf("error: %v", err)
	}
}

func TestHTTPTransportStreamChunkHook_Passthrough(t *testing.T) {
	p := newTestPlugin(t)
	ctx := newBifrostContext()
	chunk := &schemas.BifrostStreamChunk{}
	out, err := p.HTTPTransportStreamChunkHook(ctx, nil, chunk)
	if err != nil {
		t.Errorf("error: %v", err)
	}
	if out != chunk {
		t.Error("expected same chunk back")
	}
}

// ---------------------------------------------------------------------------
// HTTPTransportStreamChunkHook — loop detection
// ---------------------------------------------------------------------------

func TestHTTPTransportStreamChunkHook_LoopDetected_ReturnsRetryWith(t *testing.T) {
	// Явно включаем loop detection — newTestPlugin использует Config{} без LoopDetection,
	// что даёт DefaultLoopDetectorConfig (Enabled: false).
	p, err := Init(&Config{
		LoopDetection: &LoopDetectorConfig{Enabled: true},
	}, noopLogger{})
	if err != nil {
		t.Fatalf("Init: %v", err)
	}
	t.Cleanup(func() { _ = p.Cleanup() })

	ctx := newBifrostContext()
	ctx.SetValue(schemas.BifrostContextKeyRequestID, "test-req-1")

	// Default config: MinTokensBefore=24, MaxNGramRepeats=5, NgramSizes=[3,5]
	// With tail excluded, need 5 previous n-gram occurrences = 29 total identical tokens.
	for i := 0; i < 40; i++ {
		chunk := &schemas.BifrostStreamChunk{
			BifrostChatResponse: &schemas.BifrostChatResponse{
				Choices: []schemas.BifrostResponseChoice{{
					ChatStreamResponseChoice: &schemas.ChatStreamResponseChoice{
						Delta: &schemas.ChatStreamResponseChoiceDelta{
							Content: ptr("hello"),
						},
					},
				}},
			},
		}
		out, err := p.HTTPTransportStreamChunkHook(ctx, nil, chunk)
		if err == nil {
			if out != chunk {
				t.Error("expected same chunk back")
			}
			continue
		}
		// First error should be a StreamInterceptionError with RetryWith
		siErr, ok := err.(*schemas.StreamInterceptionError)
		if !ok {
			t.Fatalf("chunk %d: expected StreamInterceptionError, got %T", i, err)
		}
		if siErr.RetryWith == nil {
			t.Fatalf("chunk %d: expected RetryWith to be set", i)
		}
		if len(siErr.RetryWith.ExtraMessages) == 0 {
			t.Fatalf("chunk %d: expected ExtraMessages to be non-empty", i)
		}
		// With the fix, ResetStream clears the triggered state for the requestID.
		// This means in the real flow inference.go can start a clean retry stream
		// with the same requestID after draining the old one.
		return
	}
	t.Fatal("expected loop detection to trigger within 40 chunks")
}

func TestHTTPTransportStreamChunkHook_NoLoop_Passthrough(t *testing.T) {
	p := newTestPlugin(t)
	ctx := newBifrostContext()
	ctx.SetValue(schemas.BifrostContextKeyRequestID, "test-req-2")

	words := []string{"The ", "quick ", "brown ", "fox ", "jumps "}
	for _, word := range words {
		chunk := &schemas.BifrostStreamChunk{
			BifrostChatResponse: &schemas.BifrostChatResponse{
				Choices: []schemas.BifrostResponseChoice{{
					ChatStreamResponseChoice: &schemas.ChatStreamResponseChoice{
						Delta: &schemas.ChatStreamResponseChoiceDelta{
							Content: ptr(word),
						},
					},
				}},
			},
		}
		out, err := p.HTTPTransportStreamChunkHook(ctx, nil, chunk)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if out != chunk {
			t.Error("expected same chunk back")
		}
	}
}

func TestHTTPTransportStreamChunkHook_Disabled(t *testing.T) {
	cfg := &Config{
		LoopDetection: &LoopDetectorConfig{Enabled: false},
	}
	p2, _ := Init(cfg, noopLogger{})

	ctx := newBifrostContext()
	ctx.SetValue(schemas.BifrostContextKeyRequestID, "test-req-3")

	for i := 0; i < 20; i++ {
		chunk := &schemas.BifrostStreamChunk{
			BifrostChatResponse: &schemas.BifrostChatResponse{
				Choices: []schemas.BifrostResponseChoice{{
					ChatStreamResponseChoice: &schemas.ChatStreamResponseChoice{
						Delta: &schemas.ChatStreamResponseChoiceDelta{
							Content: ptr("loop "),
						},
					},
				}},
			},
		}
		out, err := p2.HTTPTransportStreamChunkHook(ctx, nil, chunk)
		if err != nil {
			t.Fatalf("unexpected error when disabled: %v", err)
		}
		if out != chunk {
			t.Error("expected same chunk back when disabled")
		}
	}
}

func TestHTTPTransportStreamChunkHook_NoRequestID_Passthrough(t *testing.T) {
	p := newTestPlugin(t)
	ctx := newBifrostContext()

	chunk := &schemas.BifrostStreamChunk{
		BifrostChatResponse: &schemas.BifrostChatResponse{
			Choices: []schemas.BifrostResponseChoice{{
				ChatStreamResponseChoice: &schemas.ChatStreamResponseChoice{
					Delta: &schemas.ChatStreamResponseChoiceDelta{
						Content: ptr("hello "),
					},
				},
			}},
		},
	}
	out, err := p.HTTPTransportStreamChunkHook(ctx, nil, chunk)
	if err != nil {
		t.Errorf("unexpected error: %v", err)
	}
	if out != chunk {
		t.Error("expected same chunk back")
	}
}

func TestHTTPTransportStreamChunkHook_ToolCallLoop(t *testing.T) {
	// Явно включаем loop detection
	p, err := Init(&Config{
		LoopDetection: &LoopDetectorConfig{
			Enabled:            true,
			MaxToolCallRepeats: 5,
		},
	}, noopLogger{})
	if err != nil {
		t.Fatalf("Init: %v", err)
	}
	t.Cleanup(func() { _ = p.Cleanup() })

	ctx := newBifrostContext()
	ctx.SetValue(schemas.BifrostContextKeyRequestID, "test-req-tc")

	for i := 0; i < 5; i++ {
		chunk := &schemas.BifrostStreamChunk{
			BifrostChatResponse: &schemas.BifrostChatResponse{
				Choices: []schemas.BifrostResponseChoice{{
					ChatStreamResponseChoice: &schemas.ChatStreamResponseChoice{
						Delta: &schemas.ChatStreamResponseChoiceDelta{
							ToolCalls: []schemas.ChatAssistantMessageToolCall{{
								Function: schemas.ChatAssistantMessageToolCallFunction{
									Name: ptr("search"),
								},
							}},
						},
					},
				}},
			},
		}
		out, err := p.HTTPTransportStreamChunkHook(ctx, nil, chunk)
		if i < 4 {
			if err != nil {
				t.Fatalf("unexpected error at call %d: %v", i, err)
			}
			if out != chunk {
				t.Error("expected same chunk back")
			}
		} else {
			if err == nil {
				t.Fatal("expected error on tool call loop")
			}
			siErr, ok := err.(*schemas.StreamInterceptionError)
			if !ok {
				t.Fatalf("expected StreamInterceptionError, got %T", err)
			}
			if siErr.RetryWith == nil {
				t.Fatal("expected RetryWith")
			}
		}
	}
}
