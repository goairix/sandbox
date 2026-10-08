# Workspace archive user access verification

Accepted scoped unit on `codex/etcd-state-management`: source `60a16e90fd23fdfe64a9a8edaf0284b7117893c5`, base `dab2eb81f538538cfe9ab7012b01cf85d3e6b5d7`. Exact eight planned paths, 900 insertions / 15 deletions. Independent task/unit review: spec compliant, quality Approved, Critical0 / Important0 / Minor0. No whole-branch or production-readiness approval.

## Delivered behavior

`KernelBoundary.OpenWorkspaceArchiveAsUser(ctx, observation, uid, gid)` permits the original PID1 archive source to read actual user-owned private files. UID/GID are bounded access policy, not authentication. Existing root API remains. Each user open locks the original kernel boundary and OS thread, clears supplementary groups, switches only filesystem IDs, queries actual state despite silent setter refusal, performs the fixed readonly open, then restores and verifies the exact original profile before delivering an FD. Unverified restoration terminates PID1 with exit70. TAR, reads and destination callbacks run after restoration.

Real/effective/saved IDs remain root0 and management P/E/I capabilities remain0xe0; bounding/ambient remain0. Only reviewed Go1.25.6, Linuxamd64/arm64, CGO0 are supported. The original census reaches AllThreadsSyscall to reject CGO, and the pinned runtime clean-template behavior is a prerequisite. No new capability, product helper process, public credential callback or remote authority was added.

## Actual verification

Evidence directory: `.superpowers/sdd/2026-10-08-workspace-archive-read-identity/`. Retained author report `author-report.md`, manifest `manifest.json`, reviewer `task-1-review-report.md`; current unit evidence is closed and must not be reused to dispatch accepted work again.

| Run | Actual result |
| --- | --- |
| `red-native-4` | Compilable all-reject stub; actual UID/GID1000 helper with all Go-thread capabilities zero created700dir/600file and was joined before census. Expected source-open assertion failed; attach/terminal/wait1. Native log1175bytes, SHA256 `05c905b53fae8eed2c72c8ebecb994d998ed4a66fcb3e27608b3f6abf1db74f0`. Exact six RED snapshots match captured hashes. |
| `carrier-final-normal` | 4 top-level /28 inclusive PASS,0FAIL0SKIP; attach/terminal/wait0. Actual content/digest, root/wrong-user EACCES, supplementary group denial/restoration, all-thread root profile at callback/return, invalid/copied/foreign objects, cancellation, writer error/short-write/panic, zero summaries, FD cleanup and single consumption. Native log7481bytes SHA256 `b0cb618efc30415844328e0f9775afdd50e0744527c3288b42aa61f97ef34067`. |
| `carrier-final-entry` | 1PASS0FAIL0SKIP; attach/terminal/wait0. Actual seccomp ERRNO0 suppresses setfsuid1000 while pretending success; readback rejects. Actual EPERM target1001 also rejects; no source delivered, exact root/groups restored. Log1199bytes SHA256 `0b03f032efc33d048bbe72c21b6ce05416d545ede625d5088b6855025e1fa5e1`. |
| `carrier-final-fatal` | Actual PID1 attach/terminal/wait70, nonrunning/nonrestarting/nonOOM; refusal marker present, no returned-source/writer/GoPASS marker. Fixture script0 means expected-failure verification only, never GoPASS. Log977bytes SHA256 `98824b0b40d445c5ee333e9ccf9f5cdcd399dc6c41d73cd29b68c7b59dce5f06`. |
| `final-host-race` | Pinned Go launcher race once:18 top-level /1387 inclusive PASS,0FAIL0SKIP, exit0, no race diagnostics;163974bytes SHA256 `a36d04539f51fa49d799834f73eb350d04874a65cfd7abd6277e0936b585bade`. Darwinarm64CGO1 excludes Linux implementation/native tests. |
| Host/Linux vet, Linuxarm64CGO0 compile | All exit0, empty streams; test helper built in each native carrier with binary hashes retained. Native CGO0 execution is not Linux race. |

All three final carriers match all eight frozen source hashes. Earlier race/vet/compile match all seven Go files; their sole difference is the later test-script lifecycle fix requiring confirmed exited state before dockerwait. Only affected carriers were rerun. Each final carrier uses the same local pinned etcdv3.6.15 image, pullnever/networknone/readonly/NNP, unchanged startup capability contract, and empty exact owned container/volume/network cleanup inventories.

Root and reviewer independently matched284 artifact size/hash pairs,8 source hashes,50 package/module inputs and20 command records. Complete stdout/stderr, UTC times, argv, exits, source hashes and allowlisted environment metadata remain retained; no full environment capture. Earlier helper-capability/root-stat failures and the short-writer empty-write fixture failure remain visible, superseded rather than counted as RED/GREEN success. No accepted old native scenario was replayed.

Author report SHA256 `3668f0464d7372fd6fa3a41056eb0271d6db0a66d0b8984d7516927274bfcc7e`; manifest SHA256 `5390736674a1d30b85b01571c374b9ba57bb9ae7632c889752df55b18e3481d1`; sole reviewer report SHA256 `52dc11617232eb274f65805c791a06014131b57bdbed41f2fb5cb2923f28ab17`.

## Root rulings on each retained boundary

| Review item | Ruling / remaining work |
| --- | --- |
| CV1 permanent USERS/DATA closure | Accept only instantaneous original census and physical source. Integrating Supervisor must prove permanent closure, original producer joins and spawn exclusion before authenticated streaming. No release authority granted. |
| CV2 authenticated identity / effects | Private-file accessibility is now physically demonstrated, closing the preceding archive unit's access gap for this supported runtime. Raw UID/GID parameters still need immutable verified birth/activation binding. Digest or exit70 proves neither remote settlement nor safe owner release. |
| CV3 platform/runtime coverage | Accept native Linuxarm64CGO0 execution and separate Darwin race/compile evidence only. Linuxamd64 execution, Linux race and foreign C threads are unproved. Runtime/ABI/CGO guards are reviewed fail-closed code, not a tested platform matrix. Deployment must preserve this supported runtime contract. |
| CV4 performance / blocked IO | Accept bounded per-open credential checks only. Capability queries impose per-file cost; no throughput, maximum archive or large existing-sandbox capacity acceptance. Blocked syscall/reader/writer physical return cannot be forced by context cancellation; retain ownership until return or process termination. |
| CV5 unexecuted faults | ENOSYS, physical remount/root replacement, close-syscall failure and panic inside raw scope were not newly forced. Inspected error/restore/confinement paths are accepted within scope; writer panic evidence is outside temporary credentials. Preserve old limitations, no synthetic fault authority claim. |
| CV6 production/data settlement | No provider/FUSE remote settlement, atomic tree snapshot, deployment or production wiring completed. Those remain required gates before Redis removal and whole-migration acceptance. |

Next integration: authenticated fixed archive purpose and bounded actual stream, binding the original task/producer and verified activation user. Then provider request-attempt settlement, exact termination and safe release; production wiring, partition scheduling, FUSE/collector, capacity, deployment/recovery and Redis removal still remain. Production main has not migrated merely because this unit passed.
