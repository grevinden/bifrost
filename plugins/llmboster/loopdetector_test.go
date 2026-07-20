package llmboster

import (
	"testing"

	"github.com/grevinden/bifrost/core/schemas"
)

func makeChatChunk(content string) *schemas.BifrostStreamChunk {
	return &schemas.BifrostStreamChunk{
		BifrostChatResponse: &schemas.BifrostChatResponse{
			Choices: []schemas.BifrostResponseChoice{{
				ChatStreamResponseChoice: &schemas.ChatStreamResponseChoice{
					Delta: &schemas.ChatStreamResponseChoiceDelta{
						Content: ptr(content),
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
		MaxNGramRepeats: 3, // tail excluded, so need 3 previous occurrences = 6 total tokens
	})
	for i := 0; i < 6; i++ {
		triggered, reason := d.Feed("req1", makeChatChunk("hello"))
		if i < 5 && triggered {
			t.Fatalf("unexpected trigger at token %d: %s", i, reason)
		}
		if i == 5 && !triggered {
			t.Error("expected trigger at token 6")
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
	for i := 0; i < 15; i++ {
		d.Feed("req1", makeChatChunk("yes "))
	}
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
		MaxNGramRepeats: 100,  // Disable ngram
		MinUniqueRatio:  0.01, // Disable diversity
		LongMatchLen:    5,
	})
	pattern := []string{"the ", "cat ", "sat ", "on ", "mat "}
	for _, word := range pattern {
		d.Feed("req1", makeChatChunk(word))
	}
	for _, word := range pattern {
		triggered, reason := d.Feed("req1", makeChatChunk(word))
		if triggered {
			t.Logf("triggered: %s", reason)
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
	d.Feed("req1", makeChatChunk("```python\n"))
	for i := 0; i < 10; i++ {
		triggered, _ := d.Feed("req1", makeChatChunk("    x = 1\n"))
		if triggered {
			t.Fatalf("should not trigger inside code block at iteration %d", i)
		}
	}
}

func TestLoopDetector_CodeBlockReset_PerChunkBacktick(t *testing.T) {
	d := NewLoopDetector(LoopDetectorConfig{
		Enabled:         true,
		MinTokensBefore: 1,
		WindowSize:      96,
		NgramSizes:      []int{3},
		MaxNGramRepeats: 2,
	})
	// В SSE ``` может приходить по одному символу: "`", "`", "`" (три чанка подряд).
	// После трёх чанков с ` должен включиться code block.
	d.Feed("req1", makeChatChunk("`")) // backtickRun = 1
	d.Feed("req1", makeChatChunk("`")) // backtickRun = 2
	d.Feed("req1", makeChatChunk("`")) // backtickRun = 3 → fence! codeFenceCount=1 → inCodeBlock=true
	// Четвёртый чанк уже должен быть внутри code block
	state := d.getOrCreateState("req1")
	if !state.inCodeBlock {
		t.Error("expected to be inside code block after 3 per-chunk backticks")
	}
	// Внутри code block токены должны пропускаться
	d.Feed("req1", makeChatChunk("python\n"))
	for i := 0; i < 10; i++ {
		triggered, _ := d.Feed("req1", makeChatChunk("    x = 1\n"))
		if triggered {
			t.Fatalf("should not trigger inside code block at iteration %d", i)
		}
	}
	// Закрываем code block также посимвольно
	d.Feed("req1", makeChatChunk("`")) // backtickRun = 1
	d.Feed("req1", makeChatChunk("`")) // backtickRun = 2
	d.Feed("req1", makeChatChunk("`")) // backtickRun = 3 → fence! codeFenceCount=2 → inCodeBlock=false
	if state.inCodeBlock {
		t.Error("expected to be outside code block after closing fence")
	}
	// После закрытия code block детектор снова активен
	triggered, _ := d.Feed("req1", makeChatChunk("outside code"))
	if triggered {
		t.Error("normal text outside code block should not trigger")
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
	state := d.getOrCreateState("req1")
	if state.triggered {
		t.Error("should not trigger for alternating tool calls")
	}
}

func TestLoopDetector_DefaultsApplied(t *testing.T) {
	// Тест проверяет, что applyLoopDetectionDefaults заполняет нулевые поля
	cfg := &LoopDetectorConfig{Enabled: true}
	applyLoopDetectionDefaults(cfg)
	if cfg.WindowSize != 96 {
		t.Errorf("WindowSize = %d, want 96", cfg.WindowSize)
	}
	if cfg.NgramSizes == nil || len(cfg.NgramSizes) != 2 || cfg.NgramSizes[0] != 3 || cfg.NgramSizes[1] != 5 {
		t.Errorf("NgramSizes = %v, want [3 5]", cfg.NgramSizes)
	}
	if cfg.MaxNGramRepeats != 5 {
		t.Errorf("MaxNGramRepeats = %d, want 5", cfg.MaxNGramRepeats)
	}
	if cfg.MinUniqueRatio != 0.25 {
		t.Errorf("MinUniqueRatio = %f, want 0.25", cfg.MinUniqueRatio)
	}
	if cfg.DiversityMinWindow != 40 {
		t.Errorf("DiversityMinWindow = %d, want 40", cfg.DiversityMinWindow)
	}
	if cfg.LongMatchLen != 16 {
		t.Errorf("LongMatchLen = %d, want 16", cfg.LongMatchLen)
	}
	if cfg.MinTokensBefore != 24 {
		t.Errorf("MinTokensBefore = %d, want 24", cfg.MinTokensBefore)
	}
	if cfg.MaxToolCallRepeats != 5 {
		t.Errorf("MaxToolCallRepeats = %d, want 5", cfg.MaxToolCallRepeats)
	}
	if cfg.Enabled != true {
		t.Errorf("Enabled = %v, want true", cfg.Enabled)
	}
}

func TestLoopDetector_EnabledWithPartialConfig(t *testing.T) {
	// Пользователь указал только enabled:true — defaults должны защитить от zero-значений
	cfg := &LoopDetectorConfig{Enabled: true}
	applyLoopDetectionDefaults(cfg)
	d := NewLoopDetector(*cfg)

	// Должно работать без panic с дефолтным WindowSize=96
	for i := 0; i < 10; i++ {
		triggered, _ := d.Feed("req1", makeChatChunk("hello "))
		if triggered {
			t.Fatalf("unexpected trigger at token %d", i)
		}
	}
}

func TestLoopDetector_DisabledConfig_AllFieldsZero(t *testing.T) {
	// Пользователь указал только enabled:false — defaults применяются,
	// но Enabled остаётся false, детектор не активен
	cfg := &LoopDetectorConfig{Enabled: false}
	applyLoopDetectionDefaults(cfg)
	d := NewLoopDetector(*cfg)

	// Должно быть выключено, несмотря на заполненные defaults
	for i := 0; i < 20; i++ {
		triggered, _ := d.Feed("req1", makeChatChunk("loop "))
		if triggered {
			t.Fatalf("detector should not trigger when disabled, triggered at token %d", i)
		}
	}
}

func TestLoopDetector_ResetStreamCleanup(t *testing.T) {
	d := NewLoopDetector(DefaultLoopDetectorConfig())
	d.Feed("req1", makeChatChunk("hello "))
	d.FeedToolCall("req1", "search")
	d.FeedToolCall("req1", "search")

	// После ResetStream состояние должно быть полностью очищено
	d.ResetStream("req1")
	if _, ok := d.streams.Load("req1"); ok {
		t.Error("expected req1 state to be removed after ResetStream")
	}

	// Новое состояние должно начаться с чистого листа
	triggered, _ := d.FeedToolCall("req1", "search")
	if triggered {
		t.Error("expected tool call count to reset after ResetStream")
	}
}
