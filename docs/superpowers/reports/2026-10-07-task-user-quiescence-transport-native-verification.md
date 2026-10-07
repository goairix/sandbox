# Root native verification — finite authenticated transport and dual drain

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
| Client LeaseRevoke NotFound | 14 | Exact test/record/JSON-row mapping retained. Original author is attributing fixture cleanup and deliberate revocation repeats; logs do not include lease IDs, so individual lease attribution must be described as source/order inference. |
| Client KV/Txn Canceled | 1 | Actual `late-cancel` final-barrier fault, record24 JSON row28. Cancellation refusal asserts nil prerequisite and fired fault boundary. |
| Server failed apply / lease not found | 42 | Only this failed-apply error class observed; three replicas for each of14 missing-lease revokes. |
| Server mixed HTTP/gRPC port | 18 | Unchanged correctness fixture startup, two per member. No production TLS/RBAC acceptance. |
| Server data directory0755 | 9 | Unchanged fixture named-volume startup, one per member. Production data permissions remain deployment work. |
| Server unsigned simple token | 9 | Unchanged fixture startup, one per member. No production authentication acceptance. |

No server error/fatal/panic event was observed in these four batches. Warning attribution is separate from product review acceptance.

## Remaining scope

Original-range review found the inherited event writer can replace the fixed receive deadline with now+5seconds; the generic error writer can add another second. Passing component cases do not establish the five-second bound for a slow reader. This issue requires a targeted original-author fix and independent scoped review, plus actual changed-path verification.

Task5 combined public chain, safe final sync/remote settlement/End/exact runtime termination/release, production API/Pool/Docker/Upload wiring, partition scheduler, FUSE/collector, large-existing-sandbox capacity, deployment/restore and Redis removal remain mandatory. Neither token absence, this local prerequisite nor these separate component passes establish whole production migration or performance improvement.
