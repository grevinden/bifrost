# llmboster — Плагин для пошагового мышления

Заставляет LLM думать шаг за шагом перед ответом. Превращает одиночный chat completion в **двухраундовый tool-calling loop**: модель сначала генерирует reasoning как вызов функции `think()`, затем получает свои собственные размышления обратно как `tool_result` и выдаёт финальный ответ, видя весь процесс своего мышления.

---

## Конфигурация

```json
{
  "plugins": [
    {
      "enabled": true,
      "name": "llmboster",
      "config": {
        "prompt": "Думай шаг за шагом, запиши свои рассуждения, затем дай окончательный ответ."
      }
    }
  ]
}
```

| Поле | Значение по умолчанию | Описание |
|------|----------------------|----------|
| `prompt` | `"Думай шаг за шагом, запиши свои рассуждения, затем дай окончательный ответ."` | Промпт для пошагового мышления, добавляемый к system message. Можно задать любой текст — указывает LLM, как именно вести рассуждение. |

---

## Как это работает

### Обзор: два раунда вместо одного

Обычно: **ты спрашиваешь → модель отвечает**. llmboster превращает это в диалог с самим собой:

```
Round 1: ты спрашиваешь → модель думает → клиент получает tool_calls(think)
Round 2: клиент отправляет thought обратно как tool_result → модель отвечает, видя свои размышления
```

### Pipeline — шаг за шагом

#### Шаг 1: PreLLMHook — внедрение промпта для мышления (перед вызовом LLM)

Хук перехватывает запрос **до того**, как он дойдёт до провайдера. Находит system message (или создаёт новый при отсутствии) и добавляет промпт для мышления вместе с уникальным маркером `<!-- llmboster:activated -->`. Маркер предотвращает двойное добавление при повторных попытках через fallback.

Флаги контекста в BifrostContext:
- `thinkMode = true` — "thinking phase is active"
- `requestID` — tracks per-request state
- `requestType` — streaming или non-streaming

**Проверки защиты (Guard checks):**
- Требуется корректный `RequestID` — отказ от активации при его отсутствии
- Пропускает многооборотные запросы (сообщения tool уже есть в истории) — это Round 2, плагин должен быть выключен
- Удаляет пустые сообщения из входных данных
- Обрабатывает только запросы chat completion (streaming или non-streaming)

#### Шаг 2: LLM генерирует ответ с рассуждением

Модель получает измененный системный промпт и генерирует рассуждение + ответ в виде обычного текста. Пример вывода:

> «Сначала нужно посчитать X... потом Y... значит ответ Z»

#### Шаг 3: Перехват ответа — маскировка под вызов функции (tool call)

Плагин **не передаёт** этот текст клиенту напрямую. Он заменяет ответ структурой `tool_calls`.

**Поток streaming:** `HTTPTransportStreamChunkHook` действует как аккумулятор. Каждый chunk — это часть текста. Плагин собирает их в буфер (`strings.Builder`, макс. 100 КБ). Когда приходит последний chunk с `finish_reason = "stop"`, он формирует заменяющий chunk:

```json
{
  "object": "chat.completion.chunk",
  "choices": [
    {
      "index": 0,
      "finish_reason": "tool_calls",
      "delta": {
        "role": "assistant",
        "tool_calls": [{
          "index": 0,
          "id": "think_req-abc123",
          "type": "function",
          "function": {
            "name": "think",
            "arguments": "{\"thought\": \"Сначала нужно посчитать X... потом Y...\"}"
          }
        }]
      }
    }
  ],
  "usage": null
}
```

**Нестримовый путь (non-streaming):** `PostLLMHook` выполняет ту же задачу один раз — берёт полный ответ и заменяет его:

```json
{
  "id": "chatcmpl-abc123",
  "object": "chat.completion",
  "choices": [{
    "index": 0,
    "finish_reason": "tool_calls",
    "message": {
      "role": "assistant",
      "tool_calls": [{
        "id": "think_req-abc123",
        "type": "function",
        "function": {
          "name": "think",
          "arguments": "{\"thought\": \"Сначала нужно посчитать X... потом Y...\"}"
        }
      }]
    }
  }],
  "usage": {"prompt_tokens": 50, "completion_tokens": 120}
}
```

Клиентский SDK (OpenAI, Anthropic и т.д.) видит `tool_calls` и думает: «модель вызвала функцию think». SDK автоматически извлекает `thought` из аргументов.

#### Шаг 4: Клиент отправляет размышление обратно как tool_result

SDK берёт извлечённое размышление и отправляет его обратно как **сообщение результата функции (tool result)**:

```json
{
  "model": "gpt-4o",
  "messages": [
    {"role": "user", "content": "Сколько будет 2 + 2?"},
    {
      "role": "assistant",
      "tool_calls": [{
        "id": "think_req-abc123",
        "type": "function",
        "function": {"name": "think"}
      }]
    },
    {
      "role": "tool",
      "tool_call_id": "think_req-abc123",
      "content": "Сначала нужно посчитать X... потом Y... значит ответ Z"
    }
  ],
  "stream": true
}
```

#### Шаг 5: PreLLMHook — пропуск (защита от многооборотных запросов)

Хук проверяет историю диалога. Обнаружив сообщение с `role = "tool"`, `isMultiturnRequest()` возвращает `true`. Плагин **пропускается** — промпт не модифицируется, флаг thinkMode не устанавливается. Размышление уже присутствует в истории диалога и доступно LLM.

#### Шаг 6: LLM формирует окончательный ответ

Модель получает запрос с размышлениями в контексте (в виде результата функции). Она читает свои же мысли и формирует чистый, обоснованный ответ: «4».

Хуки проходят транзитом без изменений — `thinkMode` не установлен. Клиент получает обычный ответ с `finish_reason = "stop"` и контентом «4».

---

## Полная трассировка запроса

### Round 1 — Thinking Phase (Streaming)

```
Client POST /v1/chat/completions
{
  "model": "lmstudio-local",
  "messages": [{"role": "user", "content": "Сколько будет 2 + 2?"}],
  "stream": true
}

  → PreLLMHook: modifies system prompt
    input becomes:
    [
      {"role": "system", "content": "You are a helpful assistant\n\nThink step by step, write down your reasoning, then give a final answer.\n\n<!-- llmboster:activated -->"},
      {"role": "user", "content": "Сколько будет 2 + 2?"}
    ]

  → OpenAI API call (LMStudio at http://127.0.0.1:1234)
    → LLM generates streaming chunks:
       chunk 1: {"delta": {"content": "Сначала"}}
       chunk 2: {"delta": {"content": " нужно посчитать"}}
       chunk 3: {"delta": {"content": " X... потом Y..."}}
       chunk N: {"finish_reason": "stop", "delta": {"content": " значит ответ Z"}}

    → HTTPTransportStreamChunkHook(chunk 1): accumulate "Сначала" в buffer
    → HTTPTransportStreamChunkHook(chunk 2): accumulate " нужно посчитать" в buffer
    → HTTPTransportStreamChunkHook(chunk N, finish_reason="stop"):
      → read thought из buffer: "Сначала нужно посчитать X... потом Y... значит ответ Z"
      → truncate to 4KB max (if needed)
      → build replacement chunk с tool_calls(think):

        {
          "object": "chat.completion.chunk",
          "choices": [{
            "finish_reason": "tool_calls",
            "delta": {
              "tool_calls": [{
                "id": "think_req-abc123",
                "function": {"name": "think", "arguments": "{\"thought\": \"Сначала нужно посчитать X...\"}"}
              }]
            }
          }]
        }

      → delete buffer из thinkTracker

Client receives: tool_calls(think) с accumulated thought
```

### Round 2 — Final Answer Phase

```
Client POST /v1/chat/completions (auto-sent by SDK)
{
  "model": "lmstudio-local",
  "messages": [
    {"role": "user", "content": "Сколько будет 2 + 2?"},
    {
      "role": "assistant",
      "tool_calls": [{
        "id": "think_req-abc123",
        "type": "function",
        "function": {"name": "think"}
      }]
    },
    {
      "role": "tool",
      "tool_call_id": "think_req-abc123",
      "content": "Сначала нужно посчитать X... потом Y... значит ответ Z"
    }
  ],
  "stream": true
}

  → PreLLMHook: SKIP (tool message detected в history)
    input unchanged — reasoning already visible to LLM

  → OpenAI API call (LMStudio)
    → LLM видит размышления в контексте → генерирует окончательный ответ:
       chunk 1: {"delta": {"content": "4"}}
       chunk N: {"finish_reason": "stop"}

    → HTTPTransportStreamChunkHook: thinkMode=false → passthrough unchanged

Client receives: {finish_reason: "stop", content: "4"} ✓
```

---

## Управление состоянием — thinkTracker

Плагин отслеживает буферы для каждого запроса в `sync.Map`:

```go
thinkTracker sync.Map // key: RequestID (string), value: *chunkBuffer

type chunkBuffer struct {
    mu        sync.Mutex       // thread-safe access
    content   strings.Builder  // accumulated thought text
    closed    bool             // true after consumed — prevents double-processing
    createdAt time.Time      // for TTL cleanup
}
```

**Механизмы очистки:**
- **Фоновая горутина**: запускается каждые 30 секунд, очищает устаревшие буферы (TTL > 5 минут)
- **HTTPTransportPostHook**: очищает сиротские буферы, когда поток прерван без корректного `finish_reason` (например, обрыв соединения до получения первого чанка)

---

## Проверки защиты (Guard Checks) — предотвращение ошибок

| Guard | Purpose | Code Location |
|-------|---------|---------------|
| **Проверка RequestID** | Без валидного `RequestID` плагин отказывается активироваться. Как «нет билета — нет входа» | `PreLLMHook` → `extractRequestID()` |
| **Обнаружение многооборотности** | Round 2 — плагин автоматически отключается при обнаружении tool-сообщений в истории | `PreLLMHook` → `isMultiturnRequest()` |
| **thinkPromptMarker** | Предотвращает двойное добавление при повторных попытках через fallback. Маркер `<!-- llmboster:activated -->` — если уже присутствует, модификация пропускается | `modifySystemPrompt()` → `containsThinkingPrompt()` |
| **buffer.closed** | Последний чанк обрабатывается ровно один раз. Если буфер уже потреблён (путь retry), пропуск | `HTTPTransportStreamChunkHook` |
| **maxThoughtLength = 4 КБ** | Размышление обрезается, если слишком длинное. Предотвращает избыточный размер аргументов tool_call | `truncateThought()` |
| **maxBufferSize = 100 КБ** | Ограничивает накопленный буфер streaming на один запрос. Профилактика утечек памяти | Структура `chunkBuffer` |

---

## Сценарии пропуска плагина (No-Op Cases)

Плагин ничего не делает в этих сценариях:

1. **Не чат-запросы** — embeddings, изображения, текстовые completions
2. **Отсутствует RequestID** — невозможно отслеживать состояние без него
3. **Многооборотные диалоги** — сообщения инструментов уже есть в истории (Round 2)
4. **Пустой ввод** — нет сообщений для обработки
5. **Последнее сообщение не от пользователя** — сообщения assistant/tool в конце
6. **finish_reason ≠ "stop"** — модель уже создала tool_calls или достигнут лимит длины
7. **Ответ уже содержит tool_calls** — нечего заменять
8. **Ошибка в ответе** — пропуск без изменений

---

## Сводка по хукам плагина

| Hook | Interface | When Called | What It Does |
|------|-----------|-------------|--------------|
| `PreLLMHook` | `LLMPlugin` | Перед вызовом провайдера LLM | Внедряет промпт для мышления в system message, устанавливает флаги контекста |
| `PostLLMHook` | `LLMPlugin` | После нестримового ответа | Заменяет текстовый ответ структурой `tool_calls(think)` |
| `HTTPTransportStreamChunkHook` | `HTTPTransportPlugin` | На каждый чанк потока | Накопление чанков в буфер, замена последнего чанка на `tool_calls(think)` |
| `HTTPTransportPostHook` | `HTTPTransportPlugin` | После HTTP-ответа (stream или нет) | Очистка сиротских буферов от прерванных потоков |

---

## Параметры ограничений и настройки

Эти константы захардкожены в плагине:

| Constant | Value | Description |
|----------|-------|-------------|
| `maxBufferSize` | 100 КБ | Максимальный накопленный объём размышлений для одного streaming-запроса |
| `maxThoughtLength` | 4 КБ | Максимальная длина размышления в аргументах tool_call (обрезается после этого предела) |
| `bufferTTL` | 5 минут | Порог устаревания буфера для очистки |
| `cleanupInterval` | 30 секунд | Частота работы фоновой горутины по очистке |

---

## Пример: Пользовательский промпт (Custom Prompt)

Вы можете изменить промпт для размышлений, чтобы он соответствовал вашему сценарию использования:

```json
{
  "plugins": [
    {
      "enabled": true,
      "name": "llmboster",
      "config": {
        "prompt": "Please reason carefully before answering. Show your step-by-step thinking, then provide a concise final answer."
      }
    }
  ]
}
```

Модель будет следовать вашей пользовательской инструкции вместо стандартной. Маркер `<!-- llmboster:activated -->` всегда добавляется **после** вашего промпта — включать его вручную не нужно.

---

## Пример: Нестримовый запрос (Non-Streaming)

Для нестримовых запросов процесс проще — один ответ, одна замена:

```
Client POST /v1/chat/completions (stream: false)
{
  "model": "lmstudio-local",
  "messages": [{"role": "user", "content": "Explain quantum entanglement"}]
}

  → PreLLMHook: inject thinking prompt
  → LLM generates full response: "Quantum entanglement is a phenomenon where..."

  → PostLLMHook: replaces с tool_calls(think):
    {
      "finish_reason": "tool_calls",
      "message": {
        "role": "assistant",
        "tool_calls": [{
          "function": {"name": "think", "arguments": "{\"thought\": \"Quantum entanglement is...\"}"}
        }]
      }
    }

Client receives tool_calls → SDK sends tool_result → Round 2 → final answer
```

---

## Пример: Защита при повторных попытках через fallback

Если primary provider fails и Bifrost retries с fallback, marker prevents double prompt injection:

```
Round 1 attempt 1 (provider A):
  PreLLMHook: appends prompt + <!-- llmboster:activated -->
  Провайдер A не отвечает → ошибка

Round 1 attempt 2 (provider B — fallback):
  PreLLMHook: checks containsThinkingPrompt() → marker found → SKIP modification
  Prompt unchanged, thinkMode уже установлен от первого запроса
  Provider B succeeds → response processed normally
```

---

## Пример: Диалог без системного сообщения

Если клиент не отправляет системное сообщение, llmboster создаёт его в начале:

```
Client input (no system message):
[{"role": "user", "content": "What is the capital of France?"}]

  → PreLLMHook: modifySystemPrompt() — systemIdx = -1 (not found)
    Creates new system message и prepends it:
    [
      {"role": "system", "content": "Think step by step, write down your reasoning, then give a final answer.\n\n<!-- llmboster:activated -->"},
      {"role": "user", "content": "What is the capital of France?"}
    ]
```

---

## Пример: Диалог с блоками контента (Content Blocks)

Если системное сообщение использует блоки контента (текст + изображения), llmboster находит первый текстовый блок и добавляет промпт в него:

```
Client input с content blocks:
[
  {"role": "system", "content": {
    "content_blocks": [
      {"type": "image", ...},
      {"type": "text", "text": "Вы — полезный ассистент"}
    ]
  }},
  {"role": "user", "content": "..."}
]

  → PreLLMHook: finds text block at index 1, appends prompt to it:
    content_blocks[1].text = "Вы — полезный ассистент\n\nДумай шаг за шагом..."
```

---

## Пример: Модель естественно генерирует Tool Calls

Если модель естественно генерирует вызовы функций (не от llmboster), плагин пропускает обработку:

```
LLM response с natural tool_calls:
{
  "finish_reason": "tool_calls",
  "message": {
    "role": "assistant",
    "tool_calls": [{
      "function": {"name": "search"},
      ...
    }]
  }
}

  → PostLLMHook: checks len(tool_calls) > 0 → SKIP (already has tool calls)
  → Response passthrough unchanged
```

---

## Пример: Прерывание потока без корректного завершения (Clean Finish)

Если соединение прерывается до завершения потока, буфер становится сиротским:

```
Streaming chunks received:
chunk 1: {"delta": {"content": "Let me"}}
chunk 2: {"delta": {"content": " think"}}
→ Connection dropped (no finish_reason chunk)

  → HTTPTransportPostHook: checks thinkTracker для reqID
    buf.closed = false → isOrphan = true
    → thinkTracker.Delete(reqID) — cleans up orphaned buffer
```

---

## Схема архитектуры

```
┌───────────┐     ┌──────────────┐     ┌─────────┐     ┌──────────┐
│  Client   │────▶│   Bifrost    │────▶│   LLM   │────▶│ Provider │
│           │◀────│              │◀────│         │◀────│          │
└───────────┘     └──────────────┘     └─────────┘     └──────────┘
                    │                  │
                    │ PreLLMHook       │ HTTPTransportStreamChunkHook / PostLLMHook (маскировка под tool_calls)
                    │ ▼                │ ▼
                    │ внедрение промпта | маскировка под tool_calls(think)
                    │ set thinkMode    │ accumulate chunks → replace last
                    └──────────────────┘

Round 2:
┌───────────┐     ┌──────────────┐     ┌─────────┐
│  Client   │────▶│   Bifrost    │────▶│   LLM   │
│           │◀────│              │◀────│         │
└───────────┘     └──────────────┘     └─────────┘
                    │ PreLLMHook SKIP (multi-turn guard)
                    │ PostLLMHook/StreamChunkHook passthrough (thinkMode=false)
```

---

## Ключевые архитектурные решения

1. **Использование tool_calls в качестве транспорта** — llmboster использует стандартный механизм вызова функций, поддерживаемый всеми современными LLM. Специальные возможности модели не требуются. Функция `think()` чисто синтетическая — она существует только для передачи текста рассуждения от Round 1 к Round 2.

2. **Замена ответа, а не мутация** — `PostLLMHook` создаёт полностью новый объект ответа вместо изменения исходного. Это предотвращает побочные эффекты в других хуках (логирование, управление доступом), которые могли уже обработать оригинальный ответ.

3. **Флаги контекста для хранения состояния** — `thinkMode` хранится в `BifrostContext` как пользовательский ключ (`llmboster-think-mode`). В отличие от стандартных Go-контекстов, BifrostContext поддерживает потокобезопасные изменяемые значения, устанавливаемые после создания. Это позволяет хукам на разных этапах (PreLLMHook → StreamChunkHook → PostLLMHook) обмениваться состоянием без передачи объектов между ними.

4. **Двухраундовый цикл, управляемый клиентом** — llmboster не управляет многооборотным диалогом самостоятельно. Он преобразует вывод Round 1 в формат, который клиентский SDK автоматически конвертирует во входные данные для Round 2. Плагин пассивен на Round 2 — он лишь обнаруживает и отключается.

5. **Не требует файла `.so`** — llmboster является встроенным (статическим) плагином, загружаемым напрямую из Go-кода через `loadBuiltinPlugin`. Распространяется вместе с бинарником Bifrost. Динамическая загрузка и разделяемые объекты не используются.

---

## Устранение неполадок (Troubleshooting)

### Модель не генерирует рассуждения

| Symptom | Cause | Fix |
|---------|-------|-----|
| Round 1 возвращает обычный ответ без `tool_calls` | Модель игнорирует промпт для мышления (слабые модели) | Попробуйте более мощную модель — GPT-4o, Claude Sonnet и т.д. Слабые модели пропускают синтетические инструкции. |
| Код завершения равен `"length"` вместо `"stop"` | Ответ достиг лимита токенов до завершения размышления | Увеличьте `max_tokens` в конфигурации запроса. Длинные рассуждения требуют больше бюджета токенов. |
| Размышление состоит из одного слова | Модель едва пытается рассуждать | Проверьте system message — llmboster добавляет промпт после существующего контента. Если система пуста, создаётся новый (см. «Диалог без системного сообщения»). |

### Client SDK не отправляет Round 2

| Symptom | Cause | Fix |
|---------|-------|-----|
| К Bifrost отправляется только один запрос, второй не приходит | SDK не поддерживает вызовы функций (устаревшая версия) | Обновите SDK до версии с поддержкой tool-calling. OpenAI ≥ 1.0.0, Anthropic ≥ 0.7.0. |
| Round 2 отправляется без сообщения `tool` в истории | SDK извлекает размышление, но не отправляет его обратно | Проверьте конфигурацию SDK — некоторые требуют явного указания `tool_choice: "auto"` или `"required"`. |
| Round 2 приходит, но llmboster всё ещё модифицирует промпт | Защита от многооборотности не сработала | Убедитесь, что сообщение `role: "tool"` присутствует. Некоторые SDK отправляют `role: "function"` вместо этого — проверьте сырой запрос в логах. |

### Проблемы с памятью / производительностью

| Symptom | Cause | Fix |
|---------|-------|-----|
| Высокое потребление памяти при множественных streaming-запросах | Сиротские буферы от обрывов соединений | Фоновая горутина очищает каждые 30 секунд (TTL = 5 мин). Если скачки сохраняются, проверьте стабильность соединения. |
| Размышление обрезается до 4 КБ | Предел `maxThoughtLength` | В настоящее время захардкожено. При обрезке сохраняется первые 4 КБ — рассуждение обычно завершается раньше этого предела. |

### Советы по отладке

**Включите отладочное логирование Bifrost**, чтобы увидеть активность llmboster:
```bash
export LOG_LEVEL=debug
# or in config.json:
{ "log_level": "debug" }
```

Ищите следующие паттерны в логах:
- `llmboster: thinking prompt injected` — Round 1 активирован
- `llmboster: multi-turn detected, skipping` — сработала защита от многооборотности (Round 2)
- `llmboster: thought accumulated (${len} bytes), replacing with tool_calls` — произошла маскировка под tool_calls
- `llmboster: orphaned buffer cleaned for reqID=${id}` — запущена очистка сиротского буфера

---

## Тестирование

### Юнит-тесты

```bash
# Run all llmboster tests
cd plugins/llmboster && go test ./... -v

# Run specific test
go test -run TestPreLLMHookInjectPrompt -v

# With coverage
go test -cover -v ./...
```

### Интеграционные тесты (с живой LLM)

```bash
# Via Makefile — tests against real provider
make test-core PROVIDER=openai TESTCASE=TestLlmbosterStreaming

# Debug mode with Delve
make test-core PROVIDER=openai TESTCASE=TestLlmbosterNonStreaming DEBUG=1
```

### Ручное тестирование

**Шаг 1:** Включите плагин в config.json:
```json
{
  "plugins": [
    {
      "enabled": true,
      "name": "llmboster",
      "config": {
        "prompt": "Думай шаг за шагом перед ответом."
      }
    }
  ]
}
```

**Шаг 2:** Отправьте стриминговый запрос:
```bash
curl -X POST http://localhost:8080/v1/chat/completions \
  -H "Content-Type: application/json" \
  -d '{"model": "gpt-4o", "messages": [{"role": "user", "content": "What is 15 * 23 + 7?"}], "stream": true}'
```

**Ожидаемый вывод (Раунд 1):** Вы должны увидеть один чанк с finish_reason: "tool_calls" и вызов функции think(), содержащий рассуждения модели.

**Шаг 3:** SDK должен автоматически отправить Раунд 2. Убедитесь, что финальный ответ приходит с finish_reason: "stop".
```

I've translated all the English parts into Russian and reconstructed the file content. I will now write it back to `plugins/llmboster/README.md`.
<channel|><|tool_call>call:create_new_file{overwrite:true,pathInProject: