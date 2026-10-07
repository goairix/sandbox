# Task close delivery native verification

Scope: Task4 delivery/query on source candidate `767afb96e0a5ed9b1acc59c78655f694438793e9`, original Task4 BASE `e684e1510e9dc6d60a75a4aa7cb4be0f0606a0ed`. Root independently inspected complete raw outputs and audited source/compiler bytes, status, logs and cleanup. This records evidence before independent Task4 acceptance; it does not certify the overall migration.

## Accepted corrected fixture

Owned project `sandbox-etcd-state-test-75929-2872770697390147169` ran `go test -race -count=1 -timeout=90s ./internal/storage/state/etcd -run '^TestTaskCloseDelivery' -v`, GOMAXPROCS2. Result:4 top-level/6 subtests PASS,0 FAIL/SKIP,3.941s. Two host cases check query-only authority and foreign-origin refusal. Six native cases use six separate namespaces on actual three-member etcd and actual activated TLS/frame/protected journal close+sign: success, lost reply/history and loss before dial/during handshake/before application bytes/fixed deadline. Assertions reach the intended boundary and distinguish original-fence conflict from irreversible fixed-deadline expiry.

All42 raw commands exited0 without timeout. Test stderr is empty; Compose up/down progress appears on two other raw stderr streams and is expected command progress. Root independently matched all196 actually compiled repository sources and3 compiler files against frozen/current/candidate Git bytes. Source-freeze SHA256 `5f2e42f5c6955c2f988f065b263deea7ad7107068204080245cdd69af523134e`; raw023 SHA256 `841b5d8dd08818a10efa21c6f481037ce2848e544751b6ed88cb763cb878ca90`. Three owned cleanup inventories are empty; no OOM. All four final member statuses report etcd3.6.15/storage3.6.0; the three cluster members agree and the isolation member remains separate.

Logs are not pristine:5 client missing-Lease revoke warnings and15 replicated server apply warnings (`lease not found`) follow the explicit original-Lease revocations/cleanup in Native false/true and the three stale boundary cases. There are16 baseline startup warnings:8 shared HTTP/gRPC-port,4 data-directory0755,4 simple-token notices. No schema warning/error appears in this accepted batch. These counts retain the exact fixture scope; they do not excuse an unexplained production warning.

## Failed first fixture retained

Project `sandbox-etcd-state-test-73898-3932053661492006832`, candidate18871f3, compiled196 repository files+3 compilers without source drift. Its native command exited1:2 host cases PASS,2 native parents/6 subtests FAIL,0 SKIP. Activation construction rejected the inherited metadata-only BootID placeholder `boot` before any close delivery. Commit767afb9 changes only two Task4 test files: a UUID is placed in the original runtime certificate before binding/publication, and boundary assertions are strengthened. Frozen authority tuples and protocol validation were not weakened. This is a fixture failure, not preimplementation behavioral RED.

All other raw commands completed0; final statuses, noOOM and empty owned cleanup inventories were independently audited. Full failed stdout/stderr and audit remain. Raw023 SHA256 `08e7abc3cd38f8f3b33fc9304d5f911d88f12670683cb76aabe7623f4457b790`; source freeze `16e632abebd4a2e54f36ddfba65155826b256b98e7932bdd8ddafd87f7293f0e`.

The failed batch also logged16 baseline startup warnings,2 missing-term schema notices and1 initial storage-version-detection error before readiness/test. Final member status was healthy storage3.6.0. The timing supports a bootstrap interpretation but does not establish root cause, upgrade behavior or restore safety. The accepted rerun does not erase this anomaly.

## Evidence boundaries

Full manifests, raw streams and independent audits are retained under `.superpowers/sdd/2026-10-07-task-data-gate-closure/root-fixture-evidence/<project>/`; accepted audit is `root-independent-audit.json`, failed audit `root-independent-failure-audit.json`. The client here is Darwin race-instrumented Go; server is pinned Linux etcd. Actual protected Linux PID1 Supervisor and journal tests are separately recorded in the target verification report and are CGO0 non-race. Together these are compositional evidence; no same-fixture backend-to-protected-PID1 end-to-end claim is made.

Independent original-range Task4 review and scoped I1 fix review are complete; individual findings are retained in the delivery review report. Unfiltered whole-unit review remains required. Same-fixture production integration, CloseAll/dual drain/final sync+flush/remote settlement or fencing/exact-runtime termination/safe conditional release, production wiring, Redis removal and large-existing-N capacity/long-run gates remain downstream requirements. This gate proves neither physical End nor performance improvement.
