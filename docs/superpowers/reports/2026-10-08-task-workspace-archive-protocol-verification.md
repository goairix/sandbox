# Task workspace archive protocol verification

Scoped protocol/frame unit accepted on `codex/etcd-state-management`. Source commit `34568d1e0cdd95f86d86085f42167e7e93543fa8`, parent `b50c9748993ba269a03b3145fd446f9f44a0ecf9`: exactly8 new paths,1340 insertions, no existing source/Session/router/Supervisor/dependency changes. Sole independent review: spec Approved, quality Approved, C0/I0/M0. This is not a production-stream or whole-migration gate.

Delivered fixed `task_workspace_archive` attribution, exact task/quiescence/claim/runtime linkage and revision order, separate <=30s start window and fixed <=1h session deadline bounded by certificates, distinct ticket/complete signature domains, strict recursive JSON, copied evidence and bounded signed summary. Standalone canonical request codec requires actual request EOF; independent archive response frames enforce kind/size and IO validity only. No actual archive sender or producer was added.

## Executed evidence

Current evidence directory `.superpowers/sdd/2026-10-08-task-workspace-archive-protocol/` is closed after this gate; do not redispatch or replay its accepted task. Retained `author-report.md`, `manifest.json`, `task-1-review-report.md`,7 command metadata records and complete streams. Root and reviewer independently matched all24 artifact hashes,8 frozen source hashes and final race/vet source maps; reviewer also matched committed blobs and exact path list. Selected Go/platform environment only, no whole environment capture.

| Evidence | Actual result |
| --- | --- |
| `red` | Compiled reject stub failed the independently stdlib-signed positive ticket assertion with `archive ticket not implemented`, exit1, empty stderr. Three embedded exact snapshots/hash pairs retained. Stdout630bytes SHA256 `498f435b761002fc8018efdc4910917df2934d92d2f004d3675433b9765a458d`. No compiler/missing-symbol failure counted as RED. |
| Focused runs | Positive GREEN retained; one later test converter incorrectly forced Version1 rather than injecting the requested invalid version. Actual failed run retained; corrected converter and focused GREEN retained. No final failure was concealed. |
| `final-race` | Pinned Go1.25.6 `test -race -count=1 -v` for controlprotocol/controltransport once after freeze; exit0,103 top-level /3245 inclusive PASS,12 new top-level archive PASS,0FAIL0SKIP. Protocol12.654s, transport1.452s; empty stderr/no race or warning diagnostics. Stdout595177bytes SHA256 `f19aecd67a0b4754ec4b88ee7d1a1b843cd1a4f00a9a5b76190f15a349e50aec`. Actual darwin/arm64CGO1, no native-platform claim. |
| `final-vet` | Same pinned Go vets both affected packages, exit0, empty stdout/stderr; all8 source hashes match frozen race and commit. |
| `source-commit` | OWN fcntl lock, expected branch/base, initially empty index,8 exact paths/hashes, gofmt stdout equality, cached whitespace/path checks, committed blob equality and finally empty index. Formatting edits used apply_patch only. |

Author report SHA256 `6389e4d171426b0c7ab8ab1c17379c45e3785f63134dccf750c83a570432a0e2`; manifest SHA256 `cb35c8d30d546afe635bafed588205e8fc7ac0a64f5c8ddd454fcbe3463c5b62`; sole review report SHA256 `5d57c7c1406f739e31ac2b33b6700c7c1089d95e992871e8464718863088b3d6`. Root/reviewer reran no tests or native fixtures. Unrelated Sentinel path remained untouched and unstaged.

## Root rulings on retained boundaries

| Item | Accepted boundary / required continuation |
| --- | --- |
| CV1 immutable KV birth/live claim | Accept pure lineage only. Backend must fence actual original reservation birth, control/owner/claim/restore epoch; decoded context grants nothing. |
| CV2 physical source/original producer | Accept typed signatures only. Target needs durable original admission and Supervisor ownership before bytes; successful source return must precede completion signing. |
| CV3 historical receipt | Accept expired-session history against fresh exact runtime certificate. It cannot extend or recreate a sending session. |
| CV4 actual received summary | Accept signed attribution only. Receiver must compare actual TAR byte count/digest/entry count; no remote write or release on a summary alone. |
| CV5 complete stream | Accept one-frame bounds only. Later receiver must enforce data-before-single-completion, cumulative bounds and actual response EOF. |
| CV6 TLS/deadlines/routing | Accept canonical request EOF only. Actual authenticated routing, five-second start request and separate bounded monotonic stream lifetime still need wiring. |
| CV7 birth user/permanent gates/joins | Prior physical user access is accepted separately, but this unit establishes no producer binding. Use verified immutable birth UID/GID, permanent USERS/DATA closure and original joins. |
| CV8 current send fences/provider settlement | Remain required. Every actual authorized request needs current original claim/reservation checks; provider retry/HTTP attempt settlement remains separate. |
| CV9 termination/release | Remain required. Neither signatures nor frame EOF prove exact termination or safe owner release. |
| CV10 sync mode/plan | Fixed purpose is accepted; future trusted backend must establish reserved `sync` mode and exact plan digest before signing/admission. Hash alone cannot select mode. |
| CV11 platform | Accept host Go1.25.6darwin/arm64 race/vet only. No Linux/native stream integration or platform matrix evidence added. |
| CV12 capacity/production | No throughput/large-existing-sandbox capacity, production/deployment or Redis removal acceptance. Package test counts are correctness evidence only. |

Next: target durable archive admission/completion and cold refusal, private Supervisor producer with verified user/source, authenticated actual stream and receiver verification; then remote settlement, exact termination and safe release. Production wiring, partition scheduling, FUSE/collector, capacity and deploy/recovery/Redis removal remain open. No external blocker; the remaining work is implementation/integration.
