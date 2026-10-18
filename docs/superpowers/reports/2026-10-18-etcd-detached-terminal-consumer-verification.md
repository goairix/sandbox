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

仅接受实际后端终态正向接线与actual Task绑定。四个新增companions的拒绝矩阵、
认证后真实same-value ABA、v2合法后继物理正向仍开放；不关闭完整terminal合同、
quota归还、自动detached消费者、统一phase准入、HTTP/部署或大N性能容量验收。
