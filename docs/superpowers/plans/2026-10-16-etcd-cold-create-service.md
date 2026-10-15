# 可恢复 plain 冷建服务实施计划

> REQUIRED SUB-SKILL: subagent-driven-development。一个完整 Create/Resume Task，多个已验证小提交；不在切片间结束，不创建重复实现任务。

**Spec:** docs/superpowers/specs/2026-10-16-etcd-cold-create-service-design.md

**Goal:** 一条具体可恢复冷建链：持久配额/32准备槽→实际 Pod POST→原 Hello/activation→原 Bind/Mount/Publish→安全准备结算。服务必须提供真实构造及调用入口，不能只交付 factory。

## Global Constraints

- 前置startup R1验收后才 immutable BASE/START-GO；此前只读预检，无源码/Go/native/Git mutation；不读其他 OWN。
- 本单元固定 plain-empty、已有部署绑定 WorkspaceIdentity、正已解析TTL；显式拒绝不支持的配置，不把它当全部业务创建或 main 切换。
- 固定 origin/镜像/namespace/Node tuple/签名信任/受控clock；真实 owned POST/GET/bridge/TLS，无任意 callback、fake kernel 或成功工厂。
- quota/32 preparation 持久无Lease，free index有限候选、原子领取/释放；未知创建、404、失租不退款；warm独立不混用。
- 保留原 publication/release 64-node预算及完整安全屏障；独立单调结算，不在满事务后追加写或提升上限。
- activation 前固定公共恢复材料、私有稳定signer显式配置，重启不换key/Birth；unknown不重发POST、不冒认不存在或死亡。
- 原 Wait/Serve/IO/transport/容器实际join，有限并发/输入/期限；actual17/prepost/member/ELF/script绑定；Root具体审核每次native。
- Go1.25.6/仅apply_patch/Root-only精确小提交/唯一fresh author+reviewer同pair修复，无nested/外部部署/升级/Sentinel/CLOSED OWN。

## Task 1: Complete cold Create/Resume service

预计修改 internal/storage/state/etcd 的新 preparation allocation 领域方法/记录/namespace校验与可选options；新增 internal/runtime/kubernetes 固定 create/inspect driver；新增 internal/provisioning 的具体 service、不可变 capsule 和 bootstrap/material恢复；按需增加 Manager 窄适配但不启用 main。新增独立 collectors/native selector，可复用 tracked startup fixture 源级真实生命周期，不能读取旧产物。

- [ ] Step1 只读预检：逐项确认原 opaque claim、slot schema/bootstrap、POST owner、activation恢复、满预算结算、plain-empty业务映射；给出最小具体接口/有限候选和事务节点数。Root裁决后开始，不因接口不齐另拆一堆成功工厂。
- [ ] Step2 新quota/preparation selector RED→显式operator初始化/持久32与runtimequota/原子freeindex+reservation/unknown只读仲裁；两个Backend实际隔离etcd验证，不启动32进程。达到绿色及时小提交。
- [ ] Step3 具体owned Create/Inspect driver RED→固定完整spec、一次POST、明确未发送/UID/unknown、GET404不补偿、实际取消/Closejoin；限定host/race/vet通过及时提交。
- [ ] Step4 具体service/capsule/公共material持久化及Hello/activation恢复RED→接原Bind/Consume/Read/Publish，不留任意factory；独立prepslot结算与quota保留，稳定typed结果/Resume/workmarker。限定验证后小提交。
- [ ] Step5 新真实native：正式POST触发launcher→原observer/bridge/TLS/Backend发布，受控ACK丢失/原intent恢复且不双POST/runtime；验证预算保留/结算、原所有owner退出/exact清理，逐个具体run经Root检查。不是大N或真实Kubelet验收。
- [ ] Step6 冻结全部来源/报告，唯一fresh完整Taskreview、同pair限定修复、tracked范围验收；继续warm/FUSE/Docker/业务API/调度与最终切换，不暂停用户的总任务。

## Preflight relationships and rulings

| Producer → consumer | Required relationship |
| --- | --- |
| identity/claim → allocation | 当前Backend/restore/opaqueclaim与永久slot归属同时比较；Lease不管理slot寿命。 |
| allocation/dispatch → POST | 封闭service复核持久一次attempt后调用真实driver；copied record不是native许可。 |
| POST UID → Hello/material | 原实际UID与Birth.BootID绑定，公共材料先固定再activation；NodeBootID不同。 |
| activation unknown → Resume | 原材料、原target pinned TLS查询；过期/unknown保留预算与恢复ID，不生成替代。 |
| publication → prep settlement | 原加载器验证immutable committed witness后独立CAS；不改满64原事务，quota继续占用。 |
| safe owner release → quota | 精确死亡/cleanup/原release证据后另结算，不给未发布出生套用已发布释放路径。 |
| service → Manager/main | typed有限构造/调用接点明确；不支持配置显式拒绝，main整体切换依赖仍保留。 |

Ruling: 这是 master 已批准的生产接线单元，连续授权无需再确认。第一条有限创建路径与后续兼容/恢复/运维缺口须如实报告；不能把准备账本或provider单独完成描述为整个创建或生产迁移完成。
