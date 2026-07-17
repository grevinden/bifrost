// Package llmboster implements a Bifrost plugin that enhances user input
// by appending an improved version of the last user message.
//
// The plugin operates in two phases:
//   - Phase 1 (normalization): combines all system messages into one
//     system message at the beginning and removes empty messages.
//   - Phase 2 (job): if the last message is from the user, sends a sub-request
//     with an improvement prompt and appends the result as a developer message.
//
// Original user messages are always preserved (Option A).
package llmboster

import (
	_ "embed"
	"strings"

	"github.com/grevinden/bifrost/core/schemas"
)

//go:embed prompts/system.md
var embeddedSystemPrompt string

//go:embed prompts/developer.md
var embeddedDeveloperPrompt string

const (
	PluginName = "llmboster"
)

// llmbosterRecursionGuard is a context key used to detect recursive sub-requests.
// When boostMessage sends a sub-request through Bifrost, the pipeline wraps the
// context in a plugin scope — but that happens for every plugin invocation, not
// just recursion. This custom key is set only by boostMessage before its sub-request,
// so it correctly distinguishes normal calls from recursive ones.
type llmbosterRecursionGuard struct{}

// ptr returns a pointer to the given value.
//
//go:fix inline
func ptr[T any](v T) *T { return new(v) }

type Config struct {
	MaxCompletionTokens *int     `json:"max_completion_tokens,omitempty"`
	Temperature         *float64 `json:"temperature,omitempty"`
	FrequencyPenalty    *float64 `json:"frequency_penalty,omitempty"`
	PresencePenalty     *float64 `json:"presence_penalty,omitempty"`
	ReasoningEffort     *string  `json:"reasoning_effort,omitempty"`
}

// chatCompleter is the minimal interface for making chat completion requests.
// It allows mocking in tests and decouples the plugin from the concrete Bifrost type.
type chatCompleter interface {
	ChatCompletionRequest(ctx *schemas.BifrostContext, req *schemas.BifrostChatRequest) (*schemas.BifrostChatResponse, *schemas.BifrostError)
}

type Plugin struct {
	logger     schemas.Logger
	client     chatCompleter
	cfg        *Config
	params     *schemas.ChatParameters
	sysContent *schemas.ChatMessageContent
	devContent *schemas.ChatMessageContent
}

var (
	_ schemas.LLMPlugin           = (*Plugin)(nil)
	_ schemas.HTTPTransportPlugin = (*Plugin)(nil)
	_ schemas.ClientAwarePlugin   = (*Plugin)(nil)
)

func Init(config *Config, logger schemas.Logger) (*Plugin, error) {
	if config == nil {
		config = &Config{}
	}
	cfg := applyDefaults(config)
	params := &schemas.ChatParameters{
		MaxCompletionTokens: cfg.MaxCompletionTokens,
		Temperature:         cfg.Temperature,
		FrequencyPenalty:    cfg.FrequencyPenalty,
		PresencePenalty:     cfg.PresencePenalty,
		Reasoning:           &schemas.ChatReasoning{Effort: cfg.ReasoningEffort},
	}
	return &Plugin{
		logger:     logger,
		cfg:        cfg,
		params:     params,
		sysContent: &schemas.ChatMessageContent{ContentStr: new(strings.TrimSpace(embeddedSystemPrompt))},
		devContent: &schemas.ChatMessageContent{ContentStr: new(embeddedDeveloperPrompt)},
	}, nil
}

func applyDefaults(c *Config) *Config {
	if c.MaxCompletionTokens == nil {
		c.MaxCompletionTokens = new(1024)
	}
	if c.Temperature == nil {
		c.Temperature = new(0.3)
	}
	if c.FrequencyPenalty == nil {
		c.FrequencyPenalty = new(0.5)
	}
	if c.PresencePenalty == nil {
		c.PresencePenalty = new(0.3)
	}
	if c.ReasoningEffort == nil {
		c.ReasoningEffort = new("none")
	}
	return c
}

func (p *Plugin) SetBifrostClient(client any) {
	if c, ok := client.(chatCompleter); ok {
		p.client = c
	}
}

func (p *Plugin) GetName() string {
	return PluginName
}

func (p *Plugin) Cleanup() error {
	return nil
}

// PreRequestHook вызывается один раз на уровне всего запроса, до того как он попадёт к провайдеру.
// В этом плагине ничего не делаем — маршрутизацию и выбор провайдера не меняем.
func (p *Plugin) PreRequestHook(ctx *schemas.BifrostContext, req *schemas.BifrostRequest) error {
	return nil
}

// PostLLMHook вызывается после того, как провайдер вернул ответ (или ошибку).
// В этом плагине ничего не меняем — просто возвращаем ответ и ошибку без изменений.
func (p *Plugin) PostLLMHook(ctx *schemas.BifrostContext, resp *schemas.BifrostResponse, bifrostErr *schemas.BifrostError) (*schemas.BifrostResponse, *schemas.BifrostError, error) {
	return resp, bifrostErr, nil
}

// PreLLMHook вызывается непосредственно перед отправкой запроса к провайдеру (например, OpenAI или Anthropic).
// Этот метод запускается ПРИ КАЖДОЙ попытке выполнения запроса — и при первом вызове,
// и при каждой последующей попытке (fallback), если предыдущий провайдер не справился.
//
// Параметры:
//   - req: Текущий запрос. Вы можете вернуть измененный объект req, чтобы обновить параметры запроса для этого вызова.
//   - shortCircuit: Если вернуть не-nil значение здесь, Bifrost пропустит реальный сетевой вызов к провайдеру
//     и сразу перейдет к обработке результата (например, если вы нашли ответ в кеше).
//   - error: Ошибки здесь не прерывают выполнение запроса. Они просто логируются как предупреждения,
//     чтобы система могла продолжить работу с другими плагинами или провайдерами.
func (p *Plugin) PreLLMHook(ctx *schemas.BifrostContext, req *schemas.BifrostRequest) (*schemas.BifrostRequest, *schemas.LLMPluginShortCircuit, error) {

	// Защита от бесконечной рекурсии:
	// Если boostMessage уже установил флаг рекурсии в контексте (подзапрос проходит
	// через тот же пайплайн), возвращаем запрос без изменений.
	if _, ok := ctx.Value(llmbosterRecursionGuard{}).(bool); ok {
		return req, nil, nil
	}

	if req == nil || req.ChatRequest == nil {
		return req, nil, nil
	}

	if len(req.ChatRequest.Input) == 0 {
		return req, nil, nil
	}

	// === Phase 1: Нормализация ===
	// Приводим input к каноническому виду: объединяем system в одно сообщение,
	// удаляем полностью пустые сообщения.
	inputLen := len(req.ChatRequest.Input)
	req.ChatRequest.Input = normalizeInput(req.ChatRequest.Input)
	if len(req.ChatRequest.Input) != inputLen {
		p.logger.Debug("llmboster: normalized input, before=%d after=%d", inputLen, len(req.ChatRequest.Input))
	}

	if len(req.ChatRequest.Input) == 0 {
		return req, nil, nil
	}

	// === Phase 2: Job ===
	// Работаем с нормализованным контекстом.
	lastMsg := req.ChatRequest.Input[len(req.ChatRequest.Input)-1]
	if lastMsg.Role != schemas.ChatMessageRoleUser {
		return req, nil, nil
	}

	improved := p.boostMessage(ctx, req.ChatRequest.Provider, req.ChatRequest.Model, req.ChatRequest.Input)
	if improved != "" {
		req.ChatRequest.Input = append(req.ChatRequest.Input, schemas.ChatMessage{
			Role:    schemas.ChatMessageRoleDeveloper,
			Content: &schemas.ChatMessageContent{ContentStr: new(improved)},
		})
		p.logger.Info("llmboster: boost succeeded, model=%s provider=%s improved_len=%d", req.ChatRequest.Model, req.ChatRequest.Provider, len(improved))
	} else {
		p.logger.Debug("llmboster: boost returned empty, model=%s provider=%s", req.ChatRequest.Model, req.ChatRequest.Provider)
	}

	return req, nil, nil
}

// boostMessage отправляет подзапрос к той же модели LLM, чтобы «улучшить» контекст чата.
// На вход подаётся нормализованная история сообщений, на выходе — сгенерированный текст,
// который потом добавим в конец истории как сообщение от разработчика.
//
// Fallback для извлечения текста ответа:
//  1. Content.ContentStr (обычный текст)
//  2. ChatAssistantMessage.Reasoning (thinking/reasoning модели: DeepSeek, OpenAI o-серия, xAI)
//  3. Если оба пусты — возвращаем ""
func (p *Plugin) boostMessage(
	ctx *schemas.BifrostContext, provider schemas.ModelProvider,
	model string, messages []schemas.ChatMessage) string {
	if p.client == nil {
		return ""
	}

	// Устанавливаем флаг рекурсии — если этот же плагин встретится в пайплайне
	// для подзапроса, он пропустит обработку и не зациклится.
	ctx.SetValue(llmbosterRecursionGuard{}, true)

	// Формируем подзапрос для «улучшающего» вызова модели.
	subRequest := make([]schemas.ChatMessage, 0, len(messages)+2)

	// 1. Системное сообщение — копируем указатель, чтобы не разделять мутабельное состояние.
	sysCopy := *p.sysContent
	subRequest = append(subRequest, schemas.ChatMessage{
		Role:    schemas.ChatMessageRoleSystem,
		Content: &sysCopy,
	})

	// 2. История чата без system-сообщений пользователя — только user/assistant/developer.
	for _, msg := range messages {
		if msg.Role != schemas.ChatMessageRoleSystem {
			subRequest = append(subRequest, msg)
		}
	}

	// 3. Developer-инструкция — копируем указатель.
	devCopy := *p.devContent
	subRequest = append(subRequest, schemas.ChatMessage{
		Role:    schemas.ChatMessageRoleDeveloper,
		Content: &devCopy,
	})

	// Копируем параметры, чтобы не разделять мутабельное состояние между запросами.
	paramsCopy := *p.params

	// Отправляем подзапрос к модели через тот же клиент, что и основной запрос.
	boosterResp, err := p.client.ChatCompletionRequest(ctx, &schemas.BifrostChatRequest{
		Provider: provider,
		Model:    model,
		Input:    subRequest,
		Params:   &paramsCopy,
	})

	// Если произошла ошибка или ответ пустой — логируем и возвращаем пустую строку.
	if err != nil {
		if err.Error != nil {
			p.logger.Warn("llmboster: boost sub-request failed: %s", err.Error.Message)
		} else {
			p.logger.Warn("llmboster: boost sub-request failed")
		}
		return ""
	}
	if boosterResp == nil || len(boosterResp.Choices) == 0 {
		p.logger.Warn("llmboster: boost sub-request returned empty response")
		return ""
	}

	// Берём первый вариант ответа — именно он содержит сгенерированный текст.
	choice := boosterResp.Choices[0]
	if choice.ChatNonStreamResponseChoice == nil || choice.ChatNonStreamResponseChoice.Message == nil {
		p.logger.Warn("llmboster: boost response missing choice data")
		return ""
	}

	msg := choice.ChatNonStreamResponseChoice.Message

	// Fallback 1: текстовый контент
	if msg.Content != nil && msg.Content.ContentStr != nil && strings.TrimSpace(*msg.Content.ContentStr) != "" {
		return strings.TrimSpace(*msg.Content.ContentStr)
	}

	// Fallback 2: reasoning/thinking (DeepSeek, OpenAI o-серия, xAI Grok)
	if msg.ChatAssistantMessage != nil && msg.Reasoning != nil && strings.TrimSpace(*msg.Reasoning) != "" {
		p.logger.Debug("llmboster: using reasoning content (no text)")
		return strings.TrimSpace(*msg.Reasoning)
	}

	p.logger.Warn("llmboster: boost response has no usable content")
	return ""
}

// HTTPTransportPreHook вызывается на уровне HTTP-транспорта до того, как запрос попадёт в пайплайн LLM.
// В этом плагине ничего не делаем — возвращаем nil (продолжаем обработку дальше).
func (p *Plugin) HTTPTransportPreHook(ctx *schemas.BifrostContext, req *schemas.HTTPRequest) (*schemas.HTTPResponse, error) {
	return nil, nil
}

// HTTPTransportPostHook вызывается после того, как ответ уже отправлен клиенту.
// В этом плагине ничего не делаем — просто возвращаем nil.
func (p *Plugin) HTTPTransportPostHook(ctx *schemas.BifrostContext, req *schemas.HTTPRequest, resp *schemas.HTTPResponse) error {
	return nil
}

// HTTPTransportStreamChunkHook вызывается для каждого чанка при потоковой передаче ответа.
// В этом плагине ничего не меняем — возвращаем чанк без изменений.
func (p *Plugin) HTTPTransportStreamChunkHook(ctx *schemas.BifrostContext, req *schemas.HTTPRequest, chunk *schemas.BifrostStreamChunk) (*schemas.BifrostStreamChunk, error) {
	return chunk, nil
}

// isEmptyMessage проверяет, является ли сообщение пустым.
// Сообщение считается пустым, если:
//   - у него нет контента (Content == nil)
//   - или текстовое поле ContentStr отсутствует / пустая строка
//   - и при этом нет блоков контента (ContentBlocks пустой)
func isEmptyMessage(msg schemas.ChatMessage) bool {
	if msg.Content == nil {
		return true
	}
	if msg.Content.ContentStr != nil && strings.TrimSpace(*msg.Content.ContentStr) != "" {
		return false
	}
	if len(msg.Content.ContentBlocks) > 0 {
		return false
	}
	return true
}

// normalizeInput приводит input к каноническому виду (Phase 1):
//  1. Все системные сообщения объединяются в одно system-сообщение в начале.
//  2. Developer-сообщения остаются на своих позициях.
//  3. Удаляются все полностью пустые сообщения (без любого содержимого).
//
// Функция мутабельно изменяет переданный слайс и возвращает результат.
func normalizeInput(input []schemas.ChatMessage) []schemas.ChatMessage {
	var systemTexts []string
	var conversation []schemas.ChatMessage

	for _, msg := range input {
		if msg.Role == schemas.ChatMessageRoleSystem {
			if msg.Content != nil && msg.Content.ContentStr != nil {
				if t := strings.TrimSpace(*msg.Content.ContentStr); t != "" {
					systemTexts = append(systemTexts, t)
				}
			}
			continue
		}
		conversation = append(conversation, msg)
	}

	var normalized []schemas.ChatMessage
	if len(systemTexts) > 0 {
		joined := strings.Join(systemTexts, "\n\n")
		normalized = append(normalized, schemas.ChatMessage{
			Role:    schemas.ChatMessageRoleSystem,
			Content: &schemas.ChatMessageContent{ContentStr: new(joined)},
		})
	}
	normalized = append(normalized, conversation...)

	result := make([]schemas.ChatMessage, 0, len(normalized))
	for _, msg := range normalized {
		if !isEmptyMessage(msg) {
			result = append(result, msg)
		}
	}
	return result
}
