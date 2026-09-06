# Workspace Storage Modes and Backend Presets Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use `superpowers:executing-plans` to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking. This repository session must execute the work inline because the user explicitly disabled subagent implementation after Task 14.

**Goal:** Make one sandbox-api release run sync and FUSE workspaces concurrently against one operator-selected MinIO or Huawei OBS backend, using common runtime images and crash-safe persistent/ephemeral cleanup.

**Architecture:** Helm selects one fixed backend preset and supplies one immutable backend identity; sandbox-api validates that selection, always maintains the ordinary Pool, and additionally maintains the FUSE Pool when FUSE is enabled. Each create request resolves `workspace_mount_mode` independently of sandbox lifecycle mode, while sync and FUSE share the same Redis workspace lease. Persistent sandboxes use the user session namespace, ephemeral workspaces use a separate lifecycle namespace that exists only to finish sync/flush and exact teardown after failures or API restarts.

**Tech Stack:** Go 1.25, Gin, Viper, Redis CAS/Lua state, Docker Engine API, Kubernetes client-go, Helm 3, s3fs 1.95, Bash integration scripts

---

## File structure and responsibility map

Create these focused files:

- `internal/sandbox/ephemeral.go`: strict `sandbox:ephemeral:v1:` record schema and atomic lifecycle store.
- `internal/sandbox/ephemeral_test.go`: namespace, validation, CAS, and exact-removal tests.
- `internal/sandbox/sync_lifecycle.go`: sync workspace lease renewal and finalization; no FUSE protocol code.
- `internal/sandbox/sync_lifecycle_test.go`: cross-mode exclusion and strong final-sync tests.
- `internal/sandbox/drain_audit.go`: release-drain state audit shared by process shutdown and the Kubernetes hook command.
- `internal/sandbox/drain_audit_test.go`: allowed generation keys and blocking lifecycle-key tests.
- `deploy/helm/sandbox/templates/_helpers.tpl`: fixed preset mapping, FUSE-enabled predicate, system-egress host derivation, and backend fingerprint.
- `deploy/helm/sandbox/templates/backend-fingerprint.yaml`: non-secret applied-backend fingerprint ConfigMap.
- `deploy/helm/sandbox/templates/pre-backend-change-drain.yaml`: conditional pre-upgrade/pre-rollback fail-closed drain hook.
- `deploy/helm/sandbox/values.schema.json`: enum/type validation for presets and mount modes.
- `scripts/test-helm-backend-switch.sh`: kind-backed unchanged/changed fingerprint upgrade test.
- `docker/images/workspace-mounter/profile-bundle.json`: the three verified descriptors bound to one s3fs artifact hash.

Modify these existing files without unrelated restructuring:

- `internal/mounter/{profile.go,manifest.go,systemcheck.go}` and tests: change one-profile image binding into strict multi-profile bundle validation.
- `cmd/workspace-mounter/main.go` and tests: remove linker-selected profile state; supervise with the compiled catalog.
- `docker/images/{workspace-mounter,sandbox-fuse}/Dockerfile`, `scripts/{verify-fuse-image.sh,test-fuse-images.sh}`: build and verify one image per runtime/architecture.
- `internal/config/config.go` and tests: add fixed backend preset plus default/enabled mount modes, retaining an explicit legacy migration path.
- `pkg/types/sandbox.go`, `internal/api/handler/{sandbox.go,handler.go}` and tests: expose and validate `workspace_mount_mode`.
- `sdk/go/{types.go,types_test.go,sandbox.go,client_test.go,README.md}`: mirror the public API.
- `internal/sandbox/{types.go,manager.go,workspace.go,workspace_lease.go,session.go}` and tests: dual-Pool routing, common lease, persistent/ephemeral publication, recovery, and finalization.
- `internal/storage/filesystem.go` and tests: allow sync and FUSE clients to be initialized from the same credential files without logging their contents.
- `cmd/sandbox/{main.go,main_test.go,drain.go,drain_test.go}`: construct both pools/clients and expose the audited drain command.
- `deploy/helm/sandbox/{values.yaml,Chart.yaml}`, `deploy/helm/sandbox/templates/{deployment.yaml,rbac.yaml}`, and `testdata/values-fuse-*.yaml`: preset-driven single-backend deployment.
- `docker/{docker-compose.yml,.env.example}`, `configs/config.yaml`: operator-facing hybrid configuration.
- `scripts/{test-helm-chart.sh,workspace-fuse-preflight.sh,workspace-fuse-matrix.sh,test-workspace-fuse-matrix.sh}` and `test/integration/workspacefuse/workspace_fuse_test.go`: hybrid and six-combination verification.
- `docs/deployment/workspace-fuse.md`, `README.md`: final deployment, switching, rollback, and code-executor behavior.

## Task 1: Replace the single-profile image contract with a common profile bundle

**Files:**

- Create: `docker/images/workspace-mounter/profile-bundle.json`
- Delete: `docker/images/workspace-mounter/profiles/minio-sigv4-path-style-v1.json`
- Delete: `docker/images/workspace-mounter/profiles/huawei-obs-public-v1.json`
- Delete: `docker/images/workspace-mounter/profiles/huawei-obs-private-2023-v1.json`
- Modify: `internal/mounter/profile.go`
- Modify: `internal/mounter/manifest.go`
- Modify: `internal/mounter/systemcheck.go`
- Modify: `internal/mounter/manifest_test.go`
- Modify: `cmd/workspace-mounter/main.go`
- Modify: `cmd/workspace-mounter/main_test.go`
- Modify: `docker/images/workspace-mounter/Dockerfile`
- Modify: `docker/images/sandbox-fuse/Dockerfile`
- Modify: `scripts/verify-fuse-image.sh`
- Modify: `scripts/test-fuse-images.sh`

- [ ] **Step 1: Write failing bundle-contract tests**

Add tests that require all three compiled verified profiles, reject missing/duplicate/unknown descriptors, reject descriptor drift, and require one exact s3fs hash:

```go
func TestProfileBundleRequiresExactCompiledCatalogAndBinaryHash(t *testing.T) {
    raw, digest := validProfileBundle(t)
    require.NoError(t, validateProfileBundle(raw, digest))

    bundle := decodeTestBundle(t, raw)
    bundle.Profiles = bundle.Profiles[:2]
    require.ErrorContains(t, validateProfileBundle(marshalTestBundle(t, bundle), digest), "compiled profile set")

    bundle = decodeTestBundle(t, raw)
    bundle.Profiles = append(bundle.Profiles, bundle.Profiles[0])
    require.ErrorContains(t, validateProfileBundle(marshalTestBundle(t, bundle), digest), "duplicate profile")

    bundle = decodeTestBundle(t, raw)
    bundle.Profiles[0].SignatureVersion = "sigv2"
    require.ErrorContains(t, validateProfileBundle(marshalTestBundle(t, bundle), digest), "descriptor")
}

func TestBundledProfilesContainsEveryProductionProfile(t *testing.T) {
    profiles := BundledProfiles()
    for _, pair := range [][2]string{
        {"minio", "minio-sigv4-path-style-v1"},
        {"obs", "huawei-obs-public-v1"},
        {"obs", "huawei-obs-private-2023-v1"},
    } {
        _, ok := profiles.Lookup(pair[1])
        require.True(t, ok, pair[1])
        require.NoError(t, CheckProductionProfile(pair[0], pair[1]))
    }
}
```

- [ ] **Step 2: Run the focused tests and confirm the old one-profile contract fails**

Run: `go test ./internal/mounter ./cmd/workspace-mounter -run 'Test(ProfileBundle|BundledProfiles)' -count=1`

Expected: FAIL because `ProfileBundle`, `validateProfileBundle`, and `BundledProfiles` do not exist.

- [ ] **Step 3: Implement strict bundle validation and remove linker profile selection**

Use one schema and the compiled typed catalog as the only source of runtime options:

```go
const ProfileBundleVersion = 1

type ProfileBundle struct {
    Version      int                 `json:"version"`
    Profiles     []ProfileDescriptor `json:"profiles"`
    S3FSSHA256   string              `json:"s3fs_sha256"`
}

func BundledProfiles() ProfileRegistry {
    profiles := make(StaticProfiles)
    for id, profile := range compiledProfileCatalog {
        if profile.Descriptor.MountParameters == MountParametersVerified && profile.Options != nil {
            profiles[id] = profile
        }
    }
    return profiles
}
```

`validateProfileBundle` must strictly decode JSON, reject duplicate JSON keys and duplicate profile IDs, compare the ID-to-descriptor map exactly without trusting input order, and match `S3FSSHA256` to `/usr/bin/s3fs`. `CheckImageReleaseWithConfig` must call `CheckProductionProfile` for every bundled descriptor. In `cmd/workspace-mounter/main.go`, delete `imageProfileID`; use `mounter.BundledProfiles()` in `supervise()` and remove the profile argument from both image-check functions.

Package `/etc/workspace-fuse/profile-bundle.json`; Docker build arguments become only `BASE_IMAGE`, `S3FS_PACKAGE_URL`, `S3FS_PACKAGE_SHA256`, and `PROFILE_BUNDLE`. Both Dockerfiles must replace the hash exactly once and run the same self-check. `scripts/verify-fuse-image.sh` accepts exactly a runtime (`kubernetes` or `docker`) and a check (`package-check` or `release-check`), then verifies all three IDs inside the bundle.

- [ ] **Step 4: Run Go and packaging contract tests**

Run:

```bash
go test ./internal/mounter ./cmd/workspace-mounter -count=1
./scripts/test-fuse-images.sh
```

Expected: both commands PASS; the shell test confirms there is no `PROFILE_ID`, `imageProfileID`, or `/etc/workspace-fuse/profile.json` reference.

- [ ] **Step 5: Commit the common-image contract**

```bash
git add internal/mounter cmd/workspace-mounter docker/images/workspace-mounter docker/images/sandbox-fuse scripts/verify-fuse-image.sh scripts/test-fuse-images.sh
git commit -m "feat: bundle verified workspace fuse profiles"
```

## Task 2: Introduce fixed backend presets and hybrid mount-mode configuration

**Files:**

- Modify: `internal/config/config.go`
- Modify: `internal/config/config_test.go`
- Modify: `configs/config.yaml`

- [ ] **Step 1: Write failing configuration migration and validation tests**

Cover the default, hybrid, legacy, and mismatch cases:

```go
func TestLoadDefaultsWorkspaceToSyncOnly(t *testing.T) {
    cfg := loadMinimalConfig(t, "")
    assert.Equal(t, "sync", cfg.Workspace.DefaultMountMode)
    assert.Equal(t, []string{"sync"}, cfg.Workspace.EnabledMountModes)
}

func TestValidateHybridWorkspaceBackendPreset(t *testing.T) {
    cfg := validHybridConfig()
    require.NoError(t, cfg.Validate())
    assert.Equal(t, "minio", cfg.Workspace.Backend.Preset)
    assert.Equal(t, "minio-sigv4-path-style-v1", cfg.Workspace.Backend.Profile)
}

func TestValidateRejectsPresetProviderOrProfileDrift(t *testing.T) {
    cfg := validHybridConfig()
    cfg.Workspace.Backend.Profile = "huawei-obs-public-v1"
    require.ErrorContains(t, cfg.Validate(), "preset mapping")
}

func TestLoadMapsLegacyFuseWithoutCreatingHybridMode(t *testing.T) {
    cfg := loadLegacyFUSEConfig(t)
    assert.Equal(t, "fuse", cfg.Workspace.DefaultMountMode)
    assert.Equal(t, []string{"fuse"}, cfg.Workspace.EnabledMountModes)
}
```

Also test duplicate modes, unknown modes, default not enabled, more than one backend, FUSE without Redis/Secret/digest images, and a new configuration mixed with legacy `workspace.mode/providers`.

- [ ] **Step 2: Run configuration tests and confirm failure**

Run: `go test ./internal/config -run 'Test(LoadDefaultsWorkspace|ValidateHybrid|ValidateRejectsPreset|LoadMapsLegacy)' -count=1`

Expected: FAIL because the new fields and preset resolver are absent.

- [ ] **Step 3: Implement the resolved configuration model and compatibility normalization**

Use a fixed catalog and a single resolved backend:

```go
type WorkspaceBackendConfig struct {
    Preset               string   `mapstructure:"preset"`
    Driver               string   `mapstructure:"driver"`
    Profile              string   `mapstructure:"profile"`
    StorageIdentity      string   `mapstructure:"storage_identity"`
    MounterImage         string   `mapstructure:"mounter_image"`
    DockerImage          string   `mapstructure:"docker_image"`
    CASecretKey          string   `mapstructure:"ca_secret_key"`
    CredentialGeneration string   `mapstructure:"credential_generation"`
    EndpointHostIPs      []string `mapstructure:"endpoint_host_ips"`
    LSMProfile           string   `mapstructure:"lsm_profile"`
    SystemEgressMode     string   `mapstructure:"system_egress_mode"`
    DNSCIDRs             []string `mapstructure:"dns_cidrs"`
    SystemEgressFQDNs    []string `mapstructure:"system_egress_fqdns"`
    SystemEgressCIDRs    []string `mapstructure:"system_egress_cidrs"`
    EndpointPorts        []int32  `mapstructure:"endpoint_ports"`
    ProxyURL             string   `mapstructure:"proxy_url"`
}

var backendPresets = map[string]struct{ Provider, Profile string }{
    "minio":              {Provider: "minio", Profile: "minio-sigv4-path-style-v1"},
    "huawei-obs-public":  {Provider: "obs", Profile: "huawei-obs-public-v1"},
    "huawei-obs-private": {Provider: "obs", Profile: "huawei-obs-private-2023-v1"},
}

type WorkspaceConfigSelection struct {
    DefaultMountMode        string                 `mapstructure:"default_mount_mode"`
    EnabledMountModes       []string               `mapstructure:"enabled_mount_modes"`
    Backend                 WorkspaceBackendConfig `mapstructure:"backend"`
    LegacyMode              string                 `mapstructure:"mode"`
    LegacyProviders         map[string]WorkspaceFUSEProviderConfig `mapstructure:"providers"`
}
```

Add the five `WorkspaceConfigSelection` fields directly to the existing `WorkspaceConfig`; the named type above defines their exact tags and types without replacing its cache, timeout, resources, Pool, auto-sync, or local-test fields. Do not register a Viper default for `workspace.mode`. After unmarshal, normalize an explicit legacy mode only when all new mode/backend fields are absent; map legacy `sync` to `[sync]` and legacy `fuse` to `[fuse]`. New blank configuration becomes `default=sync, enabled=[sync]`. Validate the selected preset against both `storage.filesystem.provider` and the resolved profile, and expose `WorkspaceConfig.MountModeEnabled(string) bool`.

- [ ] **Step 4: Run configuration and command wiring tests**

Run: `go test ./internal/config ./cmd/sandbox -count=1`

Expected: PASS with old config fixtures migrated explicitly and new hybrid fixtures accepted.

- [ ] **Step 5: Commit the configuration model**

```bash
git add internal/config/config.go internal/config/config_test.go configs/config.yaml
git commit -m "feat: add workspace backend presets and mount modes"
```

## Task 3: Add request-level workspace mount mode to the API and SDK

**Files:**

- Modify: `pkg/types/sandbox.go`
- Modify: `internal/sandbox/types.go`
- Modify: `internal/sandbox/errors.go`
- Modify: `internal/api/handler/sandbox.go`
- Modify: `internal/api/handler/handler.go`
- Modify: `internal/api/handler/handler_test.go`
- Modify: `sdk/go/types.go`
- Modify: `sdk/go/types_test.go`
- Modify: `sdk/go/sandbox.go`
- Modify: `sdk/go/client_test.go`
- Modify: `sdk/go/README.md`

- [ ] **Step 1: Write failing handler and SDK contract tests**

```go
func TestCreateSandboxRejectsMountModeWithoutWorkspace(t *testing.T) {
    response := postSandbox(t, `{"mode":"ephemeral","workspace_mount_mode":"fuse"}`)
    assert.Equal(t, http.StatusBadRequest, response.Code)
    assert.Contains(t, response.Body.String(), "WORKSPACE_MOUNT_MODE_INVALID")
}

func TestSandboxResponseIncludesResolvedWorkspaceMountMode(t *testing.T) {
    sb := &sandbox.Sandbox{Config: sandbox.SandboxConfig{Mode: sandbox.ModePersistent, WorkspaceMountMode: sandbox.WorkspaceMountFUSE}}
    sb.Workspace = &sandbox.WorkspaceInfo{MountType: sandbox.WorkspaceMountFUSE}
    assert.Equal(t, "fuse", sandboxToResponse(sb).WorkspaceMountMode)
}

func TestWorkspaceMountModeJSON(t *testing.T) {
    raw, err := json.Marshal(sandbox.CreateSandboxRequest{
        Mode: sandbox.ModeEphemeral, WorkspacePath: "jobs/a", WorkspaceMountMode: sandbox.WorkspaceMountFUSE,
    })
    require.NoError(t, err)
    assert.Contains(t, string(raw), `"workspace_mount_mode":"fuse"`)
}
```

- [ ] **Step 2: Run API/SDK tests and confirm failure**

Run:

```bash
go test ./internal/api/handler ./pkg/types -count=1
(cd sdk/go && go test ./... -count=1)
```

Expected: FAIL on the missing request, response, SDK, and internal config fields.

- [ ] **Step 3: Implement typed request, response, and error mapping**

Add these public SDK constants and mirror their string values internally:

```go
type WorkspaceMountMode string

const (
    WorkspaceMountSync WorkspaceMountMode = "sync"
    WorkspaceMountFUSE WorkspaceMountMode = "fuse"
)

type CreateSandboxRequest struct {
    Mode                 Mode               `json:"mode"`
    WorkspacePath        string             `json:"workspace_path,omitempty"`
    WorkspaceMountMode   WorkspaceMountMode `json:"workspace_mount_mode,omitempty"`
    WorkspaceSyncExclude []string           `json:"workspace_sync_exclude,omitempty"`
}
```

The snippet shows the four workspace/lifecycle fields to add or replace; retain the existing `Timeout`, `Resources`, `Network`, and `Dependencies` fields with their current tags. `pkg/types.CreateSandboxRequest` uses `binding:"omitempty,oneof=sync fuse"`. The handler rejects a supplied mount mode when `workspace_path` is empty before calling the manager. Add `ErrInvalidWorkspaceMountMode`, map it to HTTP 400 and `WORKSPACE_MOUNT_MODE_INVALID`, pass the value into `sandbox.SandboxConfig`, and return the resolved mode only when a workspace exists. `sdk/go.SandboxOptions` must pass the same field.

- [ ] **Step 4: Run API and SDK tests**

Run the two commands from Step 2.

Expected: PASS, including backward-compatible JSON with the field omitted.

- [ ] **Step 5: Commit the API contract**

```bash
git add pkg/types/sandbox.go internal/sandbox/types.go internal/sandbox/errors.go internal/api/handler sdk/go
git commit -m "feat: expose workspace mount mode per sandbox"
```

## Task 4: Start both pools and route each create request to sync or FUSE

**Files:**

- Modify: `internal/storage/filesystem.go`
- Modify: `internal/storage/filesystem_test.go`
- Modify: `internal/sandbox/manager.go`
- Modify: `internal/sandbox/manager_test.go`
- Modify: `internal/sandbox/workspace.go`
- Modify: `internal/sandbox/workspace_test.go`
- Modify: `cmd/sandbox/main.go`
- Modify: `cmd/sandbox/main_test.go`

- [ ] **Step 1: Write failing dual-Pool routing tests**

```go
func TestManagerHybridRoutesDefaultSyncAndExplicitFUSE(t *testing.T) {
    mgr, ordinary, fuse := newHybridManager(t)

    syncSandbox, err := mgr.Create(context.Background(), SandboxConfig{Mode: ModePersistent, WorkspacePath: "team/sync"})
    require.NoError(t, err)
    assert.Equal(t, WorkspaceMountSync, syncSandbox.Config.WorkspaceMountMode)
    assert.True(t, ordinary.acquired(syncSandbox.RuntimeID))

    fuseSandbox, err := mgr.Create(context.Background(), SandboxConfig{Mode: ModePersistent, WorkspacePath: "team/fuse", WorkspaceMountMode: WorkspaceMountFUSE})
    require.NoError(t, err)
    assert.Equal(t, WorkspaceMountFUSE, fuseSandbox.Config.WorkspaceMountMode)
    assert.True(t, fuse.consumed(fuseSandbox.RuntimeID))
}

func TestManagerNoWorkspaceUsesOrdinaryPoolWithoutStorage(t *testing.T) {
    mgr, ordinary, objectStore := newHybridNoWorkspaceManager(t)
    sb, err := mgr.Create(context.Background(), SandboxConfig{Mode: ModeEphemeral})
    require.NoError(t, err)
    assert.True(t, ordinary.acquired(sb.RuntimeID))
    assert.Zero(t, objectStore.calls())
}
```

Add startup tests asserting the ordinary Pool always warms and FUSE Pool warms only when `fuse` is enabled. Add a negative test for explicit FUSE when only sync is enabled.

- [ ] **Step 2: Run focused manager/wiring tests and confirm failure**

Run: `go test ./internal/storage ./internal/sandbox ./cmd/sandbox -run 'Test.*(Hybrid|MountMode|NoWorkspace|BothPools)' -count=1`

Expected: FAIL because `ManagerConfig` still has one global `WorkspaceMode` and FUSE startup skips the ordinary Pool.

- [ ] **Step 3: Initialize both data paths and resolve the request mode once**

Replace `ManagerConfig.WorkspaceMode` with:

```go
type ManagerConfig struct {
    PoolConfig            PoolConfig
    DefaultMountMode      WorkspaceMountType
    EnabledMountModes     map[WorkspaceMountType]bool
    FUSEPool              *FUSEPool
    WorkspaceCoordinator  *WorkspaceCoordinator
    WorkspaceObjectClient storage.WorkspaceObjectClient
}
```

The snippet is the exact replacement for the current selection/Pool portion of `ManagerConfig`; retain the existing `WorkspaceMarkerProfile`, `FUSEHealthInterval`, timeout, exec, and auto-sync fields. Add a pure resolver that returns no mount mode for no workspace, otherwise applies the default and rejects disabled modes. Store the resolved value in `SandboxConfig` before any runtime allocation. Set `WorkspaceInfo.MountType=WorkspaceMountSync` for remote sync workspaces and keep local bind mounts as `WorkspaceMountLocal`.

In `Manager.Start`, restore lifecycle state first, reconcile orphans, warm the ordinary Pool unconditionally, then start FUSE Pool when enabled. Add this storage entry point and cover inline, file, and mixed-source cases:

```go
func NewFileSystemFromConfiguredCredentials(cfg config.FileSystemConfig, storageIdentity string) (fs.FileSystem, *FileSystemMeta, error)
```

For remote providers it calls `LoadFileSystemCredentials`, injects the pair only into a private provider-construction copy, constructs the driver, and zeroes the owned byte buffers before returning; local storage bypasses credential loading. It never logs either value. In `cmd/sandbox/main.go`, always construct this filesystem for sync. Construct the object client, FUSE secret materializer, coordinator, and FUSE Pool only when FUSE is enabled.

- [ ] **Step 4: Run storage, manager, and wiring tests**

Run: `go test ./internal/storage ./internal/sandbox ./cmd/sandbox -count=1`

Expected: PASS; existing sync tests and existing FUSE Pool tests both remain green.

- [ ] **Step 5: Commit dual-Pool routing**

```bash
git add internal/storage/filesystem.go internal/storage/filesystem_test.go internal/sandbox/manager.go internal/sandbox/manager_test.go internal/sandbox/workspace.go internal/sandbox/workspace_test.go cmd/sandbox
git commit -m "feat: run sync and fuse workspace pools together"
```

## Task 5: Enforce one cross-mode workspace lease for sync and FUSE

**Files:**

- Create: `internal/sandbox/sync_lifecycle.go`
- Create: `internal/sandbox/sync_lifecycle_test.go`
- Modify: `internal/sandbox/workspace_lease.go`
- Modify: `internal/sandbox/workspace_lease_test.go`
- Modify: `internal/sandbox/workspace.go`
- Modify: `internal/sandbox/manager.go`
- Modify: `internal/sandbox/session.go`

- [ ] **Step 1: Write failing cross-mode exclusion and safe-unmount tests**

```go
func TestSyncAndFUSEUseTheSameWorkspaceLeaseKey(t *testing.T) {
    coordinator := NewWorkspaceCoordinator(newAtomicMemoryStore(), time.Minute, 10*time.Second)
    syncLease := acquireWorkspaceForMode(t, coordinator, WorkspaceMountSync, "team/shared/")
    _, err := coordinator.Acquire(context.Background(), leaseRequestForMode(WorkspaceMountFUSE, "team/shared/"))
    require.ErrorIs(t, err, ErrWorkspaceLeased)
    require.NoError(t, coordinator.Release(context.Background(), syncLease, runtime.TerminationEvidence{}))
}

func TestSyncUnmountReleasesLeaseWithoutStoppingRuntime(t *testing.T) {
    mgr := newLeasedSyncManager(t)
    sb := createSyncWorkspace(t, mgr, "team/shared")
    require.NoError(t, mgr.UnmountWorkspace(context.Background(), sb.ID))
    assert.True(t, mgr.runtimeStillRunning(sb.RuntimeID))
    assert.False(t, mgr.workspaceOwnerExists("team/shared/"))
}

func TestRestoreReacquiresExpiredLeaseFromExactOwner(t *testing.T) {
    coordinator, owner := expiredExactOwnerFixture(t, WorkspaceMountFUSE)
    restored, err := coordinator.Restore(context.Background(), owner)
    require.NoError(t, err)
    assert.Equal(t, owner.Generation, restored.OwnerSnapshot().Generation)
    assert.Equal(t, owner.RuntimeUID, restored.OwnerSnapshot().RuntimeUID)
}
```

Also test two sync sandboxes conflict, different prefixes succeed, renewal loss closes the sync operation gate, and a stale FUSE owner still blocks sync after TTL expiry.

- [ ] **Step 2: Run lease/lifecycle tests and confirm failure**

Run: `go test ./internal/sandbox -run 'Test(SyncAndFUSE|SyncUnmount|SyncLease|WorkspaceLease)' -count=1`

Expected: FAIL because sync does not currently acquire the coordinator lease.

- [ ] **Step 3: Add access-mode-aware owner records and sync lifecycle management**

Persist the mount type in both lease and owner values without adding it to the Redis key:

```go
type WorkspaceLeaseRequest struct {
    Provider, StorageIdentity, Bucket, Prefix string
    SandboxID, Runtime, RuntimeID, RuntimeUID  string
    MountType WorkspaceMountType
}

type WorkspaceOwner struct {
    Provider, StorageIdentityHash, Bucket, Prefix, WorkspaceHash string
    SandboxID, Runtime, RuntimeID, RuntimeUID                    string
    MountType WorkspaceMountType `json:"mount_type"`
    Generation int64           `json:"generation"`
    MountAttempt uint8         `json:"mount_attempt"`
    UpdatedAt time.Time        `json:"updated_at"`
}

type workspaceLeaseRecord struct {
    Version             uint8              `json:"version"`
    Phase               string             `json:"phase"`
    Token               string             `json:"token"`
    Provider            string             `json:"provider"`
    StorageIdentityHash string             `json:"storage_identity_hash"`
    Bucket              string             `json:"bucket"`
    Prefix              string             `json:"prefix"`
    WorkspaceHash       string             `json:"workspace_hash"`
    SandboxID           string             `json:"sandbox_id"`
    Runtime             string             `json:"runtime"`
    RuntimeID           string             `json:"runtime_id"`
    RuntimeUID          string             `json:"runtime_uid"`
    MountType           WorkspaceMountType `json:"mount_type"`
    CreatedAt           time.Time          `json:"created_at"`
    Generation          int64              `json:"generation"`
}
```

The lease key remains provider + storage identity hash + bucket + canonical prefix. `safeToReleaseOwner` accepts zero termination evidence for sync only after the sync operation gate is exclusively closed and final sync/unmount completed; FUSE retains the existing exact runtime termination rule.

`sync_lifecycle.go` owns the lease, renewal, and gate for a mounted sync workspace. `MountWorkspace` acquires the lease before `syncToContainer`, binds the exact runtime UID, starts renewal, and publishes the workspace only after sync-in succeeds. The create path uses a private mount helper so a workspace sandbox is not inserted into the public in-memory map before sync-in, lifecycle persistence, and lease publication all succeed. `UnmountWorkspace` closes admission, runs final sync-out, clears the workspace, releases the lease, and reopens ordinary sandbox admission. Lease loss closes admission and schedules strong finalization rather than permitting another sync/FUSE writer.

Extend `WorkspaceCoordinator.Restore` so an exact non-expiring owner can atomically republish an expired TTL lease with the same generation after the Manager has already verified the exact runtime UID and mode-specific health. It must never allocate a new generation, overwrite a live lease, or adopt a changed owner; concurrent replicas race through `SetNX`, and only the winner starts renewal.

- [ ] **Step 4: Run sandbox and race tests**

Run:

```bash
go test ./internal/sandbox -count=1
go test -race ./internal/sandbox -run 'Test(SyncAndFUSE|SyncLease|SyncUnmount)' -count=1
```

Expected: PASS with no data race and no cross-mode concurrent owner.

- [ ] **Step 5: Commit common workspace leasing**

```bash
git add internal/sandbox/sync_lifecycle.go internal/sandbox/sync_lifecycle_test.go internal/sandbox/workspace_lease.go internal/sandbox/workspace_lease_test.go internal/sandbox/workspace.go internal/sandbox/manager.go internal/sandbox/session.go
git commit -m "feat: share workspace leases across sync and fuse"
```

## Task 6: Separate persistent sessions from ephemeral cleanup records

**Files:**

- Create: `internal/sandbox/ephemeral.go`
- Create: `internal/sandbox/ephemeral_test.go`
- Modify: `internal/sandbox/session.go`
- Modify: `internal/sandbox/manager.go`
- Modify: `internal/sandbox/manager_test.go`
- Modify: `internal/sandbox/workspace.go`

- [ ] **Step 1: Write failing namespace, CAS, and publication tests**

```go
func TestEphemeralLifecycleStoreUsesIsolatedNamespace(t *testing.T) {
    store := newAtomicMemoryStore()
    lifecycle := NewEphemeralLifecycleStore(store)
    record := validEphemeralRecord()
    require.NoError(t, lifecycle.Create(context.Background(), record))
    assert.True(t, store.hasKey("sandbox:ephemeral:v1:"+record.SandboxID))
    assert.False(t, store.hasKey("sandbox:session:v2:"+record.SandboxID))
}

func TestEphemeralFUSECreateDoesNotPublishUserSession(t *testing.T) {
    mgr, stores := newEphemeralFUSEManager(t)
    sb, err := mgr.Create(context.Background(), SandboxConfig{
        Mode: ModeEphemeral, WorkspacePath: "jobs/a", WorkspaceMountMode: WorkspaceMountFUSE,
    })
    require.NoError(t, err)
    assert.False(t, stores.sessions.exists(sb.ID))
    assert.True(t, stores.ephemeral.exists(sb.ID))
}
```

Add strict decode tests for unknown fields, invalid mode/state, empty runtime UID, inconsistent FUSE fields, stale revision CAS, and exact delete after a newer record replaces the expected value.

- [ ] **Step 2: Run ephemeral tests and confirm failure**

Run: `go test ./internal/sandbox -run 'TestEphemeral' -count=1`

Expected: FAIL because the ephemeral lifecycle store does not exist and FUSE publication always writes a session.

- [ ] **Step 3: Implement the private cleanup record and mode-aware publication**

Use this schema and state progression:

```go
const ephemeralLifecycleKeyPrefix = "sandbox:ephemeral:v1:"

type EphemeralFinalizationState string

const (
    EphemeralActive          EphemeralFinalizationState = "active"
    EphemeralFinalizing      EphemeralFinalizationState = "finalizing"
    EphemeralRemovingRuntime EphemeralFinalizationState = "removing-runtime"
    EphemeralReleasingLease  EphemeralFinalizationState = "releasing-lease"
)

type EphemeralLifecycleRecord struct {
    Version          int                       `json:"version"`
    SandboxID        string                    `json:"sandbox_id"`
    RuntimeID        string                    `json:"runtime_id"`
    RuntimeUID       string                    `json:"runtime_uid"`
    WorkspacePath    string                    `json:"workspace_path"`
    MountType        WorkspaceMountType        `json:"mount_type"`
    Owner            WorkspaceOwner            `json:"owner"`
    PreparationID    string                    `json:"preparation_id,omitempty"`
    PoolKey          string                    `json:"pool_key,omitempty"`
    ReservationToken string                    `json:"reservation_token,omitempty"`
    PoolRevision     uint64                    `json:"pool_revision,omitempty"`
    State            EphemeralFinalizationState `json:"state"`
    Revision         uint64                    `json:"revision"`
    UpdatedAt        time.Time                 `json:"updated_at"`
}
```

Require `state.AtomicStore`; implement `Create` with `SetNX`, `Load`, `List`, `Transition(expectedRevision, nextState)`, and `RemoveExact`. A sync record must have no FUSE fields; a FUSE record must have all four. Add `Manager.SetEphemeralLifecycleStore`.

Replace `publishSandboxAndSession` with `publishSandboxLifecycle`: persistent mode saves `sandbox:session:v2:` before in-memory publication; ephemeral workspace mode creates `sandbox:ephemeral:v1:` before publication. No-workspace ephemeral sandboxes write neither namespace. All later FUSE flush/session updates call a helper that writes the correct persistent or ephemeral store.

- [ ] **Step 4: Run session, ephemeral, and FUSE manager tests**

Run: `go test ./internal/sandbox -run 'Test(Session|Ephemeral|Manager.*FUSE)' -count=1`

Expected: PASS; `SessionStore.List` never sees ephemeral records and public Get/List recovery never exposes them.

- [ ] **Step 5: Commit lifecycle namespace separation**

```bash
git add internal/sandbox/ephemeral.go internal/sandbox/ephemeral_test.go internal/sandbox/session.go internal/sandbox/manager.go internal/sandbox/manager_test.go internal/sandbox/workspace.go
git commit -m "feat: persist ephemeral workspace cleanup state"
```

## Task 7: Make final sync/flush and restart cleanup strong and retryable

**Files:**

- Modify: `internal/sandbox/sync_lifecycle.go`
- Modify: `internal/sandbox/sync_lifecycle_test.go`
- Modify: `internal/sandbox/ephemeral.go`
- Modify: `internal/sandbox/ephemeral_test.go`
- Modify: `internal/sandbox/manager.go`
- Modify: `internal/sandbox/manager_test.go`
- Modify: `internal/sandbox/workspace.go`
- Modify: `internal/sandbox/errors.go`
- Modify: `internal/api/handler/handler.go`

- [ ] **Step 1: Write failing finalization and restart-recovery tests**

```go
func TestDestroyEphemeralSyncRetainsRuntimeAndLeaseWhenFinalSyncFails(t *testing.T) {
    mgr, rt, lifecycle := newFailingFinalSyncManager(t)
    sb := createEphemeralSyncWorkspace(t, mgr)
    err := mgr.Destroy(context.Background(), sb.ID)
    require.ErrorIs(t, err, ErrSandboxCleanupPending)
    assert.False(t, rt.wasRemoved(sb.RuntimeID))
    assert.True(t, lifecycle.exists(sb.ID))
    assert.True(t, mgr.workspaceOwnerExists(sb.Workspace.RootPath+"/"))
}

func TestStartupFinalizesEphemeralFUSEWithoutRestoringUserSession(t *testing.T) {
    first, shared := createCrashedEphemeralFUSEFixture(t)
    first.crashWithoutCleanup()
    restored := newManagerFromSharedState(t, shared)
    require.NoError(t, restored.Start(context.Background()))
    assert.False(t, restored.hasSandbox(shared.sandboxID))
    assert.False(t, shared.ephemeral.exists(shared.sandboxID))
    assert.False(t, shared.runtime.exists(shared.runtimeUID))
}
```

Add cases for retrying sync success, FUSE durable-flush failure, runtime removal ambiguity, release CAS conflict, persistent sync shutdown, and orphan reconciliation including ephemeral runtime UIDs.

- [ ] **Step 2: Run finalization tests and confirm failure**

Run: `go test ./internal/sandbox ./internal/api/handler -run 'Test.*(FinalSync|Finaliz|CleanupPending|EphemeralFUSE)' -count=1`

Expected: FAIL because sync destroy ignores `syncFromContainer` errors and startup ignores ephemeral records.

- [ ] **Step 3: Implement an idempotent finalization state machine**

Add `ErrSandboxCleanupPending` and map it to HTTP 503 with code `SANDBOX_CLEANUP_PENDING`. Finalization order is fixed:

```text
sync: close gate -> final sync-out -> remove runtime -> release lease -> remove lifecycle/session state
fuse: close gate -> quiesce -> durable flush -> shutdown/unmount -> remove runtime -> release lease -> remove Pool record -> remove lifecycle/session state
```

Each completed external boundary must transition the stored record before proceeding; retry uses the stored state and exact runtime/owner identity. Never remove a runtime or lease after a failed/ambiguous earlier boundary. `Destroy` returns cleanup pending while retaining the in-memory sandbox in `StateDestroying`; the background reconciler retries with bounded backoff.

At startup, restore `StateReady` persistent sessions for users; a persistent session already in `StateDestroying` resumes finalization instead of being recreated or published. Then list ephemeral lifecycle records and run only their finalization. Include all of their RuntimeUIDs in the protected set before runtime orphan reconciliation. A malformed or unreadable lifecycle record aborts startup fail-closed. No ephemeral record is inserted into `m.sandboxes` for public lookup.

- [ ] **Step 4: Run sandbox, handler, and race tests**

Run:

```bash
go test ./internal/sandbox ./internal/api/handler -count=1
go test -race ./internal/sandbox -run 'Test.*(Destroy|Finaliz|Ephemeral|Lease)' -count=1
```

Expected: PASS; no test observes a deleted runtime after failed final sync or an exposed recovered ephemeral sandbox.

- [ ] **Step 5: Commit strong finalization**

```bash
git add internal/sandbox internal/api/handler
git commit -m "fix: make workspace finalization crash safe"
```

## Task 8: Drain all active state and expose a fail-closed release audit

**Files:**

- Create: `internal/sandbox/drain_audit.go`
- Create: `internal/sandbox/drain_audit_test.go`
- Modify: `internal/sandbox/manager.go`
- Modify: `internal/sandbox/manager_test.go`
- Modify: `cmd/sandbox/drain.go`
- Modify: `cmd/sandbox/drain_test.go`
- Modify: `cmd/sandbox/main.go`
- Modify: `cmd/sandbox/main_test.go`

- [ ] **Step 1: Write failing full-drain and audit tests**

```go
func TestManagerStopPreservesPersistentAndFinalizesEphemeral(t *testing.T) {
    mgr, state := newMixedManagerForShutdown(t)
    require.NoError(t, mgr.Stop(context.Background()))
    assert.Equal(t, 2, state.persistentSessions())
    assert.Equal(t, 2, state.persistentRuntimes())
    assert.Zero(t, state.ordinaryPoolSize())
    assert.Zero(t, state.ephemeralRecords())
}

func TestManagerDrainReleaseFinalizesAllModesAndPools(t *testing.T) {
    mgr, state := newMixedManagerForShutdown(t)
    require.NoError(t, mgr.DrainRelease(context.Background()))
    assert.Zero(t, state.activeSandboxes())
    assert.Zero(t, state.persistentSessions())
    assert.Zero(t, state.ordinaryPoolSize())
    assert.Zero(t, state.fusePoolRecords())
    assert.Zero(t, state.workspaceOwners())
    assert.Zero(t, state.ephemeralRecords())
}

func TestDrainAuditAllowsGenerationCountersButRejectsLifecycleState(t *testing.T) {
    store := newAtomicMemoryStore()
    require.NoError(t, store.Set(context.Background(), "sandbox:workspace:generation:stable", []byte("9"), 0))
    require.NoError(t, AuditDrainedState(context.Background(), store))
    require.NoError(t, store.Set(context.Background(), "sandbox:ephemeral:v1:active", []byte("{}"), 0))
    require.ErrorContains(t, AuditDrainedState(context.Background(), store), "ephemeral")
}
```

Add drain command tests that reject any managed Pod, NetworkPolicy, CiliumNetworkPolicy, session, owner, active lease, ephemeral record, or FUSE pool record. Unrelated namespace resources and workspace generation counters must not block.

- [ ] **Step 2: Run shutdown/drain tests and confirm failure**

Run: `go test ./internal/sandbox ./cmd/sandbox -run 'Test.*(StopPreserves|DrainRelease|DrainAudit|DrainKubernetes)' -count=1`

Expected: FAIL because `Manager.Stop` has no error result, there is no separate release-wide drain, and the drain command only waits for API replicas.

- [ ] **Step 3: Implement full Manager shutdown and post-scale audit**

Change `Manager.Stop(context.Context) error` into the normal rolling-shutdown path. It must reject new creates, wait for in-flight creates, finalize ephemeral workspaces, preserve persistent sandboxes and their session/owner/lease records for a new replica, stop this replica's FUSE Pool maintenance, drain its ordinary prepared Pool, and join failures. Add `Manager.DrainRelease(context.Context) error` for backend switching and Helm deletion; it restores both lifecycle namespaces, finalizes every persistent and ephemeral sandbox, drains both Pools, and runs the zero-state audit. Callers log errors and never claim a successful drain when either method is non-nil.

Implement:

```go
var blockingDrainPrefixes = []string{
    "sandbox:session:v2:*",
    "sandbox:ephemeral:v1:*",
    "sandbox:workspace:lease:*",
    "sandbox:workspace:owner:*",
}

func AuditDrainedState(ctx context.Context, store state.Store) error
```

The audit lists each explicit prefix plus read-only `fusepool:*`, and never uses broad deletion. The Kubernetes drain command first disables HPA, scales the API Deployment to zero, waits for its Pods to terminate, then constructs a drain-only Manager from the current Chart image/config, calls `DrainRelease`, and verifies zero managed runtime Pods and dynamic policies in the runtime namespace plus zero blocking Redis state. It reports findings but does not force-delete unknown resources.

- [ ] **Step 4: Run shutdown, race, and full Go tests**

Run:

```bash
go test ./internal/sandbox ./cmd/sandbox -count=1
go test -race ./internal/sandbox -run 'TestManager(Stop|DrainRelease)' -count=1
go test ./... -count=1
```

Expected: PASS; timeout/failure tests return a non-nil drain error and leave evidence intact.

- [ ] **Step 5: Commit audited drain support**

```bash
git add internal/sandbox/drain_audit.go internal/sandbox/drain_audit_test.go internal/sandbox/manager.go internal/sandbox/manager_test.go cmd/sandbox
git commit -m "feat: audit complete sandbox release drains"
```

## Task 9: Make Helm preset-driven and guard backend changes

**Files:**

- Create: `deploy/helm/sandbox/templates/_helpers.tpl`
- Create: `deploy/helm/sandbox/templates/backend-fingerprint.yaml`
- Create: `deploy/helm/sandbox/templates/pre-backend-change-drain.yaml`
- Create: `deploy/helm/sandbox/values.schema.json`
- Create: `scripts/test-helm-backend-switch.sh`
- Modify: `deploy/helm/sandbox/values.yaml`
- Modify: `deploy/helm/sandbox/Chart.yaml`
- Modify: `deploy/helm/sandbox/templates/deployment.yaml`
- Modify: `deploy/helm/sandbox/templates/rbac.yaml`
- Modify: `deploy/helm/sandbox/templates/pre-delete-drain.yaml`
- Modify: `testdata/values-fuse-minio.yaml`
- Modify: `testdata/values-fuse-obs-public.yaml`
- Modify: `testdata/values-fuse-obs-private.yaml`
- Modify: `scripts/test-helm-chart.sh`

- [ ] **Step 1: Write failing render tests for all presets and both modes**

Extend `scripts/test-helm-chart.sh` to render each overlay and assert:

```bash
grep -Fq 'name: SANDBOX_WORKSPACE_DEFAULT_MOUNT_MODE' <<<"$rendered"
grep -Fq 'value: "sync"' <<<"$rendered"
grep -Fq 'name: SANDBOX_WORKSPACE_ENABLED_MOUNT_MODES' <<<"$rendered"
grep -Fq 'value: "sync,fuse"' <<<"$rendered"
grep -Fq 'name: SANDBOX_WORKSPACE_BACKEND_PRESET' <<<"$rendered"
grep -Fq 'sandbox.huaxisy.com/backend-fingerprint:' <<<"$rendered"
! grep -Fq 'SANDBOX_WORKSPACE_PROVIDERS_' <<<"$rendered"
! grep -Fq '0.0.0.0/0' <<<"$rendered"
```

For MinIO assert the path-style endpoint FQDN only. For both OBS presets assert the base endpoint plus the bucket-prefixed virtual-host endpoint. Assert public/private overlays use the same mounter digest and the same Docker FUSE digest. `helm lint` must reject an unknown preset or a default mode outside the enabled list.

- [ ] **Step 2: Run Helm tests and confirm failure**

Run: `./scripts/test-helm-chart.sh`

Expected: FAIL because the Chart still exposes provider maps and one global `workspace.mode`.

- [ ] **Step 3: Implement helpers, schema, values, and backend fingerprint resources**

The main values shape is:

```yaml
config:
  storage:
    filesystem:
      preset: minio
      bucket: sandbox-storage
      region: us-east-1
      endpoint: minio.example.com
      storageIdentity: production-object-store
      credentialGeneration: rotation-1
      caSecretKey: ""
      endpointHostIPs: []
      systemEgressMode: cidr
      dnsCIDRs: ["1.1.1.1/32"]
      systemEgressCIDRs: ["192.0.2.10/32"]
      endpointPorts: [443]
  workspace:
    defaultMountMode: sync
    enabledMountModes: [sync, fuse]
    fuseImages:
      mounter: registry.i.huaxisy.com/library/ai-infra/sandbox-fuse-mounter@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa
      docker: registry.i.huaxisy.com/library/ai-infra/sandbox-fuse-docker@sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb
```

`_helpers.tpl` maps exactly three preset values to provider/profile and fails on any other input. It derives OBS virtual-host FQDNs and never permits arbitrary s3fs options. The fingerprint applies `toJson` and `sha256sum` to a dictionary containing preset, provider, profile, storage identity, endpoint, region, bucket, API/runtime Secret names, credential generation, CA key, endpoint host IPs, system-egress mode/FQDN/CIDR/ports, and the two common image digests. It contains no AK/SK or Secret data.

Render the fingerprint on a ConfigMap annotation and on the API Pod template. The pre-upgrade/pre-rollback template uses `lookup` to read the installed ConfigMap; a missing or different old annotation renders the current Chart image with Redis/runtime configuration and `--drain-release`. The command scales the old API to zero before recovering and draining its state. The same fingerprint renders no backend drain Job. Keep the pre-delete hook independent, but make it invoke the same `--drain-release` command before Redis can be deleted.

- [ ] **Step 4: Run render/lint tests, then exercise the hook in kind**

Run:

```bash
./scripts/test-helm-chart.sh
./scripts/test-helm-backend-switch.sh
```

Expected: PASS. The kind script proves an image-only upgrade preserves active lifecycle state, while a changed storage identity triggers scale-to-zero and refuses the upgrade until runtime and Redis state are empty.

- [ ] **Step 5: Commit Helm presets and guard**

```bash
git add deploy/helm/sandbox testdata/values-fuse-minio.yaml testdata/values-fuse-obs-public.yaml testdata/values-fuse-obs-private.yaml scripts/test-helm-chart.sh scripts/test-helm-backend-switch.sh
git commit -m "feat: guard helm workspace backend presets"
```

## Task 10: Update Docker Compose and operator configuration for one hybrid backend

**Files:**

- Modify: `docker/docker-compose.yml`
- Modify: `docker/.env.example`
- Modify: `configs/config.yaml`
- Modify: `scripts/test-fuse-images.sh`

- [ ] **Step 1: Add failing Compose/config contract assertions**

Add shell assertions that Compose exposes one preset/backend and common images, and removes duplicated MinIO/OBS provider maps:

```bash
grep -Fq 'SANDBOX_WORKSPACE_DEFAULT_MOUNT_MODE=${WORKSPACE_DEFAULT_MOUNT_MODE:-sync}' "$compose"
grep -Fq 'SANDBOX_WORKSPACE_ENABLED_MOUNT_MODES=${WORKSPACE_ENABLED_MOUNT_MODES:-sync}' "$compose"
grep -Fq 'SANDBOX_WORKSPACE_BACKEND_PRESET=${STORAGE_PRESET:-minio}' "$compose"
grep -Fq 'SANDBOX_WORKSPACE_BACKEND_MOUNTER_IMAGE=${FUSE_MOUNTER_IMAGE:-}' "$compose"
grep -Fq 'SANDBOX_WORKSPACE_BACKEND_DOCKER_IMAGE=${FUSE_SANDBOX_IMAGE:-}' "$compose"
! grep -Fq 'SANDBOX_WORKSPACE_PROVIDERS_MINIO_' "$compose"
! grep -Fq 'SANDBOX_WORKSPACE_PROVIDERS_OBS_' "$compose"
```

- [ ] **Step 2: Run the image/Compose contract and confirm failure**

Run: `./scripts/test-fuse-images.sh`

Expected: FAIL on the old mode and duplicated provider environment variables.

- [ ] **Step 3: Simplify Compose and the example environment**

Use `STORAGE_PRESET=minio|huawei-obs-public|huawei-obs-private`, one storage endpoint/bucket/identity/generation, and the two runtime-specific common FUSE image digests. Default to `WORKSPACE_DEFAULT_MOUNT_MODE=sync` and `WORKSPACE_ENABLED_MOUNT_MODES=sync`; enabling FUSE uses the literal comma-separated `sync,fuse`.

The `sandbox-images` helper must pull/build each common FUSE image once. Do not alter the Docker daemon, restart Docker Desktop, or introduce a host `/workspace` mount. Keep the root-only credential staging directory behavior unchanged.

- [ ] **Step 4: Run Compose rendering and image contract tests**

Run:

```bash
docker compose --env-file docker/.env.example -f docker/docker-compose.yml config >/dev/null
./scripts/test-fuse-images.sh
```

Expected: PASS without reading or printing `docker/.env`.

- [ ] **Step 5: Commit operator configuration**

```bash
git add docker/docker-compose.yml docker/.env.example configs/config.yaml scripts/test-fuse-images.sh
git commit -m "chore: simplify hybrid workspace configuration"
```

## Task 11: Expand integration coverage across lifecycle modes, mount modes, runtimes, and backends

**Files:**

- Modify: `test/integration/workspacefuse/workspace_fuse_test.go`
- Modify: `scripts/workspace-fuse-preflight.sh`
- Modify: `scripts/workspace-fuse-matrix.sh`
- Modify: `scripts/test-workspace-fuse-matrix.sh`

- [ ] **Step 1: Add failing matrix assertions**

The script-level test must require these API paths for every enabled backend:

```text
ephemeral + no workspace
ephemeral + sync workspace
persistent + sync workspace
ephemeral + FUSE workspace
persistent + FUSE workspace
same-prefix sync/FUSE conflict
different-prefix sync/FUSE concurrency
```

Keep the six provider/runtime combinations: MinIO, Huawei public OBS, Huawei private OBS × Kubernetes, Docker. Require one common Kubernetes mounter digest and one common Docker FUSE digest across all three profile evidence files.

- [ ] **Step 2: Run the matrix unit test and confirm failure**

Run: `./scripts/test-workspace-fuse-matrix.sh`

Expected: FAIL because the current test only counts six profile/runtime calls and does not check hybrid lifecycle cases or common digests.

- [ ] **Step 3: Implement API-driven hybrid integration cases**

Extend the Go integration test to create sandboxes only through sandbox-api. For sync, write a file, call `SyncWorkspace(from_container)`, destroy, recreate, and verify content. For FUSE, write, call durable flush, destroy, recreate, and verify content. For ephemeral final-sync, omit manual sync and verify destruction persists content. For no workspace, verify object-store marker/list counters remain unchanged. Conflict tests must assert HTTP 409 for the second same-prefix request regardless of mode order.

The preflight script supplies `workspace_mount_mode` explicitly for FUSE and omits it for default sync. Evidence JSON records both common image digests, preset, runtime, and all seven API-path results without credentials.

- [ ] **Step 4: Run local unit/integration gates**

Run:

```bash
./scripts/test-workspace-fuse-matrix.sh
go test ./test/integration/workspacefuse -count=1
WORKSPACE_FUSE_RUN_INTEGRATION=1 ./scripts/workspace-fuse-preflight.sh docker --profile testdata/fuse/profiles/minio-sigv4-path-style-v1.yaml
```

Expected: PASS against the existing local MinIO through the project Compose startup path. Do not restart the Docker daemon.

- [ ] **Step 5: Commit the hybrid matrix**

```bash
git add test/integration/workspacefuse/workspace_fuse_test.go scripts/workspace-fuse-preflight.sh scripts/workspace-fuse-matrix.sh scripts/test-workspace-fuse-matrix.sh
git commit -m "test: cover hybrid workspace lifecycle matrix"
```

## Task 12: Update deployment documentation and perform final verification

**Files:**

- Modify: `docs/deployment/workspace-fuse.md`
- Modify: `README.md`
- Modify: `docs/superpowers/specs/2026-09-01-workspace-container-fuse-mount-design.md` only if implementation names differ from the approved names

- [ ] **Step 1: Update the deployment runbook**

Document:

- the three preset values and the rule that one release selects exactly one;
- the common Kubernetes/Docker FUSE images and independent profile evidence;
- default sync plus explicit request-level FUSE examples;
- ordinary and FUSE Pool sizing under sandbox-api ownership;
- ephemeral no-workspace, sync finalization, and FUSE finalization behavior;
- Secret/credential-generation rotation;
- backend fingerprint drain, unchanged-fingerprint upgrade, failed drain recovery, and the prohibition on direct rollback to a pre-guard Chart;
- Docker Compose startup and kind/`ds-ai-research` Helm deployment commands;
- preservation of “public internet allowed, private networks blocked unless explicitly whitelisted” for user traffic;
- trusted mounter system egress as a separate narrowly scoped path;
- an explicit warning not to restart Docker merely to recover an application deployment.

- [ ] **Step 2: Run static and unit verification**

Run:

```bash
gofmt -w internal cmd pkg
go test ./... -count=1
(cd sdk/go && go test ./... -count=1)
./scripts/test-fuse-images.sh
./scripts/test-helm-chart.sh
./scripts/test-workspace-fuse-matrix.sh
git diff --check
```

Expected: every command exits 0.

- [ ] **Step 3: Run Docker API-driven MinIO smoke verification**

Run the project deployment without printing `.env`:

```bash
docker compose --env-file docker/.env -f docker/docker-compose.yml up -d --build sandbox-images redis sandbox-api
docker compose --env-file docker/.env -f docker/docker-compose.yml ps
WORKSPACE_FUSE_RUN_INTEGRATION=1 ./scripts/workspace-fuse-preflight.sh docker --profile testdata/fuse/profiles/minio-sigv4-path-style-v1.yaml
```

Expected: API, Redis, and required helpers are healthy; default sync and explicit FUSE both pass through sandbox-api.

- [ ] **Step 4: Run Kubernetes verification in order**

First run kind with the local registry. Then push architecture-correct common images under `registry.i.huaxisy.com/library/ai-infra/sandbox-fuse-*`, deploy namespace `sandbox-fuse` in `ds-ai-research`, and run MinIO, Huawei private OBS, and Huawei public OBS presets one at a time. For each preset, verify both sync and FUSE before switching. Use the backend-change hook between presets and confirm the prior release is fully drained. Leave the final user-requested preset running for inspection.

Expected: all six runtime/profile combinations pass, the two modes coexist in each deployment, no managed resource or blocking Redis state from the prior backend remains, and user network-policy probes still allow public access while denying private addresses without a whitelist.

- [ ] **Step 5: Commit documentation and final evidence**

```bash
git add docs/deployment/workspace-fuse.md README.md docs/superpowers/specs/2026-09-01-workspace-container-fuse-mount-design.md
git commit -m "docs: document hybrid workspace deployment"
git status --short --branch
```

Expected: the branch is clean and only ahead of its remote.

## Self-review coverage map

- Common images and strict multi-profile bundle: Task 1.
- Fixed single-backend preset and operator simplicity: Tasks 2, 9, 10.
- Request-level sync/FUSE with both Pools maintained by sandbox-api: Tasks 3 and 4.
- Same-prefix sync/FUSE exclusion: Task 5.
- Persistent versus ephemeral state separation: Task 6.
- Strong final sync/flush and restart cleanup: Task 7.
- Backend-change drain and zero-state gate: Tasks 8 and 9.
- Docker and Kubernetes plus all three storage profiles: Task 11 and Task 12.
- Existing public/private network policy semantics: Tasks 9, 11, and Task 12.
- Deployment documentation: Task 12.
