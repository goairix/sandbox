# Inline Workspace Credentials Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Restore configuration-only AK/SK deployment for Docker Compose and Helm while delivering FUSE credentials only through the private one-shot mounter control channel.

**Architecture:** `sandbox-api` loads the existing inline filesystem credentials and gives an owned copy to the selected runtime. Prepared FUSE shells remain credential-free; `AuthorizeWorkspaceMount` adds credentials to the private wire request, and the trusted supervisor creates the s3fs password file only in `/run/s3fs` tmpfs while persisting a credential-free authorization identity. Compose and Helm stop requiring operator-managed credential files and Kubernetes Secrets.

**Tech Stack:** Go 1.25, Docker Engine API, Kubernetes client-go, Helm templates, Docker Compose, s3fs/FUSE, Bash contract tests.

**Spec:** `docs/superpowers/specs/2026-09-09-inline-workspace-credentials-design.md`

## Global Constraints

- Docker reads `STORAGE_ACCESS_KEY` and `STORAGE_SECRET_KEY` from `.env`; Helm reads `config.storage.filesystem.accessKey` and `config.storage.filesystem.secretKey` from values.
- AK/SK must never enter Redis, PoolKey, labels, annotations, events, logs, API responses, or user exec environments.
- Prepared Pool shells remain prefix-free and credential-free.
- FUSE credentials are delivered only over the existing trusted one-shot authorize exec stdin channel.
- The mounter persists only sanitized bootstrap/authorization identity; credentials live only in memory and `/run/s3fs/passwd-s3fs` tmpfs.
- Kubernetes custom CA compatibility may retain its existing optional CA file/Secret path; AK/SK must not depend on that path.
- Existing system egress and user-network rules remain unchanged.
- Implementation is inline in the current session; no subagents.

---

### Task 1: Accept inline credentials for FUSE configuration

**Files:**
- Modify: `internal/config/config.go`
- Modify: `internal/config/config_test.go`
- Modify: `internal/storage/filesystem.go`
- Test: `internal/storage/filesystem_test.go`

**Interfaces:**
- Consumes: existing `config.FileSystemConfig.AccessKey`, `SecretKey`, and `CredentialFiles`.
- Produces: FUSE validation that requires a complete inline pair and rejects a mixed inline/file source.

- [ ] **Step 1: Write failing configuration tests**

Add cases proving a valid FUSE config uses inline credentials, either missing inline field fails without revealing its value, and setting any AK/SK credential-file field together with inline values fails as a mixed source.

```go
valid.Storage.FileSystem.AccessKey = "test-access"
valid.Storage.FileSystem.SecretKey = "test-secret"
valid.Storage.FileSystem.CredentialFiles = config.FileSystemCredentialFileConfig{}

assert.ErrorContains(t, validate(withSecretKey("")), "secret_key")
assert.NotContains(t, err.Error(), "test-access")
assert.ErrorContains(t, validate(withAccessFile("/run/secrets/access")), "credential source")
```

- [ ] **Step 2: Run the tests and verify RED**

Run: `go test -count=1 ./internal/config ./internal/storage`

Expected: FAIL because `validateFUSE` still requires credential files and forbids inline AK/SK.

- [ ] **Step 3: Implement inline-only FUSE validation**

Replace the file-required checks in `validateFUSE` with complete-pair validation:

```go
hasInline := filesystem.AccessKey != "" || filesystem.SecretKey != ""
hasFiles := filesystem.CredentialFiles.AccessKeyFile != "" || filesystem.CredentialFiles.SecretKeyFile != ""
if !hasInline || filesystem.AccessKey == "" || filesystem.SecretKey == "" {
    return fmt.Errorf("config: storage.filesystem.access_key and secret_key are required when workspace.mode is \"fuse\"")
}
if hasFiles {
    return fmt.Errorf("config: inline and file credential sources cannot be mixed when workspace.mode is \"fuse\"")
}
```

Keep `storage.LoadFileSystemCredentials` as the single loader; it already returns owned byte slices for inline credentials.

- [ ] **Step 4: Run the tests and verify GREEN**

Run: `go test -count=1 ./internal/config ./internal/storage`

Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/config/config.go internal/config/config_test.go internal/storage/filesystem.go internal/storage/filesystem_test.go
git commit -m "feat: allow inline fuse credentials"
```

---

### Task 2: Move s3fs credential creation to one-shot authorization

**Files:**
- Modify: `internal/fuseprotocol/protocol.go`
- Modify: `internal/fuseprotocol/protocol_test.go`
- Modify: `internal/mounter/supervisor.go`
- Modify: `internal/mounter/supervisor_test.go`
- Modify: `cmd/workspace-mounter/main_test.go`

**Interfaces:**
- Produces: `fuseprotocol.MountCredentials` and credential-bearing `AuthorizeRequest` wire fields.
- Produces: `AuthorizeRequest.Sanitized() AuthorizeRequest`, which clears credentials before persistence or state assignment.
- Consumes: fixed `BootstrapConfig.PasswdFile`; `AccessKeyFile`/`SecretKeyFile` are no longer required.

- [ ] **Step 1: Write failing protocol and supervisor tests**

Add tests proving:

```go
request := fuseprotocol.AuthorizeRequest{
    Version: 1,
    AccessKey: []byte("access"),
    SecretKey: []byte("secret"),
}
sanitized := request.Sanitized()
assert.Empty(t, sanitized.AccessKey)
assert.Empty(t, sanitized.SecretKey)
```

Supervisor authorization must create `passwd-s3fs` as mode `0600`, pass a credential-free request to the mount marker, reject empty/partial/newline/colon credentials, and return generic errors without credential values.

- [ ] **Step 2: Run the tests and verify RED**

Run: `go test -count=1 ./internal/fuseprotocol ./internal/mounter ./cmd/workspace-mounter`

Expected: FAIL because authorization has no credentials and bootstrap still reads mounted files.

- [ ] **Step 3: Add the bounded private wire fields**

Define:

```go
type MountCredentials struct {
    AccessKey []byte `json:"access_key"`
    SecretKey []byte `json:"secret_key"`
}

type AuthorizeRequest struct {
    // existing identity fields...
    Credentials MountCredentials `json:"credentials"`
}

func (r AuthorizeRequest) Sanitized() AuthorizeRequest {
    r.Credentials = MountCredentials{}
    return r
}
```

Bound each credential to `maxWorkspaceSecretBytes`, reject NUL/newline and `:` in the access key, and explicitly zero owned byte buffers after use.

- [ ] **Step 4: Move password publication into `Supervisor.Authorize`**

`Bootstrap` validates only non-secret immutable mount configuration and persists `bootstrap.json`. `Authorize` validates credentials, builds `access:secret\n`, atomically writes the fixed `PasswdFile`, passes only `request.Sanitized()` to marker persistence/state, and removes the password file on any failure before the mount process starts.

Do not include credentials in formatted errors or health output.

- [ ] **Step 5: Run the tests and verify GREEN**

Run: `go test -count=1 ./internal/fuseprotocol ./internal/mounter ./cmd/workspace-mounter`

Expected: PASS.

- [ ] **Step 6: Commit**

```bash
git add internal/fuseprotocol internal/mounter cmd/workspace-mounter
git commit -m "feat: deliver fuse credentials at authorization"
```

---

### Task 3: Give runtimes in-memory credentials without persisting them

**Files:**
- Modify: `internal/runtime/types.go`
- Modify: `internal/runtime/docker/runtime.go`
- Modify: `internal/runtime/docker/runtime_test.go`
- Modify: `internal/runtime/docker/container.go`
- Modify: `internal/runtime/docker/container_test.go`
- Modify: `internal/runtime/kubernetes/runtime.go`
- Modify: `internal/runtime/kubernetes/runtime_test.go`
- Modify: `internal/runtime/kubernetes/pod.go`
- Modify: `internal/runtime/kubernetes/pod_test.go`
- Modify: `internal/runtime/docker/secrets.go`
- Modify: `internal/runtime/docker/secrets_test.go`

**Interfaces:**
- Produces: `runtime.FUSECredentials{AccessKey, SecretKey []byte}` with `Clone()` and `Zero()` helpers.
- Produces: `docker.NewWithFUSECredentials(...)` and `kubernetes.WithFUSECredentials(...)` constructor option.
- Consumes: `fuseprotocol.AuthorizeRequest.Credentials` from Task 2.

- [ ] **Step 1: Write failing runtime tests**

For both runtimes, assert prepared Pod/container metadata and bootstrap contain no known test credential, while the private authorize wire request contains an owned copy:

```go
credentials := runtime.FUSECredentials{AccessKey: []byte("do-not-persist-access"), SecretKey: []byte("do-not-persist-secret")}
rt := newRuntimeWithCredentials(t, credentials)
info := prepareFUSE(t, rt)
assert.NotContains(t, serializedPreparedObject(info), "do-not-persist")
authorizeFUSE(t, rt, info)
assert.Equal(t, []byte("do-not-persist-access"), fake.authorize.Credentials.AccessKey)
```

Add recovery/list tests proving credentials are not required in labels, Redis-compatible specs, or persisted bootstrap identity.

- [ ] **Step 2: Run the runtime tests and verify RED**

Run: `go test -count=1 ./internal/runtime/docker ./internal/runtime/kubernetes`

Expected: FAIL because Docker copies external files and Kubernetes mounts a Secret.

- [ ] **Step 3: Implement runtime-owned credentials**

Each runtime constructor clones the supplied credential bytes. `AuthorizeWorkspaceMount` clones them into the private request and defers zeroing the request copy after `execControl` returns. The runtime keeps its owned source only for the `sandbox-api` process lifetime and never serializes it.

Keep credentials outside `SandboxSpec`, `WorkspaceFUSESpec`, runtime labels, pool snapshots, and error strings.

- [ ] **Step 4: Remove AK/SK mounts from prepared resources**

- Kubernetes: remove `accessKey`/`secretKey` Secret items and the `workspace-credentials` volume/mount when no custom CA is configured. Keep only optional CA projection compatibility.
- Docker: remove AK/SK materialization and their host bind; special containers receive credentials only through authorize stdin. Reduce the existing secret materializer to the optional custom-CA path and remove secret-root lifecycle state when `CASecretKey` is empty.
- Both: bootstrap contains only non-secret paths such as `/run/s3fs/passwd-s3fs`.

- [ ] **Step 5: Run the runtime tests and verify GREEN**

Run: `go test -race -count=1 ./internal/runtime/docker ./internal/runtime/kubernetes`

Expected: PASS with credential non-persistence assertions.

- [ ] **Step 6: Commit**

```bash
git add internal/runtime
git commit -m "feat: authorize runtimes with in-memory credentials"
```

---

### Task 4: Wire application startup to inline credentials

**Files:**
- Modify: `cmd/sandbox/main.go`
- Modify: `cmd/sandbox/main_test.go`
- Modify: `internal/sandbox/manager_test.go`

**Interfaces:**
- Consumes: `storage.LoadFileSystemCredentials(config.FileSystemConfig)`.
- Consumes: runtime constructors from Task 3.
- Produces: startup wiring that clones credentials into the selected runtime and zeroes temporary buffers.

- [ ] **Step 1: Write a failing startup wiring test**

Extract `loadRuntimeFUSECredentials(cfg *config.Config) (runtime.FUSECredentials, error)` and assert Docker and Kubernetes FUSE startup receive inline credentials while sync-only startup does not initialize credential-bearing runtime state.

```go
credentials, err := loadRuntimeFUSECredentials(cfg)
require.NoError(t, err)
assert.Equal(t, []byte("inline-access"), credentials.AccessKey)
defer credentials.Zero()
```

- [ ] **Step 2: Run the tests and verify RED**

Run: `go test -count=1 ./cmd/sandbox ./internal/sandbox`

Expected: FAIL because startup still constructs Docker `FileSecretMaterializer` and Kubernetes runtime has no credential constructor.

- [ ] **Step 3: Replace file materializer startup wiring**

When FUSE is enabled, call the common credential loader, convert the owned result into `runtime.FUSECredentials`, construct the selected runtime with it, and zero the temporary source after the runtime clones it. Sync-only startup retains the ordinary constructors.

- [ ] **Step 4: Run the tests and verify GREEN**

Run: `go test -count=1 ./cmd/sandbox ./internal/sandbox`

Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add cmd/sandbox internal/sandbox
git commit -m "feat: wire inline workspace credentials"
```

---

### Task 5: Restore configuration-only Compose and Helm deployment

**Files:**
- Modify: `docker/docker-compose.yml`
- Modify: `docker/.env.example`
- Delete: `docker/prepare.sh`
- Delete: `scripts/test-compose-prepare.sh`
- Modify: `scripts/test-fuse-images.sh`
- Modify: `deploy/helm/sandbox/values.yaml`
- Modify: `deploy/helm/sandbox/values.schema.json`
- Modify: `deploy/helm/sandbox/templates/deployment.yaml`
- Modify: `deploy/helm/sandbox/templates/_helpers.tpl`
- Modify: `deploy/helm/sandbox/templates/pre-backend-change-drain.yaml`
- Modify: `deploy/helm/sandbox/templates/pre-delete-drain.yaml`
- Modify: `deploy/helm/sandbox/templates/runtime-role.yaml`
- Modify: `testdata/values-fuse-minio.yaml`
- Modify: `testdata/values-fuse-obs-public.yaml`
- Modify: `testdata/values-fuse-obs-private.yaml`
- Test: `scripts/test-helm-chart.sh`
- Test: `scripts/test-helm-backend-switch.sh`

**Interfaces:**
- Produces: Compose mappings `STORAGE_ACCESS_KEY`/`STORAGE_SECRET_KEY` to the existing application env names.
- Produces: Helm values `config.storage.filesystem.accessKey`/`secretKey` and direct API/drain env injection.
- Consumes: inline validation and runtime delivery from Tasks 1-4.

- [ ] **Step 1: Write failing Compose and Helm contract tests**

Require:

```yaml
- SANDBOX_STORAGE_FILESYSTEM_ACCESS_KEY=${STORAGE_ACCESS_KEY:-}
- SANDBOX_STORAGE_FILESYSTEM_SECRET_KEY=${STORAGE_SECRET_KEY:-}
```

Reject AK/SK credential-file env mappings, Compose credential bind mounts, required `workspaceCredentials` Secret names, and AK/SK volumes in rendered Helm resources. Rendered API/drain jobs must contain inline env values but dynamic sandbox Pod tests must prove they do not.

- [ ] **Step 2: Run deployment tests and verify RED**

Run: `./scripts/test-fuse-images.sh && ./scripts/test-helm-chart.sh && ./scripts/test-helm-backend-switch.sh`

Expected: FAIL on the current file/Secret deployment contract.

- [ ] **Step 3: Simplify Docker Compose**

Add the two legacy variables to `.env.example`, map them to `SANDBOX_STORAGE_FILESYSTEM_ACCESS_KEY` and `...SECRET_KEY`, remove AK/SK credential-file mappings and credential-directory bind mounts, and remove `prepare.sh` as a deployment prerequisite. Remove `WORKSPACE_SECRET_STAGING_ROOT` from the normal no-CA path; retain a separately named optional CA input only for deployments that actually configure a custom CA.

- [ ] **Step 4: Simplify Helm values and templates**

Add `accessKey` and `secretKey` to values/schema, inject them only into API and drain workloads, remove AK/SK Secret volumes and Secret RBAC, and exclude the literal credential values from the backend fingerprint. Retain `credentialGeneration` in the fingerprint so rotation still drains safely.

- [ ] **Step 5: Run deployment tests and verify GREEN**

Run:

```bash
docker compose --env-file docker/.env.example -f docker/docker-compose.yml config >/dev/null
./scripts/test-fuse-images.sh
./scripts/test-helm-chart.sh
./scripts/test-helm-backend-switch.sh
```

Expected: PASS.

- [ ] **Step 6: Commit**

```bash
git add docker deploy/helm/sandbox scripts
git commit -m "deploy: read workspace credentials from config"
```

---

### Task 6: Rewrite deployment documentation around one config file

**Files:**
- Modify: `docs/deployment/docker-compose-deployment-upgrade.md`
- Modify: `docs/deployment/helm-deployment-upgrade.md`
- Modify: `docs/deployment/workspace-fuse.md`
- Modify: `docs/superpowers/specs/2026-09-01-workspace-container-fuse-mount-design.md`

**Interfaces:**
- Consumes: final Compose and Helm keys from Task 5.
- Produces: one configuration-only installation and upgrade path for each runtime.

- [ ] **Step 1: Add a failing documentation contract test**

Extend `scripts/test-fuse-images.sh` or create `scripts/test-inline-credential-docs.sh` to reject normal-path references to `prepare.sh`, `docker/secrets`, `workspaceCredentials.apiSecretName`, and manual workspace credential Secret creation. Require both inline key names in the Docker and Helm examples.

- [ ] **Step 2: Run the documentation contract and verify RED**

Run: `./scripts/test-fuse-images.sh`

Expected: FAIL because current runbooks require files and Secrets.

- [ ] **Step 3: Rewrite the runbooks**

Docker instructions become: update `.env`, run `docker compose config`, then `docker compose up -d -t 600`. Helm instructions become: update values, run `helm lint`, then `helm upgrade --install`. State plainly that values and Helm release history contain AK/SK, recommend restricting file/release access, and never reprint credentials in diagnostics.

- [ ] **Step 4: Run the documentation contract and verify GREEN**

Run: `./scripts/test-fuse-images.sh && git diff --check`

Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add docs scripts
git commit -m "docs: simplify workspace credential deployment"
```

---

### Task 7: Full regression and image verification

**Files:**
- Modify only files required by failures proven during this task.

**Interfaces:**
- Consumes: all previous tasks.
- Produces: fresh evidence that inline credentials do not regress sync, FUSE Pool, Docker, Kubernetes, Helm, or Compose behavior.

- [ ] **Step 1: Run static and unit verification**

```bash
go vet ./...
go test -race -count=1 ./internal/config ./internal/fuseprotocol ./internal/mounter ./internal/runtime/docker ./internal/runtime/kubernetes ./internal/sandbox
go test -count=1 ./...
./scripts/test-workspace-fuse-matrix.sh
./scripts/test-fuse-images.sh
./scripts/test-helm-chart.sh
./scripts/test-helm-backend-switch.sh
docker compose --env-file docker/.env.example -f docker/docker-compose.yml config >/dev/null
git diff --check
```

Expected: every command exits 0.

- [ ] **Step 2: Build and inspect both FUSE images without leaving cache pressure**

```bash
docker buildx build --platform linux/arm64 -f docker/images/workspace-mounter/Dockerfile -t sandbox-fuse-mounter:v0.0.0-inline-verify --load .
docker buildx build --platform linux/arm64 -f docker/images/sandbox-fuse/Dockerfile -t sandbox-fuse-docker:v0.0.0-inline-verify --load .
FUSE_IMAGE=sandbox-fuse-mounter:v0.0.0-inline-verify SANDBOX_IMAGE=sandbox-runtime:v0.2.12 ./scripts/verify-fuse-image.sh kubernetes package-check
SANDBOX_IMAGE=sandbox-fuse-docker:v0.0.0-inline-verify ./scripts/verify-fuse-image.sh docker package-check
```

Expected: both image contracts pass. Check free disk before and after; remove only the two explicit verification tags and prune only build cache generated by this verification.

- [ ] **Step 3: Run credential leak assertions**

Use fixed test-only canary values and scan rendered YAML, prepared runtime metadata, Redis test snapshots, and captured logs. The canaries may appear only in the API/drain deployment configuration explicitly accepted by the design and in the private authorize input captured by test fakes.

- [ ] **Step 4: Commit any proven verification fixes**

When verification proves a defect, add only the exact files changed for that reproduced failure and commit them with `git commit -m "test: verify inline workspace credentials"`. Skip this commit when verification required no code changes.
