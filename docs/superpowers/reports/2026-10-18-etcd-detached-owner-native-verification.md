# detached owner 释放与幸存历史组件验证

源码 `1049226`（辅助 `0dddd91`）。只接受首次真实63节点释放提交后丢 ACK、原
Claim 失效后的只读仲裁，以及删除 index 后的独立历史认证。普通/损坏 ACK、发送前
absence Unknown、新 Claim/Run、cutoff/Close 等矩阵仍开放；完整4B、消费者、quota/
终态和统一 phase 内存准入未完成。功能约70%、整体约45%、生产切换0%。

私有 writer 验原 registry/job/Kernel/current Claim/Run，捕获原 cutoff 并贯穿所有
读写。原 purpose owner 与 Claim latch 下仅一次保存最多8KiB的 canonical candidate，
随后59比较+三个单键 Delete+永久 seal Put，共63节点。Unknown 保留原候选；finish
等原调用返回后退休 ctx/cancel，再公布 done。同 Claim 后续只能独立只读槽查询，
完整 native 历史及原候选 bytes 相同才返回 Committed；旧 Current 不变成新权限。

新私有互斥 local/detached proof 保持公开 v1 严格。v2 读取13个真实存活来源：Task、
Q/Stage receipt/reservation、none、issuer、settlement、实际 Task 推导的 snapshot、
creation intent、publication/proof/enrollment/activation；不读取或重造已删 index、
当前 E/S、publication receipt。认证完整 native envelope/stamp、原 fact 封存的 E、
来源/历史签名、真实 observer proof、七槽 complete 和 digest。原 live fact 仍另验
当前 E/S，原 classifier 仍额外验 publication Reference；纯提取未放松旧门禁。

paid pure decoder 复用已读 actual Task；standalone 将 Task 并入 head RPC，保持三次
读取。历史重复 Task 的完整 envelope/bytes 必须相同，返回 proof 拥有全部 points，
保留后续16点 pin图。13点 value caps共208896字节；维度实测34节点，请求20796、
满cap回复220118字节，符合128/256KiB。这不是全 phase owned 图或64MiB容量证明。

Host `detachedownerhistoryred01` 三正向在原 stub 失败、unsigned graph负例通过；
实际 `run-35bd1d98bcf3494c89bf9901b3263d08` 在真实proof/七槽及13原生点通过后，
于新释放 stub 断言失败。原始失败、122源码/四ELF稳定、原进程 Wait 和精确清理保留。

辅助实际 `run-704c6b6a96544fffa1728fb42596c9c0` PASS：父18.60s/worker14.57s。
真实 CAS 请求18871、回复67字节；测试先核实际 Op 全部选项等于三个精确单键 Delete
及 lease0/no-Prev Put，再转发 actual Commit 成功后丢 ACK。三键确实缺失，永久 seal
存在。原 owner 已 done/ctx退休后释放 Claim，两次只读仲裁、standalone 与 paid cold13
均通过。Then/Else（含嵌套）禁止任何写；index/当前E/S读取、Grant、额外 Sign均零；
13个永久点及 termination/provider 全 envelope 未变。一个 foreign companion 负例拒绝。
这场不覆盖 pre-send absence、新 Claim 或普通 ACK 路径。

原 PID1 Wait=SIGKILL、worker Wait无错误、用户probe与三个etcd原Wait、Serve join及
cgroup empty/rmdir/unmount均通过；无SKIP/FAIL/OOM，自有容器按确切ID移除。122源码
与四ELF前后稳定。state ELF `406fb37a4f9a920fac78446935b057afd3e62b564967bf42dc31beb0d18ee236`，
parent `ead2f3dd3ace0656352292a8b3ccd331692937972b60704311706bfef8b1bf5d`。

Host九项 `detachedownerhistorygreen01`（9155输入）、race十四项含原classifier五项
`detachedownerhistoryrace01`（9160）、Linux state/control build（9264/9469）及 vet
`detachedownerhistoryvet01`（9264）通过。每轮完整17项 Go 输入、工具/源码归档与
前后指纹审计通过。唯一审查员完整九文件/直接依赖、实际记录及冻结指纹后，限定
Spec/Quality PASS；注释、逻辑分段和包职责均核对，未增加导出权限或反向依赖。
证据在辅助 B/D 工作树相应 `.superpowers/sdd/2026-10-18-etcd-detached-*` 目录。

主分支 `1049226` 集成验证 `run-fd16e85c42a64ec09cef3f126dd55aa1` 同样通过：
父19.13s、worker14.63s；实际63节点 CAS 请求18878、回复67字节，提交后丢 ACK、
失效 Claim 两次只读仲裁、standalone/paid cold13 均通过。原 worker/PID1/三个etcd
的 Wait 及原清理断言通过；无SKIP/FAIL/OOM，确切自有容器已移除。124源码与四ELF
前后指纹稳定。主分支 state/control 构建分别9266/9471输入，完整证据审计通过。
state ELF `ab490b9eaf811b72a828ccf7acb67267f30c5b72432267551bfae0d709d044e8`，
parent `166b12f87e908267b4d744a1a42a40679b020205a06988c81f275edb14251616`。
此集成保留同一组件的验收边界，不补全开篇列出的恢复和消费者矩阵。
