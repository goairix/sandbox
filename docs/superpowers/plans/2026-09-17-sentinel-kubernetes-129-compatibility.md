# Sentinel Kubernetes 1.29 兼容整改实施计划

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking. 本次按用户要求在当前工作区直接执行，不创建 worktree、不检查分支差异、不委派。

**Goal:** 恢复 Chart 的 Kubernetes 1.29 支持下限，并验证 CCE 1.31 单节点三成员的真实部署能力。

**Architecture:** 只修改版本门禁，原生身份 sidecar、ordinal、公私钥隔离、持久卷与生产强反亲和不变。单节点调度覆盖通过只接受固定测试 release 和随机测试 namespace 的 post-renderer 完成。实际验证分为单卷供应/挂载/回收、三成员启动与认证/ACK，成功与失败都按精确 UID 收尾。

**Tech Stack:** Helm、Go helmtests、Ruby/Psych、kubectl、CCE/Everest、Redis 7。

**执行状态（2026-09-17）：** 本地版本/安全矩阵和测试 renderer 已完成；CCE 单卷与
三成员 Sentinel 功能子集通过，正常卸载及 PV/namespace 回收完成。完整 API 部署受
AppArmor loader/DNS 准入契约阻塞，真实 1.29 生命周期未执行，两者不声明通过。
详见 [实测报告](../../testing/2026-09-17-sentinel-kubernetes-129-compatibility.md)。

**用户追加约束：** 后续测试统一使用 `sandbox-fuse` release/资源前缀，依靠独立随机
namespace 隔离，历史执行名称保留；兼容处理不得改变此前集群已验收的默认行为和安全校验。
新增第二节点后系统组件全部 Ready，差分诊断仍证明 DNS 准入不匹配；只操作隔离测试
namespace/DS，未修改生产代码。后续 DNS 兼容整改另行设计，不在版本门禁提交中混入。

---

## 文件职责

- `deploy/helm/sandbox/templates/_helpers.tpl`：唯一生产行为变更为版本下限。
- `internal/helmtest/redis_sentinel_test.go`：复用渲染和原有结构安全断言，拆开旧版本拒绝与资源预算断言。
- `internal/helmtest/redis_sentinel_version_test.go`：支持版本、低版本拒绝及低版本安全配置矩阵。
- `testdata/sentinel-kubernetes-compatibility/post-renderer.rb`：仅改变单节点隔离测试 StatefulSet 的反亲和。
- `internal/helmtest/sentinel_single_node_test.go`：post-renderer 的精确变更和失败关闭测试。
- `docs/deployment/built-in-redis-sentinel.md`、`docs/deployment/helm-deployment-upgrade.md`、`deploy/helm/sandbox/values.yaml`：同步版本及必需功能说明，不改镜像。
- `docs/testing/2026-09-17-sentinel-kubernetes-129-compatibility.md`：记录本地证据、实际 CCE 结果和残留清理，不提交私有文件。

### Task 1: 版本门禁的 RED/GREEN

- [x] 给渲染辅助函数增加显式 KubeVersion 参数；原默认调用保持 1.33。

```go
func renderChart(t *testing.T, chart string, overrides ...string) []map[string]any {
    return renderChartForKubeVersion(t, chart, "1.33.0", overrides...)
}
```

- [x] 将原 `TestSentinelNativeTopologyAndPermissions` 的断言提取到 `assertSentinelNativeTopologyAndPermissions(t, docs)`，原测试继续调用它。
- [x] 新增 `TestSentinelKubernetesVersionCompatibility`，分别用 `sentinelOverrides()` 与 `autoSentinelOverrides()` 在 1.29.0、1.30.0、1.31.0、1.31.14-r20-31.0.62.9-arm64、1.32.0、1.33.0 渲染并调用原结构安全断言。
- [x] 新增低版本拒绝测试：1.27.0、1.28.0、1.28.15-vendor 必须失败且错误含 `Kubernetes >=1.29`；新增 1.29/1.31 的持久化关闭、密码相同、认证键相同、缺失 LSM 豁免和生产 standalone 拒绝矩阵。
- [x] 原 `TestSentinelRequiresKubernetes133AndIdentityMemoryBudget` 改为只检查 identity 64Mi/256Mi 预算，版本断言由新矩阵承担。
- [x] 运行 RED：`go test -tags helmtests ./internal/helmtest -run 'SentinelKubernetesVersion|SentinelRejectsUnsupportedKubernetes|SentinelLowerVersionSafety' -count=1`。预期支持低版本因旧 >=1.33 门禁失败，而不是编译错误。
- [x] 最小修改门禁：

```gotemplate
{{- if not (semverCompare ">=1.29.0-0" .Capabilities.KubeVersion.Version) -}}{{ fail "built-in Sentinel requires Kubernetes >=1.29 with SidecarContainers and PodIndexLabel enabled" }}{{- end -}}
```

- [x] 重跑矩阵验证 GREEN，并执行 `go test -tags helmtests ./internal/helmtest -count=1`、`go test ./cmd/redis-bootstrap ./internal/redisbootstrap -count=1`。

### Task 2: 单节点测试调度覆盖

- [x] 先写 Ruby post-renderer 的进程级测试；有效输入必须只改变目标 StatefulSet 的一个反亲和字段，其余完整对象深度相等；无环境、错误 namespace、错误 release、目标缺失、重复目标和原字段不是固定强反亲和都必须失败且 stdout 为空。
- [x] RED：`go test -tags helmtests ./internal/helmtest -run SentinelSingleNodePostRenderer -count=1`，预期因 renderer 尚不存在失败。
- [x] 实现限定 `SANDBOX_SENTINEL_TEST_RELEASE=sandbox-fuse`、namespace 满足 `\Asandbox-cce-sentinel-test-[a-f0-9]{12}\z` 的 renderer；safe_load 无 aliases，找到唯一同 namespace 的 `sandbox-fuse-redis-sentinel` StatefulSet。（首次执行为旧测试名称，用户要求后经 RED/GREEN 改为统一名称。）

```ruby
anti = target.fetch('spec').fetch('template').fetch('spec').fetch('affinity').fetch('podAntiAffinity')
terms = anti.fetch('requiredDuringSchedulingIgnoredDuringExecution')
raise 'invalid term' unless terms.size == 1 && terms[0]['topologyKey'] == 'kubernetes.io/hostname'
anti.delete('requiredDuringSchedulingIgnoredDuringExecution')
anti['preferredDuringSchedulingIgnoredDuringExecution'] = [{'weight' => 100, 'podAffinityTerm' => terms[0]}]
```

- [x] renderer 所有验证先完成才输出；异常只输出固定拒绝提示，不输出含凭据的输入/exception。保持全部资源，不过滤 API 或 Job，不修改持久卷与身份字段。
- [x] GREEN：重跑 renderer 测试，完整 Helm 测试继续通过。

### Task 3: CCE 真实执行（显式目标、有界等待）

- [x] 显式使用仓库 `var/tmp/cce/cce-sandbox-fuse-kubeconfig.yaml` 的 `internal` context；复核 kube-system UID `cebeee78-9003-49bf-9442-3fea70515499`、Node UID `69a79f2b-4f61-44c7-b46a-c46230c3530c`、1.31 版本和测试 namespace 不存在。收紧私有文件权限至 0600，不修改内容。
- [x] 建立随机测试 namespace，记录 UID。申请一个 `csi-disk`、1Gi RWO 探测 PVC（供应等待最多 180 秒），非特权小型测试 Pod 挂载后只写读本次随机 token 并验证；供应/挂载失败则记录脱敏事件，不申请更多卷、不改用 hostPath/emptyDir。
- [x] 正常删除探测 Pod，再以 DeleteOptions UID 前置条件删除 PVC；核对所绑定 PV 精确 UID、Delete 回收及 PV 对象消失。回收不完成则停止，不能宣称底层云账单已终止。
- [x] 供应与回收通过后才安装隔离 release；bootstrap 镜像先核对现有私有 values 中的引用和 amd64 manifest，不构建镜像。私有 values 仅供部署，原文件与渲染清单不入报告。
- [x] 使用当前 Chart、上述 post-renderer、显式随机 namespace/runtime namespace、csi-disk/1Gi、固定三 API、关闭 HPA、ordinary 池 1/3、FUSE 池 1/2、用户提供的 Pod/Service CIDR 和随机存储子路径运行 Helm install；不改变镜像 tag、认证/身份、TLS、LSM 或 productionSafetyChecks。
- [x] 观察真实三个 StatefulSet ordinal、三个独立 PVC、identity 常驻、主从角色、三个 Sentinel/quorum、初始化 phase、认证及 ACK；`--wait --wait-for-jobs --timeout 20m` 总预算内进行，工具轮询每次不超过 30 秒并持续给用户进度。
- [x] 若数据面隔离或 API/AppArmor/FUSE 另有失败，单列证据，不把 Redis Ready 当作全功能通过；不自动扩大修复范围。
- [x] 成功或失败均先正常 Helm drain/uninstall；只处理本次隔离 namespace 的精确资源。Pod 正常终止确认后删除自身保留状态/身份/PVC，检查 PV 回收与 namespace 消失；不 force、不删除业务资源。
- [x] 本地 1.29 原生能力验证与真实 CCE 1.31 分开记录；不能把较高版本或渲染结果写成 1.29 Sentinel HA 已实测。（真实 1.29 生命周期未执行，已单列未验收。）

### Task 4: 文档、复审、提交

- [x] 部署前提改为 Linux/Kubernetes >=1.29，明确 SidecarContainers 和 PodIndexLabel 必须实际启用；离线渲染示例使用真实版本，不用 1.33 掩盖环境。注明单节点调度覆盖仅用于测试。
- [x] 保留历史预检中的旧失败，追加本次整改/真实验证链接；报告按成功、失败和未测分开，记录资源精确收尾。
- [x] 重新运行完整 Helm/bootstrap 测试、`git diff --check`，复审只有版本门禁的生产行为变更、没有新生产配置或镜像变更。
- [x] 只提交本次实现、测试和文档；私有 kubeconfig、values、认证/身份数据不提交。（DNS 兼容未实现，完整 API 阻塞和未测项保留在报告。）
