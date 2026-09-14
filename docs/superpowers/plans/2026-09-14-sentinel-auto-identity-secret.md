# Sentinel 自动身份 Secret 实施计划

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 用户只填写 values，Helm 自动创建全新 Sentinel 身份，保留安装只验证复用，不更换私钥。

**Architecture:** 普通一次性 Job 使用 typed namespace Kubernetes API，GET 固定状态和三个 PVC，最后 CREATE 固定 immutable Secret。模板共享一次缓存的 clusterID，只有无保留对象的自动模式授予该次创建许可；初始化 Job 和 API 沿用现有启动 gate。

**Tech Stack:** Go、client-go typed CoreV1Interface、Ed25519、Helm 3/4、结构化 YAML 测试、隔离 Redis 7 Docker 夹具。

---

工作目录固定 `/private/tmp/sandbox-sentinel-identity.yCPZko/worktree`，分支 `codex/sentinel-auto-identity`。主目录不编辑、不切分支、不构建发布镜像、不操作业务集群。设计为 `docs/superpowers/specs/2026-09-14-sentinel-auto-identity-secret-design.md`。

## Task 1：安全的 namespace 身份确保器与 CLI

**Files:**
- Create: `internal/redisbootstrap/identity_secret_kubernetes.go`、`identity_secret_validation.go`、`identity_secret_api.go`、对应 `_test.go`（含实际 HTTP transport）。
- Create: `cmd/redis-bootstrap/identity_command.go`、对应 `_test.go`。
- Modify: `cmd/redis-bootstrap/main.go`。

- [x] 写首次创建与重复复用测试，使用真实 Ed25519 生成器及 typed fake 对象存储；确认无实现时编译失败，随后空实现导致预期断言失败。

```go
type IdentitySecretOptions struct {
    Namespace, StatefulSetName, SecretName, StateConfigMap string
    Members [3]string
    FreshClusterID string // 空值只能验证已有 Secret。
    Timeout time.Duration // 默认两分钟，上限两分钟。
}
// EnsureIdentitySecret(ctx, coreClient, options) error
// 测试：先 Ensure，再 GET serverSecret，验证四个数据项；再次 Ensure 后 DeepEqual 原对象。
```

- [x] `go test ./internal/redisbootstrap -run TestEnsureIdentity -count=1`，RED 必须是首次缺少 Secret 或契约断言，而非夹具拼写错误。
- [x] 实现固定名称、namespace、UID/RV、Opaque immutable、三个 32-byte seed 与严格公钥 JSON 对应校验，登记 digest 对应；错误仅固定分类，不带 API 原文。
- [x] 写旧状态缺身份、旧/无标记 PVC、CM 缺失保留 PVC、显式只读、坏数据、UID/RV 变化、同名并发、未知提交结果、取消与超时测试；运行确认 RED，补齐两次 snapshot 和 Create 前后 GET。

```go
// CREATE 前：两次 Pending/no-registration/同 UID-RV snapshot；三个固定 PVC
// 仅允许缺失或 annotation sandbox/redis-cluster-id == FreshClusterID。
// CREATE AlreadyExists 或提交结果未知：GET 并验证服务端原对象，禁止覆盖。
// CREATE 后：允许同一安装正常登记/Initialized，必须验证登记 digest。
```

- [x] CLI 添加 `ensure-identity` 模式，参数如下；只使用 in-cluster config，5s请求超时、QPS2/Burst3；成功静默，私钥不进入 stdout。

```text
ensure-identity -namespace NS -statefulset STS -secret SECRET
  -state-configmap CM -members-json '["member0","member1","member2"]'
  -fresh-cluster-id CID -timeout 2m
```

- [x] CLI help/参数错误先于 ENV、文件与 API 调用；CLI 测试正常、错误、安全静默和取消。运行 `go test ./cmd/redis-bootstrap ./internal/redisbootstrap -count=1`。
- [x] 任务 SPEC review，再 QUALITY review，修复并复审。

## Task 2：Helm 普通身份 Job、生命周期与中文部署配置

**Files:**
- Modify: `deploy/helm/sandbox/templates/_helpers.tpl`、`redis-sentinel.yaml`、`deployment.yaml`（实际 chart 路径按现有仓库定位）。
- Create: 同目录 `redis-sentinel-identity.yaml`。
- Modify: 对应 chart `values.yaml`、`values-builtin-sentinel.yaml`、中文部署说明，`internal/helmtest/redis_sentinel_test.go`。

- [x] 用现有结构化渲染测试增加省略 identitySecretName 的自动模式，先运行确认原 mandatory-name 逻辑失败。

```yaml
redis:
  mode: sentinel
  password: REDIS_DATA_PASSWORD_32_CHARS_MINIMUM
  sentinel:
    identitySecretName: ""
    existingSecret: ""
    password: SENTINEL_PASSWORD_DIFFERENT_32_MINIMUM
```

- [x] helper `sandbox.sentinelIdentitySecretName` 统一所有引用；共享 root cache 生成 state clusterID，保留全部合法 CM data。渲染 lookup GET 固定 CM/Secret/三个 PVC；旧对象缺身份拒绝，旧 PVC 缺 CM 拒绝。非空身份为外部引用只读。
- [x] 新 StatefulSet 的 PVC 注释 `sandbox/redis-cluster-id: <同一 clusterID>`；Helm 额外 fixed StatefulSet GET（总六次 GET），旧 VCT 注释保持原样不补写 CID，有 CID 须匹配 CM，无新身份许可，不新增 Job STS 权限。身份 Job 使用上述 CLI，普通资源而非 Hook，不挂载凭据或 PVC、不传密码，独立 namespace SA。
- [x] Role GET resourceNames 限定 Secret/CM/三个 PVC；仅自动模式增加 namespace Secrets CREATE；无 list/update/patch/delete/ClusterRole。Job 非root999、dropALL、seccomp、只读根，120s预算；revision 名字不超过63字符。
- [x] 结构化测试同 CID、有效 Secret 名一致、RBAC、无 Hook/凭据、env无重复、名称边界、standalone/external无新资源，中文示例改为自动模式。
- [x] 运行 `bash scripts/test-helm-chart.sh` 及 auth/backend-switch/AppArmor 脚本；任务 SPEC review，再 QUALITY review，修复并复审。

## Task 3：自动身份驱动真实 Redis 夹具与最终交付

**Files:**
- Modify: `internal/redisbootstrap/sentinel_runtime_integration_test.go`。
- Create: `docs/superpowers/reports/2026-09-14-sentinel-auto-identity-validation.md`。
- Modify: 本计划完成勾选与最终中文部署文档。

- [x] 真实三成员矩阵先创建固定 CM，再 Ensure 自动身份，再 GET 原 Secret 投影各自 seed；重复 Ensure 保持对象字节，不用手动生成替代。

```go
// core := fake.NewSimpleClientset(cm)
// EnsureIdentitySecret(ctx, core.CoreV1(), opts)
// secret, err := core.CoreV1().Secrets(ns).Get(ctx, opts.SecretName, metav1.GetOptions{})
// 将 secret.Data["redis-i"] 投影到对应单个成员，不复制其他 seed。
```

- [x] 运行真实三成员、root 文件事务两个现有隔离夹具；只使用缓存 redis:7-alpine，清理自己创建的精确资源，报告 fake API 与真实 Redis 的边界。
- [x] 新鲜全量 `go test -p 2 ./... -count=1`、相关 race、`go vet ./...`、仓库 lint、四套 Helm 脚本和 `git diff --check`。失败修复直到通过，默认 skip 不当成在线验收。
- [x] 最终独立整合 review，核对批准设计每条，整改后再次验证。
- [x] `git add` 精确本轮文件，提交功能；确认主目录仍 main 且未被编辑，不 push/merge/删 worktree。最终给新镜像配置、存储类、两个密码、自动身份与认证 Secret 空引用、AppArmor 关闭，以及 Helm --wait-for-jobs 预算；不要求手工建资源。

## 计划自审

批准设计的创建授权、保留身份、私钥对应、未知提交、命名、最小权限、清理边界分别对应 Task 1/2；真实恢复与性能路径不改变对应 Task 3。上面的函数、注释 key 与 CLI flags 是 Chart/runtime 共用接口。无业务集群修改、无 main 修改、无发布镜像构建。
