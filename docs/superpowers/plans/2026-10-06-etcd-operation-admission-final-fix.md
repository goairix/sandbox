# Final fix wave report

Date: 2026-10-06. Workspace: `/Users/dysodeng/project/go/cloud/sandbox`.
Base: `c3fdc6f6ee8062ceb75fdc1e0c206b51dd7d98f1`.
Final source HEAD: `29c3cf6a40f1b2545071e565fdcc0fb655e26362`.

This is the single authorized final fix wave. I1 (both callers), I2 and M1 are implemented and independently committed after focused native/race verification, package vet, formatting, diff checks and self-review. M2 remains explicitly deferred and was not implemented. The controller's final fresh owned fault/race and whole-repository gates remain required; their earlier results are not reused as evidence for this source.

Read final-fix-brief.md, task-constraints.md, final-review.md, the actual fixture/task context and the systematic-debugging, test-driven-development (including writing-good-tests), and verification-before-completion skills. No subagents, separate workspaces, global/member faults, production/runtime calls, credentials, lifecycle changes, push or merge. The existing Sentinel and controller Operation plan edits remain outside these commits.

## Commits and dispositions

| Finding | Disposition | Commit |
| --- | --- | --- |
| I1: serializable empty fence Txns | Fixed both RenewOperation and checkPreparationReplay with a default linearizable point Get inside the comparison Txn, strict returned point validation and actual gRPC boundary regressions. | `358a21ac5fbcb643e30f8941943625c038ca96dc` |
| I2: incomplete failed-admit historical evidence | Fixed explicit completion/no-completion classification and original guard/record/revision checks. | `eec014e73e861054bd537dc0ef34b5a71e26b3a0` |
| M1: delayed admission teardown before worker completion | Fixed release/cancel/bounded join before hook restoration and fixture teardown; completion signal survives worker Fatal/Goexit. | `29c3cf6a40f1b2545071e565fdcc0fb655e26362` |
| M2: generic Stage Grant-boundary comprehensive copy regression | Explicitly deferred, nonblocking, as authorized. Existing deep-copy code was not changed. | none |

## I1: implementation and wire invariants

Files: `internal/storage/state/etcd/operation_lifecycle.go`, `runtime_preparation.go`, new `fence_check.go`, new `fence_check_test.go`.

Both callers send the original comparisons with `Then(OpGet(b.identityKey)).Else(OpGet(b.identityKey))`. Neither uses WithSerializable, historical revision, scan, writes, another pre-Get, retry or sleep. This fixes the same-Txn linearizability classification rather than relying on endpoint timing. Renew still checks the original five permanent fences and exact original guard/token/receipt/mutation envelopes, then renews only the original Lease; failure still makes capability loss irreversible and prevents KeepAlive. Bind/Consume replay remain metadata only.

The only new shared production helper is:

```go
func (b *Backend) validateFenceCheckResponse(response *clientv3.TxnResponse) error
```

It lives in `fence_check.go` and is called only by RenewOperation and checkPreparationReplay. It reuses existing operationResponseHeader and operationNestedHeader envelope checks, requires exactly one non-nil Range response, validates More/count/single point, exact identity key and coherent positive revisions no newer than the outer header. Successful comparisons additionally require the expected permanent Lease-0 identity value; failed comparisons may legitimately return a changed/missing identity, while malformed envelopes are rejected. It does not read, mutate or adopt new authority.

The conservative operation counts are at most 30 comparisons + 1 success read + 1 failure read = 32 for mutation Renew (data Renew has 26 comparisons), and 4 base + all original 24 creation comparisons + 2 reads = 30 for preparation replay. Both remain below 64, use one executed metadata point and the existing bounded keys/values; no extra RPC is introduced. The gRPC regression also checks each actual protobuf request's serialized size <= 256 KiB and total comparison/branch count <= 64. Input/key/value limits and prior conservative admission byte accounting remain unchanged. The fixed-shape active-cost test continues passing after the one read is added.

`interceptFenceRPC(t, b, guard, check) *int` is test-only. A native client with a gRPC unary interceptor observes the final `etcdserverpb.TxnRequest` for the original guard comparison after clientv3 serialization/building and lets the RPC execute against the real fixture. Tests cover Renew, Bind replay, Consume replay, each with successful and recreated-guard failure cases. Both branches must contain only one point Range with Serializable=false and Revision=0; an empty or serializable-only Txn or separate pre-Get cannot satisfy this assertion. All 24 creation comparisons remain present. Tests additionally exercise 14 malformed response shapes for each of Renew and preparation replay; Renew cannot KeepAlive or revive after these failures. Existing changed/recreated permanent/envelope and server replay fence tests remain enabled and passed.

## I2: complete evidence and API distinctions

Files: `internal/storage/state/etcd/operation_admission.go`, `operation_admission_fault_test.go`. No new helper, RPC, read set or resolver refactor. Existing private `operationAdmissionFailure(response, c, result) error` keeps its interface and its fixed evidence order: identity/restore/receipt/guard/token.

Present guard/token records must strictly decode, match c.record and preserve exact c.value bytes. Receipt-backed committed history requires a positive known c.guardRevision, the original guard first revision, a token later than the guard, a receipt later than the guard, the same token/receipt first revision and matching original Reference/Lease/immutable envelopes. Aborted history requires the original guard and later matching receipt with no token. No historical result is assigned before all required evidence passes.

Two caller phases were explicitly considered and confirmed with the controller: guard initialization may fail before c.guardRevision is known (the private field still contains the preflight math.MaxInt64 placeholder; it is replaced by the actual header revision only on successful guard initialization), and admission may fail after guard initialization. If neither token nor receipt exists and the returned Txn is known to have performed no write, the ordinary CAS rejection remains ErrConflict/OperationAborted; a present guard must still decode and match the private record bytes. This is not receipt-backed historical inference. A rebuilt guard without completion records remains ordinary CAS rejection, preserving the existing original-fences regression. Token without receipt remains ErrCorruptReceipt. Any receipt-backed history with unknown/missing/rebuilt original guard is corruption/OperationUnknown with nil capability.

Expanded real-commit/point-evidence tests include missing guard, missing token, missing receipt, receipt-only, aborted-without-guard, guard rebuilt after completion, guard rebuilt before a freshly recreated token/receipt pair, completion-before-guard, aborted wrong order, aborted-with-token, and changed exact guard/token bytes. Existing completion revision/body/Lease defects remain. Valid committed and valid aborted historical results pass. Each final case checks nil capability, the unchanged reference, one original Lease revoke, independent nil cleanup diagnostic and server TimeToLive == -1. The all-BeginOperation runs also preserve successful-header/canceled/deadline/revoked undelivery history, plain CAS rejections and independent cleanup-error tests. No ability is reconstructed from a public Reference.

## M1: failure-safe native test teardown

Only `internal/storage/state/etcd/operation_resolve_test.go` changed. TestResolveOperationLateCompleteTransaction now uses a cancelable context, an independent `finished` channel closed via goroutine defer, and a later-registered t.Cleanup that releases the delayed hook, cancels the context, joins completion with a five-second bound, and only then restores the Lease hook and revokes the original Lease under a separate five-second cleanup context. Fixture cleanups execute afterward. Normal result assertions and all four scenarios (abort, lost commit reply, revoke, natural expiry) remain.

The timeout path reports an explicit failure and does not race the worker by restoring its hook. It does not introduce an unbounded wait; normal native context cancellation and release allow the worker to terminate. No production goroutine/ticker is introduced. Old generic Stage/Acquire harnesses remain outside this scoped fix.

New TestDelayedOperationCleanupAfterFatal launches only the native abort subtest in a bounded child test process, sets the test-only SANDBOX_TEST_DELAYED_OPERATION_FATAL flag and intentionally triggers parent Fatal before release plus worker Fatal after release. The worker checks that its original hook is still installed, and a cleanup observer checks the independent completion signal before fixture cleanup. The child is expected to exit with test failure; the parent requires the two injected assertion markers and successful completion marker, rejecting unfinished-worker/hook-order/race diagnostics. This verifies completion even though the normal result channel is never sent.

## RED → GREEN evidence

All commands used the existing owned manual loopback fixture; no script lifecycle/global faults were invoked. Shell environment prefix for every native test command below:

```sh
TEST_ETCD_ENDPOINTS=http://127.0.0.1:50987,http://127.0.0.1:50989,http://127.0.0.1:50979 TEST_ETCD_FIXTURE_PROJECT= TEST_ETCD_CONTAINERS=
```

Fixture project `sandbox-etcd-state-dispatch-codex-20261006-a70c18d2`, server 3.6.15. Fixture helpers verified three distinct members in cluster f45703ebe16eae30. Commands were run in the shared workspace. Log paths below contain complete command output; all GREEN package outputs were `ok github.com/goairix/sandbox/internal/storage/state/etcd <duration>`.

| Unit | Command after environment prefix | Result / log |
| --- | --- | --- |
| I1 RED before production fix | `go test ./internal/storage/state/etcd -run 'TestFenceCheck' -count=1` | exit 1, 3.686s; six actual wire cases reported empty branch where one point required, and all 28 malformed response cases accepted old empty success. `/tmp/etcd-final-fix-i1-red.log` |
| I1 GREEN | `go test ./internal/storage/state/etcd -run 'TestFenceCheck\|TestRenewOperation\|TestRuntimePreparationReplayChecksServerFences\|TestOperationActiveCost' -count=1` | exit 0, 10.071s. `/tmp/etcd-final-fix-i1-green.log` |
| I1 race | same selection with `go test -race` | exit 0, 13.587s. `/tmp/etcd-final-fix-i1-race.log` |
| I2 RED before production fix | `go test ./internal/storage/state/etcd -run TestBeginOperationFailureCompletionEnvelope -count=1` | exit 1, 2.767s; 11 invalid shapes were accepted as conflict or misclassified. `/tmp/etcd-final-fix-i2-red.log` |
| I2 GREEN | `go test ./internal/storage/state/etcd -run 'TestBeginOperation' -count=1` | exit 0, 12.805s. `/tmp/etcd-final-fix-i2-green.log` |
| I2 race | same selection with `go test -race` | exit 0, 16.805s. `/tmp/etcd-final-fix-i2-race.log` |
| M1 RED before cleanup fix | `go test ./internal/storage/state/etcd -run '^TestDelayedOperationCleanupAfterFatal$' -count=1` | exit 1, 1.414s; actual `admission still running at fixture cleanup`, `hook restored while admission running`, and closing-client warnings. `/tmp/etcd-final-fix-m1-red.log` |
| M1 GREEN | `go test ./internal/storage/state/etcd -run 'TestDelayedOperationCleanupAfterFatal\|TestResolveOperationLateCompleteTransaction' -count=1` | exit 0, 33.118s; includes real 30-second natural original Lease expiry. `/tmp/etcd-final-fix-m1-green.log` |
| M1 race | same selection with `go test -race` | exit 0, 33.964s; subprocess reuses the race-instrumented test binary. `/tmp/etcd-final-fix-m1-race.log` |

In the Markdown table, escaped `\|` denotes the ordinary regex `|` supplied to the shell, not a literal backslash in the executed regex. The missing-receipt control was added after I2 RED as preservation coverage; it is not claimed as newly failing behavior. M1 RED intentionally induces assertion failures; those failures and its connection-closing warnings are evidence of the old cleanup defect, not an unexplained GREEN failure.

`go vet ./internal/storage/state/etcd` exited 0 after each functional unit. gofmt -l on each unit's affected files returned no output. `git diff --check` passed before each commit, and `git diff c3fdc6f6ee8062ceb75fdc1e0c206b51dd7d98f1..HEAD --check` passed after all three commits. No full package, whole repository or fresh owned fault gate was duplicated; the controller explicitly reserves those for the final source.

## Self-review and remaining concerns

Reviewed the committed range: seven source/test files, 406 insertions / 40 deletions. Public API signatures, operation receipt family, original operation Lease TTL/deadline/irreversible loss rules, permanent identity/restore/control fences, no-write replay behavior, original 24 creation comparisons, 64 operation and 256 KiB budgets are retained. Added test transport clients are fixture-scoped and cleaned up; production still uses the shared backend connection. I2 adds no resolver reads, runtime inference, permanent receipt, capability adoption or Lease resurrection. M1 restores hooks only after independent worker completion and boundedly cleans the original Lease.

The two existing plan modifications remain uncommitted and untouched by this worker:
- docs/superpowers/plans/2026-09-17-sentinel-kubernetes-129-compatibility.md
- docs/superpowers/plans/2026-10-06-etcd-operation-admission.md

No unresolved implementation ambiguity remains within this wave. The controller's final scoped review and fresh source gates remain pending. M2 remains an accepted nonblocking follow-up. This wave does not claim overall phases 1–5 completion, production migration, runtime terminal/drain guarantees, global scale/SLO, or removal of Redis. All whole-branch declined-to-judge follow-ups remain the controller's recorded overall requirements.


## Controller 后续 gate

已完成独立scoped review与最终source29c3cf6的owned/repo/vet/build验证，均通过；见本目录operation-admission-verification.md。上文“pending”保留为实现者报告原始时点，不表示当前仍待验证。
