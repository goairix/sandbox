# HCE EulerOS 2.0 双架构 AppArmor Parser Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 在管理员提供的、已固定摘要的 HCE 2.0 构建环境中，为 amd64 与 arm64 生成可审计的最小 `sandbox-apparmor-parser` RPM，并提供离线验收和受控节点验证文档。

**Architecture:** `tools/apparmor-hce` 提供源码锁定、签名/摘要校验、架构检查、隔离构建和 RPM 审计脚本；构建环境由调用方按架构传入固定 digest 的 HCE builder image，不在仓库中假设或下载基础镜像。RPM spec 只安装真实 parser 及经过审计的运行时文件，不修改 containerd、systemd 或现有 profile；现场安装和 containerd 重启保持人工维护步骤。

**Tech Stack:** AppArmor 4.1.7 userspace、RPM/rpmbuild、Docker BuildKit/buildx、POSIX shell、Python 3 标准库、Helm 3、`apparmor_parser`、ELF/RPM 元数据工具。

---

## 文件边界

- Create: `tools/apparmor-hce/source.lock` — 上游版本、源码资产、摘要、签名与受信公钥指纹的锁定格式。
- Create: `tools/apparmor-hce/build.sh` — 参数校验、源码验证、按架构调用隔离 builder、导出 SRPM/RPM/清单。
- Create: `tools/apparmor-hce/verify.sh` — 不安装到宿主机的 RPM payload、依赖、ELF、架构和 parser 行为审计。
- Create: `tools/apparmor-hce/Containerfile` — 使用调用方传入的 HCE builder image 构建 libapparmor/parser 与 RPM；不包含默认基础镜像名。
- Create: `tools/apparmor-hce/rpm/sandbox-apparmor-parser.spec` — 最小 parser-only RPM，禁止服务/策略覆盖和安装脚本副作用。
- Create: `tools/apparmor-hce/tests/contract.sh` — 构建入口和 RPM 审计的正向/负向契约测试。
- Modify: `docs/deployment/apparmor-loader.md` — 双架构源码锁定、构建、签名、安装前提及真实 enforce 验收。
- Modify: `docs/deployment/helm-deployment-upgrade.md` — 将 parser/CRI 节点维护列为 Helm 前置条件和升级后的验证步骤。
- Create: `docs/testing/2026-09-18-hce-apparmor-parser-build-validation.md` — 只记录摘要、架构、版本、检查状态及脱敏结果的验收报告模板。

### Task 1: 建立源码锁定与构建输入契约

**Files:**
- Create: `tools/apparmor-hce/source.lock`
- Create: `tools/apparmor-hce/README.md`
- Test: `tools/apparmor-hce/tests/contract.sh`

- [ ] **Step 1: Write the failing contract checks**

在 `contract.sh` 中先写 shell 检查：缺少 `source.lock` 字段、架构值不是 `x86_64`/`aarch64`、builder image 未设置或不是 digest 引用时必须退出非零；不得打印 token、kubeconfig、values 或原始环境变量。

- [ ] **Step 2: Run the contract test and verify it fails for the missing inputs**

Run: `bash tools/apparmor-hce/tests/contract.sh --case missing-source-lock`

Expected: 以非零退出并输出固定的 `source lock is required`，不输出任何凭据或源码内容。

- [ ] **Step 3: Define the locked-source format and input validator**

`source.lock` 使用固定字段：`version=4.1.7`、官方 release archive URL、archive SHA-256、签名 URL、受信公钥指纹、`source_revision`。`build.sh` 的 `validate_inputs()` 只接受明确的 `--arch amd64|arm64`、`--builder-image registry/path@sha256:...`、`--output-dir`，把架构映射为 `x86_64`/`aarch64` 并拒绝其它值。签名/摘要失败、TLS 失败或公钥指纹不匹配立即退出，不能自动更换源。

- [ ] **Step 4: Add a no-secret README usage contract**

README 给出只读示例：`./tools/apparmor-hce/build.sh --arch amd64 --builder-image "$HCE_BUILDER_IMAGE_DIGEST" --output-dir ./out/amd64`；入口必须拒绝未设置或不是 SHA-256 digest 的值，并说明 `HCE_BUILDER_IMAGE_DIGEST` 由管理员从受信 registry 传入，构建器必须实际报告 HCE 2.0 和对应架构，且示例不代表仓库中存在该镜像。

- [ ] **Step 5: Run positive and negative contract checks**

Run: `bash tools/apparmor-hce/tests/contract.sh`

Expected: 正向字段校验通过；错误架构、tag 而非 digest、缺少签名或摘要均被拒绝。提交本任务。

### Task 2: 编写隔离 HCE builder 和 parser-only RPM spec

**Files:**
- Create: `tools/apparmor-hce/Containerfile`
- Create: `tools/apparmor-hce/rpm/sandbox-apparmor-parser.spec`
- Modify: `tools/apparmor-hce/tests/contract.sh`

- [ ] **Step 1: Add a failing payload-policy test**

在测试中对 spec 文本和构建出的 payload 检查：不得出现 `systemd`, `apparmor.service`, `/etc/apparmor.d`, 通用 profiles、`%post`/`%pre`/`%trigger`、containerd 配置路径或 `--nodeps`。仅允许真实 parser、其明确运行库、专属 ABI 数据、许可证和构建清单。

- [ ] **Step 2: Implement the Containerfile contract**

Containerfile 使用 `ARG HCE_BUILDER_IMAGE` 后再 `FROM ${HCE_BUILDER_IMAGE}`，首行构建检查 `/etc/os-release` 为 HCE 2.0、`uname -m` 与目标 `TARGETARCH` 匹配，并检查 `rpm-build`, `gcc`, `make`, `bison`, `flex`, `pcre2` 等依赖存在。它按上游顺序构建 libapparmor、执行 `make check`、构建 parser，再调用 `rpmbuild`；不运行 privileged 命令、不写宿主路径、不安装 init/service。

- [ ] **Step 3: Implement the minimal RPM spec**

spec 将 parser 安装到 HCE 实际的 `/usr/sbin/apparmor_parser`，并仅在 `/sbin` 是系统正常链接布局时提供兼容入口；使用 `%config` 之外的专属数据目录，不接管 `/etc/apparmor.d`。不写任何 scriptlet；架构由 rpmbuild 产物和构建入口双重校验，ELF 不匹配直接失败。

- [ ] **Step 4: Run static payload checks**

Run: `bash tools/apparmor-hce/tests/contract.sh --case spec-policy`

Expected: 包含服务、通用 profile、配置修改脚本、危险路径或 `--nodeps` 的 fixture 全部失败；最小 spec 通过。提交本任务。

### Task 3: 实现双架构构建、清单和 RPM 审计

**Files:**
- Modify: `tools/apparmor-hce/build.sh`
- Modify: `tools/apparmor-hce/verify.sh`
- Modify: `tools/apparmor-hce/tests/contract.sh`

- [ ] **Step 1: Add failing artifact checks**

测试先要求每个成功构建目录包含 SRPM、一个匹配架构的 RPM、`SHA256SUMS`、构建元数据和审计结果；缺一个、RPM 架构不匹配或清单摘要不一致必须失败。

- [ ] **Step 2: Implement the isolated build invocation**

`build.sh` 为每次运行创建权限受限临时工作目录，下载并验证锁定源码，使用 `docker buildx build --load` 或等价的本地 OCI 导出调用 Containerfile；只传入目标架构和固定 builder digest，禁止读取 Kubernetes 配置。导出文件后立即计算清单，输出路径和状态，不输出命令环境。

- [ ] **Step 3: Implement RPM/ELF audit**

`verify.sh` 在 HCE builder 容器中运行 `rpm -qpl`, `rpm -qp --requires`, `rpm -qp --scripts`，检查绝对路径白名单、无脚本副作用、架构字段、文件属主/模式和依赖闭合；使用 `readelf -h/-l/-d` 核验 machine、解释器和 GLIBC 符号，不以 `file` 或仿真替代运行验证。发现假 parser、缺运行库、越界路径或覆盖现有配置即非零退出。

- [ ] **Step 4: Add parser behavior checks**

在隔离 HCE builder 中安装测试 RPM（不使用 `--nodeps`/`--nogpgcheck`），执行真实 `apparmor_parser --version`、`-Q -K` 编译由 Helm profile 模板替换出的完整 profile，并记录 stdout/stderr 的摘要和退出状态。`-Q -d` 仅作为语法报告，不计作编译或 enforce 证据；任何 warning、ABI/features 不匹配或编译失败均停止。

- [ ] **Step 5: Exercise positive/negative matrix**

Run: `bash tools/apparmor-hce/tests/contract.sh --case artifact-matrix`

Expected: amd64 与 arm64 的合法 fixture 各通过；错误架构、错误摘要、损坏签名、缺库、越界 payload、占位 parser、tag builder image 和伪造 parser 输出均失败。提交本任务。

### Task 4: 增加离线 profile/内核能力验收工具

**Files:**
- Modify: `tools/apparmor-hce/verify.sh`
- Modify: `tools/apparmor-hce/tests/contract.sh`

- [ ] **Step 1: Write failing profile-hash tests**

测试使用 `deploy/helm/sandbox/files/apparmor/workspace-mounter.profile`，通过 Helm 既有替换规则生成 profile，断言没有 `__SANDBOX_PROFILE_NAME__`，且归一化文本 SHA-256 与 profile 名称和 Chart digest 一致。

- [ ] **Step 2: Implement immutable profile compilation**

`verify.sh` 只读渲染指定 Chart fixture，按现有名称替换和 trim/LF 规则生成临时 profile；用真实 parser `-Q -K` 编译，不加载、卸载或修改宿主机策略。内核 features 只能由显式 `--kernel-features-dir` 传入并校验来源摘要；未提供时报告“仅用户态编译检查”，不得推断内核兼容。

- [ ] **Step 3: Add safety negative fixtures**

拒绝含占位符、profile 名称不符合既有 hash、quiet 掩盖 warning、弱化 ABI/features、删除 `/dev/fuse`/mount 约束或额外通配权限的 fixture；不要修改生产 profile 以通过测试。提交本任务。

### Task 5: 更新部署和运维文档

**Files:**
- Modify: `docs/deployment/apparmor-loader.md`
- Modify: `docs/deployment/helm-deployment-upgrade.md`
- Create: `docs/testing/2026-09-18-hce-apparmor-parser-build-validation.md`

- [ ] **Step 1: Document build and provenance commands**

写出锁定源码核验、管理员传入 builder digest、amd64/arm64 分开构建、内部 RPM 签名和 `verify.sh` 命令；明确不能把 Debian parser 镜像内容复制到 HCE，不能使用 `--nogpgcheck`，也不能把交叉/仿真构建当成现场通过。

- [ ] **Step 2: Document node maintenance gate**

说明 Helm loader 只加载策略，不能提供宿主机 parser 或改变有效 CRI 配置；安装前需核验节点身份、PDB、容量和恢复方案，再由管理员按维护流程 cordon/drain、安装真实 RPM、审查 CRI 配置并受控重启 containerd。禁止 kubelet 重启、主机根目录/runtime socket 安装 Pod、Unconfined 降级和扩大节点范围。

- [ ] **Step 3: Add per-architecture acceptance report**

报告模板只允许填写版本、架构、摘要、parser 运行状态、真实 Localhost enforce、FUSE 创建/读写/销毁、拒绝证据、遗留资源数和节点恢复状态；要求把“构建通过”“amd64 现场通过”“arm64 现场通过”“生产资格”分开填写，禁止用其它 OS 的 arm64 结果代替 HCE arm64。

- [ ] **Step 4: Run documentation checks**

Run: `git diff --check && rg -n '未完成|待定|--nogpgcheck|Unconfined|Debian parser' docs/deployment/apparmor-loader.md docs/deployment/helm-deployment-upgrade.md docs/testing/2026-09-18-hce-apparmor-parser-build-validation.md`

Expected: 仅出现安全边界说明，不出现未完成占位符；命令、路径、架构命名与工具契约一致。提交本任务。

### Task 6: 受控测试节点现场验收（不由构建脚本自动执行）

**Files:**
- Modify: `docs/testing/2026-09-18-hce-apparmor-parser-build-validation.md`
- Test: `tools/apparmor-hce/verify.sh` and the administrator-run maintenance procedure

- [ ] **Step 1: Recheck approved target identity and readiness**

在任何写入前重新确认测试节点 `172.16.30.166` UID、kube-system namespace UID、Ready/unschedulable、PDB selector、资源容量和恢复入口；若任一不匹配，停止，不自动选择第二节点或生产节点。

- [ ] **Step 2: Install the matching signed RPM using the platform maintenance path**

管理员在目标节点执行正常 RPM 安装和签名校验，记录事务摘要；安装前备份并审查有效 containerd 配置，安装包不允许自带配置覆盖或服务重启。

- [ ] **Step 3: Enable and verify CRI AppArmor with a controlled containerd restart**

仅修改已审查的有效 CRI AppArmor 开关/路径，按维护流程受控重启获准的 containerd；分别记录静态配置和运行中有效配置，等待节点与系统组件恢复。若 parser、CRI 或恢复任一步失败，停止后续 FUSE 验收，不切换到 Unconfined。

- [ ] **Step 4: Run isolated real FUSE and denial checks**

在临时 namespace 中将 loader、API 和 FUSE 测试 Pod 限定到目标节点，使用已有生产安全门禁；验证 mounter/s3fs 的 Localhost enforce、真实挂载读写、策略拒绝和正常销毁/资源收尾。既有业务池不改动，第二节点不加载策略。

同时保存目标 containerd 为实际 FUSE Pod 生成的默认 profile 名称、状态和 parser 结果摘要；它必须与项目 profile 的 enforce 证据分开记录，不能以任一 profile 的存在代替另一项验收。

- [ ] **Step 5: Record the result and stop at the correct gate**

分别写入 amd64 构建、amd64 现场、arm64 构建、arm64 现场和生产资格状态。没有 HCE arm64 节点时只记录 arm64 构建结果，其余状态保持未完成；不凭单节点成功宣称双架构生产通过。提交验收报告。

## Verification and handoff

完成计划后，按任务逐次执行 `bash tools/apparmor-hce/tests/contract.sh`、各架构 `build.sh`/`verify.sh`、现有 Helm/AppArmor 回归脚本、Go 测试和 `git diff --check`。所有失败门禁必须保留证据并停止，不通过修改安全 profile、绕过 RPM 签名或扩大节点范围来“修复”。每个任务独立提交；最终提交前复核 `git status --short` 只含本计划范围内的文件，并使用 `docs/testing/2026-09-18-hce-apparmor-parser-build-validation.md` 汇总脱敏结果。
