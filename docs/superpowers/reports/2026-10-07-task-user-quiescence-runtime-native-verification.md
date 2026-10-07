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

## Fix round 1 — new actual interleaving gates

After the human requested resumption, Root ran six genuinely new selectors from immutable candidate `3dd3f6f848e7ad884334de55b81637301320440d`. Runtime changes are `4d42440` and `6061026`; the original six baseline gates above were not replayed. This appendix records amended-source component evidence; independent scoped review and Task2 acceptance remain pending.

| Selector | Actual outcome | Go top/sub PASS | Project |
| --- | --- | --- | --- |
| `^TestTaskQuiesceNativeCommittedStart$` | PASS / exit0 | 1/0 | `task-quiesce-local-ad097c4d063a` |
| `^TestTaskQuiesceNativeEarlierIsolation$` | UNKNOWN_ISOLATED / exit70 | 0/0 | `task-quiesce-local-7012a505d759` |
| `^TestTaskQuiesceNativePendingDeadline$` | UNKNOWN_ISOLATED / exit70 | 0/0 | `task-quiesce-local-0d79007fded4` |
| `^TestTaskQuiesceNativeSigningDeadline$` | UNKNOWN_ISOLATED / exit70 | 0/0 | `task-quiesce-local-49f750224ba5` |
| `^TestTaskQuiesceNativeFailedTerminalQuery$` | UNKNOWN_ISOLATED / exit70 | 0/0 | `task-quiesce-local-284f0b4bb580` |
| `^TestTaskQuiesceNativeRepeatedClose$` | UNKNOWN_ISOLATED / exit70 | 0/0 | `task-quiesce-local-77854a3ccf02` |

CommittedStart released the same original committed Start path, joined the sole Wait/result/watchdog/owner and original timer, and obtained actual PID1 namespace census/non-reaping ECHILD revalidation: registered1/local1, no never-spawned member. Repeated successful Close retained the original timer.

The other five cases produced actual attach and Docker exit70, non-OOM/nonrunning, exact armed/refusal markers and PID1 isolation diagnostics. They have no Go PASS, FAIL or SKIP and prove UNKNOWN isolation only. EarlierIsolation began the actual cooperative isolation owner before advancing its same timer; pending/signing paused configured AuthorityClock under the actual journal mutex. Failed-terminal obtained a readable terminal and fresh empty census but the failed original coordinator still returned error/nil wire. Failed-terminal and repeated-Close consumed private failureOnce to inspect the failure window before advancing the existing production timer; these do not prove unmodified immediate cooperative isolation. Clock contention is not a blocked kernel fsync, and committed Start gating is context preflight before the OS syscall.

Root audited every retained raw command exit/timeout and stdout/stderr/stdin hash, actual compiled repository input against archive/current source, module/archive/tool/three ELF hashes, root payload custody and target final state. Each fixture kept the original 64MiB/.5CPU/128PID/netnone/NNP/readonly/default security contract and four bootstrap capabilities. All six labeled container/volume/network inventories were empty before and after cleanup; result publication followed cleanup. Positive stderr is empty; each negative stderr contains the expected sandbox-isolation diagnostic and is retained in full.

Evidence: `root-fix1-native-runs.json`, `root-fix1-pid1-native-audit.json` and the six named `root-fixture-evidence/<project>/` directories in this plan OWN. Host race remains separate from Linux CGO0 execution. The binding original31s timer identity is asserted before controlled earlier expiry; the new gates do not independently wait31s. Original real32s successful-history evidence remains the distinct baseline result. Task4/Task5, production lifecycle, Redis removal and capacity obligations remain unchanged.
