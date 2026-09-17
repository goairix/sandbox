# Kubernetes DNS 准入兼容部署

## 适用范围

当前双华云 CCE 1.31 实测会额外注入 `single-request-reopen` 和 `timeout=2`。
NodeLocal 还可能把 loader 的 DNSPolicy/resolver/search 改写，导致严格启动门禁失败。
兼容方式是管理员提前声明两项 options，并排除目标 Pod 的 NodeLocal 自动注入；
不是从准入结果学习信任，也不是忽略所有 DNS 差异。

默认 `disableNodeLocalDNSInjection=false`、`dnsOptions=[]`。原 ds-ai-research 或其它
已经正常工作的 Cilium/Calico/VPC 环境不需要增加这两项；原默认 Pod 模板和池指纹不变。
Kubernetes 最低支持仍为 1.29，不因本配置提高版本下限。

本文是本地修复的部署说明，真实部署需更新后的 API 镜像；本地测试不能证明新镜像
已经在 CCE/原集群通过现场验收。此前真实 CCE 结果见 [兼容测试报告](../testing/2026-09-17-sentinel-kubernetes-129-compatibility.md)。

## 双华云环境 values 增量

将下列内容合入原环境 values 的同一个 config/runtime/kubernetes 映射，不要覆盖
其它已有 namespace、CIDR、节点选择器、Redis、存储、认证或 AppArmor 配置：

```yaml
config:
  runtime:
    kubernetes:
      disableNodeLocalDNSInjection: true
      dnsOptions:
        - name: single-request-reopen
          value: ""
        - name: timeout
          value: "2"
```

Helm 自动给 loader PodTemplate 和 API 创建的普通/FUSE 物理 Pod 添加
`node-local-dns-injection=disabled`，并使用一致 options。不需要手工创建或修改
namespace、Secret、DaemonSet、系统 webhook。API、Redis/Sentinel、bootstrap/identity
Job 自己的 DNS 不变；新 env 只供 API/drain 程序构造沙盒意图。

两项可以独立启用。options 最多两项、名称不得重复，仅支持：

- `single-request-reopen`，value 必须为 `""`。
- `timeout`，value 必须是 `"1"`..`"30"` 的规范十进制字符串，无前导零。

每项仅含 name/value 两个必需字符串。未知名称/属性、错误类型、重复选项、非法值
会拒绝；不要填入官方其它场景中的 ndots、nameservers 或 searches。本次没有提供
任意 DNSPolicy/resolver/search 配置接口。租户不能覆写已启用的 opt-out 标签。
已配置集合允许排列变化、无值开关的 nil/空表示；缺少、追加或修改值仍然拒绝。

本地配置使用 `runtime.kubernetes.disable_node_local_dns_injection` 和 `dns_options`。
对应环境变量为 `SANDBOX_RUNTIME_KUBERNETES_DISABLE_NODE_LOCAL_DNS_INJECTION`
（精确 true/false）及 `SANDBOX_RUNTIME_KUBERNETES_DNS_OPTIONS`
（JSON 数组，最多 1024 字节）。env 按字段优先于文件；YAML/JSON 新项必须显式写出，
相关结构路径/选项不能使用 alias/merge、重复属性或点号折叠路径。

## 镜像构建与 Chart 更新

本次需要重新构建并发布 **sandbox-api**，因为 Pod 构造/比较在 API 二进制中；
不要只同步 Chart 配置却沿用不识别这些环境变量的旧 API。
loader、Redis、Redis-bootstrap、mounter、sandbox-runtime 的二进制和 AppArmor profile
本次未修改，无需为了 DNS 兼容重新构建它们。

从仓库根目录执行，镜像 tag 使用新的、不可复用的版本；下列两项由发布人员填写：

```bash
SANDBOX_DNS_API_TAG='<新的镜像tag>'
SANDBOX_DNS_BUILD_PLATFORMS='linux/amd64,linux/arm64'
docker buildx build \
  --file docker/Dockerfile \
  --platform "$SANDBOX_DNS_BUILD_PLATFORMS" \
  --tag "registry.i.huaxisy.com/library/ai-infra/sandbox-api:$SANDBOX_DNS_API_TAG" \
  --push .
```

CCE 当前两个节点实际为 amd64；不能从 Kubernetes 服务端版本后缀的 arm64 推断
工作节点架构。可仅构建实际目标架构，或使用上面的多架构命令；本轮代理未运行构建/推送。
按自己现有 buildx builder 运行，不要求为了此次修复更换构建环境。

完整同步新版 `deploy/helm/sandbox`（Chart.yaml、templates、files、schema），合入上述
环境增量并更新 `image.tag` 为新 API tag。使用已有 release/namespace 和外部环境文件：

```bash
helm upgrade --install sandbox-fuse ./sandbox-fuse \
  --kubeconfig /opt/sandbox/env/cce-sandbox-fuse-kubeconfig.yaml \
  --kube-context internal \
  --namespace aiadp-sandbox-fuse \
  --values /opt/sandbox/env/values-cce.yaml
```

若当前配置仍维护在 Chart 内的 values.yaml，完整同步其它 Chart 文件、合入增量并
更新镜像后，也可沿用不带 --values 的命令；不要混用新旧 templates/schema。
上面的 kubeconfig/context 是当前 CCE 测试目标示例，请替换为发布机器上的实际路径；
其它集群使用各自的 context，不修改全局 current-context，也不要误升级原测试集群。
私有文件与渲染输出含凭据，限制权限，不在聊天或报告里输出完整清单。

改变有效兼容配置会改变普通/FUSE 池契约和 Helm backend fingerprint，现有安全
pre-upgrade drain/旧代际退休流程会执行；不要跳过 hooks 或把未确认终止的 Pod 当作
已销毁。仅更换 options 顺序不改变代际，两项保持默认值不新增指纹字段。
API 与新装/pre-delete drain 使用同一 DNS 环境生成规则。pre-upgrade drain 存在旧 API
Deployment 时仍沿用其已安装 env，先按旧配置清理旧资源，仅使用新 fingerprint 判断
是否需要 drain；不把新旧 DNS env 混在一起。inspection/drain 不等待 loader 启动，
旧 runtime 的 UID 精确终止不依赖新 DNS 意图匹配。

## 验收边界

用户更新镜像后分别复测原集群默认路径和 CCE 显式路径，确认 loader/API、普通/FUSE
池、池命中、跨 API 操作、真实 LSM、网络与存储功能。隔离测试使用统一 release
`sandbox-fuse`、随机测试 namespace，不改当前业务资源。

当前 CCE 未启用 DataPlane V2，NetworkPolicy 是否真实执行仍单列取证；DNS 修复
不代表网络隔离已经通过。当前存储仍为 MinIO，不代表 OBS 已通过。同节点 Sentinel
功能证据不代表节点级 HA；真实 Kubernetes 1.29 生命周期也不能用 1.31 结果代替。
