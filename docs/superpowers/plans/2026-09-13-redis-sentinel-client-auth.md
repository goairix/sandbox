# Redis Sentinel Client Authentication Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Forward independently configured Redis Sentinel username and password without changing existing data-node credentials, connection validation, or durability behavior.

**Architecture:** Extract the existing address normalization, topology validation, and go-redis UniversalOptions construction into an unexported pure builder. Add SentinelUsername and SentinelPassword to the storage Options and forward them explicitly; New continues to create and ping the client, then configure the existing ACK behavior unchanged. Native config/env and Helm wiring belong to the parent task, not this bounded change.

**Tech Stack:** Go 1.25, github.com/redis/go-redis/v9 v9.18.0, testify.

---

## Scope and file ownership

- Modify `internal/storage/state/redis/store.go`: two credential fields and pure `universalOptions` builder used by New.
- Create `internal/storage/state/redis/options_test.go`: deterministic forwarding, normalization, tuning, and topology-validation tests with no Redis service dependency.
- Create this plan only under `docs/superpowers/plans`.
- Do not change modes, ACK/fencing implementations, native config, CLI, Helm, or other agents' files. Do not commit, push, build release images, or mutate live clusters. Work directly in the authorized current checkout.

### Task 1: Specify missing credential forwarding (RED)

- [x] Create `options_test.go` with table-driven independent credentials and a builder call:

```go
func TestUniversalOptionsSentinelCredentials(t *testing.T) {
    for _, tc := range []struct {
        name, username, password string
    }{
        {name: "unset"},
        {name: "password only", password: "sentinel password\\\""},
        {name: "username only", username: "sentinel-user"},
        {name: "both", username: "sentinel-user", password: "sentinel password\\\""},
    } {
        t.Run(tc.name, func(t *testing.T) {
            got, err := universalOptions(Options{
                Mode: ModeSentinel, Addrs: []string{"sentinel-a:26379", "sentinel-b:26379"},
                MasterName: "sandbox-master", Username: "data-user", Password: "data-password",
                SentinelUsername: tc.username, SentinelPassword: tc.password,
            })
            require.NoError(t, err)
            require.Equal(t, tc.username, got.SentinelUsername)
            require.Equal(t, tc.password, got.SentinelPassword)
            require.Equal(t, "data-user", got.Username)
            require.Equal(t, "data-password", got.Password)
            require.Equal(t, "sandbox-master", got.MasterName)
            require.Equal(t, []string{"sentinel-a:26379", "sentinel-b:26379"}, got.Addrs)
        })
    }
}
```

- [x] Run `go test ./internal/storage/state/redis -run '^TestUniversalOptions' -count=1`. Expected RED: missing `universalOptions`, `SentinelUsername`, and `SentinelPassword`, with no unrelated failure. Record the exact output.

### Task 2: Minimal implementation (GREEN)

- [x] Add the following string fields beside existing Username/Password in Options:

```go
SentinelUsername string
SentinelPassword string
```

- [x] Move only the current normalization/validation into the pure builder, with explicit independent credentials:

```go
func universalOptions(opts Options) (*redis.UniversalOptions, error) {
    mode := opts.Mode
    if mode == "" {
        mode = ModeStandalone
    }
    addrs := append([]string(nil), opts.Addrs...)
    if len(addrs) == 0 && opts.Addr != "" {
        addrs = []string{opts.Addr}
    }
    if len(addrs) == 0 {
        return nil, errors.New("redis: at least one address is required")
    }
    switch mode {
    case ModeStandalone:
        if len(addrs) != 1 {
            return nil, errors.New("redis: standalone mode requires exactly one address")
        }
    case ModeSentinel:
        if opts.MasterName == "" {
            return nil, errors.New("redis: sentinel mode requires a master name")
        }
    case ModeCluster:
        if opts.DB != 0 {
            return nil, errors.New("redis: cluster mode supports database 0 only")
        }
    default:
        return nil, fmt.Errorf("redis: unsupported mode %q", mode)
    }
    return &redis.UniversalOptions{
        Addrs: addrs, MasterName: opts.MasterName, Username: opts.Username,
        Password: opts.Password, SentinelUsername: opts.SentinelUsername,
        SentinelPassword: opts.SentinelPassword, DB: opts.DB, PoolSize: opts.PoolSize,
        MinIdleConns: opts.MinIdleConns, DialTimeout: opts.DialTimeout,
        ReadTimeout: opts.ReadTimeout, WriteTimeout: opts.WriteTimeout,
        MaxRetries: opts.MaxRetries,
    }, nil
}
```

- [x] Replace New's moved block and inline UniversalOptions construction with:

```go
clientOptions, err := universalOptions(opts)
if err != nil {
    return nil, err
}
client := redis.NewUniversalClient(clientOptions)
```

- [x] Run `gofmt -w internal/storage/state/redis/store.go internal/storage/state/redis/options_test.go`, then `go test ./internal/storage/state/redis -run '^TestUniversalOptions' -count=1`. Expected GREEN: all independent credential cases pass. Leave ping cleanup, durability defaulting, ACK settings, and safety commands unchanged.

### Task 3: Compatibility and regression verification

- [x] Before the production extraction, include these deterministic test cases alongside the RED credential tests:

```go
func TestUniversalOptionsDefaultStandalone(t *testing.T) {
    got, err := universalOptions(Options{Addr: "redis:6379"})
    require.NoError(t, err)
    require.Equal(t, &goredis.UniversalOptions{Addrs: []string{"redis:6379"}}, got)
}

func TestUniversalOptionsConnectionTuning(t *testing.T) {
    addrs := []string{"redis-a:6379", "redis-b:6379"}
    got, err := universalOptions(Options{
        Mode: ModeCluster, Addr: "ignored:6379", Addrs: addrs,
        Username: "data-user", Password: "data-password", PoolSize: 17,
        MinIdleConns: 3, DialTimeout: 2 * time.Second, ReadTimeout: 3 * time.Second,
        WriteTimeout: 4 * time.Second, MaxRetries: 5,
    })
    require.NoError(t, err)
    require.Equal(t, &goredis.UniversalOptions{
        Addrs: []string{"redis-a:6379", "redis-b:6379"}, Username: "data-user",
        Password: "data-password", PoolSize: 17, MinIdleConns: 3,
        DialTimeout: 2 * time.Second, ReadTimeout: 3 * time.Second,
        WriteTimeout: 4 * time.Second, MaxRetries: 5,
    }, got)
    addrs[0] = "mutated:6379"
    require.Equal(t, "redis-a:6379", got.Addrs[0])
}

func TestUniversalOptionsStandaloneDatabase(t *testing.T) {
    got, err := universalOptions(Options{Mode: ModeStandalone, Addr: "redis:6379", DB: 4})
    require.NoError(t, err)
    require.Equal(t, 4, got.DB)
}

func TestUniversalOptionsTopologyValidation(t *testing.T) {
    for _, tc := range []struct {
        name string
        opts Options
        want string
    }{
        {"no address", Options{}, "redis: at least one address is required"},
        {"default standalone multiple", Options{Addrs: []string{"a:6379", "b:6379"}}, "redis: standalone mode requires exactly one address"},
        {"explicit standalone multiple", Options{Mode: ModeStandalone, Addrs: []string{"a:6379", "b:6379"}}, "redis: standalone mode requires exactly one address"},
        {"sentinel master missing", Options{Mode: ModeSentinel, Addr: "a:26379"}, "redis: sentinel mode requires a master name"},
        {"cluster database", Options{Mode: ModeCluster, Addr: "a:6379", DB: 1}, "redis: cluster mode supports database 0 only"},
        {"unsupported mode", Options{Mode: "unknown", Addr: "a:6379"}, "redis: unsupported mode \"unknown\""},
    } {
        t.Run(tc.name, func(t *testing.T) {
            got, err := universalOptions(tc.opts)
            require.EqualError(t, err, tc.want)
            require.Nil(t, got)
        })
    }
}
```

- [x] Run `go test ./internal/storage/state/redis -count=1` and `go test -race ./internal/storage/state/redis -count=1`. Expected: deterministic suites pass; tests requiring TEST_REDIS_ADDR remain skipped when unset, with no live Redis operation.
- [x] Run `git diff --check` and review `git diff -- internal/storage/state/redis/store.go`. Expected: no whitespace errors, no durability/safety implementation changes, no unrelated edits.
- [x] Report exact RED/GREEN commands/results and owned touched files to the parent. Do not commit; the user's no-commit boundary overrides the skill's usual commit step.

## Plan self-review

This bounded plan covers the approved spec's client credential forwarding and compatibility requirement only. Sentinel deployment, four real authentication links, Helm/native wiring, startup recovery, failover, and production HA acceptance are explicitly outside its completion claim. `universalOptions(Options) (*redis.UniversalOptions, error)` is the same signature throughout; old Mode/address/DB validations and all existing UniversalOptions fields are preserved.

## Execution evidence

- Initial RED: `go test ./internal/storage/state/redis -run '^TestUniversalOptions' -count=1` exited 1 with missing `universalOptions` and unknown `SentinelUsername`/`SentinelPassword` fields.
- Behavioral RED: after extracting the existing builder and adding the fields, but before forwarding them, the same command exited 1. `password_only` expected `sentinel password\\\"` but got empty; `username_only` and `both` expected `sentinel-user` but got empty. Other builder compatibility cases passed. This additional step verifies the forwarding assertions independently of the initial compile failure.
- Focused GREEN: the same command passed after adding the two explicit UniversalOptions mappings, `ok github.com/goairix/sandbox/internal/storage/state/redis 0.540s`.
- Package GREEN: `TEST_REDIS_ADDR= go test ./internal/storage/state/redis -count=1` passed, `ok github.com/goairix/sandbox/internal/storage/state/redis 0.356s`.
- Race GREEN: `TEST_REDIS_ADDR= go test -race ./internal/storage/state/redis -count=1` passed, `ok github.com/goairix/sandbox/internal/storage/state/redis 1.643s`.
- `git diff --check` exited 0; production diff leaves ping cleanup and all durability/ACK/safety implementations unchanged. Explicitly empty TEST_REDIS_ADDR prevents live integration tests from running.
- Integration choice: keep the existing branch and checkout as-is for the parent task; no commits, pushes, branch/worktree operations, or release/live-cluster changes.
