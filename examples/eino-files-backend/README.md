# Eino filesystem 对接 Sandbox Files v2

这个独立模块把 Files v2 适配成 Eino v0.9.22 的六个文件工具，并可选接入沙盒内的 `execute` 工具，不修改服务端或现有 Files v1 接口。Eino 中的 `/a.txt` 映射到沙盒内的 `/workspace/a.txt`。

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
    Instruction: "文件工具的 /a.txt 对应 execute 中的 /workspace/a.txt；命令当前目录是 /workspace。",
    Handlers: []adk.ChatModelAgentMiddleware{middleware},
})
```

要让 Eino 的 `execute` 在同一个沙盒内运行 Bash 命令，选择同步或流式模式之一：

```go
commands := einofiles.NewCommandBackend(sandboxHandle)
middleware, err := einofiles.NewDiffMiddleware(ctx, sandboxHandle.Files(),
    einofiles.WithStreamingShell(commands)) // 或 einofiles.WithShell(commands)
```

两个模式均保留原六个文件工具，额外注册 `execute`。`Shell` 汇总标准输出、标准错误和退出码；`StreamingShell` 按服务端 SSE 事件输出，并在 `done` 事件中返回退出码。当前 Kubernetes 执行流把非零退出码发成固定格式的 `error` 事件，适配器也会将这个精确格式还原为退出码；其他 `error` 仍报错。标准输出和标准错误由不同管道读取，其交错顺序以实际 SSE 到达顺序为准。空命令和后台模式被拒绝；输出最多展示 1 MiB，超出时标记截断。流缺少可识别的终止事件会报错。命令复用现有沙盒执行 API，不在运行 Eino 的进程里启动 shell。

命令的当前目录是沙盒的 `/workspace`，相对路径可直接访问文件工具创建的文件。文件工具使用虚拟根 `/`，所以它报告的绝对路径 `/a.txt` 在命令里应写成 `/workspace/a.txt`；接入 Agent 时应将这一映射写入 Agent 指令。

`NewMiddleware` 保持 Eino 标准 `edit_file` 的返回值。`NewDiffMiddleware` 只替换这个工具：Files v2 完成精确文本替换后，再读一次文件并返回观察到的 unified diff，例如：

```diff
--- /a.txt
+++ /a.txt
@@ -1,2 +1,2 @@
 alpha
-world
+etcd
```

两个 middleware 的 `ls` 和 `grep` 均使用更接近命令行的展示方式，工具名称与 Eino 原有参数保持一致：

```text
ls {"path":"/"}          → -rw-r--r-- 1 1000 1000 11 Oct 29 12:00 2026 /eino-demo.txt
grep {"pattern":"etcd"} → /eino-demo.txt:2:etcd
```

`ls` 按 `ls -alh` 的列顺序展示真实 mode、链接数、数字 UID/GID、人类可读大小、UTC 修改时间和虚拟路径；隐藏文件会列出。数字 UID/GID 不会猜测容器内用户名。`/workspace` 是沙盒根目录，因此不生成可越界的 `..` 项；文件 API 不提供分配块数，因此不伪造 `total`。旧 Files v2 服务未返回 Unix 元数据时，相应列用 `?` 表示。`grep` 默认显示匹配内容和行号，仍支持原有 `output_mode=files_with_matches|count`、`-A/-B/-C`、`-i`、`glob`、`type`、`head_limit` 和 `offset`。超出服务端搜索范围时返回不完整错误，不会把部分结果当作完整结果。单次 `grep` 输出限制为 64 KiB，可通过 `head_limit` 缩小输出。

diff 工具在编辑前最多读取 256 KiB；文件过大或不是 UTF-8 时直接拒绝编辑。返回的 diff 最多 64 KiB。编辑成功但读回失败时，会说明“已编辑、无法提供 diff”；读回内容不符合本次替换的预期时，不会声称得到可靠 diff。两次读取和一次编辑是独立 HTTP 操作，因此这个 diff 只供 Agent 展示，不能作为原子补丁或并发安全证明。标准 middleware 不受 diff 快照大小限制。

在此目录运行 `go test ./...`，即可在不连接模型或集群的情况下验证 Eino 工具注册、编辑结果，以及 SDK 到 Files v2 的 HTTP 调用。

真实集群验证可设置 `EINO_FILES_LIVE_BASE_URL`、`EINO_FILES_LIVE_API_KEY` 后运行 `go test -run '^TestLiveEino(Filesystem|Shell)$' -v -count=1`。这两个 opt-in 测试分别创建独立的 FUSE 沙盒，直接调用 Eino 文件工具或同步/流式 `execute`，并通过公开 API 删除沙盒；不要把密钥写进命令历史或测试日志。
