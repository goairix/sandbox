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
