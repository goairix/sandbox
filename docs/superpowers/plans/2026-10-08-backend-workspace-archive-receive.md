# Backend workspace archive receive core implementation

Spec: `docs/superpowers/specs/2026-10-08-backend-workspace-archive-receive-design.md`.
Base `54e5b77`; branch `codex/etcd-state-management`.

## Global constraints

Existing authorized feature checkout. No push/merge/deploy/Redis deletion, new trust installation, unrelated Sentinel changes or old CLOSED SDD workspace reads. All source/test/report/format edits apply_patch. Sole fresh author, sole independent task review then Root acceptance; whole-migration review later. Exact-path small source commit with current OWN fcntl git-commit.lock covering add through commit and empty index pre/post. Keep evidence lean (raw log SHA, source/Git SHA, command results); do not add full supply-chain custody scripts or replay earlier gates. Raise exact required path changes to Root before edits.

## Task 1: Original bounded spool and archive receive validation

Exact eight new paths under `internal/storage/state/etcd/`: `task_archive_receive_types.go`, `task_archive_spool.go`, `task_archive_spool_unix.go`, `task_archive_spool_unsupported.go`, `task_archive_receive.go`, `task_archive_tar.go`, `task_archive_receive_test.go`, `task_archive_spool_test.go`. No client.go/options/public Backend behavior change in this task.

- [x] Private original store/reservation/witness types; validated trusted limits, fixed bounded duplicate/parent index, global concurrency/disk/index accounting before DATA, origin seals and copied diagnostics.
- [x] Original private nofollow/exclusive spool IO, Unix owner/link/type/inode checks, unsupported fail closed, single original receiving owner and physical close/join/unlink/refund with uncertainty retained. No lock across arbitrary IO/waits and no public path/FD/capability factory.
- [x] Real sealed Session frame receive, actual streaming bytes/hash, exact original verifier/expected completion binding, authenticated EOF, TAR validation including logical PAX entries, unsorted names, fixed path/type/size/end/parent/duplicate constraints. No arbitrary summary promotion, receipt-only empty DATA artifact or upload call.
- [x] Compiled genuine behavior RED before implementation. Meaningful pure host real-TLS/private-file tests for positive consumer plus auth/data/TAR/EOF/quota/lifetime refusals. Focused iteration; final new selectors race once, vet package and Linux arm64 CGO0 compile once. No old native selector or affected runtime/launcher gate replay.
- [x] Self-review, exact-eight source commit and compact hash-backed report. Sole independent review, tracked acceptance, close this plan and continue original backend command sender/claim-renewal integration. No claim of actual PID1/backend KV/production/capacity completion.

Accepted source `bd46b2bcfc85f23db94aaeb16dd7aff749523ce8`. Verification: `docs/superpowers/reports/2026-10-08-backend-workspace-archive-receive-verification.md`. Follow-on original command preparation has a separate fresh plan; current core adds no native KV send authority.
