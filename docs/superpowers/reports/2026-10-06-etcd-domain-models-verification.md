# etcd 领域模型验收

日期：2026-10-06，分支`codex/etcd-state-management`。范围为domain identity、records、codec和固定keys，未接入Manager/allocator。

已交付：稳定length-framed workspace identity（provider/storage identity hash/bucket/canonical prefix）；七类schema1永久记录；ID→partition placement；严格且有界的JSON codec；compact snapshot digest保留<>&、U+2028/U+2029、大整数和数字表示；UTC期限与非空身份校验。原始storage identity不留在WorkspaceIdentity字段，principal/key只生成hash。

独立spec与quality review均PASS，本批无剩余必要P1/P2。修复了nil runtime wire省略、创建尚无UID时不能表示cleanup pending的问题；active/exclusive仍须exact runtime，cleanup中的nil只表达未知，不证明absence。codec采用闭合类型dispatch和泛型fresh decode，不使用reflection；typednil和失败不修改目标、缺失字段不继承旧值均有回归。private codec没有授权能力，namespace/完整key/value/restore关联由下一领域读提交验证。

实际测试：行为测试先RED再GREEN；最终主审`go test -race ./internal/storage/state/etcd -run '^TestDomain' -count=1`通过（2.001s）；实现者与独立质量审查另跑race/vet通过；实现者`go test ./...`通过，部分包命中缓存。未将unit结果作为真实etcd领域事务已通过的证据。模型文件gofmt和diff检查通过。

后续原子Acquire写request/owner/fence/intent/control/snapshot/placement与receipt；publishing仍不授予runtime创建/执行权限，须后续live claim和gate。owner释放与runtime绑定不在本提交。用户Sentinel计划修改未纳入。
