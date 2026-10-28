# Eino `ls -alh`：DS FUSE 实测（2026-10-29）

在 `ds-ai-research/sandbox-fuse-etcd` 以独立 API Deployment 和独立 etcd 状态前缀运行 Files v2；业务 `sandbox-api` 镜像与 Service 未改。测试镜像固定为 API `sha256:34427cb185f0ce3b1548b79cb22b2645eae57e6cf92998adeee6d17ff0a8f4d2`、FUSE runtime `sha256:b6c2f89e9a4de0635a30ac090dc6f38ef1dee0f15e2c2aa62f413c7996e7f9d0`。Eino 工具直接调用真实 API，没有 ChatModel。

`TestLiveEinoFilesystem` 两轮通过，分别约 11.06 秒、11.30 秒。第二轮新建 FUSE 沙盒 `sandbox-xtccvv7p69`，写入 11 字节正文和 1536 字节隐藏文件，调用 Eino `ls` 得到：

```text
-rwxr-xr-x 1 1000 1000 1.5K Oct 28 20:32 2026 /.eino-hidden
-rwxr-xr-x 1 1000 1000   11 Oct 28 20:32 2026 /eino-demo.txt
```

隐藏文件可见，大小按 1024 进位显示，权限、链接数及数字 UID/GID 来自运行时 stat。时间按 UTC 输出。`read_file`、`edit_file` 的 unified diff、`glob` 和 `grep` 同轮通过。文件的 `0755` 权限是本次真实写入后观察到的状态，本次只验证 `ls` 如实呈现元数据。

两轮的公开沙盒销毁均成功，两个 Pod 均已消失。对两条准确的 MinIO 测试 workspace 前缀先列出对象，再删除并复查为空；本地 port-forward、临时 Deployment 与 ConfigMap 已删除。业务 `sandbox-api` 仍为 2/2 Ready。隔离 etcd 前缀保留，不触碰业务前缀。

本次验证覆盖真实 DS FUSE 挂载及 API 到 Eino 的元数据链路；不代表业务镜像已升级，也不代表大容量或跨集群验收。
