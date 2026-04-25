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
    Timeout: 300,                    // seconds; 0 = server default, -1 = never expire
    Resources: &sandbox.ResourceLimits{Memory: "256Mi", CPU: "500m"},
    Network:   &sandbox.NetworkConfig{Enabled: true, Whitelist: []string{"api.openai.com"}},
})
if err != nil {
    return err
}
defer sb.Close(ctx)

// Response now includes Timeout and ExpiresAt
info, _ := client.GetSandbox(ctx, sb.ID())
fmt.Println(info.Timeout)   // seconds; -1 = never expire
fmt.Println(info.ExpiresAt) // nil when timeout = -1
```

### Execute code

```go
// Synchronous
result, err := sb.Run(ctx, "python", `print("hello")`)
// result.Stdout, result.Stderr, result.ExitCode, result.Duration

// Streaming (SSE)
ch, err := sb.RunStream(ctx, "python", `
import time
for i in range(5):
    print(i)
    time.sleep(0.1)
`)
for ev := range ch {
    switch ev.Type {
    case sandbox.SSEEventStdout:
        fmt.Print(ev.Content)
    case sandbox.SSEEventStderr:
        fmt.Fprint(os.Stderr, ev.Content)
    case sandbox.SSEEventDone:
        fmt.Printf("exit %d (%.2fs)\n", ev.ExitCode, ev.Elapsed)
    case sandbox.SSEEventError:
        fmt.Println("error:", ev.Content)
    }
}
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

// List (shallow)
files, err := sb.ListFiles(ctx, "/workspace")

// List recursively with pagination
resp, err := sb.ListFilesRecursive(ctx, sandbox.ListFilesRecursiveRequest{
    Path:     "/workspace",
    MaxDepth: 3,
    Page:     1,
    PageSize: 50,
})
// resp.Files, resp.TotalCount, resp.Page, resp.PageSize

// Read lines
lines, err := sb.ReadFileLines(ctx, sandbox.ReadFileLinesRequest{
    Path:      "/workspace/main.py",
    StartLine: 10,
    EndLine:   20, // 0 = read to end of file
})
// lines.Lines, lines.TotalLines

// String replacement
err = sb.EditFile(ctx, sandbox.EditFileRequest{
    Path:       "/workspace/main.py",
    OldStr:     "hello",
    NewStr:     "world",
    ReplaceAll: true,
})

// Replace line range
err = sb.EditFileLines(ctx, sandbox.EditFileLinesRequest{
    Path:       "/workspace/main.py",
    StartLine:  5,
    EndLine:    8,   // 0 = replace to end of file
    NewContent: "# replaced\n",
})
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

### TTL

```go
// Dynamically extend or shorten sandbox lifetime (seconds, must be > 0)
resp, err := sb.UpdateTTL(ctx, 7200)
fmt.Println(resp.Timeout)   // 7200
fmt.Println(resp.ExpiresAt) // new expiration time
```

### Agent Skills

```go
// List all skills in the sandbox
list, err := sb.ListSkills(ctx)
for _, s := range list.Skills {
    fmt.Println(s.Name, "-", s.Description)
}

// Get full skill content and attached files
skill, err := sb.GetSkill(ctx, "sandbox-execute")
fmt.Println(skill.Content)
for _, f := range skill.Files {
    fmt.Println(f.Path)
}

// Read an attached file
rc, err := sb.GetSkillFile(ctx, "sandbox-execute", "scripts/execute.sh")
defer rc.Close()
```

Skills are stored at `/workspace/.agent/skills/{name}/SKILL.md` inside the sandbox.

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
| `UpdateTTL(ctx, id, req)` | PUT /api/v1/sandboxes/:id/ttl |
| `Exec(ctx, id, req)` | POST /api/v1/sandboxes/:id/exec |
| `ExecStream(ctx, id, req)` | POST /api/v1/sandboxes/:id/exec/stream |
| `Execute(ctx, req)` | POST /api/v1/execute |
| `ExecuteStream(ctx, req)` | POST /api/v1/execute/stream |
| `UploadFile(ctx, id, path, r)` | POST /api/v1/sandboxes/:id/files/upload |
| `DownloadFile(ctx, id, path)` | GET /api/v1/sandboxes/:id/files/download |
| `ListFiles(ctx, id, dir)` | GET /api/v1/sandboxes/:id/files/list |
| `ListFilesRecursive(ctx, id, req)` | POST /api/v1/sandboxes/:id/files/list-recursive |
| `ReadFileLines(ctx, id, req)` | POST /api/v1/sandboxes/:id/files/read-lines |
| `EditFile(ctx, id, req)` | POST /api/v1/sandboxes/:id/files/edit |
| `EditFileLines(ctx, id, req)` | POST /api/v1/sandboxes/:id/files/edit-lines |
| `MountWorkspace(ctx, id, req)` | POST /api/v1/sandboxes/:id/workspace/mount |
| `UnmountWorkspace(ctx, id)` | POST /api/v1/sandboxes/:id/workspace/unmount |
| `SyncWorkspace(ctx, id, req)` | POST /api/v1/sandboxes/:id/workspace/sync |
| `GetWorkspaceInfo(ctx, id)` | GET /api/v1/sandboxes/:id/workspace/info |
| `ListSkills(ctx, id)` | GET /api/v1/sandboxes/:id/skills |
| `GetSkill(ctx, id, name)` | GET /api/v1/sandboxes/:id/skills/:name |
| `GetSkillFile(ctx, id, name, path)` | GET /api/v1/sandboxes/:id/skills/:name/files/*filepath |

### Convenience methods on Client

| Method | Description |
|---|---|
| `NewSandbox(ctx, opts)` | Create sandbox, return high-level handle |
| `Run(ctx, language, code)` | One-shot execution via POST /api/v1/execute |

### Sandbox handle methods

| Method | Description |
|---|---|
| `Run(ctx, language, code)` | Execute code, return full result |
| `RunStream(ctx, language, code)` | Execute code, stream SSE events |
| `UploadFile(ctx, path, r)` | Upload file |
| `DownloadFile(ctx, path)` | Download file |
| `ListFiles(ctx, dir)` | List directory (shallow) |
| `ListFilesRecursive(ctx, req)` | List directory recursively |
| `ReadFileLines(ctx, req)` | Read line range from file |
| `EditFile(ctx, req)` | String replacement in file |
| `EditFileLines(ctx, req)` | Replace line range in file |
| `MountWorkspace(ctx, rootPath, exclude...)` | Mount workspace |
| `UnmountWorkspace(ctx)` | Unmount workspace |
| `Sync(ctx)` | Sync container → host |
| `SyncTo(ctx)` | Sync host → container |
| `WorkspaceInfo(ctx)` | Workspace status |
| `EnableNetwork(ctx, whitelist)` | Enable network |
| `DisableNetwork(ctx)` | Disable network |
| `UpdateTTL(ctx, timeoutSeconds)` | Dynamically update sandbox TTL |
| `ListSkills(ctx)` | List agent skills |
| `GetSkill(ctx, name)` | Get skill content |
| `GetSkillFile(ctx, name, path)` | Get skill file |
| `Close(ctx)` | Destroy sandbox |

## License

MIT
