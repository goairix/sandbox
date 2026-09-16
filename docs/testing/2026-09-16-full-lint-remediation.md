# 全仓 lint 整改验收

## 结果与真实基线

已关闭 QUALITY-01。宿主无上限 lint 的真实基线为 197 项（104 errcheck、2 ineffassign、
82 staticcheck、9 unused），不是默认输出截断后的 81 项。Linux/arm64 目标额外发现一个
Linux-only 错误字符串规范问题，也已修复。当前两个目标的无上限 lint 均为 0 issues。

本次不改 values/tag、部署、AppArmor 规则、安全权限或公开协议，不构建/推送镜像。

## 行为修复与回归

- 两个 runtime 的文件 count/size/mtime 使用同一严格解析器。拒绝部分解析、负数、溢出、
  NaN/Inf，错误固定且不回显租户输出；合法分数时间戳保持原纳秒语义。原 find/分页命令未变，
  本次不声称解决 legacy shell pipeline 错误被 wc/head 掩盖的所有问题。
- 一次性 SSE 的 heartbeat/事件写失败立即返回，沿用原 durable cleanup。可控失败 writer
  在旧版 handler 上失败，修复后无需等待请求取消即可完成；用真实、独立本地 Redis 7.4
  验证 destroying 记录及清理 context 不依赖请求取消。没有额外 producer、共享锁或同步终止等待。
- Docker 凭据检查原 marshal 未导出字段得到 `{}`，新增 image/managed-label 断言先失败；
  改读真实 Config/HostConfig 后通过，保留 AK/SK 不进容器配置及 bootstrap 的断言。
- concurrent-first-create fake reactor 的结构型 GetAction 抢先匹配 DeleteAction，导致生产
  exact-attempt cleanup 回归失败；按 verb 分派后，standard/Cilium 删除与确认均通过。

## 等价整理及复审边界

错误文字仅首字母规范化；De Morgan、类型转换和 Docker SDK 现行类型为等价改动。移除全仓
没有调用的 9 个 private helper，包括遗留 name-only 网络 upsert/delete；现行 exact UID、
replacement protection 和两种网络提供方主路径未变。

资源回收的显式忽略限定为只读 FD、空设备、pipe/socket 纯释放和已完成协议后的幂等关闭；
没有删除现有 Write/Sync/协议 ACK/关键 Close 的错误检查。HTTP 测试驱动关闭错误合并到返回
error。正常 Start/Stop/WarmUp/JSON/tracker 用例检查成功，故障注入用例检查相应错误，worker
goroutine 使用非 Fatal 断言。故意 nil-context 拒绝负例用一行有理由的 SA1012 窄例外保留，
没有全局排除；exclusiveUse 的销毁同步屏障保留，使用 defer Unlock 表达生命周期。

按已批准的内联计划自审：未增加业务网络往返、Redis key 或 goroutine；没有缩小 lint 范围。
测试 fixture 修正不计作已复现线上故障。五节点现场验收使用用户当前部署镜像，不能把它当作
尚未构建的本地修改已上线证据。

## 验证命令

```sh
golangci-lint run --timeout=3m --max-issues-per-linter=0 --max-same-issues=0
GOOS=linux GOARCH=arm64 golangci-lint run --timeout=3m --max-issues-per-linter=0 --max-same-issues=0
go test ./... -count=1 -timeout=3m
go build ./...
go vet ./...
GOOS=linux GOARCH=arm64 go build ./...
go test -race ./internal/runtime/... ./internal/sandbox ./internal/mounter ./internal/workspaceprobe -count=1 -timeout=3m
TEST_REDIS_ADDR=127.0.0.1:16390 go test ./internal/api/handler -count=1 -timeout=2m
go test -tags helmtests ./internal/helmtest -count=1 -timeout=2m
go test ./test/integration/helm -count=1 -timeout=2m
bash scripts/test-helm-apparmor-loader.sh
git diff --check
```

以上命令均通过；全仓默认运行中需要外部 API/集群的 opt-in 集成用例仍跳过，不冒充现场通过。
专用 Redis 只在本机 loopback、无持久卷，不读取/删除线上 Redis 状态。

用户后续发布功能修复需重建 sandbox-api；mounter 与 Redis-bootstrap 也有本地源码质量改动，
仅资源回收/文字等价语义，不改变 AppArmor profile 或 Sentinel 模板，无需为本次节点验收先
升级线上这两个镜像。跨 CNI 验收仍等用户环境，FUSE 创建性能按要求暂缓。
