# Persistent workspace sync intent Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use subagent-driven-development with a fresh sole author and sole independent review.

**Goal:** Atomically retain original put intent and shared permanent partition slot under complete current native fences.

**Architecture:** Operator-pinned immutable budget bounds256 fixed partitions; source/body retain one original draft; native intent/slot writes are atomic and never Lease-attached or refunded by Close. No provider dispatch capability is produced.

**Tech Stack:** Go1.25.6, existing native etcd3.6 client/protobuf, canonical strict boundedJSON, current framedTLS fixture.

**Spec:** `docs/superpowers/specs/2026-10-08-workspace-sync-intent-design.md`.

## Global Constraints

codex/etcd-state-management source baselinef1e67e6/planning4f48722. Prior OWN CLOSED never read/reuse/re-dispatch; no Sentinel/dependencies/credentials/cloud/deploy/push/merge/trust/Redis edits. Source/docs/tests/format changes apply_patch only; no cat/heredoc/Python source writes/gofmt-w. Originalten paths plus sharednamespace fixture and strictprefix-preflight rulings below, totaltwelve. Existing64nodes/64KiBrecord/256KiBrequest/reply and source/spool quota unchanged. Fixed256partitions; S1..64, B4096..16384, slot value512, budget config1024, keys1024. No per-idle-N actors/cache/newmap or global counter. No arbitrary caller-success/refund/witness; actualworker/provider authority deferred. Only newselector; no accepted suite replay. Small exact source commit under OWNfcntlLOCK_EX emptyindex pre/post; Root docs excluded.

## Review Focus

- Concurrent Backends contend on same native slot and budget, no duplicate independent allowance; unknown survives original process/claim lifetime.
- Empty wholeoperations prefix in same writeTxn, range semantic and exactprotobuf caps, all originalhistorypins retained.
- ACK loss before/aftercommit and absent/lateTxn: no repeated write, different slot, newbody or false nondispatch evidence.
- Body/source seal/partialRead/Close race: no native work outside registeredgate, no lock acrossIO/join/clientclose.
- Corrupt/rewritten/Leased/ABA intent/slot/budget or coldload: diagnostics only, no promotion to execution/refund/safe-release.

## Task 1: Fixed native slots, original put intent admission and diagnostics

**Files:** Create `internal/storage/state/etcd/task_workspace_sync_intent_types.go`, `task_workspace_sync_intent_records.go`, `task_workspace_sync_intent_budget.go`, `task_workspace_sync_intent_admission.go`, `task_workspace_sync_intent_read.go`, `task_workspace_sync_intent_test.go`, `task_workspace_sync_intent_native_test.go`. Modify `client.go` only optional budget Options/validatedcopy/constructor nativepin, `task_workspace_sync_source_types.go` only one bounded body draft binding, and `task_archive_delivery_native_test.go` only default-preserving optionalbudget fixture wrapper/helper that initializes native marker+sets Options before New. Tenpaths. No claim/sourceguard/close/protocol/limit rewrites without concrete Root ruling.

Root sharednamespace ruling additionally authorizes task_close_delivery_fixture_test.go only default-preserving optional setup factory, totalelevenpaths. Newnative_test factory constructs second distinct task/workspace/runtime through actual same-namespace native Acquire/Claim/Dispatch/Publish/Destroy, yielding separateoriginalarchive/source/body for secondBackend. Never clone historicalKV/capability; nilfactory staysoriginal. Budgetmarker existingcanonicalbytes/revision must be reusedwithoutPut rewrite. Actualfactory signature may be chosen byauthor to match existingpreparationFixture returntuple.

Root preflight ruling adds task_quiesce_stage.go, totaltwelve: actualtaskCheckWire accepts only exact namespace operations-prefix VERSION/EQUAL/0 range specialcase fromspec, oldpoint rules unchanged. Newhostselector tests wrongprefix/target/value/result/end/namespace/keylimits plus actualrequest andbothreply caps. No accounting-clone normalization, no nativeprefix weakening. Existinghelper node definition counts63 for current50+6cmp+2put+5failure; originalactualprotobuf stillchecked.

Root identity/expiry rulings: intent binds BudgetRevision original config birth; coldLoad rejects samebytes config delete/recreate. Optional budget fixture may perform exactoriginallease boundedTimeToLive cleanup per spec, only verified TTL<0 skips originalRevoke; defaultnil path and all other failures unchanged. Actualnaturalexpiry still requires persistent intent/slot and quota assertions.

**Consumes:** exactoriginal PreparedTaskWorkspaceSyncSource.begin/end/local, current taskWorkspaceSyncBody/entry, archive.owner immutablecomparisons/prefix/prepared history, taskCheckWire/nativeheader andstrictJSON helpers, WorkspaceIdentity/TaskWorkspaceSyncPlan. ExistingoriginalClose registration retained. Accepteddefsreadable, CLOSEDOWNnot.

**Produces:** `TaskWorkspaceSyncBudgetOptions`; private budget pin/strictrecordcodec/key helpers/draft; private `source.reservePutIntent(ctx,body) (taskWorkspaceSyncIntentResult,error)` metadata only; public copied `TaskWorkspaceSyncIntentReference/Entry` and Backend.LoadTaskWorkspaceSyncIntent diagnostics. Public recordfields structuralClaim attribution, no actualUID/settlement/capability pretence.

- [x] Write callable scaffold and meaningful compiled RED after actual canonical native/TLS receive+source+body, expect durable intent/slot; capture log. Add bounded/strictcodec/error-option tests before implementing correspondingbehavior.
- [x] Implement canonicalrecords/key attribution and fixedbudget copy/constructornative pin. Nil opt compatible; fail bad/missing/rewritten/leased config beforeintentwrite. Fixedfee upperbound512+2*1024+B, no leased credits/global CAS.
- [x] Register reserve under source gate, originalbody no priorreads/Close andone draft; acquire complete immutable comparisons plus sameTxn VERSION-rangeempty condition andbudget/intent/slot CAS. Actual protobufpreflight request/reply <=64nodes/64KiBrecord/256KiB. No permission pins removed.
- [x] Implement bounded native candidate selection, precisecurrent/quota failures; unknownfixedattempt no resubmit, originalonlymetadataarbitration, coldpublicboundeddiagnosticread; no refund/GC/dispatch API. AllClose leavesintent/slot permanent andjoinsactiveRPC.
- [x] Cover allfive review focuses with realownedetcd/TLS: twoBackend samepartition competition via distincttask/originalsources, unknowncommit/lateTxn andabsence, leaseexpiry/Close retainedcharges, wholeprefix insertion/ABA/restore/budgetidentity, Close heldnativeRPC andcopy/partialbody. Default fixture path remains covered/refuses disabledintent; no oldselector replay. Report faultseam limits andmetadata-only scope.
- [x] Run final `/Users/dysodeng/go/go1.25.6/bin/go test -race -count=1 ./internal/storage/state/etcd -run '^TestTaskWorkspaceSyncIntent' -v`, `PATH=/Users/dysodeng/go/go1.25.6/bin:$PATH bash scripts/test-etcd-state.sh -run '^TestTaskWorkspaceSyncIntent' -v`, packagevet/Linuxarm64CGO0compile. gofmt -d/apply_patch/diffcheck; selfreview/rawlogs/current+GitSHA/metadatareport exacttwelve sourcecommit promptly. Sole review then Root acceptance; next actualtrustedworker/providerwire+terminalsettlement remainsopen.

Accepted source7391a5e/fix27feaf6 after sole review+scopedfixreview C0/I0/M0. Baseline native38actualleaf0SKIP/WARN; fix scopedhost1/native1 leaf plusvet/cross/format. Verification `../reports/2026-10-08-workspace-sync-intent-verification.md` preserves distinct evidence ranges and firstunknownbirth diagnostics limits. No productionmigration/capacity/safe-release claim.
