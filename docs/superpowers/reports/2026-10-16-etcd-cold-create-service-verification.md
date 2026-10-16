# 可恢复 plain 冷建服务验收

2026-10-16。范围基准 `028593ffae0ecf8781c3f27d892d9cdc3270198a`，最终源码 `ba88a12663b2c459d4db66bd248baad8c50d6bd8`。完整单元通过唯一 fresh reviewer 的初审及同 pair R1限定复审；I1、I2、M1均已处理，无新增 Critical/Important。Root独立核对来源和实际运行证据后验收本单元。

## 已实现

- 显式 operator 初始化、runtime quota与全cell32个持久preparation槽、固定候选free index及原子双槽领取，不按已有sandbox数创建常驻控制器或扫描全部slot。
- 固定部署配置的具体 Create/Resume 服务、原提交专属一次POST许可、原目标完整spec/UID检查；未知结果或GET404不补发创建、不退款。
- 原Hello/activation、发送activation前固定公共材料、稳定私有签名配置；丢ACK后关闭并重建service及Backend，使用原材料恢复。
- 原Bind/Mount/Read/Publish接线；独立39节点准备结算认证13项留存历史，归还preparation，保留runtime quota。原publication及owner release的64节点事务不改。
- typed结果携带原request/intent/stage/attempt及runtime诊断。早期失败保留诊断副本，实际目标和权限仍由Backend原记录认证；完成结果不授予健康或执行能力。

## 小步提交

库存与领取：`5df6680`、`adcea58`；owned provider：`0d8698e`；原始attempt与UID：`8e2afc4`；公共恢复材料：`b99b7dc`；构造及capsule：`2399c8c`、`afbf32a`；历史结算：`104ba8c`；完整service接线：`4e25b11`；R1诊断回归：`62752b5`；共享collector/退出管理：`ba88a12`。计划同步另有独立文档提交，没有合并成一个大提交。

## 实际验证及范围

各组件的限定host/race/Linux vet与真实TLS etcd测试分别在其冻结来源上完成。最终R1新增诊断host测试先实际断言失败，再通过；race通过，Linux provisioning vet原日志为空。最终实际17字段输入数量分别为8877/8888/8966。未声称最终源码跑过全仓库race矩阵。

最终真实service `r1-service-green-01`：runner9101项、正式launcher2003项来源；原run `b20828 / session59285 / a24afb` 退出0。普通与activation ACK丢失两case通过；后者关闭原service及Backend，冷重建后验证原UID/BootID、原材料不变、伪造diagnostic不改变目标、早失败副本及完成重放刷新。每case仅一次POST/正式runtime，prep归还且runtime quota保留。

六个原bridge、两个正式PID1、两个原TLS etcd及observer/clock/IO owner完成join。attach/container Wait为0、无OOM，精确容器 `eb4eae0f85e2bb4266539dd3cc5d42bf05a0a79ca78ac82786a7a1048e5ae571` 已删除，所持inventory为空。

独立结算最终 `step4-settle-green-02` 的四项真实etcd case覆盖commit/guard丢ACK及exact reference、两个Backend仲裁、prep复用、index缺失/历史证书过期和篡改拒绝。实际完整事务39节点。原run `1b9961 / session72920 / 15b6a1` 退出0；原Backend与etcd退出，容器 `039be95acf517a2c0d40dd9e5e4efdb65f19d4a8494457d3035e5f0608b738ab` 精确删除。index删除是元数据故障，未冒称实际安全Destroy。

共享collector的第二wrapper最终 `r1-shared-close-green-01` 只运行既有attempt-close窄selector；原run `d37056 / session67273 / a69bdb` 退出0。runner9101项、Backend9010项，原Backend Wait=nil、etcd SIGTERM后join；attach/container Wait0、无OOM，容器 `ae8e75bd165551bfccb700aa61d46b66fde96221e52cd0c0aa3f65c7387afc97` 精确删除。没有重放旧整个矩阵。

Root核对最终所有actual17输入集合、前后SHA、归档每个常规成员与当前字节；实际ELF64/AArch64、Go1.25.6/Linux/CGO0及二进制SHA正确。两个wrapper连同共享helper分别在source前校验，归档精确两成员且pre/post/current一致；原Docker create安全及资源参数逐字保留。最终runner SHA为 `5bf78e79e9ccc6c70191d1651dc3c2380fe27b985c59b421c81d153147037cf8`，launcher为 `b1b58cd7ec51060c526c1c9ebf660aab9fe5cb8cec4abec251756596fc67c9af`。

## 历史与限制

原RED、构造UUID失败、guard注入器失败、未执行的build-only epoch均保留，没有覆盖或改称成功。etcd隔离fixture的单端口/simple-token及关闭listener诊断保留原文；attempt-close保留39条关闭client后的LeaseRevoke重试，编号0–38。这些仅结合真实PASS、原Wait及精确清理作生命周期分类，不称日志无警告。

当前支持固定plain-empty、已有部署绑定WorkspaceIdentity和正已解析TTL。full Manager/main仍未切换；runtime quota安全归还、never-expire、初始下载/依赖、warm/FUSE/Docker、业务认证、scheduler/due、证书维护、未发布出生回收、容量与生产部署恢复、Redis移除仍属后续工作。本fixture不是实际Kubelet/CRI/OCI打包验收，少量真实runtime不证明大量存量或吞吐性能。总迁移继续，不能将本单元称为生产迁移完成。
