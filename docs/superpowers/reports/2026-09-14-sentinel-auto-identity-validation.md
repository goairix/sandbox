# Sentinel 自动身份 Secret 验证记录

日期：2026-09-14。隔离分支 `codex/sentinel-auto-identity`，基于 `d28c0ec`，在仓库外临时 worktree 实施；主目录 main 保持不变。以下不表示生产集群、Kubernetes RBAC/CSI 或真实 AppArmor 内核验收。

## 改动与创建边界

默认空 identitySecretName 使用普通一次性 Job 自动创建固定 immutable Secret；非空外部引用只读。只有无保留 CM/PVC/STS/Secret 的自动安装获得本次 CID 许可，Pending 本身不授权换钥匙。已有 Secret 逐项验证 seed 与公钥，登记 digest 匹配；不更新、覆盖或删除任何身份、卷、登记或角色。

Helm 缓存六个固定对象 GET，不 list；新 PVC 模板标 CID，旧 VCT 无 CID 保持原注释，有 CID 必须匹配状态，避免不可变模板升级失败。运行 Job 仅读指定 Secret/CM/三个 PVC；自动模式 namespace Secret CREATE 的授权边界如实记录，外部模式不授予 CREATE。

实际 REST GET/POST 抑制 Warning；POST MaxRetries(0)，提交不确定时只 GET 服务端对象。创建前状态/旧已见 PVC UID/RV 校验，创建后 PVC 继续 pin UID/存在，允许正常绑定 RV 推进。所有 GET 共享两分钟预算、有界重试与取消，成功不输出密钥。

## RED / GREEN 证据

- 首次创建：空实现原本返回成功但 GET Secret NotFound，断言失败；实现后创建四个有效数据项、immutable/keep，无 Job ownerReference；重复调用对象保持原 bytes/metadata，只有一次 CREATE。
- 保留/无许可/不合法输入：旧 CID、旧/无标记或删除中 PVC、缺原 CM/Secret、已登记或 Initialized 缺身份、坏 seed/pub/多键/mutable/type/meta、UID/RV变化均拒绝；无 update/patch/delete/list。
- postCreate 已见 PVC 消失/同 CID 不同 UID：修前两个断言失败，整改后拒绝。
- 真实 HTTP：提交成功后 500 + Retry-After，修前底层重发 POST 断言失败；整改后一次 POST、读回原对象。普通 typed GET 正向 control 收到合法 Warning 一次，Ensure 所有请求收到零次，证明抑制路径有效。
- late CM：新匹配 CID PVC 先出现修前误拒绝，修后等待真正 CM 再创建；旧 marker/无许可立即拒绝，无 CM 一直不出现则有界超时无 CREATE。
- 临时第二轮 CM/PVC、Create 前 Secret、Create 后 PVC API 故障恢复，不导致第二次 CREATE。AlreadyExists、已提交/未提交未知结果分别正确读回或有界失败。
- CLI：help/参数错误先于 client，静态错误，成功静默；只读已有身份、取消、路由及两分钟预算验证。
- Helm 自动名称原 mandatory 校验 RED 后通过；254 字符成员 DNS 原被接受后拒绝，253 字符边界通过。旧 VCT 无 CID/保留自定义注释/foreign CID 四例 RED 后通过；普通资源、最小权限、无凭据挂载、同 CID、长度、默认模式不变均结构化验证。

## 隔离真实 Redis 矩阵

`scripts/test-built-in-redis-sentinel.sh` 使用当前 worktree 编译的 Linux 程序、自动 Ensure 生成的身份和 Redis 7 实际进程，覆盖 fresh Reserved → 三签名登记 → 配置事务 → 写入 ACK → Initialized、登记后取消并恢复、不同认证、非零主切换、旧主从角色恢复、单 Redis 重启、全停冷启动、较高少数 epoch 阻断、空旧卷/缺状态阻断、ACK不足拒绝、三成员 DNS IP 全变。第一轮通过（主矩阵69.46s，新IP14.43s），最终二次新鲜重跑也 exit 0（主矩阵54.65s，新IP6.25s）。

`scripts/test-redis-bootstrap-rewrite.sh` 两轮实际通过真实配置重写、冷恢复和 root 准备/文件事务中断矩阵（最终实际配置重写0.48s）。夹具按唯一 owner 精确清理，均报告自有容器/卷/网络零遗留；不清理用户镜像/业务资源。

测试使用 `redis:7-alpine`，本机新拉取测试镜像 digest `sha256:ff02b58f971e7d7d156a1267e283fcbbeee91773b6aa36c49dac28ecfe28eadf`。未构建/推送发布镜像。Kubernetes namespace API 为 typed fake/实际本地 HTTP transport，Helm lookup 为实际 Helm 连接隔离 HTTP API；不冒充真实 Kubernetes admission/RBAC/CSI 验收。

## 最终检查

- runtime 与 Chart 的 SPEC / QUALITY、最终独立集成复审全部通过，无开放 Critical/Important/Minor。本轮两项非阻塞建议（登记夹具边界、初装中断重试说明）已整改并读回复审关闭。
- 四套 Helm 脚本父代理最新全部 exit 0（chart/backend-switch/sentinel-auth/apparmor-loader）。测试 Python 为临时 venv，仅安装 PyYAML，不修改项目依赖。
- 父代理最新全仓 `go test -p 2 ./... -count=1`、完整 runtime/CLI race、完整 `-tags helmtests` race、目标身份 race、`go vet ./...`、`git diff --check` 均 exit 0。默认集成开关 skip 仍是 skip，不表示线上验收；两个真实隔离脚本另行明确开启并通过。
- 仓库没有 Makefile、golangci/lint 配置；执行 gofmt、go vet 和 Helm lint，不声称运行不存在的 lint。

## 已有后续硬化候选（非本轮新缺陷）

Helm 保留 CM 的渲染前校验沿用既有成员/CID检查，完整 phase、登记、公钥 digest 由 runtime Job 严格校验。在需要 backend/cleanup drain 的升级中，坏 CM 可能先排空再由普通 Job 拒绝；可以后续单独增加升级前静态 fail-closed 核验，不在 Helm 模板中复制完整加密登记验证器。本轮未放宽身份创建/初始化 gate，也不把隔离 Helm 保留 data 测试说成实际 Secret/grant 绑定验收。全新部署不涉及旧坏状态。

## 初次安装中断边界

最终复审指出首次身份生成前 Job 调度/镜像超时可能已留下普通 CM/PVC，此时不能笼统建议新 revision upgrade：缺身份保护会拒绝。中文说明已整改，只有原 Secret 已生成时才可只读重试；从未生成身份的失败初装需管理员核验无登记/业务、备份并确认精确处理范围，不自动删除/重新授权。正常初装仍只配置 values，不要求手工创建身份资源。

## 线上交付边界

只构建包含本轮代码的 sandbox-api，新 Chart 配合部署；用户 main 中旧 runtime 镜像打包不受影响。配置 mode sentinel、两个不同32~256字符安全 token、RWO StorageClass，身份/认证外部引用留空即自动管理；不要求手工建 Secret。此次 standalone → Sentinel 按既定排空备份卸载重装，非数据在线迁移；保留卷组重装须保留原 CM/身份和原认证。AppArmor 加载器本轮关闭，后续真实节点内核与 FUSE profile 验收仍单独完成。
