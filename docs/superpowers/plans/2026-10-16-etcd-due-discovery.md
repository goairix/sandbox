# Due discovery implementation plan

Required workflow: subagent-driven-development; one new sole author and one new sole Task reviewer, no children. Existing feature checkout `codex/etcd-state-management`; immutable BASE f95d100f26ba6ba81f38ed2289090f36f0ca86f1. Spec: `docs/superpowers/specs/2026-10-16-etcd-due-discovery-design.md`.

## Task 1: Atomic enrolled expiry index, bounded worker pages and fenced expiry task admission

Deliver one coherent actual discovery-to-metadata-consumer chain. New focused due record/key/range/admission files within `internal/storage/state/etcd`; narrow enrolled publication protocol/budget/record compatibility and actual worker lifetime integration; focused behavioral tests, one new finite three-member Linux native test, operator description. Exact paths/interfaces frozen by Step1, not an open-ended package refactor.

- [ ] Step1 read-only preflight: exact protocol compatibility and 64-node accounting, canonical index, bounded page/cursor and budget owner, trusted expiry clock, existing destroy Stage integration; Root freezes concrete interfaces and conflict rulings.
- [ ] Step2 tests-only publication/index/compatibility RED; implement closed atomic publication slice, verify and immediately commit.
- [ ] Step3 tests-only bounded fixed-revision Range and real budget/lifetime RED; implement, verify and commit.
- [ ] Step4 tests-only Txn-fenced expired metadata-task admission RED; implement, verify and commit. No runtime side effects or hard-slot claim implied.
- [ ] Step5 one fresh finite real three-member chain, operator record and small commit. No old fixture artifact/matrix replay.
- [ ] Step6 whole Task review BASE→HEAD, same pair scoped fixes, tracked bounded acceptance and closure; continue actual Watch/cache/dirty/task worker and remaining production migration without stopping.

## Initial interface checks

| Producer→consumer | Constraint / potential conflict |
| --- | --- |
| enrolled publish→due candidate | Native v2 is full64; new closed protocol must preserve all54compares and old replay, not post-write index |
| candidate→page | strict partition/time identity, permanently stored discovery only; response bounded before RPC |
| Range share→actual native owner | original empty bucket and at-most-one actual charged RPC; cancellation/Close joins actual return |
| page cursor→next Range | fixed revision/window/origin, compaction invalidates rather than restart silently |
| candidate→destroy preparation | current control and exact trusted expiry plus worker/profile/session/partition/claim-share in actual commit |
| Stage→existing destroy consumer | preserve generic64budget and existing attempt/result/cleanup semantics; no physical permission from TaskReference |
| Task internal | tests must verify real atomic writes, fail/unknown/replay and actual fences, not only schema/API shape |

Ruling: scope combines atomic publication, bounded discovery and metadata task admission because isolated carrier-only work delays the user's real scheduling objective. Watch/cache and mounted physical execution follow this prerequisite; do not describe this Task as the full production scheduler. No re-approval is needed under the existing full migration authorization.
