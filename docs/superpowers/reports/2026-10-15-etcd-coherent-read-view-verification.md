# 原生一致读与只读复核验收

范围接受：`970ce46`、`3b5178f`及测试修正`04ac585`。仅新增四个`domain_read_view*`文件，未接manager/main，当前生产仍Redis。

Workspace/ID先有界发现，再同一revision读取owner/fence/placement/control；发现记录变化则冲突，不跟随新对象。返回owned副本，私有self/backend/key/MOD证据不能由调用方拼装。Confirm只读比较四MOD与identity/restore屏障，拒绝同值改写、删除重建ABA和伪造view，不授予健康、期限或后续副作用权限。

| 验证 | 实际结果与边界 |
| --- | --- |
| 新增host race与真实三成员 | 九个顶层case/56子case；两项纯host，其余七项实际etcd；无endpoint SKIP。read-boundary与confirm各限定组PASS，真实RED保留。 |
| 最大namespace修正 | 原509字节样本经唯一M1修正为512并断言；仅重跑改变的WireBudget，host race PASS，未重跑native。 |
| 有界payload/wire | 四record合计13312B；最大模型14nodes、12569B request、16273B domain reply；实际workspace/ID读12/11nodes、Confirm10nodes，低于64nodes/256KiB。 |
| 调用边界 | read最多两次应用逻辑提交，Confirm一次；应用无自动重试循环。不是client传输层所有物理尝试次数保证。 |
| 静态检查 | 作者包vet、own-file format和scoped diffcheck通过；Root核对精确提交和新旧测试epoch。 |
| 独立审查 | 一次Task review Missing0/Extra0/C0/I0/M1；同一reviewer限定复审测试修正后C0/I0/M0。 |

环境为Go1.25.6 Darwinarm64 race客户端、linuxarm64 etcd3.6.15三成员，cluster `f45703ebe16eae30`。实际image digest/config与批准fixture一致。编译包含另一Task并行的独立package源码，按各epoch记录，不声称孤立HEAD构建。

Root重新核对最终四源码SHA与read-boundary/confirm/maximum-namespace三个实际binary SHA，全部匹配；读取原始RUN/PASS日志及最终最大namespace日志。最终测试修正只改变测试fixture，旧native日志仍绑定原source epoch，未改写为最终测试源码运行结果。

专属compose项目`sandbox-coherent-view-20261015-1`实际up/stop/down和原Docker wait已回收；wait命令exit0，四container退出码原样`137,0,0,0`，首个137原因未知，不推断OOM或stop-timeout。Root再次点查该精确项目label下container/volume/network均空。测试PASS与清理退出码分开披露，不宣称整轮可靠性或容量通过。

本单元不包含snapshot payload/LRU、受控时钟、健康准入、业务接线、scheduler或生产切换，也未验证大量已有sandbox的容量/性能。
