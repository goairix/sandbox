# Cold quota return 纯校验拆分验证

本改动只提取 return/receipt 校验，供后续 worker 分阶段点读复用。

`decodeColdQuotaReturnProof` 保留原 canonical return 解码及完整 history 等值绑定，立即计算原始值的 SHA-256。中间 stamp 仅保存 attempt、birth 与按值复制的 Entry，不保留原始 KV 或 history 图。`decodeColdQuotaReturnReceipt` 检查原 attempt 对应的精确 key、immutable 与相同 birth，再执行原 receipt 解码及 committed canonical bytes 检查；每次返回独立 Entry 副本。

公开 wrapper 的签名、先校验 return 再读取 4096 字节 receipt 的顺序及错误传播保持原样。原 live/recovery 的 reference 相等检查保持原样。中间 metadata 不产生 worker、mutation 或物理清理权限。新文件保持 etcd 包私有，复用原 record/receipt 不变量，无新增导出接口或跨包可变状态。

## Root 执行结果

| 轮次 | 结果 | 输入数 / 原始输出字节 | 原始输出 SHA-256 |
| --- | --- | --- | --- |
| RED02 | 失败仅为两个批准的新方法尚未存在 | 9103 / 1288 | `3002037103bf5d4796c26c885cb3e4ea400db8979d7fa847baef9c136133ba8c` |
| host01 | PASS，1.456s，新纯校验与原 Stage cohort 顺序回归 | 9104 / 2590 | `2a3f6f1424e5774f89ac7b960c24aa1e1a9a1913536d9634468d2c0741dd5c80` |
| race01 | PASS，3.301s，增加原 prepared、journal、ColdRuntimeQuotaReturn 回归 | 9109 / 10656 | `b70ef7cdc6de51772c2813d494c926914c730e11c8af95c2223de772a6be9f04` |
| Linux vet01 | PASS，Linux arm64、CGO=0，etcd 包 | 9199 / 0 | `e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855` |

各轮保留完整 17 项依赖输入指纹、pre/post、工具记录及复用的仓库范围源码快照，由 Root 核对一致性。沿用一份共享 Go 构建及模块缓存，不打包整个依赖或工具链。host 后只修正新增测试的 gofmt 排版；未削弱测试断言。最终 gofmt 与 diff check 干净。

最初 RED01 的 go-list 因尚未解决的依赖合并冲突而失败，不能算功能 RED。冲突解为已验收主分支相同的 journal/shared-base-helper 内容，完成合并后才执行真实 RED02，确认仅缺少目标 API 后授权实现。

新测试验证释放原 KV/history 后中间值仍然完整、返回副本相互独立，以及错误 history 绑定、错误 key/birth、mutable/leased/malformed/noncanonical/aborted/foreign-attempt receipt 均被拒绝。

本轮没有实现 worker postread、完整认证恢复、Resolve、自动 actor、生产切换或容量验收。host/race 及 Linux 静态检查不代表 native etcd 或大规模运行验证。
