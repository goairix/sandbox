# Concise Docker FUSE Pool Resource Names Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Replace Docker FUSE Pool `prep-<32-hex>` resource names with the sync-style `sandbox-pool-<10-alphanumeric>` family while retaining fail-closed cleanup compatibility for resources created by older releases.

**Architecture:** Generate one short preparation ID per FUSE Pool instance and derive the runtime, gateway, pair network, and cache volume names from its shared suffix. Keep labels, PoolKey, logical ID, secret root, and immutable Docker runtime UID as the authoritative identity; accept the legacy preparation ID format only when reconciling labeled resources so upgrades can clean them safely.

**Tech Stack:** Go, Docker Engine API, Testify, Docker Compose

---

### Task 1: Lock the new and legacy naming contracts with tests

**Files:**
- Modify: `internal/runtime/docker/runtime_test.go`

- [ ] **Step 1: Add a failing generator and validator test**

Add a test that calls `newDockerPreparationID` repeatedly and requires every value to match `^sandbox-pool-[a-z0-9]{10}$`, remain unique in the sample, and pass `validDockerPreparationID`. In the same test require legacy `prep-00000000000000000000000000000000` to remain valid, and reject truncated, uppercase, and malformed variants.

- [ ] **Step 2: Add a failing resource-family test**

Require these exact mappings:

```text
sandbox-pool-a1b2c3d4e5 -> sandbox-pool-a1b2c3d4e5
                         sandbox-gw-pool-a1b2c3d4e5
                         sandbox-pair-pool-a1b2c3d4e5
                         sandbox-fuse-cache-pool-a1b2c3d4e5
```

Also require legacy `prep-<32-hex>` inputs to retain their old gateway, network, and cache names so recovery never guesses a renamed legacy resource.

- [ ] **Step 3: Run the focused tests and verify RED**

Run:

```bash
go test ./internal/runtime/docker -run 'TestDockerPreparationIDNaming|TestDockerFUSEResourceNames' -count=1 -v
```

Expected: FAIL because the generator still returns `prep-<32-hex>` and concise resource-name helpers do not exist.

### Task 2: Implement concise names without weakening identity checks

**Files:**
- Modify: `internal/runtime/docker/runtime.go`
- Modify: `internal/runtime/docker/network.go`
- Modify: `internal/runtime/docker/container.go`

- [ ] **Step 1: Generate a sync-style preparation ID**

Use `crypto/rand` to produce 10 lowercase alphanumeric characters and return `sandbox-pool-<suffix>`. Keep generation local to the Docker runtime package to avoid an import cycle with `internal/sandbox`.

- [ ] **Step 2: Validate both current and legacy preparation IDs**

Make `validDockerPreparationID` accept exactly either `sandbox-pool-[a-z0-9]{10}` or `prep-[0-9a-f]{32}`. Do not accept arbitrary Docker names or use resource names as authorization.

- [ ] **Step 3: Centralize derived resource names**

Add helpers that strip the leading `sandbox-` only for the new format before applying resource-specific prefixes. Use them consistently during creation, inspection, rollback, removal, and orphan reconciliation. For legacy IDs, preserve the existing concatenation exactly.

- [ ] **Step 4: Run focused tests and verify GREEN**

Run:

```bash
go test ./internal/runtime/docker -run 'TestDockerPreparationIDNaming|TestDockerFUSEResourceNames|TestDockerPrepare|TestDockerReconcile' -count=1 -v
```

Expected: PASS.

### Task 3: Document, regress, and verify the real Compose flow

**Files:**
- Modify: `docs/deployment/workspace-fuse.md`

- [ ] **Step 1: Document the resource names and upgrade behavior**

Record the four new names, state that the suffix is operationally readable rather than authoritative identity, and document conservative cleanup support for legacy `prep-<32-hex>` resources.

- [ ] **Step 2: Run full static and unit verification**

Run:

```bash
gofmt -w internal/runtime/docker/runtime.go internal/runtime/docker/runtime_test.go internal/runtime/docker/network.go internal/runtime/docker/container.go
go vet ./...
go test -race ./...
go -C sdk/go test ./...
git diff --check
```

Expected: all commands exit 0.

- [ ] **Step 3: Rebuild Compose and verify actual Docker names**

Drain the current test sandbox through sandbox-api, then run:

```bash
docker compose --env-file docker/.env -p sandbox-local-e2e -f docker/docker-compose.yml up -d --build
```

Create a new FUSE sandbox through sandbox-api using only `workspace_path`. Require the consumed runtime and replenished prepared runtime names to match `sandbox-pool-[a-z0-9]{10}`, and their gateways to match `sandbox-gw-pool-[a-z0-9]{10}`. Verify `/workspace` remains `fuse.s3fs`, UID/GID remain 1000, and a written object is readable from MinIO.

- [ ] **Step 4: Commit implementation**

```bash
git add internal/runtime/docker/runtime.go internal/runtime/docker/runtime_test.go internal/runtime/docker/network.go internal/runtime/docker/container.go docs/deployment/workspace-fuse.md docs/superpowers/plans/2026-09-06-concise-fuse-pool-names.md
git commit -m "fix: shorten docker fuse pool resource names"
```
