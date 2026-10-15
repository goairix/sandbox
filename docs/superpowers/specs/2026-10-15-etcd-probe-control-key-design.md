# Probe 签名密钥原生持久化

用户在已批准的完整etcd迁移中询问probe密钥存储，已说明scope永久保存/CAS初始化/启动内存加载/迁移原seed/TLS与RBAC边界，随后要求汇报并继续。此单元落实该接线前置，不启用main或替代生产Redis。

## 接口与记录

`(*Backend).ProvisionWorkspaceControlKey(ctx context.Context, seed []byte) (ed25519.PublicKey, error)`为operator初始化/迁移入口。必须提供精确32字节seed，内部复制；不自行生成、不接旧Store、不扫描Redis，不提供覆盖/删除/轮换接口。首次安装的随机seed由operator调用方生成一次，迁移显式传入原seed。应用普通身份只需加载权限；实际RBAC/部署在后续接线完成。

`(*Backend).LoadWorkspaceControlKey(ctx context.Context) (ed25519.PrivateKey, error)`为API启动只读加载入口，返回独立内存key。调用方用已有Kubernetes `WithWorkspaceControlKey`安装；签名时不再请求etcd，Pod仍只有公钥。本单元不改变已有runtime函数或main。

记录固定在本Backend namespace的`meta/workspace-control-seed-v1`，value为原32字节seed，永久无Lease，单次创建、version1/create=mod；无额外schema记录或N相关状态。第一版单cell，authority/scope/cell/cluster与现有Namespace和Identity严格一致；不引入跨cell共享协议。

## 正确性与故障

- 全部提交保留现有identity/restore四比较；当前Backend已由New严格绑定，空keyspace不会初始化identity。metadata漂移、取消、关闭、RPC错误不允许返回替代key。
- Provision只提交一次应用层CAS：key不存在时Put原seed，不带TTL；已存在必须严格校验且与输入seed完全相等才返回公钥。同seed幂等不改MOD/version，不同seed冲突且不覆盖。
- CAS回执不明，仅用有界只读查询仲裁相同seed，确认精确永久记录和当前operator屏障后可返回；无法确认返回unknown/错误，不重新发写入。client传输层既有行为另计，不宣称物理尝试数为1。
- Load只有有界点读Txn，无写入、Lease、Watch、每sandbox actor或缓存；缺失、错误长度、附Lease、非初始version、损坏响应均失败，不新建key。
- 对请求/响应做固定节点/字节预检及严格cluster/revision/nestedheader/key/count/more/KV lease/create/mod/version/unknown字段检查，沿用64nodes/256KiB上限；不复制通用框架或放宽现有validator。
- 私钥/seed不进入日志、错误、诊断、Pod、snapshot或测试失败值打印；返回owned数据，错误和临时副本清理。TLS/RBAC不等于静态加密，磁盘/备份保护及operator/API凭证隔离为部署义务。

## 验收

真实三成员etcd验证首次初始化、同seed并发幂等、不同seed冲突、跨Backend/重启加载公钥一致、输入/返回值复制、缺失/损坏/Lease/version拒绝、identity/restore漂移、取消和RPC故障、CAS实际提交回执丢失后只读仲裁且应用写提交计数1；最大namespace/wire边界。仅新增host race/必要native用例与包vet，不重跑已验收suite。保留原始源码/binary epoch、失败、实际Docker wait和精确fixture清理；不将有限测试当大N性能/生产切换验收。
