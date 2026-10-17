# Pending cleanup 原子创建切片验证

本切片在已验收 cleanup 执行器之后，为手动销毁和到期任务增加原子 pending 记录。
任务创建已提交但创建者退出或丢失响应时，待办证据仍随 Task、intent 和回执持久存在。
自动分页发现、历史回填、pending 终态移除、共享调度及 mounted 自动模式尚未实现；
默认 main 仍使用 Redis。本报告不代表整个 pending-worker Task 或生产迁移完成。

## 实现

新增私有 `cleanup-pending/<TaskID>`，严格规范 JSON、2048 字节上限，包含 TaskReference
及规范 Task 字节的 SHA256。记录永久、不可变，解码检查 namespace、restore epoch、
键、版本、租约和固定视图修订。它仅提供候选定位，不能授予执行权限。

公共 destroy mutation builder 同事务写 Task 和 pending，比较 pending 不存在。
保留原五份权威记录和 worker/session/partition/share 比较。仅对已解码 due 行使用
精确 Value+正 ModRevision 代替冗余四项比较，为 pending 的比较和写入腾出两个节点。
删除重建、同内容更新及租约附加均因版本变化被拒绝；没有改通用 taskFence 或 Stage。

两入口在原 Grant 前共用 begin、commit、实际 ResolveStage 的完整预检，保持请求
128KiB、响应256KiB、64节点上限。到期 mutation 为43比较+7写入+原预留14=64，
实际 commit 为51比较+8成功操作+4失败读取=63；手动入口分别为25+6+14=45、33+7+4=44。
旧预检的仲裁投影与源码不同，新投影按实际 ResolveStage 的5比较/1写入/3失败读取修正，
没有修改解析协议。最大合法字段与最大整数投影通过，合成超大比较值被较窄预检拒绝。

记录编码和事务预检按职责分私有文件，入口只做实际组合；事务不变量继续由 etcd 包
持有。关键约束、逻辑分段及包边界经作者和 Root 自审，gofmt 与差异检查通过。
完整 Task 的唯一独立审查仍在整个自动消费者完成后进行。

## 实际验证与保留失败

- 行为 RED：正常提交、提交后丢响应、手动 builder 三例明确因缺少 pending 失败，
  CAS 拒绝例通过。随后缺私有 API 的编译 RED 单独保留，不当作行为复现。
- 定向 host：新增四组加原八组到期准入回归，共12组/47子例，通过1.962s。
- 修正后的 race：同12组/47子例，通过2.939s；无数据竞争、跳过或告警。
- 旧 `TestTaskDestroyAtomic/mutation-shape` 单独定向通过1.111s，未选择依赖真实 etcd
  fixture 的其它子例。两个旧测试只调整新增原子写后的形状预期，原权限断言均保留。
- Linux arm64/CGO0 的 etcd 包 vet 通过。

以上是有界 gRPC 模型和纯 builder 验证，尚未执行真实 etcd 或本切片的物理 native。
host/race 分别9031/9036份输入、Linux vet 9122份输入，全部17类 Go-list 字段及源码、
工具前后清单和每份归档均已独立核对；各阶段在后续 Go 或源码修改前审计。
无 Docker 测试资源被创建。

Root 新采集脚本的首轮 race 清单漏传 `-race`，实际测试虽通过，该轮输入绑定无效，
明确不计入验收。原工具、原始日志和归档保留；修正 Go-list 选项并记录实际命令后，
只重跑 race。新清单实际包含 race 专用 Go、汇编和 syso 文件；旧 host 清单选择正确，
已在工具修改前完整审计，不重复执行。不能用结构化清单核对通过掩盖选项不匹配。

| 接受的结果 | 原始日志 SHA256 |
| --- | --- |
| host | `b8913d96bd18fe861edf5d119d8373a7a0f80724cc61fc614cc0ffe30a3c0919` |
| race 重跑 | `770af3138f2999ef116ac72da87c8512b84cf8d6d3df11be78d5aaf9672de8d4` |
| 旧手动形状 | `e7eef17250baac8b2a2396a070c9389faba67568e0f1f60a1432904fb03690b8` |

下一片只实现80条固定 revision 的 pending 分页，复用原 Range/Relist 所有权与真实
关闭等待，再接历史回填、结算和 actor。后续仍需 pending Watch R+1、独立 worker 角色、
大 N 容量与故障、部署恢复及 Redis 移除；这里不承诺吞吐提升或固定任务开始时延。
