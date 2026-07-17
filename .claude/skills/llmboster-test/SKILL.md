---
name: llmboster-test
description: Test the llmboster plugin through Bifrost with local LLM providers (LMStudio, Ollama, etc.). Use when asked to test llmboster functionality, verify thinking mode works correctly, or debug llmboster integration issues. Invoked with /llmboster-test <FEATURE_NAME> or /llmboster-test all.
allowed-tools: Read, Grep, Glob, Bash, Edit, Write, Task, AskUserQuestion, TodoWrite
---

# llmboster Plugin Testing

Test the llmboster plugin through Bifrost with local LLM providers (LMStudio, Ollama, etc.). The llmboster plugin captures LLM "thinking" and converts it to tool_calls (think function), enabling reasoning-aware inference.

## Usage

```
/llmboster-test all                    # Test llmboster with all configured providers
/llmboster-test <PROVIDER_NAME>        # Test llmboster with a specific provider
/llmboster-test unit                   # Run only unit tests (no live API calls)
/llmboster-test integration            # Run integration tests with live API
```

## Prerequisites

1. **Local LLM server running** (LMStudio, Ollama, vLLM, etc.)
   - LMStudio: `http://127.0.0.1:1234/v1`
   - Ollama: `http://127.0.0.1:11434/v1`
   - vLLM: `http://127.0.0.1:8000/v1`

2. **Model available** (e.g., qwen3.6-35b-a3b, llama3, etc.)

3. **Bifrost binary built** with llmboster plugin support

## Testing Workflow

### Step 1: Verify Local LLM Server

```bash
# Check if LMStudio is running and model is available
curl -s "http://127.0.0.1:1234/v1/models" | python3 -m json.tool

# Test direct request to LMStudio
curl -s "http://127.0.0.1:1234/v1/chat/completions" \
  -H "Content-Type: application/json" \
  -d '{
    "model": "qwen3.6-35b-a3b",
    "messages": [{"role": "user", "content": "What is 2+2?"}],
    "max_tokens": 10,
    "temperature": 0.7
  }' | python3 -m json.tool
```

Expected: LMStudio should return a response with `choices` populated and `reasoning_content` field.

### Step 2: Configure Bifrost

Create or update `/tmp/app/config.json`:

```json
{
    "$schema": "https://www.getbifrost.ai/schema",
    "client": {
        "enable_logging": true,
        "initial_pool_size": 10
    },
    "config_store": {
        "enabled": true,
        "type": "sqlite",
        "config": {
            "path": "/tmp/app/bifrost.db"
        }
    },
    "logs_store": {
        "enabled": false
    },
    "providers": {
        "openai": {
            "keys": [
                {
                    "name": "lmstudio-local",
                    "value": "sk-lmstudio-not-a-real-key",
                    "weight": 1,
                    "models": ["*"]
                }
            ],
            "network_config": {
                "base_url": "http://127.0.0.1:1234",
                "default_request_timeout_in_seconds": 120,
                "max_retries": 0
            }
        }
    },
    "plugins": [
        {
            "name": "llmboster",
            "enabled": true,
            "config": {
                "prompt": "Think step by step. First reason through the problem carefully, then provide your final answer."
            }
        }
    ]
}
```

**CRITICAL**: The `base_url` should NOT include `/v1` — Bifrost appends it automatically. Common mistake: using `http://127.0.0.1:1234/v1` results in double `/v1/v1/` path.

### Step 3: Start Bifrost with Debug Logging

```bash
# Kill existing Bifrost
ps aux | grep bifrost-http-clean | grep -v grep | awk '{print $2}' | xargs kill 2>/dev/null
sleep 3

# Start Bifrost with debug logging
cd /home/rasty/Documents/bifrost && nohup /tmp/bifrost-http-clean \
  -app-dir /tmp/app \
  -log-level debug > /tmp/bifrost_test.log 2>&1 &
sleep 10

# Verify llmboster plugin is loaded
grep -i "llmboster" /tmp/bifrost_test.log | tail -5
```

Expected output:
```
llmboster plugin initialized (prompt length: XX chars)
plugin status: llmboster - active
```

### Step 4: Test Through Bifrost

```bash
# Test chat completion through Bifrost → LMStudio
curl -s "http://127.0.0.1:8080/v1/chat/completions" \
  -H "Content-Type: application/json" \
  -H "Authorization: Bearer sk-lmstudio-not-a-real-key" \
  -d '{
    "model": "openai/qwen3.6-35b-a3b",
    "messages": [
      {"role": "user", "content": "What is 2+2? Answer with just the number."}
    ],
    "max_tokens": 10,
    "temperature": 0.7
  }' | python3 -m json.tool
```

Expected response should include:
- `choices` populated (not null)
- `reasoning` field with model's thinking process
- `reasoning_details` array with the same text
- `usage` with token counts

### Step 5: Verify llmboster Plugin Behavior

Check that llmboster added the system prompt by examining the request body sent to LMStudio. Use a proxy or check Bifrost logs:

```bash
# Check if llmboster activated marker is in the request
grep -i "llmboster:activated" /tmp/bifrost_test.log || echo "Marker not found in logs"

# Verify system prompt was added (check proxy logs if using one)
```

Expected: The request to LMStudio should include a system message with:
```json
{
  "role": "system",
  "content": "Think step by step. First reason through the problem carefully, then provide your final answer.\n\n<!-- llmboster:activated -->"
}
```

### Step 6: Run Unit Tests

```bash
# Run llmboster plugin unit tests
cd /home/rasty/Documents/bifrost/plugins/llmboster && go test -v
```

Expected: All 30+ tests should pass.

### Step 7: Run make test-core (Optional)

```bash
# Run core provider tests for openai (LMStudio is OpenAI-compatible)
cd /home/rasty/Documents/bifrost && make test-core PROVIDER=openai
```

Note: Tests may be skipped if OPENAI_API_KEY is not set, which is expected when using LMStudio.

## Common Issues and Solutions

### Issue 1: `choices: null` in response

**Symptom**: Bifrost returns `{"choices": null, ...}` instead of actual choices.

**Root Cause**: Usually caused by incorrect `base_url` configuration (includes `/v1` when it shouldn't).

**Solution**:
1. Check config.json `network_config.base_url` — should NOT include `/v1`
2. Correct format: `"http://127.0.0.1:1234"` not `"http://127.0.0.1:1234/v1"`
3. Restart Bifrost after config change

### Issue 2: `dial tcp 127.0.0.1:1234: connect: connection refused`

**Symptom**: Bifrost cannot connect to LMStudio.

**Root Cause**: LMStudio is not running or model is not loaded.

**Solution**:
1. Verify LMStudio is running: `curl -s "http://127.0.0.1:1234/v1/models"`
2. Check if model is loaded in LMStudio UI
3. Restart LMStudio and reload the model

### Issue 3: llmboster plugin not active

**Symptom**: Logs show `plugin status: llmboster - inactive` or no llmboster logs at all.

**Root Cause**: Plugin not enabled in config.json or binary doesn't include llmboster support.

**Solution**:
1. Verify `"enabled": true` in plugins array for llmboster
2. Check Bifrost binary was built with llmboster: `grep -r "llmboster" /home/rasty/Documents/bifrost/transports/bifrost-http/lib/config.go`
3. Restart Bifrost after config change

### Issue 4: Empty reasoning_content from LMStudio

**Symptom**: LMStudio returns empty `reasoning_content` field.

**Root Cause**: Model doesn't support thinking mode or max_tokens is too small for reasoning.

**Solution**:
1. Try with larger max_tokens (e.g., 100 instead of 10)
2. Verify model supports reasoning (qwen3.6-35b-a3b does)
3. Check LMStudio logs for model capabilities

## Debugging Tips

### Use a Proxy to Inspect HTTP Traffic

If you need to see exactly what Bifrost sends to LMStudio:

```python
# Start a simple Python proxy on port 8888
cd /tmp && python3 << 'EOF' &
import http.server, json, urllib.request, sys

class ProxyHandler(http.server.BaseHTTPRequestHandler):
    def do_POST(self):
        content_length = int(self.headers.get('Content-Length', 0))
        post_data = self.rfile.read(content_length) if content_length > 0 else b''
        
        print(f"\n{'='*80}", flush=True)
        print(f"PROXY RECEIVED REQUEST:", flush=True)
        print(f"URL: {self.path}", flush=True)
        headers_dict = {}
        for key, value in self.headers.items():
            headers_dict[key] = value
        print(f"Headers: {json.dumps(headers_dict)}", flush=True)
        if post_data:
            try:
                body_str = post_data.decode('utf-8')[:500]
                print(f"Body (first 500 chars): {body_str}", flush=True)
            except:
                print(f"Body (binary, first 500 bytes): {post_data[:500]}", flush=True)
        print(f"{'='*80}\n", flush=True)
        
        try:
            req = urllib.request.Request(
                'http://127.0.0.1:1234' + self.path,
                data=post_data,
                headers=headers_dict,
                method='POST'
            )
            
            with urllib.request.urlopen(req, timeout=60) as response:
                response_body = response.read()
                
                print(f"\n{'='*80}", flush=True)
                print(f"PROXY RECEIVED RESPONSE FROM LMSTUDIO:", flush=True)
                print(f"Status: {response.status}", flush=True)
                try:
                    resp_json = json.loads(response_body.decode('utf-8'))
                    print(f"Response (first 1000 chars): {json.dumps(resp_json, indent=2)[:1000]}", flush=True)
                except Exception as e:
                    print(f"Response parse error: {e}", flush=True)
                    print(f"Response (binary, first 1000 bytes): {response_body[:1000]}", flush=True)
                print(f"{'='*80}\n", flush=True)
                
                self.send_response(response.status)
                for header, value in response.getheaders():
                    if header.lower() not in ['transfer-encoding', 'connection']:
                        self.send_header(header, value)
                self.end_headers()
                self.wfile.write(response_body)
        except Exception as e:
            print(f"Error forwarding to LMStudio: {e}", flush=True)
            import traceback
            traceback.print_exc()
            self.send_response(500)
            self.send_header('Content-type', 'application/json')
            self.end_headers()
            self.wfile.write(json.dumps({"error": str(e)}).encode())
    
    def log_message(self, format, *args):
        pass

server = http.server.HTTPServer(('127.0.0.1', 8888), ProxyHandler)
print("Proxy server started on port 8888", flush=True)
sys.stdout.flush()
server.serve_forever()
EOF

sleep 3

# Update Bifrost config to use proxy: "base_url": "http://127.0.0.1:8888"
# Then test through Bifrost and check proxy output for request/response details
```

### Check Bifrost Logs

```bash
# Check llmboster-related logs
grep -i "llmboster\|thinking\|reasoning" /tmp/bifrost_test.log | tail -20

# Check all debug logs for chat_completion
grep -i "chat_completion\|openai\|request.*completed" /tmp/bifrost_test.log | tail -30
```

## Success Criteria

✅ llmboster plugin initialized and active in Bifrost logs
✅ Request to LMStudio includes system prompt with thinking instructions
✅ Response from LMStudio includes `reasoning_content` field
✅ Bifrost response includes `reasoning` and `reasoning_details` fields
✅ All unit tests pass (`go test -v` in plugins/llmboster)
✅ No errors in Bifrost logs during request processing

## Notes

- The llmboster plugin modifies the system prompt to add thinking instructions
- It uses a marker `<!-- llmboster:activated -->` to track when it's active
- Reasoning content from LMStudio is mapped to Bifrost's `reasoning` field
- The plugin works with both streaming and non-streaming responses
- For streaming, llmboster accumulates chunks and replaces finish_reason "stop" with tool_calls containing think()
