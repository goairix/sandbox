# Root native verification — runtime user quiescence

Candidate `32f8c9ea51255b094dd72891fcd604c92b0222a5`, original Task2 BASE `4fe9c60128a7e4c3b7aae8c1cef2f480d19fc4ef`. This records component execution, not Task2 acceptance, combined dual-drain acceptance or production migration. Independent review is still pending; a provisionally identified standalone stop deadline failure path must be resolved before acceptance.

## Actual execution

All six selectors ran separately as Linux arm64 PID1 with unchanged 64MiB memory, .5 CPU, 128 PIDs, readonly root and payload, network none, NNP/default seccomp/LSM, and the original four bootstrap capabilities. Actual target security, image digest, runtime ID/UID, tmpfs and contract digest are retained in inspect-derived attestations. Bootstrap evidence reports PID1, subreaper, NNP, zero ambient/bounding capabilities and effective/permitted/inheritable224. This is the supported cgroup-v2 fixture, not all kernel configurations.

| Selector | Outcome | Go top/sub PASS | Project | Raw stdout SHA256 |
| --- | --- | --- | --- | --- |
| `^TestTaskQuiesceTerminalNative$` | PASS / exit0 | 1/7 | `task-quiesce-local-180f8f271030` | `22a6ebfa049dfd0e771cf078b560978f7b0dbbe4c2d6eabe3be037d4a480ae16` |
| `^TestTaskQuiesceNativeDoubleFork$` | PASS / exit0 | 1/0 | `task-quiesce-local-8ea39868560c` | `244157de57941e4c20d3a2c280557d9341771deb74736cd32373447f1bb0e996` |
| `^TestPID1QuiescenceNativeSeal$` | PASS / exit0 | 1/10 | `task-quiesce-local-9be09879d413` | `b4fe10fee1d9535d4c7e1c11bb49ffac8f816aceaffd9c769ed8da1a784771d3` |
| `^TestTaskQuiesceNativeLocal$` | PASS / exit0 | 1/0 | `task-quiesce-local-9d93b94431fd` | `212380e1b2a82953d00eee4c1c97b49e8e719325dcdd75b8524f7514049988af` |
| `^TestTaskQuiesceNativeMonitorLoss$` | UNKNOWN_ISOLATED / exit70 | 0/0 | `task-quiesce-local-bdeef49c233b` | `377e848114235973e25be252aec3781cc759474cc01d63f4055eacc5e3ad8fc4` |
| `^TestTaskQuiesceNativeUnknownChild$` | UNKNOWN_ISOLATED / exit70 | 0/0 | `task-quiesce-local-f65cc8f6143b` | `bb005bb5c06dbdea4a949570a4e710dbb98640815ecb68bf05d183d4b3cfab16` |

Four positive selectors:4 top-level/17 subtest PASS, zero FAIL/SKIP. Two deliberate faults: original unknown child and killed original monitor returned actual process exit70, non-OOM, fixed pre-loss marker and isolation diagnostics, with no Go PASS or terminal-quiescence claim. Unknown-child diagnostic reports `user_quiescence_unknown`; monitor-loss reports `monitor_lost`, unknown execution and no terminal receipt. Full stderr retained125/280bytes respectively; positive test stderr empty.

Local live hold process was captured in the original frozen set and reached local_terminal; original coordinator and deadline owner joined. Fresh genuine successful history query ran after real32.023seconds, retaining the original timer. Double-fork execution produced DOUBLE_READY and registered1/local1 terminal evidence in this run; marker alone does not prove continued liveness across the barrier. Terminal producer exercised seven cases: success plus six file/clock failures, including readable durable terminal that remained unsignable after poison. Seal gate exercised original actual empty census/ECHILD, copied observation refusal, strict parsing and nine private saved-seal perturbations against unchanged kernel; no namespace/mount/cgroup replacement was performed.

## Custody and independent audit

Root used immutable complete Git archives, Go1.25.6 absolute existing toolchain with GOTOOLCHAIN=local/GOENV=off/GOWORK=off/GOFLAGS empty. Actual GoFiles/CgoFiles/SFiles repository inputs, module files, three Go tool binaries, all three Linux ELF hashes and complete command stdout/stderr/stdin hashes were audited against retained files. Daemon-copied payload tar independently confirms root0:0, executable0755 and attestation0600 with exact bytes before start. Linux CGO0 execution has no race detector; author Darwin race results remain separate. Generated wrappers, stdlib/module cache and native C supply chain are not separately certified.

Each accepted fixture retained all raw commands, actual pre/final target inspect, daemon tar, source archive, security contract and result only after successful cleanup. All six before/after labeled container/volume/network inventories are empty; cleanup recovered and inspected owned project IDs. Daemon-successful/client-unknown create fault was statically reviewed but not physically injected.

Four rejected preparations remain as honest pre-test evidence: initial default-Go toolchain download timeout/network failure, canonical CAP_ name mismatch, and Docker Tmpfs-versus-Mounts representation mismatch. No local test began in those preparations. Corrections changed fixture assertions/toolchain only, never quotas, target privileges or product source; all rejected inventories were cleaned. Metadata's separate initial toolchain failure is documented in its report.

Evidence lives under `.superpowers/sdd/2026-10-07-task-user-quiescence/`: `root-pid1-native-audit.json`, `root-frozen-source-audit.json`, fixture utility reviews/fix confirmation and each named `root-fixture-evidence/<project>/` directory. No accepted predecessor gate was replayed.

## Remaining requirements

Task2 independent spec/quality review and deadline finding closure are required. Deterministic Start-in-flight models do not establish a physically blocked OS Start; saved-seal perturbations do not establish actual namespace replacement; uninterruptible syscall/scheduler limitations remain. Task4 authenticated delivery, Task5 same-chain three-member metadata→protected production PID1→dual drain, final FUSE/remote settlement, production wiring/Redis removal and large-existing-sandbox capacity remain mandatory. Separate component results grant none of those outcomes.
