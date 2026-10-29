# Sandbox Helm Chart

当前 Chart：`0.4.0`；项目镜像：明确的 `v0.4.0` tag，**待从当前代码构建发布**。

正式环境步骤、资源说明、完整镜像表及验收要求见 [生产部署手册](../../../docs/deployment/helm-production.md)；直接填写 values 或引用 Secret 的两种凭据配置、无换行写入与回读校验见 [Secret 手册](../../../docs/deployment/helm-secrets.md)。

- 新安装使用 `values.yaml` 默认值 + `values-production.yaml` + Chart 外的环境 values，后加载的配置覆盖前者。生产主线为内置 etcd + FUSE；默认三个 etcd 成员、每成员独立 10Gi PVC，支持配置 StorageClass。对象存储不由 Chart 安装。
- 默认 `values.yaml` 启用内置三成员 etcd 与持久化 PVC；需要三个可调度节点和可用 StorageClass，生产再叠加生产配置。
- `values-external-etcd.yaml` 提供外部 etcd 可选入口（关闭内置集群并显式填写 endpoints），叠加在生产 overlay 后。
- `values-production-redis.yaml` 保留外部 Sentinel 示例，`values-builtin-sentinel.yaml` 提供内置 Sentinel 示例。不要把 Redis 与 etcd 两套生产 overlay 叠加。
- `values-etcd.yaml` 仅作最小接线示例；生产主线不需再叠加。
- 完整发布 Chart 目录，不混用不同提交的 templates/schema/files。所有环境地址、Secret 和节点配置都需按现场填写；镜像存在性与架构须在发布后核验。

```bash
helm lint deploy/helm/sandbox \
  -f deploy/helm/sandbox/values-production.yaml \
  -f /opt/sandbox/env/values-prod.yaml
```

此次 `values-production.yaml` 从旧的外部 Sentinel 示例改为新安装的 etcd 主线。已有 Redis release 继续使用原来的 Redis values，不以替换 overlay 的方式迁移状态。镜像从旧 tag 更新可能同时改变多个组件，升级前按生产手册评估排空和兼容性。

卸载 Chart 默认保留 etcd PVC；重装复用原 release/namespace/clusterDomain 和原卷。不要通过删除 PVC 或直接扩缩副本恢复 quorum。
