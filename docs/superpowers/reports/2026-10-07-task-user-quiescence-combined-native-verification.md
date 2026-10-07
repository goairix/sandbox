# Task-user quiescence: actual combined native verification

Status: **partial physical Active evidence, not Task5/unit/production acceptance**. The first actual test returned PASS, but Root independently found its namespace invariant snapshot was vacuous. Corrected coverage and a changed Active run are required. LateArrival/Unknown and the full original-unit review remain pending.

## First Active fixture — retained partial result

- Candidate `05615b4505b202c58d9c228d78d87c9aec90f700`; Task5 original BASE `cf58a66051ec1f7b1ea1dc4c8bc7c376c93c9857`; full unit BASE `3f1e2698ffaa0b46f427291ae66c6c72c1e1b6e2`.
- Fixture `sandbox-quiesce-combined-0e054d6e3b3b`, selector `^TestTaskDualDrainNativeActive$`: actual Linux arm64 CGO0 **1 top PASS, 0 subtests, 0 FAIL/SKIP**, 30.83s including natural native lease expiry. This is the test's actual outcome, not acceptance of its incomplete invariant assertion.
- Real production `/native/sandbox-launcher serve` PID1; target Runtime ID=UID `767fea14a7d50d6d4e3b718a71db586deccb5e816940bc8407d0d7ddc6cf74e2`; Boot `d6ef35c4-571b-4b5f-b20a-0e6f7bc50f19`.
- Actual three-member etcd, separate bounded backend/metadata relay, exactly eight named volumes, internal metadata network, no host ports, no metadata/private mount or route in target. Initializer/custody removed before target measurement. Infrastructure helpers are not counted as combined PASS.
- Target and auxiliary budgets remain 64MiB, .5CPU, PID128. Target has read-only root/payload, network none, NNP, default security and only the four approved bootstrap capabilities; normal production bootstrap is used. Server quotas remain inherited unset, explicitly separate from target. No target debug exec or production test mode.

Root verified all **143 command records** with complete stdout/stderr lengths/hashes, exit0 and no timeout, archived commit/source, **281 actual compiled repository inputs**, go.mod/go.sum, installed Go1.25.6 and three Go tool hashes, effective Linux arm64 CGO0/GOPROXY=off environment, three ELF payloads and daemon readback ownership/modes. Generated wrappers, standard library and module cache are not independently supply-chain certified.

| Root-built artifact | Bytes | SHA256 |
|---|---:|---|
| combined.test | 32431516 | d0f0b43bb508ec94b76cdc6241ee613cb9dbb577510f9af017578cee8114ba96 |
| sandbox-launcher | 8229196 | e18e9724771d2776e2d6deedf4ef1acf707deaa45746c5a4f07e4aa6222a8bc4 |
| combined-user | 3719582 | b7ca55848c50823e662284fb8bcf32b27f1c09019ec0d6813a06b9992b043d9f |

These are Root's archive-built/custodied binaries, distinct from the author's checkout-built outputs. Compiled manifest SHA256 `7ee9939fb9be127de7886ef73172fc06d63b89697d6b8ecb3714652cd77d9b97`. Author's selected Darwin race run had three native SKIPs and is only host compilation/race instrumentation evidence; it is not this physical run.

## Supported physical observations and the coverage defect

The genuine public creation/publication/bootstrap path activates the actual fresh target birth. The UID1000 probe observes DAC denial for protected control/clock sockets and absence of target metadata/private mounts. Original hold and doublefork executions are live in actual PID1 census before and after destroying/DATA closure. Genuine signed USERS terminal evidence has registered2/local_terminal2/never_spawned0 and is compared with both original verified execution receipts. Live token plus local terminal returns no prerequisite. Original30s leases disappear naturally with actual key absence and native TTL=-1; the new public dual-drain prerequisite records final empty revision50, immutable reservation revision43, common intent/receipt birth45 and literal53 comparisons+two whole-prefix ranges=55 nodes. Authenticated management query still works after users drain.

**Root found an important test coverage defect:** `Namespace.Root()` already ends in `/`, but `snapshot()` appends another `/`. The actual raw record set before/after contains only one doubled-slash `//fixture-health` key (backend log lines31/50). It does not observe real owner/control/index/task/remote records. Thus the namespace unchanged-state assertion cannot establish its advertised property. The first native PASS is retained as partial physical-chain evidence and cannot close Active/Task5. Original author is correcting the canonical prefix, adding nonvacuous required-record assertions, retaining a meaningful RED regression and repairing failure cleanup before a scoped Active rerun. No product state-manager source is changed by this finding.

Exact registered-before-spawn/Start-in-flight timing remains separately attributed to accepted Task2 component evidence; this public run does establish actual original live-owner crossing. It does not claim that exact component timing was physically forced.

## Finite diagnostics — one fixture, not capacity or performance acceptance

Each sample includes an authenticated diagnostic connection and its FD/goroutine overhead. These are actual measured states; no fleet/N extrapolation or uninstrumented benchmark is made.

| Phase | Active owners | PID1 RSS kB | Cgroup memory bytes | Cgroup PIDs |
|---|---:|---:|---:|---:|
| Activated idle | 0 | 9728 | 6582272 | 7 |
| After destroy/DATA close | 2 | 12628 | 27324416 | 31 |
| Attempted active quiescence | 2 | 12740 | 19755008 | 22 |
| Users drained, management alive | 0 | 13268 | 9560064 | 9 |

The attempted active sample really observed active2 at `2026-10-07T20:17:25.879536002Z`; it is not mislabeled terminal. Census/cgroup fields are sampled during concurrent shutdown and are not an atomic allocation benchmark. Final target census contains only PID1. This run does not measure throughput, capacity, large-N stability, Linux race instrumentation or improvement over Redis.

## Logs, cleanup and retained attribution

Actual client JSON WARN/ERROR count0. Three server logs retain12 WARN: mixed HTTP/gRPC port6, data directory0755 permission3, unsigned simple token3; no server ERROR/FATAL/PANIC. They are fixture configuration warnings, not production TLS/RBAC/deployment approval.

Backend bridge reports48 before-stop observations and relay52; each retains16 details and the complete count, leaving32/36 details unretained under the selected bound. Retained origins distinguish EOF, first failure and partner error after pair close; no claim that every overflow detail is individually classified or that logs are pristine. Both bridges join with zero pairs/three listeners. Target and relay cooperative teardown exit0/nonOOM. Final exact-label container/volume/network inventories are all empty. Fixture disposal is not product runtime termination, remote settlement or owner release.

Full raw evidence stays in the current unit's protected OWN under `root-fixture-evidence/sandbox-quiesce-combined-0e054d6e3b3b/`: records, source/archive/tool manifests, payload tar/readback, daemon inspections, initializer protected inventory, logs, result, cleanup and Root audit. Private key-containing custody remains0700/0600 and is never copied into this report. Authoritative backend raw record69 is54352 bytes, SHA256 `c4ec8b646d631914c66e1ce07abfe88082f12a79f79e70c951bfafba34e4b32f`; repeated full logs are byte-identical. Root independently audited all records; helper assertion success is not used to paper over the discovered snapshot defect.

Next gates: corrected Active coverage, LateArrival's original expired packet and current-certificate refusal, Unknown's genuine ACK loss/same-original query and declared failed-target/stale-socket refusal, then one full new-unit original-range review. Fresh different-Boot recovery remains separately required in later deployment/recovery work; the selected stale-socket failure phase creates no new Boot/key. Final sync/flush/remote settlement/End/safe release/production wiring/scheduler/FUSE/collector/N capacity/deployment/Redis removal remain mandatory downstream.
