# Root native verification — quiescence metadata and operation pages

Frozen source candidate `32f8c9ea51255b094dd72891fcd604c92b0222a5`, original Task3 BASE `4fe9c60128a7e4c3b7aae8c1cef2f480d19fc4ef`. This records actual component tests; Task3 remains unaccepted pending independent review fixes for immutable attempt persistence and claim-aware issuer registration. It establishes neither protected PID1 composition nor production migration/capacity.

## Actual batches

Eight disjoint selectors from `task-3-evidence/native-selectors.json` ran once each in a fresh unique three-member cluster, with48 per-case native fixtures. Exact selectors and all PASS names remain in `root-metadata-native-audit.json` and raw test2json. Go1.25.6 Darwin arm64 CGO1 race binary ran against actual Linux arm64 etcd3.6.15, pinned image digest `sha256:5ed4e32061aa061f970d6500629213e8579ef17571dcf9789074966acb9afad1`. Three service IDs, cluster/member checks, healthy inspect and exact three-container inventory were retained. No foreign service or pull was used.

| Batch | Coverage | Top/sub PASS | Project | Raw stdout SHA256 |
| --- | --- | --- | --- | --- |
| A | success; 65-charge and byte preflight | 1/4 | `sandbox-etcd-state-test-12631-1791390187108509000` | `05d76dae6f33cdc56d27f3a24b2190bb46f7b796dd1ba41bc02c2057276aa9b9` |
| B | lost registration/Grant/begin/commit/resolve/cleanup replies | 1/8 | `sandbox-etcd-state-test-13578-1791390259024953000` | `a12c2609a99e63a09932ffbb9485787d284097ad9cca3227660174560976bcb4` |
| C | postcommit cancellation/deadline/fence and signer boundaries | 1/12 | `sandbox-etcd-state-test-14000-1791390279702562000` | `a1d0d5d997804b7184bc17884029a9280890697555d13d66a9129a2209e183f1` |
| D | cold/history/no-adoption and receipt corruption | 1/9 | `sandbox-etcd-state-test-14365-1791390300056426000` | `191d6949c3f028d78ed4b1e207d6388d6d0892c5c7f3f313cb0ea385f7e87964` |
| E | original receipt deletion/rewrite/recreation/lease/restore fences | 1/6 | `sandbox-etcd-state-test-14495-1791390316698301000` | `3cf2e39bb3a81dc908cf6838ebae2eb6a0398e9f55d687e150610d36128e6503` |
| F | delayed exact packet after revoke/replacement/natural expiry | 1/3 | `sandbox-etcd-state-test-15304-1791390334339333000` | `d39c8633f26458c416e059cc0763566f1d57d23f70797e80b8781768d5b319b5` |
| G | 20 operations, pagination/disappearance and final fresh empty Txn | 1/1 | `sandbox-etcd-state-test-15817-1791390353458217000` | `b7e7d2eaadd9b9e864e5f679bc144366996b6eea55b42edd6eafb0b962e579fd` |
| H | range/cluster/ordering/identity/claim/fence response rejection | 1/11 | `sandbox-etcd-state-test-16255-1791390368848653000` | `056ab180af16f7d75ef139a698068b193676e1fa72827c67af2ad4d146c000a3` |

Observed total8 batch top-level PASS/54 subtest PASS, zero FAIL/SKIP. These are fresh Root native results; author's earlier host48 native skips remain skips and are not reclassified. Eight raw test commands exited0 with empty process stderr. Original test stdout includes81 client warnings, retained and classified below.

## Custody, quota and cleanup

Each batch retained the complete committed Git archive,226 actual repository GoFiles/CgoFiles/SFiles inputs, go.mod/go.sum, four existing Go tool hashes, exact compiled race binary, all31 raw command outputs and SHA256, actual three member pre/post inspections, resolved Compose and source/compiler postchecks. Root independently checked all recorded bytes/hashes/exits, exact declared PASS counts, absence of FAIL/SKIP, exact members and empty before/after container/volume/network inventories. Source/candidate/current compiled inputs matched in these runs. The eight binaries have distinct absolute build paths and retained distinct hashes; they are separately attributed, not claimed byte-identical.

All batches used unchanged accepted repository Compose server settings: inspected Memory0, NanoCpus0, PidsLimit unset. This correctness fixture does not establish production quotas or fleet performance. Root's test runner timeout was180seconds/outer210seconds; the author requested90seconds, but the actual configured limits are reported here without claiming90. Actual test invocations lasted approximately3–8.4seconds. Host race execution is separate from protected Linux CGO0 PID1 component execution. Generated wrappers, stdlib/module cache and native C toolchain are not separately supply-chain certified.

The initial metadata preparation `sandbox-etcd-state-test-11663-1791390037799888000` failed before build/cluster start while the default Go attempted toolchain download and network verification timed out. Its complete failed output and empty cleanup remain retained. Root switched all Go invocations to the already-installed absolute Go1.25.6 with GOTOOLCHAIN=local, GOENV=off/GOWORK=off/GOFLAGS empty; no installation or quota increase. Fixture exact full-ID comparisons and subsequent narrow amendments were independently approved. Accepted predecessor suites were not replayed.

## Warning dispositions

All events remain unmodified in `root-metadata-warning-inventory.json` and `root-metadata-server-warning-inventory.json`; no logger suppression or pristine-output claim. Root consumed the complete author warning-origin report and checked the original claimant release, delayed packet and pagination lifecycle source seams.

| Observed class | Count | Disposition |
| --- | ---: | --- |
| Client LeaseRevoke NotFound |80|48 original CloseData-claim cleanup repeats +1 Stage cleanup lost-reply retry +4 deliberate current-claim loss/release +2 delayed revoke/replacement cleanup +2 expiry cleanup +23 additional pagination lifecycle revokes. Original cleanup treats missing lease idempotently. Per-event project/test/record24/line mapping retained; logs lack LeaseID, so exact lease attribution is source/order inference. Budget bytes' event is parent fixture cleanup, not a pre-Grant Revoke. |
| Client KV/Txn DeadlineExceeded |1|Delayed-claim expiry case's captured original bounded RPC expired while only original Stage guard was kept alive. Subsequent unchanged comparison/write packet replay under fresh bounded IO was rejected by actual native CAS; timeout alone was not used as rejection proof. |
| Server failed to apply request / lease not found |240|80 lease-revoke requests reported on all three replicated members. This is the actual missing-lease class; no other failed-apply error class observed. |
| Server HTTP+gRPC single port warning |48|Unchanged test Compose startup configuration, two per member. Production TLS/auth deployment remains a separate mandatory task. |
| Server /etcd-data permission0755 warning |24|Unchanged fixture named-volume directory, one per member. These temporary clusters do not certify production data-volume permissions; deployment must enforce0700. |
| Server simple token unsigned warning |24|Unchanged fixture startup auth configuration, one per member. This test configuration is not production mTLS/RBAC acceptance. |

The warning census contains no server error/fatal/panic event. No unexpected client method/error class was observed. Warning explanations do not dismiss the independent source review findings or certify unrelated cleanup paths.

## Remaining requirements

Independent Task3 review found two Important issues despite passing these component cases: a new claimant can replace an aborted/unresolved attempt before committed history exists, and writing issuer-registration composition lacks the required full original-claim fence at each RPC. Both are fix-round1 work and require targeted regression/native verification plus scoped independent review. This report is baseline evidence, not acceptance of that source.

Task4 authenticated receipt/query composition and fresh dual-drain prerequisite, Task5 same-chain three-member metadata→protected production PID1→dual drain, production scheduler/API/Pool/Docker/Upload integration, FUSE/remote settlement, deployment/restore/Redis removal and large-existing-sandbox measurements remain required. Page progress or token absence alone grants no successful business End, remote settlement or safe owner release.
