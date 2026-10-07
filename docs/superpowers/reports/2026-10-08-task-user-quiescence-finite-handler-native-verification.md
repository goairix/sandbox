# Finite-handler fix — actual changed-seam native verification

Status: **the one new protected-PID1 native gate passed with complete cleanup**. Original whole-unit Important finding I1 still requires its scoped re-review before unit acceptance. Whole etcd production migration remains incomplete. This report adds changed-seam evidence; it does not relabel or replay the three previously accepted Task5 production metadata-chain cases.

Frozen runtime/test commit: `8f455a0c74ba7b809a51abb0f093e613ecb3efbe`, original fix BASE `a7ebee70ae273e2cf473af5a7c6157665745006c`. Root consumed the sole author's complete report, meaningful behavioral RED/GREEN, complete raw classification for both affected-package race checks, vet/ordinary Linux compile outputs and source-freeze/commit correction append before launch. The verified runtime change gives pending persistence to the one registered original coordinator while finite callers stop waiting under their own context. Named Supervisor and finite journal signing entries refuse contention without detached request workers. Contention refusal reduces availability and never grants a replacement attempt.

## Actual fixture and outcome

Project `task-quiesce-local-c7f597fcfa20`; exact selector `^TestTaskQuiesceNativeFiniteBorrowerPending$`, ordinary package `internal/runtime/controlrunner`. Actual result: **1 top-level PASS, 0 subtests, 0 FAIL, 0 SKIP, 5.04s**, target exit0/non-OOM, empty native stderr. All35 recorded commands exited0 without timeout. Root independently checked all72 recorded stdout/stderr/stdin stream lengths and SHA256 values, immutable Git archive,123 actual repository compilation inputs, module inputs,3 Go tool hashes,3 daemon-custodied ELFs, actual target/attestation/config/security and empty final containers/volumes/networks. Root audit exits0.

Same previously approved local component topology: Linux arm64 CGO0 actual PID1 fixture test ELF, original launcher/monitor and fixed UID1000 user ELF; no etcd members in this component, no new production debug hook or target auxiliary process. Original64MiB/.5CPU/PID128/netnone/readonly/NNP/default security, KILL/SETUID/SETGID/SETPCAP bootstrap capabilities only, private protected journal tmpfs and readonly root-owned payload remain. Build uses fixed local Go1.25.6 with GOENV/GOWORK off, GOTOOLCHAIN local, empty GOFLAGS, GOMAXPROCS2 and GOPROXY off; no installation, image pull or native race claim.

| Observation | Actual assertion and limit |
| --- | --- |
| Pending persistence outlives finite caller | A test-only Context.Err pause holds the actual original Journal.Lookup/journal mutex beyond the original server5s connection deadline. Real Unix mTLS quiesce registers USERS closure and the sole coordinator before waiting. The caller receives no receipt; its actual transport borrower count reaches0 by original expiry plus the bounded1s observation tolerance, while the original journal owner is still held. This is actual handler release, beyond a socket-only error. Serve's unchanged defer ordering removes the connection slot before borrower release. |
| No late caller response | Armed real connection wrapper counts0 post-handshake application writes from that expired handler. No late ACK/refusal is emitted. |
| Contention refusals | Two later connection handshakes fail promptly while the pending coordinator holds the admission mutex. They are not authenticated query envelopes delivered during that pause. Separate host genuine mTLS already-handshaken quiesce/query/exec-query/CloseData-query lock regressions cover the later entry seams. |
| Original coordinator survives | Release only the original Lookup. The same captured original attempt/execution and timer continue; deadline-started remains31s, with t0 before the original caller expiry. No resend, second coordinator, new epoch or renewed deadline occurs. |
| Actual user completion | One original live UID1000 hold execution contributes registered1/never0/local-terminal1. Actual PID1 original namespace census and nonreaping ECHILD are revalidated, authentic terminal emitted. Original coordinator terminal elapsed5.019264294s. |
| Fresh original history | Genuine fresh mTLS duplicate and query return verified terminal receipts from the same attempt/timer; receipt counts remain1/1. |
| Shutdown lifecycle | Concurrent/repeated Close, Serve return and original coordinator/timer/execution/Wait/watchdog channels are joined. Journal reports closed and every key byte is cleared. Final original elapsed5.025490627s is below31s. Close occurs after terminal, not during stalled pending I/O. |

Both markers occur after their actual assertions: `QUIESCE_FINITE_PENDING_BORROWER_RELEASED` and `QUIESCE_FINITE_ORIGINAL_COORDINATOR_JOINED`. No WARN/ERROR/FATAL/PANIC JSON output was observed in this native test. No isolation70 is expected or counted as success.

## Immutable evidence and processing corrections

Raw artifacts remain under current OWN `root-fixture-evidence/task-quiesce-local-c7f597fcfa20`: `records.json`, `result.json`, source manifest/archive, contracts/target before/final, daemon payload custody, cleanup inventories and `root-final-unit-fix1-audit.json`. Native record24 stdout SHA256 `184c69743d3540d20913abd90183313b12b7a54507ab3853f153ccfff0cb3cea`; source manifest SHA256 `aa0647a03f2720223a03a0cc444bf61924af7dc68e957a302490ac9da8a42f5d`; archive SHA256 `ba26792c01dfb02979ede7cb7ca34eea72cd62f243486eea108bb2196bb4db8a`. Actual target UID `c62d1e22857eace4f7204ba5a1124b6973f4418b5fac6ece0d9062430c769305`, security contract digest `bc470f632b2b34884cb13804573af29de29e84f8bc1ef5fc3359d01824e92198`.

| ELF | Actual bytes | SHA256 |
| --- | ---: | --- |
| runner.test | 10454810 | `9ff5de181d6a28c9993657a70edef6d99dbd6d8682c0d51a7dbcc95499bc1385` |
| sandbox-launcher | 8231335 | `61c32a384f5065fffcc45b1e55acaa5b3e2f52117277b6c52f8a352f29f238df` |
| user | 3827230 | `2e7583265b62bbd1d85d997b4d75524a1d15af552ef38f52895a47ef6012594a` |

Two failed read-only auditor assertions preceded corrected audit0; neither changed product/test/fixture inputs or caused native replay. Actual daemon HostConfig readback differs only in default `OomKillDisable:false→null`, independently checked as unset/false on both sides; every other HostConfig field and mounts match, and original resource/security contract fields are exact. Do not claim byte-identical HostConfig. Actual daemon attestation is compact1323-byte JSON while the local public artifact is pretty1353-byte JSON; parsed objects and contract hash match, daemon bytes match their recorded custody digest, and every ELF's local/daemon bytes match exactly. Do not claim identical serialization for those attestation copies.

The author's mistaken scratch-report-only commit `f59d25a` was removed by guarded mixed reset under shared commit lock after verifying its sole OWN report path and source8f455a0 parent. The ignored report/raw evidence and all13 frozen source hashes remained intact; current delivered source commit is8f455a0. Creation/correction history is retained in the sole author report and raw logs; no hard reset or unrelated file edit occurred.

## Acceptance boundaries

Host race raw2170 lines contain124 top-level/1069 nested-inclusive PASS lines,0 FAIL and3 explicit host SKIPs (unsupported native terminal-before-ACK and two UID0 ownership subtests). Host race and Linux CGO0 physical execution are different evidence. This changed-seam controlled Context.Err/journal contention is not stalled-fsync or uninterruptible-OS preemption, nor every negative/native concurrency branch. The actual native component cannot alone certify the earlier production Backend/etcd chain at this new freeze; that earlier evidence remains separately attributed and source composition is for scoped review.

Generated wrappers/stdlib/module cache are not independently supply-chain certified. Fleet/existing-N memory, idle costs, performance/capacity, full journal/power loss, fresh-Boot deployment recovery, final sync/flush, FUSE/remote settlement, business End, exact termination, safe owner release, partition scheduling/collector, production cutover and Redis removal remain outside this gate and mandatory downstream obligations. Do not stop the migration heartbeat or mark production complete on this result.
