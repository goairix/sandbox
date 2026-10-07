# Root native verification — finite authenticated transport and dual drain

Final status: Task4 components accepted at source `fc605d6` after scoped I1 review; full dispositions are in [transport-review](2026-10-07-task-user-quiescence-transport-review.md). The baseline below preserves its original source and pending-review attribution. Task5 and full production migration remain pending.

Frozen source `7fdb188c1eda42b713cc38e07259dfb9c6a79753`, original Task4 BASE `acee0c8c12e3cf590ecef903dc92fa07ebd9c0f8`. These are separate component results. Task4 remains unaccepted while its independent original-range review and strict five-second write-deadline correction are pending.

## Actual execution

Root ran four fresh batches once each, using the already installed Go1.25.6 and pinned Linux arm64 etcd3.6.15 image digest `sha256:5ed4e32061aa061f970d6500629213e8579ef17571dcf9789074966acb9afad1`. No accepted predecessor gate was replayed.

| Batch | Actual coverage | Top/sub PASS | Actual fixtures | Project |
| --- | --- | --- | ---: | --- |
| Protected PID1 | Original genuine terminal before ACK; real mTLS/EOF effect fallback and query; management remains usable; foreign attribution refused | 1/0 | 1 | `task-quiesce-local-4377d3ad0791` |
| Metadata delivery | Lost ACK preserves fixed reference/no resend; original claim loss during handshake and immediately before bytes | 3/0 | 3 | `sandbox-etcd-state-test-73019-1791398088096231000` |
| Metadata conjunction | Empty tokens plus unknown local result refuses; authenticated terminal plus nonempty tokens refuses; actual zero permits new local prerequisite; old send remains refused | 1/2 | 2 | `sandbox-etcd-state-test-73978-1791398297353693000` |
| Final native barrier | Same-byte receipt/reservation rewrite, original claim loss and late cancellation at the new full-prefix read yield nil prerequisite | 1/4 | 4 | `sandbox-etcd-state-test-75146-1791398561601676000` |

Total six top-level and six nested PASS names, nine actual metadata fixtures and one protected PID1 fixture, zero FAIL/SKIP. Nested and grouping PASS names do not increase the number of actual fixtures. Metadata native output is actual Darwin arm64 CGO1 race execution against Linux etcd; protected PID1 is Linux arm64 CGO0 execution, without Linux race instrumentation.

Metadata fixtures use protocol-created, genuinely signed accepted/terminal wire to test the backend consumer. Those wire producers do not prove process termination. The protected PID1 test first invokes the original private verified closure entry and waits for its genuine terminal producer, then sends the exact effect and query through real Supervisor.Serve/mTLS. It establishes genuine completed-original fallback, not a forced concurrent public begin-to-ACK race or the public Backend-to-production-launcher chain. Task5 still owes that same physical combined chain.

## Source, custody and resources

Current OWN `root-task4-native-audit.json` and `audit_task4_native.py` retain Root's full read-only audit. All128 recorded commands exited0 without timeout; both raw streams' lengths and SHA256 match their records. The complete committed archive matches its source manifest. Each metadata batch checked244 actual repository Go/Cgo/assembly inputs, go.mod/go.sum, four installed Go tools and its exact compiled binary. The protected batch checked121 actual repository inputs, module inputs, three Go tools, and three Linux arm64 ELF payloads. Generated wrappers, standard library, module-cache dependencies and the native C toolchain are not separately supply-chain certified.

The daemon-returned payload tar proves root UID/GID, executable0755, attestation0600 and exact payload bytes. Compact attestation bytes match their custody hash; the host pretty JSON is semantically identical. Exact runtime UID/container ID is `281522c8bf2cbee5dba961d12db74be6c4ac0ea43b448e215af49afdc89c0e7d`. Image ID is `sha256:ffe670d574b95a6bd22d4a2fd8b72a36c1d5893fe6eba4958836192a8bd1e50c`.

The actual PID1 inspection and kernel observation retain64MiB memory,0.5CPU,128 PID limit, read-only rootfs, isolated network/PID namespace, no-new-privileges, drop ALL plus only KILL/SETUID/SETGID/SETPCAP bootstrap capabilities, unchanged default security policy and the approved journal/tmp tmpfs. Kernel output confirms PID1, non-dumpability/subreaper and unchanged privilege contract. Original namespace census and non-reaping ECHILD are revalidated; this zero-user fixture reports registered/never/local counts0. The whole test ran approximately20ms; it is not a throughput or capacity measurement.

All three metadata clusters retain exact three unique container/member identities, one positive cluster identity per batch, correct endpoint mappings and3.6.15 status observations for all nine per-case fixtures. Members remained running/healthy without OOM. Their unchanged correctness-fixture settings are Memory0/NanoCpus0/PidsLimit unset; they do not validate production server quotas.

Every batch retained before/after inspections and empty scoped container, volume and network inventories after owned cleanup. The one-line protected runner allowlist amendment only adds the exact new selector; it changes no privilege, quota, topology or timeout.

## Retained warnings

The original raw streams and per-event coordinates remain in `root-task4-client-warnings.json` and `root-task4-server-warnings.json`. No logger suppression or pristine-output claim.

| Class | Count | Current evidence and limit |
| --- | ---: | --- |
| Client LeaseRevoke NotFound | 14 | Exact test/record/JSON-row mapping retained. Nine repeated initial CloseData-claim cleanups, three deliberately revoked current-claim cleanups, one released original-claim cleanup and one original operation-lease cleanup. Client logs lack lease IDs, so ownership is source/order inference; server logs independently identify14 exact request/lease pairs replicated on three members. |
| Client KV/Txn Canceled | 1 | Actual `late-cancel` final-barrier fault, record24 JSON row28. Cancellation refusal asserts nil prerequisite and fired fault boundary. |
| Server failed apply / lease not found | 42 | Only this failed-apply error class observed; three replicas for each of14 missing-lease revokes. |
| Server mixed HTTP/gRPC port | 18 | Unchanged correctness fixture startup, two per member. No production TLS/RBAC acceptance. |
| Server data directory0755 | 9 | Unchanged fixture named-volume startup, one per member. Production data permissions remain deployment work. |
| Server unsigned simple token | 9 | Unchanged fixture startup, one per member. No production authentication acceptance. |

No server error/fatal/panic event was observed in these four batches. Warning attribution is separate from product review acceptance.

## Remaining scope

Original-range review found the inherited event writer can replace the fixed receive deadline with now+5seconds; the generic error writer can add another second. Passing component cases do not establish the five-second bound for a slow reader. This issue requires a targeted original-author fix and independent scoped review, plus actual changed-path verification.

Task5 combined public chain, safe final sync/remote settlement/End/exact runtime termination/release, production API/Pool/Docker/Upload wiring, partition scheduler, FUSE/collector, large-existing-sandbox capacity, deployment/restore and Redis removal remain mandatory. Neither token absence, this local prerequisite nor these separate component passes establish whole production migration or performance improvement.

## Fix round1 — changed finite writer

Original author committed `fc605d69a371f846534a2e2c334c1f2bac1fb001`: all four quiesce reply branches consume the original bounded context deadline; generic errors refuse expired/canceled dispatch and clamp to the original deadline. Existing exec output keeps its separate per-frame deadline. Independent scoped review resolves I1 with no new Critical/Important/Minor finding; Root accepts the bounded Task4 component scope with every downstream obligation retained.

Actual host tests42/43 retain the five initial meaningful nonreading-TLS RED failures and preliminary GREEN. Final45 includes the real production five-second EOF/dispatch/refusal bound, accepted/terminal blocked writes, delayed/expired/canceled error handling and old execution-output compatibility: three top-level/six nested PASS. Controlled mutation46 restores the old generic error deadline reset and the real five-second test fails at its original-deadline+150ms join guard. Final47 runs both affected packages with race successfully;48vet and49build exit0 with empty stderr. Record44 is a retained incorrect test assumption about the clock's separate one-second inner context, not product defect evidence. Host transport-only opaque payloads are not valid drain receipts.

The original optional test filename collided with an inherited file in the uncommitted tree. Before commit the author restored all four inherited tests byte-identically (SHA256 `3724c1fd65357eaaebc1b1e101f216844b23197ddc59e519b9a5b829095b2c62`) and placed new tests in the verified-absent `supervisor_quiesce_transport_deadline_test.go`. Only two production files and that new test file are in the fix commit. No inherited coverage loss is committed.

Root executed only the changed protected selector against frozen `fc605d6`: `task-quiesce-local-3db028ac18b4`, one actual PID1 top-level PASS/zero FAIL/SKIP. All35 commands exited0 without timeout. Root `root-task4-fix1-native-audit.json` verifies122 actual repository inputs, three installed tools, source archive SHA256 `b9018e7393abde4f044a4d91944b26d0906f43bf0d314dbd487cae3b1dbb7959`, three daemon-custodied ELF payloads, unchanged resource/security contract and empty cleanup. Actual runtime UID/container ID is `2207746e7ca4ef4e58624f60ac4ac48b906856a7af371550108b392a0d8c5231`. It revalidates the original census/ECHILD and genuine zero-user terminal before real authenticated fallback/query. This is current-candidate Linux CGO0 component evidence; slow-reader sensitivity is separately the host TLS regression, and public combined-chain proof remains Task5.

Root also rechecked all244 compiled repository inputs for each of the earlier three metadata batches against final source: all remain identical, and none includes controlrunner. No metadata/predecessor fixture replay or relabeling of an old binary as a new candidate occurred. The complete original51 embedded author JSON records match their retained files, including failures; the original75,599-byte prefix remains unchanged. Source/tool current/Git hashes and final diff checks are retained in record51. All baseline warnings remain recorded; the changed protected run emitted no warning/error event. Original component, Linux race, supply-chain, future flush, capacity and production-migration limits remain.
