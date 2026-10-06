# 创建响应明确返回是否复用

用户指定创建响应必须包含布尔字段 reused，SDK 同步暴露，调用方不通过缓存或 ID 推断。

- Manager.GetOrCreate 在实际成功分支返回本次申请的 reused：创建成功为 false，已有可用实例及正常恢复复用为 true；该结果不保存到实例状态。
- 创建 HTTP 响应用独立 CreateSandboxResponse 包含必填 reused，false 不省略；GET 响应保留原语义。
- Go SDK 保持 CreateSandbox 的既有返回类型，增加 SandboxResponse.Reused；NewSandbox 的句柄增加 Reused()。
- SDK 对创建响应缺失或 null reused 拒绝将未知结果默认为新建。发布顺序为先部署包含该字段的服务端，再升级 SDK。
- 验证首次创建、跨副本复用、租约恢复、过期替换、并发只有一个 false、无 workspace 每次 false、SDK 在全新客户端中读取服务端 true/false 及必填字段检查。
