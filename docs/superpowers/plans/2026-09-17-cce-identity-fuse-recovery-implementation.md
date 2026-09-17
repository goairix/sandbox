# CCE 身份绑定与未启动 FUSE 清理实施计划

> 已获用户确认，当前目录实施；不建 worktree，不检查/切换分支，不委派，不修改节点，不新增云卷。

设计：[整改设计](../specs/2026-09-17-cce-identity-fuse-recovery-design.md)。最低 Kubernetes 1.29；无接口、配置或 Helm 资源变更。

## 1. 创建前稳定读取（测试→实现→负向回归）

- [x] 在 `internal/redisbootstrap/identity_pvc_stability_test.go` 添加同 UID/RV 漂移回归；先运行 `go test ./internal/redisbootstrap -run TestIdentityPVCBinding -count=1` 确认旧代码失败。
- [x] 新增 `identity_pvc_stability.go` 并接入 `identity_secret_kubernetes.go`：固定 CM 身份/版本及已观察 PVC UID；仅 RV 漂移按既有 250ms/共享超时重试；稳定路径不增加请求。
- [x] 补充持续漂移、取消、PVC 消失/换 UID/删除/许可变化、CM 变化、并发 Secret 和不确定 Create；真实 HTTP 测试验证最多一次 POST、错误正文/警告不泄漏。
- [x] 运行相关包测试，核查密钥仅在稳定之后生成，现有 Secret 保持只读。

## 2. 未启动 FUSE 正常终止（测试→实现→证据回归）

- [x] 在 `internal/runtime/kubernetes/fuse_unstarted_cleanup_test.go` 添加已调度但未启动回归；先运行相关测试，确认 shutdown 不可执行导致旧代码失败。
- [x] 新增 `fuse_termination.go` 并接入 `runtime.go`：保守状态判定只授权正常 UID 删除；保留宽限期和 finalizer；有界观察 kubelet 终止，丢失/替换/身份变化/finalizer 丢失不作为证明。
- [x] 覆盖 init/普通/临时容器、历史状态和异常混合状态；运行中/未知状态保留既有 shutdown/fencer 路径，不额外请求/分配。
- [x] 补充删除响应丢失、等待中启动、超时/取消、替换、恢复及 Finalize 顺序测试；运行运行时和沙盒池回归。
- [x] 增加运行中纯判定基准，记录分配数，不作为 CCE 实测。

## 3. 文档、自审与提交

- [x] 更新 `docs/deployment/apparmor-loader.md` 与 `helm-deployment-upgrade.md`：镜像 parser 不等于节点 CRI 支持；提供只读前提检查，不指导盲目重启 CRI。
- [x] 写 `docs/testing/2026-09-17-cce-identity-fuse-recovery-remediation.md`：两个镜像需重建，无新增 values；CCE 验收仍需节点 AppArmor 支持及部署测试。
- [x] 内联自审身份固定/共享预算/最多一次 Create/无强制删除/证明持久化后才释放 finalizer 和策略/默认行为/隐私。
- [x] `go test ./... -count=1 -timeout=3m`
- [x] `go test -tags helmtests ./internal/helmtest -count=1 -timeout=3m`
- [x] `go test -race ./internal/redisbootstrap ./cmd/redis-bootstrap ./internal/runtime/kubernetes ./internal/sandbox -count=1 -timeout=3m`
- [x] `go build ./...`、`go vet ./...`、`GOOS=linux GOARCH=arm64 go build ./...`
- [x] 主机及 linux/arm64：`golangci-lint run --timeout=3m --max-issues-per-linter=0 --max-same-issues=0`
- [x] `bash scripts/test-helm-apparmor-loader.sh`、`git diff --check`
- [x] 更新实际证据与清单，精确暂存并审查 diff，提交代码；不构建/推送镜像、不部署线上。

实施与本地验收完成。本计划随代码提交；现场验收与节点前提仍按整改报告单独跟进。
