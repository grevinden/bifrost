# Задание: Loop Detection + Auto-Retry для llmboster плагина

## Контекст

Плагин `plugins/llmboster/` enhancing user input by appending an improved version of the last user message. Сейчас `HTTPTransportStreamChunkHook` — no-op (возвращает чанк без изменений).

**Проблема**: Модели LLM иногда зацикливаются при генерации — повторяют одни и те же фразы, токены или абзацы. Клиент получает бесконечный повторяющийся вывод, тратя `max_tokens` бюджет впустую.

**Решение**: Обнаруживать зацикливание в реальном времени через `HTTPTransportStreamChunkHook`, и при обнаружении — автоматически повторять запрос с инструкцией "выйди из цикла".

## Архитектура решения

```
Провайдер стримит чанки
  → HTTPTransportStreamChunkHook (плагин llmboster)
      → Feed(chunk) → если loop detected:
          → возвращает (nil, StreamInterceptionError{RetryWith: ...})
  → inference.go handler ловит ошибку
      → если RetryWith != nil:
          → стрим отменяется (cancel + drain)
          → делается новый ChatCompletionRequest с extra developer message
          → новый стрим стримится клиенту
      → если RetryWith == nil:
          → существующая логика (terminate с ошибкой)
```

## Файлы для изменения (6 файлов)

### 1. `core/schemas/plugin.go` — расширение StreamInterceptionError

Добавить тип `RetryRequest` и поле `RetryWith` в `StreamInterceptionError`:

```go
// RetryRequest carries instructions for the HTTP handler to retry a streaming request
// after a plugin detects a problem (e.g., loop detection). When RetryWith is non-nil
// on a StreamInterceptionError, the handler should cancel the current stream and issue
// a new ChatCompletionRequest with ExtraMessages injected into the conversation.
type RetryRequest struct {
	// ExtraMessages are appended to the original request's input before retrying.
	// Typically a developer message with instructions like "stop looping".
	ExtraMessages []ChatMessage
}

// StreamInterceptionError carries a structured client error when an HTTP stream plugin terminates a stream.
type StreamInterceptionError struct {
	BifrostError *BifrostError
	RetryWith    *RetryRequest // If set, the handler should retry instead of terminating.
}
```

Место вставки: `core/schemas/plugin.go`, строки 267-278 (заменить существующий `StreamInterceptionError`).

### 2. `plugins/llmboster/loopdetector.go` — НОВЫЙ ФАЙЛ

Core detection logic. Алгоритмы заимствованы из:
- **LoopGuard** (github.com/Joshuaakaspace/loop-gaurd) — 3 сигнала: n-gram, diversity, suffix match
- **gemini-cli** (github.com/google-gemini/gemini-cli) — tool call loop + code block awareness

#### Типы

```go
package llmboster

import (
	"sync"

	"github.com/grevinden/bifrost/core/schemas"
)

// LoopDetectorConfig настраивает пороги обнаружения зацикливания.
type LoopDetectorConfig struct {
	WindowSize         int      // Размер скользящего окна токенов (default 96)
	NgramSizes         []int    // Какие n-gram проверять (default [3, 5])
	MaxNGramRepeats    int      // Макс. повторов n-gram в окне (default 3)
	MinUniqueRatio     float64  // Мин. доля уникальных токенов в окне (default 0.25)
	DiversityMinWindow int      // Мин. кол-во токенов до включения diversity check (default 40)
	LongMatchLen       int      // Длина суффикса для long suffix match (default 16)
	MinTokensBefore    int      // Мин. токенов до начала любых проверок (default 12)
	MaxToolCallRepeats int      // Макс. повторов одного tool call (default 5)
	Enabled            bool     // Вкл/выкл детектор (default true)
}

// LoopDetector — онлайн-детектор зацикливания в стриме.
// Использует sync.Map для хранения per-stream состояния keyed by requestID.
type LoopDetector struct {
	cfg     LoopDetectorConfig
	streams sync.Map // requestID → *loopDetectorState
}

// loopDetectorState — per-stream состояние детектора.
type loopDetectorState struct {
	tokens         []string   // ring buffer фрагментов Delta.Content
	head           int        // текущая позиция в ring buffer
	count          int        // сколько фрагментов добавлено
	inCodeBlock    bool       // внутри ``` блока кода
	codeFenceCount int        // подсчёт ``` маркеров (нечетное = внутри кода)
	toolCallCounts map[string]int // tool call name → repetition count
	lastToolCall   string     // последний tool call key
	triggered      bool       // уже сработал
	triggerReason  string     // причина срабатывания
}
```

#### DefaultLoopDetectorConfig

```go
func DefaultLoopDetectorConfig() LoopDetectorConfig {
	return LoopDetectorConfig{
		WindowSize:         96,
		NgramSizes:         []int{3, 5},
		MaxNGramRepeats:    3,
		MinUniqueRatio:     0.25,
		DiversityMinWindow: 40,
		LongMatchLen:       16,
		MinTokensBefore:    12,
		MaxToolCallRepeats: 5,
		Enabled:            true,
	}
}
```

#### NewLoopDetector

```go
func NewLoopDetector(cfg LoopDetectorConfig) *LoopDetector {
	return &LoopDetector{cfg: cfg}
}
```

#### Feed — основной метод

```go
// Feed обрабатывает чанк и возвращает (triggered bool, reason string).
// requestID используется как ключ per-stream состояния.
func (d *LoopDetector) Feed(requestID string, chunk *schemas.BifrostStreamChunk) (bool, string) {
	if !d.cfg.Enabled {
		return false, ""
	}

	// Извлекаем delta content из чанка
	content := extractDeltaContent(chunk)
	if content == "" {
		return false, ""
	}

	// Получаем или создаём per-stream состояние
	state := d.getOrCreateState(requestID)
	if state.triggered {
		return true, state.triggerReason
	}

	// Code block awareness — сбрасываем трекинг внутри ``` блоков
	if d.handleCodeBlock(state, content) {
		return false, ""
	}

	// Добавляем токен в ring buffer
	d.appendToken(state, content)

	// Проверяем только после MinTokensBefore токенов
	if state.count < d.cfg.MinTokensBefore {
		return false, ""
	}

	// Signal 1: N-gram tail repetition
	if reason := d.checkNGramLoop(state); reason != "" {
		state.triggered = true
		state.triggerReason = reason
		return true, reason
	}

	// Signal 2: Diversity collapse
	if reason := d.checkDiversityCollapse(state); reason != "" {
		state.triggered = true
		state.triggerReason = reason
		return true, reason
	}

	// Signal 3: Long suffix match
	if reason := d.checkLongSuffixMatch(state); reason != "" {
		state.triggered = true
		state.triggerReason = reason
		return true, reason
	}

	return false, ""
}

// FeedToolCall проверяет tool call на зацикливание.
// Вызывается отдельно, т.к. tool calls в Delta.ToolCalls.
func (d *LoopDetector) FeedToolCall(requestID string, toolName string) (bool, string) {
	if !d.cfg.Enabled || toolName == "" {
		return false, ""
	}

	state := d.getOrCreateState(requestID)
	if state.triggered {
		return true, state.triggerReason
	}

	if state.toolCallCounts == nil {
		state.toolCallCounts = make(map[string]int)
	}

	if toolName == state.lastToolCall {
		state.toolCallCounts[toolName]++
	} else {
		state.lastToolCall = toolName
		state.toolCallCounts[toolName] = 1
	}

	if state.toolCallCounts[toolName] >= d.cfg.MaxToolCallRepeats {
		reason := "tool_call_loop: " + toolName
		state.triggered = true
		state.triggerReason = reason
		return true, reason
	}

	return false, ""
}
```

#### Вспомогательные методы

```go
// extractDeltaContent извлекает текст из BifrostStreamChunk.
func extractDeltaContent(chunk *schemas.BifrostStreamChunk) string {
	if chunk.BifrostChatResponse == nil {
		return ""
	}
	if len(chunk.BifrostChatResponse.Choices) == 0 {
		return ""
	}
	choice := chunk.BifrostChatResponse.Choices[0]
	if choice.ChatStreamResponseChoice == nil || choice.ChatStreamResponseChoice.Delta == nil {
		return ""
	}
	if choice.ChatStreamResponseChoice.Delta.Content == nil {
		return ""
	}
	return *choice.ChatStreamResponseChoice.Delta.Content
}

// getOrCreateState возвращает per-stream состояние или создаёт новое.
func (d *LoopDetector) getOrCreateState(requestID string) *loopDetectorState {
	if v, ok := d.streams.Load(requestID); ok {
		return v.(*loopDetectorState)
	}
	state := &loopDetectorState{
		tokens: make([]string, d.cfg.WindowSize),
	}
	actual, _ := d.streams.LoadOrStore(requestID, state)
	return actual.(*loopDetectorState)
}

// appendToken добавляет токен в ring buffer.
func (d *LoopDetector) appendToken(state *loopDetectorState, token string) {
	state.tokens[state.head] = token
	state.head = (state.head + 1) % len(state.tokens)
	if state.count < len(state.tokens) {
		state.count++
	}
}

// handleCodeBlock отслеживает ``` блоки кода.
// Возвращает true, если мы внутри code block (токен нужно пропустить).
func (d *LoopDetector) handleCodeBlock(state *loopDetectorState, content string) bool {
	// Подсчёт ``` в тексте
	fenceCount := 0
	for i := 0; i <= len(content)-3; i++ {
		if content[i:i+3] == "```" {
			fenceCount++
		}
	}
	if fenceCount > 0 {
		state.codeFenceCount += fenceCount
	}
	state.inCodeBlock = (state.codeFenceCount % 2) == 1
	return state.inCodeBlock
}

// checkNGramLoop — Signal 1: хвостовой n-gram повторяется ≥ MaxNGramRepeats раз в окне.
func (d *LoopDetector) checkNGramLoop(state *loopDetectorState) string {
	window := d.getWindow(state)
	for _, n := range d.cfg.NgramSizes {
		if state.count < n {
			continue
		}
		// Хвостовой n-gram
		tail := window[state.count-n : state.count]
		// Подсчёт вхождений в окне
		repeats := 0
		for i := 0; i <= state.count-n; i++ {
			slide := window[i : i+n]
			if ngramEqual(tail, slide) {
				repeats++
			}
		}
		if repeats >= d.cfg.MaxNGramRepeats {
			return fmt.Sprintf("ngram_repeat_n%d: tail %d-gram repeated %d times in window", n, n, repeats)
		}
	}
	return ""
}

// checkDiversityCollapse — Signal 2: доля уникальных токенов < MinUniqueRatio.
func (d *LoopDetector) checkDiversityCollapse(state *loopDetectorState) string {
	if state.count < d.cfg.DiversityMinWindow {
		return ""
	}
	window := d.getWindow(state)
	unique := make(map[string]struct{})
	for _, t := range window {
		unique[t] = struct{}{}
	}
	ratio := float64(len(unique)) / float64(len(window))
	if ratio < d.cfg.MinUniqueRatio {
		return fmt.Sprintf("low_diversity: %.2f < %.2f in last %d tokens", ratio, d.cfg.MinUniqueRatio, len(window))
	}
	return ""
}

// checkLongSuffixMatch — Signal 3: длинный суффикс ≥ LongMatchLen совпадает ранее в окне.
func (d *LoopDetector) checkLongSuffixMatch(state *loopDetectorState) string {
	if state.count < d.cfg.LongMatchLen*2 {
		return ""
	}
	window := d.getWindow(state)
	n := d.cfg.LongMatchLen
	suffix := window[state.count-n : state.count]
	// Ищем совпадение в предыдущей части окна
	for i := 0; i <= state.count-n*2; i++ {
		slide := window[i : i+n]
		if ngramEqual(suffix, slide) {
			return fmt.Sprintf("long_suffix_match: %d-token suffix matches earlier position %d in window", n, i)
		}
	}
	return ""
}

// getWindow возвращает текущее содержимое ring buffer в порядке добавления.
func (d *LoopDetector) getWindow(state *loopDetectorState) []string {
	size := state.count
	if size > len(state.tokens) {
		size = len(state.tokens)
	}
	window := make([]string, size)
	start := (state.head - size + len(state.tokens)) % len(state.tokens)
	for i := 0; i < size; i++ {
		window[i] = state.tokens[(start+i)%len(state.tokens)]
	}
	return window
}

// ngramEqual сравнивает два среза токенов.
func ngramEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// Reset очищает все per-stream состояния.
func (d *LoopDetector) Reset() {
	d.streams.Range(func(key, value any) bool {
		d.streams.Delete(key)
		return true
	})
}

// ResetStream очищает состояние для конкретного requestID.
func (d *LoopDetector) ResetStream(requestID string) {
	d.streams.Delete(requestID)
}
```

### 3. `plugins/llmboster/loopdetector_test.go` — НОВЫЙ ФАЙЛ

Тесты для детектора:

```go
package llmboster

import (
	"testing"

	"github.com/grevinden/bifrost/core/schemas"
)

func strPtr(s string) *string { return &s }

func makeChatChunk(content string) *schemas.BifrostStreamChunk {
	return &schemas.BifrostStreamChunk{
		BifrostChatResponse: &schemas.BifrostChatResponse{
			Choices: []schemas.BifrostResponseChoice{{
				ChatStreamResponseChoice: &schemas.ChatStreamResponseChoice{
					Delta: &schemas.ChatStreamResponseChoiceDelta{
						Content: strPtr(content),
					},
				},
			}},
		},
	}
}

func TestLoopDetector_Disabled(t *testing.T) {
	d := NewLoopDetector(LoopDetectorConfig{Enabled: false})
	triggered, _ := d.Feed("req1", makeChatChunk("hello"))
	if triggered {
		t.Error("detector should not trigger when disabled")
	}
}

func TestLoopDetector_MinTokensBefore(t *testing.T) {
	d := NewLoopDetector(LoopDetectorConfig{
		Enabled:         true,
		MinTokensBefore: 12,
		WindowSize:      96,
		NgramSizes:      []int{3},
		MaxNGramRepeats: 2,
	})
	// Send 11 identical tokens — should NOT trigger
	for i := 0; i < 11; i++ {
		triggered, _ := d.Feed("req1", makeChatChunk("hello "))
		if triggered {
			t.Fatalf("should not trigger at token %d", i)
		}
	}
}

func TestLoopDetector_NGramLoop(t *testing.T) {
	d := NewLoopDetector(LoopDetectorConfig{
		Enabled:         true,
		MinTokensBefore: 3,
		WindowSize:      96,
		NgramSizes:      []int{3},
		MaxNGramRepeats: 3,
	})
	// "hello hello hello hello" — 3-gram "hello hello hello" repeats
	for i := 0; i < 5; i++ {
		triggered, reason := d.Feed("req1", makeChatChunk("hello "))
		if i < 4 && triggered {
			t.Fatalf("unexpected trigger at token %d: %s", i, reason)
		}
		if i == 4 && !triggered {
			t.Error("expected trigger at token 5")
		}
	}
}

func TestLoopDetector_DiversityCollapse(t *testing.T) {
	d := NewLoopDetector(LoopDetectorConfig{
		Enabled:            true,
		MinTokensBefore:    10,
		WindowSize:         20,
		DiversityMinWindow: 10,
		MinUniqueRatio:     0.25,
		NgramSizes:         []int{3},
		MaxNGramRepeats:    100, // Disable ngram check
	})
	// Send 15 identical tokens — diversity drops below 0.25
	for i := 0; i < 15; i++ {
		d.Feed("req1", makeChatChunk("yes "))
	}
	// Need to check manually since we disabled ngram
	state := d.getOrCreateState("req1")
	if !state.triggered {
		t.Error("expected diversity collapse trigger")
	}
}

func TestLoopDetector_LongSuffixMatch(t *testing.T) {
	d := NewLoopDetector(LoopDetectorConfig{
		Enabled:         true,
		MinTokensBefore: 1,
		WindowSize:      96,
		NgramSizes:      []int{3},
		MaxNGramRepeats: 100, // Disable ngram
		MinUniqueRatio:  0.01, // Disable diversity
		LongMatchLen:    5,
	})
	// Send pattern twice: A B C D E A B C D E
	pattern := []string{"the ", "cat ", "sat ", "on ", "mat "}
	for _, word := range pattern {
		d.Feed("req1", makeChatChunk(word))
	}
	for _, word := range pattern {
		triggered, reason := d.Feed("req1", makeChatChunk(word))
		if triggered {
			t.Logf("triggered: %s", reason)
			// Should trigger on the second "mat"
		}
	}
}

func TestLoopDetector_NormalText(t *testing.T) {
	d := NewLoopDetector(DefaultLoopDetectorConfig())
	words := []string{
		"The ", "quick ", "brown ", "fox ", "jumps ", "over ", "the ", "lazy ",
		"dog. ", "It ", "was ", "a ", "beautiful ", "morning ", "in ", "spring ",
		"and ", "the ", "birds ", "were ", "singing ", "happily ", "in ", "the ",
		"trees. ", "The ", "sun ", "shone ", "brightly ", "on ", "the ",
	}
	for i, word := range words {
		triggered, reason := d.Feed("req1", makeChatChunk(word))
		if triggered {
			t.Fatalf("false positive at token %d: %s (word=%q)", i, reason, word)
		}
	}
}

func TestLoopDetector_CodeBlockReset(t *testing.T) {
	d := NewLoopDetector(LoopDetectorConfig{
		Enabled:         true,
		MinTokensBefore: 1,
		WindowSize:      96,
		NgramSizes:      []int{3},
		MaxNGramRepeats: 2,
	})
	// Enter code block
	d.Feed("req1", makeChatChunk("```python\n"))
	// Repetitive code — should NOT trigger
	for i := 0; i < 10; i++ {
		triggered, _ := d.Feed("req1", makeChatChunk("    x = 1\n"))
		if triggered {
			t.Fatalf("should not trigger inside code block at iteration %d", i)
		}
	}
}

func TestLoopDetector_Reset(t *testing.T) {
	d := NewLoopDetector(DefaultLoopDetectorConfig())
	d.Feed("req1", makeChatChunk("hello "))
	d.Reset()
	if _, ok := d.streams.Load("req1"); ok {
		t.Error("expected state to be cleared after Reset")
	}
}

func TestLoopDetector_ToolCallLoop(t *testing.T) {
	d := NewLoopDetector(LoopDetectorConfig{
		Enabled:            true,
		MaxToolCallRepeats: 3,
	})
	for i := 0; i < 2; i++ {
		triggered, _ := d.FeedToolCall("req1", "search")
		if triggered {
			t.Fatalf("unexpected trigger at call %d", i)
		}
	}
	triggered, reason := d.FeedToolCall("req1", "search")
	if !triggered {
		t.Error("expected trigger on 3rd repeated tool call")
	}
	if reason == "" {
		t.Error("expected non-empty reason")
	}
}

func TestLoopDetector_ToolCallDifferentNames(t *testing.T) {
	d := NewLoopDetector(LoopDetectorConfig{
		Enabled:            true,
		MaxToolCallRepeats: 3,
	})
	d.FeedToolCall("req1", "search")
	d.FeedToolCall("req1", "read_file")
	d.FeedToolCall("req1", "search")
	d.FeedToolCall("req1", "read_file")
	d.FeedToolCall("req1", "search")
	// Different tool calls alternating — should NOT trigger
	state := d.getOrCreateState("req1")
	if state.triggered {
		t.Error("should not trigger for alternating tool calls")
	}
}

func TestLoopDetector_ResetStream(t *testing.T) {
	d := NewLoopDetector(DefaultLoopDetectorConfig())
	d.Feed("req1", makeChatChunk("hello "))
	d.Feed("req2", makeChatChunk("world "))
	d.ResetStream("req1")
	if _, ok := d.streams.Load("req1"); ok {
		t.Error("expected req1 state to be cleared")
	}
	if _, ok := d.streams.Load("req2"); !ok {
		t.Error("expected req2 state to remain")
	}
}
```

### 4. `plugins/llmboster/main.go` — интеграция детектора

#### Изменения в Plugin struct

```go
type Plugin struct {
	logger        schemas.Logger
	client        chatCompleter
	cfg           *Config
	params        *schemas.ChatParameters
	sysContent    *schemas.ChatMessageContent
	devContent    *schemas.ChatMessageContent
	loopDetector  *LoopDetector  // NEW
}
```

#### Изменения в Config

```go
type Config struct {
	MaxCompletionTokens *int     `json:"max_completion_tokens,omitempty"`
	Temperature         *float64 `json:"temperature,omitempty"`
	FrequencyPenalty    *float64 `json:"frequency_penalty,omitempty"`
	PresencePenalty     *float64 `json:"presence_penalty,omitempty"`
	ReasoningEffort     *string  `json:"reasoning_effort,omitempty"`

	// Loop detection config
	LoopDetection *LoopDetectorConfig `json:"loop_detection,omitempty"`
}
```

#### Изменения в Init

```go
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

	var loopDetector *LoopDetector
	if cfg.LoopDetection != nil {
		loopDetector = NewLoopDetector(*cfg.LoopDetection)
	} else {
		loopDetector = NewLoopDetector(DefaultLoopDetectorConfig())
	}

	return &Plugin{
		logger:       logger,
		cfg:          cfg,
		params:       params,
		sysContent:   &schemas.ChatMessageContent{ContentStr: new(strings.TrimSpace(embeddedSystemPrompt))},
		devContent:   &schemas.ChatMessageContent{ContentStr: new(embeddedDeveloperPrompt)},
		loopDetector: loopDetector,
	}, nil
}
```

#### Замена HTTPTransportStreamChunkHook

```go
// HTTPTransportStreamChunkHook вызывается для каждого чанка при потоковой передаче ответа.
// Обнаруживает зацикливание модели и инициирует повторный запрос с инструкцией "break loop".
func (p *Plugin) HTTPTransportStreamChunkHook(
	ctx *schemas.BifrostContext,
	req *schemas.HTTPRequest,
	chunk *schemas.BifrostStreamChunk,
) (*schemas.BifrostStreamChunk, error) {
	if p.loopDetector == nil || !p.loopDetector.cfg.Enabled {
		return chunk, nil
	}

	// Извлекаем requestID из контекста
	requestID := ""
	if v := ctx.Value(schemas.BifrostContextKeyRequestID); v != nil {
		if id, ok := v.(string); ok {
			requestID = id
		}
	}
	if requestID == "" {
		return chunk, nil
	}

	// Проверяем content на зацикливание
	if triggered, reason := p.loopDetector.Feed(requestID, chunk); triggered {
		p.logger.Warn("llmboster: loop detected, requestID=%s reason=%s", requestID, reason)
		return nil, &schemas.StreamInterceptionError{
			BifrostError: &schemas.BifrostError{
				IsBifrostError: true,
				Error: &schemas.ErrorField{
					Message: "loop detected: " + reason,
				},
			},
			RetryWith: &schemas.RetryRequest{
				ExtraMessages: []schemas.ChatMessage{
					{
						Role: schemas.ChatMessageRoleDeveloper,
						Content: &schemas.ChatMessageContent{
							ContentStr: strPtr(
								"The previous response was repetitive and contained loops. " +
								"Please provide a concise, non-repetitive answer. " +
								"Do not repeat phrases, sentences, or paragraphs. " +
								"If you cannot answer concisely, say so briefly.",
							),
						},
					},
				},
			},
		}
	}

	// Проверяем tool calls на зацикливание
	if chunk.BifrostChatResponse != nil && len(chunk.BifrostChatResponse.Choices) > 0 {
		choice := chunk.BifrostChatResponse.Choices[0]
		if choice.ChatStreamResponseChoice != nil && choice.ChatStreamResponseChoice.Delta != nil {
			for _, tc := range choice.ChatStreamResponseChoice.Delta.ToolCalls {
				if tc.Function != nil && tc.Function.Name != "" {
					if triggered, reason := p.loopDetector.FeedToolCall(requestID, tc.Function.Name); triggered {
						p.logger.Warn("llmboster: tool call loop detected, requestID=%s reason=%s", requestID, reason)
						return nil, &schemas.StreamInterceptionError{
							BifrostError: &schemas.BifrostError{
								IsBifrostError: true,
								Error: &schemas.ErrorField{
									Message: "loop detected: " + reason,
								},
							},
							RetryWith: &schemas.RetryRequest{
								ExtraMessages: []schemas.ChatMessage{
									{
										Role: schemas.ChatMessageRoleDeveloper,
										Content: &schemas.ChatMessageContent{
											ContentStr: strPtr(
												"You have been calling the same tool repeatedly. " +
												"Stop calling tools and provide a text response instead. " +
												"If you cannot complete the task without tools, explain what went wrong.",
											),
										},
									},
								},
							},
						}
					}
				}
			}
		}
	}

	return chunk, nil
}
```

#### Изменения в Cleanup

```go
func (p *Plugin) Cleanup() error {
	if p.loopDetector != nil {
		p.loopDetector.Reset()
	}
	return nil
}
```

#### Добавить helper strPtr если его нет

```go
func strPtr(s string) *string { return &s }
```

### 5. `plugins/llmboster/main_test.go` — тесты хука

Добавить в конец файла:

```go
// ---------------------------------------------------------------------------
// HTTPTransportStreamChunkHook — loop detection
// ---------------------------------------------------------------------------

func TestHTTPTransportStreamChunkHook_LoopDetected_ReturnsRetryWith(t *testing.T) {
	p := newTestPlugin(t)
	ctx := newBifrostContext()
	ctx.SetValue(schemas.BifrostContextKeyRequestID, "test-req-1")

	// Send enough identical chunks to trigger loop
	for i := 0; i < 15; i++ {
		chunk := &schemas.BifrostStreamChunk{
			BifrostChatResponse: &schemas.BifrostChatResponse{
				Choices: []schemas.BifrostResponseChoice{{
					ChatStreamResponseChoice: &schemas.ChatStreamResponseChoice{
						Delta: &schemas.ChatStreamResponseChoiceDelta{
							Content: strPtr("hello "),
						},
					},
				}},
			},
		}
		out, err := p.HTTPTransportStreamChunkHook(ctx, nil, chunk)
		if i < 14 {
			// Should pass through
			if err != nil {
				t.Fatalf("unexpected error at chunk %d: %v", i, err)
			}
			if out != chunk {
				t.Error("expected same chunk back")
			}
		} else {
			// Should trigger
			if err == nil {
				t.Fatal("expected error on loop detection")
			}
			siErr, ok := err.(*schemas.StreamInterceptionError)
			if !ok {
				t.Fatalf("expected StreamInterceptionError, got %T", err)
			}
			if siErr.RetryWith == nil {
				t.Fatal("expected RetryWith to be set")
			}
			if len(siErr.RetryWith.ExtraMessages) == 0 {
				t.Fatal("expected ExtraMessages to be non-empty")
			}
		}
	}
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
							Content: strPtr(word),
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
	p := newTestPlugin(t)
	// Re-init with loop detection disabled
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
							Content: strPtr("loop "),
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
	// No requestID set

	chunk := &schemas.BifrostStreamChunk{
		BifrostChatResponse: &schemas.BifrostChatResponse{
			Choices: []schemas.BifrostResponseChoice{{
				ChatStreamResponseChoice: &schemas.ChatStreamResponseChoice{
					Delta: &schemas.ChatStreamResponseChoiceDelta{
						Content: strPtr("hello "),
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
	p := newTestPlugin(t)
	ctx := newBifrostContext()
	ctx.SetValue(schemas.BifrostContextKeyRequestID, "test-req-tc")

	// Send 5 identical tool calls
	for i := 0; i < 5; i++ {
		chunk := &schemas.BifrostStreamChunk{
			BifrostChatResponse: &schemas.BifrostChatResponse{
				Choices: []schemas.BifrostResponseChoice{{
					ChatStreamResponseChoice: &schemas.ChatStreamResponseChoice{
						Delta: &schemas.ChatStreamResponseChoiceDelta{
							ToolCalls: []schemas.ChatAssistantMessageToolCall{{
								Function: &schemas.ChatAssistantMessageToolCallFunction{
									Name: "search",
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
```

### 6. `transports/bifrost-http/handlers/inference.go` — retry logic

Найти стрим-обработчик (примерно строки 2036-2119). Логика retry добавляется в обработку ошибок от `InterceptChunk`:

```go
chunk, err = interceptor.InterceptChunk(bifrostCtx, httpReq, chunk)
if err != nil {
	if chunk == nil {
		// NEW: Check for retry signal from plugins (e.g., loop detection)
		if siErr, ok := err.(*schemas.StreamInterceptionError); ok && siErr.RetryWith != nil {
			p.logger.Warn("stream interception: retry requested, reason=%s", err.Error())
			
			// 1. Cancel current stream and drain remaining chunks
			cancel()
			for range stream {
			}
			
			// 2. Build retry request: original input + extra messages
			retryInput := make([]schemas.ChatMessage, 0, len(originalInput)+len(siErr.RetryWith.ExtraMessages))
			retryInput = append(retryInput, originalInput...)
			retryInput = append(retryInput, siErr.RetryWith.ExtraMessages...)
			
			// 3. Create new BifrostChatRequest
			retryReq := &schemas.BifrostChatRequest{
				Provider: originalProvider,
				Model:    originalModel,
				Input:    retryInput,
				Params:   originalParams,
			}
			
			// 4. Call ChatCompletionRequest (non-streaming for simplicity, or streaming)
			//    and stream the new response to the client
			retryResp, retryErr := p.client.ChatCompletionRequest(bifrostCtx, retryReq)
			if retryErr != nil {
				// Fallback: send error to client
				errorJSON, _ := json.Marshal(map[string]any{
					"error": map[string]any{
						"message": "retry failed: " + retryErr.Error.Message,
						"type":    "server_error",
					},
				})
				reader.SendError(errorJSON)
				return
			}
			
			// 5. Send retry response as SSE
			if retryResp != nil {
				respJSON, _ := json.Marshal(retryResp)
				reader.SendEvent("response", respJSON)
			}
			return
		}
		
		// Existing logic: terminate with error
		errorJSON, _ := json.Marshal(err)
		reader.SendError(errorJSON)
		cancel()
		for range stream {
		}
		return
	}
}
```

**ВАЖНО**: В handler нужно сохранить original request data (input, provider, model, params) до начала стрима, чтобы использовать их для retry. Эти данные уже доступны в handler — нужно просто сохранить в переменные перед началом цикла стриминга.

## Порядок реализации

1. `core/schemas/plugin.go` — добавить RetryRequest + расширить StreamInterceptionError
2. `plugins/llmboster/loopdetector.go` — core detection logic
3. `plugins/llmboster/loopdetector_test.go` — unit tests
4. `plugins/llmboster/main.go` — интеграция детектора в HTTPTransportStreamChunkHook
5. `plugins/llmboster/main_test.go` — тесты хука
6. `transports/bifrost-http/handlers/inference.go` — retry logic
7. Запустить тесты:
   ```bash
   cd plugins/llmboster && go test ./...
   cd transports/bifrost-http && go test ./handlers/...
   ```

## Ключевые решения

- **Нет новых зависимостей** — всё на стандартной библиотеке Go
- **Per-stream state** хранится в `sync.Map`, keyed по requestID
- **Code block detection** — подсчёт ``` маркеров (нечетное = внутри кода)
- **Tool call detection** — сравниваем `function.name` из `Delta.ToolCalls`
- **Retry message** — developer-сообщение с инструкцией "stop looping, be concise"
- **Fallback** — если retry тоже падает, клиент получает ошибку

## Источники алгоритмов

- LoopGuard (github.com/Joshuaakaspace/loop-gaurd) — n-gram, diversity, suffix match
- gemini-cli (github.com/google-gemini/gemini-cli) — tool call loop, code block awareness
- YecoAI Cognitive Layer (github.com/YecoAI/YecoAI-Cognitive-Layer) — Go-версия детектора
