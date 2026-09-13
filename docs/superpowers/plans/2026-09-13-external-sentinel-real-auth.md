# External Sentinel Real Authentication Verification Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Reproducibly verify four real authentication links with independent special-character dummy Redis/Sentinel credentials and a production Store write with replica ACK.

**Architecture:** A uniquely named, local-only Compose project contains three Redis, three Sentinel, one isolated network and six named data/state volumes. A cached Go container generates private configs using production `redisbootstrap.QuoteConfigValue`; a separate cached Go test container on the same network runs the env-gated integration test with repository/module cache read-only and build cache in container `/tmp`. All credentials are fixed public test constants; no `.env`, real keys, image build, cluster, or existing object is used.

**Tech Stack:** Cached redis:7-alpine, cached golang:1.25-alpine, Docker Compose, Go/go-redis, existing QuoteConfigValue and Store New/options builder.

---

## Owned files and exclusions

- Create `testdata/sentinel-auth/credentials.go`, `generate/main.go`, `compose.yaml`, and `README.md`.
- Create `internal/storage/state/redis/sentinel_auth_integration_test.go`.
- Create `scripts/test-redis-sentinel-auth.sh`.
- Create this plan and `docs/testing/2026-09-13-external-sentinel-real-auth-results.md`.
- Never edit production, Helm, the committed feasibility fixture, or other agents' files. No commits, image builds/pulls, deployment, network partition/failover, or production HA completion claim.

### Task 1: Test-first contract and isolation checks

- [x] Define shared public dummy constants with distinct usernames and passwords containing spaces, double quotes, backslash and Unicode. The integration test only enables for `TEST_REDIS_SENTINEL_AUTH_ADDRS=sentinel-0:26379,sentinel-1:26379,sentinel-2:26379`; any other nonempty value fails before dialing, and empty env skips.
- [x] Add the new integration test before constructing the fixture. Use a 75-second overall test context, bounded direct client options and bounded topology polling. Execute wrong-Sentinel and wrong-data Store New subtests before the positive write. Existing credential mapping is already GREEN: do not revert production/other agents' code to manufacture RED. Real wrong-password denial provides fixture authentication enforcement evidence.
- [x] Run `go test ./internal/storage/state/redis -run '^TestSentinelAuthIntegration$' -count=1 -v`; expected SKIP before fixture env is provided, not a claimed real-auth pass.
- [x] Script preflight requires both cached images and empty matching project labels. Generate a non-user-overridable project `sandbox-sentinel-auth-<timestamp>-<pid>-<random>`; force `--env-file /dev/null` so Compose never reads repository `.env`.

### Task 2: Minimal real fixture

- [x] Generator calls `redisbootstrap.QuoteConfigValue` for entire ACL password arguments (`>` plus dummy password), masterauth, Sentinel auth-pass and sentinel-pass. Write six mode-0600 private configs to a freshly created task temp directory. Configs disable default ACL users, grant distinct `data-user`/`sentinel-user` test users, configure masteruser/replica authentication and Sentinel auth-user/peer sentinel-user, stable member announce DNS, AOF and one required healthy replica.

```go
quotedACLPassword, err := redisbootstrap.QuoteConfigValue(">" + fixture.DataPassword)
if err != nil { return err }
// Render the whole ACL argument in double quotes, not >"partial password".
```

- [x] Compose Redis/Sentinel services only copy their own generated config when absent, chmod 0600 and exec the official Redis entrypoint; each has its own named `/data` volume, config bind read-only, and no published ports. Go tools use repository and host GOMODCACHE read-only, GOPROXY=off/GOTOOLCHAIN=local, `/tmp` build cache, no inherited host credentials; generator uses network none.
- [x] Script installs EXIT/INT/TERM cleanup before first container mutation. Before `down --volumes`, validate every matching container's exact project/service label and project-prefixed name, every exact allowed volume name, and the one exact project network. Refuse broad cleanup or unknown targets. After cleanup, all matching resource lists must be empty. Remove only the six explicit generated temp files, then rmdir the task temp directory; never use prune/down-all or broad recursive removal.

### Task 3: Real authentication and ACK evidence

- [x] Poll until master has two online replicas, both replica link statuses are up, and all three Sentinel MASTER/REPLICAS/SENTINELS reports are healthy with two Redis replicas/two usable peers and CKQUORUM reporting three usable Sentinels. This verifies replication, Sentinel-to-Redis and Sentinel peer authenticated PING links rather than just hello discovery.
- [x] On every Redis and Sentinel, unauthenticated PING and cross-end password must return NOAUTH/WRONGPASS. Production Store New with wrong Sentinel password and wrong data password must fail under bounded context; do not log passwords or raw config.
- [x] Correct production Options use distinct DataUsername/Password and SentinelUsername/Password, mode Sentinel, replica_ack, ackReplicas=1 and bounded ackTimeout. Store New must succeed; Store Set and Get must agree. Both direct replicas must eventually read the value, proving actual replication.
- [x] Get a dedicated connection from the production-created go-redis client; SET a fresh key then WAIT 1 3000 on that same connection must return at least one. This is an actual-write offset, never an offset-zero/read-only WAIT.

```go
conn := store.client.(*goredis.Client).Conn()
defer conn.Close()
if err := conn.Set(ctx, key, "actual-write", time.Minute).Err(); err != nil { t.Fatal(err) }
acked, err := conn.Wait(ctx, 1, 3*time.Second).Result()
if err != nil || acked < 1 { t.Fatal("same-connection replica acknowledgement failed") }
```

- [x] Run `bash scripts/test-redis-sentinel-auth.sh` and record actual versions, negative denial/positive Store/WAIT results, and post-trap zero-resource confirmation. If cache/config/network/runtime fails, record the real gap and no success claim.
- [x] Run syntax, formatting, env-disabled focused Go tests and `git diff --check`. Keep new fixture/report uncommitted for parent review.

## Self-review

This experiment closes independent special-character ACL credentials and production client discovery/ACK reachability only. It deliberately does not validate built-in Chart construction, clusterID/bootstrap authority, hostname/IP replacement, failover/cold recovery, real Kubernetes scheduling, business Redis, or absolute data durability. The original feasibility fixture remains untouched; use of QuoteConfigValue is executable, not an unverified hand-written quote.

## Execution handoff

Real authentication runs passed three times (5.591s, 5.858s, 4.643s) with actual-write WAIT 1 returning 2, 1, 1. Every success cleaned its exact containers/six volumes/network to zero. Added a bounded optional `--verify-failure-cleanup` argument (not arbitrary command override): self-owned runner exits 42; actual run preserved exit 42, cleaned all exact resources to zero, and omitted authentication PASS. Docker inventory query errors now propagate rather than appearing as empty lists; tools profile is active for cleanup coverage.

Report: `docs/testing/2026-09-13-external-sentinel-real-auth-results.md`. Env-disabled restricted focused Go checks passed 0.720s and race 1.624s (real integration skipped). A broader matching command observed unrelated parallel new mode-validation RED; no production/other agent test file was edited. Script syntax and diff checks passed; current checkout remains uncommitted for parent review.

After the parent completed that independent mode-validation fix, final env-disabled entire Redis package verification passed 2.527s, and package race passed 2.467s. Owned fixture/test/plan/report files are now frozen for parent independent rerun and integration.
