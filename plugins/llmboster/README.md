# llmboster — Плагин улучшения запросов (Query Booster + Loop Detection)

Улучшает запросы пользователя перед отправкой LLM: автоматически нормализует контекст, отправляет sub-запрос к той же модели для «полировки» последнего сообщения (prompt refinement) и добавляет результат как developer message. Исходное сообщение пользователя всегда сохраняется (Option A).

Дополнительно: обнаруживает зацикливание модели в стриминговых ответах по трём сигналам (n-gram, diversity, long suffix) + tool call loop и инициирует повтор с инструкцией «break loop».

---

## Конфигурация

```json
{
  "plugins": [
    {
      "enabled": true,
      "name": "llmboster",
      "config": {
        "max_completion_tokens": 1024,
        "temperature": 0.3,
        "frequency_penalty": 0.5,
        "presence_penalty": 0.3,
        "reasoning_effort": "none",
        "loop_detection": {
          "enabled": true,
          "window_size": 96,
          "ngram_sizes": [3, 5],
          "max_ngram_repeats": 5,
          "min_unique_ratio": 0.25,
          "diversity_min_window": 40,
          "long_match_len": 16,
          "min_tokens_before": 24,
          "max_tool_call_repeats": 5
        }
      }
    }
  ]
}
```

### Параметры бустинга

| Поле | По умолчанию | Описание |
|------|-------------|----------|
| `max_completion_tokens` | `1024` | Максимум токенов для sub-запроса |
| `temperature` | `0.3` | Температура (низкая — более детерминированное улучшение) |
| `frequency_penalty` | `0.5` | Штраф за частотность (чтобы не повторяться) |
| `presence_penalty` | `0.3` | Штраф за присутствие тем |
| `reasoning_effort` | `"none"` | Режим рассуждения (`"none"`, `"low"`, `"medium"`, `"high"`) |

### Параметры loop detection

| Поле | По умолчанию | Описание |
|------|-------------|----------|
| `enabled` | `false` | Включить детектор зацикливания |
| `window_size` | `96` | Размер скользящего окна токенов (ring buffer) |
| `ngram_sizes` | `[3, 5]` | Какие n-gram проверять на повторение |
| `max_ngram_repeats` | `5` | Максимум повторов tail n-gram в окне |
| `min_unique_ratio` | `0.25` | Минимальная доля уникальных токенов |
| `diversity_min_window` | `40` | Минимум токенов до проверки diversity |
| `long_match_len` | `16` | Длина суффикса для long suffix match |
| `min_tokens_before` | `24` | Минимум токенов до начала проверок |
| `max_tool_call_repeats` | `5` | Максимум повторений одного tool call подряд |

---

## Как это работает

### Две фазы

```
        ╔═══════════════════════════════════════════════╗
        ║           Клиент (HTTP / SDK)                  ║
        ╚═══════════════════════════════════════════════╝
                          │
                          ▼
        ╔═══════════════════════════════════════════════╗
        ║         PreLLMHook (peред LLM call)            ║
        ║                                                ║
        ║  Phase 1: normalizeInput()                     ║
        ║  ├── Объединить system-сообщения               ║
        ║  └── Удалить пустые сообщения                  ║
        ║                                                ║
        ║  Phase 2: boostMessage()                       ║
        ║  ├── Если последнее сообщение от user           ║
        ║  ├── Отправить sub-запрос LLM:                 ║
        ║  │   [system] Universal Prompt Architect       ║
        ║  │   [user] original message                   ║
        ║  │   [dev]   Refine this instruction...        ║
        ║  └── Добавить результат как developer message   ║
        ╚═══════════════════════════════════════════════╝
                          │
                          ▼
        ┌─────────────────────────────────────────────────┐
        │         Provider (OpenAI, Anthropic, ...)        │
        │         LLM генерирует финальный ответ           │
        └─────────────────────────────────────────────────┘
                          │
                          ▼ (SSE stream)
        ╔═══════════════════════════════════════════════╗
        ║   HTTPTransportStreamChunkHook (loop detect)   ║
        ║                                                ║
        ║  Для каждого чанка:                            ║
        ║  ├── Signal 1: n-gram repetition              ║
        ║  ├── Signal 2: diversity collapse             ║
        ║  ├── Signal 3: long suffix match              ║
        ║  └── Tool call loop detection                  ║
        ║                                                ║
        ║  Если loop → StreamInterceptionError           ║
        ║               └── RetryWith: "break loop"      ║
        ╚═══════════════════════════════════════════════╝
                          │
                          ▼
                   Клиент (SSE)
```

### PreLLMHook — пошагово

1. **Recursion guard**: если контекст уже содержит флаг `llmbosterRecursionGuard` (установлен при sub-запросе), возвращаем запрос без изменений — предотвращает бесконечную рекурсию.

2. **Phase 1 — normalizeInput**: 
   - Все system-сообщения объединяются в одно в начале диалога
   - Developer-сообщения остаются на своих позициях
   - Пустые сообщения (nil/whitespace) удаляются

3. **Phase 2 — boost** (только если последнее сообщение от user):
   - Собирается sub-запрос: `[system prompt] + [история чата без system] + [developer instruction]`
   - **System prompt**: плагин встраивает промпт «Universal Context Architect & Prompt Synthesizer», который динамически адаптируется к любой теме
   - **Developer instruction**: чёткая инструкция «Refine the user's raw input into a single cleaned English instruction»
   - Вызывается `ChatCompletionRequest` (синхронно) к той же модели
   - Извлечение ответа с fallback:
     1. `Content.ContentStr` — обычный текст
     2. `ChatAssistantMessage.Reasoning` — для DeepSeek, OpenAI o-series, xAI Grok
   - Результат добавляется в конец Input как `developer`-сообщение

**Важно**: исходное сообщение пользователя **всегда сохраняется** — улучшенная версия только дополняет его.

### HTTPTransportStreamChunkHook — Loop Detection

Детектор анализирует каждый чанк SSE-стрима. Используется per-stream состояние (ключ — `requestID`), которое автоматически чистится при повторном запросе.

#### Сигнал 1: N-gram Loop
Хвостовые n токенов (по умолчанию n=3 и n=5) сравниваются со всеми предыдущими n-gram в окне. Если хвост повторился ≥ `max_ngram_repeats` раз (не считая текущего) — loop.

**Пример детекции:**
```
Токены: "a", "b", "c", "a", "b", "c", "a", "b", "c", ...
n=3, tail=["a","b","c"], repeats=2 → trigger
```

#### Сигнал 2: Diversity Collapse
Считается `len(unique_tokens) / window_size`. Если отношение падает ниже `min_unique_ratio` — модель зациклилась на одних и тех же токенах.

**Пример:** 40 токенов, из них только 4 уникальных → ratio = 0.10 < 0.25 → trigger.

#### Сигнал 3: Long Suffix Match
Хвостовые `long_match_len` токенов сравниваются со всеми более ранними позициями в окне. Если идентичный суффикс найден — модель повторяет блок текста.

#### Tool Call Loop
Отслеживаются повторяющиеся вызовы одного и того же инструмента подряд. Если один tool вызван ≥ `max_tool_call_repeats` раз последовательно — loop.

**Обработка code blocks:** контент внутри ``` игнорируется детектором (не добавляется в окно), чтобы повторяющийся код не вызывал ложных срабатываний. SSE может отправлять ` ``` ` как три отдельных символа — это корректно обрабатывается.

#### Retry механизм
При детекции зацикливания:
1. Текущий стрим отменяется и дренируется
2. В `inference.go` создаётся новый стрим с тем же `requestID`
3. К исходным сообщениям добавляется developer-сообщение: *«The previous response was repetitive... Please provide a concise, non-repetitive answer»*
4. Новый стрим проходит те же plugin-хуки
5. **Вторичное зацикливание** не retry-ится — ошибка возвращается клиенту

---

## Пример: полный цикл запроса

**Исходный запрос:**
```
[
  {role: "system", content: "You are a helpful assistant"},
  {role: "user",   content: "make it scale"}
]
```

**После `normalizeInput`:** без изменений (уже канонический).

**Sub-запрос `boostMessage`:**
```
[
  {role: "system",   content: "You are the Universal Context Architect..."},
  {role: "user",     content: "make it scale"},
  {role: "developer", content: "Refine the user's raw input into..."}
]
```

**Ответ LLM на sub-запрос:**
> Act as a Principal Cloud Architect. Analyze the system architecture... provide a scalable, cloud-native refactoring strategy...

**Итоговый запрос к провайдеру:**
```
[
  {role: "system",   content: "You are a helpful assistant"},
  {role: "user",     content: "make it scale"},
  {role: "developer", content: "Act as a Principal Cloud Architect..."}
]
```

Модель получает исходный запрос + улучшенную версию как developer message.

---

## Сценарии пропуска (No-Op)

| Условие | Поведение |
|---------|-----------|
| Recursion guard в контексте | Пропуск — предотвращает бесконечную рекурсию sub-запроса |
| `req == nil` или `ChatRequest == nil` | Пропуск |
| Пустой `Input` | Пропуск |
| Последнее сообщение не от user | Пропуск (например, assistant отвечает на предыдущий) |
| `client == nil` (нет BifrostClient) | Бустинг не выполняется |
| Ошибка sub-запроса | Логируется, исходный запрос без изменений |
| Пустой ответ от бустера | Исходный запрос без изменений |

---

## Пример: минимальная конфигурация

```json
{
  "plugins": [
    {
      "enabled": true,
      "name": "llmboster"
    }
  ]
}
```

Без явной конфигурации используются все defaults. Loop detection выключен (`loop_detection.enabled: false`).

---

## Пример: бустинг + loop detection

```json
{
  "plugins": [
    {
      "enabled": true,
      "name": "llmboster",
      "config": {
        "temperature": 0.2,
        "reasoning_effort": "low",
        "loop_detection": {
          "enabled": true,
          "max_ngram_repeats": 8,
          "max_tool_call_repeats": 10
        }
      }
    }
  ]
}
```

---

## Сводка по хукам

| Хук | Назначение |
|-----|-----------|
| `PreRequestHook` | No-op (не участвует в маршрутизации) |
| `PreLLMHook` | Нормализация + бустинг запроса |
| `PostLLMHook` | No-op (ответ не модифицируется) |
| `HTTPTransportPreHook` | No-op |
| `HTTPTransportPostHook` | No-op |
| `HTTPTransportStreamChunkHook` | Loop detection + RetryWith |

---

## Защита от рекурсии

`boostMessage` перед sub-запросом устанавливает в `BifrostContext` кастомный ключ `llmbusterRecursionGuard{}`. Когда sub-запрос проходит через тот же пайплайн, `PreLLMHook` видит этот ключ и возвращает запрос без изменений, не пытаясь бустить его снова.

**Почему не используется штатный plugin scope:** Bifrost оборачивает контекст в plugin scope при каждом вызове плагина — это происходит для всех плагинов, не только при рекурсии. Свой ключ позволяет точно отличить рекурсивный вызов от обычного.

---

## Тестирование

### Юнит-тесты

`main_test.go` (28 тестов):

| Группа | Тесты |
|--------|-------|
| Init / Cleanup | `TestInit`, `TestCleanupIdempotent` |
| Embedded prompt | `TestEmbeddedPromptNotEmpty` |
| isEmptyMessage | 5 кейсов (nil, empty, whitespace, non-empty, content blocks) |
| PreLLMHook | nil request, no chat, empty input, all empty, last not user, preserves, no system, multiple systems, empty filtering |
| normalizeInput | systems in middle, at end, empty skipped, whitespace-only |
| Multimodal | no client, with text boost |
| boostMessage | reasoning fallback, text preferred, empty response |
| System filtering | user system excluded, multiple systems filtered |
| Recursion guard | `TestPreLLMHook_RecursiveSubRequest_ReturnsUnchanged` |
| Interface compliance | 3 интерфейса |
| Stream chunk hook | loop detected (RetryWith), no loop, disabled, no requestID, tool call loop |

`loopdetector_test.go` (15 тестов):

| Группа | Тесты |
|--------|-------|
| Core signals | disabled, min tokens, n-gram loop, diversity collapse, long suffix |
| False positives | normal text |
| Code blocks | ` ``` ` in one chunk, per-character ` `` ` across chunks |
| Tool calls | repeated same tool, alternating tools |
| Config | defaults, partial config, all-zero disabled |
| Cleanup | Reset, ResetStream |

### Интеграционные тесты

Полноценное тестирование с живой LLM:
```bash
make test-core PROVIDER=openai TESTCASE=TestLlmboster
```

### Ручное тестирование

Через HTTP (cURL):

```bash
curl -X POST http://localhost:8080/v1/chat/completions \
  -H "Content-Type: application/json" \
  -H "x-bf-api-key: <key>" \
  -d '{
    "model": "gpt-4o",
    "messages": [
      {"role": "system", "content": "You are a helpful assistant."},
      {"role": "user", "content": "make it scale"}
    ],
    "stream": true
  }'
```

Для проверки loop detection можно смоделировать зацикливание (в тестах).

---

## Устранение неполадок

### Бустер не срабатывает
- Проверьте, что последнее сообщение — от user (не assistant)
- Проверьте, что клиент Bifrost передаётся через `SetBifrostClient`
- Проверьте логи: `llmboster: boost returned empty` / `llmboster: boost sub-request failed`
- Если контекст содержит `llmbosterRecursionGuard` — это нормально, это sub-запрос

### Loop detection не срабатывает
- Убедитесь, что `loop_detection.enabled: true` (по умолчанию false)
- Проверьте, что в контексте есть `requestID`
- Для коротких ответов уменьшите `min_tokens_before`
- Для редких повторений уменьшите `max_ngram_repeats` или увеличьте `window_size`

### Ложные срабатывания loop detection
- Увеличьте `max_ngram_repeats` (реальные повторения — редко > 10)
- Увеличьте `min_unique_ratio` (0.30-0.35)
- Проверьте, не открыт ли code block (` ``` `) — детектор должен его игнорировать
- Добавьте `ngram_sizes: [4, 6]` — большие n-gram меньше подвержены ложным совпадениям

### Производительность
- Loop detection работает O(n*m) где n — window_size, m — число ngram_sizes, обычно < 500 операций на чанк
- Ring buffer pre-allocated, без аллокаций в hot path (кроме первой аллокации на поток)
- Per-stream состояние автоматически чистится через `ResetStream` при retry
- При отключённом детекторе (`enabled: false`) оверхэда нет

---

## Ключевые архитектурные решения

1. **Sub-запрос через тот же BifrostClient** — бустер использует тот же экземпляр клиента, что и основной запрос, поэтому проходит через те же плагины, fallback-логику и governance. Recursion guard предотвращает повторную обработку.

2. **Original user message всегда сохраняется** — улучшенная версия добавляется как developer message, не заменяя исходную. LLM видит обе и может выбрать лучшую.

3. **Ring buffer для токенов** — O(1) добавление, pre-allocated буфер, zero-alloc в hot path после первой инициализации.

4. **Code block awareness** — контент внутри ``` не попадает в детектор, предотвращая ложные срабатывания на повторяющемся коде. Поддержка SSE (по символам) реализована через character-by-character подсчёт backtickRun.

5. **Latching trigger** — после первого срабатывания детектор всегда возвращает triggered=true для этого requestID, пока не будет вызван ResetStream.

6. **Retry с тем же requestID** — после loop stream отменяется, но новый стрим использует тот же requestID. ResetStream гарантирует, что детектор начнёт с чистого листа.
