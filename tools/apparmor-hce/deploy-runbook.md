# AppArmor + FUSE + Helm 部署手册（运维版）

本手册面向只拿到 `tools/apparmor-hce/`、Helm chart 发布包、镜像地址和 kubeconfig 的运维人员。无需拿到项目源码。

适用范围：Kubernetes 1.29 及以上、Linux、amd64 或 arm64、containerd CRI、EulerOS/HCE 2.0。本文把 AppArmor 当作可选但必须完整验收的安全能力；不能用 `allowMissingLSMForKind: true` 绕过生产检查。

## 0. 先看清楚交付物和职责

你需要从发布方拿到以下内容：

| 物品 | 用途 |
|---|---|
| 本目录 `tools/apparmor-hce/` | 构建规格、源码锁定信息、校验工具和本手册 |
| Helm chart 包（例如 `sandbox-fuse-0.3.x.tgz`，或 chart 目录） | 执行 Helm 部署；本目录不包含项目 chart |
| `sandbox-api`、`sandbox-runtime`、`sandbox-fuse-mounter`、`sandbox-apparmor-loader` 镜像 | API、普通池、FUSE 池和 AppArmor loader |
| 对象存储参数、Redis 参数、API key Secret、镜像仓库拉取权限 | 业务配置；不要写进命令历史或日志 |
| 经过签名的生产 RPM | 生产节点安装；本文自行构建的 RPM 只能用于测试 |

运维负责：节点前提、RPM 构建/安装、containerd 配置、Helm values、Helm 升级、部署等待、API 验收和回退。生产环境的节点维护窗口、排空和 CCE 节点模板修改必须由平台管理员批准。

## 1. 准备目录和工具

在一台可访问 AppArmor 官方下载站点和 HCE 软件源的 amd64/arm64 构建节点执行。不要在业务 Pod 里构建，也不要把生产 kubeconfig、Secret 或项目源码复制进构建上下文。

```bash
set -euo pipefail

mkdir -p "$HOME/sandbox-apparmor-tools"
cd "$HOME/sandbox-apparmor-tools"
# 把发布方提供的 tools/apparmor-hce/ 原样复制到此目录
test -f apparmor-hce/rpm/sandbox-apparmor-parser.spec
test -f apparmor-hce/vendor/ax_check_compile_flag.m4
test -f apparmor-hce/source.lock
command -v rpm
command -v rpmbuild
command -v dnf
command -v gpgv
command -v sha256sum
command -v curl
command -v kubectl
command -v helm
command -v jq
```

如果构建节点没有 `rpmbuild`，按第 2 节安装依赖。`uname -m` 必须与要构建的 RPM 架构一致：`x86_64` 构建 amd64，`aarch64` 构建 arm64。不要用 amd64 RPM 安装到 arm64 节点，反之亦然。

## 2. 原生构建 AppArmor parser RPM

### 2.1 安装构建依赖

```bash
sudo dnf install -y --setopt=install_weak_deps=False \
  rpm-build gcc gcc-c++ libstdc++-static make bison flex \
  autoconf automake libtool pkgconf-pkg-config dejagnu perl \
  perl-Test-Simple perl-Pod-Checker perl-podlators python3 binutils-extra
```

### 2.2 下载并核验官方源码

源码版本由 `apparmor-hce/source.lock` 锁定，不要自行换版本。当前锁定版本为 AppArmor 4.1.7。

```bash
set -euo pipefail
cd "$HOME/sandbox-apparmor-tools"

curl --fail --location --retry 3 --proto '=https' --tlsv1.2 \
  'https://gitlab.com/apparmor/apparmor/-/archive/v4.1.7/apparmor-v4.1.7.tar.gz' \
  -o apparmor-v4.1.7.tar.gz
curl --fail --location --retry 3 --proto '=https' --tlsv1.2 \
  'https://gitlab.com/api/v4/projects/4484878/packages/generic/signatures/4.1.7/apparmor-v4.1.7.tar.gz.asc' \
  -o apparmor-v4.1.7.tar.gz.asc

echo 'ded4cd419b8a05002a108a0912208e2098695752664c6a59464d2dda418e0452  apparmor-v4.1.7.tar.gz' | sha256sum -c -
```

准备**只含公钥**的 keyring。公钥指纹必须由发布方通过第二条可信渠道确认，不能只相信网络下载结果。当前锁定指纹为：

```text
3ECDCBA5FB34D254961CC53F6689E64E3D3664BB
```

示例下载方式（下载后必须人工核对指纹）：

```bash
gpg --no-default-keyring --keyring "$PWD/apparmor-trusted-public.gpg" \
  --keyserver hkps://keyserver.ubuntu.com \
  --recv-keys 3ECDCBA5FB34D254961CC53F6689E64E3D3664BB
gpg --no-default-keyring --keyring "$PWD/apparmor-trusted-public.gpg" --fingerprint \
  3ECDCBA5FB34D254961CC53F6689E64E3D3664BB
gpgv --keyring "$PWD/apparmor-trusted-public.gpg" \
  apparmor-v4.1.7.tar.gz.asc apparmor-v4.1.7.tar.gz
```

`gpgv` 必须显示 AppArmor Development Team 的 Good signature。指纹不一致、签名失败或 SHA256 不一致时立即停止。

### 2.3 构建

下面命令必须在目标架构节点原生执行。`rpmbuild -ba` 会同时构建二进制 RPM 和 SRPM，并运行 spec 中的测试。

```bash
set -euo pipefail
cd "$HOME/sandbox-apparmor-tools"
case "$(uname -m)" in
  x86_64)  rpm_arch=x86_64; expected_arch=amd64 ;;
  aarch64) rpm_arch=aarch64; expected_arch=arm64 ;;
  *) echo "unsupported architecture: $(uname -m)" >&2; exit 1 ;;
esac

build_top="$PWD/rpmbuild-$expected_arch"
rm -rf "$build_top"
mkdir -p "$build_top"/{BUILD,BUILDROOT,RPMS,SOURCES,SPECS,SRPMS}
cp apparmor-v4.1.7.tar.gz \
   apparmor-hce/vendor/ax_check_compile_flag.m4 \
   "$build_top/SOURCES/"
cp apparmor-hce/rpm/sandbox-apparmor-parser.spec "$build_top/SPECS/"

rpmbuild -ba --define "_topdir $build_top" \
  "$build_top/SPECS/sandbox-apparmor-parser.spec"

rpm_file="$build_top/RPMS/$rpm_arch/sandbox-apparmor-parser-4.1.7-1.$rpm_arch.rpm"
test -f "$rpm_file"
sha256sum "$rpm_file"
rpm -K "$rpm_file"
rpm -qp --qf 'name=%{NAME} version=%{VERSION}-%{RELEASE} arch=%{ARCH}\n' "$rpm_file"
```

生产构建必须看到 `rpmbuild` 成功完成并运行完整测试。不要用 `rpmbuild -bb --nocheck` 生成生产包；该选项只能用于已经批准的临时测试排障。生产还必须经过内部 RPM 签名，`rpm -K` 必须显示组织的签名校验通过。

## 3. 在每个 Kubernetes 节点安装并启用 AppArmor

对所有将运行 API、普通池或 FUSE 池的 Linux 节点逐台执行。先维护一个节点，再处理下一个节点；生产必须先排空并确认 PDB、容量和业务窗口。

### 3.0 一键只读检查（安装前和安装后都执行）

先把本目录复制到节点，然后执行：

```bash
sudo ./apparmor-hce/check-node-prerequisites.sh
```

脚本只读检查以下项目：节点架构、内核 AppArmor、securityfs、宿主机 `/sbin/apparmor_parser`、parser 可执行性、containerd 服务、containerd 配置文件，以及**运行中 CRI** 的 `config.disableApparmor`。它不会安装软件、修改配置、加载 profile 或重启服务。

安装前预期可能是 `parser missing` 或 `disableApparmor=true`；这是待整改项。安装和重启 containerd 后必须再次执行，最终看到：

```text
RESULT: PASS
```

任何 `[FAIL]` 都必须先处理，不能用 Helm values 绕过。脚本退出码为 0 表示全部通过，退出码为 1 表示节点不满足条件，退出码为 2 表示命令参数错误，适合纳入节点批量巡检。

### 3.1 安装前检查

```bash
set -euo pipefail
test "$(uname -m)" = x86_64       # amd64 节点；arm64 节点改为 aarch64
test "$(cat /sys/module/apparmor/parameters/enabled)" = Y
mountpoint -q /sys/kernel/security
test -f /etc/containerd/config.toml
systemctl is-active --quiet containerd
```

如果内核返回 `N`、securityfs 不存在，或实际 CRI 不是 containerd，先停止。本文不自动安装内核模块、不切换到 SELinux、不改 kubelet。

### 3.2 安装 RPM

将对应架构、已签名的 RPM 放到节点，例如 `/var/tmp/sandbox-apparmor-parser-4.1.7-1.aarch64.rpm`：

```bash
set -euo pipefail
pkg=/var/tmp/sandbox-apparmor-parser-4.1.7-1.$(uname -m).rpm
test -f "$pkg"
rpm -K "$pkg"                 # 生产必须有组织签名
rpm -qp --qf '%{ARCH}\n' "$pkg" | grep -Fx "$(uname -m)"
sudo rpm --test -i "$pkg"
sudo rpm -i "$pkg"
sudo rpm -V sandbox-apparmor-parser
test -x /sbin/apparmor_parser
/sbin/apparmor_parser --version
```

### 3.3 配置 containerd

CCE 节点的 containerd 配置可能由节点模板或平台配置管理回写。若平台有持久化入口，必须在那里修改；只改本地文件可能在节点升级后失效。

先确认 containerd 启动参数和备份文件：

```bash
systemctl show -p ExecStart containerd
sudo cp -a /etc/containerd/config.toml \
  "/var/tmp/containerd-config.toml.$(date -u +%Y%m%dT%H%M%SZ).bak"
```

编辑 `/etc/containerd/config.toml`，在唯一的 `[plugins."io.containerd.grpc.v1.cri"]` 段设置：

```toml
disable_apparmor = false
```

不得修改 sandbox image、CNI、registry、cgroup 或 snapshotter。然后执行：

```bash
sudo containerd --config /etc/containerd/config.toml config dump >/dev/null
sudo systemctl restart containerd
sudo systemctl is-active --quiet containerd
sudo crictl info | grep -m1 '"disableApparmor": false'
kubectl get node "$(hostname)" -o wide
```

containerd 的 AppArmor 能力会在启动时缓存；不重启 containerd 不能作为通过。若 `crictl` 仍显示 `true`、字段缺失、containerd 未 active 或节点不 Ready，立即回退备份并停止部署：

```bash
sudo cp -a /var/tmp/containerd-config.toml.<时间戳>.bak /etc/containerd/config.toml
sudo systemctl restart containerd
```

## 4. 准备 Helm values

运维需要一个完整的、权限受控的 `values-production.yaml`。不要把 API key、对象存储 Secret、Redis 密码写到命令行。下面是 AppArmor/FUSE 相关的最小片段，必须合并到实际 values，不要覆盖其它业务配置：

```yaml
image:
  repository: registry.example.com/ai/sandbox-api
  tag: v0.3.34

apparmorLoader:
  enabled: true
  image:
    repository: registry.example.com/ai/sandbox-apparmor-loader
    tag: v0.3.24
    pullPolicy: IfNotPresent
  checkIntervalSeconds: 10
  parserTimeoutSeconds: 10
  startupTimeoutSeconds: 180
  resources:
    requests: {cpu: 25m, memory: 32Mi}
    limits: {cpu: 250m, memory: 128Mi}

startupProbe:
  enabled: true

config:
  runtime:
    type: kubernetes
    kubernetes:
      namespace: ""
      # 单节点测试可写 hostname；生产写经过验收的共同标签。
      nodeSelector: {}
      # CCE/VPC 无法完整自动发现时填写完整网段；两项必须同时填写。
      podCIDRs: []
      serviceCIDRs: []
      networkPolicyProvider: auto
  workspace:
    enabledMountModes: [sync, fuse]
    allowMissingLSMForKind: false
    fuseImages:
      mounter: registry.example.com/ai/sandbox-fuse-mounter:v0.3.35
  security:
    # 推荐引用已有 Secret，不在 values 明文保存。
    apiKeySecretName: sandbox-api
```

如果集群是双华云 CCE VPC，控制台给出的完整容器网段和 Service 网段应分别填入 `podCIDRs`、`serviceCIDRs`，例如 `10.0.0.0/16` 和 `10.247.0.0/16`；不要从几个 Pod IP 猜网段。Calico、Cilium 或云厂商 IPAM 的填写方式以实际插件为准。

内置 Redis Sentinel 不是 AppArmor 的必选项。首次部署可继续使用 `redis.mode: standalone`；启用 Sentinel 时必须保留 `startupProbe.enabled: true`、使用独立 bootstrap 镜像、保留身份 Secret/PVC，不要用 `--atomic` 或 Helm rollback 代替状态恢复。

## 5. 执行 Helm 部署

以下命令由运维在持有 kubeconfig 的管理机执行。chart 可以是发布方提供的目录或 `.tgz` 包；不要要求运维检出项目源码。

```bash
set -euo pipefail
export KUBECONFIG=/secure/path/cluster-kubeconfig
CHART=/secure/path/sandbox-fuse-0.3.x.tgz   # 或 /secure/path/sandbox-fuse chart 目录
VALUES=/secure/path/values-production.yaml
NAMESPACE=aiadp-sandbox-fuse
RELEASE=sandbox-fuse

kubectl version --short
helm version
helm lint "$CHART" -f "$VALUES"
helm template "$RELEASE" "$CHART" -n "$NAMESPACE" -f "$VALUES" >/var/tmp/sandbox-fuse-rendered.yaml
grep -q 'kind: DaemonSet' /var/tmp/sandbox-fuse-rendered.yaml

kubectl get namespace "$NAMESPACE" >/dev/null 2>&1 || \
  kubectl create namespace "$NAMESPACE"

helm upgrade --install "$RELEASE" "$CHART" \
  --namespace "$NAMESPACE" \
  --values "$VALUES" \
  --wait --timeout 15m
```

预升级 hook 可能先排空 API、普通池和 FUSE 池，等待时间取决于活动沙盒数量。不要在有活动任务时强制删除 hook Job，不要用 `--force`。如果使用内置 Sentinel，不要使用 `--atomic` 或 `helm rollback`；按当前 values 重新执行一次 `helm upgrade`。

## 6. 部署后验收

```bash
kubectl -n "$NAMESPACE" get deploy,ds,sts,pods -o wide
kubectl -n "$NAMESPACE" rollout status deploy/sandbox-fuse-api --timeout=10m
kubectl -n "$NAMESPACE" rollout status ds/sandbox-fuse-apparmor-loader --timeout=10m
helm -n "$NAMESPACE" status "$RELEASE"
```

必须满足：API Deployment 可用副本达到期望值；loader DaemonSet 在所有目标 Linux 节点 Ready；Redis/ Sentinel 状态与 values 一致；没有 API 启动探针失败或 `apparmor is not supported`。FUSE 空闲池的 mounter 可能显示 Kubernetes `1/2`，这是尚未授权挂载的正常状态，不能单凭该列判断失败。

### 6.1 检查实际 profile

选择一个 FUSE pool Pod：

```bash
POD=$(kubectl -n "$NAMESPACE" get pods -l sandbox.managed=true \
  -o jsonpath='{.items[0].metadata.name}')
kubectl -n "$NAMESPACE" exec "$POD" -c workspace-mounter -- \
  /bin/cat /proc/1/attr/current
```

输出必须包含项目 profile 名称并以 `(enforce)` 结尾。只看到 `RuntimeDefault`、`unconfined`、空值或 exec 失败时停止，不要把 `allowMissingLSMForKind` 改为 true。

### 6.2 API 端到端测试

先转发 API，不把 API key 打印到终端：

```bash
kubectl -n "$NAMESPACE" port-forward svc/sandbox-fuse-api 18080:8080 >/var/tmp/sandbox-api-port-forward.log 2>&1 &
PF_PID=$!
trap 'kill "$PF_PID" 2>/dev/null || true' EXIT
sleep 2

API_KEY=$(kubectl -n "$NAMESPACE" get secret sandbox-api \
  -o jsonpath='{.data.api-key}' | base64 -d)
curl --fail --silent --show-error http://127.0.0.1:18080/health
curl --fail --silent --show-error http://127.0.0.1:18080/ready

CREATE=$(curl --fail --silent --show-error -X POST \
  -H "Authorization: Bearer $API_KEY" -H 'Content-Type: application/json' \
  http://127.0.0.1:18080/api/v1/sandboxes \
  -d '{"mode":"ephemeral","timeout":180,"workspace_path":"ops-apparmor-smoke","workspace_mount_mode":"fuse"}')
echo "$CREATE" | jq -e '.id and (.workspace_mount_mode == "fuse")' >/dev/null
SANDBOX_ID=$(echo "$CREATE" | jq -r '.id')

curl --fail --silent --show-error -X DELETE \
  -H "Authorization: Bearer $API_KEY" \
  "http://127.0.0.1:18080/api/v1/sandboxes/$SANDBOX_ID"
```

生产上线验收不能只做 `/health`；至少要完成一次普通沙盒和一次 FUSE 沙盒的创建、exec 读写、workspace sync、优雅销毁，并确认对应 Pod、NetworkPolicy、挂载和 Redis 状态没有遗留。测试失败时保留 API、mounter、loader 和 kubelet/containerd 日志，不要先删除现场。

## 7. 常见故障处理

| 现象 | 处理 |
|---|---|
| `apparmor is not supported` | 检查目标节点 `/sbin/apparmor_parser`、`/sys/module/apparmor/parameters/enabled`、securityfs、`crictl info` 的 `disableApparmor`，并确认重启过 containerd。loader Ready 不能代替这些检查。 |
| API 反复启动失败，提示 FUSE pool | 先看 API 日志和目标节点 CRI，不要删除 Redis/PVC；确认 mounter 镜像包含 `/usr/local/bin/workspace-mounter` 且架构正确。 |
| loader Ready 但 mounter 为 `unconfined` | 停止发布，检查 profile digest、Pod 注解和 containerd 实际支持；不能放宽安全开关。 |
| Helm upgrade 一直 Pending/Drain | 查看 drain Job 日志和剩余 managed sandbox/NetworkPolicy；等待正常清理，不要重复创建不同 release。 |
| HPA 显示 CPU unknown | 集群没有 metrics API 时属于观测缺失，不等同于 AppArmor 失败；生产应单独部署/验收 metrics。 |
| CCE 重启后又变成 `disable_apparmor=true` | 节点配置由 CCE 模板回写，必须在平台持久化入口修改；不要只重复手工编辑本地文件。 |

## 8. 回退

### 8.1 回退 Helm

先停止新流量并按正常 drain 方式卸载/升级。不要直接删除 namespace：

```bash
helm -n "$NAMESPACE" get manifest "$RELEASE" >/var/tmp/sandbox-fuse-last-manifest.yaml
helm -n "$NAMESPACE" uninstall "$RELEASE" --wait --timeout 15m
```

如果 Helm hook 阻塞，先根据 Job 日志清理 managed sandbox 和 NetworkPolicy；只有确认不存在活动工作区和持久状态后再处理遗留 Job/PVC。Redis Sentinel 的身份 Secret/PVC 不得在未确认状态前删除。

### 8.2 回退节点

每台节点恢复之前保存的 containerd 配置并重启：

```bash
sudo cp -a /var/tmp/containerd-config.toml.<时间戳>.bak /etc/containerd/config.toml
sudo systemctl restart containerd
sudo systemctl is-active --quiet containerd
sudo crictl info | grep -m1 '"disableApparmor": true'
```

确认没有其它工作负载依赖测试 parser 后，平台管理员才可以执行 `sudo rpm -e sandbox-apparmor-parser`。生产签名 RPM 不要用 `rpm -e` 随意移除；先走变更审批。

## 9. 安全红线

- 不使用 `--force`、`--nodeps`、伪造 `/sbin/apparmor_parser` 或 `allowMissingLSMForKind: true`。
- 不在命令行、values 提交、Helm 日志或工单中公开 API key、Redis 密码、对象存储 AK/SK。
- 不把 loader 的 privileged 权限扩展为 hostPID、hostNetwork、containerd socket 或主机根目录挂载。
- 不把未签名 RPM、`--nocheck` 构建产物或单节点测试结果直接当作生产认证。
- 不使用 `helm rollback` 恢复内置 Sentinel 状态；保留身份 Secret/PVC，按目标 values 重新执行 upgrade。
