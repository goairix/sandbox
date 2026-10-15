# Kubernetes enrolled bridge 验证记录

当前组件已范围验收：唯一完整审查提出的Write/CloseWrite deadline错误语义I1已修复，同一reviewer限定复审Approved，无剩余阻断项。main仍Redis，不能称生产切换或整体迁移完成。

## 交付与小提交

固定候选namespace、Pod/Node完整tuple点查，静态凭证/TLS快照，唯一GET WebSocket/v5 exec固定`/usr/local/bin/sandbox-launcher bridge`，返回实际owned net.Conn供原Destination/bootstrap消费。没有任意命令/endpoint/callback/plugin/fileloader/proxy/redirect/retry/压缩或协议fallback。constructor不联网、零值/复制client拒绝，MaxActive1..16即时背压、建立总期限≤5秒。

metadata≤256KiB、解析前HTTPheaders≤64KiB、每WSmessage≤64KiB、stdout队列16messages、stdin分块32KiB、stderr≤4KiB/status≤16KiB。真实v5 `[255,0]`只半关闭stdin，继续读stdout；只有正常1000关闭、合法/空成功status及空stderr才EOF，裸TCP/异常close/malformed/重复/别名/非零status拒绝。候选和传输成功不成为native authority或健康证明。

每个当前活跃连接只有一个reader和一个deadline waiter，Close先关闭原TCP，再协议关闭并等待reader/deadline/已进入IO，清空队列后退slot。建立取消回调stop+join，成功Dial后不保留短建立ctx。GET同步ownedTCP/TLS，不启动隐式SDKdial；cleanup仅复用纯静态配置快照，原HTTPtransport/dialtracker/预算不变。

源码BASE`c89c58d6bf571781b89a7e26f08f8b56fceb81ab`，共4个连贯源码提交/10个唯一文件：

- `103eef47d4049ee0212dfaba74ff163fb67f1eb8`：静态快照、目标核对、两次有限GET及专用输入collector。
- `68373d43749dd77d03d426e3ba1b03d8fa403d37`：真实HTTP/TLS WebSocket、字节连接、FIN和ctx移交。
- `950481b37626ae648d1a97cd540415539102b7cf`：故障/上限/join、deadline线性化、严格status/origin与Linuxvet支持。
- `711d4e9080f1566cfe185878be77f03fcd1c7d8e`：Write/CloseWrite传播首个终止原因，新增真实阻塞写入和排队FIN超时回归。

初选moby/SPDY在只读预检因不可join后台goroutine、未cap解压分配和FIN/错误EOF混同被否决，未编写或运行SPDY产品实现。最终依赖为仓库原有gorilla版本，无升级。

## 原验证与独立核对

14个新top selectors分阶段host/race通过，受影响Darwin/Linuxvet通过。真实loopback HTTP和fixture-CA验证的TLS两条路径验证84KiB二进制往返、固定三次请求/完整query/v5、32KiBstdin、半关闭后响应、成功建立后立即取消原ctx仍可用和原server完成。其他有限故障包括Pod/Node kind/UID/boot/deletion/OS、所有target字段、config/callback/plugin/file拒绝、TLS握手及每GET/upgrade的取消/超时/clientClose、metadata/header/message/status/stderr超界、非法协议/压缩/重定向、异常close/裸EOF、16slot背压、实际socket阻塞Write/满stdout队列和关闭join。

原预期RED保留：缺DTO/constructor、pendingupgrade、duplicate/casealiasstatus、空?/# origin；另有原collector旧Bash empty-array失败（Go前）和一次NodeUID测试错误地选了原值的失败。修复后限定验证通过，不隐藏历史失败。slice1 green2→finalhost有实际joinedAfterFunc sync.Once修复；fixtureclose信号补强后仅重跑受影响三个selectors，不重放旧矩阵。

Root独立actual17输入审计：第一片host/vet7524、race7535；第二片host/vet7526、race7537，前后filename/SHA及当时当前bytes一致，原RED仅历史绑定。第三片每epoch 7527host/vet、7538race前后绑定；最终Linuxvet7566inputs绑定最后格式化源码。Root独立审计原ef276e/87449/6f4da0、50e7ba/99925/d2b4e2、ebcf36/13108/0beb32均退出0；原最终日志PASS/无FAIL、SKIP、racewarning，vet为空。

R1之前的最后Darwin验证后只有测试字符串连接处一行空格被gofmt调整。Root0ee53b/12486/645550将当时当前该行逆向重构为旧字节、独立匹配最后host/race/vet捕获SHA，并核对其余7527/7538输入全部当时当前字节相同；未为纯格式重放测试，未将旧hash改写为当前同源。原9条finalmanifest SHA与R1前冻结作者report SHA`f49bf36fe9ecffaade3d26af3756af6757d2eb596d34a5ef9e825125f68345d0`精确匹配；该报告后续追加R1记录，原快照保持历史属性。不同阶段不是最终同源全套验证。

## 审查与边界

唯一fresh完整Taskreview读取65491字节/1833行完整产品diff，SHA`e38e51a91f8d0719a029fcf89eb07155bb77b0e3126bf0b5fa525206616585b3`，初次结论C0/I1/M0、Needsfix。I1：已阻塞Write/CloseWrite在deadline waiter记录os.ErrDeadlineExceeded并关闭TCP后，直接返回底层socketclosed错误；错误丢失Timeout分类，旧“write deadline”测试实际调用Read。

R1修复让abort返回锁内保存的首因，两个写路径在原物理中止后返回该因，保留已完成分块的n、锁序及slot退款时机。新增`TestEnrolledBridgeEnteredWriteDeadline`用真实非读取peer、tiny TCP buffer及实际调用栈确认FD.Write阻塞和CloseWrite排队；SetWriteDeadline/SetDeadline两子例分别验证errors.Is和net.Error.Timeout、部分写入、实际IO/reader/deadline/server退出与Close后退款。第一次fixture错误假定首帧已完成导致n=0失败，保留原日志；纠正peer先读取一帧后，RED2真实复现两操作丢失超时分类。

限定host/race两子例GREEN，受影响Darwin/Linuxvet原退出0且日志为空。Root c803fb独立核验actual17输入：host/vet7528、race7539、Linuxvet7567前后文件名及SHA不变且匹配最终源码；46ff58独立匹配exact2/full10 manifests，格式检查为空。未重放原14selector矩阵、未运行native。同一reviewer读取6320字节R1 diff，SHA`9e7a64145d7a2cba3bbf3c4126393efa178313f0f24eec474ac6f3916fc52015`，仅复审I1及直接新增破坏，结论Approved，无剩余C/I/M。

Root此前还指出测试helper的结果信号早于deferClose、16连接用例未等待全部server：已将原ws.Close置于完成信号前，消费16个原结果，随后受影响host/最终newfault race通过。httptest.Server.Close不能代替hijacked连接join，此处没有以缓冲channel冒充退出。

Git均Root-only exactpaths/expectedHEAD/common→OPEN OWN fcntl锁，小步branch/HEAD/indexempty/exactstage/diffcheck/commit/postempty验证。无native、真实APIserver/kubelet/CRI/clustercredential/Redis/secret/部署/依赖升级或Sentinel改动。

服务端须支持exec WebSocket v5/stream-close translation；Kubernetes1.29需相应featuregate，1.31起默认启用，依据[Kubernetes官方说明](https://kubernetes.io/blog/2024/08/20/websockets-transition/)，真实部署协商仍待验。下一startup Task会将正式bridge exec接入真实PID1/observer/TLS，并修复实际EmptyDir初始权限。未实现Create/claim/quota/Pool/API/main、长期证书/observer槽GC、分区/FUSE、大N容量、部署恢复或Redis移除；本组件无按已有sandbox N增长的常驻map/timer/扫描，仍不能据此宣称线上性能已提升。
