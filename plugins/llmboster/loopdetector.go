package llmboster

import (
	"fmt"
	"strings"
	"sync"

	"github.com/grevinden/bifrost/core/schemas"
)

// LoopDetectorConfig настраивает пороги обнаружения зацикливания.
type LoopDetectorConfig struct {
	WindowSize         int     `json:"window_size"`           // Размер скользящего окна токенов (default 96)
	NgramSizes         []int   `json:"ngram_sizes"`           // Какие n-gram проверять (default [3, 5])
	MaxNGramRepeats    int     `json:"max_ngram_repeats"`     // Макс. повторов n-gram в окне (default 5)
	MinUniqueRatio     float64 `json:"min_unique_ratio"`      // Мин. доля уникальных токенов в окне (default 0.25)
	DiversityMinWindow int     `json:"diversity_min_window"`  // Мин. кол-во токенов до включения diversity check (default 40)
	LongMatchLen       int     `json:"long_match_len"`        // Длина суффикса для long suffix match (default 16)
	MinTokensBefore    int     `json:"min_tokens_before"`     // Мин. токенов до начала любых проверок (default 24)
	MaxToolCallRepeats int     `json:"max_tool_call_repeats"` // Макс. повторов одного tool call (default 5)
	Enabled            bool    `json:"enabled"`               // Вкл/выкл детектор (default true)
}

// LoopDetector — онлайн-детектор зацикливания в стриме.
// Использует sync.Map для хранения per-stream состояния keyed by requestID.
type LoopDetector struct {
	cfg     LoopDetectorConfig
	streams sync.Map // requestID → *loopDetectorState
}

// loopDetectorState — per-stream состояние детектора.
type loopDetectorState struct {
	tokens         []string       // ring buffer фрагментов Delta.Content
	head           int            // текущая позиция в ring buffer
	count          int            // сколько фрагментов добавлено
	inCodeBlock    bool           // внутри ``` блока кода
	backtickRun    int            // счётчик последовательных ` в чанках подряд (для SSE где ``` приходит по 1 символу)
	codeFenceCount int            // подсчёт ``` маркеров (нечетное = внутри кода)
	toolCallCounts map[string]int // tool call name → repetition count
	lastToolCall   string         // последний tool call key
	triggered      bool           // уже сработал
	triggerReason  string         // причина срабатывания
	windowBuf      []string       // pre-allocated буфер для getWindow()
}

// DefaultLoopDetectorConfig возвращает конфигурацию по умолчанию.
func DefaultLoopDetectorConfig() LoopDetectorConfig {
	return LoopDetectorConfig{
		WindowSize:         96,
		NgramSizes:         []int{3, 5},
		MaxNGramRepeats:    5,
		MinUniqueRatio:     0.25,
		DiversityMinWindow: 40,
		LongMatchLen:       16,
		MinTokensBefore:    24,
		MaxToolCallRepeats: 5,
		Enabled:            false,
	}
}

// NewLoopDetector создаёт новый детектор с указанной конфигурацией.
func NewLoopDetector(cfg LoopDetectorConfig) *LoopDetector {
	return &LoopDetector{cfg: cfg}
}

// Feed обрабатывает чанк и возвращает (triggered bool, reason string).
// requestID используется как ключ per-stream состояния.
func (d *LoopDetector) Feed(requestID string, chunk *schemas.BifrostStreamChunk) (bool, string) {
	if !d.cfg.Enabled {
		return false, ""
	}

	content := extractDeltaContent(chunk)
	if content == "" {
		return false, ""
	}

	state := d.getOrCreateState(requestID)
	if state.triggered {
		return true, state.triggerReason
	}

	if d.handleCodeBlock(state, content) {
		return false, ""
	}

	d.appendToken(state, content)

	if state.count < d.cfg.MinTokensBefore {
		return false, ""
	}

	if reason := d.checkNGramLoop(state); reason != "" {
		state.triggered = true
		state.triggerReason = reason
		return true, reason
	}

	if reason := d.checkDiversityCollapse(state); reason != "" {
		state.triggered = true
		state.triggerReason = reason
		return true, reason
	}

	if reason := d.checkLongSuffixMatch(state); reason != "" {
		state.triggered = true
		state.triggerReason = reason
		return true, reason
	}

	return false, ""
}

// FeedToolCall проверяет tool call на зацикливание.
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

// extractDeltaContent извлекает и нормализует текст из BifrostStreamChunk.
// Убирает ведущие/замыкающие пробелы, чтобы сравнивать содержательные фрагменты.
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
	return strings.TrimSpace(*choice.ChatStreamResponseChoice.Delta.Content)
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
// В SSE маркер ``` может приходить как один чанк "```", так и по одному символу
// (три чанка "`", "`", "`"). Поэтому используем character-by-character подсчёт
// с сохранением состояния между чанками (state.backtickRun).
// Возвращает true, если мы внутри code block (токен нужно пропустить).
func (d *LoopDetector) handleCodeBlock(state *loopDetectorState, content string) bool {
	for _, ch := range content {
		if ch == '`' {
			state.backtickRun++
		} else {
			if state.backtickRun >= 3 {
				state.codeFenceCount++
			}
			state.backtickRun = 0
		}
	}
	// Если чанк закончился на трёх и более ` — это тоже fence (SSE случай).
	if state.backtickRun >= 3 {
		state.codeFenceCount++
		state.backtickRun = 0
	}
	state.inCodeBlock = (state.codeFenceCount%2 == 1)
	return state.inCodeBlock
}

// checkNGramLoop — Signal 1: хвостовой n-gram повторяется ≥ MaxNGramRepeats раз в окне.
// Хвостовой n-gram (текущая позиция) не учитывается в подсчёте — считаются только
// предыдущие появления до текущей позиции.
func (d *LoopDetector) checkNGramLoop(state *loopDetectorState) string {
	window := d.getWindow(state)
	for _, n := range d.cfg.NgramSizes {
		if state.count < n {
			continue
		}
		tail := window[state.count-n : state.count]
		repeats := 0
		for i := 0; i < state.count-n; i++ {
			slide := window[i : i+n]
			if ngramEqual(tail, slide) {
				repeats++
			}
		}
		if repeats >= d.cfg.MaxNGramRepeats {
			return fmt.Sprintf("ngram_repeat_n%d: tail %d-gram repeated %d times before current position", n, n, repeats)
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
	if d.cfg.LongMatchLen <= 0 || state.count < d.cfg.LongMatchLen*2 {
		return ""
	}
	window := d.getWindow(state)
	n := d.cfg.LongMatchLen
	suffix := window[state.count-n : state.count]
	for i := 0; i <= state.count-n*2; i++ {
		slide := window[i : i+n]
		if ngramEqual(suffix, slide) {
			return fmt.Sprintf("long_suffix_match: %d-token suffix matches earlier position %d in window", n, i)
		}
	}
	return ""
}

// getWindow возвращает текущее содержимое ring buffer в порядке добавления.
// Использует pre-allocated буфер из loopDetectorState для избежания аллокаций.
func (d *LoopDetector) getWindow(state *loopDetectorState) []string {
	size := state.count
	if size > len(state.tokens) {
		size = len(state.tokens)
	}
	if cap(state.windowBuf) < size {
		state.windowBuf = make([]string, size)
	}
	window := state.windowBuf[:size]
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
	if v, ok := d.streams.LoadAndDelete(requestID); ok {
		if st, ok := v.(*loopDetectorState); ok {
			st.toolCallCounts = nil
			st.windowBuf = nil
			st.backtickRun = 0
		}
	}
}
