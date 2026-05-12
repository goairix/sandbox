# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Project Overview

Sandbox is a secure, API-driven code execution service built in Go. It provides isolated container environments for running untrusted Python, Node.js/TypeScript, and Bash code with fine-grained resource controls and network isolation. A single sandbox container supports all languages simultaneously.

Module: `github.com/goairix/sandbox`

## Build & Run

```bash
# Build the binary
go build -o sandbox ./cmd/sandbox

# Run with config file
./sandbox -config configs/config.yaml

# Run with environment variables (prefix: SANDBOX_)
SANDBOX_RUNTIME_TYPE=docker SANDBOX_SECURITY_API_KEY=test ./sandbox

# Local development with Docker Compose
cd docker && cp .env.example .env && docker-compose up -d
```

## Testing

```bash
# Run all tests
go test ./...

# Run a single package's tests
go test ./internal/sandbox/...

# Run a specific test
go test ./internal/sandbox/... -run TestManager_CreateSandbox

# Run with verbose output
go test -v ./internal/runtime/... -run TestGlob
```

Tests use `testify` (assert/require). Mock implementations live alongside the test files (e.g., `mockRuntime` in manager_test.go).

## Architecture

```
cmd/sandbox/main.go          → Entry point, wiring
internal/
  api/                       → HTTP layer (Gin), routes, middleware (auth, rate-limit, OTEL)
  api/handler/               → Request handlers (sandbox, exec, files, workspace, skills)
  sandbox/                   → Core business logic: Manager, Pool, Session, Workspace
  runtime/                   → Runtime interface + Docker and Kubernetes implementations
  storage/                   → ScopedFS (path-safe wrapper), pluggable filesystem backends
  storage/state/redis/       → Redis-backed session persistence
  config/                    → Viper config loading (YAML + env vars)
  logger/                    → Zap structured logging with trace context
  telemetry/                 → OpenTelemetry (traces, metrics, logs)
pkg/types/                   → Public API request/response types
sdk/go/                      → Go client SDK
```

### Key Abstractions

- **Runtime** (`internal/runtime/runtime.go`): Interface over Docker and Kubernetes backends. All container operations (create, exec, file I/O) go through this.
- **Manager** (`internal/sandbox/manager.go`): Orchestrates sandbox lifecycle, pool allocation, workspace sync, and session persistence.
- **Pool** (`internal/sandbox/pool.go`): Pre-warms containers for low-latency allocation. Configurable min/max size with periodic refill.
- **ScopedFS** (`internal/storage/scoped_fs.go`): Filesystem wrapper that constrains all paths to a root directory, preventing directory traversal.

### Request Flow

HTTP Request → Gin middleware (auth, rate-limit, tracing) → Handler → Manager → Runtime (Docker/K8s)

### Dual Runtime

- **Docker**: Uses gateway sidecar for network filtering. Container operations via Docker API.
- **Kubernetes**: Uses NetworkPolicy for network isolation. Pod operations via client-go.

### Sandbox Modes

- **Ephemeral**: One-shot execution, container destroyed after use.
- **Persistent**: Long-lived sandbox with TTL, state stored in Redis, auto-restored on API restart.

## Configuration

All config via `configs/config.yaml` or environment variables with `SANDBOX_` prefix. Nested keys use underscores: `SANDBOX_STORAGE_STATE_REDIS_ADDR=redis:6379`.

## 语言

与用户交流一律使用中文。

## Code Conventions

- Dependency injection via constructors (Manager, Handler, Runtime all receive dependencies as params)
- Concurrency: `sync.RWMutex` for shared maps, channels for streaming, goroutines for background tasks (pool refill, auto-sync)
- Error handling: explicit returns, no panics in library code. Context cancellation errors are silently ignored in responses.
- Structured logging with Zap; trace context injected via `trace.Gin(c)` in handlers
- Security: constant-time API key comparison, regex validation for dependency names (`^[a-zA-Z0-9._-]+$`), seccomp profiles, read-only root FS
