# Kubernetes metadata prefix 设计

## 背景

当前 Helm 模板和 Go 代码使用 `sandbox.huaxisy.com` 作为 Kubernetes annotation 与
finalizer 的限定名前缀。该前缀来自特定部署环境，但仓库的公共项目身份是
`github.com/goairix/sandbox`，因此不应继续把环境所属域名写入项目协议。

Kubernetes metadata key 只能包含一个分隔前缀和名称的 `/`。因此仓库地址
`github.com/goairix/sandbox` 不能原样作为 annotation key 或 finalizer。自定义 finalizer
还必须使用公开限定名称，不能退化成无前缀的 `sandbox`。

## 决策

使用 GitHub 组织的 Pages 命名空间作为 DNS 前缀，并把项目名放在 key 名称中：

| 用途 | 新名称 |
|---|---|
| backend fingerprint annotation | `goairix.github.io/sandbox-backend-fingerprint` |
| drain protocol annotation | `goairix.github.io/sandbox-drain-protocol` |
| cleanup protocol annotation | `goairix.github.io/sandbox-cleanup-protocol` |
| FUSE runtime cleanup finalizer | `goairix.github.io/sandbox-fuse-runtime-cleanup` |

这些名称符合 Kubernetes qualified name 结构：左侧是 DNS 子域形式的前缀，右侧是长度不超过
63 个字符的名称。annotation 与 finalizer 使用同一命名规则，避免出现多套所有者标识。

## 部署边界

本次变更以全新 Helm 安装为边界，不实现 `sandbox.huaxisy.com/*` 的双读、双写或旧 finalizer
迁移。已部署的测试 release 先卸载并清理，再使用包含新命名的 Chart 重新安装。

现有 backend/cleanup protocol 的 upgrade drain 机制继续保留，用于采用新命名后的后续版本
升级；本次仅不为旧 metadata key 增加兼容分支。

代码不得保留旧前缀作为 fallback。这样可以避免把一次性的环境迁移逻辑固化为长期协议，也
避免 Helm 模板和 Go runtime 对同一字段使用不同名称。

## 实现范围

- 替换 Go drain 逻辑中的三个 annotation 常量；
- 替换 Kubernetes FUSE Pod cleanup finalizer 常量；
- 替换 Helm Deployment、fingerprint ConfigMap 和 upgrade/rollback hook 中的 annotation；
- 更新 Helm 渲染测试、Go 单元测试、部署文档和原 FUSE cleanup 设计文档；
- 搜索仓库，确保运行时代码、模板和有效测试中不存在 `sandbox.huaxisy.com`。

不把 prefix 暴露为 `values.yaml` 配置项。它是控制器与资源之间的稳定协议标识，允许每次安装
任意改变会使控制器无法识别自己创建的资源。

## 验收标准

1. Helm 渲染结果只包含 `goairix.github.io/sandbox-*` metadata key；
2. 新建 FUSE Pod 携带 `goairix.github.io/sandbox-fuse-runtime-cleanup` finalizer；
3. drain、upgrade 和 rollback guard 从新 annotation key 读取协议值；
4. Go 测试、Helm 测试、`go vet ./...` 和 `go test ./...` 全部通过；
5. 除历史说明外，仓库不再包含 `sandbox.huaxisy.com`。
