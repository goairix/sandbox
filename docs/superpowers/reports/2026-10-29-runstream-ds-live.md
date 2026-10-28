# RunStream：DS 真实执行流验收（2026-10-29）

## 范围

在 `ds-ai-research/sandbox-fuse-etcd` 运行当前代码构建的独立 API Deployment `sandbox-runstream-smoke-20261029`。镜像为 `registry.i.huaxisy.com/library/ai-infra/sandbox-fuse-etcd-api:v2-runstream-739f3cd0-arm64`，推送后的 manifest digest 为 `sha256:ab9e8cb23fc3ed21b40ae4826ded4bb3611eabc6edefb79abd1b6d49906b60f2`。它的 Pod 标签不匹配业务 Service，使用独立状态 scope `sandbox-fuse-etcd/runstream-smoke-20261029-01`、独立 etcd 前缀 `/sandbox/runstream-smoke-20261029-01/`，两个预热池最小值均为 0。只通过本地端口转发请求测试 Deployment，未切换业务 Service 或镜像。

测试使用真实 Kubernetes runtime 和普通持久沙盒；执行流与 FUSE 沙盒使用同一 Kubernetes exec 路径。未在此轮测试 MinIO/FUSE 文件操作，也未测试容量。

## 结果

`sdk/go` 的 opt-in `TestLiveExecutionStream` 两次通过。第二次实际记录：

| 观察 | 结果 |
| --- | --- |
| 测试沙盒 | `sandbox-xh14lal3zv` |
| 首块 stdout | `05:25:58.044636 +08:00`，包含 `first` |
| 第二块 stderr | `05:26:00.058991 +08:00`，包含 `second`，比首块晚约 2.01 秒 |
| 终止事件 | `done(exit_code=7)`；非零进程退出未变成运行时错误 |
| 静默 `sleep 30` | HTTP 流在约 30.2 毫秒内建立，无需等待输出或心跳 |
| 主动关闭 | `Close` 后同一沙盒的公开同步执行返回 `resumed`、退出码 0 |

第一次运行也得到约 2.00 秒的两块间隔、`done(7)` 和约 33.6 毫秒的静默开流时间。真实流中 stdout/stderr 分别由独立管道读取，跨管道顺序不保证与进程写入顺序相同；子进程自身的缓冲仍需由命令负责 flush。

对照运行了现有业务 API 上的 `TestLiveEinoShell`，真实 FUSE 沙盒 `sandbox-yesarakdel` 的同步退出码 7 与 Eino 流式退出码 4 均通过。现有业务镜像仍为 `v1-paged-owner-18dd9e90-arm64`，其旧 `error` 退出码格式由适配器兼容；它不是本次新服务端语义的验收对象。

## 清理与边界

上述三个测试沙盒均通过公开 API 销毁，准确的三个 Pod 名只读查询均为空。临时 Deployment、ConfigMap 和两条本地端口转发已删除；业务 `sandbox-api` 保持 2/2 Ready，镜像未变。独立 etcd 测试前缀和推送的测试镜像 tag 暂留作验收记录，未触碰业务前缀。

本次证明逐块输出、非零退出码、静默开流和主动关闭后的执行门恢复；不证明取消时远端子进程必被立即杀死，也不覆盖高并发、大输出或容量场景。
