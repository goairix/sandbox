# 普通池删除中断后的策略收尾整改

## 本批范围

本批针对 v0.3.20 现场验证确认的普通池清理缺口，沿用已经确认的精确 UID / CAS 方案。没有调整 Helm 配置、镜像版本或 Redis 数据布局，没有操作 `ds-ai-research` 集群，也没有删除历史孤儿策略。

FUSE 创建耗时、销毁长尾、Redis Cluster 及真实高可用故障矩阵不属于本批完成项；Calico / 双华云仍需真实环境验收。

## 原因与修复

普通池退休预算为 5 秒。Pod 正常终止尚未确认时，策略删除不会执行；后续重试如果 Pod 已消失，原 `cleanupRecord` 会跳过 runtime 删除、直接退役池记录；指定 UID 的 `RemoveOrdinarySandbox` 也会提前返回 NotFound。这两层快捷分支会丢掉绑定策略的清理机会与重试证据。

现在清理顺序为：CAS 发布 cleanup → 使用持久化 RuntimeRef 重试 → 确认策略收尾 → CAS 删除记录。

- runtime 在 Pod 已消失时仍查找同 namespace、同 runtime ID、同目标 UID 的绑定 NP/CNP，并保留策略归属校验、Pod absence 重查、policy UID 删除前置条件和删除后读回确认。
- 不同 UID 或其它 namespace 的策略不参与删除；遇到同名替换 Pod 拒绝确认成功。
- 目标 UID 匹配但策略归属/selector 异常时返回 `ErrNetworkStateUncertain`，不能以另一条正常策略删除成功掩盖异常资源。
- 池记录已绑定 RuntimeID/UID 时，即使 inventory 没返回 Pod 也必须调用精确 remover；NotFound 必须由既有 policy cleaner 收尾，没有能力、权限失败或读回不确定都保留 cleanup 记录。
- 未绑定 runtime 的准备项不猜测 ID，不获得额外删除权限。
- 可选 Cilium resource 的 404 不阻断普通策略收尾；403 等错误继续上报。该收尾不依赖当前选择 Cilium 作为网络提供方，标准 NetworkPolicy 路径同样适用。
- Pod 删除确认的 context 超时/取消保留原错误并增加 `ErrTerminationUnconfirmed` 分类。退休日志仅对具有 context 中断因果的该分类降为 WARN；权限失败、替换等真实问题仍为 ERROR。

保留 5 秒退休预算和既有 claim/owner fencing：不增加长时间锁持有、额外后台任务或稳态全量孤儿扫描，也不采用强删 Pod / 直接删 Redis 记录。

## 回归与验证

修复前的失败回归直接证明：Pod 不存在但策略存在时 runtime 提前 NotFound；普通池会返回 nil 并删除记录。独立复审发现的“正常 NP/CNP 删除成功掩盖另一条畸形目标策略”也经过 RED → GREEN 验证。

回归覆盖 missing Pod、NP/CNP 精确 UID、其它 namespace、替换 Pod、删除权限失败、可选 CNP API 404、删除读回未消失、目标策略异常、API 调用取消/超时、清理多次失败保留记录、无 cleaner、未绑定准备项，以及旧池退休后保留新池记录。

本地验证使用独立 Redis `127.0.0.1:16381`，不连接业务 Redis。可复现命令：

```sh
go test ./internal/runtime/kubernetes ./internal/sandbox \
  -run 'TestExactOrdinaryRemoval|TestOrdinaryDeletionTimeout|TestOrdinaryPoolCleanup' -count=1
TEST_REDIS_ADDR=127.0.0.1:16381 go test -p 1 ./... -count=1 -timeout=120s
TEST_REDIS_ADDR=127.0.0.1:16381 go test -race -p 1 \
  ./internal/runtime/kubernetes ./internal/sandbox \
  ./internal/storage/state ./internal/storage/state/redis -count=1 -timeout=180s
go build ./...
go vet ./...
golangci-lint run --new-from-rev=HEAD
bash scripts/test-helm-chart.sh
git diff --check
```

部署 API/FUSE 和故障驱动依赖现场环境变量的测试，本批未在线执行；全仓本地测试结果不代替现场验收。增量 lint 结果也不代表历史全仓 lint 问题已清零。

最终结果：上述命令全部退出 0，增量 lint 为 `0 issues`，Helm 检查为 PASS。最终 race 中 Kubernetes runtime / sandbox / state / Redis state 分别为 4.979 / 7.879 / 1.290 / 8.439 秒，未报告 race。独立只读复审的 Important 已修复并重新确认，无剩余 Critical/Important。

本次临时 Redis 容器在验证后停止并移除，临时测试数据随容器删除；没有留下后台验证进程。

## 部署后验收

本批仅需将新实现编入 sandbox-api 镜像，没有新增 values/config 配置。镜像构建与 Helm 升级由用户统一操作；本批没有创建新的镜像版本标签。

部署后需验证旧普通池正常退休时即使跨越短清理预算，Pod 消失后对应 UID 的 NP/CNP 最终也消失，cleanup 记录只在成功后退役，新池仍可跨副本领取。不得把同名替换或权限失败写成收尾成功。

现场历史 `sandbox-sandbox-pool-udjrjjtses` 策略在本批没有删除，不能声称线上全 release 已零遗留；已丢失池记录的历史资源不能靠本次 record 重试修复来承诺自动清除，需要单独核对精确归属证据。
