# Heartbeat Mechanism - Changelog

## 新增功能

### 自动心跳保活机制

为流式执行接口添加了自动心跳机制，解决长时间静默任务导致的连接超时问题。

## 变更文件

### 1. 服务端 - 事件类型定义

**文件**: `internal/runtime/types.go`

```go
const (
    StreamStdout StreamEventType = "stdout"
    StreamStderr StreamEventType = "stderr"
    StreamDone   StreamEventType = "done"
    StreamError  StreamEventType = "error"
    StreamPing   StreamEventType = "ping" // 新增：心跳保活
)
```

### 2. 服务端 - API 类型定义

**文件**: `pkg/types/exec.go`

```go
// 新增：Ping 事件数据结构
type SSEPingData struct {
    Timestamp int64 `json:"timestamp"` // Unix 时间戳（秒）
}
```

### 3. 服务端 - Exec 流式处理器

**文件**: `internal/api/handler/exec.go`

在 `ExecStream` 函数中添加心跳定时器：

```go
// 心跳定时器，防止静默期超时
heartbeatInterval := 30 * time.Second
ticker := time.NewTicker(heartbeatInterval)
defer ticker.Stop()

for {
    select {
    case <-c.Request.Context().Done():
        return
    case <-ticker.C:
        // 发送心跳 ping 事件
        pingData := types.SSEPingData{Timestamp: time.Now().Unix()}
        jsonData, _ := json.Marshal(pingData)
        fmt.Fprintf(c.Writer, "event: ping\ndata: %s\n\n", jsonData)
        if flusher != nil {
            flusher.Flush()
        }
    case event, ok := <-ch:
        // 处理正常事件
    }
}
```

### 4. 服务端 - Execute 流式处理器

**文件**: `internal/api/handler/execute.go`

在 `ExecuteOneShotStream` 函数中添加相同的心跳逻辑。

### 5. 客户端 SDK - 事件类型

**文件**: `sdk/go/types.go`

```go
const (
    SSEEventStdout SSEEventType = "stdout"
    SSEEventStderr SSEEventType = "stderr"
    SSEEventDone   SSEEventType = "done"
    SSEEventError  SSEEventType = "error"
    SSEEventPing   SSEEventType = "ping" // 新增：心跳保活
)

// SSEEvent 结构体新增字段
type SSEEvent struct {
    Type      SSEEventType
    Content   string
    ExitCode  int
    Elapsed   float64
    Timestamp int64   // 新增：Type == SSEEventPing 时设置
}
```

### 6. 客户端 SDK - 事件解析

**文件**: `sdk/go/client.go`

在 `parseSandboxSSEEvent` 函数中添加 ping 事件解析：

```go
case SSEEventPing:
    var d struct{ Timestamp int64 `json:"timestamp"` }
    if err := json.Unmarshal([]byte(data), &d); err == nil {
        return SSEEvent{Type: SSEEventPing, Timestamp: d.Timestamp}, true
    }
```

### 7. 示例代码

**新增文件**: `examples/heartbeat/main.go`

演示如何处理心跳事件的完整示例。

### 8. 文档

**新增文件**: `docs/heartbeat.md`

详细的心跳机制说明文档，包括：
- 问题背景
- 解决方案
- 工作原理
- 使用示例
- 最佳实践
- 故障排查

## 技术细节

### 心跳间隔

- **间隔**: 30 秒
- **理由**: 服务端 `WriteTimeout` 为 120 秒，30 秒是其 1/4，提供足够安全边际

### 事件格式

SSE 格式：
```
event: ping
data: {"timestamp":1714089600}

```

### 向后兼容性

- 旧版本客户端会忽略未知的 `ping` 事件类型
- 不影响现有的同步执行接口 (`/exec`)
- 仅在流式接口 (`/exec/stream`, `/execute/stream`) 中启用

## 使用场景

适用于以下长时间静默的任务：

1. **依赖安装**: `npm install`, `pip install`
2. **媒体处理**: 视频转码、音频处理
3. **数据处理**: 大文件解析、数据库迁移
4. **编译构建**: 大型项目编译链接

## 测试建议

```bash
# 运行示例
cd examples/heartbeat
export SANDBOX_API_KEY="your-api-key"
go run main.go

# 预期：在 90 秒静默期内收到 2-3 个 ping 事件
```

## 性能影响

- **网络开销**: 每 30 秒约 50 字节，可忽略不计
- **CPU 开销**: ticker 触发和 JSON 序列化，几乎无影响
- **内存开销**: 无额外内存分配

## 后续优化建议

1. **可配置间隔**: 允许通过配置文件调整心跳间隔
2. **自适应间隔**: 根据命令类型动态调整（如视频处理使用更长间隔）
3. **客户端重连**: SDK 支持自动重连和断点续传
4. **监控指标**: 添加 Prometheus 指标监控心跳发送频率

## 相关 Issue

解决了长时间静默任务导致的连接超时问题，特别是：
- npm install 安装大型依赖包
- 视频/音频处理任务
- 数据库迁移和大文件处理
