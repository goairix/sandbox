# etcd + FUSE 全新部署

当前版本已统一到 [Helm 正式环境部署手册（0.4.0）](helm-production.md)。项目镜像均使用明确的 `v0.4.0` tag，**待从当前代码构建、推送与验收**。旧版文档中的 DS/CCE 测试 tag、纯 digest 和现场 namespace 不再作为新安装示例。

生产默认随 Chart 安装内置三成员 etcd，每成员独立 10Gi PVC，StorageClass 可配置；至少需要三个可调度节点。生产安装使用完整的 `deploy/helm/sandbox/`，叠加 `values-production.yaml` 和 Chart 外的环境 values。最小 `values-etcd.yaml` 仍用于接线演示，不需要与生产 overlay 同时加载。

```bash
helm lint deploy/helm/sandbox \
  -f deploy/helm/sandbox/values-production.yaml \
  -f /opt/sandbox/env/values-prod.yaml
```

在执行安装前，按生产手册完成镜像发布、内置 etcd 的 StorageClass/PVC 与外部对象存储准备、Secret、节点 CRI/AppArmor/FUSE 实际验收，以及环境端点、节点标签和专属 prefix 配置。该命令仅验证渲染，不能代替这些前提。

本流程仅适用于全新独立安装，不迁移 Redis 数据或接管原沙盒。历史恢复步骤、灰度测试范围与镜像摘要作为审计证据保留在 [etcd 状态容量与发布验收记录](../superpowers/reports/2026-10-24-etcd-capacity-and-release.md)，不构成当前镜像已发布或大容量已通过的结论。
