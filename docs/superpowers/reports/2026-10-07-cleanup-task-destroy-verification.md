# 精确 control 下的销毁意图：Task2 验收

原始范围 `43ea3efe4f0c4699dd76eb8e0a72921e1b75d0ee..495e7d7f4f7f233821697fc49115e3bdcd90a1b5`。`435da3c` 原子准备清理意图/入场顺序；`495e7d7` 原 Stage 仲裁和围栏故障覆盖。三新增文件559行，无已有产品源码改动。独立 spec/quality review 通过：Critical0、Important0、Minor2、Cannot-verify9，逐项处理如下。

PrepareDestroy 精确读取原 active 五点链、要求调用者旧 control ModRevision，生成新的 task UUID/原 Stage attempt。5个永久 put（control→destroying、task、intent、link、初始 pending checkpoint），20个原五点 value/Mod/Create/Lease 比较和4个新key不存在比较，含原 Stage协议预留共43/64项；编码/预算在Grant前校验。保留原 runtime、gate、expiry、snapshot、owner/fence/index/已有token。手动销毁不拒绝已经过期但仍存在的 active 对象。prepared不等于committed，committed不等于目标持久关闭或物理End。

## 实际验证与来源

Darwin arm64 Go1.25.6 CGO1，`go test -race -count=1 -timeout=90s -run '^TestTaskDestroy(Atomic|AdmissionOrder|Unknown|Fences)$' -v ./internal/storage/state/etcd`，实际三成员 etcd3.6.15：4顶层+38子测试PASS、0FAIL/SKIP、exit0。37个公开元数据fixture namespace加1个纯mutation形状测试。覆盖atomic shared birth/owner保留、过期手动对象、输入/TTL/旧revision拒绝、竞争、一前一后与迟到入场、丢提交响应、abort后迟到commit、五点值/改写/重建/Lease、四不存在条件、restore、原guard重建/revoke与旧abortedreceipt。

项目 `sandbox-etcd-state-test-20646-1791361861` 全25raw exit0；660份worker当前/副本/Git、663份Root当前fixture源码及Go/compile/link三程序hash一致。八个既有authority/codec/Stage文件精确等于原BASE。四个etcd成员固定本地digest镜像、loopback端口、自己项目的named-volume、64MiB/0.5CPU/128PID/ROroot/dropALL/NNP，前后健康无OOM，回收容器/卷/网络三个精确清单空。完整Go/status/metadata/up/down输出均读取到EOF。Host的37fixture子SKIP与缺失新API的compile1保留，只是历史，不冒充行为RED或实际验收。vet/最终source/diff/worktree检查0，不重跑旧Task1/kernel或全仓suite。

Root Ruling6添加四次有界只读status：本次新成员报告server3.6.15、storageVersion3.6.0；仅证明读取时这些成员的字段，不是旧Task1成员恢复、升级/snapshot或源码归属证明。实际stdout3条重复Revoke NotFound WARN；metadata26WARN（16fixture配置、1启动missing-term、9对应三个revoke在三成员apply），0ERROR；原始告警完整保留，未抑制/清洗。

## 每项 finding 与边界

- M1：三条client与九条server重复revoke告警属于原Stage幂等cleanup/负例；检查断言成功。deferred minor，最终单元review继续核对；不以suppression替代错误分类。
- M2：fixture启动配置/权限/token/schema告警非pristine，完整保留。deferred minor；本次新status不能擦除既有启动输出或证明生产安全。
- CV1：Ruling7明确新TaskClaim四字段比较不套用既有Stage。原Stage成功CAS只比较guard值/原Lease/Create，无ModRevision成功CAS；本次guard重建/revoke测试不认证同值同Lease原地改写失活检测。接受继承边界；若加强基础Stage须独立评审原语/调用点，当前未改产品。
- CV2：Ruling7明确原Stage用服务器原Lease围栏，没有本地单调deadline；不能将它算作TaskClaim grant/renew send-time证据。Task3必须另行实现并实际验证其本地截止/失活语义。
- CV3：本次只新增lost reply、delayed commit/abort及guard/receipt相关故障，没有重新证明所有malformed/corrupt/grant/cancel路径。现有Stage/receipt/mutation源码与BASE一致；保留 `2026-10-06-etcd-stage-verification.md` 原始实际Stage证据及已封存认证执行验证。若原语改变必须针对改动另验，不把两例扩成穷尽认证。
- CV4：原Lease TaskClaim、renew/release及完整关联链checkpoint CAS为强制Task3工作，初始pending/Reference不授予该能力。
- CV5：持久目标gate关闭、helper接受操作与token两侧排空、remote settled/fenced、精确终止、mount/policy cleanup及最终owner/index/control释放仍为后续强制工作。本次禁止物理成功推断。
- CV6：没有新foreign拒绝/failover/生产TLS/RBAC证明；foreign容器存在不算测试，生产与故障切换门禁保留。
- CV7：四个新成员status字段观察仅为直接诊断；无旧成员恢复/源码binary映射/完整schema升级降级snapshot认证，相关部署恢复门禁保留。
- CV8：宿主Darwin race对Linuxetcd，不是Linux应用race/PID1/FUSE。已有认证执行具体平台证据见 `2026-10-07-authenticated-target-execution-verification.md`，新的Task驱动物理协议必须独立验收。
- CV9：37namespace、固定fixture配额不是大N memory floor/fleet性能上限；主规格已有N×活跃率/申请率/operation/GC/长稳测试矩阵仍需执行，不声称Redis替换已切生产或性能已有提升。

Ruling7的代价是保留既有Stage成功CAS缺少guard Mod和本地deadline的限制，避免未经授权把Task2变为全Stage协议重写；新TaskClaim的四字段、本地deadline和无复活约束保持不变。正式spec/Global已澄清，源代码未修改；review完整记载两侧语义。

## 原始报告与审查保留

OWN `.superpowers/sdd/2026-10-07-cleanup-task-authority/` 继续保存全部raw/来源/报告/review；单元结束统一封存。接受时固定内容：

- `task-2-report.md` SHA256 `86bb9b8a59381c758937cd7e0bed7bd1a65bd4cdc507816f2e90dae9e0fcb8c7`。
- `task-2-review.md` SHA256 `be6a48bdb1cc31b03951f42a73bcc7dcbae12fd6d3ef655c578020bf9a887bb8`。
- `root-task-2-real-etcd-audit.json` SHA256 `b2012a2e4ad079ac3361beab071a5eb684f097432cd2857367dcb418990ed686`。
- `root-task-2-log-classification.json` SHA256 `dedb4fce3e648dd284f24b16d6b1f2b048612368d3a384d3dab9c977b4829b78`。
