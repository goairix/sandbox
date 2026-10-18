# source JSON 尾随值拒绝的分配边界

`75fae0b` 修改实际 source 调用链中的三个严格解码器：
`strictPreparationMetadata`、`decodeTypedDomain`、`decodeRuntimeDispatchReceipt`。
原实现为了检查 EOF，会将第二个 JSON 值完整 Decode 到 any，随后才拒绝。
现在只读取下一个 token；只有 EOF 接受，任意 token 或错误仍返回原 corrupt 类别。
typed domain 的 fresh destination 仍在全部检查完成后才安装，receipt 失败仍返回零引用。
没有放宽 schema、字节 cap、canonical 检查或签名绑定。

新增回归对 EOF/尾随空白、对象、数组、null、布尔、数字、字符串、未闭合或非法
尾随内容检查原语义，并检查失败不修改目标。另一回归使用预构造的1024对象数组，
输入在原64KiB通用 decoder cap 内；构造输入不计入分配测量。
原始 RED `sourcejsontailred01` 中语义组通过，但 strict metadata / typed domain
分别8273/8227次分配，超过宽松256次门槛而失败。GREEN 对同一输入为56/13次，
race 为56/14次；不是零分配，也不是整个 worker 的内存或性能证明。

定向 GREEN 两顶层组及22顶层相关 race 回归全部通过，无 SKIP/FAIL/race；覆盖
domain 原子赋值/边界、dispatch、preparation、enrolled publication 以及已接受的
终态 local/detached 认证回归。Linux vet 通过。各轮17类完整 Go 输入集合、逐项
前后哈希、源码/工具归档审计通过。唯一独立审查员限定 Spec/Quality PASS，注释、
逻辑分段和包职责均通过。原生集群没有为这个纯解码错误路径改动另跑，不能称
最新源码完成整个 native 迁移验收。

| 记录 | 退出码 | 输入数 | 日志字节 | 日志 SHA256 |
| --- | --- | --- | --- | --- |
| sourcejsontailred01 | 1 | 9161 | 2542 | 9083f2247f121a8146d453182c445d28678d3fd82a6bafeac8aef0273cc503b7 |
| sourcejsontailgreen01 | 0 | 9161 | 2130 | 3057e7f0d69d24110f4d7d200e5670bd8ab1289038ae4cef42f7efe469b52cef |
| sourcejsontailrace01 | 0 | 9166 | 11416 | b4df8ce717c7563b3361e3ab5dd90cfe4216a7ced76a4c720fcd055118485480 |
| sourcejsontailvet01 | 0 | 9271 | 0 | e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855 |

记录位于 D 工作树 `.superpowers/sdd/2026-10-18-etcd-detached-runtime-native/`，
命令工作目录是主仓库；审查位于主仓库当前 detached-cleanup OWN 下。
本片只关闭上述三处 source 错误路径的整树分配，其他校验器、source workspace、
完整 phase 准入及大 N 容量仍按其各自范围推进。默认生产入口仍为 Redis。
