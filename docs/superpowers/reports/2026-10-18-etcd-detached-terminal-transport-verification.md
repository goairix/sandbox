# detached 终态读取传输组件验证

源码 `9328ab4`（辅助 `a2e5284`），仅接入三种私有传输维度：13点历史、22 pin最终
读取及22 pin后继最终读取。原v1枚举/caps、18 pin、256调用准入、八次RPC及
`8*RequestTimeout+1s` 和 Close 实际等待均不变。旧消费者尚未选用新profile，
不计为终态消费者接通、历史认证、quota归还或完整迁移完成。

新profile在dispatch前检查具体及最大请求不超过128KiB，并独立检查单回复256KiB、
八次读取合计512KiB。最终/后继最终为同一第八次RPC的替代项，不能相加或多读一次。
仍沿用原flat shape先于protobuf Size的解析，未放宽通用四点/四pin preflight。
实际protobuf最大模型：历史37节点，请求39634/回复230073字节；最终33节点，
35538/29561字节；后继最终35节点，37604/40005字节。八次回复总396566字节。
这是有限传输模型，未证明全phase内存、并发容量或真实历史权限。

六项顶层测试（含子项23）覆盖旧维度、三种新增维度、点/pin边界、dispatch前拒绝、
flat13解析及最大wire预算。未知profile、错误点数/外部key或pin、范围/零pin和23pin
均在dispatch前拒绝；递归回复、末点超cap、带lease历史点由实际host parser拒绝。
原RED在新维度和budget stub处失败，其余旧行为/负例通过。

辅助 `detachedterminaltransportgreen01`、`detachedterminaltransportrace01`、Linux
`detachedterminaltransportvet01` 均通过，分别9155/9160/9263输入。独立完整17项
输入、工具/源码归档及前后指纹审计通过；唯一审查员限定 Spec/Quality PASS，并核对
注释、分段及私有terminal传输包职责，未扩大导出接口。

主分支 `detachedmainterminaltransport01` 六项/23子项通过，9158输入，raw4772字节，
SHA256 `d2f4a065cc72a2148e6ce4a89ad63bd6e53e362cad36b538b5f06f15af54509f`；
完整证据审计通过，两文件与辅助接受版本逐blob相同。证据位于D工作树
`.superpowers/sdd/2026-10-18-etcd-detached-runtime-native`。本次仅接受传输组件；
下一片需复用actual Task和B私有proof，接入真实终态消费者且保持八次读取上限。
