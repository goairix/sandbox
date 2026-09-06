# Huawei OBS Private Profile Rollout Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [x]`) syntax for tracking.

**Goal:** Promote the tested 2023 Huawei private-cloud OBS ordinary-bucket s3fs contract to a release-selectable profile, build a dedicated immutable image, switch the ARM64 Helm deployment, and pass the same HTTP API acceptance matrix used for MinIO.

**Architecture:** Keep public Huawei OBS blocked and promote only `huawei-obs-private-2023-v1`. The compiled profile is the sole source of executable s3fs options; the JSON manifest remains non-executable audit metadata, and credentials remain Kubernetes Secrets. The current MinIO Helm values and image digests remain the rollback target until OBS API acceptance and exact-prefix cleanup complete.

**Tech Stack:** Go 1.25, s3fs 1.95, Docker Buildx, Kubernetes/Kind-compatible sidecar lifecycle, Cilium FQDN policy, Helm 3, Huawei OBS S3v2 API.

**Spec:** `docs/superpowers/specs/2026-09-01-workspace-container-fuse-mount-design.md`

## Global Constraints

- Execute inline in the current session; do not use subagents.
- Promote only the private-cloud profile; `huawei-obs-public-v1` remains candidate and release-blocked.
- Preserve TLS certificate and hostname verification; never add `no_check_certificate` or `ssl_verify_hostname=0`.
- Freeze virtual-host addressing with `url=https://obs.cn-southwest-268.shuanghuayun.com`, canonical `endpoint=cn-southwest-268`, `sigv2`, and `compat_dir`.
- Use `/bin/sync -f -- /workspace` as the verified quiesced durable-flush command and ordinary `fusermount3 -u`; never use lazy or forced unmount.
- Bind images to the actual s3fs 1.95 artifact SHA-256 `fb45cbc9f8303ae6d919b8b27ee9f443e285b08e1250f4aa85ca50ea2cfea695` and deploy by registry digest.
- Keep credentials out of Git, image layers, Helm values, command output, and documentation.
- Preserve the existing no-private-network default; system egress permits only configured DNS and the exact OBS FQDN on port 443.
- Delete only exact test object prefixes and exact temporary Kubernetes resources.

---

### Task 1: Promote the private OBS compiled profile

**Files:**
- Modify: `internal/mounter/profile_test.go`
- Modify: `internal/mounter/profile.go`
- Modify: `internal/config/config_test.go`

**Interfaces:**
- Consumes: `Profile`, `ProfileDescriptor`, `LookupCompiledProfile`, and `CheckProductionProfile`.
- Produces: selectable `huawei-obs-private-2023-v1` with `Options(BootstrapConfig) ([]string, error)` and `Flush(BootstrapConfig) []string`.

- [x] **Step 1: Write the failing profile contract test**

```go
func TestCompiledHuaweiOBSPrivateProfileHasExactVerifiedMountContract(t *testing.T) {
    profile, ok := LookupCompiledProfile("obs", "huawei-obs-private-2023-v1")
    require.True(t, ok)
    assert.Equal(t, MountParametersVerified, profile.Descriptor.MountParameters)
    assert.Equal(t, DurableFlushVerified, profile.Descriptor.DurableFlush)
    assert.Equal(t, "virtual-host", profile.Descriptor.AddressingStyle)
    assert.Equal(t, "sigv2", profile.Descriptor.SignatureVersion)
    options, err := profile.Options(fuseprotocol.BootstrapConfig{
        Provider: "obs", Endpoint: "https://obs.cn-southwest-268.shuanghuayun.com", Region: "cn-southwest-268",
    })
    require.NoError(t, err)
    assert.Equal(t, []string{
        "-o", "url=https://obs.cn-southwest-268.shuanghuayun.com",
        "-o", "endpoint=cn-southwest-268", "-o", "sigv2", "-o", "compat_dir",
    }, options)
    assert.Equal(t, []string{"/bin/sync", "-f", "--", "/workspace"}, profile.Flush(fuseprotocol.BootstrapConfig{MountPath: "/workspace"}))
}
```

- [x] **Step 2: Run the test and verify RED**

Run: `go test ./internal/mounter -run '^TestCompiledHuaweiOBSPrivateProfileHasExactVerifiedMountContract$' -count=1`

Expected: FAIL because the profile is currently unverified and not selectable.

- [x] **Step 3: Implement the exact private OBS profile**

Add `huaweiPrivate2023Options`, requiring provider `obs`, a canonical HTTPS endpoint, and a canonical region; return only `url`, `endpoint`, `sigv2`, and `compat_dir`. Mark the descriptor mount and flush statuses verified, addressing `virtual-host`, and wire `syncFilesystem` as `Flush`. Do not change the public profile.

- [x] **Step 4: Update configuration tests**

Add a valid private OBS Kubernetes configuration case and retain explicit tests proving the public profile remains blocked and provider/profile mismatches fail closed.

- [x] **Step 5: Run focused tests**

Run: `go test ./internal/mounter ./internal/config -count=1`

Expected: PASS.

### Task 2: Freeze manifest, image gate, and deployment evidence

**Files:**
- Modify: `docker/images/workspace-mounter/profiles/huawei-obs-private-2023-v1.json`
- Modify: `scripts/test-fuse-images.sh`
- Modify: `docs/superpowers/specs/2026-09-01-workspace-container-fuse-mount-design.md`
- Modify: `docs/deployment/workspace-fuse.md`

**Interfaces:**
- Consumes: the Task 1 descriptor and the Dockerfile's unique build-time s3fs hash placeholder.
- Produces: a manifest that exactly matches the compiled descriptor and passes both package and release gates.

- [x] **Step 1: Change the manifest expectation first and verify RED**

Change the private profile tuple in `scripts/test-fuse-images.sh` from `blocked-pending-flush-spike` to `verified`, then run `scripts/test-fuse-images.sh`.

Expected: FAIL until the private manifest and compiled descriptor are updated consistently.

- [x] **Step 2: Update the private manifest**

Set `mount_parameters` and `durable_flush` to `verified`, `endpoint_option` to `url`, `region_option` to `endpoint`, `addressing_style` to `virtual-host`, and `signature_version` to `sigv2`. Retain the unique all-zero SHA placeholder for Docker build substitution and keep executable options out of JSON.

- [x] **Step 3: Record provider-spike evidence in documentation**

Document s3fs 1.95, verified TLS/SNI, SigV2, canonical region, `compat_dir`, UID/GID 1000 operations, 25 MiB multipart SHA-256 direct-API readback, `sync -f`, ordinary unmount, remount readback, and exact cleanup. State that public OBS remains blocked and that the private-cloud service version is still operational metadata to obtain from the vendor when available.

- [x] **Step 4: Verify static contracts**

Run: `go test ./internal/mounter ./internal/config -count=1 && scripts/test-fuse-images.sh && scripts/test-helm-chart.sh`

Expected: PASS.

### Task 3: Build and deploy the immutable private OBS image

**Files:**
- No credential-bearing repository files.
- Runtime-only build directory under a secure temporary path.

**Interfaces:**
- Consumes: profile-bound `workspace-mounter`, pinned base image, s3fs package URL/SHA, and `sandbox-workspace-obs-private` Secret.
- Produces: `registry.i.huaxisy.com/library/ai-infra/sandbox-fuse-mounter-obs-private:<tag>@sha256:<digest>` and a Helm revision selecting provider `obs`.

- [x] **Step 1: Compile the profile-bound ARM64 binary**

Run `GOOS=linux GOARCH=arm64 CGO_ENABLED=0 go build -ldflags '-X=main.imageProfileID=huawei-obs-private-2023-v1'` into a temporary build context and verify the binary is ARM64.

- [x] **Step 2: Build and push the dedicated image**

Use `docker buildx build --platform linux/arm64 --push` with the existing pinned base image, the verified s3fs artifact URL/SHA, the private manifest, and the exact profile ID. Resolve the registry digest and run the image `package-check` and `release-check` entrypoints.

- [x] **Step 3: Render OBS Helm values without credentials**

Select `storage.filesystem.provider=obs`, bucket `sandbox-fuse-workspace`, HTTPS endpoint, region `cn-southwest-268`, Secret `sandbox-workspace-obs-private`, profile `huawei-obs-private-2023-v1`, a distinct storage identity and credential generation, the dedicated digest, `cilium-fqdn`, the OBS FQDN, and endpoint port 443.

- [x] **Step 4: Helm lint/render, upgrade, and wait**

Run `helm lint`, render the manifests for inspection, then `helm upgrade --install --wait --timeout 8m`. Verify API/Redis are Ready and the prepared pool is intentionally `1/2 Running` with mounter unready before binding.

### Task 4: Repeat HTTP API acceptance on private OBS and finalize

**Files:**
- Modify: `docs/deployment/workspace-fuse.md` with sanitized acceptance results.

**Interfaces:**
- Consumes: the deployed OBS Helm revision and existing API key Secret.
- Produces: end-to-end evidence for delayed pool binding, FUSE semantics, API surface, cleanup, and rollback readiness.

- [x] **Step 1: Run core lifecycle API checks**

Verify health, authentication rejection, persistent create with only `workspace_path`, pool hit and UID preservation, GET, TTL, network update, Bash/Python/Node.js exec, workspace info, both sync directions, immutable mount/unmount responses, delete, and missing sandbox 404.

- [x] **Step 2: Run file and streaming API checks**

Verify direct upload/read/list/recursive-list/glob/read-lines/edit/edit-lines/download, multipart init/chunk/status/complete, SSE exec, network-required rejection, skills list/get/file, and traversal rejection.

- [x] **Step 3: Verify durability and pool refill**

After `from_container` sync, read the exact object through an independent S3v2 client; after delete, verify the consumed Pod UID is gone and `sandbox-api` replenishes a new `1/2 Running` prepared Pod.

- [x] **Step 4: Clean exact test data and run repository verification**

Delete only the exact OBS API test prefixes, then run `go test ./... -count=1`, `scripts/test-fuse-images.sh`, and `scripts/test-helm-chart.sh`.

- [x] **Step 5: Commit the completed change**

Review `git diff --check`, verify no secrets are tracked, and commit profile, tests, Helm/deployment documentation, the 404 fix, and this plan with a scoped feature commit message.
