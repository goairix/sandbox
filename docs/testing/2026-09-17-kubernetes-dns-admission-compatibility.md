# Kubernetes DNS 准入兼容本地实施报告

## 结论与范围

2026-09-17，显式 DNS 准入兼容和错误诊断一致性修复的本地交付门禁通过。
默认 false/空列表保留普通/FUSE 构造模板、loader/API/Redis PodTemplate 及原池指纹。
仅管理员启用的选项允许排列变化和无值开关的 nil/空表示；其它 DNS、安全与身份差异
继续拒绝。最低 Kubernetes 支持版本仍为 1.29。

本轮没有访问或修改集群、读取私有 values/kubeconfig、构建/推送镜像、创建 worktree、
检查 main/分支或委派子代理。此前 CCE 根因证据见
[实测报告](2026-09-17-sentinel-kubernetes-129-compatibility.md)；本地验证不能替代现场。
配置与发布步骤见 [DNS 兼容部署说明](../deployment/kubernetes-dns-admission.md)。

## 实施内容

- config/runtime 共用最多两项的严格选项校验；类型、名称、重复、值及原文泄露负例保留。
  env 逐字段覆盖文件，JSON 最多 1024 字节；所采用 YAML/JSON 新字段的结构路径和
  options 拒绝 alias/merge、重复属性及点号折叠路径。
- 普通/FUSE 物理 Pod 固定 NodeLocal opt-out 标签和 options；标签冲突在任何写入前
  拒绝，UpdateLabels 不能删除或改为其它值。普通池 claim 保留物理标签与 CNI 身份。
- matcher/reason 共用原默认化与有限正规化，修正 Priority=0 遮蔽真实 DNS 差异的提示。
  DNSPolicy、nameservers、searches、HostAliases、安全字段及 UID 规则没有放宽。
- loader 初次、观察及最终复核均检查管理员配置；DS 模板不能作为可信 options 来源。
  原 DaemonSet owner/generation、digest、probe、volume、profile 及 Ready 门禁保留。
- 非默认配置才参与 WarmPoolContract 和 Helm backend fingerprint；默认无占位字段，
  不提高模板版本，仅改变 options 顺序不换代。Redis/Sentinel PodTemplate 不受影响。
- API 与新装/pre-delete drain 复用条件 env helper；pre-upgrade 仍复制 installed API env
  清理旧资源，用 desired fingerprint 判断变更，不混入新 DNS 配置或重复 env。
  inspection/drain 不等待 loader，旧 UID 终止不依赖新的 Pod 意图匹配。

## RED/GREEN 与自审

实施前先冻结当前工作区的公开 fixture，并复现：matcher 已拒绝 DNS 差异，reason
却报 spec.Priority。修复后同一用例准确报 spec.DNSConfig。

配置测试先暴露旧弱类型读取接受错误 bool/null/选项类型等行为；新 API 未定义导致的
编译失败只是脚手架阶段，不把它作为行为 RED 的替代。实现后严格读取、类型、重复
属性、大小写、alias/merge、JSON 尾随内容、大小边界和 env 优先级通过。
Helm 原代码忽略新增设置、未改变契约且接受非法值；补 schema/helper 后通过正负矩阵。

inline 自审补测另拦住两项边界：点号折叠配置路径不能被当作未配置忽略；即使 desired
和 actual 同时被改写，也不能接受管理员 opt-out 标签以外的值。均先复现失败后修复。
补测包含真实普通创建及超时后的模糊创建、未知准入变化后的资源补偿、普通池 claim、
独立开关/options 组合、并发自有副本、loader 模板篡改和所有配置的拒绝范围。
原终止/replacement UID、网络、AppArmor 与 FUSE 安全负向测试全部保留并跑全仓回归。

两个测试 fixture 问题只在测试中修正，没有为通过测试改生产规则：普通 readiness 的
原轮询间隔是 500ms，测试应给 1s 而不是 100ms；两个全新 Sentinel 安装会生成不同
clusterID，DNS fixture 改用固定公开身份后完整比较资源，不删除任何安全/身份字段。

## 默认基线

完整对象先 JSON 编码再做 SHA-256；这些常量在生产修改前记录，修改后测试仍相等。
API template 摘要包含其 backend fingerprint 注解。公开 fixture 不含真实密钥。

| 基线 | SHA-256 |
| --- | --- |
| 普通完整 Pod | `a8e4406774995e4bb2c4fac136d9a0fc142c82ac8d0e6aa5a9a15f2bbf2ae57e` |
| prepared FUSE 完整 Pod | `44530bbabacf0b481ad7d1317afe5637a712d2add0b15e4bc0b99554b48989cf` |
| API PodTemplate | `0741a9dbeef99a955540d7cbca279d13be71c3bb024214458030902e61f649c2` |
| loader PodTemplate | `d12941e36702f546e509e3fe066deb979b998d5cae582932f97eac9fafdba19d` |
| standalone Redis PodTemplate | `c287ff99b6ff26d718585fcafa9ba5d61754452e08c6065b30a4b0e6a4948fac` |

默认 WarmPoolContract fixture 仍为
`kubernetes-sandbox-pod/v3:control=1:runtime:cilium=false`。
Helm 省略两项与显式 false/[] 的完整公开资源相等；显式配置与 options 反序的指纹相等。
显式配置与默认配置的指纹不同，Redis/Sentinel PodTemplate 仍相等。

## 本地性能

同一开发机器，五组重复采样，不与实际 FUSE 挂载/PodReady 延迟混为一谈：

```bash
go test ./internal/runtime/kubernetes -run '^$' \
  -bench '^BenchmarkPodIntent(Default|ConfiguredDNS)$' -benchmem -count=5
```

| 路径 | ns/op 五组 | 中位数 ns/op | B/op | allocs/op |
| --- | --- | ---: | ---: | ---: |
| 修改前默认 matcher | 15649 / 15174 / 15184 / 15218 / 16489 | 15218 | 15704 | 199 |
| 修改后默认 matcher | 15834 / 15292 / 15116 / 15154 / 15161 | 15161 | 15704 | 199 |
| 显式两选项、实际排列反序 | 15983 / 15980 / 16055 / 15995 / 15949 | 15983 | 16200 | 222 |

默认分配不增加，中位数约 -0.37%，属采样波动，不宣称性能提升。
默认基准调用保留的默认 matcher wrapper，显式基准调用配置上下文入口；两者都使用
相同普通 Pod 基础 fixture。显式路径包含两项新指针及受限副本正规化，不能把全部
23 次额外分配归因于排序算法。比较/构造最多两项，不增加外部发现、DNS 查询、
Kubernetes 请求、HTTP 转发、goroutine、全局锁或模块依赖。

## 新鲜最终交付门禁

代码与自审补测最终状态下执行，以下全部 exit 0；没有放宽 lint 或安全断言：

```bash
go test ./... -count=1 -timeout=3m
go test -tags helmtests ./internal/helmtest -count=1 -timeout=3m
go test -race ./internal/kubecontract ./internal/config ./cmd/sandbox \
  ./internal/runtime/kubernetes ./internal/sandbox -count=1 -timeout=3m
go build ./...
go vet ./...
GOOS=linux GOARCH=arm64 go build ./...
golangci-lint run --timeout=3m --max-issues-per-linter=0 --max-same-issues=0
GOOS=linux GOARCH=arm64 golangci-lint run --timeout=3m \
  --max-issues-per-linter=0 --max-same-issues=0
bash scripts/test-helm-apparmor-loader.sh
git diff --check
```

全仓测试含 cmd/redis-bootstrap、Redis 状态、Docker、Kubernetes、普通/FUSE 池及现有
integration 包；最长包 redisbootstrap 59.320s，Kubernetes runtime 11.631s。
完整 Helm 测试 9.927s，覆盖 1.29、1.31、当前 CCE 服务端后缀与 1.33 渲染。
race 五包通过，无竞争报告；主机/Linux arm64 lint 均 0 issues，AppArmor 模板脚本 PASS。
构建仅编译源码，不是镜像构建或现场功能测试。

## 未验收项与下一步

1. 用户发布新的 sandbox-api 镜像并完整同步 Chart，再验证原 ds-ai-research 默认路径
   和 CCE 显式配置；loader、Redis/bootstrap、mounter/runtime 二进制无需为本修复重建。
2. 现场确认两节点 loader/API、普通/FUSE 池命中、跨 API 操作、真实 LSM、读写及销毁，
   只使用随机测试 namespace 和统一 release sandbox-fuse；最多三个 1Gi 云卷。
3. 未启用 DataPlane V2 的真实 NetworkPolicy 执行仍需单独取证；本修复不是隔离通过证明。
4. 真实 Kubernetes 1.29 生命周期、开启 DataPlane V2 的集群、OBS 和节点级 Sentinel HA
   均未由本次本地验证完成。当前 CCE 后端仍是 MinIO。
5. 现场结束后按正常 hooks uninstall，确认 Pod 消失、PVC/PV UID 与 Delete 回收并清理
   本轮 namespace；不移除 finalizer、不卸载内核 profile，不把 PV 消失表述为账单核销。
