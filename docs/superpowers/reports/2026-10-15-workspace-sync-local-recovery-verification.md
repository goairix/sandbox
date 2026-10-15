# 新claim本地普通文件冷恢复范围验收

接受源码`509632a/d7dc18b/fbe66c1/a46d203/5a78262`及审查修复`f68923a/d8e6eb7`。独立Task review初轮C0/I2/M0，原两问题限定复审通过；Root另发现合法不可写mode恢复缺口，单独修复与同一reviewer限定审查通过，无未解决源码阻断项。主程序仍Redis。

公共RecoverTaskWorkspaceSyncLocal现在完成cold bound归档、fresh claim/完整历史与认证query、实际合作writer排除、旧charged intent独立修复、新文件reserve、terminal、原子checkpoint/free/delete和冷重启继续。新source/body/charge/completion不重建旧发送能力，不填旧draft；旧outcome仍unknown。固定64归档owner与S1..64槽扫描，无按已有sandbox数增长的map/actor或全prefix发现。

| 范围 | 实际验证 |
| --- | --- |
| 正常继续 | 两文件冷恢复、再次冷重启只完成剩余文件且已完成文件inode/mtime不变；65文件/455B、S1连续回收通过。 |
| 不确定与中断 | 实际partial32768/70001、truncate/fsync/close中断后fresh repair；terminal/GC提交ACK丢失后只读仲裁、写次数1。 |
| 当前与历史权限 | 实际31秒签名会话/producer窗口过期后只读query继续，DATA/双COMPLETE/EOF/Close错误拒绝；claim失效/history ABA/TAR/target/boot/ns变动拒绝。 |
| 关闭与内存 | write/fsync/close/query/terminal/GC真实工作join前同inode重开阻挡；metadata scratch在gate内释放引用、无逃逸slice；I+4M收费未放大，不承诺即时RSS释放。 |
| R1 | monitor独立登记、前台等待而非误报失权；terminal仲裁保留原generation，postguard超时后只读恢复。两实际RED→GREEN及当前positive通过。 |
| I3 | 旧put已chmod0000/0444后不确定时，私有O_PATH/procfd inode锚定临时写权限，fresh修复后恢复计划mode；两实际RED→GREEN及当前positive通过，无新kernel6.6下限。 |
| 原生预算 | 当前/历史50pins，whole-op-prefix与budget完整；GC61比较+3写=64nodes，256KiB预检；原record/schema/旧validator保持。 |

环境为Go1.25.6、实际Linuxarm64 CGO0非特权worker、三成员etcd3.6.15、固定512MiB且隔离网络fixture。仅跑新增组和机械抽取真正受影响的旧local/settlement兼容组；分批PASS不等于完整容量/可靠性验收。host race最终2.469s，affected vet/cross/format/bash/diff通过，原输出与源码epoch分别保留。

Root读取各选定原始RUN/PASS/失败、actual原始三child Wait/attach Wait/精确owned清理；R1与I3故障RED保持exit1。最终29本Task源码SHA、最终native编译所选264源码全部匹配；41个历史native binary manifest全部重新核对。353项必要稳定证据SHA封存，排除live ledger/report/review，不把旧epoch或failed binary说成当前源码全套测试。

**失败证据限制：** `authority-green2`开发自锁修复前发生240秒超时，原三child Cmd.Wait缺失且不可恢复。Root核对精确停止容器exit2/OOMfalse、保存日志后仅清理该ID，精确标签资源空；attach Wait不能代替原三Wait，该epoch始终失败。其他编译/fixture失败及etcd明文隔离fixture/关闭warn/error原样保留，不称输出pristine。tmpfs/进程重启不等于断电或主机丢失验收。

尚未完成：目录与删除多余项、durable完整sync-complete、实际runtime/基础设施终止和owner释放；云provider重试/分片/远端结算、生产API/Pool/Docker/Upload接线、分区调度/FUSE/collector、大N容量、部署恢复及Redis移除。本单元结果只授予owned进度诊断，不授予释放权限。
