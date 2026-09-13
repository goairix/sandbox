# Sentinel Bootstrap Safety Primitives Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Implement bounded, fail-closed identity, recovery, configuration and attestation primitives without wiring a bootstrap controller.

**Architecture:** Pure decisions consume validated fixed-three-member namespace and volume snapshots. Persisted Sentinel observations are discovery evidence, never promotion authority. Attestation signatures bind a volume marker to the current authenticated Redis process; transport and Pending phase transition remain separate unimplemented protocol requirements.

**Tech Stack:** Go 1.25, standard-library JSON, crypto/rand, HMAC-SHA256, table-driven tests, race detector.

---

Scope is only `internal/redisbootstrap/*` and this plan. No command/config/Chart/store edits, commits, image operations, or live-cluster writes.

### Task 1: Fixed membership and recovery decisions

**Files:** Create `internal/redisbootstrap/state.go`, `internal/redisbootstrap/decision.go`; test `internal/redisbootstrap/state_test.go`, `internal/redisbootstrap/decision_test.go`.

- [x] Write tests for exact-three unique DNS membership, phase/identity mismatch, missing namespace state with retained volume, partial Pending, nonzero prior primary, duplicate evidence, fewer than two retained observations, higher minority epoch, replacement following current primary and no wrapper promotion.
- [x] Run `go test ./internal/redisbootstrap` and observe undefined exported primitives (RED).
- [x] Implement `ClusterState.Validate`, `VolumeIdentity.Validate`, `SelectEvidence`, `DecideBootstrap`; initial seed requires the complete caller-provided inventory to be genuinely empty. Incomplete persisted configuration yields Wait. A prior primary is restored only when its member/role/epoch matches selected persisted evidence. Pending persisted primaries also require evidence; only persisted replicas can resume without role changes.
- [x] Run `go test ./internal/redisbootstrap` (GREEN).

Executable decision-test contract:

```go
decision, err := DecideBootstrap(&cluster, Member{DNS: cluster.Members[0], Ordinal: 0}, inventory, evidence)
if err != nil || decision.Action != Wait {
    t.Fatalf("partial initialization must not reseed: %+v, %v", decision, err)
}
```

### Task 2: Strict configuration and durable state helpers

**Files:** Create `internal/redisbootstrap/config.go`, `internal/redisbootstrap/files.go`; tests in matching `_test.go` files.

- [x] Write quoted-value tests for spaces, quotes/backslashes and every control rune; strict JSON rejects unknown/duplicate fields and trailing values; atomic state writes leave complete private files and never create broad directories.
- [x] Run `go test ./internal/redisbootstrap` (RED for missing helpers).
- [x] Implement `QuoteConfigValue`, `ParseClusterState`, `ParseVolumeIdentity`, `WriteVolumeIdentity` and `NewVolumeIdentity`. Keep filesystem scope caller-selected, use private same-directory temporary file, fsync/exclusive-hard-link/fsync-parent, and explicit errors. Identity creation never replaces an existing destination; update/role transactions are out of scope.
- [x] Run `go test ./internal/redisbootstrap` (GREEN).

Executable quoting contract:

```go
got, err := QuoteConfigValue(`a b"c\d`)
if err != nil || got != `"a b\"c\\d"` {
    t.Fatalf("unsafe quoting: %q, %v", got, err)
}
```

### Task 3: Proposed local readiness attestation

**Files:** Create `internal/redisbootstrap/attestation.go`; test `internal/redisbootstrap/attestation_test.go`.

- [x] Write MAC tampering, foreign member/cluster/marker, short key, empty/malformed run_id, stale same-name replacement, endpoint mismatch and valid attestation tests.
- [x] Run `go test ./internal/redisbootstrap` (RED).
- [x] Implement `SignAttestation` and `VerifyAttestation`. Verification compares the signed member and current authenticated endpoint's member and run_id; shared password is not volume proof. `BusinessWritersAllowed` requires valid Initialized state.
- [x] Run `go test -race ./internal/redisbootstrap` and `go vet ./internal/redisbootstrap` (GREEN).

Executable stale-instance contract:

```go
err := VerifyAttestation(key, proof, cluster, identity, endpoint)
if err == nil {
    t.Fatal("old run_id must not attest a same-name replacement")
}
```

### Delivery boundary and unresolved requirements

This package does not implement authenticated remote inventory acquisition, a PVC-local attestation server, a challenge/response transport, ConfigMap resourceVersion transition, role/config write transaction, safe Redis/Sentinel config interpretation, process startup, readiness topology checks, replication barrier/WAIT, or phase-gated API deployment. A marker-only crash intentionally blocks rather than resetting roles. A selected majority is persisted discovery evidence only; native Sentinel remains the sole election authority. A MAC shared across members requires trusted local signer isolation and trusted expected marker inventory, not arbitrary API access to the signing key. The verifier must fetch INFO run_id freshly from the authenticated fixed endpoint and protect that fetch against endpoint substitution. Same-process replay is not a topology freshness proof. No operational HA completion claim follows from pure tests.

### Proposed attestation contract (not a deployed wire protocol)

`ReadinessAttestation` version 1 binds `clusterID`, exact DNS/ordinal `member`, 128-bit local `markerID` and Redis's 40-hex `runID`. Signature is lowercase hex HMAC-SHA256 over a domain-separated canonical JSON struct with empty signature. Keys require at least 32 bytes and must be dedicated attestation keys, not an assumption that a Redis password is a PVC identity. `VerifyAttestation` accepts an externally trusted expected local marker and externally acquired current authenticated member INFO; `AuthenticatedEndpoint.Authenticated` is the caller's assertion, not network authentication performed by this package.

Any future service must derive signed fields itself from its own validated PVC and authenticated local Redis, reject a Reserved/incomplete configuration, and never accept arbitrary remote marker/run_id inputs for signing. A fixed-member transport needs independently authenticated endpoint binding (for example mutually authenticated TLS), a trusted initial marker-registration transaction, key isolation from business writers, and a fresh verifier Redis INFO acquisition. To require freshness within one Redis process, design a nonce/deadline in a new explicitly versioned signed contract; version 1 deliberately only defeats prior-process/same-name replacement replay. Concurrent Pending registration, atomic role/config reservation and monitor-enable gating must be proven before using Seed actions to launch a real cluster. No Job, remote service or coordinator is supplied here.

### Verification ledger

No commits are authorized for this subtask.

- Initial `go test ./internal/redisbootstrap`: RED with undefined state/decision primitives; subsequent GREEN.
- Configuration/state-helper and attestation tests each separately observed RED with missing helpers before implementation, then GREEN.
- Additional assertion RED: lost-state evidence allowed FollowPrimary; higher retained inventory epoch allowed older FollowPrimary; JSON null ordinal was coerced to zero. Implemented source-retention/epoch guards and strict null rejection, then GREEN.
- Independent review regression RED: matching-epoch primary conflicts authorized FollowPrimary. Four failing cases covered live source versus retained source, a lower-epoch live source versus its same-epoch retained source, unreported retained state versus selected majority, and mutually contradictory retained states at a lower epoch. Reject every same-epoch Sentinel primary mapping conflict before returning an action; old-primary-follow now uses genuinely higher live epoch 5 over prior epoch 4. Subsequent `go test -race -count=1 -cover ./internal/redisbootstrap`, vet and golangci-lint passed (88.2%, 0 lint issues).
- Follow-up cross-source RED: retained source 0 epoch 2/primary 0 and source 1 epoch 3/primary 0 conflicted with live source 0 epoch 3/primary 1 while two live epoch 4 reports named primary 2. The decision incorrectly restored primary 2. Merge every valid live observation into the global retained/discovered `primaryAtEpoch` mapping, rejecting any same-epoch mismatch across all sources before role actions.
- Partial-state matrix checks all 26 nonempty combinations of three empty/Reserved/Configured members, for each local ordinal: none permits SeedPrimary or SeedReplica.
- `go test -race -cover ./internal/redisbootstrap`: PASS, 88.2% statement coverage, no reported races.
- `go vet ./internal/redisbootstrap`: PASS.
- `golangci-lint run ./internal/redisbootstrap/...`: PASS, 0 issues.
- `git diff --check`: PASS. Other agents' unrelated files left untouched.
