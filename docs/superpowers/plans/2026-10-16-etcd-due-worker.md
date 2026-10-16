# Resident due worker implementation plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development. One new sole author and one new sole whole-Task reviewer; Root executes verification and Git.

**Goal:** Run the real bounded expiry consumer with recoverable progress, continuous fixed-revision discovery and original-worker lifetime ownership.

**Architecture:** An owned worker actor coordinates original session renewal, partition/budget use and the existing due page/admission APIs. Permanent diagnostic checkpoints and bounded native Watch replay preserve discovery continuity; only actual admission transactions authorize metadata changes.

**Tech stack:** Go1.25.6, pinned etcd3.6.14 client/native, existing worker control and enrolled v3 due protocol.

**Spec:** `docs/superpowers/specs/2026-10-16-etcd-due-worker-design.md`, implementing the approved master scheduler design.

## Global constraints

All Binding constraints in the named spec apply verbatim. Existing feature branch `codex/etcd-state-management`; record immutable BASE only after the current due-discovery Task closes. No source edits before exact read-only preflight/API and Root tests-only RED gates. Root alone Go/Git/Docker/gofmt; apply_patch for edits. No closed OWN/pairs, Sentinel document, duplicate authors or production enablement claims.

## Review focus

- Late writes below a persisted frontier and events spanning list/Watch/window rollover must remain reachable.
- Unknown metadata results must retain the original attempt and block covered progress; no new-attempt retry from a diagnostic cursor.
- Native Watch buffering, fragment assembly and queue costs must be bounded before allocation and joined on release/Close.
- Early candidates and no-credit waits must survive restart and allow original session renewal without a busy loop.
- Huge overdue sets, compaction and invalid caches must expose lag and bounded rebuilding, never full-sandbox runtime reads or false readiness.

## Task 1: Real resident expiry consumer

**Files:** Focused new `internal/storage/state/etcd/due_worker*.go`, `due_checkpoint*.go`, `due_watch*.go` and tests; narrow existing due page/worker lifetime/Backend.Close integration only where exact preflight establishes need. One new `due_worker_native_linux_test.go`; operator documentation. Freeze exact files/interfaces in current Task ledger before tests. Do not refactor unrelated accepted protocols.

**Consumes:** `OpenWorkerSession`/`RenewWorkerSession`, original partition and Range/relist/claim shares, `ReadDuePage`, `AdmitExpiredTask`, existing exact Stage outcome resolution and Backend.Close lifetime mechanisms.

**Produces:** A real explicitly started/stopped worker, bounded synchronized/lag status and fenced persistent diagnostic scan progress. Exact exported signatures and strict schema/version/key layout are the first preflight deliverable, not permission to guess them during implementation.

- [x] Step1 read-only preflight: exact API/schema/checkpoint8192/128KiB templates, finite original-call Watch with predecode bounds, pre-Grant durable identity, renewal margin and two-phase joins frozen. Root rulings include native progress retry/operation order, event-revision continuation and75s native chain; Step2 tests-only GO.
- [x] Step2 tests-only durable progress/restart/unknown and actual page-resume RED; implement strict checkpoint, actual finite consumer and pre-Grant original attempt persistence.25focusedgroupshost/raceandLinuxvetPASS; commit1f7f7b665b1e438a3023257cce2cb4906ab7acc1. Fullresident/Watchnotyetdelivered.
- [x] Step3 actual original-connection bounded Watch/cache and release joins committed a22f5fcd0a0897a739c621e0670b8de7c1eaa9bf. Nineteen new groups plus directly affected original regressions:52 host/race groups PASS, Linuxvet PASS; complete source/tool/input audits before commit. Predecode aggregate limits, grouped61-node guards, PRE coverage/POST tail, cross-partition expiry ordering and repeated progress covered. Full actor/native/whole-Task acceptance remain next.
- [x] Step4 resident loop, actual expiry/pending recovery, budget waits, original-lease renewal, window rollover and two-phase joined shutdown committed6463b5d46e4efa9ea1818ddb03ecf2cf2abcd4c8. Fifteennewactor/lifetimegroupsanddirectregressions:67host/racePASS,LinuxvetPASS; actualinput/source/tool/archiveauditspassedbeforecommit. UserAGENTS.md requirescomments/readability/packageassessment; currentTaskcriticalcommentsandlogicalspacingupdated. Native/fullTaskreviewremainnext.
- [x] Step5 three-member actual resident chain PASS10.91s after one newtest-helper decoder fix; finalLinuxbuild/vet/entireinput/runtime/cleanup auditsPASS. Commit1017b66513096b70f62e6e2ee82b579d585e925d includesnewnativefileandoperatorguide. Realexpiry/Watchlateinsert/exactunknownrestart/oldfenceloss/3sessionjoins verified. FirstfailednativeandRootcollectorcorrectionsretained;3etcdchildrenownedKill+actualWait(signal:killed),notgracefulshutdown. No physicalexecution/capacityclaim.
- [ ] Step6 whole Task spec+quality review from immutable BASE through all slices, scoped same-pair fixes, tracked bounded acceptance and CLOSED ledger; continue persistent task execution and production migration without stopping.

## Initial interface/conflict scan

| Producer → consumer | Concrete boundary to settle before implementation |
| --- | --- |
| fixed-R page → persistent resume | New authority must validate diagnostic checkpoint and build its own view; no old cursor adoption or newer-header R substitution |
| checkpoint → unknown admission | Only known settled coverage advances; original attempt identity survives loss of acknowledgement |
| listR → WatchR+1 | Late insert below frontier and future candidates must survive crash/window movement |
| Watch transport → bounded cache | clientv3 internal slice can grow; receive size/fragment assembly and original stream ownership need explicit bounds |
| session one-native owner → resident renew | Finite discovery operations and context-aware waits must yield for original renewal, not concurrent reconstruction |
| real controller → Backend.Close | Actual native receives, callbacks and cleanup must return before authority/resources are released |
| Task internal | Durable progress, Watch and actor form one real discovery consumer; physical task hard32/mounted executor remains separate |
