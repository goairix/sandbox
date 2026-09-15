# Sentinel Upgrade Availability Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 将 Redis bootstrap 命令从 sandbox-api 镜像中拆出，并用固定 45 秒可续租 FUSE refill lease 与 API 启动实时拓扑门禁保证内置 Sentinel 升级期间的性能和高可用。

**Architecture:** 独立的 `sandbox-redis-bootstrap` 最小镜像只服务 Sentinel StatefulSet 与安装/升级辅助 Job，Helm 用单独 values 契约渲染它，API tag 因此不再改变 Redis PodTemplate。FUSE refill 继续使用 Redis token/revision fencing，但失效 lease 固定 45 秒；新 API 在创建状态 store、运行时池和 HTTP 服务前，使用现有身份公钥对三成员 Sentinel/Redis 拓扑及 `WAIT 1` ACK 做实时证明。

**Tech Stack:** Go 1.25、go-redis v9、Redis 7/Sentinel、Helm 3、Kubernetes 1.33、Docker Buildx、Bash。

---

## 文件结构

- 新建 `docker/images/redis-bootstrap/Dockerfile`：只构建并封装 `cmd/redis-bootstrap`。
- 新建 `scripts/test-redis-bootstrap-image.sh`：验证 API/bootstrap Dockerfile 解耦及 amd64/arm64 可编译性。
- 修改 `docker/Dockerfile`：删除 redis-bootstrap 构建和复制，只保留 API 二进制。
- 修改 `deploy/helm/sandbox/templates/_helpers.tpl`：解析、验证并渲染独立 bootstrap 镜像，同时扩充 startupProbe 安全预算。
- 修改三个 Sentinel 模板：所有 built-in helper 使用 bootstrap 镜像，非 Sentinel rollback guard 仍用 API 镜像。
- 修改 `values.yaml`、`values-builtin-sentinel.yaml`、`values.schema.json`：增加带中文注释的镜像契约与 schema。
- 修改 Helm 结构测试和 shell 测试：验证镜像解耦、非法值拒绝、升级稳定及探针预算。
- 修改 `internal/sandbox/fuse_pool.go` 及测试：实现固定 45 秒 lease、15 秒续租与丢锁取消。
- 修改 `internal/storage/state/redis/pool_test.go`：用真实 Redis 时间验证过期接管和旧 token fencing。
- 修改 `cmd/sandbox/redis_bootstrap_gate.go` 及测试：实现可注入测试的实时拓扑门禁。
- 更新两份部署文档、事故记录，并新增整改验证报告。

### Task 1: 拆分最小 Redis bootstrap 镜像

**Files:**
- Create: `docker/images/redis-bootstrap/Dockerfile`
- Create: `scripts/test-redis-bootstrap-image.sh`
- Modify: `docker/Dockerfile`

- [ ] **Step 1: 先写镜像边界失败测试**

创建可执行脚本，检查新 Dockerfile 存在、两个最终镜像互不携带对方命令，并交叉编译两种生产架构：

```bash
#!/usr/bin/env bash
set -euo pipefail
[[ $# == 0 ]] || { printf 'usage: test-redis-bootstrap-image.sh\n' >&2; exit 2; }
repo_root="$(CDPATH= cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)"
api="$repo_root/docker/Dockerfile"
bootstrap="$repo_root/docker/images/redis-bootstrap/Dockerfile"
test -f "$bootstrap"
grep -Fq './cmd/redis-bootstrap' "$bootstrap"
grep -Fq '/app/redis-bootstrap' "$bootstrap"
! grep -Fq '/app/sandbox' "$bootstrap"
! grep -Fq 'docker-cli' "$bootstrap"
! grep -Fq './cmd/redis-bootstrap' "$api"
! grep -Fq '/app/redis-bootstrap' "$api"
tmp="$(mktemp -d "${TMPDIR:-/tmp}/sandbox-bootstrap-build.XXXXXX")"
trap 'rm -f -- "$tmp/redis-bootstrap-amd64" "$tmp/redis-bootstrap-arm64"; rmdir -- "$tmp"' EXIT
for arch in amd64 arm64; do
  GOPROXY=off CGO_ENABLED=0 GOOS=linux GOARCH="$arch" \
    go build -trimpath -ldflags='-s -w' -o "$tmp/redis-bootstrap-$arch" ./cmd/redis-bootstrap
  test -x "$tmp/redis-bootstrap-$arch"
done
printf 'redis bootstrap image contract: PASS\n'
```

- [ ] **Step 2: 运行测试并确认它因新 Dockerfile 不存在而失败**

Run: `bash scripts/test-redis-bootstrap-image.sh`

Expected: FAIL at `test -f docker/images/redis-bootstrap/Dockerfile`.

- [ ] **Step 3: 新增独立多架构 Dockerfile**

写入：

```dockerfile
# syntax=docker/dockerfile:1
FROM --platform=$BUILDPLATFORM golang:1.25.4 AS builder
ARG TARGETOS=linux
ARG TARGETARCH
WORKDIR /src
COPY go.mod go.sum ./
RUN GOPROXY=https://goproxy.cn,direct go mod download
COPY cmd/redis-bootstrap ./cmd/redis-bootstrap
COPY internal ./internal
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -trimpath -ldflags='-s -w' -o /out/redis-bootstrap ./cmd/redis-bootstrap

FROM alpine:3.22
RUN apk add --no-cache ca-certificates
WORKDIR /app
COPY --from=builder /out/redis-bootstrap /app/redis-bootstrap
ENTRYPOINT ["/app/redis-bootstrap"]
```

- [ ] **Step 4: 从 API Dockerfile 删除 helper 命令**

保留 `./cmd/sandbox` 构建、`/app/sandbox`、`var`、配置文件和 `docker-cli`；删除下面两行：

```dockerfile
RUN CGO_ENABLED=0 go build -trimpath -ldflags='-s -w' -o /redis-bootstrap ./cmd/redis-bootstrap/
COPY --from=builder /redis-bootstrap /app/redis-bootstrap
```

- [ ] **Step 5: 验证静态边界和两架构编译**

Run: `chmod +x scripts/test-redis-bootstrap-image.sh && bash scripts/test-redis-bootstrap-image.sh`

Expected: `redis bootstrap image contract: PASS`.

- [ ] **Step 6: 验证两个 Go 命令仍可独立构建**

Run: `go build ./cmd/sandbox ./cmd/redis-bootstrap`

Expected: exit 0，仓库根目录不产生二进制文件。

- [ ] **Step 7: 提交镜像拆分**

```bash
git add docker/Dockerfile docker/images/redis-bootstrap/Dockerfile scripts/test-redis-bootstrap-image.sh
git commit -m "build: split Redis bootstrap image"
```

### Task 2: 让 Helm 独立渲染 bootstrap 镜像且保持 API-only 升级稳定

**Files:**
- Modify: `deploy/helm/sandbox/templates/_helpers.tpl`
- Modify: `deploy/helm/sandbox/templates/redis-sentinel.yaml`
- Modify: `deploy/helm/sandbox/templates/redis-sentinel-identity.yaml`
- Modify: `deploy/helm/sandbox/templates/pre-rollback-backend-guard.yaml`
- Modify: `deploy/helm/sandbox/values.yaml`
- Modify: `deploy/helm/sandbox/values-builtin-sentinel.yaml`
- Modify: `deploy/helm/sandbox/values.schema.json`
- Modify: `internal/helmtest/redis_sentinel_test.go`
- Modify: `internal/helmtest/sentinel_defaults_test.go`
- Modify: `scripts/test-helm-chart.sh`

- [ ] **Step 1: 写 API tag 不改变 Redis PodTemplate 的失败测试**

在 `internal/helmtest/redis_sentinel_test.go` 增加测试；先渲染两个 API tag，再比较 StatefulSet 的完整 `spec.template`：

```go
func TestSentinelBootstrapImageIsIndependentFromAPI(t *testing.T) {
	base := append(sentinelOverrides(),
		"redis.sentinel.bootstrapImage.repository=registry.example.com/sandbox-redis-bootstrap",
		"redis.sentinel.bootstrapImage.tag=v0.1.0",
		"redis.sentinel.bootstrapImage.pullPolicy=IfNotPresent",
	)
	a := render(t, append(append([]string{}, base...), "image.tag=v1.0.0")...)
	b := render(t, append(append([]string{}, base...), "image.tag=v1.0.1")...)
	aTemplate := mapping(t, mapping(t, object(t, a, "StatefulSet", "sandbox-redis-sentinel")["spec"])["template"])
	bTemplate := mapping(t, mapping(t, object(t, b, "StatefulSet", "sandbox-redis-sentinel")["spec"])["template"])
	if !reflect.DeepEqual(aTemplate, bTemplate) {
		t.Fatal("API-only tag change altered Redis StatefulSet PodTemplate")
	}
	want := "registry.example.com/sandbox-redis-bootstrap:v0.1.0"
	pod := mapping(t, aTemplate["spec"])
	for _, c := range []map[string]any{container(t, pod, "initContainers", "prepare"), container(t, pod, "initContainers", "identity")} {
		if c["image"] != want || c["imagePullPolicy"] != "IfNotPresent" {
			t.Fatalf("Sentinel helper image mismatch: %#v", c)
		}
	}
	for _, pair := range [][3]string{{"Job", "sandbox-redis-sentinel-identity-1", "ensure-identity"}, {"Job", "sandbox-redis-sentinel-initialize-1", "initialize"}, {"Job", "sandbox-rollback-backend-guard", "verify-backend"}} {
		jobPod := mapping(t, mapping(t, mapping(t, object(t, a, pair[0], pair[1])["spec"])["template"])["spec"])
		if c := container(t, jobPod, "containers", pair[2]); c["image"] != want {
			t.Fatalf("%s image = %v", pair[1], c["image"])
		}
	}
}
```

同时导入 `reflect`，并把旧断言文字“使用当前 API bootstrap binary”改为“使用独立 bootstrap binary”。

- [ ] **Step 2: 运行结构测试并确认 StatefulSet 仍随 API tag 改变**

Run: `go test -tags helmtests ./internal/helmtest -run TestSentinelBootstrapImageIsIndependentFromAPI -count=1`

Expected: FAIL，`prepare`/`identity` 仍渲染 sandbox-api 镜像。

- [ ] **Step 3: 写 values/schema 非法配置失败测试**

在同一测试文件增加表驱动测试，分别传入空 repository、空 tag、非法 pullPolicy，要求 Helm 在生成资源前失败：

```go
func TestSentinelBootstrapImageRejectsInvalidValues(t *testing.T) {
	_, file, _, _ := runtime.Caller(0)
	chart := filepath.Join(filepath.Dir(file), "..", "..", "deploy", "helm", "sandbox")
	for _, invalid := range []string{"string:redis.sentinel.bootstrapImage.repository=", "string:redis.sentinel.bootstrapImage.tag=", "redis.sentinel.bootstrapImage.pullPolicy=Sometimes"} {
		t.Run(strings.ReplaceAll(invalid, "/", "_"), func(t *testing.T) {
			args := []string{"template", "sandbox", chart, "--namespace", "release-ns", "--kube-version", "1.33.0"}
			for _, value := range append(sentinelOverrides(), invalid) {
				flag := "--set"
				if strings.HasPrefix(value, "string:") { flag, value = "--set-string", strings.TrimPrefix(value, "string:") }
				args = append(args, flag, value)
			}
			if output, err := exec.Command("helm", args...).CombinedOutput(); err == nil || !strings.Contains(string(output), "bootstrapImage") {
				t.Fatalf("invalid bootstrap image accepted: %s", output)
			}
		})
	}
}
```

再用 `partialSentinelChart(t, false, ...)` 从 Chart 自身 `values.yaml` 删除整个
`redis.sentinel.bootstrapImage`，执行 `helm template` 并断言错误包含
`redis.sentinel.bootstrapImage 必须是配置映射`。这条测试证明即使部署目录没有同步
schema，built-in Sentinel 也不会隐式回退到 API 镜像。standalone 与 external 的同类
partial chart 继续渲染成功，证明未启用内置 Sentinel 时不消费该字段。

- [ ] **Step 4: 增加 Helm helper 与 values 契约**

在 `_helpers.tpl` 增加独立 helper，并从 `sandbox.sentinelConfig` 的通用拷贝中排除 `bootstrapImage`：

```gotemplate
{{- define "sandbox.sentinelBootstrapImageConfig" -}}
{{- $input := get .Values.redis.sentinel "bootstrapImage" -}}
{{- if or (kindIs "invalid" $input) (not (kindIs "map" $input)) -}}{{ fail "redis.sentinel.bootstrapImage 必须是配置映射" }}{{- end -}}
{{- $image := dict -}}
{{- range $key := list "repository" "tag" "pullPolicy" -}}
{{- $value := get $input $key -}}
{{- if not (kindIs "string" $value) -}}{{ fail (printf "redis.sentinel.bootstrapImage.%s 必须是字符串" $key) }}{{- end -}}
{{- if empty $value -}}{{ fail (printf "redis.sentinel.bootstrapImage.%s 不得为空" $key) }}{{- end -}}
{{- $_ := set $image $key $value -}}
{{- end -}}
{{- if not (has $image.pullPolicy (list "Always" "IfNotPresent" "Never")) -}}{{ fail "redis.sentinel.bootstrapImage.pullPolicy 必须是 Always、IfNotPresent 或 Never" }}{{- end -}}
{{- $image | toJson -}}
{{- end -}}

{{- define "sandbox.sentinelBootstrapImage" -}}
{{- $image := include "sandbox.sentinelBootstrapImageConfig" . | fromJson -}}
{{- printf "%s:%s" $image.repository $image.tag -}}
{{- end -}}
```

在 `sandbox.validateRedis` 的 built-in Sentinel 分支调用一次该 helper。默认 values 增加：

```yaml
    # Redis/Sentinel 身份与初始化专用最小镜像；只在 bootstrap 程序变化时升级。
    bootstrapImage:
      repository: registry.i.huaxisy.com/library/ai-infra/sandbox-redis-bootstrap
      tag: v0.1.0
      pullPolicy: IfNotPresent
```

schema 的 `redis.sentinel.properties` 增加以下对象；模板 helper 负责按模式执行“缺少整
个对象”的条件校验，避免 schema 的无条件 required 误伤 standalone/external：

```json
"bootstrapImage": {
  "type": "object",
  "additionalProperties": false,
  "required": ["repository", "tag", "pullPolicy"],
  "properties": {
    "repository": {"type": "string", "minLength": 1},
    "tag": {"type": "string", "minLength": 1},
    "pullPolicy": {"enum": ["Always", "IfNotPresent", "Never"]}
  }
}
```

- [ ] **Step 5: 将所有 built-in helper 消费者切到独立镜像**

`redis-sentinel.yaml` 与 `redis-sentinel-identity.yaml` 顶部解析：

```gotemplate
{{- $bootstrapImage := include "sandbox.sentinelBootstrapImageConfig" . | fromJson }}
```

所有直接执行 `/app/redis-bootstrap` 的容器改为：

```gotemplate
image: {{ include "sandbox.sentinelBootstrapImage" . | quote }}
imagePullPolicy: {{ $bootstrapImage.pullPolicy }}
```

`pre-rollback-backend-guard.yaml` 先计算两个局部变量，确保非 built-in 模式行为不变：

```gotemplate
{{- $guardImage := include "sandbox.apiImage" . -}}
{{- $guardPullPolicy := .Values.image.pullPolicy -}}
{{- if $builtinSentinel -}}
{{- $bootstrapImage := include "sandbox.sentinelBootstrapImageConfig" . | fromJson -}}
{{- $guardImage = include "sandbox.sentinelBootstrapImage" . -}}
{{- $guardPullPolicy = $bootstrapImage.pullPolicy -}}
{{- end -}}
```

- [ ] **Step 6: 扩充 API startupProbe 恢复预算**

将 helper 的预算改为门禁或 initializer 加 45 秒孤儿 lease 和 180 秒余量：

```gotemplate
{{- $budget := add (max 600 (int $sentinelConfig.initializeTimeoutSeconds)) 225 -}}
```

把 `TestSentinelAPIStartupProbeCoversGateAndRuntimeMargin` 的最小预算断言从 780 更新为 825，并继续验证用户更长预算不被缩短。

- [ ] **Step 7: 更新覆盖示例与 Shell 回归断言**

在 `values-builtin-sentinel.yaml` 增加相同 `bootstrapImage`。在 `scripts/test-helm-chart.sh` 检查默认 repository/tag，并比较仅 `image.tag` 不同的两个 StatefulSet `spec.template`，要求零差异。

- [ ] **Step 8: 运行 Helm 测试**

```bash
go test -tags helmtests ./internal/helmtest -count=1
bash scripts/test-helm-chart.sh
bash scripts/test-helm-sentinel-auth.sh
helm lint deploy/helm/sandbox
```

Expected: 全部 PASS；standalone 不渲染 Sentinel；built-in helper 全部使用独立镜像。

- [ ] **Step 9: 提交 Helm 解耦**

```bash
git add deploy/helm/sandbox internal/helmtest/redis_sentinel_test.go internal/helmtest/sentinel_defaults_test.go scripts/test-helm-chart.sh
git commit -m "feat(helm): decouple Sentinel bootstrap image"
```

### Task 3: 将 FUSE refill 锁改为 45 秒可续租 lease

**Files:**
- Modify: `internal/sandbox/fuse_pool.go`
- Modify: `internal/sandbox/fuse_pool_test.go`
- Modify: `internal/storage/state/redis/pool_test.go`

- [ ] **Step 1: 写配置无关的 TTL 失败测试**

```go
func TestFUSEPoolRefillLeaseIsBoundedIndependentlyOfPoolSize(t *testing.T) {
	cfg := fusePoolConfig()
	cfg.MaxSize = 20
	cfg.PrepareTimeout = 120 * time.Second
	cfg.RefillInterval = time.Minute
	pool := NewFUSEPool(newFUSEMockRuntime(), newMemoryFUSEPoolRepository(), cfg, fixedFUSESpec("pool-key"))
	assert.Equal(t, 45*time.Second, pool.refillLockTTL())
	assert.Equal(t, 15*time.Second, pool.refillLockRenewInterval())
}
```

- [ ] **Step 2: 运行测试并确认旧实现返回约 42 分钟**

Run: `go test ./internal/sandbox -run TestFUSEPoolRefillLeaseIsBoundedIndependentlyOfPoolSize -count=1`

Expected: FAIL，实际 TTL 为 `42m0s`。

- [ ] **Step 3: 实现固定 TTL 与生产续租周期**

常量区增加：

```go
fusePoolRefillLeaseTTL           = 45 * time.Second
fusePoolRefillLeaseRenewInterval = 15 * time.Second
```

`FUSEPool` 增加仅供包内测试缩短 ticker 的 `refillLeaseRenewInterval time.Duration`，并实现：

```go
func (p *FUSEPool) refillLockTTL() time.Duration { return fusePoolRefillLeaseTTL }

func (p *FUSEPool) refillLockRenewInterval() time.Duration {
	if p.refillLeaseRenewInterval > 0 { return p.refillLeaseRenewInterval }
	return fusePoolRefillLeaseRenewInterval
}
```

`startRefillLease` 用 `p.refillLockRenewInterval()` 建 ticker，续租仍传固定 TTL。删除旧的 pool size/prepare timeout/overflow 计算。

- [ ] **Step 4: 让丢锁和续租测试快速且确定**

在 `TestFUSEPoolLostRefillLockCancelsPreparationAndLeavesNoReservation` 创建 pool 后设置 `pool.refillLeaseRenewInterval = time.Millisecond`。增加成功续租测试：推进 memory repository 的 `now`，等待毫秒 ticker 后确认锁的 `until` 重新等于服务器时间加 45 秒，结束时调用 `lease.stop()`。

- [ ] **Step 5: 增加真实 Redis 过期接管与 fencing 测试**

```go
func TestFUSEPoolRefillLockExpiresAndFencesOldOwner(t *testing.T) {
	skipIfNoRedis(t)
	s := testStore(t)
	repo := NewFUSEPoolRepository(s)
	poolKey := poolTestID("lease")
	cleanupFUSEPool(t, s, []string{poolKey}, nil)
	require.True(t, mustRefillLock(t, repo, poolKey, "old", 100*time.Millisecond))
	require.Eventually(t, func() bool {
		ok, err := repo.TryRefillLock(context.Background(), poolKey, "new", time.Second)
		return err == nil && ok
	}, 2*time.Second, 20*time.Millisecond)
	ok, err := repo.RenewRefillLock(context.Background(), poolKey, "old", time.Second)
	require.NoError(t, err)
	assert.False(t, ok)
	require.NoError(t, repo.UnlockRefill(context.Background(), poolKey, "new"))
}
```

- [ ] **Step 6: 运行池与 Redis repository 测试**

```bash
go test ./internal/sandbox -run 'TestFUSEPool(RefillLease|LostRefillLock|ExpiredLock)' -count=1
go test ./internal/storage/state/redis -run 'TestFUSEPool(RefillLockExpires|RenewRefillLock)' -count=1
```

Expected: PASS；没有测试 Redis 时沿用既有明确 SKIP 行为。

- [ ] **Step 7: 运行 race 回归**

Run: `go test -race ./internal/sandbox ./internal/storage/state/redis -count=1`

Expected: PASS，无 ticker/goroutine 泄漏和 data race。

- [ ] **Step 8: 提交 lease 修复**

```bash
git add internal/sandbox/fuse_pool.go internal/sandbox/fuse_pool_test.go internal/storage/state/redis/pool_test.go
git commit -m "fix: bound FUSE refill lease recovery"
```

### Task 4: API 启动前验证实时 Sentinel 拓扑与复制 ACK

**Files:**
- Modify: `cmd/sandbox/redis_bootstrap_gate.go`
- Modify: `cmd/sandbox/redis_bootstrap_gate_test.go`

- [ ] **Step 1: 写 verifier 参数与重试失败测试**

增加单元测试依赖 helper：

```go
func successfulRedisBootstrapGateDependencies() redisBootstrapGateDependencies {
	return redisBootstrapGateDependencies{
		waitInitialized: redisbootstrap.WaitBootstrapInitialized,
		readFiles:       redisbootstrap.ReadBootstrapFiles,
		verifyTopology:  func(context.Context, redisbootstrap.TopologyOptions, bool) error { return nil },
		waitRetry:       func(context.Context) error { return nil },
	}
}
```

把现有 initialized/endpoint 测试改为调用 `waitForRedisBootstrapWithDependencies`。新增测试让 verifier 前两次失败、第三次成功，并断言 `all=true`、固定 Registration/PublicKeys、MasterName、两套独立密码和 `time.Duration(c.AckTimeoutMS)*time.Millisecond` 均正确，最终调用次数为 3。

同时把 `apiBootstrapFixture` 的配置补为 `AckTimeoutMS: 1000`；测试必须比较 verifier
收到的 ACK timeout 恰好为一秒，不能用零值绕过 `TopologyOptions` 的有效性契约。

- [ ] **Step 2: 写 projection 替换 fail-closed 测试**

让 `readFiles` 第一次返回原 fixture，verifier 成功后第二次返回 ClusterID 被修改的副本；断言 `errRedisBootstrapGate`。另加 verifier 永久失败 + 50ms caller deadline 测试，断言 `context.DeadlineExceeded`。

- [ ] **Step 3: 运行测试并确认缺少注入入口和拓扑调用**

Run: `go test ./cmd/sandbox -run TestRedisBootstrapGate -count=1`

Expected: FAIL，依赖类型和带依赖门禁函数尚未定义。

- [ ] **Step 4: 实现依赖边界与实时验证循环**

```go
const redisBootstrapGateTimeout = 10 * time.Minute

type redisBootstrapGateDependencies struct {
	waitInitialized func(context.Context, redisbootstrap.BootstrapFilePaths) error
	readFiles       func(context.Context, redisbootstrap.BootstrapFilePaths) (redisbootstrap.BootstrapFiles, error)
	verifyTopology  func(context.Context, redisbootstrap.TopologyOptions, bool) error
	waitRetry       func(context.Context) error
}

var defaultRedisBootstrapGateDependencies = redisBootstrapGateDependencies{
	waitInitialized: redisbootstrap.WaitBootstrapInitialized,
	readFiles:       redisbootstrap.ReadBootstrapFiles,
	verifyTopology:  redisbootstrap.VerifyTopology,
	waitRetry: func(ctx context.Context) error {
		timer := time.NewTimer(time.Second)
		defer timer.Stop()
		select { case <-ctx.Done(): return ctx.Err(); case <-timer.C: return nil }
	},
}
```

`waitForRedisBootstrap` 调用带依赖版本。带依赖版本沿用静态契约和 endpoint 一一绑定，然后循环：

```go
options := redisbootstrap.TopologyOptions{
	Registration: *files.Registration, PublicKeys: files.PublicKeys,
	MasterName: c.MasterName, DataPassword: c.Password, SentinelPassword: c.SentinelPassword,
	AckTimeout: time.Duration(c.AckTimeoutMS) * time.Millisecond,
}
if err := dependencies.verifyTopology(ctx, options, true); err != nil {
	if ctx.Err() != nil { return ctx.Err() }
	if err := dependencies.waitRetry(ctx); err != nil { return err }
	continue
}
current, err := dependencies.readFiles(ctx, paths)
if err != nil || !sameRedisBootstrapFiles(files, current) {
	if ctx.Err() != nil { return ctx.Err() }
	return errRedisBootstrapGate
}
return nil
```

`sameRedisBootstrapFiles` 先比较 Cluster，再要求两侧 Registration 同为非空并比较解引用后
的值，最后对三把 `ed25519.PublicKey` 使用 `bytes.Equal`。依赖函数为空、registration
缺失、phase 非 Initialized、endpoint 重复或缺失立即返回稳定错误。日志只记录开始和
成功阶段，不包含凭据、公钥、proof、Redis 响应或 token；重试不逐秒输出 ERROR。

- [ ] **Step 5: 验证 cleanup/audit 绕过与 main 启动顺序**

保留 `cleanup == true` 的最早返回。确认 `main.go` 中门禁仍在 runtime、state store、普通/FUSE pool 和 HTTP server 之前；不改变 drain/audit/recovery 路径。

- [ ] **Step 6: 运行 API 单元与 race 测试**

```bash
go test ./cmd/sandbox -run 'TestRedisBootstrapGate|TestRedisOptions' -count=1
go test -race ./cmd/sandbox ./internal/redisbootstrap -count=1
```

Expected: PASS；永久失败只在 caller deadline 后退出；成功调用使用 `all=true`。

- [ ] **Step 7: 运行真实三成员 Sentinel verifier**

Run: `bash scripts/test-built-in-redis-sentinel.sh`

Expected: `TestSentinelRuntimeThreeMembers` 与 `TestSentinelRuntimeNewIP` PASS，最后 owned containers、volumes、network 为零。fixture 证明门禁调用的同一 `VerifyTopology` 实现完成真实身份、切主、lineage、barrier 和 ACK 验证；调用/重试/projection 绑定由单元测试覆盖。

- [ ] **Step 8: 提交 API 门禁**

```bash
git add cmd/sandbox/redis_bootstrap_gate.go cmd/sandbox/redis_bootstrap_gate_test.go
git commit -m "fix: verify Sentinel topology before API startup"
```

### Task 5: 更新中文部署说明并完成全量验证

**Files:**
- Modify: `docs/deployment/built-in-redis-sentinel.md`
- Modify: `docs/deployment/helm-deployment-upgrade.md`
- Modify: `docs/testing/2026-09-15-sentinel-api-upgrade-restart-incident.md`
- Create: `docs/testing/2026-09-15-sentinel-upgrade-availability-remediation.md`

- [ ] **Step 1: 更新 Sentinel 最终 values**

删除 API 镜像包含 helper 的旧说明，明确首次上线需要：

```yaml
image:
  repository: registry.i.huaxisy.com/library/ai-infra/sandbox-api
  tag: REPLACE_WITH_NEW_SANDBOX_API_TAG
redis:
  sentinel:
    bootstrapImage:
      repository: registry.i.huaxisy.com/library/ai-infra/sandbox-redis-bootstrap
      tag: v0.1.0
      pullPolicy: IfNotPresent
```

说明 bootstrap tag 只在 `cmd/redis-bootstrap` 改变时升级，不能与 API tag 绑定，也不回退到 API 镜像。

- [ ] **Step 2: 补齐多架构构建命令**

在 Buildx 章节加入：

```bash
SANDBOX_BOOTSTRAP_VERSION=v0.1.0
docker buildx build \
  --builder sandbox-apparmor-build \
  --file docker/images/redis-bootstrap/Dockerfile \
  --platform "$SANDBOX_BUILD_PLATFORMS" \
  --tag "$SANDBOX_BUILD_REGISTRY/sandbox-redis-bootstrap:$SANDBOX_BOOTSTRAP_VERSION" \
  --push .
docker buildx imagetools inspect \
  "$SANDBOX_BUILD_REGISTRY/sandbox-redis-bootstrap:$SANDBOX_BOOTSTRAP_VERSION"
```

保留 API 独立构建命令，写明只改 API Go 代码时不重建 bootstrap 镜像。

- [ ] **Step 3: 写清首次迁移与后续升级行为**

首次 helper 迁移会触发一次预期的 OrderedReady 三成员滚动；每次只允许一个成员不可用并核对 quorum。迁移完成后再只改 API tag，Redis StatefulSet `currentRevision`、Pod UID、restartCount 必须不变。

- [ ] **Step 4: 更新事故状态和整改报告**

事故记录追加修复提交、45 秒恢复上限和部署后验证项。整改报告逐条记录实际命令、退出码、PASS/SKIP、未构建推送镜像和未操作线上集群的边界。

- [ ] **Step 5: 运行格式化和定向全套测试**

```bash
gofmt -w cmd/sandbox/redis_bootstrap_gate.go cmd/sandbox/redis_bootstrap_gate_test.go internal/sandbox/fuse_pool.go internal/sandbox/fuse_pool_test.go internal/storage/state/redis/pool_test.go internal/helmtest/redis_sentinel_test.go internal/helmtest/sentinel_defaults_test.go
bash scripts/test-redis-bootstrap-image.sh
go test ./cmd/sandbox ./internal/redisbootstrap ./internal/sandbox ./internal/storage/state/redis -count=1
go test -tags helmtests ./internal/helmtest -count=1
bash scripts/test-helm-chart.sh
bash scripts/test-helm-sentinel-auth.sh
bash scripts/test-built-in-redis-sentinel.sh
```

Expected: 全部 PASS；真实 Redis 不可用时只有依赖测试 Redis 的既有用例可明确 SKIP，Docker fixture 不静默跳过。

- [ ] **Step 6: 运行静态与竞态验证**

```bash
go test -race ./cmd/sandbox ./internal/redisbootstrap ./internal/sandbox ./internal/storage/state/redis -count=1
go vet ./cmd/sandbox ./internal/redisbootstrap ./internal/sandbox ./internal/storage/state/redis
golangci-lint run ./cmd/sandbox/... ./cmd/redis-bootstrap/... ./internal/redisbootstrap/... ./internal/sandbox/... ./internal/storage/state/redis/...
go build ./cmd/sandbox ./cmd/redis-bootstrap
git diff --check
```

Expected: 全部 exit 0，lint 为 0 issues，`git diff --check` 无输出。

- [ ] **Step 7: 复核 Chart 渲染差异**

渲染相同 bootstrapImage、不同 API tag 的两份 YAML，仅比较 Sentinel StatefulSet `spec.template`，要求零差异；再改变 bootstrapImage tag，要求只有 helper 镜像字段变化。Deployment 保持 `maxUnavailable: 0`、`maxSurge: 1`，默认 startupProbe 预算不少于 945 秒。

- [ ] **Step 8: 提交文档和验证证据**

```bash
git add docs/deployment docs/testing/2026-09-15-sentinel-api-upgrade-restart-incident.md docs/testing/2026-09-15-sentinel-upgrade-availability-remediation.md
git commit -m "docs: document Sentinel availability upgrade"
```

- [ ] **Step 9: 部署后验收边界**

用户推送两个镜像并更新测试集群后，不回滚、不删除 Redis key：

```bash
kubectl --context ds-ai-research -n aiadp-sandbox-fuse rollout status statefulset/sandbox-fuse-redis-sentinel --timeout=20m
kubectl --context ds-ai-research -n aiadp-sandbox-fuse rollout status deployment/sandbox-fuse-api --timeout=20m
kubectl --context ds-ai-research -n aiadp-sandbox-fuse get pods -o wide
kubectl --context ds-ai-research -n aiadp-sandbox-fuse get statefulset sandbox-fuse-redis-sentinel -o jsonpath='{.status.currentRevision}{"\n"}{.status.updateRevision}{"\n"}'
```

先完成首次 bootstrap 镜像迁移，再做一次 API-only tag upgrade。第二次升级前后保存 Redis Pod UID/restartCount/currentRevision，要求完全不变；API 逐副本 Ready。受控终止持有 refill lease 的 API 后，不人工删锁，要求 45 秒内另一副本接管并补池，最后审计 session、workspace、普通/FUSE pool、Pod、NetworkPolicy/CiliumNetworkPolicy 无测试残留。
