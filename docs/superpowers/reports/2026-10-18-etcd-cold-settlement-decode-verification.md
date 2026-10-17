# Cold settlement 纯校验复用验证

新增私有 `decodeColdSettlement`，直接验证已提供的 receipt，无额外 RPC。共享来源校验保留原 settled/completion、reservation/work 同 ModRevision、晚于 publication birth、canonical work marker 约束；receipt 检查精确派生 key、不可变性、同事务 birth、原解码及 canonical committed 内容。

公开 `verifyColdSettlement` 在原 4096 字节读取前执行同一来源校验，保留错误优先级、签名及 RPC 顺序。外层 quota 历史读取的两次 receipt RPC 未改变；后续 worker 可用一份已认证 receipt 同时满足原两组检查。共享 helper 不授予 runtime、quota 或物理清理权限；保持 etcd 包私有，未扩大导出接口或引入跨包可变状态。注释和逻辑分段已检查。

## Root 真实执行结果

| 轮次 | 结果 | 输入数 / 原始输出字节 | 原始输出 SHA-256 |
| --- | --- | --- | --- |
| RED01 | 仅缺少批准的新方法，真实失败 | 9109 / 698 | `ac147374ffac70b3440b3ca412c38d3f77478c84059b2903176ed30889de7b3d` |
| host01 | PASS，1.303s | 9110 / 4854 | `a8bb7fcb5c2f0861059e00fba015cd277d0b52a382afd8fff2cd071e92ed034a` |
| race01 | PASS，3.351s，包含原 quota prepared/journal/return 回归 | 9115 / 15326 | `0a4d771a712ba536fcfdd7107bc86e3ba0fbdd8416d8081af9f24de791d7f8bd` |
| Linux vet01 | PASS，Linux arm64，CGO=0，etcd 包 | 9205 / 0 | `e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855` |

Root 在后续 Go 或源码修改前，核对各轮全部 17 项编译输入、pre/post、工具、复用仓库快照与当时 current 源码；指纹一致。gofmt/diff check 干净。使用共享 Go 构建和模块缓存，未生成完整依赖或工具链归档。

新测试覆盖无客户端的有效提供 receipt、17 种来源拒绝及公开 wrapper 在非法 locator 前保留来源错误且不发送 RPC，以及 key/birth/mutable/lease/malformed/noncanonical/aborted/foreign-attempt receipt 拒绝。

本切片是历史分步读取的校验依赖，不是自动 worker、完整历史认证、32 任务内存、native etcd、容量或生产迁移验收。
