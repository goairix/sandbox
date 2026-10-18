# detached 终态查询首片验证

源码 `12af159`（辅助 `f37fc98`）接通 etcd 后端公开方法 `ReadSandboxReleased` 的
detached 分支。原 cohort 的实际 Task KV 传入 B 纯校验器，seal/head 严格分型后
读取9或13点历史；实际 Task Version、key、bytes 必须与重复历史一致。完整私有
local/detached proof 认证完成后，观察适配器借用全部来源，最终读取保留21 pin
（存在后继 owner 时上限22）。不合成 local seal、不调用额外三次 standalone loader。

原 flat shape 先于 protobuf Size 的传输、八次 RPC、256调用准入、原 deadline 和
Close 等待实际 client 返回不变。v1 仍九点历史/18 pin；只对认证 detached 选择
新最终 profile。独立传输预算在原准入前预检，沿用原128KiB请求/256KiB回复及八次
512KiB总回复限制。这里的公开方法是后端方法，HTTP/managed worker/quota尚未接完。

Host 原始 RED 在三个 actual Task 错配（Version/key/value）应拒绝处失败，原v1
正向和其它负例通过。实际 `run-8f5d98d2fc5f4e2dab75f138a150d749` 先通过真实
observer证明、七槽complete、owner释放/失效Claim及B冷历史认证，随后在新增后端
调用因旧v1解码返回corrupt而失败；父EOF是后果。132源码、四ELF稳定、原Wait、
无OOM及确切移除记录保留，不把fixture失败冒充产品RED。

GREEN `run-f9f4cdd55d26447c84b9a6aceadacd72` 父18.65s/worker14.57s通过。
实际八次只读请求点数1/4/1/4/1/2/13/5，最大请求3009、最大回复30117字节，
回复合计51566字节；最终32节点、21 pin，包含全部13来源及head/seal。只在最后
确认旧index缺席一次；当前E/S读取、Grant、额外签名均零，永久源/head完整envelope
不变。原worker/PID1/三个etcd Wait及原物理清理通过，132源码与四ELF前后稳定，
无SKIP/FAIL/OOM且确切容器已移除。state ELF
`b2b939d5fccc8706e9412f54a5399e71e8f586d2c5afd08e8c24558c0001d85f`，parent
`efaef19f461d53ddf76c53e82068840089ead1138ea1503f69cd11d29010ab32`。

Host四项、race十项（含六项原v1签名/pin/后继/Close回归）、Linux vet及两构建
通过，输入分别9159/9164/9269/9269/9474；逐轮完整17项Go输入、源码/工具归档和
前后指纹审计通过。唯一审查员完整六文件、直接依赖、真实日志与冻结指纹后限定
Spec/Quality PASS，注释、分段及包职责均通过。

主分支 fresh Host四项通过；Linux两构建各9269/9474输入及完整审计通过。与辅助
验收输入仅做工作树前缀归一，完整路径集合双向相等且逐项SHA相同，含依赖/工具链/
测试与嵌入输入；环境、tags、GOOS/GOARCH、selector相同，模块要求及go.sum一致。
因此复用上述同输入辅助native证据，主分支未另跑native。等值记录在主仓库
`.superpowers/sdd/2026-10-18-etcd-detached-cleanup/terminal-consumer-native-input-equivalence.json`，
原生证据在D工作树对应目录；没有把子集hash核对当完整输入等值。

后续测试提交 `366869d`（辅助 `f9a4888`）补齐四个 companions 各四种拒绝：
缺失、foreign key、非零 lease，以及自身 codec 合法但 sandbox 绑定错误。
每例均先读取真实原生历史，只替换一份克隆回复；公开方法返回 corrupt/nil，
恰好七次只读 RPC、一次注入，不进入最终读取。16例结束后，全部13来源及
seal/head 的实际字节与 MVCC envelope 不变，Grant、额外签名及当前 E/S 读取均零。

真实 post-auth ABA 在第八次 API 请求提交前发生，此时完整历史认证与 cohort
匹配已经结束。原始 client 用身份/restore 及 activation 的 Create/Mod/Value/
Lease/Version 比较提交一次同值 Put，实际 MOD 从20推进至77。原最终事务真实
选择两点 metadata Else，公开方法返回 conflict/nil；postread 确认仅该点 Version
加一、Mod 推进，值/Create/lease 不变，没有恢复旧 MVCC 或伪造历史。

实际 `run-12b1dd6eb9fc4472929aff05abb580c0` 父19.39s/worker14.62s通过，原正向
仍八次 RPC/21 pin，回复合计51610字节。16拒绝和真实ABA全部通过；136源码与
四ELF前后稳定，原worker/PID1/三个etcd Wait返回，无SKIP/FAIL/OOM且容器确切移除。
日志 SHA256 `26fd96560cace69c26a7a5d699f36dba41b37317712fe44946f1fc9a7614a70e`。
Linux state/control/vet 输入9270/9475/9270，逐轮完整17项输入及源码/工具归档
审计通过。唯一审查员限定 Spec/Quality PASS，注释、逻辑分段及包边界通过。

前两次 native 失败如实保留，定位为测试全局计数被仍运行的 worker 后台事务污染，
不能当作产品放过错误历史的 RED。修正仅用私有 context 标记隔离本次 API 计数，
后台事务原样委托；没有停止 actor、放宽断言或修改产品。该隔离也不构成原生
race-detector 或共享 client 字段安装同步的证明。

主分支 fresh Linux 两构建及完整输入审计通过；与上述辅助输入做完整双向路径集合、
逐项 SHA、环境和模块语义核对后相等，记录在主仓库
`.superpowers/sdd/2026-10-18-etcd-detached-cleanup/terminal-negative-native-input-equivalence.json`。
仅复用同输入原生证据，主分支未另跑 native；Linux-only 测试增补没有重复旧 Host 组。

已接受实际后端终态正向、actual Task绑定、四 companion 拒绝及真实认证后ABA。
v2合法后继物理正向仍开放：现有真实申请生产者无法在同一合法detached workspace
产生后继请求，不能靠删除原请求、手写四伴随或伪造身份补出正例。不关闭完整
terminal合同、quota归还、自动detached消费者、统一phase准入、HTTP/部署或大N容量验收。
