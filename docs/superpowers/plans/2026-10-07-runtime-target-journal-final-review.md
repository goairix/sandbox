# Protected runtime target journal final review and decisions

This intermediate unit provides protected closed-gate and immutable unknown-exec diagnostics. Production Manager and runtime adapters still use the previous paths; the complete native etcd replacement, authenticated target, real PID1, dual drain, scheduler/tasks/collector/safe GC/native configuration and Redis removal remain required. No push, merge or deployment occurred.

Frozen Go source4dd09c8; initial independent final review7642fc1. One final script-only improvement34b9a03 adds explicit exited/Runningfalse proof. Actual final host race495PASS/0FAIL/2nonrootownershipSKIP, actual canonical Linux503PASS/0FAIL/0SKIP, realetcd fullrepository29passingpackages, fullvet/build/scopeddiff pass; all exact-owned fixtures observed empty. Scoped final fix review confirms T3-M1 ADDRESSED and no new breakage; all current-unit findings are closed. This does not establish production migration completion.

## Rulings I made, in order, including costs if wrong

The following is the exhaustive ordered list from this unit's own ledger. Earlier 1–52 inherited boundaries and their earlier ordered decisions/costs remain in [exec-effect final review](2026-10-07-etcd-exec-effect-final-review.md); all64 individual boundary lines are preserved in the complete current independent review below. Mandatory later is retained authorized work, not a waiver.

1. Ruling: continue necessary reversible targetjournal artifacts under original approved overall architecture and repeated continue instruction without new artifact permission handoff — no external action/new productscope — cost if wrong interface/schema/futureconsumer rework.

2. Ruling: deliver passive closed/unknown protectedstorage before authenticated target/PID1 consumer, no public set-open/accepted/terminal/GC or launchcap — durable evidence is needed but cannot invent physical proof from file/signature — cost if wrong an additional storage protocol refinement/phase integration cycle; overall migration remains mandatory.

3. Ruling: verify realLinux root ownership using network-none alreadypinned/localtestimage + readonlyCGO0testbinary + ownedtemporaryvolume, separately from macOShostrace — actualwrongUID tests cannot be replaced by mockstat or a skippedrootcase — cost if wrong Linuxfixture/build tooling refinement; notLinuxrace/powerloss/productionisolation certification.

4. Ruling: bound logicalcontent256MiB70/85%,contentfiles65536,page128 and explicitlypoison unknownpersist, preserving evidence — resource/error boundaries prevent unlimited unknown retention or falseack, OSfsyncnonpreemptible and filesystemblocks separate — cost if wrong threshold/budget/availability tuning and future safeGC/recovery work; no deletingunknown to make progress.

5. Ruling: encodeGateManifest accepts only closed while Validate/decode retain legal historicalopen — worker identified producer/decoder ambiguity; first public writers are closed-only and Task2 historical-open fixture is explicitoldtypedJSON, no privateencoding shortcut for activation — cost if wrong future legitimate reopen requires a separately reviewed encoder/producer refinement; no current physical authority.

6. Ruling: final journal review reads complete new unit delta8a71e73..finalHEAD and checks named affected unchanged integrations with full branch history/stat/package context; reuses previously certified b99..397880b oldbranch review rather than pretending fresh EOF of all oldcode — journal adds newpackage/newscript only, old sourceunchanged and prior final gates/permanent evidence available — cost if wrong an overlooked cross-unit interaction requires broader oldbaseline re-review and integration rework; oldbaseline claims never relabeled current freshwholebranchEOF.

7. Ruling: final boundary 53: retain management UID/root as trusted writer boundary; deployment must isolate it from every user UID — mutex/flock/POSIX modes do not defend a malicious same-identity writer — cost if wrong credential/IPC isolation or stronger kernel boundary rework.

8. Ruling: final boundary 54: retain trusted canonical parent and supported local filesystem/ACL/mount provisioning contract — FD no-follow/owner/mode checks alone do not certify arbitrary deployment ACLs or mount semantics — cost if wrong filesystem policy screening and supported-environment refinement.

9. Ruling: final boundary 55: certify process hard-exit and syscall-boundary fsync contract only; no power-loss/hardware certification — actual native tests do not simulate host/power failure or every instruction — cost if wrong additional crash/durability recovery validation and possible storage-protocol rework.

10. Ruling: final boundary 56: retain nonpreemptible OS IO/mutex waiting; errors after uncertain mutation poison and never acknowledge success — context cannot interrupt every kernel fsync and waiting mutex has no universal promptness guarantee — cost if wrong latency/backpressure scheduling refinement while preserving evidence.

11. Ruling: final boundary 57: require later measured inode/block/PID1 process/FD overhead under original user quota — logical bytes exclude physical metadata and journal cap is not preallocated — cost if wrong resource budgeting/launcher topology tuning; no silent quota increase.

12. Ruling: final boundary 58: retain fail-closed missing manifest/incomplete temp/identity change behavior without automatic repair — history cannot safely attest fresh identity or revive lost authority — cost if wrong explicit fenced recovery tooling and reduced automatic availability.

13. Ruling: final boundary 59: retain possible close/reopen failure at hard byte/file limits or disk errors; never delete unknown for room — durable gate replacement needs actual temporary capacity — cost if wrong capacity headroom/reservation and operational recovery refinement.

14. Ruling: final boundary 60: keep host race distinct from Linux CGO0 native and unsupported-constructor rejection — no Linux race or unsupported execution was performed — cost if wrong additional platform/race certification before a wider support promise.

15. Ruling: final boundary 61: retain complete temps as passive evidence only; future target/recovery must inspect uncertain/differing attempts before physical decisions — cold scan counts/validates temps but Lookup/immutable retry uses final command path only — cost if wrong additional unknown-intent reconciliation and acceptance protocol; final-file absence never authorizes replay.

16. Ruling: final boundary 62: retain caller initial JournalIdentity/path as trusted configuration, not attested runtime activation — constructor validates shape/current UID/existing-target refusal only — cost if wrong actual independent birth/BootID/certificate install/activation protocol.

17. Ruling: final boundary 63: retain exact-label best-effort cleanup plus independent inventory verification for actual runs — script suppresses removal exits; zero inventory is current evidence rather than universal cleanup success — cost if wrong fixture error reporting/recovery refinement; never assume cleanup on failed inventory.

18. Ruling: final boundary 64: retain 0/1000 warm-point IO equality as measured synthetic shape, not production fleet SLO — cold own-history scan/bucket creation/EINTR and actual processes have separate costs — cost if wrong real metadata/runtime/churn/QPS/p99/capacity measurement and possible performance tuning.

## Complete independent full-unit review

# Protected runtime target journal — final architecture and integration review

Frozen review range: `8a71e73a280ccb945364f0b397b32ce94c1c9cef..7642fc17bb3e9df23dad6453272859b8ff5e02b3`. Product source is `4dd09c8f0c539a5729972ad498a6445a1b4161fa`; later HEAD changes are documentation only. Independently checked current HEAD. Applied the requested `requesting-code-review/code-reviewer.md` method. This report is the only file written; no source, index, HEAD, branch, fixture, user configuration, credential, unrelated Sentinel document or sibling SDD workspace mutation/access, and no subagents or test reruns.

## Requirements and quality

**Requirements: PASS for this protected passive journal unit. Quality: Approved. Findings: Critical 0, Important 0, Minor 1 retained (T3-M1).** No new blocking product, integration, test or current-unit plan defect was found. Task 2 I1 remains fixed; Task 2 M1 is independently adjudicated CLOSED below. T3-M1 is the existing optional script completion-evidence improvement, not a new duplicate finding.

The current full specification and task plan are satisfied by the new strict wire types, actual protected filesystem implementation, closed-only public gate persistence, immutable diagnostic record/query APIs, bounded accounting and verified fixtures. The final controller checkbox is appropriately pending this review/disposition; it is not evidence that production phases 1–5 are complete. The unit introduces a new package and script, with no change to existing production integration or dependencies.

## Strengths and exact source evidence

- Strict, transactional wire boundary: `internal/runtime/controltarget/journal_codec.go:100` checks the byte bound, UTF-8, Unicode escape validity and exact nested schema before typed decoding. Missing/null/duplicate/case-substituted/unknown fields and trailing input fail; `:64` and `:84` assign the temporary decoded value only after semantic validation. The producer at `:44` rejects open while the validator at `journal_validation.go:23` allows historical open for safe closing. `journal_validation.go:33` restricts records to unknown and preserves the original context/time window constraints. Tests derive their mutated fields from typed fixtures rather than the production schema (`journal_codec_test.go:114`).
- Actual directory protection: `journal_files_unix.go:21` traverses parent components through no-follow directory descriptors, rejects noncanonical paths and verifies the immediate management-owned parent. `:60` checks owner, type, exact permissions and single-link regular files; `:115` uses O_NOFOLLOW/CLOEXEC/NONBLOCK, avoiding FIFO blocking. `:136` checks size before a bounded read and rejects size changes. These are real FD checks, not the lexical ScopedFS abstraction. The root substitution test (`journal_files_test.go:221`) verifies that an already-open journal continues using the pinned original directory.
- Actual lifetime lock and durable close: `journal_files_unix.go:197` obtains flock before existing-store validation/persistence; constructor errors close owned descriptors. `:176` syncs each new directory and its parent; `:302` performs exclusive temp creation, complete write, file fsync, same-directory rename and directory fsync. `:356` publishes the closed in-memory gate only after persistence succeeds. `journal.go:88` marks uncertainty poisoned and accounting unknown; later writes are denied by `:69`. Errors preserve possibly committed bytes. `CloseGate` does not advance epoch or grant launch authority.
- Bounded cold recovery and fixed point APIs: `journal_accounting.go:16` reads pages of 128; `:62` validates root entries, then commands entries, then fixed bucket names without nesting retained parent pages or caching the history. Every record/temp is decoded and bound. `journal_files_unix.go:385` reads only the UUID-selected bucket/file; `:423` performs immutable absence checking and projected capacity checks. Counters update after successful persistence, not after uncertain writes. The 70/85 percent policy, hard byte limit and reserved gate-temp file slot are distinct (`journal.go:124`, `journal_files_unix.go:367`, `:444`).
- Opaque evidence remains diagnostic: `journal_exec.go:27` consumes existing verified evidence, checks its 4096-byte canonical wire/digest/getters, and copies only context, digests and time. `:67` checks the current journal binding, compares every canonical record byte for retries, and only then calls the new-record helper. No current-time/provider/Lease or launcher API is introduced. The full ticket/signature/argv/env/stdin/stdout is not stored. Historical expiration does not prevent a diagnostic read or imply authority.
- Lifecycle and failure tests use real operations: `journal_gate_test.go:202` and `journal_exec_fault_test.go:21` inject before/after synchronous syscall boundaries; `journal_gate_test.go:467` covers the directory-sync chain. The new record worker is released/cancelled/joined before hook restoration (`journal_exec_fault_test.go:185`), concurrent observers are joined (`:222`), and the Fatal subprocess oracle uses independent cleanup success plus an actual missing-evidence negative case (`:275`, `:299`). No product background worker, idle timer, watch, connection or record cache is added.

## Issues

### Critical (Must Fix)

None.

### Important (Should Fix)

None.

### Minor (Nice to Have)

**T3-M1 — preserve existing optional terminal-state assertion.** `scripts/test-runtime-target-journal.sh:80` reads only `.State.ExitCode` after `docker start --attach`. A created/running container can expose a default zero ExitCode, so the script's completed-test interpretation depends on successful attach waiting for termination. The branch at `:89` rejects a nonzero attach result even when inspected ExitCode is zero; this prevents the ordinary attach-error/default-zero false success. The actual reviewed run has final test PASS, actual/attach/script exit 0 and independent empty cleanup inventories. No false pass was observed. Require an explicitly exited state/Running=false before accepting ExitCode, or use an explicit container wait, to make the repeatable script's completion evidence self-contained. This is a robustness improvement, not a reason to repeat the full accepted native suite or block passive-unit continuation.

## Previously raised findings

- **Task 2 I1: CLOSED.** The final `journal_gate_test.go:434` independent `cleanupOK` is cleared by each close/poison/evidence/reopen/reopened-close failure; `:459` prints success only if all succeeded. The parent at `:375` requires opposite marker expectations for complete and real missing-retained-evidence cases. Read the scoped fix review and independently inspected the stored RED: it contains `cleanup success marker=true want=false`, actual removal, and the ignored evidence diagnostic. GREEN and race each have six PASS nodes, zero FAIL/SKIP. This validates the formerly missing non-join postcondition oracle; it is not merely acceptance of the intentional child's exit 1.
- **Task 2 M1: CLOSED, actual proof established.** `journal_exec_fault_test.go:129` first calls public RecordUnknown with real verifier-produced evidence, saves the complete record, full command/gate bytes and full Status, closes the parent, then runs the actual child opener. `journal_gate_test.go:162` bounds and joins that child and checks exit 23; `:182` opens the journal and reaches `os.Exit(23)` without Close. The parent cold-opens again, compares the complete returned record, bytes and Status, verifies idempotent retry and rejects a newly authenticated extended window. Host full race log line 779 and Linux native log line 781 independently record unknown 1095 B + closed gate 361 B = LogicalBytes 1456, Records 1, TemporaryFiles 0 and known accounting. This directly supplies the previously absent retained-unknown process-restart proof. It does not claim the child originally wrote the record, power-loss durability, a crash at every IO instruction, or command replay.

## Named unchanged integration checks

1. **Opaque evidence compatibility and ownership.** Read complete `controlprotocol/exec_start_types.go` and `exec_start.go`, the shape helpers in `publication_verify.go:209–267`, and `publication_codec.go:176–185`. ExecStartEvidence fields are private; Wire copies bytes. The only normal nonzero producer freshly verifies pinned-root issuer, actual descriptor, exact expected context, signature and bounded time before returning canonical evidence. The protocol's max wire is 4096, its encoder is json.Marshal, and all context/runtime fields match the new schema. The new journal correctly distinguishes protocol HTML escaping from its own SetEscapeHTML(false) record encoding. Its equality comparison receives decoded UTC values rather than caller-controlled time representations. This check found no drift or authenticity shortcut.
2. **Original capability versus historical storage.** Read complete `storage/state/etcd/exec_effect_types.go`, `exec_effect.go`, `exec_effect_read.go` and `exec_effect_validation.go`. PreparedExecEffect has private origin/capability/draft fields and only a public diagnostic Reference; the journal adds no accessor or constructor for it. Prepare checks the original capability, fixed draft deadline, exact retained record/issuer fences and fresh ticket before returning Prepared (`exec_effect.go:16`, `:102`, `:157`). Load returns copied structural history (`exec_effect_read.go:14`), never a capability. The private draft keeps its original deadline and context (`exec_effect_validation.go:27`, `:33`, `:90`). Neither a local unknown nor a loaded etcd ticket can enter a new launch path through this delta.
3. **No premature Manager/runtime wiring.** A targeted Go-source search of internal/cmd/pkg found no controltarget import or constructor/RecordUnknown call outside its own package. The exact new git name-status contains only the journal package, script and two journal documents. Read Manager's imports/fields and Exec acquisition-to-dispatch path (`internal/sandbox/manager.go:178`, `:2221`): it still calls `m.runtime.Exec`. Read Docker's Exec path through native create/attach (`internal/runtime/docker/exec.go:17`), Kubernetes dispatch methods (`internal/runtime/kubernetes/runtime.go:2845`) and execInPod's native request construction (`kubernetes/exec.go:31`). Existing adapters remain unchanged. `cmd/sandbox/main.go:268` still constructs redisstate; `:334`/`:401` retain Redis repositories. Config conversion in `cmd/sandbox/redis_config.go:10` still supplies Redis options. These are deliberately uncompleted migration paths, not evidence that the journal enforces execution yet.
4. **Protected storage composition.** Fresh complete review of both cold constructors/accounting and hot record APIs confirms shared mutex/flock ownership, complete identity/runtime/epoch binding, no history relist in Lookup/RecordUnknown, no fresh counters after poison, no automatic missing-manifest repair, and no call to external storage or runtime APIs. Unsupported build stubs reject construction and do not supply an unlocked fallback. This resolves the earlier task-scoped inherited-filesystem review limitation at the full-unit level.
5. **Additional concrete cross-unit risk: retained temporary intent interpretation.** Cold scan validates/counts complete temps but does not promote them to final records; Lookup only reads `<command>.json`, and new-record absence checking does not compare arbitrary retained temps. That preserves the specified passive audit/recovery model: only successful final persistence establishes the immutable returned record, and an error/absence does not prove no attempt occurred. A later physical consumer/recovery tool must not infer failure or replay eligibility from final-file absence, or treat differing retained attempts as fresh launch authority. This is explicitly retained as boundary 61 below, not silently assumed solved by canonical retry.

## Verification evidence actually inspected

No suite, race, vet, build, Docker or focused test was rerun in this review. No new unresolved code doubt required one. Read the complete implementation reports for Tasks 1, 2 (including fix append) and 3; all three task reviews and the scoped Task 2 fix review; the full Root final-gates report, native reports and JSON metadata. Checked selected raw stored logs by parsing all result/warning markers and extracting the named acceptance lines rather than claiming every verbose test line was manually reread.

- `root-final-host-race.log`: 1017 lines; independently counted 495 PASS, 38 top-level PASS, zero FAIL, two expected actual-chown root-only SKIPs; package 38.468s, final `ROOT_HOST_RACE_EXIT=0`. No DATA RACE/panic/warning marker. Both skip names are explicit. This is host race, not Linux race.
- `task-3-root-linux.log`: 1030 lines; independently counted 503 PASS, 38 top-level PASS, zero FAIL/SKIP; final PASS, `actual_exit=0 attach_exit=0`, `ROOT_SCRIPT_EXIT=0`. Actual owner tests and all six layout owner subcases are PASS. Read the source-associated native report/JSON: exact owned project `sandbox-target-journal-test-68272-1791322082`, script exit 0 and independently empty container/volume/network inventories, all inventory commands exit 0. Script cleanup suppresses individual rm errors, so individual rm exits are not independently established for this run.
- Raw full-repository log read in full: 29 passing test packages and eight no-test packages, native etcd 117.057s, controltarget 30.141s, `ROOT_FULL_REPO_EXIT=0`. Read the exact owned wrapper: fresh native etcd and foreign endpoints/container identities are exported before `go test -count=1 ./...`. Read JSON recording empty post-trap inventories for `sandbox-etcd-state-test-69171-1791322195`. Nonverbose output does not prove per-test skip or injected-warning counts. The listed ignored no-test package was not opened or used as evidence.
- `root-final-vet.log` and `root-final-build.log` explicitly contain `ROOT_VET_EXIT=0` and `ROOT_BUILD_EXIT=0`. Root records the scoped diff-check exit 0. These are stored command results, not a claim of reviewer execution; empty output alone was not treated as an exit-code proof.
- Task 1 raw RED summaries: validation 61 FAIL; codec 259 FAIL/62 PASS; final focused 326 PASS/zero FAIL/SKIP. Task 2 I1 RED has three outer failures plus its captured intentional child failure; focus/race each six PASS/zero FAIL/SKIP. Native Task 2 ProtectedFiles has 34 PASS and fix selector 39 PASS, both zero FAIL/SKIP; their JSON includes actual exited state, wait exit 0, source-associated binary hashes, exact ownership and zero cleanup inventories. This establishes their historical task gates without relabeling them final-full-suite evidence.
- Task 3 focused race log: 65 PASS/zero FAIL/SKIP. Root final host/Linux logs confirm the same M1, capacity and 0/1000 fixed-point measurements at frozen product source. Actual public fill is 50 records/55111 B, next 1095 B rejected at projected 85 percent, with existing retry/query/close available. Observed warm-bucket new write is six openat/four fstat/one write/one file fsync/one rename/one directory fsync/four closes; retry and Lookup are two openat/three fstat/four read attempts/two closes, with zero directory pages for both histories. Cold setup and new-bucket creation are excluded. These are observed syscall attempts, not a universal EINTR-free upper bound or QPS/p99 claim.
- Actual 1000-record logical content is 1,110,361 B and three steady journal FDs. Linux reports 81,920 allocated directory bytes plus 4,100,096 allocated file bytes; host directory-block zero is not zero inode/metadata cost. The hard-file fixture and cold recovery remain finite synthetic evidence.

## Source coverage and method limits

Read **all 4363 lines / 175741 bytes** of `review-8a71e73..7642fc1.diff`, including every new product/test/script/document hunk, to actual EOF. Consecutive main passes were 1–500, 501–1050, 1051–1700, 1701–2350, 2351–3000, 3001–3650 and 3651–4363. The first display elided part of the specification; re-read 145–218 to recover that interval before claiming complete coverage. The initial combined requirements display and a combined implementation-report display were also truncated; the complete specification was recovered from the full diff, and Task 2 report lines 45–125 were reread to recover the latter. Independently compared the package's diff body against `git diff --no-ext-diff 8a71e73 7642fc1`: byte-identical. All 18 changed paths are additions, 4214 inserted lines.

Read the current full spec/plan, own progress rulings, supplied complete branch log/stat and the full-branch package header/history context. The package header adds the final doc-only commit missing from the earlier saved standalone log. Read the complete permanent exec-effect final review and complete authority final review, including their final coverage/fix dispositions. Under Root's explicit efficiency ruling, the **1,867,864-byte `b99b823..7642fc1` old-plus-new context package was not freshly read end-to-end**. Its certified old baseline is reused, while this new delta and named affected unchanged integrations were read fresh. This is not a fresh whole-branch EOF claim and does not replace the eventual global production call-graph gate.

One targeted search initially guessed `internal/manager`, which does not exist; the correct `internal/sandbox/manager.go` was located immediately. An initial exclusion glob included journal hits; it was corrected to `!**/controltarget/**` for the external-call search. Neither navigation issue affected source, evidence or verification results. No unrelated document/configuration/secret or sibling workspace was opened.

## Declined to judge and inherited individual dispositions

Each line states a distinct considered boundary and reason. Mandatory later means required authorized migration work, not a waiver. Predecessor IDs 1–52 are retained individually so no earlier disposition is silently dropped; entries explicitly marked CLOSED/SATISFIED are tracking statuses rather than newly declined behavior. All other entries need the controller's retained or new ruling before global completion.

1. Physical Create/Prepare deduplication and crash recovery — mandatory later; local journal storage does not invoke or arbitrate physical runtime creation.
2. Truthful birth/ready observations and receipt issuance — mandatory later; closed file contents and signatures do not establish live mount/gate observations.
3. Physical mount-once and FUSE retry exclusion — mandatory later; no physical mount consumer is delivered.
4. Durable intent with current original operation/task authority — metadata and passive journal advanced; target consumption and independent task authority remain mandatory.
5. Target Begin/End, streams/descendants and terminal receipts — mandatory later; unknown is deliberately not physical terminal proof.
6. CloseData/CloseAll, exclusive barriers, both drains and legal reopen — mandatory later; this close-only journal cannot prove drain or open a new epoch.
7. TTL extension, destroy/task machine and safe owner release — mandatory later; no evidence-gated lifecycle engine is supplied.
8. Certificate install/activation/rotation/revocation and UUID uniqueness lifecycle — mandatory later at targets; this unit does not install or activate authority.
9. Production issuance and root/delegate key custody — mandatory later; test signers and opaque evidence interfaces do not establish operational isolation.
10. Actual trusted PID1/launcher, management UID and IPC/proc/FD/key isolation — mandatory later; UID0 filesystem tests are not a production sandbox boundary.
11. All raw/legacy execution bypass removal and descendant containment — mandatory later; Manager and both existing adapters remain unmigrated as directly checked.
12. Production controlled clock and target-time rechecks — mandatory later; journal intentionally accepts historical diagnostics without a new clock.
13. Atomic UTC expiry at storage commit/external effect — not promised; existing bounded observations and later target rechecks remain the chosen contract.
14. Warm pool, persistent slots/free indexes and recovery accounting — mandatory later; fixed journal point cost does not implement pool/quota coordination.
15. Docker/upload/extract/workspace mutation adapters — mandatory later; passive records do not enforce final payload/options at those boundaries.
16. Partition scheduler, R+1 watches, shared renewal and bounded queues/caches — mandatory later; no replacement orchestration is supplied here.
17. Collector/readiness/autosync and hung FUSE probe slots — mandatory later; no real bounded probe scheduler is implemented.
18. Safe journal/receipt/snapshot GC, history/churn and unknown backpressure — local capacity refusal advanced; safe retention/recovery/global budgeting remain mandatory and unknown cannot be deleted by age.
19. Manager/API/SDK/native-only configuration wiring — mandatory later; the absence of current wiring is intentional for this intermediate passive unit.
20. Old-version drain and complete Redis/Sentinel removal — mandatory later; current production Redis initialization remains, and this review does not approve migration completion.
21. Production mTLS/auth/RBAC/prefix/bootstrap isolation — mandatory later; local native fixtures do not establish deployment controls.
22. Disaster restore epochs and old process/writer/credential quarantine — mandatory later; local exact identity mismatch is only one prerequisite.
23. Actual 10k/100k/1M Pod/FUSE, QPS/p99 and recovery SLOs — mandatory later; synthetic file histories are not real fleet acceptance.
24. Multi-cell routing/cross-cell semantics — phase 6, outside current phases 1–5; no cross-cell guarantee is inferred.
25. Malicious trusted Clock/signer/callback implementations — outside the trusted dependency contract; in-process hostile code isolation is not claimed.
26. Arbitrary Byzantine envelopes across every old generic native path — not newly audited/retrofitted; the unchanged certified baseline and new strict consumer remain separate.
27. Coordinated forgery by an authorized raw etcd writer — outside application capability guarantees; production writer isolation remains mandatory.
28. Early-Fatal join modernization for every old harness — deferred maintenance; new journal workers are reviewed, not a certification of every old test helper.
29. Prior actual-Grant Stage-copy regression M2 — CLOSED in the predecessor unit; this journal does not change that code or reopen the accepted fix.
30. Current dependency/image vulnerability or supply-chain certification — not performed; no dependency/image changes are introduced and no advisory audit was requested.
31. Unrelated Sentinel edits and controller bookkeeping — preserved/excluded; controller must finish its own journal gate checkbox without staging unrelated work.
32. Original remaining monotonic Lease start bound — predecessor producer implemented; delivery and target rechecks remain mandatory, and journal history cannot substitute.
33. Accepted-only renewal and non-start command purposes — separate mandatory protocols; unknown records expose neither accepted nor renew/task/file authority.
34. Final payload reconstruction and credential/env/path/default policies — target enforcement mandatory; digest-only storage deliberately lacks the payload.
35. Standalone registry Load detecting coherent pre-discovery recreation without an original revision — intentionally not promised; no new registry behavior in this delta.
36. Permanent issuer-rotation retention and safe GC — mandatory later; journal capacity does not budget or delete the issuer registry.
37. Provider concurrency/lifetime/exact-signer retention and rotation availability — unchanged trusted operator contract, not reimplemented by passive journal.
38. Generic Stage writes/deletes to command-issuers — CLOSED predecessor hardening retained; journal introduces no generic Stage entry point.
39. Certificate validity at exact storage commit — not a historical-storage authority promise; subsequent authenticated effect delivery must recheck freshness.
40. Source reading gate — SATISFIED for the complete new journal delta; certified old baseline reused under explicit ruling, no fresh 1.8 MB whole-branch EOF claim.
41. Legacy creation-claim decoder strictness — unchanged trusted-storage/corruption classification boundary; no new path adopts its history as authority.
42. Birth-publication delegate/root equality — retained compatibility/key-custody boundary; journal does not change that certificate family or waive production key separation.
43. Exec three-clause versus Renew four-clause operation fences — unchanged predecessor contract/budgets; journal adds no writer that can exploit the difference.
44. Coherent externally forged effect/receipt pair — outside raw-writer protection; local history adds no means to reconstruct a Prepared capability.
45. Historical opaque etcd tickets not freshly verified by Load — intentional diagnostic API; journal accepts opaque verifier evidence, not raw loaded tickets, and grants no live authority.
46. Generic Stage invalidation of exec business history — unchanged trusted mutation surface; no journal call introduces it and safe GC remains separate.
47. Provider rotation during uncertain pre-entry registration — retained provider retry contract; no new journal-side registration/signing behavior.
48. Stage Revoke proof and Begin-failure cleanup context — unchanged metadata-cleanup boundary; neither it nor Journal.Close establishes external termination.
49. Preemptible capability-mutex waiting — unchanged predecessor contract; journal also serializes IO and does not promise prompt cancellation while another operation holds its mutex.
50. Recovery after creator loss/original deadline — remains metadata-only; neither etcd Load nor local unknown/temps can remint payload/capability or replay commands.
51. Conservative fixed ticket-window availability — retained cost; no record retry extends NotAfter or business expiry.
52. Delivery from cached Prepared — still requires future fresh original fences/time/target checks; only diagnostic Reference is public and current code has no transport.
53. Malicious same-management-UID/root mutation of journal/lock/directories — outside the specified filesystem writer boundary; lifetime mutex/flock assumes the protected identity is isolated in deployment.
54. Ancestor-directory trust, ACL/mount/filesystem deployment policy — outside the constructor's explicit owner/mode/no-symlink checks; trusted canonical parent and supported local filesystem provisioning remain required, not proven by POSIX mode alone.
55. Power loss, host failure, filesystem hardware persistence or crash at every machine instruction — not certified by fsync-boundary injection and process hard exit; those tests prove the stated process/filesystem contract only.
56. Immediate cancellation of blocked OS fsync and mutex waiters — not promised; pre/post context checks prevent successful late acknowledgements but cannot universally interrupt the underlying syscall.
57. Physical disk blocks/inodes, idle target process/FD overhead and preservation of user resource quota — later PID1/deployment budgeting mandatory; LogicalBytes excludes block and directory overhead and reserves no 256 MiB upfront.
58. Automatic repair/archive of missing manifests, malformed/incomplete temps or binding/BootID changes — intentionally unavailable; fail closed and retain evidence until a separately reviewed recovery protocol.
59. Always-available close/recovery at hard byte/file limits or disk errors — not promised; gate temp needs real capacity and no unknown evidence may be removed merely to obtain success.
60. Linux race and unsupported-platform execution certification — not performed; actual Linux CGO0 native and host race are distinct, unsupported constructors explicitly reject configuration.
61. Complete retained temp versus committed command identity — passive audit model only; temp files are validated/countable but not promoted or queried as final records, so future recovery must account for uncertain/differing attempts before any physical decision.
62. Arbitrary caller-supplied initial identity and path authority — trusted launcher configuration contract; Create validates shape/current UID and refuses existing targets, but does not prove the supplied runtime binding is a real attested incarnation.
63. Script cleanup when Docker removal/inspection fails — best-effort exact-label cleanup; final reviewed zero inventories establish this run only, not universal cleanup success. No individual rm-exit claim for the canonical final script run.
64. Synthetic hot-path equality as production performance — explicitly not inferred; cold scans grow with this runtime's history, warm-bucket counts omit bucket creation and can vary with EINTR.

## Recommendations and assessment

Preserve the closed I1/M1 evidence and the one remaining optional T3-M1 disposition in permanent unit documentation. If the controller elects the terminal-state assertion, use its one grouped final fix wave and a scoped review/appropriate focused script verification; another broad review or unchanged full-suite rerun is unnecessary. Carry each retained boundary into subsequent target/transport, real launcher, dual-drain, task, scheduler, collector, GC and production migration work.

**Ready to merge this passive journal unit into the continuing feature branch? Yes.** Requirements are met, actual host/native/repository gates support the reviewed source, and no blocking issue remains; T3-M1 is nonblocking.

**Ready for global production merge/deployment or to declare phases 1–5 / Redis removal complete? No.** The new unit stores closed/unknown diagnostics only; production launch enforcement, lifecycle, native wiring, safe retention and migration acceptance are still mandatory. This review authorizes prerequisite continuation, not global completion.

## Complete independent scoped final fix review

**T3-M1 — Require actual terminal completion before accepting/labelling the native journal test result** — ADDRESSED. `scripts/test-runtime-target-journal.sh:80` now obtains Status, Running and ExitCode in one inspection. Lines 83–86 preserve failed-inspection rejection; lines 89–97 require exactly `exited false` and a canonical decimal exit in 0..255 before the completion label at line 98. A created/running state's default zero cannot be accepted. Lines 100–103 retain nonzero attach handling when actual exit is zero and retain actual test exit propagation.

### New Breakage in the Fix Diff

None. Critical: 0; Important: 0; Minor: 0. The anchored tuple check rejects incomplete, malformed and nonterminal observations. Canonical decimal validation avoids shell octal interpretation, and the range check rejects values above 255. The fix changes only the post-attach observation/validation/completion evidence branch. The diff leaves fixture creation, lifecycle/traps, actual test invocation and existing exit precedence intact; it contains no Go, image or product-configuration change.

### Out-of-Scope Observations

None. No previously accepted unit or old branch code was re-reviewed.

### Checks and Evidence

- Read the own final-fix brief first, the complete scoped re-review prompt, the complete implementer `final-fix-report.md`, and the complete supplied fix package `review-7642fc1..34b9a03.diff` once. Package names source BASE `7642fc17bb3e9df23dad6453272859b8ff5e02b3`, HEAD `34b9a03beed289fc97678a284fa6be7702d13082`, one script-only commit, 13 insertions and 2 deletions. No changed-file reread or Git command was needed.
- The implementer report names `bash -n`, working/staged `git diff --check`, complete diff self-review and script-only staging/commit, all with reported exit 0. These are reported implementer checks, not reviewer-executed tests.
- Read the complete root `final-fix-root-linux-report.md` and `final-fix-root-linux.json`, and the final 12 lines of the exact root raw log. They identify canonical execution at the same frozen HEAD. The raw ending confirms `PASS`, `Linux native test state=exited running=false actual_exit=0 attach_exit=0`, zero top-level failures/skips, and `ROOT_SCRIPT_EXIT=0`.
- Independently parsed the exact root raw log with `awk` for result markers: command exit 0; `PASS=503 top_PASS=38 FAIL=0 SKIP=0`. This agrees with root's JSON/report.
- Independently searched that exact log with `rg -ni 'warning|data race|panic'`: command exit 0; exactly one match at log line 65, the measured capacity-state diagnostic `warning=true` from `journal_capacity_test.go:50`. No DATA RACE or panic match. Root classifies zero actual emitted test warnings, consistent with the sole raw match being a capacity-state diagnostic.
- Root's JSON records the exact owned project `sandbox-target-journal-test-71964-1791323093`: post-trap container, volume and network inventory commands each exit 0 with empty remaining lists and empty stderr. This supports empty observed inventories; suppressed individual cleanup command exits are not separately proved.
- Two initial combined reads returned exit 1 because the dispatch's concatenated filename hints were interpreted as `diffreview-7642fc1..34b9a03.diff` and `final-fix-root-linux-report.json`, which do not exist. A narrowly filtered `rg --files` located the actual diff and JSON names. Both were then read successfully and completely. No review content was omitted due to those failed filename reads.
- No suite, Docker command, Git command, subagent, unrelated Sentinel/configuration/secret read, sibling-workspace access, or source mutation was performed. Only this explicitly requested own report was written.
- The actual native run proves the accepted successful terminal path. Rejection/attach-error/nonzero actual exit branches were assessed from the fix diff; no runtime failure-path coverage is claimed. Native CGO0 execution does not prove Linux race, power-loss or production isolation. Prior unchanged Go gates remain root-owned earlier evidence and were not rerun or recast as reviewer execution.

### Verdict

**Fix round:** All findings addressed, no new Critical/Important breakage. Findings addressed: 1/1; open: 0; new breakage: 0; out-of-scope observations: 0.
