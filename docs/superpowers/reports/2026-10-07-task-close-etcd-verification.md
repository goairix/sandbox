# Task CloseData etcd verification

## Candidate and ownership

Task original BASE `cb5b9002d07759627fbfd96a586fcdea99124fb6`; source candidate `0627000742785536f0d28835c596a308d42a61c2`; review HEAD `f6ed42aa4fcd007e6d292429c5c6fab1a00ff652` adds Task2 documentation only. Source commits are `acc7a13`, `57e0b70`, and test-only `0627000`. Root used complete original-range owned diff,17files, plus12explicit Task2/document exclusions. Independent review acceptance is recorded separately; this verification does not itself accept the task.

Evidence OWN is `.superpowers/sdd/2026-10-07-task-data-gate-closure/`. All failed attempts, command streams, manifests and payloads remain preserved. No cleanup/archive/deletion of OWN has occurred.

## Host checks and custody

Root independently consumed all27 Task3 captured command streams and produced `task-3-evidence/root-command-census.json`, including argv, exit, raw-record SHA256, source/compiler differences and PASS/FAIL/SKIP counts. Final host race ran in Darwin arm64, Go1.25.6, CGO1, with empty GOFLAGS. Its raw13top/43sub PASS contains8 parents whose native children skipped: actual host behavior is5top/32sub PASS, zero FAIL,91native SKIP. Skipped native parents are not server arbitration evidence. Final host package duration2.091s. Vet/build/scoped diff exited0 with empty stdout/stderr.

Those earlier final checks have precisely one current source difference, `task_close_effect_test.go`: candidate002 corrects an assertion comparing byte slices versus json.RawMessage and adds independently derived digest checks. Production source is unchanged. The corrected host selector compiled but skipped without the native fixture; its vet/diff passed. Candidate002 native execution below covers the corrected test. No claim is made that earlier host checks tested later test bytes or that a full repository suite ran.

Root candidate002 audit verified724 current/copied/frozen Git source files and3 compiler files. Manifest SHA256 `cc1604bcabb01338fd45850fad2b2f2791175157db701476e239b2f057d42b91`. Each native batch separately binds176 actual compiled repository files and3 compilers to this candidate/current source. The724file census is source custody, not724 tested files. Module-cache and all standard-library source are not claimed as exhaustive forensic custody.

## Real three-member native arbitration

Root exclusively provisioned fresh isolated owned fixtures; author launched no external services. Local pinned etcd3.6.15 Linuxarm64 image RepoDigest `sha256:5ed4e32061aa061f970d6500629213e8579ef17571dcf9789074966acb9afad1` and imageID `sha256:ffe670d574b95a6bd22d4a2fd8b72a36c1d5893fe6eba4958836192a8bd1e50c` were inspected. No image pull or source-build provenance is claimed. Three actual members and a separate foreign cluster ran with64MiB memory,.5CPU,128PID, read-only root, drop-all capabilities, no-new-privileges and owned volumes/network. HTTP loopback fixtures do not establish production TLS/RBAC.

Each native Go selector used host race, GOMAXPROCS2, count1 and90s timeout. Seven fresh disjoint accepted batches executed once at candidate002:

| Batch | Selector scope | Top PASS | Sub PASS | Actual namespace fixtures |
|---|---|---:|---:|---:|
| A | Success / Signing / Budget |3|12|12|
| B | Unknown / Postcommit / Responses |3|17|17|
| C | Read / Delayed |2|13|13|
| D | Fences task / intent / link / placement |1|20|16|
| E | Fences control / owner / workspace-fence / runtime-index |1|20|16|
| F | Fences claim / guard / issuer |1|15|12|
| G | Shared Exec issuer registry / reply loss / corrupt Else |3|3|5|

Total14batch-top/100sub PASS, zero FAIL/SKIP,91 actual namespace fixture triplets;294 captured raw commands all exit0. Each fixture logged3 distinct member IDs in the expected cluster. Final member status/health and foreign separation were checked before teardown. No OOM; all owned container/network/volume inventories empty after teardown. Aggregate SHA256 `2d723ba7c96334cf0475814814485fe2d78118e51d221a46b8fe159ed5fb988e`, `task-3-evidence/root-candidate002-native-audit.json`. Per-batch actual selectors, command hashes, full logs/status/source manifest are retained under `root-fixture-evidence/<project>/`.

## Warnings and errors retained

Accepted native results are not pristine. B emitted1 client WARN, C7. Seven LeaseRevoke NotFound events were deliberate repeat cleanup of already revoked/expired leases; the eighth event is C/expiry KV/Txn DeadlineExceeded on the original bounded context. B/cleanup first really revoked the Stage guard then injected reply loss; retry acknowledges the already missing guard. C/revoke,new-claim,expiry,after-claim-release and read/new-claim preserve original captured cleanup identity. C/expiry also sends the exact original native comparison/write shape under a fresh bounded context while the existing Stage guard is still present: refusal comes from missing original claim, not solely the expired request context. No automatic new claim/command/intent is inferred.

The seven missing-lease revokes generated21 replicated server apply WARN (B3,C18), each exact error `lease not found` and lease_revoke operation. Every batch also emitted16 startup WARN:8 shared HTTP/gRPC port,4 fresh-volume0755 permission and4 simple-token notices. These are isolated fixture characteristics, not production configuration approval.

E and F each emitted2 schema-detection WARN and1 storage-update ERROR on etcd-3 (`missing term information`). E events11:45:22.794874–.797003 UTC preceded compose healthy wait completion11:45:24.216138 and test start11:45:24.712984. F events11:45:37.024272–.025328 preceded healthy wait11:45:38.575662 and test start11:45:39.142271. All4 final statuses in each batch reported server3.6.15/storage3.6.0, healthy and no OOM. Exact timestamps/stack traces remain in `root-server-anomaly-disposition.json` and raw logs.

Primary pinned [schema code](https://github.com/etcd-io/etcd/blob/v3.6.15/server/storage/schema/schema.go), [version monitor](https://github.com/etcd-io/etcd/blob/v3.6.15/server/etcdserver/version/monitor.go) and [server monitor](https://github.com/etcd-io/etcd/blob/v3.6.15/server/etcdserver/server.go) support a bootstrap-timing inference: detection can reject absent stored version plus term0; the monitor logs an update error, and later monitoring may trigger again. This is an inference, not demonstrated root cause of these binaries or a proven recovery trace. Finite native metadata tests do not prove rolling upgrades, restores, long-running storage health or production capacity. The independent reviewer must assess this explicit limitation; tests were not replayed to erase log noise.

## Failed attempts and fixes

1. Schema/effect tests-first failed compilation because APIs did not exist. Neither is a behavioral RED.
2. Native fault-test compilation initially used the wrong Revoke-hook signature; corrected in author-owned tests before native execution.
3. Record-birth tests produced three genuine expected-error-got-nil failures for claim-at-intent, issuer-at-intent and issuer-after-intent. Strict birth-order validation fixed them; focused race1top/3sub passed.
4. Root first native preparation used a project name rejected by the existing strict fixture guard; tests failed before metadata provisioning. Actual members were healthy, teardown empty. Not native metadata proof.
5. Candidate001 A executed Budget/Signing successfully; Success failed because equal bytes were compared as different Go concrete types. Author test-only0627000 fixed it and added independent digest assertions. Candidate002 accepted A is a genuine later execution; failed candidate001 streams remain.
6. Author source audit first asserted unchanged HEAD; Task2 documentation advanced HEAD and the check failed. Later corrected source-byte/Git audit verified724 files and3 compilers at candidate002/current docs-only HEAD. The failed metadata record remains.
7. Root log classifiers initially expected pristine logs, rejected normal inherited cost T.Log, then incorrectly compared client and server missing-lease wording. Exact stream/operation classifications were corrected without rerunning any test. These parser failures are not Go test failures and are retained in progress chronology.

## Scope retained for downstream work

This proves strict task-close intent schema, original TaskClaim fencing, immutable issuer registration, conservative budget59 versus actual58 native wire nodes, original Stage reconciliation and copied diagnostic fixed-key history under the named finite tests. It does not close a physical target, grant delivery authority from history, establish durable runtime receipt, drain accepted executions/etcd operations, terminate an incarnation, settle remote effects or release owner/control/index. Task4 authenticated transport/query, subsequent CloseAll/dual drain/final sync+flush/remote settlement or fencing/exact termination/safe release, production wiring/Redis removal and large-existing-N capacity gates remain mandatory. No fleet performance gain is claimed.
