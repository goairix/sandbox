# etcd live sandbox Lookup 验证记录

基线为 `2106f825915160b5afc6b51caf6dc8bf3eb9f07d`；产品及测试提交到 `93f7e4f7d67262cf7ae99521d2cf5597bf27889b`。本记录只覆盖普通 service 按 ID 查询及实际运行时健康验证。唯一原审查者完成 fix round 1 定点复核：R1/R2 均 ADDRESSED，无新增 Critical/Important/Minor，任务验收通过。

## 交付与提交

| 提交 | 交付 |
| --- | --- |
| `b3dc75bc` | 有限健康协议、原 Session 路由和 Journal 无 IO 标量观察 |
| `4a1a71f2` | 同 Backend 原 read view 绑定的完整历史 runtime/dispatch 读取 |
| `8ca3712c` | 实际 Supervisor 受保护健康入口及状态重验 |
| `cf6b5e40` | `PlainColdService.Lookup(ctx,id)`：当前 metadata、原创建时间、owned 配置、真实健康及最终确认 |
| `223c2d20` | 原正式 launcher、真实 TLS etcd 和 Kubernetes bridge 的 v1/v2 集成验证 |
| `93f7e4f` | TLS 首次握手即保留 record 边界；严格区分裸 FIN 与认证关闭；两项回归 |

Lookup 沿原 service 的单次准入、并发预算、20 秒上限及 Close 取消/join。读取原 snapshot 和完整历史链，保持公开 WorkspaceIdentity 验证；不从 hash 反造身份。CreatedAt 来自不可变 dispatch，当前 expiry 可以已续期。健康查询绑定原固定目标、mTLS pin、当前 identity/gate 和内部随机 nonce；帧各不超过 4096 字节。不采样 `/proc`、扫描进程、创建用户进程或写 etcd。

成功前检查受控 UTC 的 `now+1s < current expiry`，并最终确认同一原 view、context 与 service Close 状态。返回 DTO 只代表查询时点观察，不能作为后续执行权限。

## 审查发现及修复

唯一任务审查发现两个 Important、零 Critical：Go TLS 预读可能把 close_notify 后的原始尾字节留在私有缓冲中；实际服务端可能把记录边界的裸 FIN 当作健康请求的正常 EOF。

R1 使用真实 TLS 服务端生成的响应密文和 close_notify，加尾字节合并交付；旧 Query 一次读入完整 451 字节后错误成功，干净对照通过。R2 对实际正式 Supervisor 完成 mTLS，发送正确健康帧后仅关闭 raw 写端；旧实现的 v1/v2 均返回有效 open health。两项 RED 的失败原因均是错误健康成功，原连接/进程已清理。

修复共享读取器只保存 5 字节 TLS header、剩余长度及错误状态，从第一次握手读取开始限制单个 record 的边界，不缓冲密文 payload、不增加读取协程。裸 EOF 转为不同于标准 EOF 的 wrapped truncation；完整最终字节伴随 EOF 时先交给 TLS 认证，非 EOF adapter 错误保持。健康客户端保留原 raw terminal 检查；服务端保留同一原 archiveDeadline 对象。旧 public Open 和旧客户端模式不变，实际正常 publication、TaskClose 和健康路径继续通过。

## 实际验证

固定工具链 Go 1.25.6。下表为修复后的最终源码；早期失败与通过均保留，未重写为最终通过。

| 验证 | 结果 |
| --- | --- |
| 健康协议/Journal targeted test | PASS，无 skip；含 coalesced-tail、首握手 record 边界和 n+EOF/adapter 错误 |
| 相同 targeted race | PASS，无 race 报告 |
| controltransport/controltarget/controlrunner/provisioning/etcd Linux vet | 退出 0，原始输出为空 |
| 4 项旧 wire route smoke | PASS，无 skip；未宣称完整旧矩阵 |
| 先前 Supervisor/Lookup host 与 race | PASS；外部 etcd 用例因缺端点明确 skip，其原公开 API 断言在实际 native 中执行 |
| 实际 `TestLiveLookupNative/v1` 与 `/v2` | 均 PASS，无 skip；整次测试 3.79 秒 |

实际 native 覆盖：正常 Lookup、返回配置/history 的所有权、只读性、TTL 续期后原 CreatedAt、固定 profile 拒绝、原 view 的 nil/复制/foreign/stale 拒绝及旧公开 API、健康前后保守 TTL、inactive、健康过程中同值 control 改写导致最终确认失败、实际 child 启动后的取消/Close、裸 FIN 拒绝、签名 TaskClose 后实际 gate 关闭拒绝。

原始 host/race/vet/runner/launcher 分别捕获 2249/2256/9161/9157/2020 个实际 GoList 输入。Root 独立核对平台/race 参数、前后路径与 SHA、每一个 source/script archive 普通文件成员及当前源码。runner 与正式 launcher 的实际 ELF 均为 Linux arm64、Go1.25.6、CGO0：

- runner SHA256：`ec46cd51e0900cc7672c23d5d9b763a64010078f033a939f9f7fdfc2594130d9`
- launcher SHA256：`519c2cae10f5a619abcf29d7fbd0b38c6ac80a90250bd209029e14d2ee8af3ec`
- 最终 native 原始日志：15390 字节，SHA256 `cc36cbf9bad067f8ef91f1c982583b64ae0267d3f78c9cc3a614d141a7a8715c`

实际 attach 和容器 Wait 均为 0、无 OOM；普通 bridge 原 Cmd.Wait/relay 均成功，4 个故意取消的原 owner 均物理 join，两次正式 PID1 Wait=0、两次原 etcd SIGTERM Wait 已 join。固定隔离配置及 3 个只读绑定前后匹配。精确容器 `28b78c4662a4253e65c1da100cadc04197aca785c61a34196e1bf836586097e2` 已删除，任务容器 inventory 为空。

## 边界与后续

当前 Docker 使用缓存的固定 Linux arm64 kindest/node 镜像 `26ad49ad…` 仅作外层文件系统，entrypoint 为上述已核对测试二进制；不运行 kind 服务。生产 runtime 镜像及部署环境不在此验收范围。fixture etcd 的 combined-port/simple-token 警告、显式 SIGTERM 的 listener-closed 错误，以及故意取消 bridge 的 closed-pipe/killed 信息均保留；普通路径的清理检查没有放宽。

本任务通过不等于生产迁移完成。正常 HTTP/main 仍使用 Redis；实际业务操作接线、分区调度、FUSE/collector、历史回收、容量与故障恢复、部署切换和 Redis 移除仍需实现或验收。小规模 native 不构成大量已有 sandbox 的性能/容量结论。
