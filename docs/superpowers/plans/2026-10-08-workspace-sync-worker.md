# Original workspace sync worker identity Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use subagent-driven-development, a fresh sole author then sole independent task review.

**Goal:** Verify and pin one actual nonprivileged sync worker process identity, without reconstructible execution authority.

**Architecture:** Add a fixed private role to existing process-global launcher bootstrap; reuse bounded native observation/hardening, bind exact worker UID/GID and original boot/start/ns/exe seal. Public reference is copied diagnostics, not provider authority.

**Tech Stack:** Go1.25.6, Linux CGO0 AllThreadsSyscall, existing x/sys/unix and UUID dependency, owned pinned Docker fixture.

**Spec:** `docs/superpowers/specs/2026-10-08-workspace-sync-worker-design.md`.

## Global Constraints

Branch codex/etcd-state-management BASE3688e5c. Prior intent OWN CLOSED neverread/reuse/re-dispatch; accepted code/trackedreports allowed. Do not touch unrelatedSentinel, Redis/main/deployment/trust/credentials/dependencies, or provider/IO/terminal protocol. No push/merge/newthread/download. Source/tests/script/docs/format apply_patch only. Never bootstrap onhost; native mutations ownedfreshLinuxCGO0containers only. OldPID1/monitor and oldTestMain modes unchanged. No per-idle-sandbox resources, callerworkerlabel/callback/success/refreconstruction. AllnativeFDs bounded+CLOEXEC+Closed. Exact11paths below; extra path needs concreteRootruling.

## Review Focus

- Realprocess role cannot be faked by configurationWorkerID, literal/copied reference or oldPID1/monitor boundary; bootstrap once evenafterfailure.
- Worker UID/GID mismatch acrossthreads and laterkernel/seal changes reject, not accepted as anynewnonroot worker.
- Strictbounded proc parsing handles comm parentheses/space and rejects malformed/overflow/magiclinkescape orwrongfilesystem.
- Native fixture lifecycle observes actualprocess terminal/join and exactownedcleanup; host never mutates livekernel.
- Oldroles and defaultnative fixture remain unchanged; diagnostic proof not inflated into actualprovider/credentials/settlement authority.

## Task 1: Actual original worker bootstrap, seal and native proof

**Files:** Modify6: `internal/runtime/launcher/kernel_types.go` onlyprivateworkerrole/validator/privateworkersealfield; `kernel_linux.go` workerbootstrap/sharedinitseal/ValidateCurrent exactworkerpins; `kernel_mutate_linux.go` onlyworkerzero-capbranch; `kernel_unsupported.go` workerunsupportedstub; `kernel_inspect_linux.go` onlyworker cross-threadUID/GID uniformity; `kernel_native_test.go` onlyfixednewTestMain mode branch. Create5: `internal/runtime/launcher/workspace_sync_worker_types.go` copiedreference+strictstructuralvalidation; `workspace_sync_worker_seal_linux.go` boundedactualselfseal/procstatparser; `workspace_sync_worker_test.go` hostmodel/unsupported/reference; `workspace_sync_worker_native_test.go` actualbootstrap/childprocess/newselector fixtures; `scripts/test-workspace-sync-worker.sh` ownednativefixture. Total11paths.

**Consumes:** existing KernelBoundary/KernelSnapshot/processglobalinitialize/inspect/mutate; procOpenAt/procReadAt/sealStat helper; fixedlocal3.6.15image digest fromtrackedoldnative script, read-onlyreference only.

**Produces:** `BootstrapWorkspaceSyncWorker() (*KernelBoundary,error)`, `WorkspaceSyncWorkerReference` copieddiagnostics andstructuralValidate; `KernelBoundary.WorkspaceSyncWorkerReference()`; exactoriginalworker seal checkedbyValidateCurrent. Fieldnames may be chosen byauthor per strictspec; no new provider capability.

- [x] Add hostworkerrolemodel callable scaffold and compiledsemanticRED without undefinedsymbols orrealhostmutation.
- [x] Implement privateworkerrole strictactualUID/GID/caps/NNP/hardening witholdrolespreserved, originalsingleattemptbootstrappolicy.
- [x] Add boundedoriginalseal/ref copy andstrictprocstat parsing; actualValidateCurrent pins UID/GID/boot/start/ns/exe, selfcopied/foreignref never authority.
- [x] Add hostnegative/reference/unsupported andnewnativeoriginal/copy/repeat/actualchildidentity plus4realnegativecontainer cases; boundedphysicaljoin/ownedcleanup, nooldselectors.
- [x] Run host `go test -race -count=1 ./internal/runtime/launcher -run '^TestWorkspaceSyncWorker' -v`; newnativeownedscript OWN; packagevet/Linuxarm64CGO0compile/gofmt-d/diffcheck. Reportactualcounts/limits andmetadataSHA/loghash, exact11pathsmallsourcecommitunderOWNfcntlLOCK_EX/prepostempty.
- [x] Soleindependentreview andscopedfixifneeded, Rootacceptance withallprooflimits, next actualtarget/providerwire/native terminalsettlement stillopen.

Accepted cdb5612/b75fbeb, sole review+script scopedfixreview remainingC0/I0/M0. Host34actualmodelleaves; 5actualnativekernelcontainers +childproof, CGO0notrace; fix3actualchildlifecyclecases+1freshnativepositive, baselinekernelnegativesnotreplayed. Verification `../reports/2026-10-08-workspace-sync-worker-verification.md` carries scope/rulings/hashes. This diagnosticbootstrap is notproduction/provider/credentials/settlement/safe-release/capacityacceptance.
