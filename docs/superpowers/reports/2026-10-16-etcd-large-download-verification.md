# 显式 etcd 大文件下载验证

分支 `codex/etcd-state-management`，基线 `c8f9a23ca59ff8ea781fb5b709ff968914c9fb31`。本单元已范围验收：唯一完整审查的 I1 经同作者修复、同审查者限定复核后 Approved，无未解决C/I或本范围 Cannot Verify；非阻断M1日志事项保留。

## 实现与提交

| 提交 | 范围 |
| --- | --- |
| `640b197` | 绑定设计和实施计划 |
| `02e81fc` | 有界私有磁盘暂存与普通二进制下载读取器 |
| `72c2980` | 固定 launcher file-download 模式与安静的应用错误退出 |
| `1a45c16` | 可选 mounted 下载预算、严格校验与 owned copies |
| `9011501` | 单原 Exec 的 HTTP 消费、成功屏障、实际写期限与关闭错误传播 |
| `b6d5176` | 独立有限 native 场景、原冷建 fixture 窄接入、operator 说明 |
| `35d3006` | 审查 I1：封存后提前 EOF 必须 latch 故障、保守计费 |

仅配置 `http.file_transfer` 时扩展原 GET download；省略对象保留512KiB路径。默认单文件2GiB、总磁盘4GiB、并发2、总期限300秒，硬界限分别2GiB、16GiB、8以及HTTP最大执行期限。配置加载保持只读；实际 Gateway.New 才打开预先存在的API EUID/0700目录、验证FD和独占flock，并对最多8个精确旧文件先全量验证再逐项清理，未知文件不删。

预收整个文件额度后才创建暂存和调用原Exec，文件bytes不入etcd，内存buffer32KiB，无按已有sandbox数量增长的新map/后台任务。固定普通UID/GID的file-download仅接收16KiB规范请求，沿原Root路径/链接约束读取真实regular FD，检查初始大小、精确原长度、EOF和最终大小。错误只用严格六种Failure code；原verified terminal/drain/exit0和Seal完成后才发HTTP200。响应有实际Content-Length与不超过10秒/总期限的socket写期限；响应头前失败明确503，之后失败以正文截断被客户端识别。

原Wait/IO和HTTP消费者实际退出后才Job.Close，Store.Close取消后等原owner；IO/身份/超限/清理以及封存后提前EOF会取消并latch不可用，保守保留额度，ready503而health仍表存活。关闭错误通过Gateway.Close、Serve、CLI向上传递。没有扩大原stdin、签名、权限、transport、重试或原始执行接口。

## 实际验证与范围

Root唯一执行Go/Git/Docker/gofmt；编辑使用apply_patch。固定Go1.25.6，host/race为Darwin arm64，Linux vet/原生构建为Linux arm64 CGO0。每个epoch在实际动作前后核对GoList17类输入、完整源/tool归档及每个成员、当前源码、原退出码和日志；native在Docker前后再核对源码、二进制和脚本。

| 检查 | 原始范围与结果 |
| --- | --- |
| spool原初修正后host/race | 各11顶层PASS，无SKIP；真实child Kill+Cmd.Wait、flock释放、stale恢复、实际FD/身份故障和owner等待 |
| helper host/race | 各6PASS，无SKIP；大于1MiB的二进制、空文件、类型/路径、增长缩短和writer错误 |
| launcher host/race | 各5PASS，无SKIP；真实子进程的binary/empty/strict failure与旧mode错误行为 |
| settings host/race | 6/7PASS，无SKIP；原OpenHTTP前置检查、旧manifest、严格预算、只读加载与clone |
| gateway集成修正后host/race | 各33PASS，无SKIP/警告，含原路由与探针；单Exec、真实socket、终态前不200、取消/Close实际join、慢客户端截断 |
| cmd serve host | 3PASS，无SKIP；各受影响包Linux vet退出0，日志空 |
| R1 spool host/race | 各12PASS，无SKIP；真实封存FD截断为0/3字节，UnexpectedEOF、取消、latch、cached Close错误、保守保留额度和拒绝新申请；Linux vet退出0/日志空 |
| R1 gateway下载消费者host/race | 各10PASS，无SKIP；只重跑受影响下载组，未将其描述为重新跑全33项 |
| native large-01 / large-r1 | 正式launcher+原service，唯一新selector，分别2.27秒/1.89秒PASS，无SKIP；后者为修复后新源码重新构建和执行 |

R1 spool host/race实际输入2057/2064，日志各3371bytes，SHA256 `d30517c9af475518bb1ddfc47d8ecb51485ed28c41c74d6ff2b0df90af041a1d` / `a859c2a7a70e0f320bfb01668d6265b8cf8157071b4c74910bfb81c731214119`；R1 gateway实际输入8960/8965，日志各3525bytes，SHA256 `86b316a72af9ec030e4c5a2bf711369bbe6951dc8ef20efe4acedef9c7af880c` / `0ed46b4cb0ba7260bfeeefbd0289217c66635ec6b29cec5b41eb829c3b05701e`。原完整gateway33项日志各11383bytes，SHA256 `734be8ba54807a5107b90da2ee628cc14d74eee0917566c91e399eab86871001` / `65a640db877e6d817a683c156f7d971b699b71d3c03d4ceced7601396b1505d0`。

两轮native都实际下载1048613字节，内容SHA256 `1e0e598dd3245df59ea21cf7422ec2198dfa1d6101e695a3e39b33e440df9ec5`，验证empty200、oversize413、FIFO422、逃逸链接拒绝并signed io_error500，UID/GID1000和控制材料不可读、精确descriptor/receipt/drain、每下载恰好一个原Exec及暂存清理。每轮恰好9个原bridge（Create3+seed1+download5），原上限16保持；9个Cmd.Wait0/IO全部join，正式PID1 Wait0，etcd实际SIGTERM。并未重放旧32秒Exec或全部文件矩阵；日志中的TestEnrolledProfileNativeExec是正式bootstrap子进程。

修复后native runner/launcher实际输入9194/2038，ELF64 AArch64、Go1.25.6/linux/arm64/CGO0/nonrace，构建日志空；二进制SHA256 `a00c44ea22827ae3d52bc46b36b5a8d5e2de887b74ef0a3ece5da1655573e72f` / `b441cc0482cdd2efd93c54191e51dc67fc0893522873bab73992d38c3ebae4e8`。原native Linux vet输入9194/退出0；修复后由spool Linux vet及新的native实际构建/执行覆盖，不冒称重跑旧native vet。外部etcd3.6.14 fixture SHA256 `9076ea9d5662fdade520a63aa7e312fcde5a4fb7d50a5515d37896204f1c0f15`，仅从许可外部缓存复制到本OWN，没有复用已关闭OWN。

Root核对两轮完整created/final容器证据：固定image、network none/private PID+cgroup/readonly、512MiB/2CPU/256PIDs、七项显式cap/NNP、三个RO挂载，原/workspace1MiB与/tmp256MiB，两个显式env加五个固定image默认env。attach/container均0、OOMfalse，精确容器已删除，独立run label清单为空。原容器`c19ae78f87ee9f088bc4fcbafea00e2ba2b5290e80e198b6130fc7067aca0df9`，R1容器`5e86ab65e53da95222b1bc6076b150ede7ced7ef4a28db04ef69d9db739c384d`。native原始日志均6200bytes，SHA256 `e7327553e0c3a813deb68f2273206d7b2d622c656db7ae30a19e3afd20eb91c3` / `4b61b1b8dfd78b71109083eb7ab59d699c1f3154d7254e0f73d781d00e98ca99`。

## 保留失败和审查

首轮spool/helper、launcher、settings、gateway tests-first均实际因冻结接口缺失而RED，Root审计后才授权产品实现。spool-race-01还发现测试在Cmd.Wait前读stderr Buffer的实际竞态，修正为原copier退出后读取；CLI首轮nil/empty bytes断言修正为精确bytes.Equal，均保留原失败日志，后续通过不抹掉早期失败。

Root自审发现封存后、响应头前的取消/写期限失败会产生隐式空200；focused RED两个分支实际200而非503，日志1205bytes SHA256 `27efa683be4c5285e56dd291f2768a5fd97f39a2eb6753359afc1684ab780d83`。最小明确503修复已被完整gateway33项及后续下载组覆盖。

唯一fresh审查初轮C0/I1/M1、Needs fixes。I1为封存后提前EOF未latch；同作者真实文件截短RED日志1246bytes SHA256 `a6fa0dd01c932c7b8f79747fd64e813abdde5b574f53edd0fcf3a99d75c994cf`，两个分支实际得到EOF，修复35d3006后上述host/race/vet/native通过。同一审查者仅复核该修复diff及相关证据，确认I1 ADDRESSED、无新增C/I、本范围无未解决Cannot Verify，最终Approved；没有重开完整审查或另派reviewer。

M1为已有fixture的etcd共端口/simple-token警告与关闭时Server closed/closed-network日志，保留到整分支收尾排查。两轮native通过不等于日志pristine。没有未说明的测试跳过或将失败分组改称全通过。

## 尚未完成

这是显式etcd模式的有界下载范围；未实测2GiB吞吐，不是生产kubelet、大N容量、分区/FUSE、上传/分片或完整迁移验收。默认入口仍Redis，生产切换0%。功能代码约70%/整体交付45%的既有粗估暂不因源码、提交或测试数量上调。后续继续生产任务接线、分区due/dirty/Watch、FUSE/collector、其余API/Pool/Docker/Upload、容量/部署恢复与Redis移除。
