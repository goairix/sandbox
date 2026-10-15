# Enrolled runtime 原子发布验证

状态：enrolled runtime 原子发布单元已范围验收。首次完整审查 C0/I2/M1；同作者完成两次修复提交，同 reviewer 限定 R1 确认全部发现已解决、无新增 C/I/M。原生 fixture 与生产集成限制仍保留。

## 实现与边界

BASE `87f822f8e9a8c64c49721090830dad53e827e852`。六个连贯小提交：`2beadb8`、`beb7e26`、`c3aa645`、`b43d9c2`、`c4a9743`、`1bd2f30`。唯一 fresh reviewer 审查包只包含这六个提交的30个路径，排除交错的 observer 和 Root 文档提交。

新 immutable profile 要求真实 Supervisor 先以自己的运行时私钥完成 node observer 登记，收到认证 ACK 后安装 journal 并开放准入；v2 ready 使用新签名域。Backend 将原始 CreationClaim、全部九项准备历史、两条登记记录与 proof 在同一原子事务中发布，原始未知 attempt 冻结，只读冷仲裁不重发写请求。旧协议 validator 与 shared Stage driver 保持原合同。

本单元只支持 plain/no mount。没有接主程序、生产 launcher/manifests、FUSE、termination composer、provider 清理、owner 释放或 Redis 切换。observer 依赖已范围验收，见 `2026-10-15-node-observer-physical-termination-verification.md`。

## 实际验证

- epoch1：真实 namespace PID1 Supervisor、真实 observer UDS/认证 ACK、v2 ready、公开 Backend 原子发布/冷读和受控 UID1000 用户执行通过；另有晚 ACK/Close 原始 owner join。整轮因 namespace fixture 误构造513而非512字节失败，保留原 FAIL 和外层退出1。
- epoch2：只补跑受影响后端。18个独立叶、两个父测试共20 PASS 事件，零 FAIL/SKIP。覆盖真实512 namespace、commit/guard 丢 ACK 后同 reference 只读仲裁、九个 history pin、真实 claim loss，以及 partial/leased/mutated/header/restore 冷读拒绝。原始 backend ELF/etcd Wait、外层退出0和精确清理结束。
- epoch3：两个 journal 安装交错中，CloseAdmission 通过；Close 的额外 disk-CLOSED 期待违反既有 poison 诊断语义，整轮保留 FAIL。未为满足测试改写旧 Journal。
- epoch4：只补两个 journal 模式；两个实际 PID1 child 加父测试共5 PASS 事件，零 FAIL/SKIP。Close 实际等待原 clock borrower，等待期间保留 key，之后清理；不安装可用 activation、不开放准入、不产出 ready/exec。取消后可保留 open 诊断 bytes，实际 cold OpenClosedJournal 重写 CLOSED，非 fresh handle 拒绝 InstallActivation。原始三条 child/etcd Wait、外层退出0和精确清理结束。

Root 核对四个 epoch 各自 runner/etcd test ELF 与现有 launcher monitor 的编译所选文件名/前后 SHA、binary/source archive SHA 和原始 Wait。epoch4 有25个本 Task 路径匹配最终该轮源码；两个 protocol 测试不在这些 native ELF，另由 host 测试覆盖。构建过的 ELF 不自动记为在每个 selector 下已执行。

新增有限 host race 为15顶层、24 PASS 事件，零 FAIL/SKIP；另有前序直接受影响的 management TLS、publication/activation 和 transport compatibility selector。host/Linux affected vet、格式、脚本语法和 scoped diff 检查通过。这些不是全仓兼容性、大 N、长期可靠性或容量验收。

## 审查与 R1

唯一完整 Task review 发现两项 Important：最终两条登记记录的8192字节检查晚于首次 preparation RPC；缺少新 Supervisor 消费者对认证 peer、nonce 和 node boot 的拒绝测试。前者须用公开入口证明超界输入零 native RPC，后者须走真实 UDS/实际 Supervisor，证明不安装、不开放、不签 ready，并实际等待原 owner；不能拿旧 producer 的物理负例替代。

前置大小检查修复已提交 `890606668170f57ddc7b17f6afd21df33ed4fcb5`。公开入口使用真正 root-signed、旧 enrollment frame 可接受的新 companion=8685字节输入，原代码实际发出一次 native RPC，修复后 unary/stream RPC 均为0且未冻结 attempt。五个受影响 host race 顶层测试及 etcd vet/格式/diff 检查通过。后续原生认证保留；大小检查只拒绝输入，不把未经认证字段当成权限。

消费者负例提交 `491238d0135e22959f00130e0a4404823dc05c47`：三个真实 PID1 NewEnrolledSupervisor 通过真实 UDS 分别拒绝不受信 root 的 observer peer、非法 nonce、root 合法但与配置不符的另一 node boot。三个 mode、三个 child 加父测试共7 PASS，零 FAIL/SKIP；无安装、准入、ready、exec，原 owner/socket/peer IO 实际 join，Close 后 key 清理。首次 nonce 错误字符串断言失败保留为4 PASS/2 FAIL，原始外层退出1；修正后的原始退出0和全部清理结束。

nonce 的拒绝边界是非法长度/非 canonical server challenge，而非任意另一个合法新32字节 nonce；客户端无预知 server nonce。现有 RequestDigest 跨请求绑定不变。认证 peer 测试证明 observer 身份消费，不能称 SO_PEERCRED 物理观测。Root 对两次 R1 native epoch 各三类编译 artifact、文件名/前后 SHA、ELF/archive SHA、三个选中 R1 Go 路径及每轮四条原始 child Wait 独立核对。etcd ELF/monitor 在该 selector 下仅构建，不能增加执行覆盖计数。

同 reviewer 只检查这两个修复提交的四路径包，确认 I1/I2/M1 全部 ADDRESSED、无新增 C/I/M。没有增加完整 review seat 或重放旧矩阵；后续可以消费该单元，仍需完成真实生产接线和其他 profile。

## 固定预算与日志

commit 保留54比较+9写+1失败读取=64节点；只读仲裁12节点。namespace≤512、key≤1024，每个 native request/success/failure response 独立≤262144字节。两条 enrollment 各≤8192、combined≤16384；proof≤8192，严格 v2/Lease0/immutable same-commit。最大 namespace 的实际提交请求59149字节，保守 success488/failure1606字节；这是一项具体 profile，不是完整最大容量结果。

Supervisor 只有原始 enrollment owner；Backend active owner1..32，Close seal 后实际 join，再释放 client/key。无按已有 sandbox 数增长的新增常驻 map/actor 或发现扫描。这些结构界限还不能证明生产性能改善。

原始日志保留 HTTP/gRPC 同端口及 simple-token 配置告警、真实 claim 撤销后 cleanup 的 LeaseNotFound、刻意 observer 写权限拒绝和显式 SIGTERM teardown。只按具名内容分类，不把未知 error 统称正常，PASS 不称 pristine。

主程序仍使用 Redis。下一完整单元继续本地同步完成→持久终止→精确 provider 清理→64节点原子 owner 释放；生产接线、分区调度、FUSE/collector、大 N 验证、部署恢复和 Redis 移除仍未完成。
