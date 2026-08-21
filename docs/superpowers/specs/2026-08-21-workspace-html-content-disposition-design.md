# Workspace HTML Content-Disposition 设计

## 目标

Sandbox 将容器工作区文件同步回后端对象存储时，为 `.html` 和 `.htm` 文件显式设置 `Content-Disposition: inline`，使支持该对象元数据的存储后端返回明确的内联展示语义。

## 范围

- 全量同步与增量同步采用相同的对象写入选项。
- `.html`、`.htm` 扩展名匹配不区分大小写。
- 非 HTML 文件不设置 `Content-Disposition`，保持现有行为。
- 所有文件继续根据扩展名设置 `Content-Type`；无法识别的扩展名继续使用 `application/octet-stream`。
- 使用 `github.com/goairix/fs v0.3.11` 提供的 `WithContentDisposition`，不修改存储驱动实现。

## 设计

将当前只返回单个 Content-Type option 的 `contentTypeOpt` 替换为统一构造对象写入选项的 `storageWriteOptions`：

```go
func storageWriteOptions(name string) []fs.Option
```

该函数始终返回 `fs.WithContentType(...)`。当文件扩展名是 `.html` 或 `.htm` 时，再追加 `fs.WithContentDisposition("inline")`。

全量同步 `fullSyncFromContainer` 和增量同步 `downloadChangedFiles` 调用 `scoped.Create` 时展开这组选项：

```go
scoped.Create(ctx, name, storageWriteOptions(name)...)
```

这样文件类型判断只保留在一个位置，两个同步路径不会产生行为差异。

## 测试

在 `internal/sandbox/workspace_test.go` 增加表驱动单元测试，将返回的 options 应用到 `fs.Options` 后验证：

- `.html` 得到 `text/html; charset=utf-8` 和 `inline`。
- `.htm` 得到 `text/html; charset=utf-8` 和 `inline`。
- 大写或混合大小写 HTML 扩展名也得到 `inline`。
- 普通文件保留对应 Content-Type，ContentDisposition 为空。
- 未知扩展名得到 `application/octet-stream`，ContentDisposition 为空。

实现完成后运行 `go test ./internal/sandbox/...`，确认同步相关测试通过。

## 兼容性

该变更只为 HTML 对象增加标准响应元数据。非 HTML 文件及本地文件系统行为不变。已存在于后端存储中的对象只有在后续被同步重写时才会获得新的 Content-Disposition。
