# Eino Shell 与 StreamingShell：DS FUSE 实测（2026-10-29）

在 `ds-ai-research/sandbox-fuse-etcd` 的现有业务 API 上，通过本地 port-forward 创建独立 FUSE 测试沙盒，直接调用 Eino middleware 的 `execute` 工具。未使用 ChatModel，未修改业务 Deployment 或镜像。

第一次测试中，同步 `printf shell-ok; exit 7` 正确展示输出和退出码；流式 `exit 4` 暴露 Kubernetes 执行流以 `error: command terminated with exit code 4` 而非 `done` 表示非零退出。该轮测试 FAIL，测试沙盒已销毁。适配器随后仅将这个精确错误格式还原为退出码，其他错误保持失败。

修复后 `TestLiveEinoShell` **PASS（3.48 秒）**。Eino 工具实际输出：

```text
Eino shell: shell-ok
[Command failed with exit code 7]

Eino streaming shell: stream-errstream-out
[Command failed with exit code 4]
```

流中标准错误先于标准输出到达，符合独立管道的实际 SSE 顺序，不代表命令原始写入顺序。测试沙盒 `sandbox-efow939x31` 与首次失败的 `sandbox-k83tfjzwr3` 均已通过公开 API 销毁，两个 Pod 均不存在。只读列出这两次测试的 MinIO workspace 前缀后，已删除其各自的空目录标记并确认前缀为空。本地 port-forward 已停止，业务 API 仍为 2/2 Ready。

本次验证证明 Eino 同步与流式 `execute` 经 SDK 在 DS FUSE 沙盒中执行，并保留非零退出码；未进行高并发、大输出、跨集群或 ChatModel 决策验收。
