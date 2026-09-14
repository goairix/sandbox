# AppArmor 可选配置默认值回归

## 问题与范围

部署者保留自己的 Chart `values.yaml`，只配置 `apparmorLoader.enabled: false` 时，原 schema 仍要求 image、resources 和三种超时，导致安装在创建任何资源前失败。缺少 schema 时，启用加载器的最小配置又会触发 `nil pointer ... image.repository`。

本次仅修复本地 Chart 和离线测试，不修改运行时代码、安全策略、线上资源或镜像。沿用已有可选加载器设计；不把离线渲染通过当作真实 AppArmor enforcement 验收通过。

## 修复

- `sandbox.apparmorLoaderConfig` 统一解析有效配置，所有相关模板消费者使用同一结果，不修改 `.Values`。
- 配置省略、空映射或 `null` 时默认关闭；缺失/null 字段取既有 Chart 默认值。镜像默认仍为原 `values.yaml` 的 repository/tag/pullPolicy，启用前仍建议显式填写已构建的可信镜像。
- 资源请求保持 `25m/32Mi`，上限 `250m/128Mi`；check/parser/startup 默认分别为 10/10/180 秒。显式资源映射不合入额外默认 limits，显式镜像和时间保留。
- schema 不再强制填写可默认字段，允许默认项 null，保留其它类型、范围、DNS 名称和未知属性限制。无 schema 的模板路径也拒绝非法布尔类型、空镜像字段、错误 pullPolicy、非整数/0/越界时间和非映射资源。
- 原 Kubernetes/FUSE/禁止 allowMissingLSM 校验、策略摘要、Linux 调度、API 门禁和最小权限保持不变。

## RED/GREEN 与验证

先新增回归并运行：带 schema 的 disabled 最小配置复现缺少五种属性，无 schema 的 enabled 最小配置复现 image.repository nil pointer；随后实现有效配置和 schema 修复。

以下离线检查通过：

```sh
go test -p 2 ./... -count=1
go test -tags=helmtests ./internal/helmtest ./test/integration/helm -count=1
go vet -tags=helmtests ./internal/helmtest ./test/integration/helm
bash scripts/test-helm-chart.sh
HELM_BACKEND_SWITCH_RUN_CLUSTER=0 bash scripts/test-helm-backend-switch.sh
bash scripts/test-helm-sentinel-auth.sh
bash scripts/test-helm-apparmor-loader.sh
git diff --check
```

新增回归覆盖带/不带 schema 的 disabled、omitted、null、empty、enabled、null 字段和 partial image；验证完整默认 DaemonSet 与原 `values.yaml` 默认渲染一致；验证显式设置保真以及 23 种非法配置。Sentinel 默认回归与这些最小配置组合验证，避免再次引入上轮缺失默认值错误。

独立只读代码复审未发现 Critical/Important/Minor 问题，重跑两套 Helm Go 测试通过，并在 Helm v3.11.1 下验证内置 standalone、外部 standalone/sentinel/cluster 的 null、最小关闭和启用但默认字段 null 共 12 组离线渲染通过。未访问或修改集群。

## 部署交接

同步完整 `templates/` 和 `values.schema.json`，保留线上 `values.yaml`，本次不需要重建镜像。加载器关闭只需 `apparmorLoader.enabled: false`；不启用 privileged 加载器，也不自动更改线上 AppArmor 开关。部署后的启动及实际运行验收另行执行。
