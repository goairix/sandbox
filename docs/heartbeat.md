# 心跳机制 (Heartbeat Mechanism)

## 概述

为了解决长时间执行且中间没有任何输出的任务（如 `npm install`、视频处理等）可能导致的连接超时问题，Sandbox 在流式执行接口中实现了自动心跳机制。

## 问题背景

在流式执行场景中，如果命令执行时间超过服务端的 `WriteTimeout`（默认 120 秒）且期间没有任何 stdout/stderr 输出，服务端会因超时而断开连接。这对于以下场景尤其明显：

- **依赖安装**: `npm install`、`pip install` 在下载大型依赖时可能长时间无输出
- **媒体处理**: 视频转码、音频处理等 CPU 密集型任务
- **数据处理**: 大文件解析、数据库迁移等 I/O 密集型任务
- **编译构建**: 大型项目编译可能在链接阶段长时间静默

## 解决方案

### 服务端实现

在 SSE 流式响应中，服务端每 30 秒自动发送一个 `ping` 事件，确保连接保持活跃：

```go
// 心跳间隔
heartbeatInterval := 30 * time.Second
ticker := time.NewTicker(heartbeatInterval)
defer ticker.Stop()

for {
    select {
    case <-ticker.C:
        // 发送心跳 ping 事件
        pingData := types.SSEPingData{Timestamp: time.Now().Unix()}
        jsonData, _ := json.Marshal(pingData)
        fmt.Fprintf(c.Writer, "event: ping\ndata: %s\n\n", jsonData)
        flusher.Flush()
    case event := <-ch:
        // 处理正常的 stdout/stderr/done/error 事件
    }
}
```

### 事件类型

新增 `ping` 事件类型：

```go
// SSE 事件类型
const (
    SSEEventStdout SSEEventType = "stdout"  // 标准输出
    SSEEventStderr SSEEventType = "stderr"  // 标准错误
    SSEEventDone   SSEEventType = "done"    // 执行完成
    SSEEventError  SSEEventType = "error"   // 执行错误
    SSEEventPing   SSEEventType = "ping"    // 心跳保活
)

// Ping 事件数据结构
type SSEPingData struct {
    Timestamp int64 `json:"timestamp"` // Unix 时间戳（秒）
}
```

### 客户端处理

Go SDK 自动解析 `ping` 事件：

```go
// SSEEvent 结构
type SSEEvent struct {
    Type      SSEEventType
    Content   string  // stdout/stderr 内容或错误消息
    ExitCode  int     // Type == SSEEventDone 时设置
    Elapsed   float64 // Type == SSEEventDone 时设置（秒）
    Timestamp int64   // Type == SSEEventPing 时设置（Unix 时间戳）
}

// 使用示例
ch, err := client.ExecStream(ctx, sandboxID, sandbox.ExecRequest{
    Language: "python",
    Code:     code,
    Timeout:  300,
})

for event := range ch {
    switch event.Type {
    case sandbox.SSEEventPing:
        // 收到心跳，连接仍然活跃
        log.Printf("Heartbeat received at %d", event.Timestamp)
    case sandbox.SSEEventStdout:
        fmt.Print(event.Content)
    case sandbox.SSEEventDone:
        fmt.Printf("Exit code: %d\n", event.ExitCode)
    }
}
```

## 工作原理

### 时序图

```
Client                  Server                  Runtime
  |                       |                       |
  |-- POST /exec/stream ->|                       |
  |                       |-- ExecStream() ------>|
  |<-- event: stdout -----|<-- stdout chunk ------|
  |                       |                       |
  |                       |  (30s 静默期)          |
  |<-- event: ping -------|  (ticker 触发)        |
  |                       |                       |
  |                       |  (继续静默)            |
  |<-- event: ping -------|  (ticker 触发)        |
  |                       |                       |
  |<-- event: stdout -----|<-- stdout chunk ------|
  |<-- event: done -------|<-- exit code ---------|
  |                       |                       |
```

### 关键特性

1. **自动触发**: 无需客户端或命令主动发送，服务端自动管理
2. **固定间隔**: 每 30 秒发送一次，远小于 120 秒的 WriteTimeout
3. **透明处理**: 客户端可以选择忽略 ping 事件，不影响正常逻辑
4. **时间戳**: 每个 ping 事件携带服务端时间戳，可用于延迟监控

## 配置参数

### 服务端配置

在 `/internal/api/server.go` 中配置超时参数：

```go
server := &http.Server{
    ReadTimeout:       30 * time.Second,
    WriteTimeout:      120 * time.Second,  // 心跳间隔应 < WriteTimeout
    ReadHeaderTimeout: 10 * time.Second,
    IdleTimeout:       120 * time.Second,
}
```

### 心跳间隔

在 `/internal/api/handler/exec.go` 和 `execute.go` 中配置：

```go
heartbeatInterval := 30 * time.Second  // 建议设置为 WriteTimeout 的 1/4
```

## 使用示例

### 示例 1: 长时间依赖安装

```go
code := `
import subprocess
import sys

print("Installing dependencies...", flush=True)

# 这个命令可能需要 5 分钟且大部分时间无输出
subprocess.run([
    "pip", "install", 
    "tensorflow", "torch", "transformers",
    "--quiet"
], check=True)

print("Installation complete!", flush=True)
`

ch, _ := client.ExecStream(ctx, sandboxID, sandbox.ExecRequest{
    Language: "python",
    Code:     code,
    Timeout:  600,  // 10 分钟超时
})

for event := range ch {
    switch event.Type {
    case sandbox.SSEEventPing:
        fmt.Print(".")  // 显示进度点
    case sandbox.SSEEventStdout:
        fmt.Println(event.Content)
    }
}
```

### 示例 2: 视频处理

```go
code := `
import subprocess
import sys

print("Processing video...", flush=True)

# FFmpeg 转码可能需要几分钟且无输出
subprocess.run([
    "ffmpeg", "-i", "input.mp4",
    "-c:v", "libx264", "-preset", "slow",
    "output.mp4",
    "-loglevel", "error"  // 只输出错误
], check=True)

print("Video processing complete!", flush=True)
`

ch, _ := client.ExecStream(ctx, sandboxID, sandbox.ExecRequest{
    Language: "bash",
    Code:     code,
    Timeout:  1800,  // 30 分钟超时
})

pingCount := 0
for event := range ch {
    if event.Type == sandbox.SSEEventPing {
        pingCount++
        fmt.Printf("\rProcessing... (%d pings received)", pingCount)
    }
}
```

## 测试

运行心跳机制测试示例：

```bash
cd examples/heartbeat
export SANDBOX_API_KEY="your-api-key"
export SANDBOX_BASE_URL="http://localhost:8080"
go run main.go
```

预期输出：

```
Created sandbox: sb_xxx

Executing long-running task with silent periods...
Watch for ping events every 30 seconds to keep connection alive

[0.1s] STDOUT: Starting long task...
[30.0s] PING #1 (timestamp: 1714089600) - connection alive
[60.0s] PING #2 (timestamp: 1714089630) - connection alive
[90.1s] STDOUT: Task completed!
[90.1s] DONE: exit_code=0, elapsed=90.05s

Total ping events received: 2
Connection remained alive throughout the silent period!
```

## 最佳实践

1. **客户端处理**: 建议在 UI 中将 ping 事件转换为进度指示器（如旋转图标、进度点）
2. **超时设置**: 命令的 `Timeout` 应大于预期执行时间，避免被运行时层面的超时中断
3. **日志记录**: 在生产环境中记录 ping 事件的频率，用于监控连接健康度
4. **错误处理**: 如果长时间未收到任何事件（包括 ping），客户端应主动断开并重试

## 技术细节

### 为什么选择 30 秒？

- **WriteTimeout 是 120 秒**: 30 秒是其 1/4，提供足够的安全边际
- **网络延迟**: 考虑到网络抖动和代理超时，30 秒是一个保守的选择
- **资源开销**: 每 30 秒一次的心跳对服务器和网络的开销可忽略不计

### 与 HTTP Keep-Alive 的区别

- **HTTP Keep-Alive**: 在 TCP 层面保持连接，但不影响应用层超时
- **SSE Ping**: 在应用层面发送数据，重置服务端的 WriteTimeout 计时器

### 兼容性

- **向后兼容**: 旧版本客户端会忽略未知的 `ping` 事件类型
- **协议标准**: 符合 SSE (Server-Sent Events) 规范
- **浏览器支持**: 可直接在浏览器中使用 `EventSource` API 接收

## 故障排查

### 问题: 仍然出现超时

**可能原因**:
- 心跳间隔 >= WriteTimeout
- 网络中间件（如 Nginx）有更短的超时设置
- 客户端 HTTP 库的超时设置过短

**解决方案**:
```go
// 客户端禁用超时（流式请求）
client := sandbox.NewClient(baseURL, apiKey, 
    sandbox.WithTimeout(0))  // 0 表示无超时
```

### 问题: 收不到 ping 事件

**检查清单**:
1. 确认使用的是流式接口 (`/exec/stream`)，而非同步接口 (`/exec`)
2. 检查服务端日志，确认 ticker 正常工作
3. 验证客户端正确解析 SSE 格式

## 相关文件

- `/internal/api/handler/exec.go` - ExecStream 处理器
- `/internal/api/handler/execute.go` - ExecuteOneShotStream 处理器
- `/internal/runtime/types.go` - StreamEventType 定义
- `/pkg/types/exec.go` - SSEPingData 定义
- `/sdk/go/types.go` - 客户端事件类型
- `/sdk/go/client.go` - SSE 解析逻辑
- `/examples/heartbeat/main.go` - 使用示例
