# Launcher kernel prerequisite complete verification record

完整worker、独立task/scoped/final审查与Root实际证明保留在此，不能以摘要替代。最终完整审查与15项逐项裁定/代价以及64+28边界见[final review](2026-10-07-launcher-kernel-boundary-final-review.md)。整个原始测试证据、完整Docker命令/actual exits/created及exited JSON/原始logs/清理记录、两个Bash故障场景及其harness见[evidence archive](2026-10-07-launcher-kernel-boundary-evidence.tar.gz)。压缩包内容包含逐文件SHA256清单，未包含编译binary；binary与完整Go源码SHA均保留。此处的旧scratch路径只是历史同名定位。

实际最终Go源码118dbfc/LinuxCGO0arm64binary eb671ddf8434bea8750654ccb3d4f868254a01aedd90edb05947a2e45cbe705e；两个真正canonical脚本73b及a833分别运行并留证。最新2378脚本6b7c58b只修复异常等待，Bash故障模拟RED→GREEN、原九实例precheck条件复核及两次独立review通过，不声称最新脚本重新native执行。33fresh nativecontainers/33volumes总66个individualrm均actualexit0，所有记录的owninventory为空；mock故障资源另计。Hostrepo30cachedpassing/8notest，vet/buildactual0空诊断；hostrace与真实CGO0Linux不是Linuxrace。没有新nativeetcd/fleet/原镜像开销/整体migration claim。


## Task1 complete implementation report

# Task 1 report — DONE (implementation; independent review owned by controller)

Scope: new internal/runtime/launcher only. No production entrypoint, Manager, image, config, etcd journal, authority, lease, background worker, raw user start, ready or terminal changes. Task 2 remains responsible for user exec isolation and canonical script.

## Slice 1, model contracts and unsupported platform

Commit `69ab0a1 feat(launcher): define exact kernel role contracts and unsupported boundary`.

Implemented exact public KernelSnapshot, private role and boundary, sentinel errors, copy Snapshot, pure role validation, unsupported-platform behavior. Fixtures independently use literal initial/final PID1 and monitor masks. Tests mutate all four positions of UID/GID; all 64 bits of each of five sets; NNP, securebits, dumpable/subreaper, PID, cap_last bounds, observed thread count bounds and unknown roles; nil/zero handle and copy semantics.

TDD RED: `go test ./internal/runtime/launcher -run '^TestKernel(Models|Handles|Unsupported)' -count=1 -v` compiled and executed with validator returning nil. It failed behavior assertions including non-root UID/GID, all mask changes, final subreaper, and other unsafe model mutations. Full output is `task-1-model-red.log`; no compiler/helper errors. The command wrapper tailed the log and returned 0, but the Go test itself printed FAIL (failure exit was not separately captured by that first wrapper). This is model RED only, not Linux proof.

GREEN: same selector count=1 without -v → `ok github.com/goairix/sandbox/internal/runtime/launcher 0.494s` (task-1-model-green.log). `go test -race ./internal/runtime/launcher` → ok 1.554s; `go vet ./internal/runtime/launcher`, gofmt, staged diff check → exit 0. Self-reviewed staged four-file slice before commit. Existing unrelated Sentinel plan edit preserved.

## Linux implementation and fixture interface (first freeze)

New Linux files contain paged task observation (128 directory entries, at most 4096 observed tasks, 64KiB per status), strict required proc field parsing, real cap_last_cap, process prctl properties, whole-observation retries up to 3 on vanished threads, serialized one-shot initialization and copied final diagnostics. No full thread map in production. All setters use Go AllThreadsSyscall/6, capset pointers are live across calls and KeepAlive after completion. Nil boundary on errors; no restoration path. ValidateCurrent is read-only and rechecks actual process identity and all observed thread role state. Thread count may change.

Controller clarification: securebits is per-thread and absent in proc status. Task 1 uses the narrowly permitted `AllThreadsSyscall6(PR_GET_SECUREBITS)` scalar GET expecting uniform zero. This is an explicit exception to the original setters-only wording. Uniform nonzero values reject; divergent results cause Go runtime fatal termination (not recoverable error, no boundary). No pointer output or single-thread fallback. Dumpable/subreaper are process properties and use actual ordinary prctl. CGO/unsupported AllThreadsSyscall returns ErrUnsupported preserving errno.

First frozen binary: `.superpowers/sdd/2026-10-07-launcher-kernel-boundary/task-1-launcher-linux-arm64.test`, compiled `CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go test -c ./internal/runtime/launcher -o <path>`, SHA256 `50f98c8911084de60cb7a1fd7aa80eda9d5e336905b4797740d71b635e805ffc`. Compile and cross-Linux package vet exit 0. Controller inspected pinned image as Linux arm64 before compilation. This is compilation only; no native result claimed yet.

Actual private test interface:

- TestMain reads `SANDBOX_LAUNCHER_TEST_MODE`. Empty runs standard package tests, and on Linux native test skips with explicit controller fixture requirement. `positive` runs full m.Run; requires true PID1. Launch with `-test.v -test.count=1 -test.timeout=45s`, no selector, `GOMAXPROCS=2`, exact four caps and NNP.
- `monitor` executes nativeMonitor, actual initial inherited P/E/I=e0 B/A=0, PrepareMonitor, copy snapshot, ValidateCurrent, repeat/wrong-role rejection, final marker, exit 0; errors to stderr and exit 1.
- Positive native test starts the same binary using private `runNativeChild(mode string) ([]byte,error)`. No argv, env exactly MODE plus GOMAXPROCS=2, cwd `/`, stdio via CombinedOutput, no ExtraFiles, 5s context and 1s WaitDelay. Modes monitor and reject-nonpid1 run actual exec from bootstrapped PID1. This is a test interface, no production start API.
- Fresh controller-owned negative PID1 modes: reject-nnp (omit NNP), reject-missing-cap (only e0 capabilities), reject-extra-cap (1e1, adds CHOWN), reject-role (normal four caps/NNP, calls PrepareMonitor in PID1), reject-thread (normal fixture; one pinned real thread drops effective KILL). They observe actual thread states before/after and require nil boundary, UnsafeKernel, and no change to surviving original threads. Expected exit 0 and rejection marker.
- reject-securebits sets securebits=1 on one pinned actual OS thread then calls BootstrapPID1. Expected runtime unequal-syscall fatal and nonzero exit, no boundary marker. Controller captures actual outcome.
- `holdNativeThreads(n, change)` is a test-only helper: locked thread workers report ready and block on release, stop closes once and bounds join to 5s. Threads retire on goroutine return without unlock. Callers defer stop before checking readiness errors. No persistent production workers.

Self-review identified a remaining native setter-failure coverage case; after first freeze run, add a real seccomp TSYNC denial of capset and require nil boundary/EPERM with actual before/after state. No fake syscall or proc getter will be used. Actual user UID/GID=1000, supplementary groups, hostile attempts, fd/secret/proc isolation remain Task 2, not proven by this slice.

Full repository host check during first native freeze: `go test ./...` exit 0. Log `task-1-repo-test.log`: all packages pass (some cached), new launcher package `ok 0.801s`; controltarget 19.024s, etcd state 1.235s. These are Darwin arm64 tests, not native Linux evidence; nonverbose output does not enumerate existing suite skips.

Controller requested both real first-setter capset EPERM and partial-sequence cap-bounding-drop EPERM after successful inheritable setup. These will be a test-only seccomp TSYNC filter, after first native batch; expected partial failure leaves I=e0 and no rollback, no boundary. Source is frozen until controller completes the first batch.

Host repository `go vet ./...` and `go build ./...` completed exit 0; owned `task-1-repo-vet.log` and `task-1-repo-build.log` are empty. No errors were hidden. Native execution remains controller-owned and pending at this report point.

## First actual Linux native result and kernel slice commit

Controller-owned first batch finished on binary SHA256 `50f98c...05ffc`, exact frozen production sources. Kernel slice committed immediately after actual native success and self-review: `524ae7d feat(launcher): verify and reduce real Linux process privileges on all threads`.

Read stored proof: `task-1-root-native-corrected-evaluation.json` and exact positive/thread/securebits logs. Positive full package: 4 top-level tests, 1368 PASS including subtests, 0 FAIL, 0 SKIP, attach exit 0, actual Docker state exited/Running=false/OOMKilled=false/ExitCode=0, wait result 0 and wait-command exit 0. Actual initial PID1: PID=1, UIDs/GIDs=[0,0,0,0], P/E/B=0x1e0, I/A=0, CapLast=40, NNP=true, dumpable=1, subreaper=false, securebits=0, observed threads=7. Final PID1: P/E/I=0xe0, B/A=0, dumpable=0, subreaper=true, securebits=0, threads=7. New threads also validated after initialization.

Actual child exec monitor PID=17: initial P/E/I=0xe0, A/B=0, four UID/GID positions zero, NNP=true, dumpable=1, subreaper=false, securebits=0, threads=5; final I=0 while P/E=e0, B/A=0, dumpable=0/subreaper=true. Actual child monitor and non-PID1 helper exited successfully via bounded CombinedOutput. Repeated role initialization is rejected.

Fresh reject-nnp, reject-missing-cap, reject-extra-cap, reject-role, reject-thread all attached/exited/waited with 0. Each reports actual rejection and unchanged surviving-thread credentials/caps/NNP. In reject-thread, actual TID8 had E=0x1c0 with P/B=0x1e0; the role validator rejected this exact thread before mutation. Native observation log structs carry zero defaults for fields not present in proc status (CapLast, Dumpable, Subreaper, Securebits and normalized Threads); those placeholders are not measurements of those properties. Full validated initial/final role snapshots carry actual property observations.

Divergent securebits fixture: one actual pinned thread set securebits=1 with real syscall returning nil; all-thread GET produced r1=1 versus expected r1=0. Actual Go runtime fatal `AllThreadsSyscall6 results differ between threads; runtime corrupted`; attach/process/wait result 2, wait-command exit 0. No boundary/success marker. This is the declared fail-closed availability cost, not a normal error return and not a failing positive test.

Controller's first orchestration process exited 1 because its expected normal-negative marker was mistakenly `kernel boundary verified: rejection`. Actual helper emitted the documented mode-specific marker and succeeded. Root retained original JSON/log evidence unchanged and wrote the corrected evaluation; all 7 cases have terminal_ok/behavior_ok/cleanup_ok=true and corrected evaluation exit 0. This was a controller oracle error, not model RED, native behavior RED, compile failure or product failure.

Actual first fixture measurements: VmRSS 9532 KiB, Go allocated 1633224 bytes, observed FD count 7 (including the observation's open FD). These are this test fixture only, not original image/runtime overhead. Every exact first-batch fixture removed by controller; separate container/volume/network inventories returned exit 0 with empty remaining lists. No worker Docker lifecycle calls.

## Second freeze, real syscall failure tests

Only native test source changed after kernel commit. TestMain adds `reject-setter` and `reject-partial`; run each as actual fresh PID1 with the same four caps plus NNP, MODE plus GOMAXPROCS=2, no argv. `denyNativeSetter` installs actual seccomp TSYNC across all threads. reject-setter refuses SYS_CAPSET with real EPERM; reject-partial refuses PR_CAPBSET_DROP after real all-thread I=e0 setup. Both require nil boundary and preserved errors.Is(ErrKernelUnavailable) and errors.Is(EPERM). Partial failure additionally checks every after-observation P/E/B=1e0, I=e0, A=0, with no rollback or spawned user code. This fixture BPF is for the tested native little-endian arm64/amd64 syscall ABI, not a production seccomp policy.

Corrected existing reject helper cleanup to propagate bounded thread-join failure rather than silently discard it; controller requested to repeat reject-thread in addition to the two new cases. No production source changed; all five production hashes match first freeze.

Final second-freeze binary: `task-1-launcher-failures-linux-arm64.test`, SHA256 `d8bdd197a61be88c80fce47dbdfa9d97b5f03c1dd97278a33f08cca390c9e1f9`; native-test source SHA256 `6d4d1579ba813b53479ebf224491fc96142f82ad1882919f4c13f5d31d69fb24`. Intermediate compilation before adjusting marker wording produced binary `42d52f9e...3cb5aa`, not submitted for actual execution; no compile errors. CGO0 arm64 build, cross-Linux package vet, host package race and diff-check passed. Second native execution pending here.

## Second actual native result, final commit and handoff

Read `task-1-root-native-failures.json` (then compactly extracted every case's terminal/inventory fields after the initial full JSON output exceeded display limits) and exact `task-1-root-native-failures-reject-setter.log` / `task-1-root-native-failures-reject-partial.log`. Controller coordinator exit 0; each of reject-setter, reject-partial and rerun reject-thread has attach exit 0, actual Status=exited/Running=false/OOMKilled=false/ExitCode=0, wait-command exit 0 and actual wait result 0. All accepted=true. Each fixture's separate container/volume/network inventory is exit 0 and empty; actual owned removes succeeded. No leftover resource claim relies on best-effort rm.

reject-setter initial actual PID=1, root UID/GID arrays, P/E/B=480 (0x1e0), I/A=0, NNP=true, dumpable=1, subreaper=false, securebits=0, CapLast=40, threads=5. TSYNC installed, actual `all-thread capset: operation not permitted`, nil boundary and wrapped EPERM assertion succeeded; surviving thread credentials/NNP/masks unchanged. reject-partial actual `all-thread prctl 24: operation not permitted`; five observed after-threads each P/E/B=480, I=224 (0xe0), A=0, root UID/GID arrays and NNP=true. Partial state is preserved; no rollback and no boundary. Rerun thread discrepancy rejection plus propagated bounded cleanup passed.

Final fresh host package `go test -race -count=1 ./internal/runtime/launcher` passed 2.332s. Host package vet, cross-Linux arm64 package vet and diff-check exit 0. Full host repository test/vet/build recorded above; production sources unchanged since those checks. No native positive failure, native behavior RED, helper compile error, or host unsupported test skip occurred. Model behavior RED and initial Root oracle correction are separately preserved above. Native tests are actual acceptance evidence; their initial compilation is not described as behavior RED.

Final test slice self-review covered BPF branch offsets, real TSYNC result/errno checks, pointer liveness, real EPERM matching, nil-boundary checks, per-thread partial masks, and propagation of worker join errors. Committed as `c2464ea test(launcher): prove real syscall failures never produce a kernel boundary`. No production source edits in this slice. Controller's independent docs-only securebits clarification commit `cbbfc84` is separate and was neither authored nor staged by this worker. Final status contains only the pre-existing unrelated Sentinel plan modification.

Owned committed files: kernel_types.go, kernel_linux.go, kernel_inspect_linux.go, kernel_mutate_linux.go, kernel_unsupported.go, kernel_models_test.go, kernel_native_test.go, kernel_unsupported_test.go under internal/runtime/launcher. Own commits: 69ab0a1, 524ae7d, c2464ea.

The exact Task 2 private interfaces remain `TestMain` + `nativeModeEnv` (`SANDBOX_LAUNCHER_TEST_MODE`), `nativeMonitor() error`, `runNativeChild(mode string) ([]byte,error)`, `holdNativeThreads(n int, change func() error) (func() error,error)`, `nativeReject(mode string) (resultErr error)`, `nativeObservations() (map[int]KernelSnapshot,error)` and `denyNativeSetter(mode string) error`. These are test-only helpers, not a new product launch contract. Existing monitor mode validates then exits; it has no user exec mode, challenge protocol, readiness API or future helper assumed by this report. Task 2 must implement its own requested user isolation handoff using actual PrepareMonitor boundary and extend the fixed test binary as necessary.

Known scope and costs: securebits mismatch can terminate Go runtime by design; any failed initialization leaves caller responsible for ending its management process and external unknown/closed. Initial transient SETPCAP is required, never retained in successful final state. No complete FUSE privilege topology, user isolation, task authority, accepted/terminal journal, drain, production Manager integration, original image overhead or full Redis removal is claimed. Host race only validates host model/unsupported code, not a Linux race build. No independent reviewer was spawned by this worker; controller owns the required fresh reviews.


## Task1 complete independent review

# Task 1 independent review

## Spec compliance

**✅ Spec compliant for Task 1. Code quality: Approved. Findings: 0 Critical, 0 Important, 0 Minor.**

Reviewed original base `7c79d6a63bcca095aa07a6e34c640349be0371e0` through head `c2464ea`, including all four commits and the binding securebits amendment. Read the complete 1,169-line review package once in three contiguous segments, including EOF. No changed source file was separately crawled; no hunk required recovery. All eight requested/allowed Go files are present. The two documentation hunks express the amended runtime-thread securebits contract rather than silently broadening the implementation.

Task 1 implements the actual startup boundary and fresh trusted monitor boundary. This approval does not fulfill Task 2 user isolation/canonical script, production deployment, or later target lifecycle requirements.

## Strengths and safety judgments

- **Exact models and private evidence:** [kernel_types.go:17](/Users/dysodeng/project/go/cloud/sandbox/internal/runtime/launcher/kernel_types.go:17) defines the specified snapshot; its private boundary, copying Snapshot and validator at lines 36, 43 and 52 enforce exact root credentials, masks, role/PID, NNP, securebits, bounds and process properties. [kernel_models_test.go:9](/Users/dysodeng/project/go/cloud/sandbox/internal/runtime/launcher/kernel_models_test.go:9) uses literal initial/final role fixtures, all UID/GID positions and all 64 bits of all five sets. [kernel_unsupported.go:5](/Users/dysodeng/project/go/cloud/sandbox/internal/runtime/launcher/kernel_unsupported.go:5) and its dedicated test preserve nil/zero precedence and unsupported behavior.

- **Startup errors and irreversible partial state:** [kernel_linux.go:25](/Users/dysodeng/project/go/cloud/sandbox/internal/runtime/launcher/kernel_linux.go:25) serializes and consumes the initialization attempt before preflight. Every preflight, setter and postflight failure returns nil boundary; a boundary exists only after final actual inspection. There is no rollback or user-launch branch. [kernel_mutate_linux.go:41](/Users/dysodeng/project/go/cloud/sandbox/internal/runtime/launcher/kernel_mutate_linux.go:41) follows the required I setup → ambient clear → each bounding drop → final P/E/I → dumpable/subreaper sequence. The two real seccomp negatives prove first-setter EPERM preserves credentials and bounding-drop EPERM preserves the actual partial I=e0 state without returning a boundary. The documented caller obligation to terminate on initialization failure remains necessary; this library does not itself terminate on ordinary errors.

- **All-thread semantics and support contract:** [kernel_mutate_linux.go:24](/Users/dysodeng/project/go/cloud/sandbox/internal/runtime/launcher/kernel_mutate_linux.go:24) uses AllThreadsSyscall/6 for every setter, with no single-thread fallback. Capset allocations pass directly through uintptrescapes calls and remain live through KeepAlive. [kernel_inspect_linux.go:239](/Users/dysodeng/project/go/cloud/sandbox/internal/runtime/launcher/kernel_inspect_linux.go:239) uses the sole permitted scalar all-thread GET for securebits on both initial and later validation. The actual divergent securebits runtime fatal is a disclosed fail-closed availability cost under the amended contract, not an ordinary returned error or a passing Go test. CGO-linked runtime rejection maps ENOTSUP to ErrUnsupported while retaining errno. Foreign/manual native threads remain outside the stated trusted CGO0 contract.

- **Bounded real proc observations:** [kernel_inspect_linux.go:26](/Users/dysodeng/project/go/cloud/sandbox/internal/runtime/launcher/kernel_inspect_linux.go:26) bounds reads; [kernel_inspect_linux.go:57](/Users/dysodeng/project/go/cloud/sandbox/internal/runtime/launcher/kernel_inspect_linux.go:57) rejects missing/duplicate required fields, invalid signs/digits/field counts, numeric overflow, mismatched PID/Tgid and invalid thread counts. The inspected walker retains one 128-entry page and one status, caps observations at 4096 and each status at 64 KiB, and uses real per-task paths. ENOENT/ESRCH restarts the complete inspection for at most three total attempts; persistent instability returns unavailable with its cause. Role validation applies to every observed thread at [kernel_inspect_linux.go:255](/Users/dysodeng/project/go/cloud/sandbox/internal/runtime/launcher/kernel_inspect_linux.go:255); enumeration count is diagnostic, not fixed identity.

- **Concurrent boundary reads:** [kernel_types.go:43](/Users/dysodeng/project/go/cloud/sandbox/internal/runtime/launcher/kernel_types.go:43) and [kernel_linux.go:48](/Users/dysodeng/project/go/cloud/sandbox/internal/runtime/launcher/kernel_linux.go:48) protect copied diagnostics and validation refresh with the same boundary mutex. Role/PID are initialized before publication and never changed. ValidateCurrent checks current PID, rereads actual kernel state and only updates verified diagnostics after success. Failure leaves the last successful diagnostic copy, which is explicitly not execution authority. No data race or lock-order cycle was found in this code.

- **No idle-N amplification:** all product observations occur synchronously during initialization or explicit ValidateCurrent. The five product files add no clock, timer, ticker, Lease, watch, background goroutine, cache refresh, transport or authority operation. Native workers and context timers exist only in [kernel_native_test.go:28](/Users/dysodeng/project/go/cloud/sandbox/internal/runtime/launcher/kernel_native_test.go:28) and its test helpers.

- **Actual monitor and lifecycle evidence:** [kernel_native_test.go:157](/Users/dysodeng/project/go/cloud/sandbox/internal/runtime/launcher/kernel_native_test.go:157) records real PID1 state; the same binary's root exec at line 187 verifies inherited P/E/I=e0 then PrepareMonitor clearing I and reestablishing dumpable/subreaper. Existing and later locked runtime threads are inspected. Worker stop functions release once and bound joins, error paths register deferred stop before Fatal, and the rejection helper propagates join errors. Child execution uses a bounded context, WaitDelay, exact environment, cwd / and no ExtraFiles. Success acceptance requires process/attach/wait outcomes in addition to markers, so a later deferred cleanup failure cannot be accepted merely because a marker was printed.

## Issues

### Critical

None.

### Important

None.

### Minor

None.

## Evidence checked and named outside-risk checks

1. **Runtime support and pointer-liveness risk:** inspected only the installed Go 1.25.6 [syscall_linux.go:1111](/Users/dysodeng/go/go1.25.6/src/syscall/syscall_linux.go:1111) AllThreadsSyscall/6 documentation and implementation. It explicitly rejects CGO-linked binaries with ENOTSUP, marks both calls uintptrescapes and documents divergent-return termination. This supports the implementation's CGO and allocation claims without a new suite run.

2. **Native evidence misclassification risk:** inspected the Root proof, corrected evaluation, second metadata and relevant raw outputs. Recomputed the original metadata SHA256 and all seven first-batch log digests; all match the correction record. Independently counted the positive raw log: 1368 PASS, 0 FAIL, 0 SKIP. Actual PID1 began P/E/B=480, I/A=0, seven threads; final P/E/I=224, A/B=0, dumpable0/subreaper1. Actual monitor PID17 began P/E/I=224 and ended I=0 with five threads. RSS9532 KiB, allocated1633224 bytes and seven observed FDs are fixture observations only.

3. **Partial mutation and cleanup risk:** read both new raw EPERM logs and all second-batch terminal/inventory fields. Reject-setter, reject-partial and rerun reject-thread have attach/process/wait0, exited/Runningfalse/OOMfalse, accepted=true; partial failure explicitly shows every observed thread I=224 and P/E/B=480. Checked recorded individual container and volume removals for all ten first/second fixtures: each exit0. All recorded independent container/volume/network inventories are exit0 and empty. This is observed cleanup evidence, not an inference from best-effort removal.

4. **Honesty of failure evidence:** the first coordinator's original exit1 remains recorded; its incorrect generic marker oracle was corrected by evaluating retained evidence, not by rewriting the original run as exit0. The divergent securebits case has actual process/attach2 and wait-command0 returning2, as expected by the binding amendment. Neither is an unexplained warning in the positive output. The model RED log contains 1360 actual unsafe-state assertion messages and terminal Go FAIL with no compiler diagnostics; its initial wrapper did not preserve the underlying Go exit independently. The report discloses that limitation accurately and does not call it native behavior RED.

No tests, package/race suites, Docker lifecycle operations or git commands were run for this review. The only write was this requested report. Source-to-freeze association is supplied by the Root proof and binding review context, including the untracked-source first freeze and unchanged product hashes; no claim is made that the first binary came solely from then-committed HEAD.

## Cannot verify / remaining boundaries

- **Task 2:** user UID/GID1000, supplementary groups, all-zero user capabilities after new threads, secret/proc/ptrace/FD denial, user child cleanup/Fatal oracle and canonical script are absent by the task split. Exact next check: extend the fixed compiled monitor mode after actual PrepareMonitor, have its trusted parent inspect every blocked user thread, run the named hostile operations, and capture bounded release/wait plus exact resource inventories under the Task 2 gate. The existing monitor helper exits after verification; it is not an implemented user handoff protocol.

- **Linux race and actual CGO-linked runtime execution:** host race covers Darwin model/unsupported paths, not Linux synchronization; CGO rejection is verified here against installed Go source, not an executed CGO fixture. The code's common mutex is sufficient for this gate and no race defect was found. If dynamic coverage is required later, use a focused concurrent Snapshot/ValidateCurrent check on one real initialized boundary in a supported CGO0 fixture; separately verify a CGO-linked initial call returns nil/ErrUnsupported/ENOTSUP before mutation. Do not describe a CGO-dependent race build as the supported positive CGO0 launcher.

- **Enumeration instability:** the three-attempt restart and bounds are verified by code and parser tests, but the supplied native logs do not demonstrate a thread disappearing exactly during status reads or exhaustion of all retries. A focused controller-owned churn/vanished-task case would provide dynamic coverage if needed; this is not evidence of a current implementation defect, and no fake proc success should substitute for real kernel behavior.

- **Later production/lifecycle integration:** no production caller, authenticated transport, activation/BootID, accepted/terminal journal, full descendant draining, dual drain, scheduling/GC, FUSE privilege topology, fleet overhead or Redis removal is established by these files. Exact later check: inspect the eventual call sites for termination and external closed/unknown on every initialization failure, then review/test those later contracts in their own affected integration gate. A boundary or successful fixture is not a business execution authorization.

## Assessment

**Task quality: Approved.** The implementation is narrowly scoped, readable across model/inspection/mutation/platform responsibilities, and supports its security-sensitive success and partial-failure claims with actual Linux evidence. No Task 1 correction is required before proceeding to Task 2; the listed verification limits and later requirements remain open boundaries rather than claims of completed migration.


## Task1 Root actual native proof

# Root Task 1 first native evidence

Source HEAD 69ab0a17d0f1d362f0632daa8b1a8a9c85895a32 includes only model slice; 4 Linux source files were untracked during this freeze. Full actual 8 Go source SHA256 association is in task-1-root-native.json. Binary task-1-launcher-linux-arm64.test SHA256 50f98c8911084de60cb7a1fd7aa80eda9d5e336905b4797740d71b635e805ffc. The original frozen-source git diff did not include untracked sources and is not a complete source package.

Actual pinned existing Linux arm64 image, fresh true PID1 containers with exact fixture/project labels, UID0, NNP (except deliberate reject-nnp), ALL dropped plus KILL/SETGID/SETUID/SETPCAP (except deliberate missing/extra cases), no network/privileged/hostPID/host socket, readonly binary/root, owned tmp volume, 64MiB/128pids/0.5CPU/GOMAXPROCS2. No image pull or etcd server. Root actual run generated seven fresh fixtures.

- Positive full launcher package: 1368 PASS including 4 top-level tests, 0 FAIL, 0 SKIP; actual attach/process/wait result0. Actual PID1 bootstrap, existing and new runtime threads, actual root monitor exec PrepareMonitor, copied diagnostic and repeat/wrong-role/nonPID1 rejection passed. VmRSS9532kB, allocated1633224B, 7 observed FDs are fixture-only observations, not baseline-image overhead or fleet/SLO proof.
- Five fresh normal negative modes reject-nnp/reject-missing-cap/reject-extra-cap/reject-role/reject-thread: actual attach/process/wait result0 and exact `kernel boundary verified: <mode> rejected without boundary or credential mutation` markers. Actual library nil-boundary/unsafe error and surviving-thread before/after checks are inside the test binary.
- reject-securebits: actual attach/process2 and docker wait command0 returning2; real runtime fatal `AllThreadsSyscall6 results differ between threads`; no boundary marker. This is the explicitly expected fail-closed divergent-thread outcome, not a passing Go test or recoverable syscall error.

Every fixture actual state exited/Runningfalse/OOMKilledfalse. Each individual exact container and volume removal was observed exit0, each per-project container/volume/network inventory exit0/empty. All seven resources gone.

Root coordinator exited1 solely because its normal-negative oracle expected the nonexistent generic `kernel boundary verified: rejection` string. Original task-1-root-native.json and task-1-root-native-coordinator.log/rawlogs are preserved unchanged. Root corrected its own runner and evaluated the stored raw outputs/terminal/wait/individual removals/inventories: task-1-root-native-corrected-evaluation.json has original metadata digest, per-log digest and all seven accepted, evaluator exit0. This is a stored-evidence correction, not a rerun or claim that the original coordinator exited0. A preliminary navigation summary used key runs instead of actual cases and raised KeyError; corrected without changing evidence, not a behavioral RED.

Worker was released to add real seccomp TSYNC capset-first and partial bounding-drop denial. Those new cases require independent actual Root execution; unchanged product positive/normal cases are not repeated without a new reason. Task 1 independent compliance/quality gate remains pending. User-exec isolation/FD/secret/ptrace/Fatal cleanup belong to Task 2 and are not proven here. No business launch, authenticated target, descendant terminal, remote-write settlement, Redis migration or overall phases1–5 completion claim.

## Second frozen-source actual Linux failure cases

Kernel slice committed524ae7d, Root independent firstfreeze sourcehash comparison showed only kernel_native_test.go changed. New CGO0 arm64 binary task-1-launcher-failures-linux-arm64.test SHA256 d8bdd197a61be88c80fce47dbdfa9d97b5f03c1dd97278a33f08cca390c9e1f9. Root actual fresh reject-setter/reject-partial/reject-thread run: coordinator exit0; each attach/process/wait0, exited/Runningfalse/OOMfalse. Actual seccomp TSYNC capset denial EPERM preserved all observed capability sets and nilboundary. Actual cap-bounding-drop denial EPERM after successful I=e0 left every observed thread I=e0/P E B1e0 with no boundary and no rollback. Changed reject-thread join-error handling validated with actual rejection/no mutation. Each exact container/volume rm individuallyexit0, each project container/volume/network inventory0/empty. Full commands, actual states, sourcehashes and outputs task-1-root-native-failures.json and three rawcase logs. Unchanged product positive/normal cases were not unnecessarily rerun. Root formal doc-only securebits clarification cbbfc84 was committed after this source-associated run; no product difference. IndependentTask1 gate pending.


## Task2 complete implementation and I1 fix report

# Task 2 implementation report

Base: `42b5b9daf38c7c6cd4844aaa44b059c520c88e22`. Scope: same-package native tests and canonical native script; no product changes, production API, deployment, push/merge, user configuration or credential access. Pre-existing unrelated Sentinel plan edit not read or staged. Root owns all Docker lifecycle. No subagents/reviewers dispatched by worker.

## Design / ordered initialization

Test-only `sync.Once` holds the actual boundary returned by `BootstrapPID1`. Shared initialization keeps the original real initial/final snapshots, existing extra threads, ValidateCurrent, repeated bootstrap/wrong-role refusal, and post-bootstrap extra-thread validation. All positive native tests obtain this boundary; they cannot recreate it from a snapshot. The existing full `m.Run` path remains. This allows test order and focused selectors without repeating an irreversible bootstrap. Root explicitly accepted this approach.

Parent audit design uses actual live `/proc/<user>/task/<tid>/status`. For protected user `fd` and `environ`, the trusted monitor's pinned OS thread temporarily sets fsuid/fsgid=1000; real/effective/saved management IDs remain root. Root explicitly accepted this test-only exception: entering/restoring fsIDs must be observed in that TID's actual status because setfsuid's returned previous value does not indicate success; defer restoration precedes mutations, no exec during audit, restore then ValidateCurrent before reuse. No CAP_SYS_PTRACE addition or production setter change is authorized.

## TDD / first native RED freeze

RED intentionally leaves the actual user exec without Credential, then independently reads its live thread proc status and requires literal UID/GID arrays `[1000 1000 1000 1000]`. Three extra locked user threads wait for the trusted monitor. This should fail on actual root IDs, not compilation, missing marker or fixture setup.

Compile command: `env CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go test -c -o .superpowers/sdd/2026-10-07-launcher-kernel-boundary/task-2-red-user-linux-arm64.test ./internal/runtime/launcher`, exit 0. Root independently reported pinned image actual Linux/arm64; no guessed architecture/pull.

Binary SHA256: `8197daa8dedc2f0979264b18ff33f0109174014191b1b48e6220fdd046ccdf49`. Changed source: `kernel_native_test.go`, new `kernel_user_native_test.go`. Source frozen pending Root execution. Normal actual PID1 fixture, exact four capabilities/NNP; env MODE=`positive`, GOMAXPROCS=2; args `-test.v -test.count=1 -test.timeout=45s`. New internal fixed modes `monitor-user` and `user`. Expected actual process nonzero and `TestKernelUserIsolation` failure displaying actual root UID/GID; native evidence pending.

## User isolation RED / GREEN actual results and first slice

Root actual RED metadata `task-2-root-native-user-red.json` and raw `-positive.log`: attach/process/coordinator exit1, wait-command0 returning1, exited/Running=false/OOM=false, 1368 PASS / 1 FAIL / 0 SKIP. `TestKernelUserIsolation` failed on parent-observed TID34 root four UID/GID arrays. Root exact container/volume removes exit0; three independently queried inventories empty/exit0. This is meaningful executed behavior RED.

First GREEN candidate `task-2-user-green1-linux-arm64.test`, SHA `71261f548a33da341aea7e19ab21e538400866711eaa63e94645afe0670b0201`, compiled exit0, actually failed. User seven threads already had all four UID/GID=1000, five caps0, NNP1, but FD audit incorrectly treated user FD3 `/sys/fs/cgroup/cpu.max` as inherited. Root actual exits1, 1368PASS/1FAIL/0SKIP; cleanup killed/waited user and inventories empty. This is a failed GREEN/oracle limitation, not new Credential RED or product defect.

Systematic diagnosis read actual Go1.25.6 sources: `runtime/cgroup_linux.go:40` opens CPU file at startup and retains it for lifetime; `runtime/proc.go:916` invokes this before reading GOMAXPROCS at921; `internal/runtime/cgroup/cgroup_linux.go:142` opens exact v2 CPU file O_RDONLY|O_CLOEXEC. Therefore setting GOMAXPROCS=2 does not suppress runtime open. Correction requires trusted parent pre-exec F_GETFD on every open FD>2 (including secret, IPC, runtime, still-open enumeration FD), no ExtraFiles, and narrow child FD audit for exact `/sys/fs/cgroup/cpu.max`, actual `0::/` cgroup membership, and fdinfo read-only+CLOEXEC. Unknown descriptors still fail. Enumeration errors/disappearance and full128 page fail, no arbitrary EBADF suppression. Only this controlled helper's management FD inheritance is proven; no general production dynamic-FD lifecycle assertion.

Second GREEN binary `task-2-user-green2-linux-arm64.test`, SHA `64b2a6378381beed3be8a6265fa9bb00f957c93ae33fcd3bfdde1aa9d2a4effb`. Actual Root metadata `task-2-root-native-user-green2.json`, raw `-positive.log`: coordinator/attach/process/wait0, exited/Running=false/OOM=false, 1369PASS/5 top-level/0FAIL/0SKIP. Trusted parent pre-exec FD3..9 CLOEXEC, including actual management secret FD4 and observer FD9. User seven threads all four UID/GID1000, five caps0, NNP1, no supplementary groups. Parent pinned audit TID30 fsIDs1000 then0 observed in actual proc; complete user env exactly MODE=user/GOMAXPROCS=2; cwd `/`; stdio pipes; runtime cpu.max FD3 flags02400000, eventpoll/eventfd4/5. User actual setuid0/capraise/ptrace PID1 and monitor28 EPERM, NNP clear EINVAL, management environ/fd directories/root0700+0600secret permission denied. Prior management fd4 is user runtime eventpoll, not inherited secret. Actual user wait0 and monitor wait0. Exact removes0/all inventories0 empty. User never supplies credential evidence, only synchronization and syscall diagnostics.

Host `go test -race -count=1 ./internal/runtime/launcher` exit0 (2.224s); host package vet, CGO0 Linux arm64 package vet and `git diff --check` exit0. These are host model/unsupported race checks and cross-platform compilation/static checks, not Linux race execution. Self-review checked all original irreversible initialization assertions remain, real parent evidence ordering, fs-ID restoration verification, exact environment/FD audit, and error propagation. No product source changes. Canonical script and Fatal cleanup still pending and excluded from first slice commit.

First slice committed immediately as `2c5389c test(launcher): verify unprivileged exec from trusted parent proc evidence` (578 inserted lines, two native test files). User test file is intentionally one responsibility and 545 lines; no unauthorized split/product abstractions added.

## Fatal cleanup RED freeze

Binary `task-2-cleanup-red-linux-arm64.test`, SHA `29b77adf10c75de56a10342b3d4441c43e510b7aa400e9976aafac40eaa4663b`; CGO0 arm64 compile exit0. Changed existing native TestMain plus new `kernel_cleanup_native_test.go`; tested user source unchanged. Full positive actual PID1 invocation remains unchanged.

Fixed `cleanup-fatal` and `cleanup-negative` modes use real `m.Run` with `-test.run=^TestKernelWorkerCleanup$ -test.v -test.count=1 -test.timeout=15s`. They call PrepareMonitor, create extra workers and a real UID1000 waiting `cleanup-user`. The namespace PID1 receives the actual child identity, opens a pidfd, registers exact cleanup before triggering helper Fatal, and independently checks helper exit1, actual live/missing child proc and cleanup evidence. RED positive deliberately lacks release/wait and should fail on actual live orphan; negative preserves that omission intentionally. Parent always stops helper first, pidfd-signals only owned child and bounded Wait4-reaps it. Expected Fatal stdout lines carry an explicit prefix so they cannot be mistaken for outer positive-suite FAIL lines. No existing t.Failed status is used to mask real cleanup errors.

Cleanup RED Root metadata `task-2-root-native-cleanup-red.json` and raw `-positive.log`: actual coordinator/attach/process1, wait-command0 returning1, exited/Running=false/OOM=false, 1370PASS/2FAIL (outer and cleanup-fatal)/0SKIP. Omitted positive cleanup left actual PID25 Sleeping/PPid1, four UID/GID1000/caps0/NNP1; independent assertion failed, then exact pidfd kill and Wait4 status9 reaped it. Intentional negative helper33 exited1, orphan41 remained Sleeping/evidence=false, negative test passed and independently killed/reaped status9. Each exact remove0; three inventories0/empty. No compile/helper error masqueraded as RED.

GREEN adds actual worker join, child release, bounded child Wait and cancellation fallback in registered Cleanup. Evidence marker depends only on those actual results; an already-failed t.Fatal cannot suppress cleanup errors. There are no mutable production hooks to restore. Workers join before explicit fixture FD cleanup; generic process cleanup is registered before readiness/error checks. The independent positive requires actual helper exit1/reached Fatal, actual user proc disappearance and actual wait/join evidence. Negative requires actual live non-zombie user and absent evidence, then owned pidfd SIGKILL and bounded actual Wait4.

Self-review removed an unnecessary async FS-audit worker: audit now runs synchronously on pinned helper main goroutine, restoring and validating all thread credentials before unlock. Failed restoration leaves the goroutine pinned until TestMain exits with failure, no new child exec or polluted pooled thread. This avoids returning while an audit timeout worker remains active. Wait diagnostics now inspect the closed done channel before reading the published outcome, including failed cleanup paths.

GREEN binary `task-2-cleanup-green-linux-arm64.test`, SHA `eb671ddf8434bea8750654ccb3d4f868254a01aedd90edb05947a2e45cbe705e`, crosscompile0. Root actual `task-2-root-native-cleanup-green.json`/raw `-positive.log`: 1372PASS/6 top-level/0FAIL/0SKIP, coordinator/attach/process/wait0, exited/Running=false/OOM=false. Positive helper17 Fatalexit1, user25 actual wait0/workers joined/evidence=true and independently absent proc. Negative helper33 Fatalexit1, user40 live Sleeping/evidence=false, independent pidfd SIGKILL then Wait4 status9. Final synchronous user audit TID62 fsIDs1000 then0 and full user isolation pass. Actual fixture PID1 VmRSS9540KiB, Go allocation1502688B, observedFD7 including observer. These are test fixture measurements only, not original production image overhead or all-N scale costs. Each exact container/volume remove0; three inventories0/empty.

Fresh host launcher race exit0 (1.806s), host package vet/crossLinux CGO0 package vet/diffcheck0. Full delta self-review checked expected Fatal output prefix, error visibility despite t.Failed, registered orphan cleanup, pidfd identity preventing wrong-process kill, actual Wait4 after helper exit/adoption, FS restoration before helper success, and unchanged production source. Root owns full repository final checks and independent reviews.

Second coherent slice committed immediately as `118dbfc test(launcher): prove Fatal cleanup with an independent live-child oracle` after actual native GREEN and fresh checks. Frozen native source hashes: cleanup test `c8e4cd60faa17fc767ba185a2226f387f45ab4b9deda531cc9abd8ca2fe89bd8`; driver `f445a94dbab8db06ba4f96cb7c1a5142b020a094bde048b9138703934486c647`; user test `036838af46c79654dc9846c576d2fc0b4bfe1ebc9a027f2b0d162151efa83831`.

## Canonical script setup and verification

Script builds CGO0 test binary for independently inspected local pinned Linux arm64/amd64 image. It creates one unique per-run `sandbox.test.project=sandbox-launcher-kernel-test-<pid>-<time>` and fixed `sandbox.test.fixture=launcher-kernel-boundary`, both on every exact owned container and volume. Each case is fresh, network none/default private PID namespace/root UID0, read-only root and binary, writable owned tmp volume only, cap-drop ALL plus exact case capability list, NNP except reject-nnp, 64MiB/128pids/0.5CPU/GOMAXPROCS2. It does not run etcd or create a network. Positive runs all tests with count1 and45s timeout; seven normal rejection modes expect0 and divergent securebits expects2. Actual attach exit, inspected terminal state including Running/OOM, docker-wait command status and returned process exit are compared. Positive fails on actual FAIL/SKIP and requires user+cleanup evidence/PASS. Independent inventories filter only the unique project so mismatched fixture labels cannot hide leftovers. Removal verifies exact names and both labels; any failed remove/inventory makes script fail. Successful canonical marker comes only from final successful cleanup/inventory observation.

Before execution, Root read-only review found Docker RepoDigests contain canonical repo@digest without the tag; script now compares `gcr.io/etcd-development/etcd@sha256:5ed4e32061aa061f970d6500629213e8579ef17571dcf9789074966acb9afad1` while retaining the pinned tag@digest image invocation. This is a setup defect caught before execution, not behavior RED. A real Bash3 empty-array+set-u check failed and informed safe optional-array expansion; bash syntax then passed. Neither event is native TDD evidence. Source manifest SHA256 covers all package Go files and script; source.diff only represents tracked changes, not untracked files. Current source/script frozen for Root canonical execution; first submitted script SHA `73b052aa8a9b5a5218860de198d36dd9a95e9a4072cec0b0db2d89e63afef563`.

Worker fresh host full repository `go test ./...`, `go vet ./...`, `go build ./...` all exit0 during source freeze. Logs `task-2-worker-repo-test.log`, `task-2-worker-repo-vet.log`, `task-2-worker-repo-build.log`; vet/build empty. Repository tests include cached packages; nonverbose output does not enumerate all existing skips. No Linux race result is claimed.

## Scope / remaining integration obligations

Changed only `internal/runtime/launcher/kernel_native_test.go`, new `kernel_user_native_test.go`, new `kernel_cleanup_native_test.go`, and `scripts/test-launcher-kernel.sh`. No product correction was needed. No production raw-start API/CLI, authorization gate, ready/terminal shortcut, Manager/config/credentials/FUSE/resource quota changes or external publish/deploy were made. Original initial/final/repeated-role/new-thread assertions and Task1 negative modes remain.

Evidence proves this controlled CGO0 test helper's kernel boundary, literal UID/GID1000/groups0/caps0/NNP1 user exec and parent proc/FD/environment observations, hostile denials, plus actual expected Fatal cleanup and deliberate live orphan detection/reaping. It does not prove original-image resource overhead, all-N idle scaling, namespace/cgroup isolation as a whole, full monitor/target descendant terminal settlement, authenticated command execution, remote settlement, FUSE initial permission topology, or the completed native etcd migration/phases1–5. Test-only temporary fsIDs and transient bootstrap SETPCAP remain explicit costs; no broad production FS credential policy is introduced. Native script currently relies on the actual cgroup-v2 fixture's exact CPU runtime file; unknown FD/cgroup layout fails clearly rather than silently weakening the oracle.

## Final canonical actual execution, script commit and handoff

Root executed both complete script variants against nine fresh exact-owned fixtures each. First submitted script SHA `73b052aa8a9b5a5218860de198d36dd9a95e9a4072cec0b0db2d89e63afef563` actually exited0, positive1372PASS/0FAIL/0SKIP, expected securebits2 and all other cases0, 18 individual removes0 and independent empty inventories. Proof retained under `task-2-root-canonical/`, `task-2-root-canonical-coordinator.log`, `task-2-root-canonical-independent-check.json`. After source freeze was released, worker added explicit `docker create --pull=never` to eliminate inspect-to-create image-removal pull fallback. No image was deleted or pulled for a test. Root first post-run old-variant evaluator mistakenly compared the newly changed script's current hash to the old expected hash, producing AssertionError; Root rechecked using the recorded pre-run73b SHA, committed Go hashes and the single flag change. This evaluator/source-association mistake did not alter native results and was not hidden by rerunning that old variant.

Final explicit-no-pull script SHA `a8333cb3dc777541c29a48d38a961f31ac9018c425022dff1a0135629db83ec6`, source commit `118dbfcf01a8817c4a51a3772a1bed6cf5153398`, built test binary SHA `eb671ddf8434bea8750654ccb3d4f868254a01aedd90edb05947a2e45cbe705e`. Actual script exit0 (Root session9034). Run project `sandbox-launcher-kernel-test-82621-1791326197`; actual local image linux/arm64 and exact pinned canonical RepoDigest. Evidence: directory `task-2-root-canonical-no-pull/`, sibling `task-2-root-canonical-no-pull-independent-check.json` and `task-2-root-canonical-no-pull-coordinator.log`. Worker read final independent-check JSON and cleanup log directly after Root reported completion. All ten committed package Go source hashes plus frozen script hash matched; actual binary equals final native GREEN source.

| Fresh mode | Attach exit | Actual process exit / wait result | wait-command exit | Observed terminal state |
| --- | ---: | ---: | ---: | --- |
| positive | 0 | 0 | 0 | exited, Running=false, OOM=false |
| reject-nnp | 0 | 0 | 0 | exited, Running=false, OOM=false |
| reject-missing-cap | 0 | 0 | 0 | exited, Running=false, OOM=false |
| reject-extra-cap | 0 | 0 | 0 | exited, Running=false, OOM=false |
| reject-role | 0 | 0 | 0 | exited, Running=false, OOM=false |
| reject-thread | 0 | 0 | 0 | exited, Running=false, OOM=false |
| reject-securebits | 2 | 2 | 0 | exited, Running=false, OOM=false |
| reject-setter | 0 | 0 | 0 | exited, Running=false, OOM=false |
| reject-partial | 0 | 0 | 0 | exited, Running=false, OOM=false |

Final positive1372PASS/0FAIL/0SKIP; all case markers independently checked. Divergent securebits runtime fatal is intentionally nonzero with no boundary marker. Actual created-container JSON confirmed resource/security configuration; exited JSON confirmed terminal state. Cleanup log records every one of nine container and nine volume removes with actual exit0. Root additionally queried container/volume/network inventories independently by unique project: all exit0/stdout empty/stderr empty. No worker Docker lifecycle occurred and no cleanup claim relies on best-effort rm. Neither canonical variant ran etcd.

Final syntax/diff-check exit0 immediately before commit. Script committed executable mode100755 as `7ba4d27 test(launcher): add repeatable pinned-image native kernel gates`. No Go source changed during the two canonical runs, and no redundant full repository test rerun followed. Final git status contains only the pre-existing unrelated Sentinel plan modification.

Delivered commits: `2c5389c`, `118dbfc`, `7ba4d27`. Task2 implementation and required actual native/script checks complete; no unresolved implementation defect identified in self-review. Root retains independent Task2 and new-delta/affected-integration review responsibility, evidence persistence, and the later overall migration obligations described above. This is an independently reviewable kernel prerequisite unit, not completed migration/deployment.

## Independent review fix round 1 — I1 only

Base `7ba4d27a9f7904998f24f867d963d55eedba35e0`. Read `task-2-review.md` completely and applied receiving-code-review/systematic-debugging/TDD verification discipline. The single Important finding is **I1 — Attach failure and timeout termination can bypass the native execution deadline.** Confirmed both exact source paths: an early failed attach could leave a running container and enter unbounded docker wait; after timeout, failed container kill followed by TERM-surviving attach could enter an unbounded local wait. No kernel/Go source, creation/security/resource contract, positive/negative marker expectations, external Docker resource or unrelated plan/configuration/credential was changed.

Focused harness: `task-2-script-failure-check.py`, executing the actual canonical Bash file with isolated fake Docker and fake Go commands via PATH. Fake Go writes an explicit fake binary fixture instead of building anything. Fake Docker records all calls and controlled state, verifies exact names for removal, and uses a real local process with a real SIGTERM handler for the timeout case. This is shell fault-injection evidence only, never native kernel/Docker/image execution evidence. The real script's 60-second attach deadline was not shortened. Cases run concurrently in isolated process groups/directories; external watchdogs own only their respective test process groups.

RED command: `python3 .superpowers/sdd/2026-10-07-launcher-kernel-boundary/task-2-script-failure-check.py red`, actual harness exit1 against unchanged script SHA `a8333cb3dc777541c29a48d38a961f31ac9018c425022dff1a0135629db83ec6`. Evidence in `task-2-script-failures-red/summary.json` and per-case logs/events/results:

- `early`: fake attach actually exited17, inspect returned `running true false 0`, and the real shell invoked fake docker wait, which remained blocked. External watchdog fired after8.016s and forced shell exit-9. Script had not reached exact-owned removal/inventory cleanup.
- `timeout`: real60s execution deadline elapsed; fake container kill actually exited19; real local attach process received TERM, recorded survival, and continued running. The real shell blocked in local wait until external watchdog fired69.010s and forced exit-9. Script had not reached cleanup.
- RED summary's initial `attach_force_terminated_and_reaped=true` subfield measured process absence after the harness's SIGKILL and does **not** establish script termination/reaping. Overall RED was false on both cases. Before GREEN, strengthened that sub-assertion to require no watchdog plus the script's actual `attachExit=137 forced=true joined=true` evidence and absent client PID. Original RED artifacts were preserved, not rewritten or reclassified. Root independently read and kept watchdog cleanup attributed to the harness.

Fix in `scripts/test-launcher-kernel.sh` only:

1. Compute the same literal expected attach/process outcome as before (0, or2 for divergent securebits), then check actual attach exit and inspected `exited false false <expected>` **before** docker wait. A mismatch records actual observations with wait=`not-run`, exits1 and reaches exact-owned cleanup immediately. Existing actual wait-command/process-result checks remain for accepted terminal states.
2. On execution timeout, record actual container-kill exit, send local attach TERM, observe up to2s, escalate to KILL if still alive, and observe up to another2s. Call local wait only after observing exit, and record its actual result. A client still observed after the bounded escalation records joined=false and fails without an unbounded wait. Script still exits1 through the existing owned-resource cleanup. This bounds the named attach-client failure paths; it makes no general guarantee about an unresponsive Docker daemon.

GREEN command: `python3 .superpowers/sdd/2026-10-07-launcher-kernel-boundary/task-2-script-failure-check.py green`, actual harness exit0, current script SHA `2378bcb5128d2ea815fc229c3b2472f44c2acf66c125acdaeae7a8164dd179eb`. Evidence `task-2-script-failures-green/summary.json` and per-case artifacts:

- `early`: script exited1 naturally in1.571s, no watchdog. Recorded `attachExit=17 terminal=running true false 0 waitCommandExit=not-run processWait=not-run`; docker wait never called. Exact mock container/volume removed, two actual shell-observed rm exit0 records, three independently queried mock inventories empty, no false success marker.
- `timeout`: script exited1 naturally in64.071s including real60s deadline, bounded escalation and cleanup; no watchdog. Container kill exit19, actual client PID86260 survived TERM, then script recorded `attachExit=137 forced=true joined=true` and client PID was absent. No docker wait call; exact mock removals/three empty inventories observed, no false success marker.

Fresh `bash -n scripts/test-launcher-kernel.sh` and `git diff --check` both exit0. Self-review checked only one source path changed; literal expected terminal/attach values are unchanged from the retained actual nine-case canonical evidence; prior actual wait/marker/count/security/resource/no-pull/ownership logic remains; local wait cannot be entered while its client is still observed alive; real kill/termination errors remain visible. No Go/native/full repository suites or Docker operations were repeated. Root explicitly confirmed no new native execution is required for this failure-path-only fix, will compare existing actual observations with the new precheck, and owns the scoped fix review. Latest script has focused shell-fault proof; earlier actual canonical proof remains associated with its original a833 SHA, not falsely relabeled as this revision.

I1 fix committed immediately after covering GREEN and self-review as `6b7c58b test(launcher): bound failed native attach shutdown paths` (only canonical script,32 insertions/5 deletions). Final worktree retains only the unrelated pre-existing Sentinel plan modification. No unresolved I1 issue found in self-review; independent scoped acceptance remains Root/reviewer-owned.


## Task2 complete original independent review

# Spec Compliance

- ✅ Task 2 spec compliant for the verifiable kernel-prerequisite implementation: same-binary fixed test modes, explicit UID/GID1000 and empty supplementary groups/ambient caps, independent live parent thread inspection, hostile syscall/proc/secret checks, actual Fatal positive/negative cleanup, and exact-owned canonical native fixtures are implemented. This is not approval of the complete migration or a merge.
- ⚠️ The quality issue I1 below blocks acceptance of the canonical script's failure handling, although the supplied successful native runs meet the requested positive/rejection observations.

# Strengths

- `internal/runtime/launcher/kernel_user_native_test.go:65,163,205,215`: the fixed-mode process driver supplies the entire environment, root cwd and explicit stdio; the trusted PID1 checks the real root monitor before and after preparation, and the prepared monitor reads the live user's actual thread statuses. User stdout supplies phases and hostile-attempt diagnostics rather than credential attestations.
- `internal/runtime/launcher/kernel_user_native_test.go:246,259,289,310,386`: explicit Credential1000/1000/groups-empty/AmbientCaps-empty is followed by four-ID, five-capability, NNP and supplementary-group checks. The filesystem-credential audit registers restoration before mutation, pins its goroutine, verifies the actual TID IDs on entry/restoration, and requires ValidateCurrent before unlocking; failed restoration retains the pin until helper exit.
- `internal/runtime/launcher/kernel_user_native_test.go:402,479,515`: actual privilege escalation and protected-management reads must fail with permission errors; parent pre-exec CLOEXEC checks and the narrow runtime CPU-file check distinguish the known runtime descriptors from leaked management descriptors. Unknown descriptors fail.
- `internal/runtime/launcher/kernel_cleanup_native_test.go:38,76,91,139,187`: cleanup is registered before readiness/Fatal, the independent PID1 holds the child's pidfd before triggering Fatal, requires actual helper exit1, checks positive proc disappearance and wait/join evidence, and detects/reaps the deliberate live negative. Real cleanup results control evidence even after Fatal; existing t.Failed cannot manufacture success.
- `internal/runtime/launcher/kernel_native_test.go:159,171`: shared sync.Once stores the actual bootstrap result; initial/final inspection, repeated-role rejection, existing threads and newly created threads remain checked. Native mode guards retain honest host skips.
- `scripts/test-launcher-kernel.sh:14,54,68,81,109`: the script uses the inspected pinned local image and explicit no-pull, all nine fresh modes, exact capability/NNP configurations and resource bounds, full-positive FAIL/SKIP checks, two-label removal checks and project-only independent inventories. Success is emitted only after successful cleanup observations.
- Evidence read in full: `task-2-report.md` and `task-2-root-native-proof.md` retain meaningful actual user-ID and missing-cleanup REDs, the failed first FD-oracle candidate, final native/source hashes, both script revisions and the corrected source-association mistake. They report final 1372 PASS / 0 FAIL / 0 SKIP, nine terminal fixtures, individual removal outcomes and distinct host/static versus native results. Expected child-Fatal output is explicitly classified; it is not hidden as a clean child run.

# Issues

## Critical (Must Fix)

- None.

## Important (Should Fix)

- **I1 — Attach failure and timeout termination can bypass the native execution deadline.** `scripts/test-launcher-kernel.sh:105`: the script records terminal state, then invokes synchronous `docker wait` at line107, and only validates `exited false false <expected>` at line111. If `docker start -a` exits early because its attachment fails while the container is still running, the bounded attach loop has already finished; `docker wait` can now block indefinitely. This is particularly relevant to rejection fixtures, which have no Go test timeout. The gate neither reports the known attachment/nonterminal failure promptly nor reaches its exact-owned cleanup trap. The same named risk exists in the timeout branch at lines93–98: docker kill failure is ignored, the local attach client receives TERM, and an unconditional wait follows without a further deadline or escalation. Sending TERM alone does not establish that the local attach client exited; if it remains alive, cleanup is again unreachable. Validate the inspected terminal state and attach outcome before calling docker wait, failing through cleanup immediately on a nonterminal state; alternatively bound the subsequent wait by the remaining execution deadline. Separately give local attach-client termination a bounded grace period and force termination/reap if needed. Reordering terminal validation fixes only the first branch, not the timeout branch. This is a source-proven absence of execution bounds, not a claim that recorded successful runs hung or a demand for general Docker daemon-health guarantees. Focused fake-Docker shell checks for early attach failure plus Running=true, and failed container kill plus an attach client that survives TERM, would exercise these paths without repeating native suites.

## Minor (Nice to Have)

- None.

# Named checks and review limits

- Read the entire original `review-42b5b9d..7ba4d27.diff` package, lines1–1082 through EOF, once in bounded ranges. Reviewed all five changed paths, including the two binding-design clarifications. No Git commands reconstructed the diff; no checkout/index/HEAD/branch mutations occurred.
- **Named cut-off check: shared irreversible bootstrap must preserve previous assertions.** The package split `initializeNativeBoundary` between hunks, so inspected only its omitted body in `kernel_native_test.go:171–220`; the original initial/final/repeated-role/fresh-thread checks are retained.
- **Named affected-helper check: repeated stop calls must safely join before cleanup.** Inspected the existing `holdNativeThreads` body at `kernel_native_test.go:111–146` (the bounded read also included adjacent preceding context). Its release is sync.Once protected and its shared join has a five-second bound; the tail was already present in the diff. No wider source crawl or external documentation lookup was needed.
- The first combined display truncated part of Root's proof; reread that proof alone through EOF. No missing evidence was inferred from truncation, and no test was rerun to regenerate evidence.
- **Named script check: bounded attach and terminal observation failure paths.** Reviewed both `scripts/test-launcher-kernel.sh:93–98` and `105–111` from the already-read package; the same I1 covers their unbounded waits. No external daemon/client failure experiment was run, and no guarantee about an unresponsive Docker daemon is inferred.
- No existing suite, race run, native fixture or Docker operation was repeated. No unrelated plan, credential, configuration or sibling workspace was read. Only this review report was written.

# Cannot Verify (separate items; not additional findings)

- ⚠️ **Underlying unchanged Task1 implementation:** this delta consumes `walkKernelThreads`, parsing and boundary operations; their complete syscall/enum/error implementation is outside the Task2 diff. Task1's independent gate remains the authority for that source, while the supplied Root native proof supports the actual observed executions.
- ⚠️ **Dynamic Linux race/CGO execution:** the supplied evidence explicitly distinguishes host model/unsupported race and CGO0 Linux execution. There is no Linux race execution proof, and CGO is outside the supported runtime contract.
- ⚠️ **Precisely forced proc-thread churn:** this task creates and holds real extra threads, but does not independently force each enumeration retry/churn failure. Do not infer that coverage from the successful extra-thread observation.
- ⚠️ **Original production image overhead and all-N idle scaling:** reported allocation/RSS/FD/thread observations are test-fixture measurements; production-image and fleet-scale cost require the later integration work.
- ⚠️ **Authenticated execution permit and real production monitor integration:** the changed entry modes are test-only. The future authenticated transport/permit, Manager and adapter migration are not delivered by this delta.
- ⚠️ **All-descendant terminal/drain and remote settlement:** the independent oracle proves the named actual cleanup child; it does not prove arbitrary target/monitor descendants, both drains or remote terminal settlement.
- ⚠️ **Namespace/cgroup escape isolation and production FUSE permission topology:** fixture resource/security settings and user kernel credentials do not certify these broader boundaries.
- ⚠️ **Fresh native etcd / complete Redis replacement and phases1–5:** no such execution or completion is claimed or demonstrated here. The supplied full-repository host checks include cached tests and cannot substitute for those later gates.
- ⚠️ **Controller's final independent new-delta/affected-integration gate and permanent evidence retention:** the report leaves those controller obligations pending; this Task2 review alone does not close them.

# Assessment

**Spec compliance:** Approved for the verifiable Task2 implementation, with the explicit verification boundaries above.

**Task quality:** Needs fixes — 0 Critical, 1 Important, 0 Minor.

**Reasoning:** Real kernel/user evidence and Fatal cleanup oracles are substantively implemented and backed by separately recorded Root native results. Fix I1 so an attachment failure cannot leave canonical acceptance waiting outside its execution deadline, then perform a scoped review of that fix; the successful existing native results do not need to be repeated merely for this review.


## Task2 complete independent scoped fix review

- **I1 — Attach failure and timeout termination can bypass the native execution deadline** — **ADDRESSED**. Both parts of the grouped finding are addressed:
  - **Early failed attach / nonterminal container before wait — ADDRESSED**, `scripts/test-launcher-kernel.sh:127`: the unchanged expected exit (0, or 2 for reject-securebits) is computed before the new precheck at line 131. Any attach-exit mismatch or state other than the exact expected `exited false false <expected>` writes the actual observations with `waitCommandExit=not-run processWait=not-run` and exits at line 133. The synchronous `docker wait` at line 136 is therefore unreachable for the reported early attach failure with Running=true. The original wait-command/process-exit validation remains at line 138.
  - **Failed container kill / TERM-surviving attach client on timeout — ADDRESSED**, `scripts/test-launcher-kernel.sh:52`: `terminate_attach` sends TERM, polls for at most a two-second grace, escalates to KILL if still observed, then polls for at most a second two-second grace. It returns with `joined=false` at line 65 if the client remains observed, without entering local wait. Local `wait` at line 67 is reached only after observing client disappearance and records its actual status. The timeout caller records the actual container-kill exit at lines 115–117, calls this helper at line 118, and exits through cleanup at line 119 even if termination fails. There is no remaining unconditional local wait of a live attach client in this timeout path.

### New Breakage in the Fix Diff

- **None** — 0 Critical, 0 Important, 0 Minor. Reviewed the full `review-7ba4d27..6b7c58b.diff` through EOF once. The only product-file change is the canonical script; expected terminal/attach values and the accepted-state wait check are preserved. The helper's failure branch cannot bypass the caller's failure exit, and the success path reports the actual local wait status rather than suppressing it.

### Evidence Checks

- **Read-only source identity check** — `shasum -a 256 scripts/test-launcher-kernel.sh` returned `2378bcb5128d2ea815fc229c3b2472f44c2acf66c125acdaeae7a8164dd179eb`, matching the retained focused GREEN summary. No tests, Docker operations, native fixtures, or git commands were run by this reviewer.
- **Retained focused early-failure GREEN checked** — `task-2-script-failures-green/early/evidence/positive-result.txt:1` records `attachExit=17 terminal=running true false 0 waitCommandExit=not-run processWait=not-run`. The raw events contain no docker wait and show exact-name removals followed by three inventory queries. `task-2-script-failures-green/early/evidence/cleanup.log:2` and line 4 record both removal exits as 0. The summary records natural script exit 1 in 1.571 seconds, with no watchdog and all early-case checks true.
- **Retained focused timeout GREEN checked** — `task-2-script-failures-green/timeout/evidence/positive.log:1` records container kill exit 19; the raw events record the attach client's actual TERM survival. `task-2-script-failures-green/timeout/evidence/positive-termination.txt:1` records `attachPID=86260 attachExit=137 forced=true joined=true`, corroborated by the shell's Killed diagnostic. The harness requires no watchdog, absent client PID, and this actual termination/wait record for its strengthened reaping check. Raw events show exact-name removals and all three inventory queries; the cleanup log records both removal exits 0. The summary records natural script exit 1 in 64.071 seconds, preserving the real 60-second execution deadline and passing all timeout-case checks.
- **Retained RED attribution checked** — `task-2-script-failures-red/summary.json` records watchdog-driven exits -9 at 8.016 and 69.010 seconds and failed overall results. Its original client-absence subfield does not prove script-owned termination/reaping; the appended report explicitly discloses this and the GREEN harness strengthens that oracle. The RED is not credited with cleanup or successful script termination.
- **Verification coverage checked** — `task-2-report.md:106` names the actual focused shell-fault GREEN command and output, and line 111 reports fresh bash syntax and diff-whitespace checks as exit 0. The covering fault cases exercise the changed control paths in the actual Bash script via fake Docker/Go, with a real TERM-surviving local process. No uncovered doubt required another focused execution. These records are shell-fault evidence, not native kernel/container evidence. Prior canonical native evidence remains bound to script SHA `a8333cb3dc777541c29a48d38a961f31ac9018c425022dff1a0135629db83ec6`; no native rerun of the new SHA is claimed.

### Out-of-Scope Observations

- **None**. No additional source, other task, or whole-branch review was performed. General Docker daemon responsiveness remains outside the narrowly stated I1 finding; this fix does not claim such a guarantee.

### Verdict

- **Fix round: All findings addressed, no new Critical/Important breakage.** Grouped findings: 1 ADDRESSED, 0 NOT ADDRESSED; both I1 subcases addressed. New breakage: C0/I0/M0. Out-of-scope observations: 0. No findings remain open.


## Task2 Root actual native proof

# Root Task 2 actual Linux evidence

## Actual user isolation behavior RED

Worker RED freeze binary task-2-red-user-linux-arm64.test SHA2568197daa8dedc2f0979264b18ff33f0109174014191b1b48e6220fdd046ccdf49 independently checked. Source association captured in task-2-root-native-user-red.json for all actual source files; new test was untracked and shared-bootstrap native driver modified, not falsely claimed committed final source. ROOT fresh pinnedLinuxarm64 truePID1 fixture samefourcaps/NNP/no-network/readonlyroot+binary/ownedtmp/64MiB128pids0.5CPU/GOMAXPROCS2, positive full package -test.v -test.count=1 -test.timeout45s.

Actual coordinator/attach/process exit1, docker wait command0 returning1; exited/Runningfalse/OOMKilledfalse. 1368PASS including4top, 1FAIL TestKernelUserIsolation, 0SKIP. Task1 realinitial/final/rootmonitor/newthread/repeatedrole checks passed. Test2 realprepared rootmonitor spawnedactualuser/helper with3additional lockedthreads but omittedCredential. Trusted parent directly read actualuserTID34 /proc status: UIDs/GIDs=[0,0,0,0], all5caps0, NNP1,7threads. Exact behaviorassertion `must have four UID/GID=1000` failed. This is expected actualbehaviorRED, not compile/helper/oraclemarkerfail and not GREEN. The old runner normalpositiveacceptance staysfalse correctly.

Each actualowned container/volume rm observedexit0, exactprojectthreeinventories0empty; no fixtureleft. Raw actualoutput task-2-root-native-user-red-positive.log; full commands/sourcehashes/state/wait/removals task-2-root-native-user-red.json. Sourcefreeze released to implementCredential GREEN afterrealresult delivered.

## First GREEN candidate actual failure retained

Candidate task-2-user-green1-linux-arm64.test SHA71261f548a33da341aea7e19ab21e538400866711eaa63e94645afe0670b0201: actualcoordinatorattachprocess1/waitcmd0value1/exitedRunningfalseOOMfalse,1368PASS/1FAIL/0SKIP. Trustedparent directuser7threads allfourUID/GID1000/allfivecaps0/NNP1; monitorinitial/finalrealprocvalid. PinnedparentauditTID31 actuallyfsUID/GID1000, userenvexact, FD0/1/2pipes; fd3=/sys/fs/cgroup/cpu.max caused strictnonstdio oraclefailure, auditfsIDsrestored0; userkilled+waited, helperexit1. Allindividualexactrm0/inventoryempty. Fullmetadata/raw task-2-root-native-user-green1.json/-positive.log preserved. Not Credential RED/GREEN or setup failure. Worker released for targetedsource-rootcause analysis, no arbitrary FD allowlist imposed byRoot. Root checked actualGo1.25.6 runtime/cgroup_linux.go defaultGOMAXPROCSInit opens persistentCPUlimitfiles; runtime/proc.go initinvokesbeforeGOMAXPROCSenvselection; internal/runtime/cgroup/cgroup_linux.go OpenCPU openscpu.max O_RDONLY|O_CLOEXEC. This is sourcehypothesis forruntime-ownFD; concreteactualtest mustdistinguish inheritedmanagementFD and ownruntimeFD, notclaim allrunningFDsstdio. Root initialrg on runtimecgroup wrapper foundno literal(cpu.max nestedimplementation), correctednestedsourceinventory; notbehaviorRED.

## User isolation real GREEN

Frozen binary task-2-user-green2-linux-arm64.test SHA64b2a6378381beed3be8a6265fa9bb00f957c93ae33fcd3bfdde1aa9d2a4effb checked independently. Actualcoordinatorattachprocesswait0/exitedRunningfalseOOMfalse,1369PASS/5top/0FAIL/0SKIP; old kernelassertionsstillpass. Actualparent pre-exec fd3..9eachCLOEXEC (cgroup/secret/eventpoll/eventfd/pipes/heldobserverdirectory); no ExtraFiles. User7actualthreadsfourUID/GID1000/all5caps0/NNP1. ActualauditTID30 fsUID/GID1000 thenrestored0, exactuserenv MODE=user+GOMAXPROCS2; FD0/1/2pipes,3cpu.maxactualfdinfo2400000/cgroup0::/,4/5eventpoll/eventfd. InstalledGoOpenCPU source+parentCLOEXEC+currentuserflag/context evidence distinguish runtime reopened read-onlyCPUFD, no blanketunknownFDallowance. Realhostile setuid0/capraise/ptracePID1+monitor28EPERM; NNPreverseEINVAL; managementenviron/fddirectory/root0700file0600secret/pathfdpermissiondenied. Oldmanagementfd4isuserownEventpoll notsecret. Actualuserwait0/monitorwait0 afterrelease. Allindividualexactrm0/threeinventories0empty. Fullsourcehashes/commands/state/log task-2-root-native-user-green2.json/-positive.log. Scope applies controlledtesthelpers only; notproductionFDlifecycle/namespace/cgroupescape ordescendantEnd. Workerreleasedtosmalluser-slicecommitandFatal/canonicalremainingwork.

User/shareddriver coherentcommit2c5389c: Root independentlygitshow comparedall9 committedGo hashes againstactualGREEN2snapshot, exactmatch. Canonicalscriptexcludeduntracked andnotverifiedyet.

## Real Fatal cleanup behavior RED

Frozen binary task-2-cleanup-red-linux-arm64.test SHA29b77adf10c75de56a10342b3d4441c43e510b7aa400e9976aafac40eaa4663b checked. Newcleanupsource/driver hashcaptured, userunchanged2c5389c. Actualfullpositivecoordinatorattachprocess1/waitcmd0return1/exitedRunningfalseOOMfalse,1370PASS/2FAIL outer+cleanup-fatal/0SKIP. Realpositivehelper m.Run Fatalexit1 intentionallyomitteduserrelease/wait; independentPID1 actuallyobserved orphan25 Sleeping/PPid1/UIDGID1000/allcaps0/NNP1 andfailed. Test-owned registered cleanup heldpidfd and actualSIGKILL+Wait4status9. Realnegativehelper33exit1/noevidence/liveorphan41Sleeping independentlydetected, expectednegativepassed, actualkill+Wait4status9. ExpectedchildGoFAILlinesprefixedclearhelperidentity, notmistakenoutercounts. Actualeachownedrm0/threeinventory0empty. Fullmetadata/raw task-2-root-native-cleanup-red.json/-positive.log. A firstRootdisplay selected remainderuntilEOF andexceededdisplaylimit; corrected boundedactualcleanupfragment retrieval, rawunaltered and no repeatednativeexecution. MeaningfulmissingcleanupRED, no marker/t.Failedfake. Workerreleased toregisterpositive cleanupbeforeFatal.

## Actual Fatal cleanup and synchronous-user audit GREEN

Frozen binary task-2-cleanup-green-linux-arm64.test SHAeb671ddf8434bea8750654ccb3d4f868254a01aedd90edb05947a2e45cbe705e checked. Driver/cleanup+useraudit sourcechanged capturedmetadata, productunchanged. Actualcoordinatorattachprocesswait0/exitedRunningfalseOOMfalse,1372PASS/6top/0FAIL/0SKIP. RealFatalhelper17exit1 but registeredcleanup releaseduser25 and actualwait0/workersjoined marker; independentPID1procread absent. Deliberatenegativehelper33exit1/liveSleepingchild40/noevidence, registeredindependentheldpidfd SIGKILL/Wait4status9. Prefixed childFAIL retainedwithoutfalseouterfailureclassification. SynchronouslockedFS auditTID62 actual1000→0/Validatebeforeunlock anduserisolationsuccess. VmRSS9540KiB/Alloc1502688B/FD7 fixture-only. Eachactualexactrm0/threeinventories0empty. Fullactualcommands/hashes/state/wait/raw task-2-root-native-cleanup-green.json/-positive.log. Workerreleasedtoimmediatesmalltestslicecommitthencanonicalninefreshfixturescriptgatenecessaryfornewscriptintegration, no additionalownrunnerrepeat.

Root read entire canonical candidatebeforeexecution and foundtaggedimage ref vs canonicalRepoDigest comparisonbug (realinspect RepoDigests have no tag). Worker script-only fixedexactrepository@digest, retainedtaggedimageinvocation; nofailedcanonicalrun claimed. Script source.diff istracked-onlyandnotfullnewuntrackedsource; allGo+scriptSHA snapshot is sourceassociation. Inventories filteruniquerunprojectonly toobservewrongfixture leftovers; exactnames+bothlabelsrequiredforremoval. Bash3 emptyarray/set-u catch fromactualshell andsourcefixreportedworker, notnativebehaviorRED. Canonicalactualexecutionpending.

## Canonical native integration gate, two honest script versions

First canonical script SHA73b052aa8a9b5a5218860de198d36dd9a95e9a4072cec0b0db2d89e63afef563 executedactualexit0, source118dbfc/binaryeb671ddf8434bea8750654ccb3d4f868254a01aedd90edb05947a2e45cbe705e. Ninefresh cases actualattach/state/wait0 exceptsecurebits2; fullpositive1372PASS0FAIL0SKIP. Eachcontainer/volumerm individualexit0 (18), uniqueproject82151-1791326048 inventoriesempty. Root independent-check JSON confirmsall9terminalfields/source10Go committedhashes/recorded scriptSHA/currentproject3independentqueries0empty. Afterfreeze release, workeradded explicitcreate --pull=never to closeimageinspect→create race. Root originalendcheckincorrectlycomparedreleasednewscripthash tooldsourceSHA andasserted; correctedusingrecordedpre-run73b+all10gitshowhashes+independentoneflagdiffSha equivalence. No native rerun hid error orreclassifiedoldscriptasnew.

Final canonical frozen script SHAa8333cb3dc777541c29a48d38a961f31ac9018c425022dff1a0135629db83ec6 differsonlythatflag. Actualfinalscript exit0 (session9034), ninefresh fixtures uniqueproject82621-1791326197; source118dbfc/all10Go+scriptSHAverified; binarysameeb671...705e. Actualcaseattach/state/wait exactexpected0/securebits2, eachStatusExited/Runningfalse/OOMfalse, positive1372PASS0FAIL0SKIP; allnecessarymarkerschecked. Script18individualexactnames/twolabelverifiedrmexit0; Rootadditionalcontainer/volume/networkquerieseachactual0empty. Fullscript-owned proof task-2-root-canonical-no-pull/ (created/exitedJSON/results/rawlogs/source+binarySHA/cleanup), independentmetadata task-2-root-canonical-no-pull-independent-check.json/coordinator.log. Explicitnewflag justified onefinalactualscript run; unchangedGo testsnot separatelyrerun outsidecanonical. Oldvariantproof retained task-2-root-canonical/. Actual ownmetadata sums15manual+18canonical=33freshcontainers/33ownedvolumes, allgone with66individualremovalsobserved0; no globalnetwork created.

Workerfullrepo go test ./... actualexit0 duringfreeze, allpackagescached/nonverbose so no pertestskipclaim; vet/build actualexit0/emptylogs. Rootreadfull test log and bothzero-byte diagnostics. Existing productionetcd/journal/Manager/adapters unchanged, previousnativeetcdcertifiedbaseline remainspreviousnotnew. Hostrace1.806s andcrossLinuxpackagevet workerfocusedchecks source118dbfc, actualsupportedCGO0native isseparate. IndependentTask2 gate/finalunitgatepending.


## Complete retained host repository output

`go test ./...`: actualworkerexit0 during source freeze; cached, nonverbose, no all-per-test skip census. `go vet ./...` and `go build ./...`: actualexit0, zero-byte logs.

```text
ok  	github.com/goairix/sandbox/cmd/apparmor-loader	(cached)
ok  	github.com/goairix/sandbox/cmd/redis-bootstrap	(cached)
ok  	github.com/goairix/sandbox/cmd/sandbox	(cached)
ok  	github.com/goairix/sandbox/cmd/workspace-mounter	(cached)
?   	github.com/goairix/sandbox/cmd/workspace-probe	[no test files]
ok  	github.com/goairix/sandbox/internal/api	(cached)
ok  	github.com/goairix/sandbox/internal/api/handler	(cached)
?   	github.com/goairix/sandbox/internal/api/middleware	[no test files]
ok  	github.com/goairix/sandbox/internal/apparmorloader	(cached)
ok  	github.com/goairix/sandbox/internal/config	(cached)
ok  	github.com/goairix/sandbox/internal/fuseprotocol	(cached)
ok  	github.com/goairix/sandbox/internal/imageref	(cached)
ok  	github.com/goairix/sandbox/internal/kubecontract	(cached)
?   	github.com/goairix/sandbox/internal/logger	[no test files]
ok  	github.com/goairix/sandbox/internal/mounter	(cached)
ok  	github.com/goairix/sandbox/internal/redisbootstrap	(cached)
ok  	github.com/goairix/sandbox/internal/runtime	(cached)
ok  	github.com/goairix/sandbox/internal/runtime/controlprotocol	(cached)
ok  	github.com/goairix/sandbox/internal/runtime/controltarget	(cached)
ok  	github.com/goairix/sandbox/internal/runtime/docker	(cached)
ok  	github.com/goairix/sandbox/internal/runtime/kubernetes	(cached)
ok  	github.com/goairix/sandbox/internal/runtime/launcher	(cached)
ok  	github.com/goairix/sandbox/internal/sandbox	(cached)
ok  	github.com/goairix/sandbox/internal/storage	(cached)
ok  	github.com/goairix/sandbox/internal/storage/state	(cached)
ok  	github.com/goairix/sandbox/internal/storage/state/etcd	(cached)
ok  	github.com/goairix/sandbox/internal/storage/state/redis	(cached)
?   	github.com/goairix/sandbox/internal/telemetry	[no test files]
?   	github.com/goairix/sandbox/internal/telemetry/log	[no test files]
ok  	github.com/goairix/sandbox/internal/telemetry/metrics	(cached)
?   	github.com/goairix/sandbox/internal/telemetry/trace	[no test files]
ok  	github.com/goairix/sandbox/internal/workspaceprobe	(cached)
?   	github.com/goairix/sandbox/pkg/types	[no test files]
ok  	github.com/goairix/sandbox/test/integration/api	(cached)
ok  	github.com/goairix/sandbox/test/integration/fuserefillelease	(cached)
ok  	github.com/goairix/sandbox/test/integration/helm	(cached)
ok  	github.com/goairix/sandbox/test/integration/workspacefuse	(cached)
?   	github.com/goairix/sandbox/var/tmp/cce	[no test files]
```

## Latest script focused fault RED and GREEN

```json
{
  "script_sha256": "a8333cb3dc777541c29a48d38a961f31ac9018c425022dff1a0135629db83ec6",
  "results": [
    {
      "scenario": "early",
      "script_exit": -9,
      "watchdog_fired": true,
      "elapsed_seconds": 8.016,
      "checks": {
        "bounded_script_exit1": false,
        "no_docker_wait_on_nonterminal": false,
        "exact_container_and_volume_removed": false,
        "observed_two_rm_exit0": false,
        "observed_three_empty_inventories": false,
        "no_false_success": true
      },
      "passed": false
    },
    {
      "scenario": "timeout",
      "script_exit": -9,
      "watchdog_fired": true,
      "elapsed_seconds": 69.01,
      "checks": {
        "bounded_script_exit1": false,
        "no_docker_wait_on_nonterminal": true,
        "exact_container_and_volume_removed": false,
        "observed_two_rm_exit0": false,
        "observed_three_empty_inventories": false,
        "no_false_success": true,
        "real_60s_deadline_reached": true,
        "container_kill_actually_failed": true,
        "attach_actually_survived_TERM": true,
        "attach_force_terminated_and_reaped": true
      },
      "passed": false
    }
  ],
  "passed": false
}
```

```json
{
  "script_sha256": "2378bcb5128d2ea815fc229c3b2472f44c2acf66c125acdaeae7a8164dd179eb",
  "results": [
    {
      "scenario": "early",
      "script_exit": 1,
      "watchdog_fired": false,
      "elapsed_seconds": 1.571,
      "checks": {
        "bounded_script_exit1": true,
        "no_docker_wait_on_nonterminal": true,
        "exact_container_and_volume_removed": true,
        "observed_two_rm_exit0": true,
        "observed_three_empty_inventories": true,
        "no_false_success": true
      },
      "passed": true
    },
    {
      "scenario": "timeout",
      "script_exit": 1,
      "watchdog_fired": false,
      "elapsed_seconds": 64.071,
      "checks": {
        "bounded_script_exit1": true,
        "no_docker_wait_on_nonterminal": true,
        "exact_container_and_volume_removed": true,
        "observed_two_rm_exit0": true,
        "observed_three_empty_inventories": true,
        "no_false_success": true,
        "real_60s_deadline_reached": true,
        "container_kill_actually_failed": true,
        "attach_actually_survived_TERM": true,
        "attach_force_terminated_and_reaped": true
      },
      "passed": true
    }
  ],
  "passed": true
}
```

## Root complete ledger, historical state preserved

Earlier pending/UNCOMMITTED lines describe that moment; later commits/gates supersede them. Every Ruling is also ordered separately in the final-review companion.

# SDD ledger — plan: docs/superpowers/plans/2026-10-07-launcher-kernel-boundary.md

Setup: continuing user-approved phases1–5/nativeetcd replacement oncodex/etcd-state-management, requestedbasefeat/workspace-fuse-mount b99b823. CurrentBASE7c79d6a. Priorjournal unit fullyclosedsource4dd09c8/script34b9a03/complete5fd2adf; permanentfullreports18orderedRulings+costs/64individualboundaries f9768d6. InitialTasks1/2/3 andONEfinalfix scopes allpassed independent; actualhostfullrace495PASS2rootSKIP0fail38.468s/nativeLinux503PASS0skip0fail terminal+exit0/allrepo realetcd29packages/vet/build/diff0, allownedfixturesgone. OriginalManager stillRedis; overallmigration FARFROMCOMPLETE. Noapprovalpending/no pushmergeproductiondeploy. OnlyunrelatedtrackedSentineledit unchanged/unread/unstaged. DoNOTredispatcholderdoneunits.

Preflight table:
| pair/task | Produces/consumes | Finding |
| 1→2 | actualKernelBoundary/publictypes/actualTestMainfixedmodes→realuserexec/proc/FD/Fatal/script | interfacespublicexactspec; Task1private driverreportedbeforeTask2; nosilenthelpersguess |
| Task1 | P/E/I/B/Aperthread+PID1/rootexecroles/proc→modelRED+actualRootnative | wholethread settersCGO0; rootfixturePID1notchildfake;platformnil/zeroexceptionsexplicit |
| Task2 | nonzeroCredential/grouplesschild/onlystdio/proc→syscall/FD/canceljoin actualproof | no productionrawstart; no parentstdoutselfattestation; canonicalfixtureRootowned |

Ruling: continue necessary reversible kernel specification/plan/implementation under original approved architecture and repeated continue; no new permission handoff for prerequisite artifacts — overall user authorized implementation andsmallcommits, no external action orproductscopecancel — cost if wrong interface/kernelpolicy/integration rework.
Ruling: use CGO0 AllThreadsSyscall and a transient SETPCAP alongside KILL/SETUID/SETGID only during bootstrap, then irreversiblydrop SETPCAP and B/A; parent P/E/Ithree, monitor I/A/Bzero beforeuserexec — boundingdrop requires SETPCAP andperthreadkernelstate needsactualnativeproof; no newongoing privilege orproductionconfigchange — cost if wrong kernel/CRIcompatibility and privilegebootstrap/deploymentreview rework; actualrootexecproof andfutureFUSEtopology remainmandatory.
Ruling: deliver standalone real kernel boundary before full authenticatedtarget/physicalmonitor integration; no publicrawstart/launchcap/ready/terminalauthority andnoimageswap — actualcaps/NNP/proc/FDsemantics mustbeconcrete beforetrustinglauncher processproof — cost if wrong additionalcomponent/integrationcycle; actualdescendantdrain/target/productionwiring/Redisremoval notwaived.
Ruling: Root-owned fresh pinned-local-image CGO0 nativePID1 fixture has fourcaps only/networknone/readonlyroot/binary/testvolume/defaultPIDnamespace/64MiB128pids0.5CPU/GOMAXPROCS2, necessarynegativeinitialNNP/capcases usefreshownedcontainers — realkernelroles cannotbeproved byhostskip/model orordinarysubprocessPID1 claim — cost if wrong testbudget/fixture refinement; nativeisnotLinuxrace ororiginalimageoverhead/fullsandboxisolationproof, no userquota silentlyincreased.

Formal spec/plan selfreview complete72/63lines, exactpublictypes/roles/masks/thread/proc/platform/error bounds, two independentlyrejectabletasks, no placeholders. Bothdocscommittedseparately441dd98/7c79d6a. OldRootfollow-on notes copiedexplicitownedsource intoownnewworkspace beforeoldworkspacecleanup; provisionalnotes notimplementationclaims.

Priorjournal ownedworkspace deleted onlyafterfullpermanentdocs/human-visiblelinks18ordereddecisions+costs/64boundaries andownfollowonnotes copiednewownedworkspace; no siblings/configs/usersources cleanup. Fullcurrentnew planworkspaceidentity firstline correct. Task1fresh /root/launcher_kernel_bootstrap_implementation astrahigh fork none per perthreadirreversible syscall/security/realrootexec judgement; BASE7c79d6a63bcca095aa07a6e34c640349be0371e0. Exactbrief/fullspeccontext/report/template/noagents/noDocker carried. NoRootproductedits whileworkeractive; Task1gatepending.

Ruling: permit a narrowly scoped CGO0 readonly AllThreadsSyscall6(PR_GET_SECUREBITS) uniformzero check as exception to setters-only rule, bothinitialpreflight andValidateCurrent; otherprocessgetterscurrentprctl/proccapsNNPperthread; trustedCGO0Go-runtime-ownedthreads only — securebitsperthreadnotinproc, singlethreadgettercannotcertifyall; actualGo docs enforceuniformreturns andfatalon divergence — cost if wrong processfatal/availability insteadofdiagnosticErr orkernelcompatibilityrefinement, no returnedboundary/falseauthority ondivergence. Rootformal spec/plan/contextclarification doc-only currentlyUNCOMMITTED(noHEADshiftmidworker), notifiedworker beforeLinuximplementation. ActuallocalGo1.25.6 syscall_linux.go1111–1135/runtimeos_linux.go fatalreturn semantics read; no inventedkernelclaim.

Task1 first native freeze Root evidence: HEAD69ab0a1 with 8 source SHA snapshots including 4 untracked Linux files; binary50f98c8911084de60cb7a1fd7aa80eda9d5e336905b4797740d71b635e805ffc. Actual positive1368PASS/4top/0FAIL/0SKIP test+attach+wait0; 5 normal reject modes actual0 and precise nilboundary/no-mutation markers; securebits divergence actual2/runtime fatal/no boundary expected. Root coordinator incorrectly matched generic rejection marker and exited1; original metadata/log preserved unchanged, corrected stored-evidence classification task-1-root-native-corrected-evaluation.json actual exit0 without rerun. Initial navigation summary script KeyError runs vs cases corrected, not product behavior. All7 actual ownedcontainer/volume removals individuallyexit0 and each exact container/volume/network inventory0/empty. VmRSS9532kB Alloc1633224B FDs7 fixture-only, not original-image overhead. Frozen diff file does not include untracked Linux files; full-source proof is source_hashes in metadata, not claimed commit69ab0a1 alone. Worker released to add real seccomp capset and partial boundingdrop cases; no product modification byRoot.

Task1 second native evidence: product524ae7d, only native test changed verified firstfreeze hashes; binaryd8bdd197a61be88c80fce47dbdfa9d97b5f03c1dd97278a33f08cca390c9e1f9. Three fresh reject-setter/partial/thread actual test+attach+wait+coordinator0, actual seccompTSYNC/EPERM/Ie0partial retained/noBoundary/norollback proof. All3 individualrm0/exactinventory0empty; no unchangedpositive/fullreruns. Worker released to commit focusedtestslice/fullreport. Root securebits doc-only cbbfc84 closes prior pending formalclarification. Task1notDONE/gatepending.

Task1 workerDONE all3 own coherentcommits69ab0a1/524ae7d/c2464ea +Rootdoccbbfc84; fullreport readthrough finalhandoff, committed8sources independently equalactualsecondfreeze hashes. Full originalBASE7c79d6a..c2464ea package4commits47737B generated once; independent astrahigh fork-none /root/launcher_kernel_bootstrap_review dispatched spec+quality read-only/noagents/nosuiterepeats; taskgatepending, no prematureTask2implementation. Task2brief/context prepared underown workspace only.

Root named pointer-lifetime risk check whileindependentreviewpending: read actualGo1.25.6 syscall_linux.go1100..1150 and changed kernel_mutate_linux.go; AllThreadsSyscall/6 and runtime dispatcher explicitlygo:uintptrescapes, product passes direct uintptr(unsafe.Pointer(header/data)) thenKeepAlive. No productedit/newtest or duplicatewhole-source review; Task1gate stillindependent.

Root plan marker update first guard incorrectly counted header literal -[ ] as a task box, AssertionError before any write; subsequent git commit had no stagedchange and failed, preserved unrelatedSentinel. Corrected to exactTask1section, no product or evidence change; not behavioralRED.
Ruling: accept Task1 independentgate C0/I0/M0 with explicit dynamic Linuxrace/CGO-linked/vanished-task schedule limits, not completed user or lifecycle proof; runtime CGOENOTSUP/uintptrescapes source check and bounded retry code/parser proof are sufficient for this narrowly supported CGO0 startup task, Task2 carries live user/concurrency/Fatal coverage — no concrete defect or missing Task1 mandatednativecase was identified; these limits are reported individually ratherthan claimedpassed — cost if wrong untested Go/kernel/environment compatibility orrarechurn path requires additionalfocusednativecases/refinement before corresponding deployment.
Task 1: complete — independent astra reviewer /root/launcher_kernel_bootstrap_review SpecPASS/QualityApproved C0I0M0, full1169line/47737B4commitpackage EOF; complete reportreadincluding allCannotVerify. Task2userisolation/canonical explicitlynext; dynamicCGO/Linuxrace/vanishedschedule are honest retainedlimits, no technicalfindingwaived. Runtime contracts namedactualGo1.25.6 sourceverified; all10fixture rawlogs/removals/inventories separatelychecked. Overallmigrationstillincomplete.

Task1gatedoc committed42b5b9d afterfocused diffcheck; Task2 BASE42b5b9daf38c7c6cd4844aaa44b059c520c88e22. Fresh /root/launcher_kernel_user_implementation astrahigh fork-none dispatched brief/context/template/report, realunprivilegedproc/FD/Fataljudgment, noagents/noDockerworker/noexternalactions, smallcommits/sourcefreeze; Task2notcomplete.

Ruling: Task2 test-only live user fd/environ audit may temporarily rawsetfsuid/setfsgid1000 on a locked trusted monitor OS thread, with pre-registered restoration, actualTIDproc fourthUID/GID verification entering/leaving, no childexecduringaudit, complete restoration+ValidateCurrent beforeunlock; failure retirelockedthread/helperfails ratherthancontinue. Productallthreadsetter/ongoingrootroles remain unchanged; sync.Once test-onlysingle actualbootstrap shared acrosspositive tests allowed ifallTask1initial/final/repeat/freshthread assertionspreserved — rootmonitorongoingthreecaps cannotread differentUID fd/environ with PTRACE_READ_FSCREDS; addingSYS_PTRACE forbidden andchildselfreport notindependentevidence. Primaryman7proc_pid_fd/environ/ptrace/setfsuid read; setfsuid silentfailure requireactualstateverification, LSM successmustactualnativeprove — cost ifwrong audit/proc/kernelcompatibility refactoring, restorefailure fataltestavailability; no productionfscredentialstrategy orpermissionexpansion. FreshimageinspectactualLinuxarm64 exactdigestnoPull suppliedworker.

Task2 actual Linux behaviorRED recorded Rootbinary8197daa8dedc2f0979264b18ff33f0109174014191b1b48e6220fdd046ccdf49 with metadata fullsourcehashes(untrackedtest/changeddriver honest). Positive fullpackage1368PASS/1FAIL(TestKernelUserIsolation)/0SKIP, actualcoordinatorattachprocess1/waitcmd0value1/exitedfalseOOMfalse. TrustedparentactualuserTID34 fourUID/GID0/all5caps0/NNP1 vs required1000, genuine omittedCredential behaviorfail, nothelper/compile/oracleerror. Actualindividualrm0/threeinventory0empty. WorkerreleasedtoGREEN. Roottest-onlyaudit clarification f57b39f formal2lines; no productedit.

Task2 first GREEN actualfailedFDoracle: binary71261f548a33da341aea7e19ab21e538400866711eaa63e94645afe0670b0201/native1368PASS1FAIL0SKIP/coordinatorattachprocess1/wait0value1, allownedrm0inventoriesempty. Realuser4UIDGID1000/5caps0/NNP1/7threads passed; fs-audittemp1000/restore0/environmentexactpassed; userruntimecpu.maxFD3 misidentifiedinheritneedsactualsourceprovenance diagnosis. Rootappliedsystematicdebugging/sourceactualGoOpenCPU O_RDONLYCLOEXEC/initbeforeenvread, workerinformed no genericallow/suppression. Originalfailurepreserved, no productsourceedit.

Task2 user GREEN actual1369PASS5top0FAIL0SKIP, binary64b2a6378381beed3be8a6265fa9bb00f957c93ae33fcd3bfdde1aa9d2a4effb/coordinatorattachprocesswait0/exitedRunningfalseOOMfalse. Directuser7threadsUIDGID1000/5caps0/NNP1; managementpreexecallCLOEXEC/actualfs-audit1000→0/envexact/runtimeCPUFDactualflags+context/syscallprocsecretrefusals/user+monitorwait0 confirmed. Exactindividualrm0/threeinventory0empty. PriorGREEN1failurepreserved. Workerreleasedtosmallcoherentuser-slicecommit, Fatal/scriptTask2pending; no taskcompletionclaim.

Task2 coherentuser/shareddriver slice immediatelycommitted2c5389c afteractualGREEN2+hostrace/Linuxvet/selfreview; Root gitshow all9committedGo sourcehashes independentlymatchactualGREEN2metadata, no relianceworkingtree midnextedit. Canonicalscriptuntrackedexcluded; FatalRED next, Task2gatepending.

Ruling: canonicalnative script uses established Root labelkeys sandbox.test.project=<freshuniquerunprefix> and sandbox.test.fixture=launcher-kernel-boundary, exactknowncontainer/volnameswithbothlabelsverified anduniqueproject-onlyinventory;mode resourcesmayreusesamerunprojectwithuniquemodenames — consistentindependentfixtureidentity plus no cross-runconstantprojectcleanup, no networkresourcecreated — cost ifwrong coordinator/scriptlocalcompatibilityorcleanupvisibilityrefinement, neverdeleteforeignidentity/no deploymentchange.
Task2 actualFatalRED binary29b77adf10c75de56a10342b3d4441c43e510b7aa400e9976aafac40eaa4663b: native1370PASS2FAILouter+positiveCleanup0SKIP/coordinatorattachprocess1/wait0value1/exitedRunningfalseOOMfalse. ActualSleepingorphans25/41 PPid1 UIDGID1000/allcaps0/NNP1; independentheldpidfd kill+Wait4status9 afterpositiveomissionassertionfail anddeliberatenegativepassed; everyownrm0/threeinventoryempty. Originalfullrawsaved, firstRootviewoverlargecorrectedboundedfragment(nonbehaviorerror). WorkerreleasedGREEN, synchronouslypinnedFSauditorfinebutnewusersourceactualfullpositivegatemustrefresh.

Task2 actualcleanup/syncauditGREEN1372PASS6top0FAIL0SKIP binaryeb671ddf8434bea8750654ccb3d4f868254a01aedd90edb05947a2e45cbe705e/coordinatorattachprocesswait0/exitedRunningfalseOOMfalse, helper17Fatal1/user25actualwait0+joined+independentabsent; deliberatehelper33Fatal1/user40actualSleeping/noevidence→pidfdkill/Wait4status9. fsAudit62actual1000→0/userisolationpassed. Allownrm0/3inventoryempty; workerreleasedcommitthencanonicalninefixtureactualscriptgatepending. RootreadonlyentirecandidatefoundcanonicalRepoDigesttagmismatch beforeexecution, workerfixedscript-only/nofakebehaviorRED; trackedsource.diff limitation explicitallsourceSHA proof.

Task2 cleanup/syncaudit immediatelycommitted118dbfc; Root all10 gitshowsourcehashesexactactualGREENsnapshot. Canonicalold73bSHA actualexit0/9freshcases1372PASS0FAIL0SKIP/securebits2/other0,18individualrm0 +3independentinventory0empty. Afterrelease scriptaddedexplicit--pullnever only; Rootverification mistakenlyusedreleasedcurrentscript vsoldhash, assertioncorrectedviarecordedpre-run73b+gitshowGo+exactoneflaghash comparison; nooldproofrewritten/no rerunhiderror. Finala833SHA canonical actualexit0/session9034/9freshsamecounts/outcomes, source118dbfc/binaryeb671same/all10Go+scriptSHAverified;18individualrm0 +Rootadditional3queries0empty/project82621-1791326197. Workerreleasedtoscriptsmallcommit/fullreportDONE; Task2gatepending. Rootfullworkerrepologreadcachedall/nonverbose honestvetbuild0; no newnativeetcdresultclaimed.

Task2 implementer DONE full final report read; commits2c5389c/118dbfc/7ba4d27 and binding Root docs f57b39f/bb3d424. OriginalBASE42b5b9d..7ba4d27 package5commits/43898bytes; fresh independent review dispatch prepared. All33containers+33volumes/66individualrm0 and own inventoryzero, no live fixtures. Task2gate pending.

Ruling: Final review reuses the certified unchanged previous migration baseline recorded in runtime-target-journal-final-review.md and verification.md (completion5fd2adf), reads the entire NEW kernel originalBASE7c79d6a..finalHEAD delta through EOF once, and checks named current integration seams (launcher production imports/callers, protocol/journal ownership/authority separation, legacy image/FUSE topology and unchanged Manager/config wiring). It does not reopen deleted predecessor workspaces or claim a fresh full old-branch EOF review — previous baseline was already independently gated and this unit adds an isolated package, native tests, one test script and docs; a fresh old1.9MB crawl adds cost without a concrete changed risk — cost if wrong a pre-existing missed defect could remain unseen until affected integration is changed; freshness is explicitly limited and subsequent migration wiring requires its own tests/review.
Ruling: Final verification retains the exact-source final host launcher race/package-vet, actual canonical CGO0 Linux all-nine-mode run, and full repository test/vet/build from the frozen118dbfc source; Root independently checks recorded output, commit/file hashes, final executable script and diff. No unchanged suite is rerun merely for a controller label. The final script-only --pull=never change already received its own complete actual nine-fixture refresh, syntax and diff checks — developer requires avoiding redundant checks after passing without new changes/failures/concerns, while the plan's controller gate is satisfied through Root-owned native execution and Root-audited full same-source reports — cost if wrong host environmental drift/cached-test coverage is not freshly executed; no current native etcd, Linux race, all-package per-test skip census, or original-image/fleet claims follow from these results.

Task2 fresh independent reviewer /root/launcher_kernel_user_review astrahigh fork-none dispatched full original range+binding context+Root proof+full worker report; no subagents/no repeated suites. Root readonly current fullrepo logs:30 cached passing packages/8 no-test-file packages, vet/build0bytes with prior actualexit0 evidence. Current kernel delta names only13 expectedfiles; production launcher import/caller search finds definitions only, no current productioncall. Final Rulings9/10 above saved before final dispatch.

Task2 independent review originalBASE42b5b9d..7ba4d27 full1082lineEOF SpecverifiablePASS/QualityNeedsFixes C0I1M0; full original report read. I1 early attach CLI failure bypasses deadline via unbounded docker wait before terminal rejection; reviewer appending related timeout TERM/unbounded local wait risk under same finding. Task2 remains unaccepted, fixround1 pending fullfinalreport. Nine CannotVerify retained: Task1underlying source (alreadyindependent gate+finalnewdelta), Linuxrace/CGO and precisechurn (Ruling6), originalimage/fleet/productiontarget/descendantdrain/escapeFUSE/fullmigration (futuremandatorynotclaimed), finalnewdelta+permanentevidence (controllerpending). No finding waived.

Task2 finalreview I1 amendment fullread: both earlyattach nonterminal-before-wait and TERMsurvival/unboundedlocalwait failurebranches. Fixround1/5 resumedoriginal /root/launcher_kernel_user_implementation (active confirmed), BASE7ba4d27, ONEgroupedI1/focusedfakeDockerREDGREEN/no unchangedGo suite repeats/no workerDocker/nativefreezeRoot. No extra finding orauthority requirement invented.

Task2 fixround1 workerDONE6b7c58b sourceSHA2378/focusedfakeDockeractualRED8.016s69.010s forced-9→GREEN1.571s64.071s naturalexit1/no watchdog, fullappendedreportread. Mockresource removals/inventories notnativecount. Root all9prior actualstate/outcomes newunchangedprecheck satisfied, committedscriptSHAmatchesGREEN; recorded JSON. No newnativeclaim/repeatGo/fullrepo, priora833scriptproof not relabeled. Scopedreviewpackage7ba..6b7 5375B/1commitready.

Ruling: For script-only I1 error-path fix6b7c58b, use focused actual Bash fault-injection RED→GREEN for both newly bounded branches and independently evaluate all nine retained real native attach/terminal/wait observations against the identical moved precheck; retain native proof under original a833 script SHA rather than rerun unchanged Go fixtures or relabel it as latest2378. The Docker create/security/capability/positive oracle modes and all ten Go sources are unchanged — developer bars redundant checks without a new uncovered concern; focused execution proves the changed failure branches and stored actual successful outcomes cover unchanged conditions — cost if wrong latestscript daemon/runtime integration could differ beyond the covered ordering/termination fault cases, requiring a newly justified focusednative run. No general Docker-daemon health/OS uninterruptible-kill guarantee or latest-script native-run claim is made.
Task2 fixround1 fresh reviewer /root/launcher_kernel_user_fix_review gpt-6.1-sol high fork-none dispatched exactI1+5375Bscopeddiff+fullfixreport+actualproof/newprecheckcheck; no subagents/repeats, Task2gate pending.

Ruling: Separate the plan's final controller review/evidence-retention checkbox from Task2's implementation checkboxes, preserving the exact requirement and leaving it unchecked until the actual final gate. Mark only the four delivered Task2 implementation checks after scoped acceptance, then perform the once-per-unit final review — original Task2's fifth checkbox included the final review itself, creating circular task-completion bookkeeping; the spec still mandates every final check and the requirement is not waived — cost if wrong plan/report tracking needs clarification, no product or test behavior changes and no premature final/migration completion claim.

Task2 fixround1/5: I1bothbranches ADDRESSED,0open; commit7ba4d27..6b7c58b/scopedreview /root/launcher_kernel_user_fix_review C0I0M0,out-of-scope0. Fullscopedreportreadincludingrawmockfaultattribution/sourceSHAandno newnativeclaim. AllnineCannotVerify explicitlyresolved as narrow retainedsupport/futuremandatory/currentcontroller final obligations, no realgap orfindingwaived. Task 2: complete (commits42b5b9d..6b7c58b, originalreviewI1fixed/scopedreviewclean). Exactfourimplementationcheckboxescomplete; finalcontrollercheckbox relocated unchangedtext perRuling12 and remainsunchecked untilfinalreview/evidence done.

Task2gatedoc aeee91a committedfocusedcheck; finalunit originalBASE7c79d6a..aeee91a reviewpackage12commits96410B ready. Fullfinalcontextbinding/spec/plan/proof/source/support/dependency/unchangedbaseline reuse and each64inheritedreference explicit; finalcontrollercheckbox pending.

Final independent /root/launcher_kernel_unit_final_review astrahigh DONE SpecPASS/QualityApproved C0I0M0; complete168line36083Breport read in two bounded ranges throughEOF, including every64inherited+28Klimit and no parked/minor. Fullnew2229line96410B12commitEOF plus namedcurrentjournal/protocol/Manager/config/adapters/images/mounter/actualGoruntime checks; native33+33/66rm0/allinventories verified; latestGo exacthash/nativebinary checked; source2378 vsactuala833 distinction retained. Reviewer ownmetadata selector Dockercontainer-rm auditAssertionError correctedstoredrecords only and navigationquerytruncation/filenameexit2 disclosed; no native/suite repeated. No final fix wave necessary (zero findings).
Ruling: K12 retains unforced fsID-restore-failure/dirty-thread-retirement execution as a test-only dynamic coverage limit, with actual1000→0 success plus source-verified pinned thread/error/helper-exit behavior sufficient for current helper scope — no production fsID strategy uses this path and no concrete defect was identified; do not claim the error branch ran — cost if wrong additional focused kernel/LSM fault coverage and audit-helper retirement refinement when the supported audit environment changes.
Ruling: K13 retains the narrow exact cgroup-v2/runtime-descriptor oracle, failing unknown layouts rather than broadening allowed FDs — actual Go startup source and parent CLOEXEC/livechild flags establish the tested fixture only, not arbitrary runtime or cgroup-v1 support — cost if wrong fixture portability/refined descriptor provenance tests; production dynamicFD support remains separate mandatory work.
Ruling: K26 certifies only actual installed Go1.25.6/CGO0/Linuxarm64 execution plus stated host/static checks, retaining other toolchain/kernel/LSM/architecture runtime combinations as uncertified — no current code/support finding was identified and no broader actual execution occurred — cost if wrong platform compatibility regression requires affected native checks before claiming wider deployment support, never a silent singlethread fallback.
