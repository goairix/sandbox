# 原任务 Lease 与保守 checkpoint：Task3 验收

原始范围 `d1b8171506d17d968dbf39747c26b62864938fd4..a5bd5ce50dcf1b94742e1c647f9ef133ba670bb5`。四个小提交：874f292完整bundle、520617f原Lease能力/typed checkpoint、cdc839f原生minimum TTL兼容、a5bd5ce失活/仲裁覆盖。独立spec/quality review通过，C0/I0/M3、Cannot-verify8逐项处理；整个单元最终review仍待执行。

TaskClaim self/origin/parentCtx封闭，只有真正领取任务才有Lease，不新增每空闲sandbox client/goroutine/timer/Watch。八个永久点与claim/guard各比较value/Lease/Create/Mod。失活不可恢复，Renew仅原KeepAlive，Release只原Revoke。checkpoint只pending/needs_reconciliation；固定共同Create但每次单独ExpectedMod CAS，同claim连续更新与新claim读取历史都合法，历史不授予外部执行权。完整typed checkpoint含Stage预留59/64项，预算在Grant前验证。

## 实际门禁与完整失败历史

宿主Darwinarm64Go1.25.6 CGO1 race，真实Linuxetcd3.6.15三成员加foreign。初次完整selector真实失败：三主成员64MiB配额下OOMKilled/137，3status不可用，3top85sub仅部分通过，3top12sub失败；15clientWARN、68metadataWARN+1startupschemaERROR。保持FAILED，未加配额、未清洗失败、未推断已恢复或OOM根因。

R8在相同64MiB/0.5CPU/128PID/ROroot/dropALL/NNP下运行3个fresh隔离批次：

|批次|selector|top/subPASS|client/metadataWARN|
|---|---|---|---|
|A31210-1791365851|`^TestTaskClaimFences$`|1/50|0/16|
|B31514-1791365895|`^TestTaskCheckpoint(CAS\|LostClaim)$`|2/13|4/28|
|C31847-1791365958|`^TestTaskClaim(Unknown\|Copies\|Lifecycle)$`|3/34|7/37|

合计6top97sub，95fresh native namespace加2host leaf，0FAIL/SKIP，75raw均exit0。每批四成员前后健康无OOM、精确项目容器/卷/网络清单空。685worker当前/副本/exactHEAD+3compiler，688Root fixture/source每批一致，6继承文件与原BASE一致。源码冻结后未修改，成功Go/status/log/up/down完整输出Root读至EOF。全部告警保留；每批16baseline、B12/C21对应replicatedrevokeapply，metadataERROR0。fresh status的storageVersion3.6.0不认证旧成员恢复或源码binary映射。

native短TTL测试实际requested1/granted2：按服务端实际(0,86400]TTL计算发送前单调deadline，保持原actual TTL续期上界。Grant/KeepAlive未知、malformed、迟到与取消；40fence mutation、8admission corrupt、竞争、copy/foreign拒绝；旧Stage在原taskLease revoke/expiry后不能被newclaim复活，丢响应仅原Stage仲裁。父ctx取消会拒绝新capability使用，但原serverguard仍live时已preparedStage可commit；取消不是远端撤回。

宿主fullhost历史6top+2hostPASS/95nativeSKIP，compile missing-symbol1不是行为RED。最后focusedfault-host/vet/diff的117源hash与candidate完全一致；较早fullhost/build历史有2个后来改动的test文件，产品源未改、build不消费test，最终native编译candidate全部测试。原argv/HEAD/hash/输出各自保留，未把较早host记录提升为相同测试源码。

## Finding与边界逐项处理

- M1：初始全run失败未解决；分批仅协议覆盖，后续容量/长稳调查强制，finalreview继续triage。
- M2：11client/33apply expectedNotFound告警完整分类，后续harness可断言额外非预期告警；不压制日志，finalreviewtriage。
- M3：48fixturebaselineWARN非pristine，配置/权限/auth选择需独立fixture/生产验证；finalreviewtriage。
- CV1：既有Stage/receipt/mutation等六文件与Task3原BASE逐字节一致；保留2026-10-06-etcd-stage-verification.md实际原Stage仲裁证据及本次changed-seam测试。R7既有Stage无成功Mod/localdeadline的限制明确接受；不提升为新TaskClaim四字段证据。
- CV2：已有recursive严格codec/预算验收见2026-10-06-etcd-domain-models-verification.md与foundation-verification.md；本次新claim codec/59operation/oversize受实际测试覆盖。未改共同decoder/accounting，不重复旧suite，不声称再次穷尽基础原语。
- CV3：identity/restore基础读逻辑未改，沿用已验收client/domain/acquisition的固定identity和restore防护；本次实际restore mutation及完整bundle测试只认证改动接口。生产restore管理另有门禁。
- CV4：初始完整selector真实OOM/137失败、3status不可用及startupERROR保持失败，三个fresh同配额批次不等于连续全selector或长稳容量。强制后续独立N/长期/容量调查，不归因为产品无缺陷或宣称OOM原因已解释。
- CV5：Darwinarm64race+Linuxetcd仅为此次实际平台。已有PID1执行验收经trackedauthenticatedverification保留；新的Task物理协议/Linux应用/FUSE仍必须单独验收。
- CV6：隔离fixture的HTTP/权限/unsignedtoken告警保留；不认证生产TLS/RBAC、恶意网络、failover与hardening，部署安全门禁仍强制。
- CV7：固定镜像身份及fresh成员server/storageVersion直接读仅描述当时成员；不证明原始失败成员恢复、源码binary映射、upgrade/downgrade/snapshot，恢复部署门禁保留。
- CV8：本次原始范围独立review与冻结hash已通过；全新单元BASE23e4337的最终review仍须执行，未运行新全仓suite。当前生产Manager仍Redis；没有整体切换或整迁移完成。

本单元不调用runtime、不证明closed/quiesced/remoteEnd、不释放owner/index/control。持久Task认证关闭、双排空、remote settled/fenced、exact termination、最终条件释放，及Manager/scheduler/FUSE/部署/Redis删除仍强制后续。没有N上限或性能提升结论。

## 固定原始报告与审查

当前OWN保留完整raw、来源、源码/编译器副本；单元最终review后统一封存。

- `task-3-report.md` SHA256 `92c9e0ddcb41ffcfa026a5879596c6aa36c2971ea69ef0fd228eeff6224e4e81`。
- `task-3-review.md` SHA256 `0e52840f3686cad64f005955990dd5210eeed3acd3efd9e14f22099e9b0ac0aa`。
- `root-task-3-real-etcd-audit.json` SHA256 `aadde9cc11d28626b088061f655a852d3923d384efe5cfaf3f5756ff4d868092`。
- `root-task-3-log-classification.json` SHA256 `3e2eff715605ab5a26993b495d14cac45178062dbf703bb169c991cc534c997f`。
- `root-task-3-host-source-history-audit.json` SHA256 `2634df660b25018cd0b6a4017df9a223d3b4dc93adecb751ecc41c72e726b2a7`。
