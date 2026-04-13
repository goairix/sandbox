# Sandbox SDK 设计文档

## 概述

为 Sandbox 服务实现 Go SDK，封装全部 HTTP API，提供底层 1:1 映射和高层便捷接口两层 API。目录结构按多语言预留，当前只实现 Go 版本。

---

## 目录结构

```
sdk/
├── go/
│   ├── go.mod              # module: github.com/goairix/sandbox-sdk-go
│   ├── client.go           # 底层 Client，1:1 映射所有 HTTP 端点
│   ├── sandbox.go          # 高层 Sandbox 对象
│   ├── errors.go           # SandboxError + 预定义错误变量
│   └── types.go            # SDK 专用类型（不依赖服务端 pkg/types）
└── python/                 # 目录预留，暂不实现
    └── .gitkeep
```

SDK 作为独立 Go module，不依赖服务端代码，类型自己定义，避免调用方引入服务端所有依赖。

---

## 底层 Client

### 初始化

```go
client := sandbox.NewClient("http://localhost:8080", "your-api-key")

// 可选配置（functional options）
client := sandbox.NewClient(baseURL, apiKey,
    sandbox.WithTimeout(30 * time.Second),
    sandbox.WithHTTPClient(customHTTPClient),
)
```

### 端点映射

| 方法 | HTTP 端点 |
|------|-----------|
| `CreateSandbox(ctx, req)` | POST /api/v1/sandboxes |
| `GetSandbox(ctx, id)` | GET /api/v1/sandboxes/:id |
| `DestroySandbox(ctx, id)` | DELETE /api/v1/sandboxes/:id |
| `UpdateNetwork(ctx, id, req)` | PUT /api/v1/sandboxes/:id/network |
| `Exec(ctx, id, req)` | POST /api/v1/sandboxes/:id/exec |
| `ExecStream(ctx, id, req)` | POST /api/v1/sandboxes/:id/exec/stream |
| `UploadFile(ctx, id, path, reader)` | POST /api/v1/sandboxes/:id/files/upload |
| `DownloadFile(ctx, id, path)` | GET /api/v1/sandboxes/:id/files/download |
| `ListFiles(ctx, id, dir)` | GET /api/v1/sandboxes/:id/files/list |
| `MountWorkspace(ctx, id, req)` | POST /api/v1/sandboxes/:id/workspace/mount |
| `UnmountWorkspace(ctx, id)` | POST /api/v1/sandboxes/:id/workspace/unmount |
| `SyncWorkspace(ctx, id, req)` | POST /api/v1/sandboxes/:id/workspace/sync |
| `GetWorkspaceInfo(ctx, id)` | GET /api/v1/sandboxes/:id/workspace/info |
| `Execute(ctx, req)` | POST /api/v1/execute |
| `ExecuteStream(ctx, req)` | POST /api/v1/execute/stream |

每个方法返回 `(ResponseType, error)`。`ExecStream` 和 `ExecuteStream` 优先级低，后续迭代实现。

---

## 高层 Sandbox 对象

`Sandbox` 对象内部持有 `*Client` 和沙箱 ID，封装常见工作流。

```go
// 创建沙箱
sb, err := client.NewSandbox(ctx, sandbox.SandboxOptions{
    Mode:      "ephemeral",  // 或 "persistent"
    Timeout:   300,
    Resources: &sandbox.ResourceLimits{Memory: "256Mi"},
    Network:   &sandbox.NetworkConfig{Enabled: true, Whitelist: []string{"api.openai.com"}},
})
defer sb.Close(ctx) // 自动销毁

// 执行代码
result, err := sb.Run(ctx, "python", `print("hello")`)
// result.Stdout, result.Stderr, result.ExitCode, result.Duration

// 文件操作
err = sb.UploadFile(ctx, "/workspace/main.py", reader)
reader, err := sb.DownloadFile(ctx, "/workspace/output.txt")
files, err := sb.ListFiles(ctx, "/workspace")

// 工作空间
err = sb.MountWorkspace(ctx, "user123/project-a")
err = sb.Sync(ctx)           // from_container 方向
err = sb.SyncTo(ctx)         // to_container 方向
err = sb.UnmountWorkspace(ctx)
info, err := sb.WorkspaceInfo(ctx)

// 网络
err = sb.EnableNetwork(ctx, []string{"api.openai.com"})
err = sb.DisableNetwork(ctx)

// 一次性执行（不需要预先创建沙箱）
result, err := client.Run(ctx, "python", `print(42)`)
```

---

## 错误处理

```go
type SandboxError struct {
    StatusCode int    // HTTP 状态码
    Code       string // 服务端错误码
    Message    string // 可读错误信息
}

var (
    ErrNotFound       = &SandboxError{StatusCode: 404, Code: "SANDBOX_NOT_FOUND"}
    ErrUnauthorized   = &SandboxError{StatusCode: 401}
    ErrRateLimited    = &SandboxError{StatusCode: 429}
    ErrTimeout        = &SandboxError{StatusCode: 408}
    ErrInvalidRequest = &SandboxError{StatusCode: 400}
)
```

`errors.Is()` 通过 `StatusCode` + `Code` 匹配，`errors.As()` 拿到完整错误详情。

---

## 类型定义（types.go）

SDK 自定义类型，镜像服务端 `pkg/types`，但不依赖服务端包：

- `CreateSandboxRequest` / `SandboxResponse`
- `ExecRequest` / `ExecResponse`
- `ExecuteRequest`
- `ResourceLimits` / `NetworkConfig` / `DependencySpec`
- `MountWorkspaceRequest` / `WorkspaceInfoResponse`
- `SyncWorkspaceRequest` / `SyncWorkspaceResponse`
- `FileInfo`（ListFiles 返回）
- `SSEEvent` 及子类型（streaming 用）

---

## 实现优先级

1. `types.go` — 类型定义
2. `errors.go` — 错误类型
3. `client.go` — 底层 Client（同步方法全部实现，streaming 暂跳过）
4. `sandbox.go` — 高层 Sandbox 对象
5. Streaming（`ExecStream` / `ExecuteStream`）— 后续迭代
