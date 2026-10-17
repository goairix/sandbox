# Detached plain 来源与终止协议验证

`361003b` 完成显式 pinned plain image 配置与 signed detached 来源分类；
`9697527` 完成严格 v2 detached termination wire。各唯一独立审查均 Spec/Quality
PASS、C/I/M=0/0/0。这些组件尚未构成实际 detached 清理消费者，HTTP保持501，
旧 native key reader明确拒绝v2，默认入口仍Redis。

## 来源与配置

image在首个RPC前限制1..4096字节，通过原kubecontract release image helper计算
plain contract，与enrolled配置精确匹配并独立复制。来源分类重建原StorageID/
request hash identity，精确绑定Task/snapshot/intent/publication和所有运行时归属，
重新执行signed historical enrolled/plain认证，拒绝local/FUSE/错绑定。
返回普通identity，不证明Task birth、Claim/Run或absence-of-effect。opaque config
digest包含原TTL，不当作snapshot digest；合法当前TTL可不同于旧certificate expiry。

Go1.25.6，Darwin arm64 CGO1 host/race；Linux arm64 CGO0 vet：

| Epoch | 范围与结果 | 输入数 | 原始日志 SHA-256 |
| --- | --- | ---: | --- |
| red01 | 批准的新符号缺失，预期exit1 | 9128 | `a36b31c7b8310a1e305c49be3812da9a76fff80fff7f310e465e26aa3294f5a2` |
| host01 | `^TestDetachedCleanup.*$`，8顶层/59含子项 PASS1.440s | 9130 | `7a98ce68ad45d2e18a5aa9b9996646acb178505cb98917934f1e21e28744930d` |
| race01 | 新来源及原identity/due fixture，17顶层/79含子项 PASS3.483s | 9135 | `84e4a60f34c6d6088321045bd34f756800a67b5c94c1e1fe137e708b0ef398b4` |
| linuxvet01 | etcd包 PASS，空日志 | 9225 | `e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855` |

正向fixture通过真实acquire/dispatch/bind/mount/enrolled publish/destroy/read API，
错签名、真实FUSE proof、不同plain contract、caller mutation与原fixture回归有覆盖。
独立审查员 `detached_source_review`；辅助提交 `1509f06`。

## 终止协议

可选DetachedEffects放在intent末尾，保留原common authority可比较性。v1 canonical
字节保持；v2要求固定profile/version/contract/settlement，Archive/LocalComplete
精确零值、MountAttempt=0，保留其余13个stamp与quiescence stage。原ticket v1
schema/domain不变，以完整IntentDigest绑定新tuple。KeysFor仍仅提供local v1键集，
拒绝v2，六个生产调用点均处理error；此提交不授予observer新native执行权限。

| Epoch | 范围与结果 | 输入数 | 原始日志 SHA-256 |
| --- | --- | ---: | --- |
| red01 | 新field/version行为不存在，预期exit1 | 2088 | `69d1781163f33127b448995e9d0a8615e1f4a2feab2149b19ed4913880214603` |
| host01 | 完整controlprotocol包，95顶层/2951含子项 PASS8.435s | 2088 | `9047f3f98af4758351e9a9383fc3dcdcf3c5330d11d29db5cb578070d5c37c55` |
| race01 | detached/TaskRuntimeTermination/NodeTermination，7顶层/99含子项 PASS1.760s | 2095 | `9eaacc38501a2afa960e9bdf084e8592b10fd67999ef464dc3f5495cbe286008` |
| linuxvet01 | controlprotocol包 PASS，空日志 | 2111 | `e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855` |

独立审查员 `detached_termination_wire_review`；辅助提交 `d1bb42c`。

## 持久记录与 native 边界的后续切片

`784f760` 为新 Pending purpose 保留完整原 Claim/lease/attempt/digest，禁止假Stage，
host回归通过。`7e1656b` 新增唯一纯settlement codec、binding hash及实际protobuf
投影；独立Task3 reviewer对该切片Spec/Quality PASS。真正RED先于产品，Root另
发现短draft低估foreign记录回复，补反例得到真实RED后仅修复失败回复按32KiB计量。

| 范围 | host | race | Linux vet |
| --- | --- | --- | --- |
| 新settlement协议 | 6顶层/103含子项 PASS1.634s | 同范围 PASS2.110s | PASS |
| Pending和事务投影 | 5顶层/15含子项 PASS1.252s | 同范围 PASS1.926s | PASS |

合法最大宽度canonical为14,491字节，E4,096字节，上限32KiB。实际protobuf写
64节点/request102,990/reply最大35,641字节；来源读取22节点/request12,720/
reply133,594；仲裁10节点/request6,354/reply35,641。非法65节点预读和超128KiB
请求拒绝。这些是组件投影测量，尚非真实writer RPC或容量验证。

`898cac7` 增显式v2固定键入口，保留原v1入口拒绝v2；local matcher拒绝foreign
variant；observer请求和实际send限制128KiB，回复/recv仍256KiB。三个真实RED
分别指向缺新键API、matcher误收、144,976字节请求被旧边界放行。唯一Task4A
reviewer对该边界切片Spec/Quality PASS，实际v2 reader/authority尚未实现。

| 范围 | host | race | Linux vet |
| --- | --- | --- | --- |
| 完整协议/相关终止协议 | 96顶层/2952含子项 PASS8.609s | 8顶层/100含子项 PASS2.490s | PASS |
| 原local与variant | 19顶层/46含子项 PASS0.844s | 同范围 PASS2.140s | PASS |
| 完整observer | 12顶层/30含子项 PASS3.679s | 同范围 PASS4.698s | PASS |

以上零skip，每轮完整compact输入/工具/源码审计通过。共享记录与新键/边界合并后，
D的定向protocol/metadata组合host通过，输入2091/9138，原始SHA分别为
`fcd5df46eb98b3bc3da3d2e632d6aa9fc697a143c34139706587b35fedd5c6ce` /
`e1c1ebc858be558c50f7c22e7bd44573a6e7993fe1260d829f9a1276a2e2ba71`，
完整审计通过。精确各轮命令/指纹/原始日志及两份独立审查在B的
`2026-10-18-etcd-detached-noeffect`与D的`2026-10-18-etcd-detached-runtime-native`。
此前失败日志原样保留。Task3 writer/Unknown/Run恢复和Task4A实际consumer均仍OPEN。

## 证据和界限

真正RED先于产品实现；每个epoch的17类输入、前后源码和工具指纹、compact manifest
完整审计通过后才继续Go或源码变更。精确命令/日志、输入指纹、完整review.diff与
reviewer.md保存在复用D/B工作树的ignored OWN `2026-10-18-etcd-detached-source`
和`2026-10-18-etcd-detached-termination-wire`。主分支集成前后原有两个无关staged
路径的index entries完全保持。全部复用主仓库构建/模块缓存，没有新原生二进制或
重复工具链归档，磁盘余量约96GiB。

GoDoc及私有注释明确配置、TTL和权限边界；逻辑按阶段分隔，配置/来源/wire按职责
分文件，依赖单向，无provisioning导入或新goroutine/lease。仍需持久no-effect writer
及Unknown/join/cold recovery、实际termination/owner/terminal/quota消费者、部署/
HTTP和真实Linux端到端组合。这些通过不代表大N、32物理并发、完整故障恢复或生产
切换；功能约70%/整体约45%的粗估不按提交数上调。
