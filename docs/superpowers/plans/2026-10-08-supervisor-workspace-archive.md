# Supervisor workspace archive implementation

Spec: `docs/superpowers/specs/2026-10-08-supervisor-workspace-archive-design.md`.
Base: `1dd11e3229d2b6698f68a70d8bad73991b05afb3`, branch `codex/etcd-state-management`.

## Global constraints

Use the existing feature checkout; no push, merge, deployment, Redis removal, or unrelated Sentinel document changes. All source, test, script, report and formatting changes use apply_patch. Do not read old CLOSED SDD workspaces or replay accepted native selectors. Each task has one fresh sole author and one independent task reviewer, then Root acceptance; whole-migration review remains at parent completion. Every meaningful verified task is committed separately with exact paths, empty index, and the plan workspace fcntl git-commit.lock. Preserve pinned carrier image and strict native lifecycle/cleanup evidence. Do not capture environment or credentials. Keep reports compact and hash-backed.

## Task 1: Sealed archive transport and destination APIs

Exact ownership (eight paths under `internal/runtime/controltransport/`): `task_archive_frame.go`, `task_quiesce_types.go`, `task_close_frame.go`, `destination.go`, new `task_archive_session.go`, new `task_archive_destination.go`, `task_archive_frame_test.go`, new `task_archive_session_test.go`.

- [x] Add canonical query purpose with nil ticket/certificate/quiescence receipt. Factor private header decoding used by standalone request and RuntimeRequest archive branch; standalone still requires actual request EOF. Preserve existing codecs.
- [x] Add RuntimeRequest.Archive. Add sealed request mode and a private send-mode helper preserving old Send/SendTaskClose/SendTaskQuiesce semantics; archive methods cannot use generic event responses and vice versa. Consume once even on send failure.
- [x] Implement Session.SendTaskWorkspaceArchive, ReadTaskWorkspaceArchiveFrame and RequireTaskWorkspaceArchiveEOF (or equivalent explicit method), context checks/deadlines and physical TLS EOF. No raw connection getter. Response context is independent of completed admission Send context. EOF does not silently discard trailing frames/bytes or treat abrupt truncation as success.
- [x] Add Destination MatchTaskWorkspaceArchive, VerifyTaskWorkspaceArchiveRuntime and VerifyTaskWorkspaceArchiveReceipt wrappers using original trusted identity, credential and immutable verifier, full original session certificate coverage and retained expected values. Follow existing task-quiescence destination API patterns; helpers do not authorize backend effects.
- [x] TDD: compile genuine failing new behavior tests before implementation; real pinned mutual TLS test for send/response lifetime/modes/EOF plus canonical routing and invalid bounds/query. Focused tests while iterating; affected controltransport host race and vet once on final source, Linux arm64 CGO0 compile. No native container required for this pure transport task.
- [x] Self-review, exact-path source commit, report command/artifact hashes plus final source hashes; Root sole independent review and tracked acceptance before Task 2.

## Task 2: Original Supervisor archive owner and activated TLS route

Initial exact ownership: modify `internal/runtime/controlrunner/supervisor_types.go`, `supervisor_transport.go`, `supervisor_failure.go`; create `supervisor_archive.go`, `supervisor_archive_transport.go`, `supervisor_archive_test.go`, `supervisor_archive_native_test.go`; create `scripts/test-runtime-supervisor-archive.sh`. If needed for real blocked TLS output, modify only `internal/runtime/launcher/testdata/archiveuser/main.go` to add a fixed `prepare-large` fixture mode after its existing actual unprivileged-capability verification. Any different required path needs Root ruling before edits.

- [x] Register and seal original archive attempt before pending IO, requiring original successful joined quiescence. Immutable independent deadline/timer before IO; birth policy UID/GID source; no arbitrary summaries or request-selected source. Pending duplicate joins/query semantics must not create DATA resend.
- [x] Transfer activated TLS response ownership from finite admission to archive worker and actual session deadline; chunk source writes into bounded frames outside locks. Separate genuine local signed terminal from final frame delivery. Attribution-only query retrieves only original successful signed completion after fresh checks and actual source owner join.
- [x] Join or independently isolate all archive/source/transport IO before destroying journal/key. Close is idempotent and concurrent callers join. Never hold Supervisor mutex across Journal.Close waiting on archive callbacks.
- [x] Compile genuine new failing test before implementation. Host affected race/vet and Linux compile once final; new restricted pinned-image native selector only, actual PID1, private UID/GID 1000 file, real TLS DATA/TAR/hash/receipt/EOF, query/refusals/admission-context transfer, physical close and blocked IO deadline. Keep abnormal expected exit evidence separate; actual wait/no-OOM/owned empty cleanup required.
- [x] Self-review, exact-path source commit, sole lifecycle reviewer and tracked acceptance. Close this plan ledger, continue backend sender/receiver ownership unit. No claim of production migration or capacity acceptance.
