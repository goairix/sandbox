# Sentinel 模板默认值修复验证

## 问题与范围

线上保留自定义 Chart `values.yaml`，新增内置 Sentinel 模式但遗漏部分默认配置；部署目录未完整同步 schema 时，原模板把缺失字段渲染为 `%!s(<nil>)` DNS、`-timeout=0s` 和 `-ack-timeout=0ms`。身份 Job 失败后，Redis/API 因身份 Secret 缺失无法挂载，初始化 Job 的有效截止时间仅为 30 秒。

本次仅修改本地 Helm 模板、schema、文档和模板测试，不修改运行时程序，不操作线上资源，也不改变已有身份/PVC 的恢复规则。

## 修复

- `sandbox.sentinelConfig` 统一解析 Sentinel 默认值，所有身份、Redis、初始化及 API 消费者复用相同配置规则。
- `sandbox.apiStartupProbe` 补齐缺失的启动探针，并保留用户更长的等待预算。
- 缺失/null 使用默认值；显式的 `false`、零值、非法 DNS/键名、错误类型和越界整数不被默认值掩盖，即使没有 schema 也在渲染阶段检查。
- schema 中有模板默认值的 Sentinel 属性不再强制出现；已配置属性仍受原类型和范围校验约束。
- 身份/PVC、固定成员 DNS、旧 PVC 模板兼容及自动创建身份的授权条件保持不变。

## 验证结果

先写回归测试并运行原模板，明确得到失败：

```text
missing defaults rendered invalid redis arguments:
[redis -master-name=%!s(<nil>) -timeout=0s -down-after-milliseconds=0 -failover-timeout-milliseconds=0 -parallel-syncs=0]
```

修复后以下检查全部通过：

```bash
go test -tags=helmtests ./internal/helmtest ./test/integration/helm -count=1
go test -p 2 ./... -count=1
go vet -tags=helmtests ./internal/helmtest ./test/integration/helm
bash scripts/test-helm-chart.sh
HELM_BACKEND_SWITCH_RUN_CLUSTER=0 bash scripts/test-helm-backend-switch.sh
bash scripts/test-helm-sentinel-auth.sh
bash scripts/test-helm-apparmor-loader.sh
git diff --check
```

新增用例覆盖：有/无 schema 的不完整 Chart values、缺失或不完整启动探针、最小 Sentinel 配置、null 默认项、用户自定义 DNS/主名称/超时/资源/认证键、显式禁用探针及多种非法配置。已有 retained 状态、缺身份拒绝、旧 StatefulSet PVC 模板等回归继续通过。

## 部署边界

同步完整新版 `templates/` 和 `values.schema.json`，可保留线上自定义 `values.yaml`，本次不需要重新构建镜像。

这只证明本地模板和程序回归通过，不代表当前线上 Sentinel 已就绪。此前失败首装已创建新状态 ConfigMap/PVC、但身份 Secret 从未生成时，直接 upgrade 仍会被恢复保护拒绝。须单独核验并处理本次失败首装的精确资源范围后再安装，不能盲目生成身份或删除旧 standalone 数据卷。
