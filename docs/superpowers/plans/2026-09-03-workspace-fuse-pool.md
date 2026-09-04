# Workspace FUSE Pool Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Replace remote workspace tar synchronization with an s3fs mount that is bound to a sandbox-api-maintained prepared Pod/container only when `workspace_path` becomes known.

**Architecture:** `sandbox-api` owns a dedicated FUSE Pool with the same WarmUp/Acquire/refill/single-use lifecycle as the existing Pool. Kubernetes prepares a locked native sidecar Pod and Docker prepares a locked special container; Acquire reserves one Redis-backed record, obtains the workspace lease, consumes one mount authorization, mounts only the requested prefix, verifies it from UID 1000, and only then publishes the sandbox session. Redis coordinates API replicas but never runs the pool control loop.

**Tech Stack:** Go 1.25, Gin, Viper, go-redis v9, Docker Engine API v27, Kubernetes client-go v0.35, s3fs, OpenTelemetry, Helm, Docker Compose, testify.

**Design:** [`docs/superpowers/specs/2026-09-01-workspace-container-fuse-mount-design.md`](../specs/2026-09-01-workspace-container-fuse-mount-design.md)

---

## File Structure

| File | Action | Responsibility |
|---|---|---|
| `internal/config/config.go` | Modify | FUSE provider, resource, timeout, Pool, credential-file and CA configuration |
| `internal/config/config_test.go` | Modify | Defaults, YAML loading and fail-closed validation |
| `configs/config.yaml` | Modify | Checked-in sync default and documented FUSE keys |
| `internal/storage/prefix.go` | Create | Canonical `sub_path` / `workspace_path` validation and one prefix builder |
| `internal/storage/prefix_test.go` | Create | Byte-identity, Unicode and traversal tests |
| `internal/storage/workspace_marker.go` | Create | Native MinIO/OBS FUSE control client, custom CA, empty-prefix marker preparation and filtering |
| `internal/storage/workspace_marker_test.go` | Create | Native-client construction and marker create/verify/filter tests |
| `internal/storage/filesystem.go` | Modify | File credentials, CA transport and storage identity metadata |
| `internal/storage/filesystem_test.go` | Modify | Credential-source and metadata tests |
| `internal/storage/state/state.go` | Modify | Compare-and-swap/delete primitives required by leases |
| `internal/storage/state/pool.go` | Create | Runtime-neutral FUSE Pool record/repository contract |
| `internal/storage/state/redis/store.go` | Modify | Atomic CAS/delete implementation |
| `internal/storage/state/redis/pool.go` | Create | Redis Lua-backed reserve/refill-lock/record operations |
| `internal/storage/state/redis/pool_test.go` | Create | Multi-client reservation and state transition integration tests |
| `internal/runtime/types.go` | Modify | FUSE spec, authorization and health types |
| `internal/runtime/runtime.go` | Modify | Prepare/authorize/ready/health/quiesce/flush methods and sized upload |
| `internal/sandbox/workspace_lease.go` | Create | TTL lease, persistent owner and one-shot `mount_attempt` CAS |
| `internal/sandbox/workspace_lease_test.go` | Create | Conflict, renewal, takeover and generation tests |
| `internal/sandbox/fuse_pool.go` | Create | sandbox-api-owned FUSE Pool lifecycle and Redis reconciliation |
| `internal/sandbox/fuse_pool_test.go` | Create | WarmUp, hit/miss, refill, max size, return and crash-state tests |
| `internal/sandbox/operation_gate.go` | Create | Closeable per-sandbox operation gate, reference draining and lifecycle watcher |
| `internal/sandbox/operation_gate_test.go` | Create | Concurrent stream, close, lease-loss and health-loss tests |
| `internal/sandbox/types.go` | Modify | FUSE workspace state persisted with a delivered sandbox |
| `internal/sandbox/manager.go` | Modify | Acquire transaction, publication gate, restore and destroy orchestration |
| `internal/sandbox/manager_test.go` | Modify | Pool hit identity, fail-closed cleanup and API gate tests |
| `internal/sandbox/workspace.go` | Modify | FUSE sync/flush and public mount/unmount semantics |
| `internal/sandbox/workspace_test.go` | Modify | FUSE 409/no-copy/flush tests |
| `internal/runtime/kubernetes/pod.go` | Modify | Prepared native sidecar Pod renderer and mount propagation |
| `internal/runtime/kubernetes/pod_test.go` | Modify | Sidecar, Secret, probe, security and immutable selector assertions |
| `internal/runtime/kubernetes/control.go` | Create | Private mounter control exec and UID 1000 mount probe |
| `internal/runtime/kubernetes/control_test.go` | Create | Prepared/authorize/ready/restart gate tests |
| `internal/runtime/kubernetes/network.go` | Modify | Pre-Pod system egress and Acquire-time user policy |
| `internal/runtime/kubernetes/network_test.go` | Create | Immutable selector and exact allow-list tests |
| `internal/runtime/kubernetes/runtime.go` | Modify | Implement three-phase FUSE runtime lifecycle |
| `internal/runtime/kubernetes/runtime_test.go` | Create | Ordering and cleanup tests with fake clients |
| `cmd/workspace-mounter/main.go` | Create | Trusted PID 1 supervisor and health/control subcommands |
| `cmd/workspace-probe/main.go` | Create | Fixed UID-1000 workspace probe and process quiesce helper inside sandbox containers |
| `internal/mounter/supervisor.go` | Create | Locked-to-authorized state machine and single s3fs child |
| `internal/mounter/supervisor_test.go` | Create | Authorization replay, child exit and restart marker tests |
| `internal/mounter/mountinfo.go` | Create | Mount type and open-writer checks without shell parsing |
| `internal/mounter/mountinfo_test.go` | Create | Proc fixture tests |
| `docker/images/sandbox-fuse/Dockerfile` | Create | Special Docker runtime image contract |
| `docker/images/workspace-mounter/Dockerfile` | Create | Kubernetes mounter image contract |
| `internal/runtime/docker/container.go` | Modify | FUSE device/capability/cache/Secret special container |
| `internal/runtime/docker/container_test.go` | Modify | HostConfig least-privilege assertions |
| `internal/runtime/docker/control.go` | Create | Root-only control exec and UID 1000 probe |
| `internal/runtime/docker/control_test.go` | Create | User/control boundary tests |
| `internal/runtime/docker/network.go` | Modify | System egress gateway at WarmUp and user rules at Acquire |
| `internal/runtime/docker/runtime.go` | Modify | Implement three-phase FUSE runtime lifecycle and cleanup |
| `internal/runtime/docker/runtime_test.go` | Create | Same-container bind and failure cleanup tests |
| `internal/api/handler/file.go` | Modify | Propagate multipart size into streaming runtime upload |
| `internal/api/handler/file_test.go` | Modify | Size propagation and oversize rejection tests |
| `internal/api/router.go` | Modify | Route-aware streaming upload limit without weakening other body limits |
| `internal/api/router_test.go` | Modify | Default 64 MiB limit and configurable streaming upload exception tests |
| `internal/api/handler/workspace.go` | Modify | Map FUSE hot mount/unmount conflicts to HTTP 409 |
| `pkg/types/workspace.go` | Modify | Backward-compatible mount/flush response fields |
| `internal/telemetry/metrics/metrics.go` | Modify | FUSE Pool, lease, mount, cache and flush metrics |
| `cmd/sandbox/main.go` | Modify | Construct Redis repositories and start the FUSE Pool inside Manager |
| `deploy/helm/sandbox/*` | Modify/Create | Values, RBAC, runtime namespace baseline and preflight job assets |
| `docker/docker-compose.yml` | Modify | Secret files, FUSE image and `/dev/fuse` deployment contract |
| `scripts/workspace-fuse-preflight.sh` | Create | Kubernetes/Docker provider profile and lifecycle preflight |
| `testdata/values-fuse-minio.yaml` | Create | Deterministic Helm FUSE render fixture |
| `testdata/fuse/profiles/*.yaml` | Create | Versioned provider compatibility evidence |
| `test/integration/workspacefuse/workspace_fuse_test.go` | Create | Runtime/provider functional and fault matrix |
| `docs/deployment/workspace-fuse.md` | Modify | Replace target-only wording with implemented commands and verified versions |

## Delivery Gates

Implementation may merge behind `workspace.mode=sync`, but FUSE traffic remains disabled until all gates pass:

1. MinIO, Huawei public OBS and the 2023 private OBS each have a versioned profile report and pinned image digest.
2. Kubernetes and Docker pass prepared → Acquire → mount → UID 1000 write probe → destroy for every enabled profile.
3. Redis/API crash tests prove one runtime UID cannot consume two mount authorizations.
4. Runtime namespace default-deny and exact system egress are installed before any prepared Pod.
5. Kubernetes graceful termination or infrastructure fencing and Docker exact-container NotFound evidence pass; force-delete alone never permits takeover.
6. Named AppArmor/SELinux profiles, continuous lease/health gating, and the 1 GiB streaming upload test pass for both runtimes.

### Task 1: Add Fail-Closed FUSE Configuration

**Files:**
- Modify: `internal/config/config.go`
- Modify: `internal/config/config_test.go`
- Modify: `configs/config.yaml`

- [ ] **Step 1: Write failing defaults and validation tests**

Add this helper and the table-driven cases to `internal/config/config_test.go`:

```go
func minimalValidConfig() *config.Config {
    return &config.Config{
        Server:  config.ServerConfig{Port: 8080},
        Runtime: config.RuntimeConfig{Type: "docker"},
        Pool:    config.PoolConfig{MinSize: 0, MaxSize: 1},
        Security: config.SecurityConfig{
            APIKey: "test", ExecTimeoutSeconds: 30, MaxExecTimeoutSeconds: 60,
            MaxUploadBytes: 2 << 30,
        },
        Workspace: config.WorkspaceConfig{Mode: "sync"},
    }
}

func TestFUSEConfigValidation(t *testing.T) {
    newValid := func() *config.Config {
        valid := minimalValidConfig()
        valid.Workspace.Mode = "fuse"
        valid.Workspace.QuotaMode = "soft"
        valid.Workspace.CacheSize = "2Gi"
        valid.Workspace.CacheMedium = "disk"
        valid.Workspace.MountTimeoutSeconds = 30
        valid.Workspace.FlushTimeoutSeconds = 30
        valid.Workspace.UnmountTimeoutSeconds = 15
        valid.Workspace.RecreateMaxAttempts = 1
        valid.Workspace.LeaseTTLSeconds = 120
        valid.Workspace.LeaseRenewIntervalSeconds = 30
        valid.Workspace.FUSEPool = config.WorkspaceFUSEPoolConfig{MinSize: 3, MaxSize: 20, RefillIntervalSeconds: 10, PrepareTimeoutSeconds: 120}
        valid.Storage.FileSystem.Provider = "minio"
        valid.Storage.FileSystem.Bucket = "sandbox"
        valid.Storage.FileSystem.Endpoint = "minio.example.com:9000"
        valid.Storage.FileSystem.CredentialFiles = config.FileSystemCredentialFileConfig{
            AccessKeyFile: "/run/secrets/storage_access_key",
            SecretKeyFile: "/run/secrets/storage_secret_key",
        }
        valid.Workspace.SecretName = "sandbox-workspace-minio"
        valid.Workspace.Providers = map[string]config.WorkspaceFUSEProviderConfig{
            "minio": {
                Driver: "s3fs", Profile: "minio-sigv4-path-style-v1",
                StorageIdentity: "minio-primary", CredentialGeneration: "2026-09-03-01",
                MounterImage: "registry.example.com/mounter@sha256:" + strings.Repeat("a", 64),
                DockerImage: "registry.example.com/sandbox-fuse@sha256:" + strings.Repeat("b", 64),
                LSMProfile: "sandbox-fuse",
                SystemEgressMode: "cidr",
                DNSCIDRs: []string{"8.8.8.8/32", "1.1.1.1/32"},
                SystemEgressCIDRs: []string{"192.0.2.10/32"},
                SystemEgressFQDNs: []string{"minio.example.com"},
                EndpointPorts: []int32{9000},
                ProxyURL: "",
            },
        }
        return valid
    }

    tests := []struct {
        name string
        edit func(*config.Config)
        want string
    }{
        {"unsupported provider", func(c *config.Config) { c.Storage.FileSystem.Provider = "s3" }, "fuse supports only minio or obs"},
        {"missing identity", func(c *config.Config) { p := c.Workspace.Providers["minio"]; p.StorageIdentity = ""; c.Workspace.Providers["minio"] = p }, "storage_identity"},
        {"mutable image tag", func(c *config.Config) { p := c.Workspace.Providers["minio"]; p.MounterImage = "mounter:latest"; c.Workspace.Providers["minio"] = p }, "sha256 digest"},
        {"bad pool bounds", func(c *config.Config) { c.Workspace.FUSEPool.MaxSize = 2 }, "fuse_pool.max_size"},
        {"bad lease cadence", func(c *config.Config) { c.Workspace.LeaseTTLSeconds = 60; c.Workspace.LeaseRenewIntervalSeconds = 30 }, "lease_renew_interval_seconds"},
        {"noncanonical sub path", func(c *config.Config) { c.Storage.FileSystem.SubPath = "workspaces/" }, "canonical relative prefix"},
    }
    for _, tt := range tests {
        t.Run(tt.name, func(t *testing.T) {
            got := newValid()
            tt.edit(got)
            require.ErrorContains(t, got.Validate(), tt.want)
        })
    }
}
```

- [ ] **Step 2: Run the focused test and confirm it fails**

Run: `go test ./internal/config -run TestFUSEConfigValidation -v`

Expected: FAIL because the FUSE configuration types and validation do not exist.

- [ ] **Step 3: Add the exact configuration types and conditional validation**

Add the types from design section 9 to `internal/config/config.go`, including `FileSystemCredentialFileConfig`, `WorkspaceFUSEProviderConfig` (with required non-`unconfined` `LSMProfile`, `SystemEgressMode`, `DNSCIDRs`, endpoint CIDR/FQDN/ports and explicit `ProxyURL`), `WorkspaceFUSEResourceConfig` and `WorkspaceFUSEPoolConfig`. Extend `WorkspaceConfig` with:

```go
type WorkspaceConfig struct {
    AutoSyncIntervalSeconds   int                                    `mapstructure:"auto_sync_interval_seconds"`
    Mode                      string                                 `mapstructure:"mode"`
    SecretName                string                                 `mapstructure:"secret_name"`
    CacheSize                 string                                 `mapstructure:"cache_size"`
    CacheMedium               string                                 `mapstructure:"cache_medium"`
    MountTimeoutSeconds       int                                    `mapstructure:"mount_timeout_seconds"`
    FlushTimeoutSeconds       int                                    `mapstructure:"flush_timeout_seconds"`
    UnmountTimeoutSeconds     int                                    `mapstructure:"unmount_timeout_seconds"`
    RecreateMaxAttempts       int                                    `mapstructure:"recreate_max_attempts"`
    LeaseTTLSeconds           int                                    `mapstructure:"lease_ttl_seconds"`
    LeaseRenewIntervalSeconds int                                    `mapstructure:"lease_renew_interval_seconds"`
    QuotaMode                 string                                 `mapstructure:"quota_mode"`
    MounterResources          WorkspaceFUSEResourceConfig            `mapstructure:"mounter_resources"`
    FUSEPool                  WorkspaceFUSEPoolConfig                `mapstructure:"fuse_pool"`
    Providers                 map[string]WorkspaceFUSEProviderConfig `mapstructure:"providers"`
}
```

Keep `workspace.mode=sync` as the default and reject unknown workspace modes. Add positive `security.max_upload_bytes` with a 2 GiB default while preserving the default 64 MiB limit for non-upload request bodies. Run FUSE-only checks only when `Mode == "fuse"`. The exact FUSE bounds are: `0 <= min_size <= max_size`, `max_size > 0`, positive refill/prepare/mount/flush/unmount/lease timeouts, `0 < lease_renew_interval <= lease_ttl/3`, `recreate_max_attempts == 1`, `cache_medium == disk`, positive parsed `cache_size`, and `quota_mode == soft`. Require Redis address, Secret name, bucket, endpoint, non-empty control-plane AK/SK credential file paths with no simultaneous inline credentials, selected provider/profile, storage identity, credential generation, digest-pinned images, non-unconfined LSM profile, `SystemEgressMode`, host-only DNS CIDRs (`/32` or `/128`), exact positive endpoint ports and non-empty approved endpoint CIDR/FQDN values compatible with the selected mode. The same DNS CIDRs, stripped to IPs, populate Pod `dnsConfig.nameservers` and system egress policy—there is no second DNS source. Reject session-token/expiry fields (model them explicitly so Viper cannot silently ignore them), mutable image tags, providers other than MinIO/OBS, non-canonical `sub_path`, wildcard FQDNs and any non-empty proxy URL.

- [ ] **Step 4: Run configuration tests**

Run: `go test ./internal/config -v`

Expected: PASS, including legacy sync configurations.

- [ ] **Step 5: Commit**

```bash
git add internal/config/config.go internal/config/config_test.go configs/config.yaml
git commit -m "feat: add workspace fuse configuration"
```

### Task 2: Build One Canonical Workspace Prefix

**Files:**
- Create: `internal/storage/prefix.go`
- Create: `internal/storage/prefix_test.go`
- Modify: `internal/sandbox/manager.go`

- [ ] **Step 1: Write the failing prefix tests**

```go
func TestBuildWorkspacePrefix(t *testing.T) {
    tests := []struct{ sub, workspace, want string }{
        {"", "team/项目", "team/项目/"},
        {"workspaces", "team/project", "workspaces/team/project/"},
    }
    for _, tt := range tests {
        got, err := storage.BuildWorkspacePrefix(tt.sub, tt.workspace)
        require.NoError(t, err)
        assert.Equal(t, []byte(tt.want), []byte(got))
    }
}

func TestBuildWorkspacePrefixRejectsNonCanonicalInput(t *testing.T) {
    for _, p := range []string{"/root", "root/", "a//b", "a/../b", ".", ".sandbox-system", ".sandbox-system/owner", "a\x00b", "a\nb"} {
        _, err := storage.BuildWorkspacePrefix("workspaces", p)
        require.Error(t, err, p)
    }
}
```

- [ ] **Step 2: Run and confirm failure**

Run: `go test ./internal/storage -run BuildWorkspacePrefix -v`

Expected: FAIL with `undefined: storage.BuildWorkspacePrefix`.

- [ ] **Step 3: Implement byte-preserving validation and construction**

```go
func BuildWorkspacePrefix(subPath, workspacePath string) (string, error) {
    if err := validateCanonicalRelativePrefix(subPath, true); err != nil {
        return "", fmt.Errorf("sub_path: %w", err)
    }
    if err := validateCanonicalRelativePrefix(workspacePath, false); err != nil {
        return "", fmt.Errorf("workspace_path: %w", err)
    }
    if subPath == "" {
        return workspacePath + "/", nil
    }
    return subPath + "/" + workspacePath + "/", nil
}

func validateCanonicalRelativePrefix(value string, allowEmpty bool) error {
    if value == "" && allowEmpty { return nil }
    if value == "" || !utf8.ValidString(value) || strings.HasPrefix(value, "/") || strings.HasSuffix(value, "/") || strings.Contains(value, "//") {
        return ErrInvalidWorkspacePrefix
    }
    for _, r := range value {
        if r == 0 || unicode.IsControl(r) { return ErrInvalidWorkspacePrefix }
    }
    for _, part := range strings.Split(value, "/") {
        if part == "." || part == ".." || part == "" { return ErrInvalidWorkspacePrefix }
    }
    if strings.Split(value, "/")[0] == ".sandbox-system" { return ErrInvalidWorkspacePrefix }
    return nil
}
```

Replace FUSE-mode path joining in Manager with this function; do not change legacy sync path behavior.

- [ ] **Step 4: Run storage and sandbox tests**

Run: `go test ./internal/storage ./internal/sandbox`

Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/storage/prefix.go internal/storage/prefix_test.go internal/sandbox/manager.go
git commit -m "feat: canonicalize workspace object prefixes"
```

### Task 3: Add Credential Files, CA Wiring and Empty-Prefix Markers

**Files:**
- Modify: `internal/storage/filesystem.go`
- Modify: `internal/storage/filesystem_test.go`
- Create: `internal/storage/workspace_marker.go`
- Create: `internal/storage/workspace_marker_test.go`

- [ ] **Step 1: Add failing credential and marker tests**

```go
type fakeWorkspaceObjects struct { data map[string][]byte; options map[string]RootMarkerOptions }

func newFakeWorkspaceObjects() *fakeWorkspaceObjects {
    return &fakeWorkspaceObjects{data: map[string][]byte{}, options: map[string]RootMarkerOptions{}}
}
func (f *fakeWorkspaceObjects) PutEmptyObject(_ context.Context, key string, options RootMarkerOptions) error {
    f.data[key] = []byte{}
    f.options[key] = options
    return nil
}
func (f *fakeWorkspaceObjects) HeadObject(_ context.Context, key string) (bool, error) {
    _, ok := f.data[key]
    return ok, nil
}

func TestLoadFileSystemCredentialsRejectsMixedSources(t *testing.T) {
    cfg := config.FileSystemConfig{
        AccessKey: "inline",
        CredentialFiles: config.FileSystemCredentialFileConfig{AccessKeyFile: "/run/secrets/access"},
    }
    _, err := LoadFileSystemCredentials(cfg)
    require.ErrorContains(t, err, "mutually exclusive")
}

func TestPrepareWorkspacePrefixWritesExactRootMarker(t *testing.T) {
    objects := newFakeWorkspaceObjects()
    err := PrepareWorkspacePrefix(context.Background(), objects, "workspaces/team-a/", RootMarkerProfile{Style: RootMarkerTrailingSlash})
    require.NoError(t, err)
    assert.Equal(t, []byte{}, objects.data["workspaces/team-a/"])
    assert.False(t, IsUserVisibleWorkspaceObject("workspaces/team-a/", "workspaces/team-a/"))
}
```

- [ ] **Step 2: Run and confirm failure**

Run: `go test ./internal/storage -run 'Credential|PrepareWorkspacePrefix' -v`

Expected: FAIL because the credential loader and marker API are missing.

- [ ] **Step 3: Implement the credential and marker seams**

Implement `LoadFileSystemCredentials` with `os.ReadFile`, trim only one trailing newline from Docker/Kubernetes Secret files, require mode not group/world-readable when the platform exposes file mode, and return owned byte slices that callers zero after client construction. Extend `FileSystemMeta`:

```go
type FileSystemMeta struct {
    Provider        StorageProvider
    LocalPath       string
    Bucket          string
    Region          string
    Endpoint        string
    SubPath         string
    UseSSL          bool
    StorageIdentity string
}

type WorkspaceObjectClient interface {
    PutEmptyObject(ctx context.Context, key string, options RootMarkerOptions) error
    HeadObject(ctx context.Context, key string) (bool, error)
}

type RootMarkerOptions struct {
    ContentType string
    Metadata    map[string]string
}
```

Add `NewWorkspaceObjectClient(cfg, credentials)` for FUSE control-plane work. Its MinIO and Huawei OBS adapters must use the providers' native SDKs, clone a dedicated HTTP transport, append `ca_file` to system roots, and keep hostname verification enabled. Do not route this constructor through `goairix/fs` v0.3.11 because its MinIO/OBS configs do not expose a custom transport. Keep `NewFileSystem` and the `goairix/fs` drivers for legacy sync mode only.

`PrepareWorkspacePrefix` must derive immutable `RootMarkerOptions` from the selected `RootMarkerProfile`, put the exact canonical prefix marker and verify it with `HeadObject`; the native adapters must implement that operation rather than calling `fs.MakeDir`. Tests must assert the exact content type/metadata for MinIO, public OBS and private-2023 OBS, cover a TLS server signed by a temporary CA (failure without the CA, success with it), provider selection, and rejection of a CA file containing no certificates. The constructor receives owned credential byte slices; the caller zeroes them immediately after SDK construction. Never set an insecure TLS flag.

- [ ] **Step 4: Run storage tests**

Run: `go test ./internal/storage -v`

Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/storage/filesystem.go internal/storage/filesystem_test.go internal/storage/workspace_marker.go internal/storage/workspace_marker_test.go
git commit -m "feat: prepare fuse workspace prefixes securely"
```

### Task 4: Add Atomic Redis State and the FUSE Pool Repository

**Files:**
- Modify: `internal/storage/state/state.go`
- Create: `internal/storage/state/pool.go`
- Modify: `internal/storage/state/redis/store.go`
- Modify: `internal/storage/state/redis/store_test.go`
- Create: `internal/storage/state/redis/pool.go`
- Create: `internal/storage/state/redis/pool_test.go`

- [ ] **Step 1: Write failing Redis integration tests**

Use the existing `TEST_REDIS_ADDR` gate and two `Store` instances:

```go
func TestReservePreparedIsAtomicAcrossClients(t *testing.T) {
    skipIfNoRedis(t)
    a, b := testStore(t), testStore(t)
    repoA, repoB := NewFUSEPoolRepository(a), NewFUSEPoolRepository(b)
    record := state.FUSEPoolRecord{RuntimeUID: "uid-1", RuntimeID: "pod-1", PoolKey: "key-1", State: state.FUSEPoolPreparing, Revision: 1}
    require.NoError(t, repoA.CreatePreparing(context.Background(), record))
    _, err := repoA.Transition(context.Background(), "uid-1", state.FUSEPoolPreparing, state.FUSEPoolPrepared, "", record.Revision)
    require.NoError(t, err)

    var wins atomic.Int32
    var wg sync.WaitGroup
    for _, repo := range []*FUSEPoolRepository{repoA, repoB} {
        wg.Add(1)
        go func(repo *FUSEPoolRepository) {
            defer wg.Done()
            if got, _ := repo.ReservePrepared(context.Background(), "key-1", uuid.NewString(), time.Minute); got != nil { wins.Add(1) }
        }(repo)
    }
    wg.Wait()
    assert.Equal(t, int32(1), wins.Load())
}
```

Also cover CAS mismatch, compare-and-delete, refill lock ownership, expired reservation remaining non-prepared, and `CountPreparingAndPrepared` across two clients.

- [ ] **Step 2: Run against Redis and confirm failure**

Run: `TEST_REDIS_ADDR=127.0.0.1:6379 go test ./internal/storage/state/redis -run 'Atomic|FUSEPool' -v`

Expected: FAIL because the atomic and pool APIs do not exist. If Redis is unavailable, start the repository Compose Redis first; do not accept a skipped result for this task.

- [ ] **Step 3: Implement contracts and Lua transitions**

Add to `state.go`:

```go
type AtomicStore interface {
    Store
    CompareAndSwap(ctx context.Context, key string, oldValue, newValue []byte, ttl time.Duration) (bool, error)
    CompareAndDelete(ctx context.Context, key string, expected []byte) (bool, error)
    Increment(ctx context.Context, key string) (int64, error)
}
```

Define in `state/pool.go`:

```go
type FUSEPoolState string
const (
    FUSEPoolPreparing FUSEPoolState = "preparing"
    FUSEPoolPrepared  FUSEPoolState = "prepared"
    FUSEPoolReserved  FUSEPoolState = "reserved"
    FUSEPoolBinding   FUSEPoolState = "binding"
    FUSEPoolConsumed  FUSEPoolState = "consumed"
    FUSEPoolCleanup   FUSEPoolState = "cleanup"
)

type FUSEPoolRecord struct {
    PreparationID   string        `json:"preparation_id"`
    RuntimeID       string        `json:"runtime_id"`
    RuntimeUID      string        `json:"runtime_uid"`
    PoolKey         string        `json:"pool_key"`
    State           FUSEPoolState `json:"state"`
    MaintainerToken string        `json:"maintainer_token"`
    ReservationToken string      `json:"reservation_token,omitempty"`
    ReservedUntil   time.Time     `json:"reserved_until,omitempty"`
    PrepareUntil    time.Time     `json:"prepare_until,omitempty"`
    CleanupToken    string        `json:"cleanup_token,omitempty"`
    CleanupUntil    time.Time     `json:"cleanup_until,omitempty"`
    UpdatedAt       time.Time     `json:"updated_at"`
    Revision        uint64        `json:"revision"`
}

type FUSEPoolRepository interface {
	CreatePreparingWithAdmission(ctx context.Context, record FUSEPoolRecord, refillToken string, maxSize int, prepareTTL time.Duration) error
	BindPreparingRuntime(ctx context.Context, preparationID, runtimeID, runtimeUID, refillToken string, expectedRevision uint64) (*FUSEPoolRecord, error)
    ReservePrepared(ctx context.Context, poolKey, token string, ttl time.Duration) (*FUSEPoolRecord, error)
	Transition(ctx context.Context, preparationID string, from, to FUSEPoolState, token string, expectedRevision uint64) (*FUSEPoolRecord, error)
	TransitionWithRefillLock(ctx context.Context, preparationID string, from, to FUSEPoolState, token, refillToken string, expectedRevision uint64, reservationTTL time.Duration) (*FUSEPoolRecord, error)
	ClaimCleanup(ctx context.Context, preparationID string, from FUSEPoolState, maintainerToken, reservationToken string, expectedRevision uint64, runtimeID, runtimeUID, cleanupToken string, ttl time.Duration) (*FUSEPoolRecord, error)
    ListByPoolKey(ctx context.Context, poolKey string) ([]FUSEPoolRecord, error)
    CountPreparingAndPrepared(ctx context.Context, poolKey string) (int, error)
	ConditionalDelete(ctx context.Context, preparationID string, expectedState FUSEPoolState, maintainerToken, reservationToken string, expectedRevision uint64) (bool, error)
	DeleteCleanup(ctx context.Context, preparationID, cleanupToken string, expectedRevision uint64) (bool, error)
	ServerTime(ctx context.Context) (time.Time, error)
    TryRefillLock(ctx context.Context, poolKey, token string, ttl time.Duration) (bool, error)
	RenewRefillLock(ctx context.Context, poolKey, token string, ttl time.Duration) (bool, error)
    UnlockRefill(ctx context.Context, poolKey, token string) error
}
```

`NewFUSEPoolRepository(store *Store)` is the only concrete repository constructor; `Store` itself is not asserted to implement this domain interface and its Redis client remains private. `PreparationID` is an opaque, bounded, valid-UTF-8 UUID generated before runtime allocation and Redis keys contain only its digest. Implement admission, bind, reserve, revision-aware transitions, cleanup claim/delete, inventory and refill-lock operations with Lua so state/token/revision checks and mutations are atomic. Admission validates the held refill token and `preparing + prepared < max_size` in the same Lua; bind makes RuntimeUID immutable and globally unique; publish validates the same lock token. `PrepareUntil`, `ReservedUntil` and `CleanupUntil` come from Redis `TIME`, not API wall clocks. Refill locks are token-renewed and every fenced write still rejects a lost token. Cleanup claims are idempotent for the same token, reject another token before expiry, and permit stale takeover after expiry. Runtime deletion occurs only after a successful exact cleanup claim; a failed removal keeps the cleanup tombstone retryable. Every successful non-idempotent mutation increments `Revision`; stale reconcilers leave the newer record and runtime untouched. Never auto-transition an expired reservation to prepared.

For a Pool miss, the requesting API first registers an unbound `preparing` intent, using `PreparationID` as runtime `spec.ID`, then prepares and atomically binds RuntimeID/RuntimeUID. After health it starts the reservation TTL from Redis `TIME` while transitioning that exact revision directly to `reserved`; it must never publish an intermediate `prepared` record. WarmUp/refill records follow the same intent/bind protocol and transition `preparing → prepared`. Bound deletion uses the FUSE-only exact `(RuntimeID, RuntimeUID)` contract; unbound recovery removes only the non-reusable `PreparationID/spec.ID`. Add tests for blocked allocation visibility, UID uniqueness, capacity admission, lock fencing, Redis clock skew, stale cleanup takeover and a two-client cold prepare.

- [ ] **Step 4: Run state tests**

Run: `TEST_REDIS_ADDR=127.0.0.1:6379 go test ./internal/storage/state/redis -v`

Expected: PASS with no skipped FUSE Pool cases.

- [ ] **Step 5: Commit**

```bash
git add internal/storage/state/state.go internal/storage/state/pool.go internal/storage/state/redis/store.go internal/storage/state/redis/store_test.go internal/storage/state/redis/pool.go internal/storage/state/redis/pool_test.go
git commit -m "feat: add atomic fuse pool state"
```

### Task 5: Freeze the Runtime FUSE Contract

**Files:**
- Modify: `internal/runtime/types.go`
- Modify: `internal/runtime/runtime.go`
- Modify: `internal/sandbox/pool_test.go`

- [ ] **Step 1: Add a compile-time fake covering the new contract**

Extend `mockRuntime` in `internal/sandbox/pool_test.go` and retain:

```go
var _ runtime.Runtime = (*mockRuntime)(nil)

func (m *mockRuntime) PrepareSandbox(ctx context.Context, spec runtime.SandboxSpec) (*runtime.SandboxInfo, error) {
    return m.CreateSandbox(ctx, spec)
}
func (m *mockRuntime) AuthorizeWorkspaceMount(context.Context, string, runtime.WorkspaceMountAuthorization) error { return nil }
func (m *mockRuntime) WaitSandboxReady(context.Context, string) (*runtime.SandboxInfo, error) { return &runtime.SandboxInfo{State: "running"}, nil }
func (m *mockRuntime) PreparedSandboxHealth(context.Context, string, string) error { return nil }
func (m *mockRuntime) WorkspaceHealth(context.Context, string) (*runtime.WorkspaceHealth, error) { return &runtime.WorkspaceHealth{Ready: true}, nil }
func (m *mockRuntime) QuiesceWorkspace(context.Context, string) (runtime.WorkspaceQuiesceToken, error) { return runtime.WorkspaceQuiesceToken{}, nil }
func (m *mockRuntime) ResumeWorkspace(context.Context, string, runtime.WorkspaceQuiesceToken) error { return nil }
func (m *mockRuntime) FlushWorkspace(context.Context, string) error { return nil }
```

- [ ] **Step 2: Run and confirm the interface test fails**

Run: `go test ./internal/sandbox -run TestPool_Acquire -v`

Expected: FAIL until the new runtime types and method signatures exist.

- [ ] **Step 3: Add the exact common types and methods**

Add `WorkspaceFUSESpec` and `WorkspaceMountAuthorization` as specified in design section 10.1. Define the previously implicit network contract explicitly:

```go
type SystemEgressMode string
const (
    SystemEgressCIDR       SystemEgressMode = "cidr"
    SystemEgressCiliumFQDN SystemEgressMode = "cilium-fqdn"
)

type SystemEgressSpec struct {
    Mode          SystemEgressMode
    DNSCIDRs      []string
    DNSPorts      []int32 // phase 1: exactly TCP/UDP 53
    EndpointCIDRs []string
    EndpointFQDNs []string
    EndpointPorts []int32 // exact configured object-store ports only
    ProxyURL      string  // phase 1 must be empty; proxy egress is unsupported
}

type TerminationEvidence struct {
    RuntimeUID       string
    GracefulUnmount  bool
    ProcessExited    bool
    InfrastructureFenced bool
}

type RuntimeFencer interface {
    ConfirmTerminated(ctx context.Context, runtimeID, runtimeUID string) (TerminationEvidence, error)
}

type OrphanReconciler interface {
    ReconcileOrphanedResources(ctx context.Context, protectedRuntimeUIDs map[string]struct{}) error
}
```

Standard Kubernetes `NetworkPolicy` accepts only `SystemEgressCIDR` and requires resolved endpoint CIDRs. `CiliumNetworkPolicy` accepts `SystemEgressCiliumFQDN` and exact FQDNs. Both reject empty DNS/endpoint destinations, non-positive ports, wildcards, private destinations not present in the approved system whitelist, and any non-empty `ProxyURL`. Add `RuntimeUID` to `SandboxInfo` and optional `WorkspaceFUSE *WorkspaceFUSESpec` to `SandboxSpec`. Add:

```go
type WorkspaceHealth struct {
    Ready          bool
    MountType      string
    Generation     int64
    LastSuccessful time.Time
    Error          string
}

type WorkspaceQuiesceToken struct {
    RuntimeUID string
    Generation int64
    Opaque     string
}
```

Extend `Runtime` with the FUSE methods above, including `QuiesceWorkspace(ctx, id) (WorkspaceQuiesceToken, error)` and `ResumeWorkspace(ctx, id, token) error`, and change upload to:

```go
UploadFile(ctx context.Context, id, destPath string, size int64, reader io.Reader) error
```

Update all runtime implementations and mocks with explicit `ErrWorkspaceFUSEUnsupported` stubs first; later tasks replace Kubernetes and Docker stubs. Quiesce tokens are bound to exact RuntimeUID/generation, single-use for resume, and never persisted; stale, replayed or cross-runtime tokens fail closed.

`RemoveSandbox` for FUSE is not successful until its runtime-specific `RuntimeFencer` produces matching termination evidence. Kubernetes may use successful supervisor unmount followed by graceful Pod UID deletion; if the node/control path is unavailable it requires an injected infrastructure fencer and otherwise returns a blocked error. Force-deleting only the API object is never evidence. Docker requires a reachable daemon and `ContainerInspect` NotFound for the exact immutable container ID.

`OrphanReconciler` is an optional post-restore capability. Runtime constructors must not delete managed resources. Manager calls it only after Redis pool/session/owner reconciliation has produced the protected runtime UID set; implementations may delete only resources not in that set.

- [ ] **Step 4: Run all compile tests**

Run: `go test ./...`

Expected: PASS; behavior is unchanged because `workspace.mode` still defaults to sync.

- [ ] **Step 5: Commit**

```bash
git add internal/runtime/types.go internal/runtime/runtime.go internal/sandbox/pool_test.go internal/runtime internal/sandbox internal/api/handler/file.go
git commit -m "refactor: add workspace fuse runtime contract"
```

### Task 6: Implement Workspace Lease and One-Shot Authorization State

**Files:**
- Create: `internal/sandbox/workspace_lease.go`
- Create: `internal/sandbox/workspace_lease_test.go`
- Modify: `internal/sandbox/session.go`

- [ ] **Step 1: Write failing coordinator tests with an atomic fake store**

```go
func TestWorkspaceCoordinatorConsumesMountAttemptOnce(t *testing.T) {
    store := newAtomicMemoryStore()
    c := NewWorkspaceCoordinator(store, 90*time.Second, 30*time.Second)
    lease, err := c.Acquire(context.Background(), WorkspaceLeaseRequest{
        Provider: "minio", StorageIdentity: "minio-primary", Bucket: "sandbox",
        Prefix: "workspaces/a/", SandboxID: "sandbox-a", RuntimeUID: "uid-a",
    })
    require.NoError(t, err)

    auth, err := c.ConsumeMountAttempt(context.Background(), lease, "pool-key")
    require.NoError(t, err)
    assert.Equal(t, uint8(1), auth.MountAttempt)
    _, err = c.ConsumeMountAttempt(context.Background(), lease, "pool-key")
    require.ErrorIs(t, err, ErrMountAuthorizationConsumed)
}

func TestWorkspaceCoordinatorDoesNotTakeOverLiveExpiredOwner(t *testing.T) {
    store := newAtomicMemoryStore()
    c := NewWorkspaceCoordinator(store, time.Second, 300*time.Millisecond)
    store.putOwner(WorkspaceOwner{Prefix: "workspaces/a/", RuntimeUID: "live-uid", Generation: 7})
    _, err := c.Acquire(context.Background(), WorkspaceLeaseRequest{Prefix: "workspaces/a/", RuntimeUID: "new-uid"})
    require.ErrorIs(t, err, ErrWorkspaceOwned)
}
```

The fake must implement `state.AtomicStore` under a mutex and expose `putOwner` only to this test file.

- [ ] **Step 2: Run and confirm failure**

Run: `go test ./internal/sandbox -run WorkspaceCoordinator -v`

Expected: FAIL because `WorkspaceCoordinator` is undefined.

- [ ] **Step 3: Implement lease and owner records**

Use keys derived only from `BuildWorkspacePrefix` output and a SHA-256 of `storage_identity`:

```go
type WorkspaceOwner struct {
    SandboxID   string    `json:"sandbox_id"`
    RuntimeUID  string    `json:"runtime_uid"`
    Generation  int64     `json:"generation"`
    MountAttempt uint8    `json:"mount_attempt"`
    UpdatedAt   time.Time `json:"updated_at"`
}

type WorkspaceLease struct {
    Key        string
    Value      []byte
    Prefix     string
    Owner      WorkspaceOwner
    ExpiresAt  time.Time
}
```

Use disjoint Redis namespaces: `sandbox:workspace:lease:`, `sandbox:workspace:owner:`, and `sandbox:workspace:generation:`; none may match the session index. `Acquire` uses `SetNX` for the TTL lease, then obtains the next strictly monotonic generation with `AtomicStore.Increment` on the persistent generation key and creates or verifies the non-TTL owner. Removing an owner never removes its generation counter. `BindRuntime(ctx, lease, runtimeUID)` CASes the initially empty owner runtime identity. `ConsumeMountAttempt` performs JSON CAS from zero to one and returns `runtime.WorkspaceMountAuthorization`. `Renew` compares the lease token before extending; `Release` compare-and-deletes the lease and removes the owner only after the caller has confirmed runtime exit.

The Manager starts renewal immediately after `Acquire` succeeds—before marker creation, binding or mount—and stops it only after publication teardown or failed-Acquire cleanup. `StartRenewal` accepts an `onLost(error)` callback. Lease loss atomically closes the sandbox operation gate (if publication already occurred), rejects new public operations, and schedules single-use runtime destruction; it never waits until the next user request. Tests cover lease loss during marker preparation, during `WaitSandboxReady`, and after publication.

- [ ] **Step 4: Run lease and race tests**

Run: `go test -race ./internal/sandbox -run 'WorkspaceCoordinator|WorkspaceLease' -v`

Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/sandbox/workspace_lease.go internal/sandbox/workspace_lease_test.go internal/sandbox/session.go
git commit -m "feat: enforce exclusive fuse workspace leases"
```

### Task 7: Implement the sandbox-api-Owned FUSE Pool

**Files:**
- Create: `internal/sandbox/fuse_pool.go`
- Create: `internal/sandbox/fuse_pool_test.go`
- Modify: `internal/sandbox/pool.go`
- Modify: `internal/sandbox/manager.go`
- Modify: `cmd/sandbox/main.go`

- [ ] **Step 1: Write failing lifecycle tests**

Create `memoryFUSEPoolRepository` in the test file with `mu sync.Mutex` and `records map[string]state.FUSEPoolRecord`; implement every `state.FUSEPoolRepository` method from Task 4 as a single locked state check/mutation. Add these deterministic helpers:

```go
func fixedFUSESpec(poolKey string) runtime.SandboxSpec {
    return runtime.SandboxSpec{
        ID: "sandbox-pool-template", Image: "sandbox:latest", RunAsUser: 1000,
        WorkspaceFUSE: &runtime.WorkspaceFUSESpec{Provider: "minio", Driver: "s3fs", PoolKey: poolKey},
    }
}

func newFUSEMockRuntime() *mockRuntime {
    return newMockRuntime()
}
```

Extend `mockRuntime` so `PrepareSandbox` records a unique `RuntimeUID`, `PreparedSandboxHealth` can be failed by test configuration, and `RemoveSandbox` records removed runtime IDs under its existing mutex. Then add:

```go
func TestFUSEPoolWarmAcquireAndRefill(t *testing.T) {
    rt := newFUSEMockRuntime()
    repo := newMemoryFUSEPoolRepository()
    pool := NewFUSEPool(rt, repo, FUSEPoolConfig{
        MinSize: 2, MaxSize: 3, RefillInterval: 10 * time.Millisecond,
        PrepareTimeout: time.Second, MaintainerToken: "api-a",
    }, fixedFUSESpec("pool-key"))

    require.NoError(t, pool.WarmUp(context.Background()))
    require.Equal(t, 2, repo.countState("pool-key", state.FUSEPoolPrepared))
    got, err := pool.Acquire(context.Background(), "pool-key")
    require.NoError(t, err)
    assert.Equal(t, state.FUSEPoolReserved, got.State)
    require.Eventually(t, func() bool { return repo.countState("pool-key", state.FUSEPoolPrepared) == 2 }, time.Second, 10*time.Millisecond)
    assert.LessOrEqual(t, repo.countPreparingAndPrepared("pool-key"), 3)
}

func TestFUSEPoolNeverReturnsAuthorizedInstance(t *testing.T) {
    rt := newFUSEMockRuntime()
    repo := newMemoryFUSEPoolRepository()
    pool := NewFUSEPool(rt, repo, FUSEPoolConfig{MinSize: 1, MaxSize: 2, PrepareTimeout: time.Second, MaintainerToken: "api-a"}, fixedFUSESpec("pool-key"))
    require.NoError(t, pool.WarmUp(context.Background()))
    record, err := pool.Acquire(context.Background(), "pool-key")
    require.NoError(t, err)
    binding, err := repo.Transition(context.Background(), record.RuntimeUID, state.FUSEPoolReserved, state.FUSEPoolBinding, record.ReservationToken, record.Revision)
    require.NoError(t, err)
    pool.ReleaseConsumed(context.Background(), *binding)
    require.Eventually(t, func() bool { return rt.wasRemoved(record.RuntimeID) }, time.Second, 10*time.Millisecond)
    _, exists := repo.record(record.RuntimeUID)
    assert.False(t, exists)
}

func TestFUSEPoolKeyExcludesOnlyAcquireFields(t *testing.T) {
    a := fixedFUSESpec("pool-key")
    b := a
    keyA, err := ComputeFUSEPoolKey(a)
    require.NoError(t, err)
    keyB, err := ComputeFUSEPoolKey(b)
    require.NoError(t, err)
    assert.Equal(t, keyA, keyB)
    b.Image = "sandbox:v2"
    keyB, err = ComputeFUSEPoolKey(b)
    require.NoError(t, err)
    assert.NotEqual(t, keyA, keyB)
}
```

Add `record(preparationID) (state.FUSEPoolRecord, bool)` to the fake repository and exact runtime removal evidence to `mockRuntime`; both take their existing mutex. Add cases for pristine return, stale health discard, Pool miss cold prepare, two concurrent refillers, expired preparing/reserved cleanup, protected/unknown binding retention, stale cleanup recovery, and Stop draining only `MaintainerToken == "api-a"`. The fake repository must perform each method as one mutex-held state check/mutation and model lock TTL/renewal plus Redis server time.

- [ ] **Step 2: Run and confirm failure**

Run: `go test ./internal/sandbox -run FUSEPool -v`

Expected: FAIL because `FUSEPool` does not exist.

- [ ] **Step 3: Implement the control loop inside sandbox-api**

```go
type FUSEPool struct {
    runtime runtime.Runtime
    repo    state.FUSEPoolRepository
    config  FUSEPoolConfig
    spec    runtime.SandboxSpec
    stopCh  chan struct{}
    wg      sync.WaitGroup
}

type FUSEPoolConfig struct {
    MinSize         int
    MaxSize         int
    RefillInterval  time.Duration
    PrepareTimeout  time.Duration
    ReservationTTL  time.Duration
    MaintainerToken string
}

func (p *FUSEPool) Start(ctx context.Context) error {
    if err := p.Reconcile(ctx); err != nil { return err }
    p.wg.Add(1)
    go p.reconcileLoop()
    return nil
}

func (p *FUSEPool) Acquire(ctx context.Context, poolKey string) (*state.FUSEPoolRecord, error) {
    token := uuid.NewString()
    record, err := p.repo.ReservePrepared(ctx, poolKey, token, p.config.ReservationTTL)
    if err != nil { return nil, err }
    if record == nil {
        record, err = p.prepareOneForReservation(ctx, token)
        if err == nil {
            record, err = p.repo.Transition(ctx, record.RuntimeUID, state.FUSEPoolPreparing, state.FUSEPoolReserved, token, record.Revision)
        }
    }
    go p.refillIfNeeded(context.Background())
    return record, err
}
```

`ComputeFUSEPoolKey` canonical-JSON serializes a projection of runtime type, sandbox image/resources/security, provider, storage identity, bucket, endpoint, region, profile, mounter digest, Secret name, credential generation, CA, cache and system egress, then returns a SHA-256 hex digest. The projection excludes the output `PoolKey` field itself and has no workspace path, prefix, workspace identity, lease generation, reservation token or request-level user network fields.

`Reconcile` runs only after token-checked refill lock acquisition and keeps that lock renewed; renewal loss cancels the round and fenced Lua prevents later writes. It obtains Redis server time, recovers expired preparation/cleanup claims, and atomically inspection-reserves every pre-existing prepared record. A required Manager guard returns `Pristine`, `Protected`, or `Abandoned`: only `Pristine` plus runtime health may return to prepared; `Protected` or an uncertain/error result stays unavailable and makes initial startup fail closed; only `Abandoned` authorizes cleanup. It counts only `preparing + prepared`, then creates until `prepared >= MinSize` without exceeding `MaxSize`. `Acquire` applies the same guard after reservation and triggers refill even when an unproven record remains isolated. `ReturnPrepared` requires `Pristine` plus health before revision-CAS. Every destructive path first claims `cleanup`, then uses exact FUSE runtime removal, then conditionally deletes the tombstone; stale CAS never removes a runtime. `ReleaseConsumed` follows the same protocol and keeps failed deletions retryable. `Start` callers share the first real reconciliation result, wait while the initial lock is busy, and Stop cancels/tracks initial reconciliation and in-flight Acquire/WarmUp/Reconcile operations. Do not change the existing non-FUSE Pool behavior.

Change `Manager.Start(ctx)` to return `error`. Sync mode preserves the existing best-effort general Pool behavior, but FUSE mode must synchronously connect/reconcile state and complete `fusePool.Start`; any Redis, reconciliation or WarmUp error is returned to `cmd/sandbox` and the HTTP server must not start listening. Add tests for each fail-closed startup error.

- [ ] **Step 4: Run Pool tests with the race detector**

Run: `go test -race ./internal/sandbox -run 'Pool|FUSEPool' -v`

Expected: PASS with stable counts and no race report.

- [ ] **Step 5: Commit**

```bash
git add internal/sandbox/fuse_pool.go internal/sandbox/fuse_pool_test.go internal/sandbox/pool.go internal/sandbox/manager.go cmd/sandbox/main.go
git commit -m "feat: maintain fuse pool in sandbox api"
```

### Task 8: Gate Sandbox Publication Around the Acquire Transaction

**Files:**
- Modify: `internal/sandbox/types.go`
- Modify: `internal/sandbox/manager.go`
- Modify: `internal/sandbox/manager_test.go`
- Modify: `internal/sandbox/session.go`
- Create: `internal/sandbox/operation_gate.go`
- Create: `internal/sandbox/operation_gate_test.go`

- [ ] **Step 1: Write failing Manager transaction tests**

Add `newFUSETestManager(t, rt) (*Manager, string)` in `manager_test.go`. It must create the locked in-memory Pool repository and atomic lease store from Tasks 6-7, inject a fake MinIO marker client, call `mgr.fusePool.WarmUp`, and return the first prepared runtime ID. Add `newBlockingFUSEMockRuntime` by embedding `mockRuntime` and blocking `WaitSandboxReady` on `allowReady` after closing `waitReadyEntered`.

```go
func TestManagerCreateFUSEUsesPreparedRuntimeAndPublishesAfterProbe(t *testing.T) {
    rt := newFUSEMockRuntime()
    mgr, preparedID := newFUSETestManager(t, rt)

    sb, err := mgr.Create(context.Background(), SandboxConfig{Mode: ModePersistent, WorkspacePath: "team/a"})
    require.NoError(t, err)
    assert.Equal(t, preparedID, sb.RuntimeID)
    assert.Equal(t, WorkspaceMountFUSE, sb.Workspace.MountType)
    assert.True(t, rt.authorizedBeforeReady)
    assert.True(t, rt.userProbePassed)
}

func TestManagerCreateFUSEIsInvisibleUntilReady(t *testing.T) {
    rt := newBlockingFUSEMockRuntime()
    mgr, _ := newFUSETestManager(t, rt)
    done := make(chan error, 1)
    go func() { _, err := mgr.Create(context.Background(), SandboxConfig{WorkspacePath: "team/a"}); done <- err }()
    <-rt.waitReadyEntered
    assert.Empty(t, mgr.listInMemorySandboxes())
    close(rt.allowReady)
    require.NoError(t, <-done)
}
```

Also test lease conflict returns the pristine shell, cancellation after mount CAS destroys it, user-network failure destroys it, and session-save failure destroys it without opening the gate.

- [ ] **Step 2: Run and confirm failure**

Run: `go test ./internal/sandbox -run 'CreateFUSE|FUSEIsInvisible' -v`

Expected: FAIL because Manager still registers before remote workspace mount.

- [ ] **Step 3: Implement a single private binding function**

Add workspace state:

```go
type WorkspaceMountType string
const (
    WorkspaceMountSync  WorkspaceMountType = "sync"
    WorkspaceMountLocal WorkspaceMountType = "local"
    WorkspaceMountFUSE  WorkspaceMountType = "fuse"
)

type WorkspaceMountState string
const (
    WorkspaceMountMounting WorkspaceMountState = "mounting"
    WorkspaceMountReady    WorkspaceMountState = "ready"
    WorkspaceMountError    WorkspaceMountState = "error"
)
```

Add a closeable, reference-counted `operationGate` per delivered sandbox rather than a bare boolean. Every public Exec/stream/file/workspace operation must acquire a reference before resolving the runtime and release it only after all streaming I/O has ended; a closed gate returns `ErrSandboxNotReady`. `CloseAndWait(ctx)` rejects new operations and waits for all existing references. `BeginExclusive(ctx)` closes admission, waits for prior references, and returns a token that either reopens the same healthy generation or permanently closes it; this is the only path used by flush/quiesce. Loading a persistent session directly must never open the gate; only successful Create publication or health-checked restore can install an open gate.

Implement `createFUSESandbox` with this exact order. Every call returns an error that immediately enters the cleanup path; the snippet names the successful values to make the state sequence unambiguous:

```go
record, err := fusePool.Acquire(ctx, poolKey)
lease, err := coordinator.Acquire(ctx, leaseRequest)
err = storage.PrepareWorkspacePrefix(ctx, objectClient, prefix, markerProfile)
err = coordinator.BindRuntime(ctx, lease, record.RuntimeUID)
auth, err := coordinator.ConsumeMountAttempt(ctx, lease, poolKey)
record, err = repo.Transition(ctx, record.RuntimeUID, state.FUSEPoolReserved, state.FUSEPoolBinding, record.ReservationToken, record.Revision)
err = m.runtime.AuthorizeWorkspaceMount(ctx, record.RuntimeID, auth)
err = m.runtime.UpdateNetwork(ctx, record.RuntimeID, cfg.Network.Enabled, cfg.Network.Whitelist, cfg.Network.BlockPrivate)
_, err = m.runtime.WaitSandboxReady(ctx, record.RuntimeID) // includes the fixed UID 1000 write probe
record, err = repo.Transition(ctx, record.RuntimeUID, state.FUSEPoolBinding, state.FUSEPoolConsumed, record.ReservationToken, record.Revision)
err = m.publishSandboxAndSession(ctx, sb)
```

Inject the FUSE-only `WorkspaceObjectClient` into Manager; legacy sync mode continues to use `fs.FileSystem`. Use defers with a `bindingStarted` flag that becomes true immediately after `BindRuntime` succeeds. While it is false, return the shell only after a full pristine probe proving there is no owner reference, mount generation, session or open gate. Once it is true, including a failure before or during `ConsumeMountAttempt`, every failure deletes the runtime and releases the lease only after confirmed exit. Never use a flag set only after the sidecar authorization call: the owner binding and `mount_attempt` CAS are already non-replayable boundaries.

Implement `publishSandboxAndSession(ctx, sb)` so the persistent session save succeeds before one locked insertion into `m.sandboxes` plus its open operation gate; `resolveAvailable` still rejects a concurrently loaded session until that final insertion. On failure delete the session, leave the gate closed and destroy the runtime. Prepared runtime IDs never enter `m.sandboxes` or the session store. Tests must separately cover cancellation after owner binding and after `mount_attempt` CAS; both destroy rather than return the shell.

After publication, start one lifecycle watcher that checks `WorkspaceHealth` and the exact runtime UID/generation at a bounded interval. s3fs exit, missing mount, generation mismatch, sidecar restart marker/restartCount, or lease-renewal loss closes the operation gate exactly once and triggers single-use destruction. During the pre-publication transaction the same signals cancel the Acquire context and enter its cleanup path. Add race tests with a blocked `ExecStream` proving new calls fail immediately while teardown waits for the existing reference.

- [ ] **Step 4: Run Manager and race tests**

Run: `go test -race ./internal/sandbox -run 'CreateFUSE|WorkspaceCoordinator|FUSEPool' -v`

Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/sandbox/types.go internal/sandbox/manager.go internal/sandbox/manager_test.go internal/sandbox/session.go internal/sandbox/operation_gate.go internal/sandbox/operation_gate_test.go
git commit -m "feat: bind fuse workspace before sandbox publication"
```

### Task 9: Render a Prepared Kubernetes Native-Sidecar Pod

**Files:**
- Modify: `internal/runtime/kubernetes/pod.go`
- Modify: `internal/runtime/kubernetes/pod_test.go`

- [ ] **Step 1: Write failing Pod shape tests**

```go
func TestCreatePodRendersPreparedFUSESidecar(t *testing.T) {
    client := fake.NewSimpleClientset()
    spec := fuseSandboxSpecForTest()
    pod, err := createPod(context.Background(), client, "sandbox-runtime", spec)
    require.NoError(t, err)

    require.Len(t, pod.Spec.InitContainers, 1)
    mounter := pod.Spec.InitContainers[0]
    assert.Equal(t, "workspace-mounter", mounter.Name)
    require.NotNil(t, mounter.RestartPolicy)
    assert.Equal(t, corev1.ContainerRestartPolicyAlways, *mounter.RestartPolicy)
    assert.True(t, *mounter.SecurityContext.Privileged)
    assert.Equal(t, []string{"/usr/local/bin/workspace-mounter", "supervise"}, mounter.Command)
    assert.Equal(t, corev1.MountPropagationBidirectional, *volumeMount(mounter.VolumeMounts, "workspace").MountPropagation)
    sandbox := pod.Spec.Containers[0]
    assert.Equal(t, corev1.MountPropagationHostToContainer, *volumeMount(sandbox.VolumeMounts, "workspace").MountPropagation)
    assert.False(t, hasVolumeMount(sandbox.VolumeMounts, "workspace-credentials"))
    assert.Equal(t, "preparing", pod.Labels["sandbox.pool.state"])
}
```

Add assertions for memory-backed workspace/mounter-run, disk-backed bounded cache, `/dev/fuse` CharDevice, startup/readiness commands, Secret mode 0400, no ServiceAccount token, no shared process namespace, UID 1000 sandbox, drop ALL and immutable pool instance label. Assert a mounter-only `SANDBOX_RUNTIME_UID` Downward API fieldRef to `metadata.uid` and a `SANDBOX_MOUNTER_BOOTSTRAP` JSON value containing only the fixed `WorkspaceFUSESpec`; the sandbox container receives neither variable.

- [ ] **Step 2: Run and confirm failure**

Run: `go test ./internal/runtime/kubernetes -run PreparedFUSESidecar -v`

Expected: FAIL because `createPod` renders only the sandbox container.

- [ ] **Step 3: Add a dedicated FUSE renderer**

Keep the legacy renderer unchanged and dispatch only when `spec.WorkspaceFUSE != nil`:

```go
func buildPod(namespace string, spec runtime.SandboxSpec) (*corev1.Pod, error) {
    if spec.WorkspaceFUSE != nil {
        return buildPreparedFUSEPod(namespace, spec)
    }
    return buildLegacyPod(namespace, spec)
}
```

Build the exact Pod contract from deployment section 6.4. The sidecar receives the Pod UID from the Downward API and fixed, non-secret bootstrap JSON (provider, bucket, endpoint, profile, AK/SK source paths, generated password/cache/CA paths, PoolKey and timeouts) in its private environment; on startup the supervisor validates both, verifies the UID against authorization later, and atomically materializes a mode-0600 bootstrap file below `/run/s3fs`. Derive `dnsConfig.nameservers` from the same host-only provider `DNSCIDRs` used by `SystemEgressSpec`; reject a CIDR that cannot be reduced to one resolver IP. Set the bottom workspace anchor to mode 0555 before reporting prepared. Do not include prefix, workspace identity or lease generation in the Pod spec, environment, labels or command line.

- [ ] **Step 4: Run Kubernetes Pod tests**

Run: `go test ./internal/runtime/kubernetes -run 'Pod|FUSESidecar' -v`

Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/runtime/kubernetes/pod.go internal/runtime/kubernetes/pod_test.go
git commit -m "feat: render prepared kubernetes fuse pods"
```

### Task 10: Add Kubernetes System Egress and Private Mounter Control

**Files:**
- Modify: `internal/runtime/kubernetes/network.go`
- Create: `internal/runtime/kubernetes/network_test.go`
- Create: `internal/runtime/kubernetes/control.go`
- Create: `internal/runtime/kubernetes/control_test.go`
- Modify: `internal/runtime/kubernetes/runtime.go`
- Create: `internal/runtime/kubernetes/runtime_test.go`

- [ ] **Step 1: Write failing policy and ordering tests**

Define `newFakeKubernetesRuntime(t)` with `fake.NewSimpleClientset`, a fake dynamic client and reactors that record create/delete actions; define `firstCreates` to return only the first NetworkPolicy and Pod create verbs. Reuse `fuseSandboxSpecForTest` from `pod_test.go`, and add a fixed fake pod status reactor for mounter-started, sandbox-running and ready transitions.

```go
func TestSystemEgressUsesImmutablePoolInstanceSelector(t *testing.T) {
    policy, err := buildSystemEgressPolicy("runtime", "instance-a", runtime.SystemEgressSpec{
        Mode: runtime.SystemEgressCIDR, DNSCIDRs: []string{"1.1.1.1/32"}, DNSPorts: []int32{53},
        EndpointCIDRs: []string{"192.0.2.10/32"}, EndpointPorts: []int32{443},
    })
    require.NoError(t, err)
    assert.Equal(t, map[string]string{"sandbox.pool.instance": "instance-a"}, policy.Spec.PodSelector.MatchLabels)
    assert.NotContains(t, policy.Spec.PodSelector.MatchLabels, "sandbox.pool.state")
}

func TestPrepareSandboxCreatesPolicyBeforePod(t *testing.T) {
    rt, actions := newFakeKubernetesRuntime(t)
    _, err := rt.PrepareSandbox(context.Background(), fuseSandboxSpecForTest())
    require.NoError(t, err)
    assert.Equal(t, []string{"create-networkpolicies", "create-pods"}, actions.firstCreates())
}
```

Add tests that standard NetworkPolicy rejects FQDN mode, Cilium policy renders exact non-wildcard FQDNs, public Exec always targets `sandbox`, private authorize targets only `workspace-mounter`, state labels are patched with resourceVersion, UID 1000 probe uses fixed argv, quiesce/resume tokens reject generation mismatch/replay, and removal keeps policies/owner when termination evidence is unavailable. A normal removal test must observe successful mounter shutdown, graceful deletion of the exact Pod UID, then NotFound before deleting policies. A node-loss test must fail closed without a configured fencer and succeed only when a fake infrastructure fencer returns evidence for the same node/Pod UID. Force-delete alone must fail.

- [ ] **Step 2: Run and confirm failure**

Run: `go test ./internal/runtime/kubernetes -run 'SystemEgress|PrepareSandbox|MounterControl' -v`

Expected: FAIL because the helpers and three-phase lifecycle are absent.

- [ ] **Step 3: Implement Kubernetes lifecycle in order**

Add private fixed-container exec:

```go
func (r *Runtime) execControl(ctx context.Context, pod, container string, argv []string, stdin string) error {
    if container != "workspace-mounter" { return ErrInvalidControlContainer }
    return execArgvInPod(ctx, r.client, r.restConfig, r.namespace, pod, container, "0", argv, stdin)
}
```

`PrepareSandbox` creates system egress first, creates the Pod second, waits for the mounter startup probe and sandbox container Running without waiting for Pod Ready, reads Pod UID into `SandboxInfo.RuntimeUID`, performs `PreparedSandboxHealth`, then patches state to prepared. `AuthorizeWorkspaceMount` JSON-encodes auth to stdin of a fixed `workspace-mounter authorize` command. `WaitSandboxReady` requires Pod Ready, mounter generation equality and a fixed UID 1000 create/read/delete probe in `sandbox`. `UpdateNetwork` creates a separate user policy selected by the same immutable instance label.

For removal, first obtain a successful `workspace-mounter shutdown` acknowledgement proving flush/unmount, then issue a non-force graceful delete and watch the exact Pod UID through termination to NotFound. If control is unavailable or the node cannot confirm graceful termination, call the configured `RuntimeFencer`; without matching fencing evidence return `ErrTerminationUnconfirmed`, keep the owner and policies, and block takeover. Only confirmed termination permits policy deletion. Never translate API-object force deletion into process-exit evidence.

- [ ] **Step 4: Run Kubernetes runtime tests**

Run: `go test -race ./internal/runtime/kubernetes -v`

Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/runtime/kubernetes/network.go internal/runtime/kubernetes/network_test.go internal/runtime/kubernetes/control.go internal/runtime/kubernetes/control_test.go internal/runtime/kubernetes/runtime.go internal/runtime/kubernetes/runtime_test.go
git commit -m "feat: bind fuse workspaces in kubernetes pods"
```

### Task 11: Build the Shared Locked Mounter Supervisor

**Files:**
- Create: `internal/mounter/supervisor.go`
- Create: `internal/mounter/supervisor_test.go`
- Create: `internal/mounter/mountinfo.go`
- Create: `internal/mounter/mountinfo_test.go`
- Create: `cmd/workspace-mounter/main.go`
- Create: `cmd/workspace-probe/main.go`

- [ ] **Step 1: Write failing state-machine tests**

```go
func TestSupervisorConsumesAuthorizationOnce(t *testing.T) {
    runner := &fakeRunner{block: make(chan struct{})}
    s := newTestSupervisor(t, runner)
    auth := Authorization{RuntimeUID: "uid-a", PoolKey: "key-a", Prefix: "workspaces/a/", LeaseGeneration: 1, MountAttempt: 1}
    require.NoError(t, s.Authorize(context.Background(), auth))
    require.ErrorIs(t, s.Authorize(context.Background(), auth), ErrAuthorizationConsumed)
    assert.Equal(t, 1, runner.starts)
}

func TestSupervisorNeverRestartsExitedS3FS(t *testing.T) {
    runner := &fakeRunner{exit: errors.New("s3fs exited")}
    s := newTestSupervisor(t, runner)
    require.NoError(t, s.Authorize(context.Background(), validAuthorization()))
    require.Eventually(t, func() bool { return s.State() == StateUnhealthy }, time.Second, 10*time.Millisecond)
    assert.Equal(t, 1, runner.starts)
}

func TestSupervisorStaysLockedWhenGenerationMarkerExists(t *testing.T) {
    dir := t.TempDir()
    require.NoError(t, os.WriteFile(filepath.Join(dir, "mount-generation"), []byte("1"), 0600))
    s := NewSupervisor(Config{RunDir: dir}, &fakeRunner{})
    require.Equal(t, StateRestartDetected, s.State())
}
```

Add bootstrap tests for missing/multiline AK/SK, atomic mode-0600 `passwd-s3fs`, source/output path separation, RuntimeUID mismatch and replay. Add proc mountinfo fixtures for `fuse.s3fs`, wrong filesystem type and missing mount. Add `workspace-probe` tests for open writable file descriptors plus quiesce/resume token generation, cross-runtime rejection and replay.

- [ ] **Step 2: Run and confirm failure**

Run: `go test ./internal/mounter -v`

Expected: FAIL because the package is absent.

- [ ] **Step 3: Implement the supervisor and CLI**

Define a versioned `BootstrapConfig` with provider, bucket, endpoint, region, profile, `AccessKeyFile`, `SecretKeyFile`, `PasswdFile`, CA file, cache directory, mount path, PoolKey, mount/flush/unmount timeouts and RuntimeUID. `PasswdFile` is fixed below `/run/s3fs` (normally `/run/s3fs/passwd-s3fs`); the AK/SK sources are fixed paths in the read-only Secret mount. It contains no workspace prefix, identity or lease generation. Validate every field against the compiled profile, reject unknown JSON fields, require absolute trusted paths, read AK/SK without logging them, atomically generate `AK:SK` plus one newline at `PasswdFile` mode 0600, and atomically persist the sanitized bootstrap mode 0600 below mode-0700 `/run/s3fs` before reporting prepared. Reject missing/empty/multiline credentials and wipe temporary byte slices. Kubernetes obtains this config from its fixed bootstrap environment plus Downward API Pod UID. Docker starts locked without a RuntimeUID and accepts exactly one `bootstrap` JSON command after `ContainerCreate` returns the immutable container ID; it must reject authorization until bootstrap succeeds and reject bootstrap replay or an ID mismatch.

Use a root-only Unix socket below `/run/s3fs`; accept bootstrap/authorization JSON only through stdin. Validate `RuntimeUID`, `PoolKey`, canonical prefix, positive generation and `MountAttempt == 1`. Atomically create `mount-generation` with `O_CREATE|O_EXCL`, then start exactly one foreground s3fs child from an argv slice:

```go
argv := []string{
    bucket + ":/" + strings.TrimSuffix(auth.Prefix, "/"), mountPath,
    "-f", "-o", "allow_other", "-o", "uid=1000", "-o", "gid=1000",
    "-o", "umask=0022", "-o", "mp_umask=0022",
    "-o", "passwd_file=" + passwdFile,
    "-o", "tmpdir=" + filepath.Join(cacheDir, "tmp"),
}
argv = append(argv, profile.FixedOptions(endpoint, region)...)
```

The compiled profile registry accepts only the configured versioned profile IDs; MinIO adds verified SigV4/path-style options, while public/private OBS options come from their separately built provider images. `prepared` checks validated bootstrap, `/dev/fuse`, Secret readability by root, mode 0555 anchor and absence of marker/mount/child. `ready` checks `fuse.s3fs` and a bounded read. `flush` and `shutdown` assume the Manager has already closed/drained its operation gate and obtained a successful sandbox-side quiesce token; they perform bounded flush/unmount and never claim success after timeout. Child exit sets unhealthy without restart.

Build `workspace-probe` into every FUSE-capable sandbox image. Its fixed `write-read-delete` command runs as UID/GID 1000. Its `quiesce` command runs inside the sandbox container, stops all other same-UID descendants, verifies no writable file descriptor targets `/workspace`, and emits a bounded token used to resume them when a flush does not destroy the sandbox. Failure to enumerate, stop or verify any process is an error; the Manager must report `flushed=false` and may not call the mounter flush path.

`workspace-probe` exposes only `self-check`, `write-read-delete`, `quiesce` and `resume`; resume accepts only the opaque token emitted by the same live probe generation. Unknown commands, arbitrary paths/argv and stale tokens fail.

The CLI exposes only `supervise`, `bootstrap`, `authorize`, `health prepared`, `health ready`, `health prepared --self-check-image`, `flush` and `shutdown`; the self-check validates packaged binaries/directories without requiring `/dev/fuse`, and unknown commands return non-zero. Kubernetes lifecycle and probes invoke this exact binary and subcommand grammar—no undocumented `mounter-control` or `mounter-health` aliases exist.

- [ ] **Step 4: Run mounter tests and build**

Run: `go test -race ./internal/mounter && go build ./cmd/workspace-mounter ./cmd/workspace-probe`

Expected: PASS and the command builds.

- [ ] **Step 5: Commit**

```bash
git add internal/mounter cmd/workspace-mounter cmd/workspace-probe
git commit -m "feat: add one-shot s3fs supervisor"
```

### Task 12: Build Pinned Kubernetes and Docker FUSE Images

**Files:**
- Create: `docker/images/workspace-mounter/Dockerfile`
- Create: `docker/images/sandbox-fuse/Dockerfile`
- Modify: `docker/images/sandbox/Dockerfile`
- Create: `docker/images/workspace-mounter/profiles/minio-sigv4-path-style-v1.json`
- Create: `docker/images/workspace-mounter/profiles/huawei-obs-public-v1.json`
- Create: `docker/images/workspace-mounter/profiles/huawei-obs-private-2023-v1.json`
- Create: `scripts/verify-fuse-image.sh`

- [ ] **Step 1: Add an image contract test script**

```bash
#!/usr/bin/env bash
set -euo pipefail
: "${FUSE_IMAGE:?set FUSE_IMAGE to a digest-pinned image}"
: "${SANDBOX_IMAGE:?set SANDBOX_IMAGE to the digest-pinned sandbox image}"
case "$FUSE_IMAGE" in *@sha256:*) ;; *) echo "image must use a digest" >&2; exit 1;; esac
case "$SANDBOX_IMAGE" in *@sha256:*) ;; *) echo "sandbox image must use a digest" >&2; exit 1;; esac
docker run --rm --entrypoint /usr/local/bin/workspace-mounter "$FUSE_IMAGE" health prepared --self-check-image
docker run --rm --entrypoint /usr/local/bin/workspace-probe "$SANDBOX_IMAGE" self-check
docker run --rm --entrypoint /bin/sh "$FUSE_IMAGE" -ceu '
  test -x /usr/bin/s3fs
  test -f /etc/fuse.conf
  test "$(stat -c %a /workspace)" = 555
  test "$(stat -c %u:%g /workspace)" = 0:0
'
```

- [ ] **Step 2: Run it against the existing sandbox image and confirm failure**

Run: `FUSE_IMAGE="$(docker image inspect sandbox:latest --format '{{index .RepoDigests 0}}')" scripts/verify-fuse-image.sh`

Expected: FAIL because the existing image has no mounter binary or s3fs contract.

- [ ] **Step 3: Add provider-specific image builds**

Both new Dockerfiles must use CI-supplied immutable base references and s3fs packages/artifacts whose SHA-256 is verified during build. The Kubernetes mounter image contains only s3fs, CA roots, profile JSON and `workspace-mounter`; the Docker special image extends the normal sandbox runtime with the same trusted binary/profile and `workspace-probe` but retains language tooling. The ordinary Kubernetes sandbox image also receives only the non-privileged `workspace-probe`; it never receives s3fs, credentials or `SYS_ADMIN`. None of the images contains credentials. Use:

```dockerfile
ARG BASE_IMAGE
FROM ${BASE_IMAGE}
ARG S3FS_PACKAGE_URL
ARG S3FS_PACKAGE_SHA256
ADD ${S3FS_PACKAGE_URL} /tmp/s3fs-package
RUN echo "${S3FS_PACKAGE_SHA256}  /tmp/s3fs-package" | sha256sum -c - \
 && install -m 0755 /tmp/s3fs-package /usr/bin/s3fs \
 && rm -f /tmp/s3fs-package \
 && install -d -m 0555 -o root -g root /workspace \
 && install -d -m 0700 -o root -g root /run/s3fs /var/cache/s3fs/tmp
COPY workspace-mounter /usr/local/bin/workspace-mounter
ENTRYPOINT ["/usr/local/bin/workspace-mounter", "supervise"]
```

CI supplies real package URLs, hashes and base digests from the provider spike; builds without them fail. Keep public and 2023 private OBS in separate jobs/digests even if options match.

- [ ] **Step 4: Build, scan and verify every profile image**

Run for each CI-produced digest:

```bash
FUSE_IMAGE="$IMAGE_DIGEST" scripts/verify-fuse-image.sh
trivy image --exit-code 1 --severity CRITICAL --ignore-unfixed "$IMAGE_DIGEST"
syft "$IMAGE_DIGEST" -o cyclonedx-json > "$ARTIFACT_DIR/${PROFILE}-${RUNTIME}.sbom.cdx.json"
cosign attest --predicate "$ARTIFACT_DIR/${PROFILE}-${RUNTIME}.sbom.cdx.json" --type cyclonedx "$IMAGE_DIGEST"
```

Expected: PASS for MinIO, Huawei public OBS and Huawei private 2023 OBS Kubernetes and Docker images; image scanning reports no critical fixable findings under the repository release policy, and every released digest has an archived CycloneDX SBOM plus signed attestation.

- [ ] **Step 5: Commit**

```bash
git add docker/images/workspace-mounter docker/images/sandbox-fuse docker/images/sandbox/Dockerfile scripts/verify-fuse-image.sh
git commit -m "build: add pinned workspace fuse images"
```

### Task 13: Implement the Docker Prepared Container Lifecycle

**Files:**
- Modify: `internal/runtime/docker/container.go`
- Modify: `internal/runtime/docker/container_test.go`
- Create: `internal/runtime/docker/control.go`
- Create: `internal/runtime/docker/control_test.go`
- Modify: `internal/runtime/docker/network.go`
- Modify: `internal/runtime/docker/runtime.go`
- Create: `internal/runtime/docker/runtime_test.go`

- [ ] **Step 1: Write failing HostConfig and lifecycle tests**

Refactor `Runtime.cli` behind a package-private `dockerAPI` interface containing only Docker methods used by this package. Implement `fakeDockerAPI` in `runtime_test.go` with mutex-protected created containers, exec users/stdin and cleanup events; `newFakeDockerRuntime(t)` injects that fake with deterministic isolated/open network IDs. Define `fuseDockerSpecForTest` with a digest-like test image, fixed PoolKey and system egress, and define `validRuntimeAuthorization` with the supplied RuntimeUID and mount attempt 1.

```go
func TestCreateContainerConfigForFUSE(t *testing.T) {
    spec := fuseDockerSpecForTest()
    cfg, host, err := createContainerConfig(spec)
    require.NoError(t, err)
    assert.Equal(t, spec.Image, cfg.Image)
    assert.Contains(t, host.CapAdd, "SYS_ADMIN")
    assert.NotContains(t, host.CapAdd, "DAC_OVERRIDE")
    require.Len(t, host.Devices, 1)
    assert.Equal(t, "/dev/fuse", host.Devices[0].PathOnHost)
    assert.True(t, host.ReadonlyRootfs)
    assert.Contains(t, host.SecurityOpt, "no-new-privileges=true")
    assert.Contains(t, host.SecurityOpt, "apparmor=sandbox-fuse")
    assert.NotContains(t, host.SecurityOpt, "unconfined")
    assert.NotContains(t, strings.Join(host.Binds, " "), ":/workspace")
}

func TestDockerPoolHitAuthorizesSameContainer(t *testing.T) {
    rt, fake := newFakeDockerRuntime(t)
    info, err := rt.PrepareSandbox(context.Background(), fuseDockerSpecForTest())
    require.NoError(t, err)
    require.NoError(t, rt.AuthorizeWorkspaceMount(context.Background(), info.RuntimeID, validRuntimeAuthorization(info.RuntimeUID)))
    ready, err := rt.WaitSandboxReady(context.Background(), info.RuntimeID)
    require.NoError(t, err)
    assert.Equal(t, info.RuntimeID, ready.RuntimeID)
    assert.Equal(t, "1000:1000", fake.lastProbeUser)
}
```

Add tests for root-only secret bind, bounded cache volume, prepared health, one-shot bootstrap after container ID allocation, fixed root control argv, UID 1000 public exec/file paths, quiesce/resume token generation and replay rejection, gateway system egress before container start, Acquire-time user rule append, exact AppArmor/SELinux profile, restart adoption, and cleanup after failure. Assert `New` performs no orphan cleanup and `ReconcileOrphanedResources` preserves every runtime UID supplied by Manager.

- [ ] **Step 2: Run and confirm failure**

Run: `go test ./internal/runtime/docker -run 'FUSE|PoolHit' -v`

Expected: FAIL because Docker has no FUSE special path.

- [ ] **Step 3: Implement the Docker path without weakening legacy containers**

Dispatch `createContainerConfig` on `spec.WorkspaceFUSE != nil`. For FUSE only, use the provider's `DockerImage`, `/dev/fuse`, `SYS_ADMIN`, drop ALL, read-only rootfs, cache volume and a root-only Secret directory computed with `filepath.Join(secretRoot, spec.ID)`. Apply the configured minimal AppArmor or SELinux profile and reject empty/`unconfined` profiles. The container entrypoint remains the trusted supervisor; all public `Exec`, `ExecStream`, `ExecPipe` and file helpers set `User: "1000:1000"`. Only `execControl` may set root and it accepts a closed enum of fixed commands.

At `PrepareSandbox`, create the gateway pair with system egress only, create/start the locked container, take the returned immutable container ID as RuntimeUID, then send exactly one bootstrap JSON carrying that ID before checking prepared. Authorization passes separate JSON through stdin. Ready requires supervisor health plus the UID 1000 fixed write probe. Removal requests supervisor shutdown and removes the exact container; it deletes gateway, cache volume and Secret directory only after a reachable daemon reports `ContainerInspect` NotFound. A timeout/host loss returns `ErrTerminationUnconfirmed` and retains owner/resources rather than claiming force removal succeeded.

Remove constructor-time `cleanupOrphanedResources`; Docker containers are stateful across sandbox-api process restarts, so `IsStateful()` returns true. Implement `ReconcileOrphanedResources` and invoke it only after Manager has restored sessions/owners/pool records. It may remove a gateway/network/container only when its immutable runtime UID is absent from the protected set.

- [ ] **Step 4: Run Docker tests**

Run: `go test -race ./internal/runtime/docker -v`

Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/runtime/docker/container.go internal/runtime/docker/container_test.go internal/runtime/docker/control.go internal/runtime/docker/control_test.go internal/runtime/docker/network.go internal/runtime/docker/runtime.go internal/runtime/docker/runtime_test.go
git commit -m "feat: bind fuse workspaces in docker containers"
```

### Task 14: Implement FUSE Workspace API, Flush and Streaming Upload Semantics

**Files:**
- Modify: `internal/sandbox/workspace.go`
- Modify: `internal/sandbox/workspace_test.go`
- Modify: `internal/api/handler/workspace.go`
- Modify: `internal/api/handler/file.go`
- Modify: `internal/api/handler/file_test.go`
- Modify: `internal/api/router.go`
- Modify: `internal/api/router_test.go`
- Modify: `pkg/types/workspace.go`
- Modify: `internal/runtime/docker/file.go`
- Modify: `internal/runtime/kubernetes/file.go`

- [ ] **Step 1: Write failing behavior tests**

Add `readyFUSEManager(t) (*Manager, *Sandbox)` and `readyFUSEManagerWithRuntime(t) (*Manager, *Sandbox, *mockRuntime)` using the in-memory Pool/lease fakes from Tasks 6-8. The helpers create through `Manager.Create`; they must not insert test sandboxes directly. Add `fileRouterWithRuntime` and `multipartBody` beside the existing handler router helper so the test records the sized upload call.

```go
func TestFUSEWorkspacePublicMountAndUnmountConflict(t *testing.T) {
    mgr, sb := readyFUSEManager(t)
    require.ErrorIs(t, mgr.MountWorkspace(context.Background(), sb.ID, "other", nil), ErrFUSEWorkspaceImmutable)
    require.ErrorIs(t, mgr.UnmountWorkspace(context.Background(), sb.ID), ErrFUSEWorkspaceImmutable)
}

func TestFUSESyncDirections(t *testing.T) {
    mgr, sb, rt := readyFUSEManagerWithRuntime(t)
    require.NoError(t, mgr.SyncWorkspace(context.Background(), sb.ID, "to_container", nil))
    assert.Zero(t, rt.copyCalls)
    require.NoError(t, mgr.SyncWorkspace(context.Background(), sb.ID, "from_container", nil))
    assert.Equal(t, 1, rt.quiesceCalls)
    assert.Equal(t, 1, rt.flushCalls)
    assert.Equal(t, 1, rt.resumeCalls)
}

func TestUploadFilePropagatesMultipartSize(t *testing.T) {
    router, rt := fileRouterWithRuntime(t)
    body, contentType := multipartBody(t, "file", "a.txt", []byte("hello"))
    req := httptest.NewRequest(http.MethodPost, "/sandboxes/sandbox-a/files/upload?path=/workspace/a.txt", body)
    req.Header.Set("Content-Type", contentType)
    router.ServeHTTP(httptest.NewRecorder(), req)
    assert.Equal(t, int64(5), rt.uploadSize)
}

func TestStreamingUploadCanExceedDefaultBodyLimit(t *testing.T) {
    // Stream a generated payload larger than 64 MiB without materializing it.
    // The upload route accepts it up to configured MaxUploadBytes; an ordinary
    // JSON route with the same body still returns 413.
}
```

- [ ] **Step 2: Run and confirm failure**

Run: `go test ./internal/sandbox ./internal/api/handler -run 'FUSEWorkspace|FUSESync|UploadFilePropagates' -v`

Expected: FAIL because FUSE is still routed through sync and upload has no size.

- [ ] **Step 3: Implement mount-type branching and bounded upload**

For `WorkspaceMountFUSE`, public mount/unmount return `ErrFUSEWorkspaceImmutable`; `to_container` returns a typed no-op without copy. `from_container` obtains the operation gate's exclusive token, waits for all API streams, calls `QuiesceWorkspace` to execute the fixed sandbox-side probe and receive a RuntimeUID/generation-bound `WorkspaceQuiesceToken`, calls `FlushWorkspace`, then calls `ResumeWorkspace` with that token, rechecks the same runtime generation and only then reopens admission. It reports `flushed=true` only after every step succeeds. If enumeration/open-writer checks fail it returns `flushed=false` and never calls the mounter flush path. Stale/replayed tokens fail and close the gate. Auto-sync skips FUSE. Add backward-compatible response fields:

```go
type WorkspaceInfoResponse struct {
    Mounted       bool       `json:"mounted"`
    RootPath      string     `json:"root_path,omitempty"`
    MountedAt     time.Time  `json:"mounted_at,omitempty"`
    LastSyncedAt  time.Time  `json:"last_synced_at,omitempty"`
    MountType     string     `json:"mount_type,omitempty"`
    MountState    string     `json:"mount_state,omitempty"`
    Flushed       bool       `json:"flushed,omitempty"`
    LastFlushedAt *time.Time `json:"last_flushed_at,omitempty"`
}
```

Map `ErrFUSEWorkspaceImmutable` and `ErrWorkspaceOwned` to HTTP 409. Replace the unconditional global 64 MiB body middleware with a route-aware limiter: all existing routes remain capped at 64 MiB, while only the exact direct file-upload route uses `security.max_upload_bytes`. Large uploads require an explicit `X-Sandbox-File-Size`; parse multipart with `Request.MultipartReader` and stream the file part directly rather than `FormFile`/`ParseMultipartForm`. Validate declared size against the configured limit and pass it to Manager/Runtime. Docker/Kubernetes write tar headers before streaming and use `io.CopyN`; reject negative, oversized, short and excess bodies without `io.ReadAll`. Tests must stream a generated 1 GiB body through the router, assert non-upload 64 MiB rejection, and sample process memory to prove it does not grow linearly.

- [ ] **Step 4: Run API, sandbox and runtime file tests**

Run: `go test ./internal/api ./internal/api/handler ./internal/sandbox ./internal/runtime/docker ./internal/runtime/kubernetes`

Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/sandbox/workspace.go internal/sandbox/workspace_test.go internal/api/router.go internal/api/router_test.go internal/api/handler/workspace.go internal/api/handler/file.go internal/api/handler/file_test.go pkg/types/workspace.go internal/runtime/docker/file.go internal/runtime/kubernetes/file.go
git commit -m "feat: add fuse workspace api semantics"
```

### Task 15: Recover, Quiesce and Destroy FUSE Sandboxes Safely

**Files:**
- Modify: `internal/sandbox/manager.go`
- Modify: `internal/sandbox/manager_test.go`
- Modify: `internal/sandbox/session.go`
- Modify: `internal/runtime/kubernetes/runtime.go`
- Modify: `internal/runtime/docker/runtime.go`

- [ ] **Step 1: Write failing crash and destroy tests**

Add `restoreFixture` using the atomic memory store plus a `mockRuntime` that records authorize/health calls. Add `readyBlockingRemoveFixture` whose `RemoveSandbox` closes `removeEntered` and waits on `allowRemove`. Both helpers create records through the production coordinator/repository APIs so the tests exercise real key encoding and CAS rules.

```go
func TestRestoreNeverReauthorizesExistingRuntimeUID(t *testing.T) {
    mgr, rt, sessions := restoreFixture(t, WorkspaceOwner{RuntimeUID: "uid-a", Generation: 4, MountAttempt: 1})
    sessions.saveFUSESandbox("sandbox-a", "runtime-a", "uid-a", 4)
    mgr.restorePersistentSandboxes(context.Background())
    assert.Zero(t, rt.authorizeCalls)
    assert.Equal(t, 1, rt.healthCalls)
}

func TestDestroyReleasesLeaseOnlyAfterRuntimeExit(t *testing.T) {
    mgr, rt, store := readyBlockingRemoveFixture(t)
    done := make(chan error, 1)
    go func() { done <- mgr.Destroy(context.Background(), "sandbox-a") }()
    <-rt.removeEntered
    assert.True(t, store.leaseExists())
    close(rt.allowRemove)
    require.NoError(t, <-done)
    assert.False(t, store.leaseExists())
}
```

Add recovery cases for prepared adoption after pristine probe, expired preparing/reserved conditional deletion, every binding record deletion, consumed/session reconciliation, sidecar restart marker, Pod force-delete without fencing, Docker API restart with live containers/gateways, and Docker host loss. Add SessionStore tests proving pool/lease/owner keys are never parsed as sessions.

- [ ] **Step 2: Run and confirm failure**

Run: `go test ./internal/sandbox -run 'RestoreNeverReauthorizes|DestroyReleasesLease|FUSERecovery' -v`

Expected: FAIL until restore/destroy is mount-state aware.

- [ ] **Step 3: Implement state-aware recovery and teardown**

Migrate session keys to `sandbox:session:v2:<id>` and list only that prefix. `Load` may read a legacy `sandbox:<id>` key only when the decoded JSON is a valid Sandbox, then writes v2 and compare-deletes the exact legacy bytes; pool/lease/owner namespaces are never scanned or decoded as sessions.

Restore delivered FUSE sandboxes only when session, owner, runtime UID and generation agree and `WorkspaceHealth` is ready; only then install an open operation gate and lifecycle watcher, and never call authorize for `mount_attempt=1`. A direct SessionStore load does not open the gate. Reconcile Pool records before WarmUp: reuse prepared only after pristine probe, conditionally delete timed-out preparing/reserved and all binding/unknown records using the inspected revision, and let consumed be owned by its persisted session. Build the protected runtime UID set from valid sessions, owners and pool records, then invoke the optional `OrphanReconciler`; this is the first point at which Docker gateways/networks may be cleaned.

Destroy permanently closes the operation gate and waits for all stream references, stops lease renewal, runs the sandbox-side quiesce probe, performs bounded best-effort flush only when quiesce succeeded, removes the Pod/container and requires matching termination/fencing evidence, then compare-and-deletes lease/owner and conditionally deletes the Pool record before refill. If exit cannot be confirmed, retain owner, record and policies/resources and mark blocked. A failed flush is reported as not durable but never upgraded to `flushed=true`.

- [ ] **Step 4: Run sandbox recovery tests with race detection**

Run: `go test -race ./internal/sandbox -run 'Restore|Destroy|FUSERecovery' -v`

Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/sandbox/manager.go internal/sandbox/manager_test.go internal/sandbox/session.go internal/runtime/kubernetes/runtime.go internal/runtime/docker/runtime.go
git commit -m "feat: recover and destroy fuse sandboxes safely"
```

### Task 16: Add FUSE Observability

**Files:**
- Modify: `internal/telemetry/metrics/metrics.go`
- Modify: `deploy/grafana/dashboards/sandbox-overview.json`
- Modify: `internal/sandbox/fuse_pool.go`
- Modify: `internal/sandbox/workspace_lease.go`
- Modify: `internal/sandbox/manager.go`

- [ ] **Step 1: Add a metric registration test**

```go
func TestFUSEMetricsInitialize(t *testing.T) {
    metrics.ResetForTest()
    require.NoError(t, metrics.InitNoop())
    require.NotNil(t, metrics.SandboxWorkspacePoolSize)
    require.NotNil(t, metrics.SandboxWorkspacePoolAcquire)
    require.NotNil(t, metrics.SandboxWorkspaceMountDuration)
    require.NotNil(t, metrics.SandboxWorkspaceLeaseLost)
}
```

- [ ] **Step 2: Run and confirm failure**

Run: `go test ./internal/telemetry/metrics -run FUSEMetrics -v`

Expected: FAIL because the instruments are undefined.

- [ ] **Step 3: Add bounded-cardinality instruments and dashboard panels**

Register the metrics named in design section 15. Labels may contain only `runtime`, `provider`, `pool_key`, `state`, `result`, `reason` and `operation`; never attach sandbox ID, prefix, endpoint, Secret or credentials. Instrument every Pool transition, mount, lease loss, cache threshold and flush. Add dashboard panels for prepared shortfall, prolonged preparing/reserved/binding, mount error rate and lease loss. Exclude only `sandbox.pool.state=prepared` from generic Kubernetes NotReady alerts.

- [ ] **Step 4: Run metrics and JSON validation**

Run: `go test ./internal/telemetry/metrics ./internal/sandbox && jq empty deploy/grafana/dashboards/sandbox-overview.json`

Expected: PASS and `jq` exits 0.

- [ ] **Step 5: Commit**

```bash
git add internal/telemetry/metrics/metrics.go deploy/grafana/dashboards/sandbox-overview.json internal/sandbox/fuse_pool.go internal/sandbox/workspace_lease.go internal/sandbox/manager.go
git commit -m "feat: observe fuse pool and workspace health"
```

### Task 17: Wire sandbox-api and Deployment Assets

**Files:**
- Modify: `cmd/sandbox/main.go`
- Modify: `deploy/helm/sandbox/values.yaml`
- Modify: `deploy/helm/sandbox/templates/deployment.yaml`
- Modify: `deploy/helm/sandbox/templates/rbac.yaml`
- Modify: `deploy/helm/sandbox/templates/networkpolicy.yaml`
- Create: `deploy/helm/sandbox/templates/runtime-role.yaml`
- Create: `deploy/helm/sandbox/templates/runtime-rolebinding.yaml`
- Modify: `docker/docker-compose.yml`
- Create: `scripts/workspace-fuse-preflight.sh`
- Modify: `docs/deployment/workspace-fuse.md`

- [ ] **Step 1: Add render and preflight smoke tests**

Add this CI command sequence to the repository deployment test job:

```bash
helm lint deploy/helm/sandbox
helm template sandbox deploy/helm/sandbox \
  --namespace sandbox-control \
  --set security.apiKey=test \
  --set config.workspace.mode=sync > /tmp/sandbox-sync.yaml
helm template sandbox deploy/helm/sandbox \
  --namespace sandbox-control \
  -f testdata/values-fuse-minio.yaml > /tmp/sandbox-fuse.yaml
python3 - <<'PY'
import yaml
for path in ("/tmp/sandbox-sync.yaml", "/tmp/sandbox-fuse.yaml"):
    list(yaml.safe_load_all(open(path, encoding="utf-8")))
PY
bash -n scripts/workspace-fuse-preflight.sh
docker compose -f docker/docker-compose.yml config >/tmp/sandbox-compose.yaml
```

- [ ] **Step 2: Run and confirm failure**

Run the command sequence above.

Expected: FAIL because FUSE values, RBAC, test values and preflight script are not implemented.

- [ ] **Step 3: Wire the control loop and deployment contract**

In `cmd/sandbox/main.go`, retain the Redis `Store`, assert only that it implements `state.AtomicStore`, construct the sole `redisstate.NewFUSEPoolRepository(store)` wrapper, generate a random API instance ownership token at process start, and pass all FUSE dependencies through `ManagerConfig`. In sync mode construct the existing `goairix/fs` filesystem. In FUSE mode load file credentials once, construct the native `WorkspaceObjectClient` with the configured CA, zero the owned credential buffers immediately, and do not construct or fall back to the `goairix/fs` MinIO/OBS driver. `mgr.Start` remains the only place that starts WarmUp/reconciliation; main must check its returned error and must not start the HTTP listener when Redis, restore, reconciliation or WarmUp fails. No new executable or Kubernetes controller owns Pool quantity.

Build `WorkspaceFUSESpec.SystemEgress` only from the selected provider's validated `SystemEgressMode`, `DNSCIDRs`, endpoint CIDRs/FQDNs, endpoint ports and empty `ProxyURL`; copy the exact same DNS values into the Pod/Docker resolver configuration. No runtime DNS lookup, global default or user request may add destinations during this mapping.

Add Helm values matching deployment section 6.3, project credential files into sandbox-api, grant runtime namespace Pod create/get/list/watch/delete/**patch**, Pod exec and NetworkPolicy permissions, and keep runtime default-deny separate from the control namespace policy. Configure the named AppArmor/SELinux profile for both runtimes and reject `unconfined`. Compose mounts `/dev/fuse` only into dynamically created special containers, exposes root-only Secret staging, and never mounts host `/workspace`.

Implement `scripts/workspace-fuse-preflight.sh` with subcommands `kubernetes`, `docker` and `record-profile`. Runtime subcommands validate image digest, Secret, `/dev/fuse`, default-deny/RBAC (including `kubectl auth can-i patch pods`), non-unconfined LSM profile, bootstrap RuntimeUID, prepared state, same runtime UID across Acquire, FUSE type, UID 1000 create/read/delete, exact endpoint reachability and evidence-backed cleanup. `record-profile` accepts an already tested profile ID, real image digest, observed service/Everest/s3fs versions and output path; it rejects non-digest images and writes the immutable YAML report. The script must use `set -euo pipefail`, a trap-based cleanup, and return non-zero on incomplete cleanup.

- [ ] **Step 4: Run deployment validation**

Run the render sequence from Step 1, then in an integration environment:

```bash
scripts/workspace-fuse-preflight.sh kubernetes
scripts/workspace-fuse-preflight.sh docker
```

Expected: all commands exit 0; prepared Kubernetes Pods remain NotReady until Acquire, and neither runtime has a host business `/workspace` mount.

- [ ] **Step 5: Commit**

```bash
git add cmd/sandbox/main.go deploy/helm/sandbox docker/docker-compose.yml scripts/workspace-fuse-preflight.sh docs/deployment/workspace-fuse.md testdata/values-fuse-minio.yaml
git commit -m "deploy: wire sandbox api managed fuse pool"
```

### Task 18: Execute Provider Compatibility and Fault Matrix

**Files:**
- Create: `testdata/fuse/profiles/minio-sigv4-path-style-v1.yaml`
- Create: `testdata/fuse/profiles/huawei-obs-public-v1.yaml`
- Create: `testdata/fuse/profiles/huawei-obs-private-2023-v1.yaml`
- Create: `scripts/workspace-fuse-matrix.sh`
- Modify: `docs/deployment/workspace-fuse.md`

- [ ] **Step 1: Implement an executable six-combination matrix**

```bash
#!/usr/bin/env bash
set -euo pipefail
profiles=(minio-sigv4-path-style-v1 huawei-obs-public-v1 huawei-obs-private-2023-v1)
runtimes=(kubernetes docker)
for profile in "${profiles[@]}"; do
  for runtime in "${runtimes[@]}"; do
    scripts/workspace-fuse-preflight.sh "$runtime" --profile "testdata/fuse/profiles/$profile.yaml"
    go test ./test/integration/workspacefuse -run TestWorkspaceFUSE -v \
      -args -runtime "$runtime" -profile "$profile"
  done
done
```

The integration suite must exercise empty prefix marker, create/read/overwrite/append/truncate/delete, directory operations, 1 GiB streaming upload, 10,000 small files, git checkout, pip/npm cache redirected to `/tmp`, lease conflict, endpoint outage, s3fs death, cache pressure, API crash at every Pool state, Redis outage, runtime restart, graceful flush, refusal of force-delete takeover without evidence, and cleanup after explicit matching fencing evidence.

- [ ] **Step 2: Run the matrix and retain the first failures**

Run: `scripts/workspace-fuse-matrix.sh`

Expected before environment-specific fixes: at least one profile may fail; retain logs keyed by runtime/profile/pool key without credentials or raw prefix.

- [ ] **Step 3: Freeze only passing profile parameters**

For each profile, write the exact tested fields with the preflight recorder; the following MinIO command is the concrete shape, and the two OBS commands use their distinct profile IDs and observed version variables:

```bash
: "${MINIO_IMAGE_DIGEST:?}"
: "${MINIO_SERVICE_VERSION:?}"
: "${S3FS_VERSION:?}"
scripts/workspace-fuse-preflight.sh record-profile \
  --profile-id minio-sigv4-path-style-v1 \
  --image-digest "$MINIO_IMAGE_DIGEST" \
  --service-version "$MINIO_SERVICE_VERSION" \
  --s3fs-version "$S3FS_VERSION" \
  --directory-marker trailing-slash-zero-byte \
  --tls-verify true \
  --option use_path_request_style \
  --option sigv4 \
  --output testdata/fuse/profiles/minio-sigv4-path-style-v1.yaml
```

Release CI accepts only the real registry digest produced by the signed build. Public and private OBS artifacts must remain separate and must record the actual service/Everest/s3fs versions observed in their target environments. Do not publish a selectable profile whose matrix failed.

- [ ] **Step 4: Re-run the complete matrix**

Run: `scripts/workspace-fuse-matrix.sh`

Expected: PASS for all six runtime/profile combinations that are enabled for release, with no credential material in logs.

- [ ] **Step 5: Commit verified profiles and report**

```bash
git add testdata/fuse/profiles scripts/workspace-fuse-matrix.sh docs/deployment/workspace-fuse.md
git commit -m "test: verify workspace fuse provider matrix"
```

### Task 19: Full Regression, Security Review and Release Gate

**Files:**
- Modify only files required by failures found in this task

- [ ] **Step 1: Run formatting, static analysis and unit tests**

```bash
go fmt ./...
go vet ./...
go test -race ./...
go -C sdk/go fmt ./...
go -C sdk/go test ./...
git diff --check
```

Expected: all commands exit 0.

- [ ] **Step 2: Run dependency, image and manifest checks**

```bash
go mod verify
go -C sdk/go mod verify
helm lint deploy/helm/sandbox
docker compose -f docker/docker-compose.yml config >/dev/null
jq empty deploy/grafana/dashboards/sandbox-overview.json
scripts/workspace-fuse-matrix.sh
```

Expected: all commands exit 0; all enabled images use verified digests.

- [ ] **Step 3: Audit the security invariants directly**

Confirm with automated assertions and runtime inspection:

```text
prepared: no user session, no public Exec/file access, no s3fs, no mount generation
authorized: exactly one mount_attempt, exact canonical prefix, no arbitrary argv
Kubernetes sandbox: UID/GID 1000, no Secret, no /dev/fuse, no SYS_ADMIN
Docker user exec: UID/GID 1000, root-only control socket and Secret
network: default deny, exact approved system egress, metadata blocked
destroy: runtime exit confirmed before lease/owner release, used instance never returns to Pool
```

Expected: every invariant has a passing test or preflight assertion; a manual statement without evidence does not pass the gate.

- [ ] **Step 4: Verify sync rollback**

Start with `workspace.mode=sync`, create/read/write/sync/destroy a remote workspace, then switch a canary to FUSE and back to sync. Confirm legacy sync uses the original Pool, no FUSE privileged shell is allocated to a sync request, and the first post-FUSE sync performs a full copy.

Expected: sync behavior and API response compatibility remain intact.

- [ ] **Step 5: Commit only actual fixes from the release gate**

If Step 1-4 changed files, stage each named file explicitly and commit:

```bash
git diff --name-only
git diff --name-only -z | xargs -0 git add --
git commit -m "fix: close workspace fuse release gaps"
```

If there are no changes, do not create an empty commit.

## Completion Criteria

- `sandbox-api` alone owns FUSE Pool WarmUp, count maintenance, refill and Drain; Redis is passive coordination/state.
- Pool hit preserves the Pod UID/container ID while binding the request's canonical `workspace_path/prefix`.
- FUSE authorization is consumed once; success, failure and cancellation after consumption all end in runtime destruction.
- Public APIs cannot address prepared shells or hot-mount an already delivered FUSE sandbox.
- Kubernetes and Docker enforce the Secret, UID, capability, network and mount-propagation boundaries in the design.
- MinIO, Huawei public OBS and Huawei private 2023 OBS are enabled only with separate passing profile evidence.
- Legacy sync remains the default and passes its regression suite.
