# PID1 workspace archive scoped acceptance

Branch `codex/etcd-state-management`. Source commit `3408bb343256432e10306375a8b461e4608271cc` (`feat(launcher): add confined PID1 workspace archive source`): six planned source/test/script paths, 1038 insertions. Scope commits `4874e441895eee6b499885130ae72bb0ba638134` and `447bfce766414d7ea865d6cad3f78fe77e464878`. Spec: `../specs/2026-10-08-pid1-workspace-archive-design.md`; plan: `../plans/2026-10-08-pid1-workspace-archive.md`.

## Delivered behavior

Original current Linux PID1 boundary and original process-absence observation can open fixed `/workspace` readonly. Native root-relative openat2 forbids traversal, symlinks, magic links and child mount crossing; unsupported errors do not fall back. Only directories and singly linked regular files are archived. A synchronous original source is single-consumption, serializes Close with actual IO, retains ownership while blocked and closes descriptors on return. Canonical TAR output has bounded pages/chunks/depth/path/entries/total bytes, including final padding. Exact file-length/metadata, final TAR close, fresh process census/root identity/context checks precede the diagnostic digest/count summary; errors yield zero summary.

This is a **physical source primitive**, not production DownloadDir wiring or an authenticated sync producer. No remote settlement or owner-release permission is returned. Momentary census cannot keep USERS/DATA gates closed or prevent future spawn; subsequent Supervisor integration must provide those guarantees.

## Verification actually executed

| Check | Result and evidence |
| --- | --- |
| Native meaningful RED | Compilable all-reject stub fails real content assertion after genuine namespace PID1 bootstrap/census. Exit1; native log648bytes, SHA256 `aa064f5f8aa383c16c01312907b02f6384155de8992f403e615246650ded1f5f`. Exact stub snapshots retained. RED covers this initial positive assertion only. |
| Earlier native GREEN attempts | Each exit1 with7top/24total PASS and one fixture restriction-probe FAIL: cross-volume rename EXDEV, then same-parent rename EROFS instead of assumed EBUSY. Raw evidence retained; fixture corrected without changing production source. Short writer fixture also corrected from invalid count to genuine short write. |
| Final focused native | `bash -x scripts/test-workspace-archive.sh <OWN>/green-native-3`; exit0;8top/25nested-inclusive PASS,0FAIL0SKIP. Raw6089bytes, SHA256 `9ddb757ff694d830cda2e054acd6e2f9fbf57ed9a669d04830842c18ef03dd3a`. One PASS is a fixture restriction probe, not physical replacement proof. |
| Native carrier and cleanup | Existing pinned image `gcr.io/etcd-development/etcd:v3.6.15@sha256:5ed4e32061aa061f970d6500629213e8579ef17571dcf9789074966acb9afad1`, Linuxarm64 CGO0; pullnever, networknone, readonly rootfs, NNP and restricted startup caps. Only new archive tests selected, original bootstrap helper reused. All four attempts have actual terminal states and empty exact owned container/volume/network inventories; final exited/not-running/not-restarting/exit0, attach0 and Docker wait0. This carrier does not run an etcd cluster or establish HA. |
| Affected host race once | Go1.25.6 darwin/arm64 CGO1: `go test -race -count=1 -v ./internal/runtime/launcher`, exit0;16top/1385nested-inclusive PASS,0FAIL0SKIP. Raw163752bytes, SHA256 `6c08a6e3d5c51efb00f443433861641f4ce5dca5e0fdac385fdc215606c23a0f`. Linux implementation/native tests are build-excluded, not implicitly validated by host race. |
| Vet / separate compile | `go vet ./internal/runtime/launcher` and Linuxarm64 CGO0 `go test -c` each exit0 with empty output. Compile-only is separate from native execution and race. |
| Evidence and source freeze | All78artifact/6owned-source/47package-module-input hashes matched; final native/race/vet/compile command metadata each match all six frozen source hashes. Five Go files gofmt-equivalent; whitespace and empty-index checks passed. Exact six-path source commit under OWN fcntl lock. Source/tests/reports/formatting use apply_patch. |

Native assertions cover real archive content/digest/names, copies/foreign/closed/second-use handles, actual outside symlink/hardlink/FIFO/socket/invalid-name rejection, cancellation/writer failure/short writes, byte/entry/depth/path caps, root-pin mismatch, truncation/same-size mutation, final new-process rejection and blocked-writer ownership. Final logs contain no unexpected WARN/ERROR/panic/race/failure; intentional failure diagnostics and CannotVerify record are retained. Existing native scenarios were not replayed.

## Independent review and Root rulings

Sole fresh reviewer read the full unfiltered1110line/41202byte package once, with two named unchanged checks (original census contract and credential/capability contract). Spec **Approved**; quality **Approved**, **C0/I0/M0**. Reviewer report SHA256 `963fd6418f2f5a5ef8f619eedfa2c3d03cbc448e623aa823ddf5c914252a6e32`. No reviewer Git/test/build/race/Docker replay; this is the scoped task/unit gate, not whole-migration review.

| Review boundary | Root ruling |
| --- | --- |
| CV1 physical root replacement | Retain CannotVerify. Actual EROFS proves required fixture restrictions; private pin mutation is sensitivity only. Do not add privileges to manufacture a PASS. |
| CV2 actual nested workspace mount | Retain CannotVerify. `/proc` EXDEV verifies the production opener's cross-mount restriction, not a nested workspace remount. |
| CV3 Linux syscall unavailable | Native ENOSYS branch not forced; direct no-fallback error propagation reviewed, Darwin unsupported branch tested. Do not claim unavailable-kernel execution. |
| CV4 maximum resource performance | Bounds/code/small-limit checks accepted, not1TiB/1million-entry throughput or large-N capacity acceptance. |
| CV5 user-private read identity | **Required before production wiring.** Native files are root-owned; user-owned0600/0700 accessibility is unverified. Existing PID1 mask0xe0 excludes DAC override/read-search, and actual permission errors fail closed. Root UID alone is not a general read policy; Linux documents these separate capabilities in [capabilities(7)](https://man7.org/linux/man-pages/man7/capabilities.7.html). Next design must authenticate the activated user's read identity while preserving the kernel credential contract; do not silently add caps or change PID1 credentials. |
| CV6 Linux race / old native tests | Accept distinct host-race/native-CGO0/compile evidence only. No Linux race or new validation of excluded/unselected accepted tests claimed. |
| CV7 atomic snapshot | Not provided. Metadata/root/census observations do not freeze privileged mutation or guarantee all-tree atomicity. |
| CV8 blocked IO | Ownership retention accepted; forced kernel-read interruption/finite arbitrary-writer completion not provided. Future transport needs real deadlines and unknown retention. |
| CV9 production authority / settlement | Remains unfinished: original authenticated task/producer/Boot binding, durable source journal, closed gates/spawn exclusion, client validation, provider per-request effect intents and settlement, exact termination and safe release. Diagnostic source summary cannot satisfy any of them alone. |
| CV10 additional cases RED | Preserve initial positive RED and later real GREEN; no per-case RED claim or regenerated evidence. |
| CV11 initial environment provenance | Initial RED metadata captured credential-bearing unrelated fields; Root sanitized metadata to explicit allowlist, preserving raw tests/stub. Later captures whitelist necessary settings. Evidence is adequate for reported behavior; complete inherited-environment provenance and historical-output revocation are not established. No credentials enter tracked records; related exposed credentials require rotation outside this code change. |

## Overall migration and next work

Accepted etcd metadata, original runtime closure/quiescence prerequisites, final-sync reservation and this source do **not** mean production migration is complete. Current `cmd/sandbox/main.go` still constructs Redis repositories; existing manager scans/resident controllers remain. No large-existing-N performance improvement or capacity benchmark has been accepted.

Next: resolve actual user-private-file read identity, then integrate the fixed authenticated archive purpose/target journal/stream and original producer issuance; record each provider request before bytes and resolve settled/unknown effects; finish exact termination/safe owner release. Production repository wiring, partition due/dirty scheduling, FUSE/collector, large-N and fault capacity tests, deployment/restore/old-version drain and Redis removal remain. Claim keepalive does not extend an already captured prerequisite deadline, and the existing5second management handler is not a large-archive lifetime budget; the next source-session design must address both explicitly.

OWN `.superpowers/sdd/2026-10-08-pid1-workspace-archive/` retains author/reviewer reports,78-artifact manifest, raw attempts/stubs/binaries, Root audits, commit proof and interface inventory. This accepted unit is closed for implementation; subsequent work must use a new owned plan and not replay it.
