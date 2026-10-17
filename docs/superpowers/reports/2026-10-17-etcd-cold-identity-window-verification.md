# 冷创建身份与业务窗口修复

## 问题与改动

真实 DELETE local 场景 `destroy-native-local-03` 未能完成归档。冷创建把运行时
receipt identity、publication、activation 的共同截止时间截到业务 TTL（60 秒），
而原归档协议要求 identity 覆盖准备时刻之后约 60 秒的完整会话。原 worker 已实际
进入关闭及排空，共 14 条桥连接完成 join，随后保持未完成状态；该失败不作为验收。

`PlainColdService.material` 现在使用两个窗口：receipt identity 保留原 15 分钟上限及
issuer 上界；publication/activation 仍取身份窗口与原业务到期时间的较早者。原业务
到期字段、两秒准入余量、签名绑定及协议验证均保持严格。长期证书更新仍需后续实现。

## 验证

新测试调用真实 material 签发并通过原签名验证器检查，未启动 etcd 或伪造物理释放：

- 60 秒业务 TTL 后的第 90 秒，receipt identity 仍能认证并覆盖后续归档窗口；
  publication 与 activation 已拒绝业务使用，identity 在原 15 分钟上限后也拒绝。
- issuer 有效期分别为 120 秒和 45 秒时，上下界仍受 issuer 约束，不重写业务 TTL。

定向 selector：`^TestPlainColdMaterial.*$`，2 组测试、2 个 issuer 子例。
RED 确认两项原行为失败，45 秒 issuer 子例通过；修复后 host/race 全部通过，Linux
arm64 vet 通过。每轮实际 17 类 Go 输入、前后摘要、source/tool 全归档及当前字节均
由 Root 独立审计通过；host/race/vet 输入数分别为 9045/9050/9134。

| 原始记录 | 字节 | SHA-256 |
| --- | ---: | --- |
| material-window-red-01 | 1462 | `83c18c1925646ea2b264195a62dcb4ddaca0e843acf76405f96f4377a34b1fdc` |
| material-window-host-01 | 652 | `5dea7c0306d57bcc0edb4136f4367a5cae6be935d3cd8e7f17ca081bf219e692` |
| material-window-race-01 | 652 | `6234026174b4a1ece8f98e65d5e4fc38ff8812aa25c83681d8c2f60e1531e4a2` |

人工检查了窗口约束注释、函数逻辑分段和包职责。本片留在原 provisioning 签发边界，
不改协议包、自动 worker 或配额语义。原 native 失败日志保留；修复后的真实 HTTP
销毁与物理清理组合尚待新的 ELF 验证，不能以本片替代整体迁移或容量验收。
