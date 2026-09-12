# Docker FUSE AppArmor Default Fix Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Make Docker FUSE mounts work on AppArmor-enabled hosts without requiring operators to install a host profile.

**Architecture:** The sandbox-api remains the sole owner of Docker HostConfig. An empty optional LSM profile maps to an explicit Docker `apparmor=unconfined` override for the trusted FUSE runtime, while non-empty profiles keep the existing confined behavior. The FUSE Pool key version changes so prepared containers created under the former `docker-default` semantics are not reused.

**Tech Stack:** Go, Docker Engine API, Testify, Markdown deployment docs.

---

### Task 1: Correct Docker FUSE security options

**Files:**
- Modify: `internal/runtime/docker/container_test.go`
- Modify: `internal/runtime/docker/container.go`

- [ ] **Step 1: Change the empty-profile test first**

Update `TestCreateContainerConfigForFUSEWithoutOptionalLSMProfile` to require:

```go
assert.Equal(t, []string{"no-new-privileges=true", "apparmor=unconfined"}, host.SecurityOpt)
```

- [ ] **Step 2: Run the focused test and verify RED**

Run:

```bash
go test ./internal/runtime/docker -run TestCreateContainerConfigForFUSEWithoutOptionalLSMProfile -count=1
```

Expected: FAIL because the current result contains only `no-new-privileges=true`.

- [ ] **Step 3: Implement the minimal HostConfig change**

Change the empty branch of `fuseSecurityOptions` to:

```go
if profile == "" {
	return []string{"no-new-privileges=true", "apparmor=unconfined"}, nil
}
```

Keep explicit `unconfined` input invalid and keep custom AppArmor/SELinux profile behavior unchanged.

- [ ] **Step 4: Run Docker runtime tests and verify GREEN**

Run:

```bash
go test ./internal/runtime/docker -count=1
```

Expected: PASS.

### Task 2: Prevent reuse of old prepared FUSE containers

**Files:**
- Modify: `internal/sandbox/fuse_pool_test.go`
- Modify: `internal/sandbox/fuse_pool.go`

- [ ] **Step 1: Add a version contract test**

Add:

```go
func TestFUSEPoolKeyUsesAppArmorDefaultV2(t *testing.T) {
	assert.Equal(t, "workspace-fuse-pool/v2", fusePoolKeyVersion)
}
```

- [ ] **Step 2: Run the focused test and verify RED**

Run:

```bash
go test ./internal/sandbox -run TestFUSEPoolKeyUsesAppArmorDefaultV2 -count=1
```

Expected: FAIL because the current version is `workspace-fuse-pool/v1`.

- [ ] **Step 3: Bump the Pool key version**

Change:

```go
fusePoolKeyVersion = "workspace-fuse-pool/v2"
```

- [ ] **Step 4: Run pool tests and verify GREEN**

Run:

```bash
go test ./internal/sandbox -count=1
```

Expected: PASS.

### Task 3: Correct operator documentation

**Files:**
- Modify: `docs/deployment/docker-compose-deployment-upgrade.md`
- Modify: `docs/deployment/workspace-fuse.md`

- [ ] **Step 1: Document the real empty-profile behavior**

State that Docker FUSE explicitly uses `apparmor=unconfined` when
`FUSE_LSM_PROFILE` is empty, because Docker's `docker-default` denies mount.
State that the remaining container restrictions stay enabled and that a loaded
custom profile may still be configured as an optional hardening step.

- [ ] **Step 2: Document the image and upgrade scope**

State that this fix requires only a new `sandbox-api` image and that operators
must keep `FUSE_LSM_PROFILE` unset unless the named profile is already loaded.

### Task 4: Verify and commit

**Files:**
- Verify all modified files.

- [ ] **Step 1: Run formatting and focused tests**

```bash
gofmt -w internal/runtime/docker/container.go internal/runtime/docker/container_test.go internal/sandbox/fuse_pool.go internal/sandbox/fuse_pool_test.go
go test ./internal/runtime/docker ./internal/sandbox -count=1
```

Expected: PASS.

- [ ] **Step 2: Run full verification**

```bash
go test ./... -count=1
go vet ./...
docker compose -f docker/docker-compose.yml config >/dev/null
```

Expected: all commands exit 0.

- [ ] **Step 3: Commit the implementation**

```bash
git add internal/runtime/docker/container.go internal/runtime/docker/container_test.go \
  internal/sandbox/fuse_pool.go internal/sandbox/fuse_pool_test.go \
  docs/deployment/docker-compose-deployment-upgrade.md docs/deployment/workspace-fuse.md
git commit -m "fix: allow docker fuse mounts under apparmor"
```
