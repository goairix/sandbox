# Task4 protected target verification

Candidate `6fe6b3edaebd4cb1d54569e0106009f37ef98088`; Task4 original BASE `e684e1510e9dc6d60a75a4aa7cb4be0f0606a0ed`. This is a focused physical target gate, not independent Task4 acceptance or complete migration. Backend delivery source continues separately and was not part of this standalone target execution.

## Actual Linux target

Root created owned project `task-close-journal-235f663323a5` (historical runner project prefix, mode is Task4 protected target). Exact new selector `^TestTaskCloseNative$` executed in a real Linux arm64 PID1, Go1.25.6 CGO0 non-race test ELF. Production sandbox-launcher monitor and existing user fixture were separately built from the same candidate. Host race evidence is separate.

1 top PASS, zero subtests/FAIL/SKIP,0.53s; all41 raw commands exit0, no timeout and no stderr. Full raw `030-linux-task4-target-tests.json` SHA256 `58f56add18f879844d3d4a5e15041653e5da9fdedda765a9a0332af00250ee4a`. Actual test assertions exercised activated mTLS task close, verified durable data_closed receipt/history query, valid original late-start replay refusal, malformed renew refusal and retained preaccepted command's actual monitor/user/local_terminal ownership. No metadata End or remote settlement was produced.

Actual kernel snapshot: PID1, all UIDs/GIDs0, Permitted/Effective/Inheritable224 (0xe0), Ambient/Bounding0, CapLast40, NNPtrue, Dumpable0, Subreapertrue, Securebits0,5threads. Actual monitor PID12 and root process PID19 reached retained local_terminal after closure. These are measured finite test facts, not fleet resource costs.

The fixture kept64MiB memory,.5CPU,128PID, read-only root, no-new-privileges, networknone (own loopback mTLS), dropALL with precisely KILL/SETGID/SETUID/SETPCAP for initial bootstrap. Root inspected Docker canonical CAP_ values; bootstrap verifies actual kernel state. Journal tmpfs64MiB root0700 and temporary tmpfs64MiB mode1777; owned payload volume read-only to target. No privileged container, added network access or quota increase.

Pinned local Linuxarm64 etcd3.6.15 image `gcr.io/etcd-development/etcd:v3.6.15@sha256:5ed4e32061aa061f970d6500629213e8579ef17571dcf9789074966acb9afad1` only supplies the container base; no image pull or image/source build provenance claim.

## Frozen source and payload custody

Root combined actual dependency lists for controlrunner tests, production cmd/sandbox-launcher and controlrunner/testdata/user. All101 compiled repository source/module files and3 compiler files match current bytes and candidate Git archive. Final source manifest SHA256 `3dae1909e9e629a5406c956053d2b8d38eaaec1c1b93d9d900c1f63510812cce`; per-package manifests also retained. Current source postcheck has no drift. Module cache/complete stdlib forensic custody is not claimed.

Three root-owned0755 ELFs were copied to the owned daemon volume then exported and inspected byte-for-byte; each payload tar retained:

| Payload | Bytes | SHA256 |
|---|---:|---|
| controlrunner.test |9802826|f19878382a2ddee4e4a44ca8e267f69808a0a2dae42f7334ea405c57b03d749b|
| sandbox-launcher |7981601|8babb239893dd1b2794bff0f0336ce0aa9b624b4bcd0651990d9cc24069ee184|
| user |3827118|e9dc8394b9b439e3b5ec3fea9f791b654e2e5de2f3198ee0c84eb42ab9bef96a|

Root constructed an inspect-bound runtimeID/runtimeUID/container image/capability/resource/payload contract, copied0400 attestation and independently exported it. Attestation SHA256 `c059c92cb34ad41801b76d17acdca6259ca9b47566252111f07d09d1f0d3b76e`; contract literal bytes hash and exact containerID match. Test root/issuer keys are generated only inside the test ELF, not production test modes or supplied runtime commands.

Final actual container ExitCode0, not running, OOMKilledfalse; owned container/network/volume inventories all empty. Root independent audit and all full command streams, manifests, ELF payloads and attestation remain under `.superpowers/sdd/2026-10-07-task-data-gate-closure/root-fixture-evidence/task-close-journal-235f663323a5/`.

## Failed preparation retained

Initial project `task-close-journal-6ea94da597d0` failed before target start/attestation when Root compared bare capability names to Docker's canonical CAP_KILL/CAP_SETUID/CAP_SETGID/CAP_SETPCAP. All35 raw commands exited0, all three payload custody checks succeeded, no source drift/OOM, final inventories empty; it is a failed Root preflight, not native product proof. Root fixed only exact capability-name parsing, retained the failed gate and ran the fresh project above. No product fix, omitted warning or increased privilege/quota.

## Explicit remaining scope

This native case uses a malformed renew ticket and does not independently prove refusal of an otherwise valid signed renew; a distinct valid-renew closure case and concurrent query/shutdown ownership case remain required. Standalone actual target plus future actual three-member backend delivery are compositional evidence, not one combined protected-target/metadata fixture. Three-member delivery, independent Task4 original-range review, whole-unit unfiltered review, CloseAll/dual drain/final sync+flush/exact termination/remote settlement or fencing/safe owner release, production wiring/Redis removal and large-existing-N/long-run capacity remain mandatory. This gate establishes neither owner release nor performance improvement. All OWN remains retained.

## Additional new-case native gates

At candidate `18871f3bec12f58fcfd9b346d65aa6632fcbe1b3`, Root ran two new selectors; it did not replay the previously accepted TestTaskCloseNative.

| Gate | Actual result | Raw commands | Compiled repo / compiler files |
|---|---|---:|---:|
| `^TestTaskCloseNativeQueryShutdown$`, project `task-close-journal-f3913dc0ec88` |1top/0sub PASS,0 FAIL/SKIP,0.03s|41 all exit0|101 /3|
| `^TestJournalTaskCloseReceipt`, project `task-close-journal-79a15592b191` |4top/4sub PASS,0 FAIL/SKIP|29 all exit0|52 /3|

Both run Linuxarm64 Go1.25.6 CGO0 non-race under unchanged resource limits; all raw stdout/stderr hashes, source/current/candidate Git bytes, payload export custody and final state independently audited. No stderr, timeout or OOM; all three owned cleanup inventories empty. Complete original failures remain. The query/shutdown test reused actual production monitor/user and inspect-bound protected PID1 fixture; the journal test requires no extra capabilities or kernel supervisor.

The new query/shutdown case starts a real hold user, waits for execution readiness, successfully sends a valid signed renewal, closes data admission, and refuses the identical valid renewal. It asserts the same active pointer/root/monitor, a live root PID, and outstanding owner/Wait channels across close; an active exec query remains unknown. A canceled authenticated task query retains its borrower until its context-honoring clock exits. Actual public Close seals new borrowers, joins the query/owner/Wait and only then clears key/journal. Actual kernel snapshot remains PID1/P=E=I224/B0/NNPtrue/Subreapertrue/Dumpable0. These assertions strengthen the original shorter preaccepted-command case; the original malformed-renew result is not rewritten as valid-renew evidence.

Journal cases cover historical receipt after ticket interval, readable terminal after uncertain terminal directory-fsync refusing signature, lock-serialized poison before/after signing and late clock/context failure returning no bytes. These are actual Linux protected IO plus named hooks, not physical power-loss experiments.

Query/shutdown source-freeze SHA256 `f2de404bc93dba72fd88ebdcd51e0337be75afca0482bfd7826314bee37439f8`; raw030 SHA256 `bdd4504fe3713144a191e959f0385305be05b3108513b5eb6039e61df88ffbb5`. Journal source-freeze `38a8167ff38c379ba82ef0bdf42e9f58cebe2874b804704413224dc4b4706080`; raw018 `d18d09fda4dc4d062bb61a3337261facb2761ca41a2cab91fe8b2b90e36004aa`. Raw streams, independent audits and manifests remain in each OWN/root-fixture-evidence project directory. Earlier target production is unchanged; its native test file later gains this new case. Source attribution stays per candidate, not an assertion that every older test file matches final bytes.

The parallel first three-member metadata delivery attempt at18871f3 failed six native subcases during invalid test activation construction, before delivery. It is recorded separately; the corrected candidate767afb9 subsequently passed all four top-level and six subtests. See the delivery verification report for the original failure, exact correction and accepted raw evidence. Task4 independent review and the complete migration remain pending.
