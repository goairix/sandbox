# etcd HTTP Gateway Implementation Plan

> REQUIRED SUB-SKILL: subagent-driven-development。一个完整可启动 HTTP 业务 Task，fresh author/reviewer，持续实现与小步提交。

**Goal:** mounted etcd 部署实际提供创建、健康查询、同步/SSE执行的 HTTP/CLI 路径，无 Redis 初始化或 fallback。

**Spec:** docs/superpowers/specs/2026-10-16-etcd-http-gateway-design.md

**Immutable BASE:** `4cb308b6629b6621367d8e1b312241c5bbd228c6`

## Global Constraints

仅显式 --serve-etcd 分支使用新模式，legacy默认路径后续完整切换；unsupported501明确列出而非假支持。一个 Deployment/Service、原能力与准入/Close、固定principal/profile/key；不引入新认证模型/任意用户authority、重复exec、raw用户exec或Redisfallback。真实body/输出/队列/并发/时间预算与socket取消/join；同步非零退出正常，unknown不伪exit0/SSEdone。旧startupcheck只读/manifest兼容。Root唯一Go/Docker/Git，apply_patch唯编辑，Sentinel禁止；旧OWN/pairs关闭，不读取或重跑旧矩阵。

## Task 1: Actual authenticated plain gateway and standalone etcd startup

**Candidate files:** 新internal/api/etcdgateway/*.go（独立窄handler，不复制旧全Handler）；原language builder必要小幅共享；deployment manifest/open的HTTP config读取与owned settings；cmd/sandbox/etcd_serve.go与main.go早分支及tests；新HTTPnative test与现cold fixture私有variant最小扩展。确切源点在预检冻结。

**Consumes:** CreateBusiness、Lookup、Exec、原DTO/SSE/command语义、原auth及boundedHTTPserver、mounted owner/signer/固定principal。

**Produces:** 可实际运行且明确受限的etcd HTTP入口、正确幂等/pending/unknown/wire、真实流断连/关闭和operator文档。

- [x] Step1 只读预检 exactmanifest/CLI/handler/router/DTO与typederror/status、state语义、body/输出/并发/执行默认值、SSEdeadline/背压/join及native源点；Root裁定冻结（OWN preflight/ledger，09:33UTC）。
- [x] Step2 tests-only最小RED，再实现。settings/共享命令、gateway、CLI及两个诊断/重放修正均已针对验证并小步提交。
- [x] Step3 targeted host/race/相关Linuxvet；实际HTTPnative的复用标记缺陷已真实RED→修正→http-03全场景PASS1.77s，无skip，11个原bridge Wait/IO及正式launcher/etcd Wait已核对；不是容量或生产部署验收。
- [x] Step4 唯一fresh reviewer原BASE→HEAD，初审I1/I2/I3经同pair R1全部解决，无新增Critical/Important，spec/质量Approved；tracked范围验收，保留M1与后续health预算项，关闭OWN后继续其余迁移。

## Root 初始共享接口检查

| Producer→consumer | 接点 | 初始结论 |
| --- | --- | --- |
| mountedHTTPsettings→CLI/handler | manifest/Open/main earlyflag | 旧check配置可无HTTP，serve必须完整；无Redis初始化或双状态路径 |
| CreateBusiness→v1 response/retry | key/IDs/state/ownedBusiness | pending同key重复POST恢复；无request-ID索引或N扫描 |
| Lookup→GET/创建完成响应 | CreatedAt/expiry/config/health | historical completion不可当健康；不伪造busy/idle |
| language/timeout/env→ExecutionRequest | 旧commandbuilder/固定UID/Env | 默认来源明确；不继承管理env/静默忽略TTY |
| service同步writer→HTTP/SSE | terminal/unknown/refs/backpressure | bounded输出与真socketdeadline，断连取消/join不自动重发 |
| server Shutdown→Deployment.Close | handlers/activecalls/bridge/wg | 先停止与取消HTTP，再实际join与Close；超时非物理join |
| Task1内部 | 四路由/CLI/native/限制说明 | complete可启动受限入口，其他路由明确501而非混旧Manager |

Ruling: 用户批准完整迁移并允许激进单环境实施；先提供明确受限的etcd独立启动路径，使已验收service实际对HTTP可用，未迁移路由failclosed501且继续后续工作，不静默丢旧功能/宣称全迁移。若该阶段边界不合适，成本是最终统一入口调整；不会改变旧默认服务或已部署环境。
