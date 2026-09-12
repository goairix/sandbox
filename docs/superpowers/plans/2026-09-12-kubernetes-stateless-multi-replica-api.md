# Kubernetes Stateless Multi-Replica API Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Make every Kubernetes API replica operate every active sandbox directly through a shared, fenced state machine without API-to-API forwarding.

**Architecture:** Add an `ActiveSandboxRepository` that owns durable records, operation leases, lifecycle mutation admission, destroy admission, controller fencing, and paged discovery. Redis implements the transitions with same-slot Lua scripts; the manager uses it only for Kubernetes, retains local gates for Docker, and rebuilds request/runtime handles from the returned snapshot. Background coordinators use short fenced leases to adopt FUSE/Sync lifecycles, while the request data path remains direct.

**Tech Stack:** Go 1.24, go-redis v9, Redis Lua/ZSET/SCAN, Kubernetes client-go, Helm, testify, miniredis, Go race detector.

---

### Task 1: Define the active sandbox state machine

**Files:**
- Create: `internal/storage/state/active_sandbox.go`
- Test: `internal/storage/state/active_sandbox_test.go`

- [ ] **Step 1: Write failing validation tests**

Cover valid `publishing`, `active`, `destroying`, and `cleanup_pending` records; reject empty sandbox/runtime identity, zero generation/revision, mismatched snapshot ID, invalid phase, invalid operation type, and invalid lease duration.

- [ ] **Step 2: Run the focused test and verify RED**

Run: `go test ./internal/storage/state -run 'TestActiveSandbox' -count=1`

Expected: compile failure because `ActiveSandboxRecord`, phases, operation types, and `ActiveSandboxRepository` are not defined.

- [ ] **Step 3: Add the exact shared types and interface**

Define:

```go
type ActiveSandboxPhase string
const (
    ActiveSandboxPublishing ActiveSandboxPhase = "publishing"
    ActiveSandboxActive ActiveSandboxPhase = "active"
    ActiveSandboxDestroying ActiveSandboxPhase = "destroying"
    ActiveSandboxCleanupPending ActiveSandboxPhase = "cleanup_pending"
)

type ActiveSandboxRecord struct {
    Version uint32
    Scope string
    SandboxID string
    Phase ActiveSandboxPhase
    Revision uint64
    Generation int64
    RuntimeID string
    RuntimeUID string
    Snapshot []byte
    CleanupCheckpoint string
    CreatedAt time.Time
    UpdatedAt time.Time
}

type ActiveSandboxOperation struct {
    SandboxID string
    Token string
    Generation int64
    Kind string
    Mutation bool
    ExpiresAt time.Time
}

type ActiveSandboxControllerLease struct {
    SandboxID string
    Token string
    InstanceID string
    PodUID string
    Generation int64
    ExpiresAt time.Time
}
```

Add sentinel errors for conflict, closed admission, stale token, corrupt record, and unsupported durability. Define repository methods for publish, activate, begin/renew/end operation, begin destroy, checkpoint, compare-delete, acquire/renew/release controller, and cursor-based scan.

- [ ] **Step 4: Run the focused tests and verify GREEN**

Run: `go test ./internal/storage/state -run 'TestActiveSandbox' -count=1`

Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/storage/state/active_sandbox.go internal/storage/state/active_sandbox_test.go
git commit -m "feat: define active sandbox state machine"
```

### Task 2: Implement atomic Redis transitions and paged scanning

**Files:**
- Create: `internal/storage/state/redis/active_sandbox.go`
- Create: `internal/storage/state/redis/active_sandbox_test.go`
- Modify: `internal/storage/state/redis/store.go`

- [ ] **Step 1: Write failing Redis contract tests**

Use miniredis to prove: keys use one `{scope-digest:sandbox-id}` hash tag; publish is SetNX; activate and checkpoint are revision CAS; concurrent BeginOperation calls coexist; a mutation token is exclusive; BeginDestroy atomically rejects new admission and reports live operations; expired ZSET members are removed using Redis server time; stale generation renew/end cannot affect a replacement; controller acquisition and renewal are token/generation fenced; and `ScanActiveSandboxes` returns bounded cursor pages without `KEYS`.

- [ ] **Step 2: Run the focused test and verify RED**

Run: `go test ./internal/storage/state/redis -run 'TestActiveSandboxRepository' -count=1`

Expected: compile failure because `NewActiveSandboxRepository` is absent.

- [ ] **Step 3: Implement same-slot Lua scripts**

Create `ActiveSandboxRepository` around the existing Redis client. Each script validates record JSON fields before mutation, uses Redis `TIME`, and returns a typed status plus the exact record bytes. Use `redis.NewScript` so EVALSHA is used automatically with bounded NOSCRIPT recovery. Do not use a read-before-write client transaction.

- [ ] **Step 4: Implement paged SCAN and durability acknowledgement**

Expose the existing client to the repository within the `redis` package. Add `DurabilityMode` values `native`, `replica_ack`, and `best_effort`; execute `WAIT replicas timeout` after safety-boundary writes only in `replica_ack` mode, and return `state.ErrDurabilityUnconfirmed` without treating the transition as absent when acknowledgement is insufficient.

- [ ] **Step 5: Run focused and package tests**

Run: `go test ./internal/storage/state/redis -count=1`

Expected: PASS, including concurrent transition tests.

- [ ] **Step 6: Commit**

```bash
git add internal/storage/state/redis/active_sandbox.go internal/storage/state/redis/active_sandbox_test.go internal/storage/state/redis/store.go
git commit -m "feat: add redis active sandbox coordination"
```

### Task 3: Publish all Kubernetes sandboxes durably

**Files:**
- Create: `internal/sandbox/active_store.go`
- Create: `internal/sandbox/active_store_test.go`
- Modify: `internal/sandbox/manager.go`
- Modify: `internal/sandbox/types.go`
- Modify: `cmd/sandbox/main.go`

- [ ] **Step 1: Write failing two-manager publication tests**

Construct two managers with one fake repository and fake Kubernetes runtime. Verify ordinary ephemeral and persistent create both publish `active` snapshots containing Runtime ID, Runtime UID, immutable generation, timeout, network, and workspace metadata before success. Inject publish/activate ambiguity and verify exact readback; inject definitive failure and verify exact runtime cleanup.

- [ ] **Step 2: Run the focused test and verify RED**

Run: `go test ./internal/sandbox -run 'TestKubernetesActivePublication' -count=1`

Expected: FAIL because ephemeral ordinary sandboxes are only process-local.

- [ ] **Step 3: Wire the active repository into Kubernetes managers**

Add `ActiveSandboxes state.ActiveSandboxRepository` and `InstanceID` to `ManagerConfig`. Serialize a cloned `Sandbox` into `ActiveSandboxRecord.Snapshot`. Kubernetes create publishes `publishing`, finishes durable workspace associations, CAS-activates the record, then updates the local cache and returns. Docker keeps the existing local/session flow.

- [ ] **Step 4: Preserve pre-publication orphan evidence**

Add create-attempt labels/annotations to Kubernetes runtime specs and pool claim migration. Reconciliation protects resources with matching publishing/active records and only removes unmatched attempts after the configured grace period.

- [ ] **Step 5: Run focused tests and verify GREEN**

Run: `go test ./internal/sandbox -run 'TestKubernetesActivePublication' -count=1`

Expected: PASS.

- [ ] **Step 6: Commit**

```bash
git add internal/sandbox/active_store.go internal/sandbox/active_store_test.go internal/sandbox/manager.go internal/sandbox/types.go cmd/sandbox/main.go internal/runtime/kubernetes
git commit -m "feat: publish kubernetes sandboxes to shared state"
```

### Task 4: Replace Kubernetes local gates with distributed operation leases

**Files:**
- Create: `internal/sandbox/distributed_operation.go`
- Create: `internal/sandbox/distributed_operation_test.go`
- Modify: `internal/sandbox/manager.go`
- Modify: `internal/sandbox/workspace.go`

- [ ] **Step 1: Write failing cross-replica operation tests**

Create on manager A and invoke Get, Exec, ExecStream, upload, download, list, glob, edit, archive, multipart, network update, TTL update, mount, and unmount through manager B. Assert B uses the shared snapshot, verifies Runtime UID using `GetSandbox`, directly calls the runtime once, and releases the exact operation token only after returned streams/readers close.

- [ ] **Step 2: Run focused tests and verify RED**

Run: `go test ./internal/sandbox -run 'TestKubernetesDistributedOperation' -count=1`

Expected: FAIL with `ErrSandboxNotFound` or `ErrSandboxNotReady` on manager B.

- [ ] **Step 3: Implement one begin/one end request admission**

For Kubernetes, `acquireSandboxOperation` calls repository `BeginOperation`, decodes and validates the snapshot, confirms Runtime ID + UID from Kubernetes, and starts a shared batch renewal scheduler for long operations. The release function is idempotent and calls exact `EndOperation`. Docker continues using `operationGate`.

- [ ] **Step 4: Make streaming lifetime exact**

Wrap returned `io.ReadCloser` values so token release occurs on Close and EOF. For channels, release only when the forwarding goroutine emits the terminal event and closes. If renewal cannot be confirmed before the local safety deadline, cancel the operation context and surface a control-plane-unavailable error.

- [ ] **Step 5: Remove local sandbox authority from Kubernetes mutations**

Use mutation admission and revision CAS for network, TTL, mount, and unmount. Local maps may cache the new revision but cannot authorize the operation. Never retry non-idempotent runtime calls after an ambiguous response.

- [ ] **Step 6: Run focused, package, and race tests**

Run: `go test ./internal/sandbox -run 'TestKubernetesDistributedOperation' -count=1`

Run: `go test -race ./internal/sandbox -run 'TestKubernetesDistributedOperation|TestOperationGate' -count=1`

Expected: PASS.

- [ ] **Step 7: Commit**

```bash
git add internal/sandbox/distributed_operation.go internal/sandbox/distributed_operation_test.go internal/sandbox/manager.go internal/sandbox/workspace.go
git commit -m "feat: operate kubernetes sandboxes from any replica"
```

### Task 5: Make destroy and cleanup globally single-writer

**Files:**
- Create: `internal/sandbox/distributed_destroy_test.go`
- Modify: `internal/sandbox/manager.go`
- Modify: `internal/sandbox/sync_lifecycle.go`

- [ ] **Step 1: Write failing concurrent destroy tests**

Start a long operation on manager A, concurrently destroy through B and C, and verify: admission closes once, no later operation starts, runtime removal waits for live tokens, exactly one remover runs, Runtime UID is checked, failures persist `cleanup_pending`, and another replica resumes the recorded checkpoint.

- [ ] **Step 2: Run the focused test and verify RED**

Run: `go test ./internal/sandbox -run 'TestKubernetesDistributedDestroy' -count=1`

Expected: FAIL because destroy is currently guarded only by the local map/gate.

- [ ] **Step 3: Implement the distributed destroy state machine**

Use `BeginDestroy` before any destructive action. Poll the repository using bounded jitter until live operation leases drain or the request context ends. At every destructive boundary validate Runtime ID + UID and persist the next cleanup checkpoint. Exact compare-delete is last; a failed cleanup returns `ErrSandboxCleanupPending` with durable evidence intact.

- [ ] **Step 4: Make TTL reaping use the same path**

The Kubernetes reaper scans active records and invokes distributed destroy for expired records. Docker retains local iteration. Duplicate scans are harmless because `BeginDestroy` elects one cleaner.

- [ ] **Step 5: Run focused and race tests**

Run: `go test -race ./internal/sandbox -run 'TestKubernetesDistributedDestroy|TestReapExpired' -count=1`

Expected: PASS.

- [ ] **Step 6: Commit**

```bash
git add internal/sandbox/distributed_destroy_test.go internal/sandbox/manager.go internal/sandbox/sync_lifecycle.go
git commit -m "feat: fence kubernetes sandbox destruction"
```

### Task 6: Add fenced FUSE and Sync lifecycle adoption

**Files:**
- Create: `internal/sandbox/lifecycle_coordinator.go`
- Create: `internal/sandbox/lifecycle_coordinator_test.go`
- Modify: `internal/sandbox/manager.go`
- Modify: `internal/sandbox/sync_lifecycle.go`
- Modify: `internal/sandbox/workspace.go`

- [ ] **Step 1: Write failing controller takeover tests**

With two managers, publish one Sync and one FUSE sandbox. Verify one controller lease holder renews workspace ownership; after holder cancellation the second manager adopts the same runtime/generation; mount authorization is not called again; and a paused stale controller is fenced before renew, health-triggered teardown, quiesce, flush, runtime removal, owner release, or record mutation.

- [ ] **Step 2: Run the focused test and verify RED**

Run: `go test ./internal/sandbox -run 'TestLifecycleCoordinator' -count=1`

Expected: FAIL because lifecycle goroutines are currently tied to creator-local maps.

- [ ] **Step 3: Implement controller leases and maintenance election**

Use 15-second controller leases renewed every 5 seconds. A maintenance lease elects one bounded SCAN dispatcher per scope; all replicas use stable hashing only as a load-distribution hint and repository acquisition as final arbitration. Limit page size, per-pass work, and worker concurrency; add jitter.

- [ ] **Step 4: Rebuild lifecycle handles from durable state**

Decode the active snapshot, load workspace owner and FUSE pool record, validate sandbox/runtime UID, workspace generation, preparation ID, pool key, and health, then construct renewal/watcher state without calling mount authorization. Any incomplete identity becomes `cleanup_pending`, not a guessed active controller.

- [ ] **Step 5: Fence every controller side effect**

Before each renewal, record CAS, quiesce, flush, pool transition, runtime deletion, and workspace-owner release, call exact controller renewal/check. Lease loss cancels the local lifecycle and leaves durable state for the winner.

- [ ] **Step 6: Run focused and race tests**

Run: `go test -race ./internal/sandbox -run 'TestLifecycleCoordinator|TestFUSE|TestSync' -count=1`

Expected: PASS without duplicate authorization or cleanup.

- [ ] **Step 7: Commit**

```bash
git add internal/sandbox/lifecycle_coordinator.go internal/sandbox/lifecycle_coordinator_test.go internal/sandbox/manager.go internal/sandbox/sync_lifecycle.go internal/sandbox/workspace.go
git commit -m "feat: adopt workspace lifecycles across replicas"
```

### Task 7: Add production Redis HA configuration and readiness

**Files:**
- Modify: `internal/config/config.go`
- Modify: `internal/config/config_test.go`
- Modify: `internal/storage/state/redis/store.go`
- Modify: `internal/storage/state/redis/store_test.go`
- Modify: `cmd/sandbox/main.go`
- Modify: `internal/api/router.go`
- Modify: `internal/api/router_test.go`
- Modify: `configs/config.yaml`

- [ ] **Step 1: Write failing configuration and readiness tests**

Cover standalone, Sentinel, and Cluster client construction; Cluster DB must be zero; Kubernetes replicas greater than one reject missing shared state; production rejects best-effort durability and embedded single Redis; readiness fails when a no-op Begin/End probe or Kubernetes probe fails while liveness remains healthy.

- [ ] **Step 2: Run tests and verify RED**

Run: `go test ./internal/config ./internal/storage/state/redis ./internal/api -run 'TestRedisHA|TestReadiness' -count=1`

Expected: FAIL because only one Redis address and `/health` exist.

- [ ] **Step 3: Implement Redis topology options**

Add mode, addrs, master name, username, password, DB, durability mode, acknowledgement replica count/timeout, pool size, minimum idle connections, read/write/dial timeout, and production validation. Construct `redis.UniversalClient` for standalone/Sentinel/Cluster with topology refresh and bounded retries.

- [ ] **Step 4: Implement dependency-aware readiness**

Expose `/ready` separately from `/health`. Readiness performs bounded shared-state and Kubernetes reachability probes without mutating sandbox state and turns false immediately during shutdown.

- [ ] **Step 5: Run focused tests and verify GREEN**

Run: `go test ./internal/config ./internal/storage/state/redis ./internal/api -count=1`

Expected: PASS.

- [ ] **Step 6: Commit**

```bash
git add internal/config internal/storage/state/redis cmd/sandbox/main.go internal/api configs/config.yaml
git commit -m "feat: configure ha redis and readiness"
```

### Task 8: Update Helm without introducing routing affinity

**Files:**
- Modify: `deploy/helm/sandbox/values.yaml`
- Modify: `deploy/helm/sandbox/templates/configmap.yaml`
- Modify: `deploy/helm/sandbox/templates/deployment.yaml`
- Modify: `deploy/helm/sandbox/templates/redis.yaml`
- Modify: `deploy/helm/sandbox/templates/_helpers.tpl`
- Modify: `deploy/helm/sandbox/README.md`
- Test: `deploy/helm/sandbox/tests/stateless_api_test.yaml`

- [ ] **Step 1: Add failing Helm render assertions**

Assert external Sentinel/Cluster values render all state options; production with embedded Redis or best-effort durability fails; API pods receive unique Pod UID and instance identity; readiness uses `/ready`; termination grace covers operation lease drain; Service remains `sessionAffinity: None`; and no owner-routing Service/env/argument is rendered.

- [ ] **Step 2: Run Helm tests and verify RED**

Run: `helm lint deploy/helm/sandbox && helm template sandbox-fuse deploy/helm/sandbox --namespace aiadp-sandbox-fuse --set replicaCount=3 --set redis.external.enabled=true --set redis.external.mode=sentinel`

Expected: render or assertion failure because HA state values are absent.

- [ ] **Step 3: Implement templates and validation**

Keep embedded Redis for explicit development use. Add external Redis Secret references, HA mode/durability values, Pod UID Downward API, `/ready` probe, and fail-fast helper checks. Do not add session affinity or internal API routing.

- [ ] **Step 4: Verify Helm variants**

Run standalone development, external Sentinel production, and external Cluster production lint/template cases. Run `rg -n 'sessionAffinity: (ClientIP|None)|owner.*routing|/ready'` over rendered manifests and require only `None`, no routing artifacts, and the readiness endpoint.

- [ ] **Step 5: Commit**

```bash
git add deploy/helm/sandbox
git commit -m "feat: deploy stateless api with ha state backend"
```

### Task 9: Full verification and live three-replica acceptance

**Files:**
- Create: `scripts/test-kubernetes-multi-replica.sh`
- Modify: `Makefile`

- [ ] **Step 1: Add the acceptance script**

The script targets an explicit context and namespace, discovers three ready API pods, creates ordinary/Sync/FUSE sandboxes through the Service, pins every sandbox-scoped operation to different API pods, runs concurrent long exec/upload/destroy cases, deletes one request pod and one controller pod, confirms runtime UID stability and controller takeover, and finally destroys only test-created resources. It must refuse an empty context/namespace and never delete by broad label without recorded test IDs.

- [ ] **Step 2: Run repository verification**

Run:

```bash
go test ./... -count=1
go test -race ./internal/storage/state/... ./internal/sandbox/... -count=1
go vet ./...
helm lint deploy/helm/sandbox
helm template sandbox-fuse deploy/helm/sandbox --namespace aiadp-sandbox-fuse >/tmp/sandbox-fuse-rendered.yaml
git diff --check
```

Expected: every command exits 0.

- [ ] **Step 3: Build the API image locally**

Run the repository's existing sandbox-api image target and confirm the binary starts with `--help`. Do not build chat or mounter images unless their source changed.

- [ ] **Step 4: Hand off image deployment to the user**

Report the exact sandbox-api image requirement and Helm changes. The user owns image publication and cluster upgrade.

- [ ] **Step 5: Run live acceptance after deployment**

Run: `scripts/test-kubernetes-multi-replica.sh ds-ai-research aiadp-sandbox-fuse`

Expected: all cross-replica operations pass; active runtimes are not recreated by API/controller Pod deletion; no repeated reconciliation errors; test cleanup reaches zero test-owned records, Pods, NetworkPolicies, and CiliumNetworkPolicies.

- [ ] **Step 6: Commit**

```bash
git add scripts/test-kubernetes-multi-replica.sh Makefile
git commit -m "test: cover stateless kubernetes api replicas"
```
