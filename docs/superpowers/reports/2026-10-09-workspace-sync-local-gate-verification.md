# 原始 local target 与写前门验收

本单元交付只读物理目标校验及原始 one-shot native 写前门，尚未执行 provider 文件写入，也未完成持久终态、退款、安全释放或生产切换。主程序仍使用 Redis。实际 local IO 由当前计划 Task2 接续。

源码提交：`fac3953452e1441b2275d7f42d9438010b132e65`（v2 intent/target/gate 与 host 验证），`7bc4ff6d84e644d65863f1840c912c909fe65fb8`（实际 Linux fixture），review 修复 `6b411a05492ba38e77d34dad46e9b52e00298b7a`。每次 exact-path 小提交持本计划 LOCK_EX，校验分支、基线、精确暂存范围和提交前后空 index；不包含无关 Sentinel 文件。

## 已交付约束

- 私有入口消费原始 worker/source/body/target；root 是受信配置解析后的 workspace root。只读 openat2 保留实际 FD、dev/inode、mount ID、filesystem magic/name、非零 UID/GID 和 0700 权限；ext4/xfs/btrfs/tmpfs 明确允许，其余拒绝。重新观察 root 路径和 FD，检测替换、权限或 worker 身份变化。
- v1 canonical 记录行为保留；v2 local profile 包含完整物理诊断，仍在原 B4096..16384 的收费 intent 内。只原始明确成功 ACK 加 postguard 设置私有 acknowledged；unknown/arbitration、冷诊断和首次配对同字节 ABA 不授予效果权限。
- 新只读 Txn 保留全部 50 历史 pins、完整 operations prefix 和 budget 条件，同快照 point-read 原 intent/slot 并检查原出生 revision、canonical bytes、charge/digest。实际 59 helper nodes、11517 request bytes、2629 reply bytes；继续受原 64 nodes/64KiB record/256KiB RPC 上限约束，无新永久 KV。
- 原始 one-shot 在请求前消耗；source/body/target 与 guard 全程登记 Add/Wait。nativeMu 只覆盖原生响应完整验证，随后释放再做物理 postguard；原 owner 登记仍保留。取消不会提前释放真实 IO/guard owner。
- Linux TestMain 仅显式模式启动实际非特权 worker、三个 loopback etcd child；默认 host 不 bootstrap。旧 Docker 全局 fault fixture 的 ID 校验不变，新窄 test hook 只核验实际保留的三个 child 与三 member/cluster 身份。

## 测试与独立核对

初始最终串行 native：14 个新实际 local 叶子加 7 个原 v1 回归叶子，0 FAIL/0 SKIP、client/protocol WARN0。原 host race 两包全量：2445 PASS 叶子、1577 SKIP、0 FAIL；host workspace-sync 子集 57 PASS/49 SKIP，不能当作 native kernel 验证。初始 vet、Linux amd64/Darwin arm64 CGO0 编译、format/bash/diff 均 exit0。

独立 review 发现两个 Important 与一个 Minor：未完成 child/attach 的所有权丢失、直接 missing/null decoder 用例遗漏，以及 postguard 仍持 nativeMu。修复后：

- host race 直接 decoder 缺失/null 十场景与 v1 strict-record 回归 exit0。
- actual postguard 语义 RED 确实失败锁释放断言；GREEN 15 个实际 local 叶子、0 FAIL/SKIP；最终精确 source 重建的 postguard 单叶再次通过，验证三个 Close 等待原始 guard。
- 实际控制器 timeout exit1 保留独立 session 的原 supervisor/attach 和未完成 child/log owner；显式释放后原 supervisor 实际 Wait attach（预期 exit1），三 child join/full raw logs 完整，单独 reap exit0，所属 container/volume/network 库存为空。seam 仅延迟真实 cmd.Wait 返回后的通知，不证明不可杀内核进程。detached supervisor 的 ending/随后缺席单列，不声称 parent Wait。
- 首 pending 试验因外部 runner 进程组清理失去原 attach Wait，reap 正确拒绝；该试验保持失败。只在确认原 host owner 已死、原容器终态及真实三 child joins 后显式失败回收，未伪造 Wait 或称成功 reap。
- 变更覆盖 vet/cross compile/bash/format/diff exit0；没有重复全量 2445 host suite。限定复审确认原代码 findings 全部 addressed、没有新增代码 C/I/M；报告中的启动错误时间归类已纠正。

Root 独立审计初始 127 和修复 155 原始 artifact SHA/bytes、12 个最终 source working/Git、两个最终 compiled source snapshots、实际叶子数与完整 child 日志标记、server JSON 类别/数量、covering exits 和 fresh exact inventories；未重复作者 suite。原 manifest 与初始报告前 13354 bytes 保持原 hash，失败记录保留。

服务器日志没有过滤：初始串行 60 WARN/12 ERROR；最终 postguard 62 WARN/12 ERROR；保留 pending 场景 174 WARN/12 ERROR。startup plaintext/token、health/election 与 teardown/peer 类别单列，green native client/protocol WARN0 不代表服务器零警告。较早 local-green 62 WARN/14 ERROR，其中两个 storage-version 错误发生于 02:27:22.407/.411 的启动 schema/cluster-version 初始化，早于首次 SIGTERM02:27:25.552；message 为 missing term information，根因未进一步确认，不归为 teardown。最终 postguard 无这两个错误。完整 raw files 与分类均保留。

## 证据与限制

初始 `evidence.json` 35783B SHA256 `0e939358d86aa666ffa04b34820be5001685f8d3c0e5232dbc3e3aa7afc6f14c`；修复 `evidence-fix1.json` 48472B SHA256 `da032c2d2016604f57b360f31c78a6460392989b803347e23ea9182e6aa98976`。报告经时间分类纠正后 SHA256 `a279efaac7c18dd76f10a1f58aba677238d771e7c8ab18d58ff73f1868ef97bf`。初始 RED 只有真实工具 transcript，没有 raw 文件，不能重建成原始日志；修复语义 RED/GREEN 有完整 raw。

Linux CGO0 不是 race；tmpfs 不证明主机重启持久性。永久 intent/slot 仍保守收费，不回收、不退款。此次不证明云 provider、部署/凭证隔离、正常 manager 恢复、安全释放、HA、性能/容量或整条迁移完成；source 重复整归档 SHA 和每 32KiB 前后 native guard 的性能成本仍需后续解决。
