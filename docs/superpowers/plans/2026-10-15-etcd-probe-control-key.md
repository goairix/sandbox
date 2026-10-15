# Probe Control Key Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development. Steps use checkbox syntax for tracking.

**Goal:** 为生产接线提供永久probe seed原生CAS初始化及只读启动加载。

**Architecture:** 单namespace固定永久key；operator显式seed、单次CAS及只读unknown仲裁，API只读加载owned Ed25519 key。既有runtime option接收key，main保持关闭。

**Tech Stack:** Go1.25.6、现有client3.6.14、三成员server3.6.15，无依赖变更。

**Spec:** docs/superpowers/specs/2026-10-15-etcd-probe-control-key-design.md

## Global Constraints

- 固定meta/workspace-control-seed-v1、32字节seed、永久无Lease、version1/create=mod；不覆盖/删除/轮换或自行生成替代seed。
- 当前identity/restore四比较，≤64nodes/256KiB；原生领域API，不做Store adapter，不改main/runtime/Recovery/共享fixture/脚本或依赖。
- seed/privatekey仅owned内存，不打印、不进入Pod/诊断；TLS/RBAC/静态加密与部署仍是后续，不能宣称生产迁移。
- 只跑新增必要用例，保留真实RED/各源码binary epoch/actualWait/exactcleanup；不读任何CLOSED OWN/无关Sentinel/线上RedisK8s/环境凭证。
- 源码/tests/docs/报告/格式仅apply_patch；raw/binary可工具生成。Git先common.git/codex-etcd-commit.lock再OWN lock，Root协调expectedHEAD/空index/精确paths/diffcheck/小提交，无push/deploy。
- 与冷恢复R1并行只共享Go编译/Git，定义在工具边界可编译，不stage另一Task文件；currentworkingtree sourceepoch如实记录。

## Review Focus

1. CAS丢ACK后误重发或换seed，existing不同seed被覆盖。
2. metadata/restore漂移被当作正常key存在，缺失返回随机替代。
3. API只读身份因加载偷偷发Put而无法最小授权，私钥泄露到日志/错误。
4. Lease/version/ABA/损坏wire或foreign namespace混入加载结果。
5. caller输入或返回key修改改变已保存seed；最大namespace与恶意响应突破上限。

### Task 1: Native immutable control key

**Files:** Create `internal/storage/state/etcd/control_key.go`与`control_key_rpc.go`（若职责小可合并，不需空stub）、`control_key_test.go`与必要`control_key_native_test.go`。不改其他源文件；确有接点需求先向Root说明，不静默扩展。

**Interfaces:** spec中的ProvisionWorkspaceControlKey(ctx,seed)(ed25519.PublicKey,error)、LoadWorkspaceControlKey(ctx)(ed25519.PrivateKey,error)。使用既有Backend/native helpers、integrationBackend/faultKV测试接点；错误沿用当前etcd领域errors，不暴露seed值。

- [x] **Step 1: RED。** 可编译拒绝入口、真实初始化/加载反例，编译失败不算RED。
- [x] **Step 2: 实现和首次小提交。** 当前operator屏障、固定CAS、只读load与ownedkey；新真实三成员positive/concurrency/readonly/复制通过即提交连贯功能。
- [x] **Step 3: 故障边界。** 丢ACK只读仲裁、错seed/Lease/version/wire/漂移/取消、最大namespace有限wire；只跑新增或实际改变源码相关用例，及时提交。
- [x] **Step 4: gate。** 新host race/native/vet/format/diffcheck、源码binary/精确cleanup与唯一fresh review已核对。原始container退出Wait缺口M1经Root透明裁定范围接受，未验收容器退出检查，不能把up --wait当退出证明。无main切换/容量/安全轮换claim。

范围验收见reports/2026-10-15-etcd-probe-control-key-verification.md，源码226b543/dc08b41；生产接线继续，不结束主分支工作。
