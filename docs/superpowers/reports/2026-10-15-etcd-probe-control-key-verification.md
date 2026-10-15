# Probe密钥原生存储范围验收

接受源码`226b543`、`dc08b41`，仅四个新增control_key文件。独立Task review Missing0/Extra0/C0/I0/M0，Root另保留下述非阻断环境证据M1。生产仍使用Redis loader，未接main或执行密钥迁移。

新增operator `ProvisionWorkspaceControlKey(ctx,seed)`和API启动`LoadWorkspaceControlKey(ctx)`。显式32字节seed存入固定namespace永久key，无Lease/version1；当前identity/restore四屏障下CAS，同seed幂等且不改MOD，不同seed冲突。回执不明仅只读仲裁，不重发写入。Load无写入、无随机替代，返回owned Ed25519 key；私钥不进入Pod、诊断或错误，临时清理有固定边界，不承诺擦除gRPC内部复制。

| 验证 | 结果 |
| --- | --- |
| 真实三成员host race | 初始化/8路同seed并发/异seed冲突/owned复制/跨Backend读取/缺失只读通过。 |
| 故障边界 | actual commit丢ACK后1CAS+1read、identity/restore漂移、坏长度/Lease/version、15种坏wire、取消/关闭和最大namespace通过。 |
| 新缺陷回归 | 无效响应临时seed清理真实RED→GREEN；传输错误不泄露seed、不重发通过，旧PASS组未重复。 |
| 检查 | 包vet exit0，format与scoped diffcheck空；read7nodes/provision9nodes，固定reply两分支预检64nodes/256KiB。 |

客户端Go1.25.6 Darwinarm64 race，server linuxarm64 etcd3.6.15；image digest/config与批准fixture一致，日志核对三endpoint的不同member和相同cluster。Root重新核对最终四源码、三实际binary与三源码归档SHA均匹配，读原始positive/focused/final-focused日志及exit文件。最终control_key.go相对实测snapshot仅函数间空行变化，Root检查差异；其他三文件字节一致，不将旧binary描述为最终四文件逐字节实测。

专属项目`probe-control-key-20261015-author`的up --wait仅证明就绪。**环境证据M1：未在删除前保留三个原始container的docker wait/退出码，不可恢复，不补造或重跑覆盖。** compose down --volumes exit0，Root再次按精确project label核对container/volume/network均空。裁定此缺口不阻断有实际race执行证据的metadata源码范围验收，但容器退出检查未验收，不声称测试环境完整可靠性通过。

后续：启动流程接线、operator导入原seed并校验公钥、API只读/初始化身份RBAC与TLS、磁盘备份保护；轮换/旧runtime排空、跨cell、性能容量与完整Redis移除均未由本单元交付。
