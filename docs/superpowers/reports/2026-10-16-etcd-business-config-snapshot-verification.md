# 业务配置快照验证与验收

2026-10-16，原BASE `9b2de478989cf0fc7b63c6212a23b3763bf17a5b` → 最终源码 `6dcfe5225d17618f71ec9b14de6884d98ef2d02c`。唯一fresh wholeTask reviewer覆盖四次提交，结论Spec compliant / Approved，无Critical/Important；Root完整读取并接受。唯一Minor为既有native配置与主动关闭日志噪声，保留原始结果，分类展示延后，不为清洁日志重跑。

`190e08d` 新增完整typed业务配置codec：requested全字段保存一次，requested digest独立于默认值/时间/服务器ID；resolved冻结首次TTL/四项资源/mount。复制、quantity解析和编码前有界检查，最终canonical业务wire≤61440B；严格UTF8/schema/duplicate/digest/显式请求与resolved一致性，返回独占数据。永久TTL可表达，当前plain服务明确拒绝执行。

`0cac237` 接入plain cold真实consumer。nil业务配置保持v1精确字节；v2包含完整快照。Create在部署读取/clock/AcquireIntent/Grant/POST前拒绝当前不支持的挂载、依赖、网络策略和不匹配的TTL/资源；进行中恢复和历史完成复用严格校验，保持原claim/fences和历史LoadSnapshot。原Backend实际完整65536B envelope编码检查在RPC/Grant之前，未引入近似大小API。

`6dcfe52` 新增固定真实Linux selector，复用原服务fixture。正常v2、activation ACK丢失后Backend/Service重启恢复、原v1兼容三个场景均实际成功，持久化业务配置经coherent读取后逐字段相等。

## 实际验证

- red01为Bash3空数组失败，未运行Go，不计RED。修正collector后red02原执行62cca4/session92594→459472 exit1，实际缺新API编译RED。codec01六组PASS后补resolved mount字符串独占复制；green01遗漏原bytes import导致consumer编译失败，修正后green02六组codec+五组provisioning全PASS。
- green02原3eb8c9/session12844→e41e7f exit0；含全字段、默认冻结、摘要独立、所有权、永久/溢出、精确60KiB边界、strict JSON、v1字节、v2严格性及12类副作用前拒绝。race01原758187/session4861→988c64 exit0，同11组PASS无race finding。最终Linuxarm64 CGO0三包vet02原a6fd6a/session22792→7c9e0c exit0，诊断0B；vet01在最终Linux-only selector增加前，仅作历史保留。
- native01构建9e2bf2/session71454→05a9bd exit0；Root实际运行1dc2f9/session39294→26a6ea exit0，三场景PASS/noSkip。3个正式launcher exit0、9个原bridge Wait/relay/IO及3个TLS etcd SIGTERM均join。attach/container Wait0、无OOM；精确容器345bffaf646f39f62fa3ca134c8d02fadd83f922cb7182e398ddc5ad868c144e已删除，Root重新查询inventory为空。
- Root独立核对actual17 GoList输入/prepost/归档逐常规成员/current、collector/helper、真实ELF及Go1.25.6 Linuxarm64 CGO0 buildinfo：green02 8903/race01 8908（matching-race）/vet02 9127；native runner9125/launcher2006。runner SHA9d05232ab8e766c6d86372d2a9fc90df08cc992883e5bb5083feda523264b962，launcher SHAa6468af42393a789ade6194c1d1e1fc6ab31799cd93b5537d577bc1d7e01a277；均实际AArch64静态ELF。reviewer核对实际raw和行为接点，未重复来源归档审计或重跑测试。

日志保留单HTTP/gRPC端口/simple-token配置提示、主动shutdown的ServerClosed/closed-listener诊断；成功与完整join证明本次场景完成，不声称日志pristine。验证只覆盖限定固定场景，没有新增完整Destroy/owner-release验收或大N/吞吐/RSS/生产容量验收。

Main/HTTP/Manager仍未切换；无workspace身份、业务幂等、freshhealth与operation接线、调度、FUSE/collector、部署恢复、Redis移除仍属总迁移后续工作。本单元不是生产迁移完成。
