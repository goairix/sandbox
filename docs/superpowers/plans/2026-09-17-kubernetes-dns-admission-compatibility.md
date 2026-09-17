# Kubernetes DNS 准入兼容实施计划

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 显式配置 CCE DNS 准入意图，修正错误诊断，保持原集群默认契约不变。

**Architecture:** config/runtime 共用 kubecontract DNS 类型和校验；runtime 精确构造/比较期望，loader 保留 controller 专属正规化。Helm 条件渲染配置，只有非默认值进入指纹。

**Tech Stack:** Go 1.25、client-go/corev1、Viper、yaml.v3、Helm schema/templates、testify、race/bench、golangci-lint。

**执行约束:** 用户理解说明后已确认继续，按 [书面设计](../specs/2026-09-17-kubernetes-dns-admission-compatibility-design.md) 内联执行，不再询问执行方式。无 worktree、分支检查、子代理、镜像构建、集群写入、回滚；镜像由用户发布后再验实际部署。

## 1. 冻结默认基线并复现诊断错误

文件：新增 `internal/runtime/kubernetes/dns_admission_test.go`、`pod_intent_benchmark_test.go`、`internal/helmtest/dns_admission_test.go`。

- [ ] 从当前普通/FUSE 构造器和公开 Helm fixture 记录完整 Pod、loader/API/Redis template、backend fingerprint 的确定性摘要；基线先通过，不读取私有 values 或 main。
- [ ] 新增默认 matcher 基准，改代码前连续运行五组：

```go
func BenchmarkPodIntentDefault(b *testing.B) {
    desired, err := buildOrdinaryPod("runtime", sandboxruntime.SandboxSpec{ID: "dns-benchmark", Image: "example.test/sandbox:v1"})
    if err != nil { b.Fatal(err) }
    current := desired.DeepCopy()
    current.UID = "fixed-uid"
    current.Spec.Priority = new(int32)
    b.ReportAllocs()
    b.ResetTimer()
    for b.Loop() {
        if !preparedPodIntentMatches(current, desired, false) { b.Fatal("unexpected mismatch") }
    }
}
```

- [ ] RED：真实 buildOrdinaryPod 的 current 副本带 UID、Priority=0、额外 timeout=2，matcher 拒绝但 reason 应报告 DNSConfig：

```go
current.Spec.Priority = new(int32)
two := "2"
current.Spec.DNSConfig.Options = []corev1.PodDNSConfigOption{{Name: "timeout", Value: &two}}
require.False(t, preparedPodIntentMatches(current, desired, false))
require.Equal(t, "spec.DNSConfig", preparedPodIntentMismatchReason(current, desired))
```

运行 `go test ./internal/runtime/kubernetes -run 'DNSAdmission|PodIntentMismatch' -count=1`，旧诊断预期 FAIL；`go test ./internal/runtime/kubernetes -run '^$' -bench '^BenchmarkPodIntentDefault$' -benchmem -count=5` 记录原性能。

## 2. 共享选项校验与 Config 严格读取

文件：新增 `internal/kubecontract/dns.go`/`dns_test.go`，`internal/config/kubernetes_dns.go`/`kubernetes_dns_test.go`；修改 `internal/config/config.go`。

- [ ] RED：合法空列表、两种选项、timeout 1/30、自有副本和排序；非法重复/未知/大小写/name/value 类型/开关非空/timeout 0/31/02/空；配置文件/env 增加 duplicate property、null、alias/merge、尾随 JSON、1024 字节上限、逐字段 env 优先级和 Docker 非默认拒绝。
- [ ] 定义共享 API：

```go
const NodeLocalDNSInjectionLabel = "node-local-dns-injection"
type DNSOption struct {
    Name string `json:"name" yaml:"name" mapstructure:"name"`
    Value string `json:"value" yaml:"value" mapstructure:"value"`
}
// ValidateDNSOptions returns a name-sorted, independently owned validated list.
func ValidateDNSOptions(input []DNSOption) ([]DNSOption, error)
```

空输入返回 nil；最多两项；复制、排序、重复拒绝；single-request-reopen 值为空，timeout 用 Atoi/Itoa 精确验证十进制 1..30，其它名称拒绝；错误固定，不回显输入。

- [ ] Config 新增两字段、SetDefault false/空列表：

```go
DisableNodeLocalDNSInjection bool `mapstructure:"disable_node_local_dns_injection"`
DNSOptions []kubecontract.DNSOption `mapstructure:"dns_options"`
```

读取每项先 LookupEnv；bool 仅 true/false，options 必须 1024 字节内 JSON 数组且 json.Valid；用 yaml.Node 保留属性/类型/重复信息，item 仅 name/value 两个必需字符串。文件 YAML/JSON 沿原始结构查找所采用字段，路径/选项拒绝重复属性、alias/merge。未声明新项的旧文档保持既有行为；其它 Viper 文件格式检查原始 bool/array/object，不弱转换。env 逐字段覆盖文件，仅校验实际采用字段。

- [ ] v.Unmarshal 前取得严格结果，将 Viper 的新字段设安全默认，再赋给 cfg。Validate 再调用共用校验，并拒绝 Docker 非默认值。字段路径错误不能回显任何原文。
- [ ] GREEN：`go test ./internal/kubecontract ./internal/config -count=1`，原 node selector、Redis、LSM 负例全保留。

## 3. Runtime 意图、比较、诊断与池指纹

文件：新增 `internal/runtime/kubernetes/dns_admission.go`；修改 `runtime.go`；扩展第 1 项测试。

- [ ] RED：普通/FUSE 创建显式 options/disabled 标签；tenant Label 冲突在写前拒绝；UpdateLabels 删除/改值拒绝，default 原行为不变。options 反序/nil 空开关通过，缺项/额外/重复/改值/search/resolver/policy/security 修改拒绝。
- [ ] functional option 契约：

```go
func WithDNSAdmission(disable bool, options []kubecontract.DNSOption) Option
```

在 option 中预验证排序并存错误/规范配置摘要，闭包及每个 Runtime/Pod 独立副本；New 在 loader 等待前拒绝无效值。apply helper 只改 options 和固定标签，不改 resolver/search/policy。CreateSandbox/PrepareSandbox 在任何写入前检查 spec.Labels 冲突；非法 option 不得写入资源。

- [ ] matcher 和 reason 共用深拷贝、默认化、原正规化条件，传同一 allowScheduledNodeName/DNS 期望。保留旧函数作为默认 wrapper。非空 options 时 desired/actual 必须与管理员集合一致后，仅对副本排序/规范无值开关；不清除 DNSConfig。理由只输出身份/元数据/Spec 路径，匹配时空理由。不要复用 loader 的调度规则。
- [ ] WarmPoolContract 只在非默认规范配置摘要非空时追加 `:dns-admission=<sha256>`；默认字符串不变，不 bump template version。原普通/FUSE pool contract 链不改。
- [ ] GREEN：`go test ./internal/runtime/kubernetes -count=1`；默认 bench 五组 allocs 不增加，ns/op 中位数增幅不超过 10%，否则定位整改；增加显式路径基准。

## 4. Loader 与命令 wiring

文件：修改 `internal/runtime/kubernetes/apparmor_loader_gate.go`、`cmd/sandbox/main.go`；新增 `cmd/sandbox/kubernetes_dns_options.go`/测试；扩展 runtime DNS 测试。

- [ ] RED：loader 显式 options 顺序变化通过，管理员/DS 期望不符拒绝；disabled 标签缺失、UID/owner/generation/profile/probe/volume 变化继续拒绝；原默认门禁断言不改。
- [ ] 保留默认 gate wrapper，加管理员上下文；首次 DS、观察及最终复核都验证相同期望。不从模板学习可信配置，仅复用 DNS 语义工具；其余 DaemonSet controller 比较保持原样。
- [ ] 新增 command helper，所有 normal/inspection/drain 模式传同一 DNS 配置：

```go
func kubernetesDNSOptions(cfg config.KubernetesConfig) []k8sruntime.Option {
    return []k8sruntime.Option{k8sruntime.WithDNSAdmission(cfg.DisableNodeLocalDNSInjection, cfg.DNSOptions)}
}
```

main 追加 helper；现有 kubernetesAppArmorOptions 在 inspection/drain 排除 loader 等待的规则不变。旧 Pod 终止/审计不依赖新意图匹配。

- [ ] GREEN：`go test ./cmd/sandbox ./internal/runtime/kubernetes -count=1`。

## 5. Helm schema、模板与非默认指纹

文件：修改 `deploy/helm/sandbox/values.yaml`、`values.schema.json`、`templates/_helpers.tpl`、`templates/deployment.yaml`、`templates/apparmor-loader.yaml`；扩展 `internal/helmtest/dns_admission_test.go`。

- [ ] RED：loader 标签/options；API/drain 同 env 且不重复；options 反序不改 fingerprint；默认整体摘要不变；Redis PodTemplate 不变；删除 schema 后 helper 仍拒绝非法值；1.29/1.31/CCE suffix/1.33 渲染覆盖。
- [ ] values 默认 false/[] 且中文注释；schema 新字段非 required，array maxItems 2，item name/value 必需字符串且 additionalProperties false，oneOf 钉住开关空值、timeout 正则 `^([1-9]|[12][0-9]|30)$`。
- [ ] `sandbox.dnsAdmissionConfig` helper 验证类型/字段/名称/重复/值并输出名称排序规范 JSON；默认遗漏/false/[] 等价，非默认时 backend contract 才加 dnsAdmission，原默认 dict 不变。
- [ ] `sandbox.dnsAdmissionEnv` helper 仅非默认条件输出 bool/options JSON，API 和 drainEnv 共用。loader 仅条件渲染 PodTemplate label/dnsConfig.options，不改其它 Spec。API/Redis/Sentinel/bootstrap Pod 自己的 DNS 不变，不修改 namespace/system webhook。
- [ ] GREEN：`go test -tags helmtests ./internal/helmtest -count=1 -timeout=3m`，原 API-only 更新及 Sentinel 安全/结构测试保留。

## 6. 自审、文档、完整验证与提交

文件：修改 `configs/config.yaml`、`docs/deployment/helm-deployment-upgrade.md`；新增 `docs/deployment/kubernetes-dns-admission.md`、`docs/testing/2026-09-17-kubernetes-dns-admission-compatibility.md`；更新本计划。

- [ ] 中文文档列最终双华云片段、原集群无需新配置、有限选项/安全边界，以及仓库实际 sandbox-api Dockerfile 构建命令；由用户自选 tag，不能擅自改镜像版本。
- [ ] inline 自审默认模板/指纹、所有 env 优先级、alias/merge、标签写前保护/claim 保留、模糊 create、loader template 信任边界、未知 DNS、安全负向、终止/replacement 精确 UID、drain 无 loader 依赖、并发副本。新缺陷先 RED 再修。
- [ ] 新鲜完整交付门禁，禁止放宽规则：

```bash
go test ./... -count=1 -timeout=3m
go test -tags helmtests ./internal/helmtest -count=1 -timeout=3m
go test -race ./internal/kubecontract ./internal/config ./cmd/sandbox ./internal/runtime/kubernetes ./internal/sandbox -count=1 -timeout=3m
go build ./...
go vet ./...
GOOS=linux GOARCH=arm64 go build ./...
golangci-lint run --timeout=3m --max-issues-per-linter=0 --max-same-issues=0
GOOS=linux GOARCH=arm64 golangci-lint run --timeout=3m --max-issues-per-linter=0 --max-same-issues=0
git diff --check
```

- [ ] 报告 RED/GREEN、基线摘要、默认/显式 bench、race、完整门禁；真实部署/真实1.29/DPV2隔离/OBS/节点HA未验收明确单列。仅 sandbox-api Go 行为变化，loader/Redis/bootstrap/mounter 二进制无需重建。
- [ ] 精确 stage 文件、cached diff/check、提交代码。用户更新镜像后再验原 ds-ai-research 和随机 CCE namespace（release 固定 sandbox-fuse），最多三个 1Gi 云卷，正常 hooks uninstall 和 UID 清理，收尾验证另记，不能提前宣称部署通过。
