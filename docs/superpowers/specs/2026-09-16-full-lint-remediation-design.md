# 全仓 lint 整改设计

## 目标与范围

用户选择处理最新后续清单第 1 项。直接在当前工作区整改，不创建 worktree、不检查分支差异，
完成后提交。保留性能与安全边界，不修改镜像 tag、Helm values、公开协议或线上部署。

2026-09-16 重新运行默认 lint 仍报告 81 项；取消输出上限后，实际基线是 197 项：errcheck
104、ineffassign 2、staticcheck 82、unused 9。因此验收必须取消上限，不以默认截断结果为全仓
总数。

## 方案比较与选择

采用按根因分类的最小整改：先区分有业务意义的错误与纯资源回收，再修正失效测试及安全相关
死代码，最后处理等价表达式与文字规范。这样每类行为变化都有独立回归和可审核差异。

另一种方式是整体自动修复，速度较快，但不能识别 nil-context 拒绝测试、销毁同步屏障及假
reactor 的接口重叠；还需要逐项人工复核。只调整 lint 排除配置会缩小检查范围，不符合全仓
整改目标。本次不采用这两种方式。

## 错误处理

- Docker 与 Kubernetes legacy 文件列表、stat、count 的解析错误不能继续伪装成 size/count 0；
  用严格的数值解析和有界错误消息，两个 runtime 保持相同语义，不打印原始不可信输出。
- SSE 客户端写失败后结束 handler，由既有清理机制收尾；不继续循环、阻塞 producer 或改变
  cleanup 确认协议。先用可控 ResponseWriter 复现失败写入后的旧行为。
- 写入、同步、读取协议的最终确认错误继续传播。文件/pipe/socket 的纯资源释放错误只有在
  不影响已验证协议结果、或属于已经关闭后的幂等回收时，才允许带明确理由的忽略。
- 单测中的 Start/Stop/WarmUp、JSON 解码、tracker 删除和 stream close 采用与场景匹配的
  成功/失败断言，不一律假定清理成功，不削弱故障注入场景。

## staticcheck 与死代码

- fake Kubernetes reactor 不能用结构型 GetAction 接口抢先匹配 DeleteAction。按 action verb
  分派后，再读取对应操作字段；新增删除分支行为回归。
- Docker 安全测试当前对只有未导出字段的 fakeContainer 做 JSON marshal，结果是空对象，
  不能证明凭据不进入 container config。改为检查真正的 Config/HostConfig 等可序列化配置，
  保留并强化无凭据落盘断言。
- nil context 的故意拒绝测试与 exclusiveUse 的销毁同步屏障必须保留。优先用清晰结构表达
  原语义；确属 lint 误报时，只允许相应行、指定规则、带行为理由的窄例外，不能全局关闭规则。
- 只有经全仓引用核对确实无调用的 private helper 才移除，尤其是遗留 name-only 网络策略
  路径；不将当前 exact UID、replacement protection、Cilium/standard 路径误删。
- 无效赋值、De Morgan 等价表达式、类型转换、错误字符串和 Docker deprecated type 仅作
  等价整理，不新增共享锁、goroutine、Redis key、控制面往返或业务可配置项。

## 验证与交付

有行为变化的解析、流式写失败、reactor 和安全测试先观察 RED，再完成最小修复并验证 GREEN。
机械等价改动以 lint 的原失败及已有行为回归为证据，不编造功能 RED。

最终在当前宿主环境及生产 Linux/arm64 构建目标分别运行无上限 golangci-lint，要求 0 issues。
执行全仓 test/build/vet、目标 runtime/sandbox/mounter/workspace-probe race、Helm 回归、diff
检查；不配置外部环境而跳过的测试不计作真实集群通过。跨 CNI 第 3 项等待用户提供环境，FUSE
性能专项继续暂缓。

本设计是书面复核入口，尚未实施代码修复；用户确认后编写逐项计划并内联执行。
