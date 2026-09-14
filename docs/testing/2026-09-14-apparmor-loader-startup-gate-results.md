# 2026-09-14 AppArmor 加载器启动门禁修复记录

## 现场只读诊断

目标为 `ds-ai-research` 集群、`aiadp-sandbox-fuse` 命名空间、`sandbox-fuse` release。约 21:17–21:24（Asia/Shanghai）检查时，加载器 DaemonSet 当前 generation 已被观察，5/5 Pod Ready、无重启；API 3 个副本均未 Ready，上一轮日志报：

```text
failed to create kubernetes runtime: AppArmor loader startup gate incomplete (client rate limiter Wait returned an error: context deadline exceeded): context deadline exceeded
```

Redis Sentinel StatefulSet 3/3 Ready，身份/初始化 Job 成功；resume Job 等待 API 可用。加载器已经使用此前验收的镜像 digest `sha256:5a984dcb978f6634acee4446bb2f1ae5d2aca918ee53b189372deb688bcf908c`，初始镜像拉取错误不再是当前阻塞原因。

加载器 DS 模板未设置 `spec.enableServiceLinks`，5 个实际 Pod 均为 `true`。对现场模板和 Pod 按门禁现有允许项归一化（节点名称、控制器亲和性/容忍、默认优先级和抢占策略）后，剩余的 spec 差异仅为 `enableServiceLinks: true` 与未设置。严格比较拒绝这些 Pod，最终超时。该问题不是加载器 Ready 检查失败，也不是 values 配错；超时日志中的 rate limiter 信息不是本次阻塞的根因。

## 本地修复与回归

`templates/apparmor-loader.yaml` 显式设置 `enableServiceLinks: false`。不修改 API 比较逻辑、策略内容/摘要、镜像、Redis 或 values 接口，加载器不依赖 service links 环境变量。生成的 Pod 保留显式 false，使模板和实际 Pod 契约一致。

先只新增渲染断言，旧模板的脚本及 Go Helm 测试均因缺少该字段失败；再补模板字段。回归覆盖完整 values、精简/空字段默认补齐、显式 PriorityClass，以及跨运行时命名空间渲染。门禁测试证明：未设置模板/默认 true Pod 会被拒绝；false/false 可启动；false/true 和 false/未设置仍被拒绝，不掩盖字段漂移。

以下命令均退出 0：

```sh
bash scripts/test-helm-apparmor-loader.sh
go test -tags helmtests ./internal/helmtest -count=1
go test ./internal/apparmorloader ./cmd/apparmor-loader ./internal/runtime/kubernetes \
  ./internal/config ./cmd/sandbox ./internal/mounter ./internal/fuseprotocol -count=1
go test -race ./internal/runtime/kubernetes -run TestAppArmorLoaderGate -count=1
git diff --check
```

Helm 回归、上述 7 个 Go 包及门禁竞态检查全部通过；Go 测试文件 `gofmt -d` 无差异。独立复审未发现问题，并独立重跑通过门禁四 case、目标 Helm 测试、Helm 脚本及 diff 检查。此前 [组件级真实挂载验收](2026-09-14-apparmor-live-prepared-results.md) 仍是独立历史记录，不将组件测试等同于线上 API 集成已通过。

## 部署及待验证边界

本次只修本地 Chart；未修改线上 release、Pod、Secret/PVC 或节点策略，也未重启业务组件。保持现有镜像版本与自己的 values，同步修复后的 `templates/apparmor-loader.yaml` 并重新 upgrade，无需构建镜像或新增 values 项。内置 Sentinel 不使用 `--atomic` 或 rollback，不删除身份状态来处理本问题。

用户升级后须确认 DS 当前模板及新 Pod 均为 false、API 全副本可用、resume Job 完成，并通过普通/FUSE API 创建、读写、执行、跨副本生命周期、销毁与池回补测试。当前回归仅证明本地模板契约修复，尚不宣称线上启动/全部 API 验收通过；其它准入漂移、节点/存储兼容性和性能仍须按实际环境验证。
