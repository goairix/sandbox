# Eino filesystem 对接 Sandbox Files v2

这个独立模块把 Files v2 适配成 Eino v0.9.22 的六个文件工具，不修改服务端或现有 Files v1 接口。Eino 中的 `/a.txt` 映射到沙盒内的 `/workspace/a.txt`。

```go
import (
    "github.com/cloudwego/eino/adk"
    einofiles "github.com/goairix/sandbox/examples/eino-files-backend"
)

// sandboxHandle 是已有的 *sandbox.Sandbox；chatModel 是业务使用的 Eino 模型。
middleware, err := einofiles.NewDiffMiddleware(ctx, sandboxHandle.Files())
if err != nil { return err }
agent, err := adk.NewChatModelAgent(ctx, &adk.ChatModelAgentConfig{
    Model: chatModel,
    Handlers: []adk.ChatModelAgentMiddleware{middleware},
})
```

`NewMiddleware` 保持 Eino 标准 `edit_file` 的返回值。`NewDiffMiddleware` 只替换这个工具：Files v2 完成精确文本替换后，再读一次文件并返回观察到的 unified diff，例如：

```diff
--- /a.txt
+++ /a.txt
@@ -1,2 +1,2 @@
 alpha
-world
+etcd
```

diff 工具在编辑前最多读取 256 KiB；文件过大或不是 UTF-8 时直接拒绝编辑。返回的 diff 最多 64 KiB。编辑成功但读回失败时，会说明“已编辑、无法提供 diff”；读回内容不符合本次替换的预期时，不会声称得到可靠 diff。两次读取和一次编辑是独立 HTTP 操作，因此这个 diff 只供 Agent 展示，不能作为原子补丁或并发安全证明。标准 middleware 不受 diff 快照大小限制。

在此目录运行 `go test ./...`，即可在不连接模型或集群的情况下验证 Eino 工具注册、编辑结果，以及 SDK 到 Files v2 的 HTTP 调用。

真实集群验证可设置 `EINO_FILES_LIVE_BASE_URL`、`EINO_FILES_LIVE_API_KEY` 后运行 `go test -run '^TestLiveEinoFilesystem$' -v -count=1`。该 opt-in 测试会创建一个有独立 workspace 的 FUSE 沙盒，直接调用 Eino 六个文件工具，并通过公开 API 删除沙盒；不要把密钥写进命令历史或测试日志。
