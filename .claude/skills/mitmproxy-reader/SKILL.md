---
name: mitmproxy-reader
description: Read HTTP traffic from mitmproxy to inspect requests/responses between Bifrost and providers. Use when debugging provider communication, verifying plugin behavior, or checking what Bifrost actually sends to upstream providers. Invoked with /mitmproxy-reader or when the user asks to check proxy traffic.
allowed-tools: Read, Grep, Glob, Bash, WebFetch
---

# mitmproxy Reader

Read HTTP traffic intercepted by mitmproxy to inspect what Bifrost sends to providers and what providers return.

## Prerequisites

- mitmproxy running with web UI on a known port (e.g., `:8081`)
- User provides the base URL with auth token

## Usage

User provides base URL:
```
http://127.0.0.1:8081/?token=ab7bee46184b1511d4b71f0b9d9cb2e6
```

## API Endpoints

### 1. List all flows (metadata)

```
GET {BASE_URL_without_query}/flows?token={TOKEN}
```

Example:
```
http://127.0.0.1:8081/flows?token=ab7bee46184b1511d4b71f0b9d9cb2e6
```

Returns JSON array of flow objects with:
- `id` — flow UUID
- `request.method`, `request.path`, `request.contentLength`
- `response.status_code`, `response.contentLength`
- `timestamp_created`
- `server_conn.peername` — target host:port

### 2. Read request body

```
GET {BASE_URL_without_query}/flows/{FLOW_ID}/request/content/Auto.json?token={TOKEN}
```

Returns the request payload (JSON auto-formatted).

Other content formats:
- `Auto.json` — pretty-printed JSON
- `Hex` — hex dump
- `Raw` — raw bytes

### 3. Read response body

```
GET {BASE_URL_without_query}/flows/{FLOW_ID}/response/content/Auto.json?token={TOKEN}
```

## Workflow

1. Ask user for base URL with token if not provided
2. Fetch `/flows?token={TOKEN}` to list all flows
3. Identify the relevant flow by method, path, timestamp
4. Fetch `/flows/{ID}/request/content/Auto.json?token={TOKEN}` for request body
5. Fetch `/flows/{ID}/response/content/Auto.json?token={TOKEN}` for response body

## Example

```bash
# List flows
curl -s "http://127.0.0.1:8081/flows?token=ab7bee46184b1511d4b71f0b9d9cb2e6" | python3 -m json.tool

# Get request body for a specific flow
curl -s "http://127.0.0.1:8081/flows/FLOW_ID/request/content/Auto.json?token=ab7bee46184b1511d4b71f0b9d9cb2e6"

# Get response body
curl -s "http://127.0.0.1:8081/flows/FLOW_ID/response/content/Auto.json?token=ab7bee46184b1511d4b71f0b9d9cb2e6"
```

## Notes

- The `/flows` endpoint returns flow metadata only — request/response bodies are NOT included
- Content must be fetched separately via `/request/content/Auto.json` or `/response/content/Auto.json`
- Token is required for all requests
- Flows are ordered by timestamp (newest first in the list)
- mitmproxy web UI must be running (port may vary — ask user)
