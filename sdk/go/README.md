# sandbox-sdk-go

Go SDK for the [Sandbox](https://github.com/goairix/sandbox) execution service.

## Installation

```bash
go get github.com/goairix/sandbox/sdk/go
```

Requires Go 1.21+.

## Quick Start

```go
import sandbox "github.com/goairix/sandbox/sdk/go"

client := sandbox.NewClient("http://localhost:8080", "your-api-key")

// One-shot execution — no sandbox management needed
result, err := client.Run(ctx, "python", `print("hello")`)
fmt.Println(result.Stdout) // hello
```

## Usage

### Client

The low-level `Client` maps 1:1 to every HTTP endpoint.

```go
client := sandbox.NewClient(baseURL, apiKey,
    sandbox.WithTimeout(30 * time.Second),
    sandbox.WithHTTPClient(customHTTPClient),
)
```

### Sandbox lifecycle

```go
sb, err := client.NewSandbox(ctx, sandbox.SandboxOptions{
    Mode:    sandbox.ModePersistent, // default: ModeEphemeral
    Timeout: 300,
    Resources: &sandbox.ResourceLimits{Memory: "256Mi", CPU: "500m"},
    Network:   &sandbox.NetworkConfig{Enabled: true, Whitelist: []string{"api.openai.com"}},
})
if err != nil {
    return err
}
defer sb.Close(ctx)
```

### Execute code

```go
result, err := sb.Run(ctx, "python", `print("hello")`)
// result.Stdout, result.Stderr, result.ExitCode, result.Duration
```

Supported languages: `python`, `nodejs`, `bash`.

### File operations

```go
// Upload
f, _ := os.Open("main.py")
err = sb.UploadFile(ctx, "/workspace/main.py", f)

// Download
rc, err := sb.DownloadFile(ctx, "/workspace/output.txt")
defer rc.Close()

// List
files, err := sb.ListFiles(ctx, "/workspace")
```

### Workspace

```go
err = sb.MountWorkspace(ctx, "/data/user123/project-a")

resp, err := sb.Sync(ctx)   // from_container → host
resp, err = sb.SyncTo(ctx)  // host → to_container

info, err := sb.WorkspaceInfo(ctx)
err = sb.UnmountWorkspace(ctx)
```

### Network

```go
err = sb.EnableNetwork(ctx, []string{"api.openai.com"})
err = sb.DisableNetwork(ctx)
```

## Error Handling

```go
result, err := client.GetSandbox(ctx, id)
if errors.Is(err, sandbox.ErrNotFound) {
    // sandbox does not exist
}

// Get full error details
var sbErr *sandbox.SandboxError
if errors.As(err, &sbErr) {
    fmt.Println(sbErr.StatusCode, sbErr.Code, sbErr.Message)
}
```

Predefined sentinels:

| Variable | Status |
|---|---|
| `ErrNotFound` | 404 `SANDBOX_NOT_FOUND` |
| `ErrUnauthorized` | 401 |
| `ErrRateLimited` | 429 |
| `ErrTimeout` | 408 |
| `ErrInvalidRequest` | 400 |

## API Reference

### Client methods

| Method | Endpoint |
|---|---|
| `CreateSandbox(ctx, req)` | POST /api/v1/sandboxes |
| `GetSandbox(ctx, id)` | GET /api/v1/sandboxes/:id |
| `DestroySandbox(ctx, id)` | DELETE /api/v1/sandboxes/:id |
| `UpdateNetwork(ctx, id, req)` | PUT /api/v1/sandboxes/:id/network |
| `Exec(ctx, id, req)` | POST /api/v1/sandboxes/:id/exec |
| `Execute(ctx, req)` | POST /api/v1/execute |
| `UploadFile(ctx, id, path, r)` | POST /api/v1/sandboxes/:id/files/upload |
| `DownloadFile(ctx, id, path)` | GET /api/v1/sandboxes/:id/files/download |
| `ListFiles(ctx, id, dir)` | GET /api/v1/sandboxes/:id/files/list |
| `MountWorkspace(ctx, id, req)` | POST /api/v1/sandboxes/:id/workspace/mount |
| `UnmountWorkspace(ctx, id)` | POST /api/v1/sandboxes/:id/workspace/unmount |
| `SyncWorkspace(ctx, id, req)` | POST /api/v1/sandboxes/:id/workspace/sync |
| `GetWorkspaceInfo(ctx, id)` | GET /api/v1/sandboxes/:id/workspace/info |

### Convenience methods on Client

| Method | Description |
|---|---|
| `NewSandbox(ctx, opts)` | Create sandbox, return high-level handle |
| `Run(ctx, language, code)` | One-shot execution via POST /api/v1/execute |

## License

MIT
