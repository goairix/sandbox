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
    // or: Network: &sandbox.NetworkConfig{Enabled: true, BlockPrivate: true}, // block RFC1918
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

// Glob pattern matching (supports **, {a,b} brace expansion, Unicode)
glob, err := sb.GlobFiles(ctx, sandbox.GlobFilesRequest{
    Path:    "/workspace",
    Pattern: "**/*.{txt,md}",
})
// glob.Files, glob.TotalCount
// More patterns: **/*.txt, *.txt, **/*keyword*.txt, **/*.{js,ts,jsx}

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

> `ReadFile` / `ReadFileLines` / `EditFile` / `EditFileLines` 在文件不存在时返回 `*SandboxError`，`errors.Is(err, sandbox.ErrFileNotFound)` 为 true。详见 [Error Handling](#error-handling)。

### Workspace

```go
err = sb.MountWorkspace(ctx, "/data/user123/project-a")

resp, err := sb.Sync(ctx)   // from_container → host
resp, err = sb.SyncTo(ctx)  // host → to_container

info, err := sb.WorkspaceInfo(ctx)
err = sb.UnmountWorkspace(ctx)
```

### Network

四种网络模式：

```go
// 模式一：隔离（默认）— 禁止所有出站流量（仅允许 DNS）
err = sb.DisableNetwork(ctx)

// 模式二：开放 — 允许所有出站流量
err = sb.EnableNetwork(ctx, nil)

// 模式三：白名单 — 仅允许访问指定目标（IP、CIDR 或域名）
err = sb.EnableNetwork(ctx, []string{"api.openai.com", "8.8.8.8"})

// 模式四：屏蔽内网 — 允许所有外网，默认屏蔽 RFC1918 私有地址段
err = sb.BlockPrivateNetwork(ctx, nil)

// 屏蔽内网 + 允许特定内网地址（internalWhitelist 中的地址仍可访问）
err = sb.BlockPrivateNetwork(ctx, []string{"10.0.1.5", "192.168.100.0/24"})
```

也可以在创建沙箱时通过 `SandboxOptions.Network` 指定初始网络模式：

```go
sb, err := client.NewSandbox(ctx, sandbox.SandboxOptions{
    Mode: sandbox.ModePersistent,
    Network: &sandbox.NetworkConfig{
        Enabled:      true,
        BlockPrivate: true,
    },
})
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

服务端在错误响应里携带 `code` 字段（机器可读）和 `message` 字段（可读说明）。SDK 把它们解析成 `*SandboxError`，可以用 `errors.Is` 与预定义 sentinel 比对：

```go
result, err := client.GetSandbox(ctx, id)
if errors.Is(err, sandbox.ErrNotFound) {
    // 沙箱不存在
}

// 文件操作可能返回两种 404：沙箱不存在 vs 文件不存在
_, err = sb.ReadFile(ctx, "/workspace/missing.txt")
switch {
case errors.Is(err, sandbox.ErrFileNotFound):
    // 文件在沙箱内不存在
case errors.Is(err, sandbox.ErrNotFound):
    // 沙箱本身已不存在
}

// 取完整错误细节
var sbErr *sandbox.SandboxError
if errors.As(err, &sbErr) {
    fmt.Println(sbErr.StatusCode, sbErr.Code, sbErr.Message)
}
```

`*SandboxError` 的 `Is` 在 sentinel 的 `Code` 非空时要求 `StatusCode` 与 `Code` 都匹配；为空时只比 `StatusCode`。所以 `ErrFileNotFound` 和 `ErrNotFound` 互不混淆。

预定义 sentinel：

| Variable | Status | Code | 触发场景 |
|---|---|---|---|
| `ErrNotFound` | 404 | `SANDBOX_NOT_FOUND` | 沙箱 ID 不存在 |
| `ErrFileNotFound` | 404 | `FILE_NOT_FOUND` | 沙箱内目标文件不存在（Read / ReadFileLines / EditFile / EditFileLines） |
| `ErrUnauthorized` | 401 | — | API key 校验失败 |
| `ErrRateLimited` | 429 | — | 限流 |
| `ErrTimeout` | 408 | — | 请求超时 |
| `ErrInvalidRequest` | 400 | — | 请求参数错误 |

服务端目前还会返回这些机器可读 code（暂未提供专用 sentinel，可通过 `*SandboxError.Code` 判断）：

| Code | Status | 含义 |
|---|---|---|
| `NO_WORKSPACE_MOUNTED` | 400 | 沙箱未挂载 workspace |
| `WORKSPACE_ALREADY_MOUNTED` | 409 | workspace 已挂载，不能重复挂载 |
| `UPLOAD_NOT_FOUND` | 404 | 分片上传 ID 不存在或已过期 |
| `UNEXPECTED_CHUNK_INDEX` | 400 | 分片顺序错误 |
| `INCOMPLETE_UPLOAD` | 400 | 分片未全部上传完就调用 Complete |

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
| `ReadFile(ctx, id, path)` | POST /api/v1/sandboxes/:id/files/read |
| `ListFilesRecursive(ctx, id, req)` | POST /api/v1/sandboxes/:id/files/list-recursive |
| `GlobFiles(ctx, id, req)` | POST /api/v1/sandboxes/:id/files/glob |
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
| `GlobFiles(ctx, req)` | Glob pattern matching files |
| `ReadFileLines(ctx, req)` | Read line range from file |
| `EditFile(ctx, req)` | String replacement in file |
| `EditFileLines(ctx, req)` | Replace line range in file |
| `MountWorkspace(ctx, rootPath, exclude...)` | Mount workspace |
| `UnmountWorkspace(ctx)` | Unmount workspace |
| `Sync(ctx)` | Sync container → host |
| `SyncTo(ctx)` | Sync host → container |
| `WorkspaceInfo(ctx)` | Workspace status |
| `EnableNetwork(ctx, whitelist)` | 开放模式（whitelist=nil）或白名单模式 |
| `BlockPrivateNetwork(ctx, internalWhitelist)` | 屏蔽内网模式，允许所有外网；internalWhitelist 中的内网地址仍可访问 |
| `DisableNetwork(ctx)` | 隔离模式，禁止所有出站流量 |
| `UpdateTTL(ctx, timeoutSeconds)` | Dynamically update sandbox TTL |
| `ListSkills(ctx)` | List agent skills |
| `GetSkill(ctx, name)` | Get skill content |
| `GetSkillFile(ctx, name, path)` | Get skill file |
| `Close(ctx)` | Destroy sandbox |

## License

MIT
