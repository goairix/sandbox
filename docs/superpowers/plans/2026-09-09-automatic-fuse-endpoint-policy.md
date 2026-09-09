# Automatic Workspace FUSE Endpoint Policy Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Reduce normal Docker Compose and Helm operation to object-storage configuration while deriving region, endpoint addresses, ports, and fail-closed FUSE system egress inside sandbox-api.

**Architecture:** Configuration normalization derives a missing MinIO/OBS region and selects the fixed compiled profile. A common endpoint-policy resolver turns the trusted release endpoint into canonical URLs, exact host/IP mappings, and exact ports; Docker and Kubernetes invoke it for every prepared FUSE shell and store only the derived non-secret policy needed for recovery. Operator-facing Compose/Helm inputs no longer expose credential files, custom workspace CA, legacy mode, durability-test, DNS, FQDN, CIDR, host-IP, port, or egress-mode fields.

**Tech Stack:** Go 1.25, `net.Resolver`, Docker Engine API, Kubernetes client-go/NetworkPolicy, Helm, Docker Compose, s3fs/FUSE, Bash contract tests.

**Spec:** `docs/superpowers/specs/2026-09-09-automatic-fuse-endpoint-policy-design.md`

**Execution constraint:** Implement inline in the current session. Do not use subagents.

---

### Task 1: Normalize region and remove operator-owned endpoint policy

**Files:**
- Modify: `internal/config/config.go`
- Modify: `internal/config/config_test.go`
- Modify: `cmd/sandbox/main.go`
- Modify: `cmd/sandbox/main_test.go`

- [ ] **Step 1: Write failing configuration tests**

Add table cases proving MinIO defaults an empty region, both standard OBS endpoints derive their region, explicit canonical region wins, and non-standard OBS without a region fails with one actionable error:

```go
func TestNormalizeFUSERegion(t *testing.T) {
    tests := []struct{ preset, endpoint, region, want string }{
        {"minio", "minio.example.com", "", "us-east-1"},
        {"huawei-obs-public", "https://obs.cn-southwest-2.myhuaweicloud.com", "", "cn-southwest-2"},
        {"huawei-obs-private", "https://obs.cn-southwest-268.shuanghuayun.com", "", "cn-southwest-268"},
        {"minio", "minio.example.com", "cn-local-1", "cn-local-1"},
    }
    for _, tt := range tests {
        t.Run(tt.preset, func(t *testing.T) {
            cfg := validPresetFUSEConfig(tt.preset, tt.endpoint)
            cfg.Storage.FileSystem.Region = tt.region
            require.NoError(t, cfg.Validate())
            assert.Equal(t, tt.want, cfg.Storage.FileSystem.Region)
        })
    }
}
```

Add a valid FUSE configuration with every former system-egress field empty. Assert validation does not mention `system_egress_cidrs`, `dns_cidrs`, `endpoint_host_ips`, `endpoint_ports`, `workspace.secret_name`, or `allow_unverified_durable_flush`.

- [ ] **Step 2: Run the tests and verify RED**

Run:

```bash
go test -count=1 ./internal/config ./cmd/sandbox
```

Expected: FAIL because empty region and empty system-egress inputs are currently rejected.

- [ ] **Step 3: Implement configuration normalization**

Add a normalization helper called by `Validate` after backend preset selection:

```go
func (c *Config) normalizeFUSERegion() error {
    if !c.Workspace.MountModeEnabled("fuse") || c.Storage.FileSystem.Region != "" {
        return nil
    }
    switch c.Workspace.Backend.Preset {
    case "minio":
        c.Storage.FileSystem.Region = "us-east-1"
        return nil
    case "huawei-obs-public", "huawei-obs-private":
        region, err := regionFromOBSEndpoint(c.Storage.FileSystem.Endpoint)
        if err != nil {
            return fmt.Errorf("config: storage.filesystem.region is required for a non-standard OBS endpoint: %w", err)
        }
        c.Storage.FileSystem.Region = region
        return nil
    default:
        return fmt.Errorf("config: unsupported workspace backend preset %q", c.Workspace.Backend.Preset)
    }
}
```

Remove operator validation/default wiring for `Workspace.SecretName`, `Workspace.AllowUnverifiedDurableFlush`, and the backend CA/system-egress fields. `buildFUSESpec` must build only the logical endpoint contract; it must not copy operator-provided IP/CIDR/FQDN/DNS/port values.

- [ ] **Step 4: Run the tests and verify GREEN**

Run:

```bash
go test -count=1 ./internal/config ./cmd/sandbox
```

Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/config cmd/sandbox
git commit -m "feat: derive fuse backend region"
```

---

### Task 2: Add a common trusted endpoint-policy resolver

**Files:**
- Create: `internal/runtime/endpoint_policy.go`
- Create: `internal/runtime/endpoint_policy_test.go`
- Modify: `internal/runtime/types.go`
- Modify: `internal/runtime/types_test.go`

- [ ] **Step 1: Write failing resolver tests**

Define a fake lookup function and tests for public TLS MinIO, private HTTP MinIO, both OBS host shapes, forbidden addresses, public HTTP rejection, and deterministic sorting:

```go
type LookupNetIPFunc func(context.Context, string, string) ([]net.IP, error)

func TestResolveFUSEEndpointPolicy(t *testing.T) {
    lookup := func(_ context.Context, network, host string) ([]net.IP, error) {
        values := map[string][]net.IP{
            "minio.example.com": {net.ParseIP("36.170.50.43")},
            "obs.cn-southwest-2.myhuaweicloud.com": {net.ParseIP("203.0.113.10")},
            "bucket.obs.cn-southwest-2.myhuaweicloud.com": {net.ParseIP("203.0.113.11")},
        }
        return values[host], nil
    }
    resolved, err := ResolveFUSEEndpointPolicy(context.Background(), spec, lookup, IPv4Only)
    require.NoError(t, err)
    assert.Equal(t, []string{"36.170.50.43/32"}, resolved.SystemEgress.EndpointCIDRs)
    assert.Equal(t, []int32{443}, resolved.SystemEgress.EndpointPorts)
}
```

Private HTTP MinIO must accept `10.20.30.40`, derive port 80 or the explicit endpoint port, and reject any lookup containing a public address. All profiles must reject loopback, link-local, multicast and unspecified results.

- [ ] **Step 2: Run the tests and verify RED**

Run:

```bash
go test -count=1 ./internal/runtime
```

Expected: FAIL because the resolver and resolved host mapping types do not exist.

- [ ] **Step 3: Implement the resolver and internal policy types**

Replace the operator-shaped `SystemEgressSpec` with an internal resolved contract:

```go
type EndpointHostMapping struct {
    Host string   `json:"host"`
    IPs  []string `json:"ips"`
}

type SystemEgressSpec struct {
    Hosts         []EndpointHostMapping `json:"hosts"`
    EndpointCIDRs []string              `json:"endpoint_cidrs"`
    EndpointPorts []int32               `json:"endpoint_ports"`
}
```

`ResolveFUSEEndpointPolicy` must clone rather than mutate its input, canonicalize MinIO/OBS endpoints, derive the exact hostname set, call only the injected resolver, filter by runtime address family, reject forbidden addresses, enforce private-only addresses for HTTP MinIO, and populate deterministic host mappings/CIDRs/ports. It must never resolve or log AK/SK.

- [ ] **Step 4: Run the tests and verify GREEN**

Run:

```bash
go test -race -count=1 ./internal/runtime
```

Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/runtime/endpoint_policy.go internal/runtime/endpoint_policy_test.go internal/runtime/types.go internal/runtime/types_test.go
git commit -m "feat: derive exact fuse endpoint policy"
```

---

### Task 3: Support private HTTP MinIO with an internal profile

**Files:**
- Modify: `internal/config/config.go`
- Modify: `internal/config/config_test.go`
- Modify: `internal/mounter/profile.go`
- Modify: `internal/mounter/profile_test.go`
- Modify: `internal/mounter/manifest.go`
- Modify: `internal/mounter/manifest_test.go`
- Modify: `docker/images/workspace-mounter/profile-bundle.json`
- Modify: `scripts/test-fuse-images.sh`

- [ ] **Step 1: Write failing profile tests**

Add a compiled profile selected internally when preset is MinIO and `use_ssl=false`:

```go
const minioPrivateHTTPProfile = "minio-sigv4-path-style-private-http-v1"

func TestCompiledPrivateHTTPMinIOProfile(t *testing.T) {
    profile, ok := LookupCompiledProfile("minio", minioPrivateHTTPProfile)
    require.True(t, ok)
    assert.False(t, profile.Descriptor.TLSRequired)
    options, err := profile.Options(fuseprotocol.BootstrapConfig{
        Provider: "minio", Endpoint: "http://minio.internal:9000", Region: "us-east-1",
    })
    require.NoError(t, err)
    assert.Contains(t, options, "url=http://minio.internal:9000")
}
```

Assert the HTTPS MinIO profile still rejects HTTP, OBS still rejects HTTP, and config automatically selects the private HTTP profile without an operator profile field.

- [ ] **Step 2: Run the tests and verify RED**

Run:

```bash
go test -count=1 ./internal/config ./internal/mounter
```

Expected: FAIL because only the TLS MinIO profile exists and the bundle rejects every non-TLS descriptor.

- [ ] **Step 3: Implement the private HTTP profile**

Add the fourth fixed profile to the compiled catalog and bundle. Keep SigV4, path-style, region endpoint and durable flush behavior identical to the TLS MinIO profile; only the canonical scheme contract differs. Permit `TLSRequired=false` in bundle validation only for the exact private HTTP profile ID. Configuration normalization selects HTTPS or private-HTTP MinIO profile solely from `STORAGE_USE_SSL`.

The runtime endpoint resolver from Task 2 remains the authority that proves the HTTP endpoint resolves only to private addresses; profile selection alone must not make public HTTP valid.

- [ ] **Step 4: Run the tests and verify GREEN**

Run:

```bash
go test -count=1 ./internal/config ./internal/mounter
./scripts/test-fuse-images.sh
```

Expected: PASS and the static bundle contract reports four compiled profiles.

- [ ] **Step 5: Commit**

```bash
git add internal/config internal/mounter docker/images/workspace-mounter/profile-bundle.json scripts/test-fuse-images.sh
git commit -m "feat: support private http minio fuse"
```

---

### Task 4: Resolve and pin endpoint policy in both runtimes

**Files:**
- Modify: `internal/runtime/docker/runtime.go`
- Modify: `internal/runtime/docker/runtime_test.go`
- Modify: `internal/runtime/docker/container.go`
- Modify: `internal/runtime/docker/container_test.go`
- Modify: `internal/runtime/docker/network.go`
- Modify: `internal/runtime/docker/network_test.go`
- Modify: `internal/runtime/kubernetes/runtime.go`
- Modify: `internal/runtime/kubernetes/runtime_test.go`
- Modify: `internal/runtime/kubernetes/pod.go`
- Modify: `internal/runtime/kubernetes/pod_test.go`
- Modify: `internal/runtime/kubernetes/network.go`
- Modify: `internal/runtime/kubernetes/network_test.go`
- Modify: `internal/sandbox/fuse_pool.go`
- Modify: `internal/sandbox/fuse_pool_test.go`

- [ ] **Step 1: Write failing runtime tests**

For Docker and Kubernetes, inject a fake `LookupNetIPFunc`, prepare a FUSE shell from a logical spec with no IP/CIDR/DNS/port fields, and assert:

```go
assert.Equal(t, []string{"36.170.50.43/32"}, createdPolicy.EndpointCIDRs)
assert.Equal(t, []int32{443}, createdPolicy.EndpointPorts)
assert.Contains(t, createdHostMappings, runtime.EndpointHostMapping{
    Host: "minio.example.com", IPs: []string{"36.170.50.43"},
})
```

Add an authorize test where DNS changes after prepare. It must return a stable endpoint-policy-changed error before sending mounter authorization; the existing manager failure path must destroy the single-use shell and schedule refill.

Update PoolKey tests so changes to logical endpoint/TLS/region/port alter the key, while reordered or changed derived IP mappings do not.

- [ ] **Step 2: Run the tests and verify RED**

Run:

```bash
go test -count=1 ./internal/runtime/docker ./internal/runtime/kubernetes ./internal/sandbox
```

Expected: FAIL because runtimes still require operator-provided `SystemEgressSpec` and DNS rules.

- [ ] **Step 3: Integrate resolver in Docker**

Give Docker runtime a default `net.DefaultResolver.LookupNetIP` dependency and a test-only option. At the start of `PrepareSandbox`, resolve a local spec copy. Build gateway iptables from exact endpoint CIDRs/ports only; remove DNS rules and configured resolvers. Build `ExtraHosts` from all `EndpointHostMapping` entries. Persist canonical resolved policy in the existing recovery label and runtime state.

Before `AuthorizeWorkspaceMount`, re-resolve the stored logical endpoint and compare canonical host mappings. On change, return `runtime.ErrWorkspaceEndpointPolicyChanged` without starting s3fs.

- [ ] **Step 4: Integrate resolver in Kubernetes**

Resolve a local spec copy before building either Pod or policy. Always create the standard exact-CIDR NetworkPolicy for FUSE system egress; remove the FUSE-specific Cilium FQDN branch and DNS egress rules. Build `HostAliases` from all resolved host mappings. Keep the unrelated Cilium user-network private-deny behavior unchanged.

Persist the canonical logical/resolved endpoint contract in protected annotations for recovery and perform the same pre-authorize refresh check as Docker.

- [ ] **Step 5: Simplify PoolKey projection**

Remove custom-CA and derived endpoint-policy fields from `fusePoolKeyProjection`. Keep preset-selected profile, logical endpoint, bucket, canonical region, TLS, storage identity, credential generation, images, cache and security resources. Derived DNS results must not force an operator-visible backend generation change.

- [ ] **Step 6: Run the tests and verify GREEN**

Run:

```bash
go test -race -count=1 ./internal/runtime/docker ./internal/runtime/kubernetes ./internal/sandbox
```

Expected: PASS.

- [ ] **Step 7: Commit**

```bash
git add internal/runtime internal/sandbox
git commit -m "feat: resolve fuse endpoint policy in runtimes"
```

---

### Task 5: Remove obsolete credential and CA staging paths

**Files:**
- Modify: `cmd/sandbox/main.go`
- Modify: `cmd/sandbox/main_test.go`
- Modify: `internal/runtime/types.go`
- Modify: `internal/runtime/docker/runtime.go`
- Delete: `internal/runtime/docker/secrets.go`
- Delete: `internal/runtime/docker/secrets_test.go`
- Modify: `internal/runtime/docker/container.go`
- Modify: `internal/runtime/kubernetes/pod.go`
- Modify: `internal/fuseprotocol/protocol.go`
- Modify: `internal/fuseprotocol/protocol_test.go`
- Modify: `internal/mounter/supervisor.go`
- Modify: `internal/mounter/supervisor_test.go`

- [ ] **Step 1: Write failing absence tests**

Assert `WorkspaceFUSESpec` and bootstrap JSON have no Secret name, CA key, CA path or staging-root fields; Docker/Kubernetes prepared resource serialization must contain none of the old workspace Secret paths.

- [ ] **Step 2: Run the tests and verify RED**

Run:

```bash
go test -count=1 ./internal/fuseprotocol ./internal/mounter ./internal/runtime/docker ./internal/runtime/kubernetes ./cmd/sandbox
```

Expected: FAIL because optional custom-CA/staging code still exists.

- [ ] **Step 3: Remove the obsolete paths**

Delete workspace CA materialization, secret-root labels, binds, volumes, constructor variants and bootstrap CA fields. Keep AK/SK delivery exclusively in the authorize stdin path implemented by the inline-credential design. The generic system CA store remains the only HTTPS trust source.

- [ ] **Step 4: Run the tests and verify GREEN**

Run:

```bash
go test -race -count=1 ./internal/fuseprotocol ./internal/mounter ./internal/runtime/docker ./internal/runtime/kubernetes ./cmd/sandbox
```

Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add cmd/sandbox internal/fuseprotocol internal/mounter internal/runtime
git commit -m "refactor: remove fuse secret staging"
```

---

### Task 6: Simplify Compose, Helm and deployment documentation

**Files:**
- Modify: `docker/docker-compose.yml`
- Modify: `docker/.env.example`
- Modify: `deploy/helm/sandbox/values.yaml`
- Modify: `deploy/helm/sandbox/values.schema.json`
- Modify: `deploy/helm/sandbox/templates/_helpers.tpl`
- Modify: `deploy/helm/sandbox/templates/deployment.yaml`
- Modify: `deploy/helm/sandbox/templates/pre-backend-change-drain.yaml`
- Modify: `deploy/helm/sandbox/templates/pre-delete-drain.yaml`
- Modify: `testdata/values-fuse-minio.yaml`
- Modify: `testdata/values-fuse-obs-public.yaml`
- Modify: `testdata/values-fuse-obs-private.yaml`
- Modify: `scripts/test-inline-credential-config.sh`
- Modify: `scripts/test-helm-chart.sh`
- Modify: `scripts/test-helm-backend-switch.sh`
- Modify: `docs/deployment/docker-compose-deployment-upgrade.md`
- Modify: `docs/deployment/helm-deployment-upgrade.md`
- Modify: `docs/deployment/workspace-fuse.md`

- [ ] **Step 1: Write failing deployment-contract tests**

Define the exact forbidden normal-path names:

```bash
forbidden='WORKSPACE_CREDENTIAL_DIR|WORKSPACE_SECRET_STAGING_ROOT|WORKSPACE_SECRET_NAME|WORKSPACE_CA_SECRET_KEY|WORKSPACE_MODE=|WORKSPACE_ALLOW_UNVERIFIED_DURABLE_FLUSH|FUSE_SYSTEM_EGRESS_MODE|FUSE_DNS_CIDRS|STORAGE_ENDPOINT_HOST_IPS|STORAGE_ENDPOINT_FQDNS|STORAGE_ENDPOINT_CIDRS|STORAGE_ENDPOINT_PORTS|workspaceCA|caSecretKey|endpointHostIPs|systemEgressMode|dnsCIDRs|systemEgressCIDRs|endpointPorts'
```

Fail when any forbidden name appears in `.env.example`, Compose mappings, Helm values/schema/rendered environment, test overlays or current deployment runbooks. Require empty MinIO region to render successfully.

- [ ] **Step 2: Run the tests and verify RED**

Run:

```bash
./scripts/test-inline-credential-config.sh
./scripts/test-helm-chart.sh
./scripts/test-helm-backend-switch.sh
```

Expected: FAIL on the current operator-owned network and CA fields.

- [ ] **Step 3: Remove the fields from deployments**

Compose maps only storage identity/credentials/endpoint/TLS/optional region, workspace modes, Pool tuning and versioned images. Helm values/schema/templates do the same. Backend fingerprint continues to include logical backend fields and excludes resolved IP addresses.

Delete workspace CA Secret volumes/RBAC and Cilium-FQDN selection from the FUSE deployment path. Do not modify the existing user-facing network policy semantics.

- [ ] **Step 4: Rewrite the current runbooks**

Provide one minimal `.env` and one minimal Helm values example. State that region is normally empty, MinIO defaults it, OBS derives it, internal HTTP MinIO is automatically limited to its resolved private IP/port, and no DNS/CIDR/port/CA/staging configuration is required.

- [ ] **Step 5: Run the tests and verify GREEN**

Run:

```bash
docker compose --env-file docker/.env.example -f docker/docker-compose.yml config >/dev/null
./scripts/test-inline-credential-config.sh
./scripts/test-fuse-images.sh
./scripts/test-helm-chart.sh
./scripts/test-helm-backend-switch.sh
git diff --check
```

Expected: PASS.

- [ ] **Step 6: Commit**

```bash
git add docker deploy/helm/sandbox testdata scripts docs/deployment
git commit -m "deploy: derive fuse endpoint policy automatically"
```

---

### Task 7: Full regression and runtime verification

**Files:**
- Modify only files required by a reproduced verification failure.

- [ ] **Step 1: Run repository verification**

```bash
go vet ./...
go test -race -count=1 ./internal/config ./internal/fuseprotocol ./internal/mounter ./internal/runtime ./internal/runtime/docker ./internal/runtime/kubernetes ./internal/sandbox
go test -count=1 ./...
(cd sdk/go && go test -race -count=1 ./...)
./scripts/test-workspace-fuse-matrix.sh
./scripts/test-fuse-images.sh
./scripts/test-helm-chart.sh
./scripts/test-helm-backend-switch.sh
docker compose --env-file docker/.env.example -f docker/docker-compose.yml config >/dev/null
git diff --check
```

Expected: every command exits 0.

- [ ] **Step 2: Verify the four backend/runtime shapes with injected DNS**

Run deterministic integration tests for public HTTPS MinIO, private HTTP MinIO, public OBS and private OBS against both Docker and Kubernetes runtime builders. Assert generated policies contain only resolved host CIDRs and the exact derived port; assert ordinary sync and no-workspace sandboxes are unchanged.

- [ ] **Step 3: Perform real-environment smoke tests only with user-provided release images**

Do not rebuild large images locally and do not restart Docker daemon. After the user publishes images containing these commits, deploy through the normal Compose/Helm path and invoke sandbox-api for sync and FUSE create/exec/read/write/delete/teardown. Validate MinIO, then Huawei private OBS, then Huawei public OBS. Preserve “public internet allowed, private network denied unless whitelisted” tests.

- [ ] **Step 4: Commit only proven fixes**

If verification exposes a defect, reproduce it in an automated test before changing production code, then commit the minimal fix. If no defect is found, do not create an empty verification commit.
