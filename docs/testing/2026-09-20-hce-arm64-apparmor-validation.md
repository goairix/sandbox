# HCE 2.0 arm64 节点 AppArmor 原生构建与验证

日期：2026-09-20。范围仅为双华云 CCE 测试集群节点 `172.16.30.91`（`aarch64`）和 `aiadp-sandbox-fuse` 测试 release；不是生产环境验收或生产安装授权。

## 原因与现场整改

该节点内核 AppArmor 为 `Y`，加载器 DaemonSet 已 Ready，项目 profile 已在内核中处于 enforce，但新 FUSE Pod 报 `failed to generate apparmor spec opts: apparmor is not supported`。与已通过的 x86 节点对比，arm64 宿主机缺少 `/sbin/apparmor_parser`，且 `/etc/containerd/config.toml` 中 `disable_apparmor = true`。containerd 1.7.29 的 CRI host 检测同时检查配置、宿主机 parser、securityfs 和模块状态，并缓存能力结果；仅在加载器容器内安装 parser 不满足 CRI 前提。

在该 arm64 **节点原生**构建：从 AppArmor 官方 GitLab 获取 `v4.1.7` 源码，SHA-256 为 `ded4cd419b8a05002a108a0912208e2098695752664c6a59464d2dda418e0452`；用独立取得的 AppArmor Development Team 公钥验证 detached signature，`gpgv` 报 Good signature。将仓库 `tools/apparmor-hce/rpm/sandbox-apparmor-parser.spec`、`tools/apparmor-hce/vendor/ax_check_compile_flag.m4` 放入节点的 RPM topdir，并由受信任 HCE 2.0 仓库安装 spec 所列 BuildRequires。核心构建命令为：

```bash
# 以下在目标 aarch64 HCE 节点执行；三个输入文件和公钥事先从受信任来源放到当前目录。
test "$(uname -m)" = aarch64
echo 'ded4cd419b8a05002a108a0912208e2098695752664c6a59464d2dda418e0452  apparmor-v4.1.7.tar.gz' | sha256sum -c -
gpgv --keyring ./apparmor-trusted-public-keyring.gpg apparmor-v4.1.7.tar.gz.asc apparmor-v4.1.7.tar.gz
dnf install -y --setopt=install_weak_deps=False rpm-build gcc gcc-c++ libstdc++-static make bison flex autoconf automake libtool pkgconf-pkg-config dejagnu perl perl-Test-Simple perl-Pod-Checker perl-podlators python3 binutils-extra
build_top=/var/tmp/sandbox-apparmor-arm64
mkdir -p "$build_top"/{BUILD,BUILDROOT,RPMS,SOURCES,SPECS,SRPMS}
cp apparmor-v4.1.7.tar.gz ax_check_compile_flag.m4 "$build_top/SOURCES/"
cp sandbox-apparmor-parser.spec "$build_top/SPECS/"
rpmbuild -ba --define "_topdir $build_top" "$build_top/SPECS/sandbox-apparmor-parser.spec"
```

本次实际使用隔离 topdir `/var/tmp/sandbox-apparmor-arm64`，原生 `rpmbuild -ba` 完成 libapparmor 测试并进入 parser 测试，但临时构建 Pod 到期，**完整 parser 测试没有跑完**。随后仅为验证节点端到端链路执行 `rpmbuild -bb --nocheck --define "_topdir /var/tmp/sandbox-apparmor-arm64" /var/tmp/sandbox-apparmor-arm64/SPECS/sandbox-apparmor-parser.spec`，生成 `/var/tmp/sandbox-apparmor-arm64/RPMS/aarch64/sandbox-apparmor-parser-4.1.7-1.aarch64.rpm`。其 SHA-256 为 `7ca6f7fa127d424a2e3a2089bf00da7cd60a49806b6e7744ed3608466301d912`。包仅含 `/usr/sbin/apparmor_parser` 和许可证，无安装脚本；`rpm -K` 仅显示 `digests OK`，**未签名、未完成完整测试，不得作为生产制品**。

该测试节点执行 `rpm --test -i`、`rpm -i` 和 `rpm -V sandbox-apparmor-parser` 均通过；`/sbin/apparmor_parser` 可执行，版本 4.1.7。先备份 `/etc/containerd/config.toml` 到 `/etc/containerd/config.toml.codex-backup-20260920`，仅将 CRI 的 `disable_apparmor` 从 `true` 改为 `false`，用 `containerd --config /etc/containerd/config.toml config dump` 校验后重启 containerd。运行中的 `crictl info` 显示 `disableApparmor: false`，containerd active，节点仍 Ready。节点配置文件标注由 CCE 创建、集群升级可能覆盖，因此生产必须由平台方找到持久化配置入口；单独改节点文件不是持久解决方案。测试节点当前保留该配置与 RPM，备份也保留。

## 集群与 API 验证

- 用户提供的多架构镜像 `registry.i.huaxisy.com/library/ai-infra/sandbox-fuse-mounter:v0.3.35` 在 arm64 节点的新 FUSE pool Pod 中运行；`workspace-mounter` 的 `/proc/1/attr/current` 为 `sandbox-fuse-37d5cf8ed9c5decd5c13aa61c97d0c91183cbb0214d61769c8430dfd0090724d (enforce)`。
- 在该容器中读取 `/etc/hosts` 成功，读取 `/etc/shadow` 返回 `Permission denied`。该负向结果与策略一致，但未单独取得 audit 事件来区分 AppArmor 与文件权限来源；不把它作为唯一的强制执行证据。
- 测试 API 申请 `workspace_mount_mode=fuse` 的临时沙盒返回 HTTP 201，沙盒内写入/读取 `/workspace/arm.txt` 返回 `arm-fuse-ok`，workspace sync 与 DELETE 均返回 HTTP 200。
- 重新执行 Helm upgrade 后 release 为 revision 5、`deployed`；API Deployment `3/3`，AppArmor loader DaemonSet `3/3`，Redis `1/1`。升级前因 arm64 CRI 不支持 AppArmor，API 曾因 FUSE pool 初始化失败反复重启；当前 Pod 的累计 restart 计数包含该历史，不代表升级后持续重启。
- 当前 HPA 的 CPU metrics 为 unknown（测试集群缺少 metrics API），与本次 AppArmor 链路无关。FUSE 空闲池 Pod 的 Kubernetes `READY 1/2` 是 mounter 尚未授权挂载时的状态；实际 API 沙盒读写、同步和销毁成功。

## 上线边界

arm64 测试节点已证明这套 parser、CRI 配置、加载器和多架构 mounter 可以完成真实 FUSE API 链路；**不能据此宣布生产节点或生产 RPM 合格**。上线前至少完成不中断的全部上游测试和制品审计、内部签名、双架构包管理与安装核验，并确认 CCE 对 containerd 配置的持久化/升级行为及节点维护方案。生产执行步骤见[单节点 runbook](../deployment/apparmor-loader.md#euleros-20-测试节点的受控整改步骤)。
