---
name: build-troubleshoot
description: Diagnose and fix Go workspace, Makefile GOWORK, and UI build issues in Bifrost. Use when make test fails with 'unknown revision', 'setup failed', or 'workspace module is replaced' errors; when make run/build fails with dependency resolution errors; or when Vite/rolldown reports parse errors in node_modules. Invoked with /build-troubleshoot <ISSUE_TYPE> or /build-troubleshoot all.
allowed-tools: Read, Grep, Glob, Bash, Edit, Write, TodoWrite
---

# Build Troubleshooting for Bifrost

Diagnose and fix Go workspace (`go.work`), Makefile `GOWORK=off`, and UI build issues in the Bifrost project.

## Common Issues & Solutions

### Issue 1: `unknown revision core/v1.7.1` / `setup failed` on `make test` or `make run`

**Symptom:**
```
go: downloading github.com/grevinden/bifrost/core v1.7.1
...
FAIL github.com/grevinden/bifrost/transports/bifrost-http [setup failed]
```

**Root Cause:** Go is trying to download published versions of local modules instead of using the workspace or replace directives. This happens when:
- `go.work` doesn't include all needed modules in `use` block, AND `replace` directives are missing from `transports/go.mod`
- `GOWORK=off` is set (disables workspace mode)

**Solution:**

1. **Fix `go.work`:** Modules with published versions (`core`, `framework`, all plugins) must be ONLY in `replace`, NOT in `use`. Only top-level modules go in `use`:
```go
use (
    ./cli
    ./tests/e2e/clis
    ./transports
)

replace (
    github.com/grevinden/bifrost/core => ./core
    github.com/grevinden/bifrost/framework => ./framework
    github.com/grevinden/bifrost/plugins/compat => ./plugins/compat
    # ... all other plugins
)
```

2. **Fix `transports/go.mod`:** Add replace directives for ALL local dependencies:
```bash
cd transports && go mod edit -replace github.com/grevinden/bifrost/core=../core
go mod edit -replace github.com/grevinden/bifrost/framework=../framework
# ... repeat for all plugins
go mod tidy
```

3. **Fix `Makefile`:** Remove `GOWORK=off` from the `test` target (line ~533):
```makefile
# Before:
@cd transports/bifrost-http && GOWORK=off gotestsum \

# After:
@cd transports/bifrost-http && gotestsum \
```

### Issue 2: `workspace module ... is replaced at all versions in the go.work file`

**Symptom:**
```
go: workspace module github.com/grevinden/bifrost/core is replaced at all versions
in the go.work file. To fix, remove the replacement from the go.work file or
specify the version at which to replace the module.
```

**Root Cause:** A module appears in BOTH `use` AND `replace` blocks of `go.work`. Go workspace doesn't allow this — if a module is in `use`, it shouldn't be in `replace`.

**Solution:** Remove modules from `replace` that are already in `use`, or vice versa. The correct pattern:
- Top-level modules (`cli`, `transports`) → `use` only
- Sub-modules with published versions (`core`, `framework`, plugins) → `replace` only (NOT in `use`)

### Issue 3: UI build fails with `[PARSE_ERROR] Unterminated multiline comment`

**Symptom:**
```
✗ Build failed in 441ms
error during build:
Build failed with 1 error:
[PARSE_ERROR] Unterminated multiline comment
    ╭─[ node_modules/react-cookie/esm/index.mjs:21:1 ]
```

**Root Cause:** A package in `node_modules` is corrupted (truncated file). This can happen due to interrupted npm install, disk issues, or network problems during download.

**Solution:**
```bash
cd ui && rm -rf node_modules package-lock.json && npm install
```

### Issue 4: `make run` builds but binary not found at `tmp/bifrost-http`

**Symptom:**
```
Built: tmp/bifrost-http (version: vdev-build)
Running bifrost-http...
bash: line 1: ./tmp/bifrost-http: No such file or directory
```

**Root Cause:** Build failed silently due to dependency resolution errors. The "Built" message is printed regardless of success/failure because it's in the same `&&` chain that doesn't check exit codes properly, OR the build was interrupted before writing the binary.

**Solution:** Check for `unknown revision` errors above the "Built" line. Fix using solutions from Issue 1.

## Verification Checklist

After applying fixes, verify:

```bash
# 1. Build succeeds
make build LOCAL=1 2>&1 | tail -5
# Expected: Built: tmp/bifrost-http (version: vdev-build)

# 2. Binary exists and is executable
ls -la tmp/bifrost-http
# Expected: ~120MB ELF binary

# 3. All tests pass
make test 2>&1 | tail -5
# Expected: DONE 1402 tests (or similar count)

# 4. Plugin unit tests pass
cd plugins/llmboster && go test -v 2>&1 | tail -5
# Expected: PASS, all tests green

# 5. Server starts and llmboster is active
./tmp/bifrost-http -app-dir tmp/app &
sleep 5
grep "llmboster" logs/*.log
# Expected: plugin status: llmboster - active
```

## Quick Diagnostic Commands

```bash
# Check go.work correctness
cat go.work | grep -A30 "^use\|^replace"

# Check if transports/go.mod has all replace directives
grep "replace.*github.com/grevinden/bifrost" transports/go.mod | wc -l
# Expected: 13 (core + framework + 11 plugins)

# Check Makefile for GOWORK=off in test target
grep -n "GOWORK=off" Makefile | grep -v "^354:\|^361:\|^371:\|^387:\|^402:\|^419:\|^592:\|^594:\|^620:"
# Expected: only line 533 should NOT have GOWORK=off (it was removed)

# Check for corrupted node_modules files
cd ui && find node_modules -name "*.mjs" -exec grep -l "OR CONS$" {} \; 2>/dev/null
# Expected: no output (files are complete)
```

## Files to Edit

| File | What to check/fix |
|------|-------------------|
| `go.work` | Modules in `use` vs `replace` — published modules only in `replace` |
| `transports/go.mod` | All 13 replace directives present for local deps |
| `Makefile` (line ~533) | Remove `GOWORK=off` from test target |
| `ui/node_modules/` | Delete and reinstall if parse errors occur |

## Notes

- The project uses a **multi-module Go workspace** (`go.work`) with local replace directives
- Published versions exist for most modules, but during development the local copies must be used
- `GOWORK=off` disables workspace mode — useful for CI releases but breaks local dev builds
- UI build uses Vite + rolldown; corrupted packages cause parse errors that look like code issues
