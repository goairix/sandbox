# Bounded Backend archive receiver acceptance

Source `bd46b2bcfc85f23db94aaeb16dd7aff749523ce8`; BASE `03b5e5997ed4577bef242549567711b47d4215b6`. Spec/plan: `2026-10-08-backend-workspace-archive-receive-design.md` / `2026-10-08-backend-workspace-archive-receive.md`. Exactly eight new private etcd receiving-core paths; no public Backend/client/production wiring changes.

The original store reserves concurrent jobs, archive disk bytes and exact fixed SHA256 path/kind index backing before IO. Empty actual-UID-owned0700 root and exclusive nonblocking directory flock reject cold bytes and concurrent stores. Original0600 nofollow/exclusive file descriptors are checked for UID/type/link/device/inode. Uncertain cleanup retains files and accounting. Only admitted active/retained jobs have owners; this code adds no idle sandbox actor, watch or timer.

The original Destination opens/sends/receives/closes its sealed real TLS Session. Actual DATA bytes/count/hash, signed exact completion attribution, authenticated EOF and physical/logical TAR validation precede successful return. Producer-compatible names without directory slash, nanosecond PAX and unsorted children are accepted. Duplicate/missing-parent/non-directory-parent paths, sparse/special metadata, malformed end/body/type/path and invalid summaries refuse. Private original witness/copy diagnostics grants no current KV, external upload or release authority.

Close cancels and physically joins original callbacks and Session.Close before file destruction/unlink/refund; cancellation or failure during the physical connection Close suppresses success. Concurrent close shares the original owner. Tests pin actual blocked callback and raw Close, private file identity uncertainty, sealed/copy refusal, constructor/job/disk/index quotas and actual DATA exceeding per-job reservation.

## Verification and independent review

Compiled behavior RED: nonempty private directory was incorrectly admitted before implementation. The raw failure is meaningful; the author's subsequent zsh read-only `status` assignment issue was corrected in final wrappers and does not itself establish a command exit. Author recorded Go test exit1 and final checks exit0.

Final commands, using `/Users/dysodeng/go/go1.25.6/bin/go`:

- `go test ./internal/storage/state/etcd -run '^TestTaskArchive' -count=1`: pass.
- `go test -race ./internal/storage/state/etcd -run '^TestTaskArchive' -count=1`: pass, package2.369s; log SHA256 `5ea4a322ef83a9fbb0078caf429a14856739c48b212399ace1cc9c53623e2afe`.
- `go vet ./internal/storage/state/etcd`: author-reported exit0, empty log.
- `CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go test -c ./internal/storage/state/etcd`: author-reported exit0, empty log; compile only, no native/PID1 execution.

Root matched all eight committed/current source byte hashes and Git blob identities, exact changed paths, five raw-log hashes and empty index. No tests were repeated for acceptance. Author report SHA256 `2b9e47dc95d339cd5ea9a300191e3b97964e60fbc83dfb9d1f4239285860c2d6`; evidence manifest SHA256 `a38934e178b4f32579dd46dbd50cbf7d9c6eabbf66651c50c1f0118b092fd19e`. Empty vet/compile logs do not independently prove exit status; the manifest carries reported command results.

Sole independent reviewer `/root/backend_archive_receive_review`: spec **PASS**, quality **APPROVED**, Critical/Important/Minor0. One diff pass plus named transport/verifier contract checks; no suite/race/native gate replay or duplicate Root hash audit. Raw artifacts belong to the closed SDD workspace for this plan; this tracked report preserves acceptance without requiring that workspace to be reopened.

## Limits and next unit

Consumer tests use actual files, pinned mutual TLS and genuine fixture signatures; they are not actual PID1 or current Backend KV sender evidence. Directory must be empty on fresh start; retained cold files require a later explicit operator cleanup policy. Index budget is exact backing allocation, not whole Go RSS or capacity/performance acceptance. Main still uses Redis.

Next: purpose-specific original Backend issuer and immutable native archive command bound to original final-sync/dual-drain/current claim, followed by guarded long receive with concurrent renewal and fresh pre-send/post-receive native checks. Provider settlement, actual termination/safe release, production adapters/scheduling/FUSE/collector/idle certificates, big-N capacity, deploy/restore/drain and Redis removal remain required.
