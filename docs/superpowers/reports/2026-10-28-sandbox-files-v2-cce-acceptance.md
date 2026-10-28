# Files v2：CCE FUSE 隔离部署验收（2026-10-28）

## 范围与镜像

- 源码：`feat/etcd-state-management`，API/FUSE 文件实现至 `67b8cad0`；Helm etcd 配置修复 `a000dc5a` 在本次镜像构建之后提交。
- 集群：`var/tmp/k8s-test/sandbox-fuse-v2-kubeconfig.yaml` 的 CCE `sandbox-fuse-etcd` 命名空间。业务 `sandbox-api` Deployment 始终保留 2/2，没有切换业务 Service、池或业务镜像。
- API：`registry.i.huaxisy.com/library/ai-infra/sandbox-fuse-etcd-api:v2-files-67b8cad0-arm64`，registry manifest digest `sha256:cd2c32345ae7ae86fedd6a4f2f9d54297dbfc1621aa27ae6f45ddeb0c2de146e`。
- FUSE runtime：`registry.i.huaxisy.com/library/ai-infra/sandbox-fuse-etcd-runtime:v2-files-3407561d-arm64`，registry manifest digest `sha256:11a2cad236bc18ea6b146b838b05e640b8db3c3171352364df2b33b2fdd056bc`。测试 API 使用该 digest，避免 FUSE 后端拒绝非 semver 镜像标签。
- 隔离 API Deployment `files-v2-api-smoke-3407561d` 使用独立状态 scope、etcd 前缀 `/sandbox/cce-files-v2-smoke-20261028/`、pool 最小值 0；通过本地 port-forward `18080:8080` 访问。测试结束后已删除 Deployment、专用 ConfigMap，port-forward 已停止。测试前缀中的记录保留，没有删除 etcd 数据。

## 真实调用结果

| 检查 | 结果 |
| --- | --- |
| 新 FUSE 镜像独立 Pod | UID 1000 可执行新 `sandbox-file-op` 和固定 `rg 14.1.1`；write/read/glob/grep 成功；越界软链接读取被拒；测试 Pod 已删除。 |
| API 认证 | `/health` 200；未带 Bearer 的 v2 stat 为 401。 |
| 沙盒 `sandbox-99tkx0zu65` | 公开创建 201；v2 PUT 204、stat/list/glob/grep/GET 200，原始字节读回一致。其 180 秒 TTL 在后续 read-lines/edit 前到期，两个后续请求返回 503；公开 GET 转为 404，Pod 消失。该轮不能证明 read-lines/edit。 |
| 沙盒 `sandbox-wmf7hrre4y` | 公开创建 201；v2 PUT 204、ReadLines 第 2 行 200、Edit `world` 为 `etcd` 204、v2 GET 得到 `hello\netcd\n` 200；v1 download 同字节 200。公开 DELETE 200，随后 GET 404，Pod 消失。 |
| 跨沙盒持久性 | 对相同 workspace `files-v2-smoke-20261028/second` 的只读查询先返回 404；新建 `sandbox-t35dcoqo4h` 返回 201 且 `reused=false`，其 v2 GET 读到前一沙盒编辑后的 `hello\netcd\n`（200）。新沙盒删除 200，再次按 workspace 查询 404，测试 Pod 均不存在。此结果证明通过新 FUSE Pod 重新挂载后可读；未独立通过 MinIO S3 API 做 HEAD/GET。 |

两次完整 FUSE 创建、编辑、销毁以及跨 Pod 读回均在实际 CCE 集群执行；本地 helper 单测和进程内 HTTP 测试不能替代这项证据。构建时 `.dockerignore` 排除了约 9 GiB 的本地 SDD 记录，API Docker context 实际传输约 7.53 MB。

## 发现与修复

通用 API Dockerfile 将 `configs/config.yaml` 打入 `/etc/sandbox/config.yaml`；该示例文件显式包含 Redis 字段。隔离 API 初次使用 Helm 风格的 etcd 环境变量启动时，配置校验拒绝 Redis 与 etcd 共存，Pod 进入 CrashLoop。测试中用专用最小 etcd ConfigMap 挂载同一路径后恢复 1/1 Ready。正式修复 `a000dc5a` 使 Helm 的 etcd API 和 pre-delete drain Job 都挂载该最小 ConfigMap；Redis 渲染保持原样。`scripts/test-helm-backend-switch.sh`、Redis/etcd 两种 `helm lint`、带 mTLS Secret 的 etcd 模板渲染、`go test ./internal/config` 均通过。修复后的 Helm 模板已渲染验证，但未另起一次完整 Helm release；本轮实际 CCE API 使用的是等价的隔离 ConfigMap。

`scripts/test-helm-chart.sh` 的 shell/Helm 检查通过，随后 `internal/helmtest` 两个旧模板 SHA 基线（API、AppArmor loader）失败；本次未修改这些基线，也不把全量 Helm 套件记为通过。

## 未覆盖

- 普通非 FUSE sandbox 镜像尚未完成本次完整构建验收；本次新镜像实测的是 ARM64 FUSE 路径。
- 较早的七包 race 构建因本机 ENOSPC 失败；清理 24 小时前未使用的 Docker build cache 后，在 macOS 主机重新运行 `go test -race -p 2 ./internal/filev2 ./internal/runtime ./internal/runtime/docker ./internal/runtime/kubernetes ./internal/sandbox ./internal/api/handler ./internal/api`，七包全部通过。核心七包非 race 测试与 vet、SDK 与独立 Eino 示例的测试/vet 也通过；这不是 Linux 或整个仓库的 race 验收。
- 尚未在 DS 部署新 API；CCE 仅隔离验证。未做生产流量切换、容量或长期故障恢复测试，也未直接核验 MinIO 对象 HEAD。
- v2 missing sandbox 的 stat 返回 503 `FILE_OUTCOME_UNKNOWN`；本次未改变状态管理器的 fail-closed 语义。

本次未读取、复制或提交 API key、etcd 客户端私钥及 MinIO 凭据。
