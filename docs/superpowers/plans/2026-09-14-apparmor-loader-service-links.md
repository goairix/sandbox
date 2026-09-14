# AppArmor 加载器启动门禁兼容修复计划

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 修复 Kubernetes 1.33 自动补入 Pod service links 字段导致 API 严格启动门禁超时的问题。

**Architecture:** 只在 Helm 加载器 DaemonSet 模板显式设置 `enableServiceLinks: false`，使模板与控制器生成的 Pod 契约一致。不放宽 API 比较、不修改 profile、镜像或 values 接口，也不操作线上资源。该补丁及回归紧密耦合，在当前分支完成，再独立复审。

**Tech Stack:** Helm、Go、Kubernetes API、现有 Python/YAML 渲染回归。

---

## Task 1: 测试先行并修复模板

**Files:**

- Modify: `scripts/test-helm-apparmor-loader.sh`
- Modify: `internal/helmtest/apparmor_defaults_test.go`
- Modify: `internal/runtime/kubernetes/apparmor_loader_gate_test.go`
- Modify: `deploy/helm/sandbox/templates/apparmor-loader.yaml`

- [x] 在脚本启用配置的 Pod spec 断言中加入：

```python
assert spec.get('enableServiceLinks') is False, 'loader must explicitly disable service links to match the startup gate contract'
```

- [x] 在 Go Helm 精简配置回归的启用分支，以及完整默认配置和显式 PriorityClass 配置中断言字段必须存在且为 false：

```go
if pod["enableServiceLinks"] != false {
    t.Fatal("loader must explicitly disable service links to match the startup gate contract")
}
```

- [x] 执行 `bash scripts/test-helm-apparmor-loader.sh` 和 `go test -tags helmtests ./internal/helmtest -run 'TestAppArmorLoaderDisablesServiceLinks|TestAppArmorTemplatesSupplyMissingDefaults' -count=1`，确认旧模板均因缺少字段而失败。
- [x] 在门禁回归加入四种组合：模板 nil/Pod true 被拒绝（原故障）；模板 false/Pod false 可启动；模板 false/Pod true 被拒绝；模板 false/Pod nil 被拒绝。通过 `appArmorLoaderObservationReady` 验证结果，保持严格字段契约。
- [x] 在加载器模板 `automountServiceAccountToken: false` 后加入：

```yaml
      # 显式关闭，避免 Pod 默认值与模板不同导致严格启动门禁误判。
      enableServiceLinks: false
```

- [x] 重跑以上失败测试并执行 `go test ./internal/runtime/kubernetes -run TestAppArmorLoaderGate -count=1`，预期全部通过。

## Task 2: 部署说明、验证和提交

**Files:**

- Modify: `docs/deployment/apparmor-loader.md`
- Modify: `docs/deployment/helm-deployment-upgrade.md`
- Create: `docs/testing/2026-09-14-apparmor-loader-startup-gate-results.md`

- [x] 在部署说明记录：加载器 Ready 而 API 门禁超时时，检查 DS 模板及其 Pod 的 `enableServiceLinks`；旧模板未设置、实际 Pod 默认 true。本次更新模板后两者均为 false，不需要新增 values 或重建镜像，不回滚或删除 Sentinel 身份/PVC。
- [x] 新建独立测试结果，保留此前组件级验收历史；区分现场只读证据、本地回归结果与待用户升级后的线上 API 验证，不宣称线上已修复。
- [x] 执行以下命令，全部退出码应为 0：

```bash
bash scripts/test-helm-apparmor-loader.sh
go test -tags helmtests ./internal/helmtest -count=1
go test ./internal/apparmorloader ./cmd/apparmor-loader ./internal/runtime/kubernetes ./internal/config ./cmd/sandbox ./internal/mounter ./internal/fuseprotocol -count=1
git diff --check
```

- [x] 请求独立复审，只读检查补丁与上下文；未发现问题，独立最小回归通过。
- [x] `git add` 以上七个明确文件及本计划，执行 `git commit -m "fix(helm): align AppArmor loader service links contract"`；检查提交后工作区状态，不 push、不操作集群。
