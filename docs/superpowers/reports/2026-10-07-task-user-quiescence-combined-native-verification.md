# Task-user quiescence: actual combined native verification

Status: **corrected Active and LateArrival physical assertions pass; Task5/unit/production acceptance pending**. The first Active run's vacuous snapshot and first Late codec failure remain retained defects. Changed-source native runs below verify their corrected assertions. Unknown and the full original-unit review remain pending.

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

## Corrected Active — actual nonvacuous invariant

Candidate `bb64c11765649b88cba9c65ed6f825ac05a7f1e9`, fixture `sandbox-quiesce-combined-14d86783b6d5`, same fixed Active selector: **1 top PASS, 0 subtests, 0 FAIL/SKIP**. All143 recorded commands exit0 without timeout. Root independently checks raw stream hashes/lengths, source archive,281compiled repository inputs,3Go tools, effective build environment, image and daemon payload custody. Manifest SHA256 `5f767dc573a8418b3d0f337e5a89bda05d4e1316078445e3d44042688d801a73`; authoritative backend record69 is214162bytes, SHA256 `a1a28ec920f1eb581656c3e824fb2e58d510ce31f5e8848b285f12415fc99b4f`.

Actual Runtime ID=UID `4dfde4b277df70b0b95354cf033f89a6f353ec4e8545f61f1d29bfe90ca7b700`, Boot `cf450b89-c50e-42b5-ba05-f08ddeee3a08`. Target uses the same production entrypoint/resource/security contract and is running/nonOOM after assertions. Actual attempted quiescence sample has active2, RSS12708kB, cgroup19406848bytes/PIDs22, including the declared diagnostic overhead. This is one measured fixture, not capacity or performance acceptance.

| Root-built artifact | Bytes | SHA256 |
|---|---:|---|
| combined.test | 32434443 | 91ae6479ded09fd1e16926b63695ca931027d77e3a037205724bd6a7e8fb909a |
| sandbox-launcher | 8229188 | b03dcff96d23658837309289cd646c619474bdb9d74ad921e836f9e1ff3a922f |
| combined-user | 3719582 | 913c5aaff3fe19492cd1bd0b89e40cd9bacc368af8a0e11616482dc50c3d1c63 |

The canonical namespace snapshots contain50records at revision42 (49055key/value bytes) and42records at revision50 (49863bytes), both More=false. Each independently logged20required anchors is present with matching Create/Mod/Lease. Root parses complete native record groups and independently confirms39retained records are byte/native-birth/lease identical. Exactly3unleased immutable additions are the original quiescence reservation (birth43), intent and StageReceipt (birth45). Exactly11missing records match declared original native lease expiry: creation claim+guard2, and three original operation token+guard+receipt groups9. No retained owner/control/index/task/remote record changes. The source's permanent claimant fence checks also pass. This repairs the prior nonce-only snapshot coverage; it does not upgrade the prior candidate's result.

Fresh native token absence/TTL=-1 plus signed local terminal evidence yields the public prerequisite under the same original claimant and literal55nodes. Historical terminal queries retain the original signed interval even after ticket NotAfter, while verifying current runtime certificate and original claimant; effect delivery still requires its original live deadline. No old ticket, worker deadline or trust interval is extended.

Client JSON WARN/ERROR count0. Server severity is **14 WARN, 1 ERROR, 0 FATAL/PANIC**, not15WARN. Twelve WARN match the first fixture configuration categories. Additional etcd-3 startup records are `etcdserver/server.go:2334` at20:40:09.509799Z and20:40:09.520412Z, `Failed to detect schema version`, error `missing term information`; `version/monitor.go:120` at20:40:09.520792Z reports `failed to update storage version`, cluster-version3.6.0, `cannot detect storage schema version: missing term information`. Exact raw records and stack remain retained in backend-independent member log record89/stderr and `root-server-warnings.json`. Actual member Status/MemberList/quorum replication passes are separate facts. No pristine log or storage-version recovery acceptance is claimed.

Backend bridge total50/retained16/omitted34 and relay52/16/36 preserve the bounded-observation limit. Both joins are0pairs/3listeners. Target and relay cooperative teardown exit0/nonOOM; exact final container/volume/network inventories are empty. Updated independent audit exit0 retains the full snapshot delta and explicit severity counts in `root-fixture-evidence/sandbox-quiesce-combined-14d86783b6d5/root-audit.json`. The earlier `server_warning_count` aggregate field counts WARN and ERROR; explicit severity fields disambiguate it. Component physical assertions pass; original-range independent review and downstream gates remain pending.

## First LateArrival — real failure retained

Source commit `0039d560e1646c8bd9a965c98769cdec4c444a39` adds retained original canonical request bytes, expired-original Backend refusal, delayed-input rejection and65s original runtime certificate expiry assertions. Host physical selectors SKIP; host race/vet and Linux ordinary-package compilation alone do not verify these native assertions.

Actual fixture `sandbox-quiesce-combined-9dcfe8d5fde0`, exact selector `^TestTaskDualDrainNativeLateArrival$`, fails after0.28s at its request roundtrip equality: production decoding normalizes absent stdin to an empty slice, while the original StartEnvelope contains nil. No delayed-input/current-expiry PASS follows. Root runner exits1; complete raw failure is retained, without treating the codec assertion failure as a product dispatch error. Exact-label cleanup finishes with all three inventories empty. Original author is fixing a meaningful canonical codec comparison with a focused regression before another changed-source native run. Unknown has not been replayed or implemented by another agent.

## Corrected LateArrival — original packet and real certificate expiry

Canonical roundtrip repair `320e3a435281d3cb913458316db93963bb6f77b8` retains complete original and decoded re-encoded header/body bytes and hash, plus mandatory exact EOF. Focused codec RED reproduces the old nil/empty equality failure; GREEN verifies actual wire semantics and trailing-byte refusal. Author's host race has2host PASS/1native SKIP, vet/Linux full-package compilation exit0. No host SKIP is counted below.

Changed fixture `sandbox-quiesce-combined-2ee34dbbbeb8`, exact LateArrival selector: **1 top PASS, 0 subtests, 0 FAIL/SKIP**,67.30s. All143command records exit0 without timeout;281repository inputs/3Go tools/3ELFs and full archive/custody are independently audited. Runtime ID=UID `3a2b445ad1a58a0103d46b1bcc103c1ac98bd7beed48de2f88b9ab95bed7b3b8`; Boot `139fcf1e-6867-4dc4-a86d-7108b92b565e`. Manifest SHA256 `c48d0a39b8a770d4f207697e7a2213ee939f9996e65ebc864c813de237a6b6f0`; complete backend record69 is160483bytes/SHA256 `5cd4934489fc47cbfeb4cf7cc4e7abfcd205524718f0d7ee8ecb09571ca1743e`.

| Root-built artifact | Bytes | SHA256 |
|---|---:|---|
| combined.test | 32507184 | fc9727957877fecf1ca6ece4feeed0eac400cf0105a1a174d91be9bb51dce979 |
| sandbox-launcher | 8229196 | ce90db7e132770aaafc88534f274189cd8a3430c087a0bfeda3b019fb0b4f4f8 |
| combined-user | 3719582 | 34164d9d44c2af38808090023deed19afcd13b8a8be3edd96738ae1aac6f0e2b |

The retained actual original exec frame is4192bytes/SHA256 `12973ea6463362f53362be41ef1c368350241b3b72c9cb259da492ef1a882375`. Native original30s token absence and TTL=-1 precede Backend refusal with0dials/application bytes. A separate fresh genuine bounded mTLS connection sends those exact original bytes after DATA/USERS close and native absence at revision38; actual event_kind5 returns `request refused or outcome unknown`. PID1 diagnostics before/after have identical PID1-only process sets, active0/closed. Signed original terminal has registered/local/never counts all0 and canonical empty execution-set digest; fresh same-original query and public dual-drain prerequisite succeed while current certificate is valid. Ticket expiry and closed gates remain possible causes of delayed refusal; closure-only causation and exact beforeSpawn/Start-in-flight physical timing remain CannotVerify, with Task2 component timing attributed separately.

Original runtime certificate/activation end is20:55:26.130571833Z, selected as original activation-time+65s before activation. Actual independent current UTC20:55:28.264028503Z follows that end. Fresh TLS fails after1real dial; public Query/prerequisite reject current expired certificate before any dial and return no evidence/prerequisite. No lease/ticket/send deadline, clock or trust interval is widened. Target remains running/nonOOM after assertions; successful authenticated management samples precede expiry. Post-expiry daemon liveness is distinct from successful authentication.

Root parses full snapshot40keys/revision32/35677bytes and38keys/revision38/42235bytes, More=false,18required anchors each:35retained whole-KVs unchanged,5declared original leased removals and3unleased immutable quiesce additions. Root audit exits0 after adapting its Python UTC reader to preserve RFC3339 nanoseconds; that auditor parsing issue required no native source change or replay. Actual client WARN/ERROR0; server12WARN/0ERROR in the same three fixture categories. Backend bridge50/16/34omitted and relay53/16/37omitted remain bounded observations, not individual classification of every overflow. Joins0pairs/3listeners, cooperative target/relay teardown0/nonOOM and all three final inventories empty. No Linux race, large-N, throughput, flush/release, recovery or production migration acceptance follows.
