# Cleanup：终止完成后仍持有 owner 的恢复

本切片基于 `39dfbee7b7d267c3234d2a76d7728a87705d8e36`，完整 Task 仍待审查，
生产迁移、容量和性能验收未完成。

## 改动和权限边界

真实终止及 provider 清理已完成，但进度写入未提交时，原 E 可能仍是
`local-complete`、完整 `pending.termination`、原 Run 已实际 joined，owner 尚未释放。
原 loader 只接受 reserved intent，新 worker 因此停止接续。

现在原两 RPC 历史读取可认证独立 completed 模式：签名终止证明、enrollment、历史
stamps、provider 七槽完成状态和摘要仍由原校验器认证。新私有 matcher 另校验 Task、
birth、完整 Pending，并保存原 termination/provider 的完整 native fences。
completed 记录合法地有 Version>1、Mod>Create；任何替换或修改仍须拒绝。

原 registry job、bound manifest/目标、43 节点新 Run CAS、原预算 managed Claim
保持原流程。新 Claim 下再次认证历史和两 fences，停止并实际等待续约后，直接进入
`ReleaseTaskWorkspaceOwnerLocal`。该入口再次验证真实 authority/history/本地目标，
执行原 60 comparisons+4 writes；Claim、目标、Run.Joined、proof/settle 等收尾仍复用原链。
completed 路径不再次 Finalize、Terminate、observer Query、修改 provider 或创建归档。

同一模式也支持 `terminated`+完整 `pending.owner-release`，本切片对此有元数据测试；
本次新增真实故障测试覆盖的是前述 `local-complete` 场景，不宣称另一故障也已实测。
旧 reserved/observer 恢复规则不变；无签名完成历史、Pending 不符或原 Run 未 joined
仍不能恢复。无新公开权限接口、新协议或新增事务形状。

## 验证

| 实测 epoch | 结果 | 原始日志 SHA-256 |
| --- | --- | --- |
| owner-held-red-01 | 修复前真实失败 8.75s；第二 worker 冲突保留、0 Claim | `dc4d738d6756837ee616675fe257de233c29ed8819030b99bfaf9c84e5b00e58` |
| owner-held-native-01 | 修复后通过 9.44s | `be68f9ac49b341a240bdd92f1ac955ab6d0233e67a69a4eff24922a22e5e51c5` |
| owner-observer-native-01 | 原 observer ACK 丢失回归通过 13.11s | `5e767eaffbd23c0f483248ad43556416ae58f2b98f9a94f8619cabfa4c944276` |
| owner-bound-native-01 | bound archive 回归通过 9.06s | `fd008871e204eb79364cc74542e75ec3a88336aef5824410bb7434db7b58468a` |
| owner-mounted-native-01 | mounted loader 回归通过 5.99s | `d85c02a6bb9da15106c005124458f3ff42b22ecc5548fe2ebe79bcd7301c3df1` |

故障发生在真实 Terminate 返回后：proxy 将一次 proposed terminated progress 请求
拦在上游前，等待原 deadline；matched/not-forwarded/joined 各 1、committed/revision
均为 0。第一 worker 的完整 E 只改变 Run.Joined，原 Pending 保持，真实 loader
认证 cleanup-complete，原三个 owner 键仍在、release 缺失；实际 Wait 后才启动第二个。

修复后第二 worker 仅一个 Claim、两个 Grants（session+Claim），归档/终止/provider
写为 0、bridge 增量为 0；五组历史（Task、终止、provider、归档、本地完成）的完整 KV
保持，三个 owner 键删除，真实 release、settled E、空 S、内容/mode/重复调用、provider/
cgroup 和两个 worker Wait=nil 均通过。原 observer 回归仍是一次 Terminate 加一次 Query。

定向 host/race 均为 4 组、110 子例，分别 1.463s、3.386s，无 DATA RACE；覆盖
CompletedTermination、TerminationReservation、BoundManifest、RecoveryPhase。
host 日志 SHA `51edc4a24045bd2d47ecfb40a66f5162b1d1a02a73a01a3fae6a34cc3d132a5c`，
race SHA `c90ebc6342afd94ec4d02513ea5f8f202908fdaed0e665a8113deabde4fa60c6`。
首轮 owner-host-01 失败在新增 fixture 编码：通用 Task 的 uid/boot 不是 provider 所需
UUID。仅对齐新 fixture 的 UID/BootID，产品校验未改；原失败日志及全部输入保留。

Linux etcd/controlrunner vet 通过；gofmt-d、diff-check 及人工注释/分段/包职责复查完成。
最新 Go1.25.6/linux/arm64/CGO0/non-race runner SHA
`cff4253106a8c6dc1017f1ce0be8a8e71bafefd81ccc40f9c4a02c9d31d01151`，monitor SHA
`8a9c853829554c99ed5b315de7fee207addc79a1b7a96f19e38f227fecce06c9`。
每个 Go epoch 的全部 17 类输入、源码/工具前后、每份 archive 均独立核对；每场 native
后重新核对二进制输入。真实容器配置、输入、工具归档、退出与清理均独立审计。
RED 的审计通过不代表 native 通过。所有容器无 OOM/超时、精确 CID 已删除、label 清空。
各场 3 条 error 为 etcd shutdown；warn 包含启动提示、注入 deadline、清理 lease-not-found，
observer 场为 7 条、其余为 8 条；无 gRPC WARNING。mounted Close 保留原 unknown/canceled 结果。

completed 历史校验与直接释放职责集中在同包私有 owner 文件，复用原协议及真实执行链。
下一步为完整 cleanup Task 审查；后续生产接线、自动发现、FUSE/collector、大规模容量、
部署恢复和 Redis 移除仍需继续，不将本切片当作生产迁移完成。
