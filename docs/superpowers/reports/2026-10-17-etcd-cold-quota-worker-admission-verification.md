# Cold quota worker paid admission

## Result and scope

A private admission method now reserves the original worker's Range and Claim
credits atomically for a future quota-return continuation. It returns the
original registered workerCall and complete W24 comparisons. It sends no RPC,
creates no lease, returns no quota and enables no automatic consumer.

The original enrolled-publication owner is checked by Backend and registry
pointer membership and remains the only context source. Worker session,
partition and both budget shares must be original registered members of the
same session, with fixed Range/Claim roles. Copies and diagnostic references
cannot reconstruct admission authority.

The existing archiveLife.mu protects membership, release/live/context/deadline/
busy checks, comparisons, both buckets and original call registration. Trial
charges use local copies of the original buckets and one captured time. If
either lacks credit, neither bucket changes. No credit is refunded after a
successful admission, even when cancellation follows.

The original beginWorkerCallLocked is the final fallible step. Only after it
succeeds are both bucket values published, charging 262144 Range bytes and one
Claim credit. Its original session cancellation and finish lifecycle are reused.
An already expired session is refused without marking it lost; if the deadline
crosses inside the final original registration, existing s.live invalidation is
preserved without publishing a charge or leaving an operation.

W24 preserves identity/restore plus all four comparisons for profile, session,
partition, Range and Claim. The caller must keep the enrolled owner alive,
compose these fences into the actual bounded transaction and join native calls
before finish. Transaction composition and authenticated quota history are
separate work; admission alone proves neither.

## Verification

The host model creates actual original session/partition/share/owner objects.
Tests install earned bucket balances and freeze earning without sleeps. They
cover both insufficient-credit cases, 33 invalid bindings/states, fully funded
foreign-session refusal, complete W24, concurrent admission charging once,
both cancellation sources, no refund and session Release waiting for finish.
These are local admission/lifecycle tests, not physical cleanup evidence.

Root uses GOMAXPROCS=2, private caches and independent checks of all 17 Go input
categories, source/tool archives, before/after/current hashes and artifacts.

- quota-admission-red-01: expected compile RED only for the missing approved
  method; 9083 inputs, 387-byte log, SHA-256
  `f797003e58765a5afbb1b12b43ff28b4a1c488de603526b1707d8a8816da095d`.
- quota-admission-host-01: PASS; 9084 inputs, 7698-byte log, SHA-256
  `652ee9b811d841841ef327b23040947b6533e71d2545e40788d8354f30df2d7d`.

- quota-admission-race-01: PASS; 9089 inputs, 7698-byte log, SHA-256
  `122f54883ffe1633b6f7523bd19aa583b02816145a9a7b738484191f7d981879`.

- quota-admission-vet-01: PASS; Linux arm64, 9178 inputs and empty output.

Root completed each accepted epoch's complete input/source/tool/archive/current
audit. The unique task reviewer received the final source, this report and the
additional race/Linux evidence before submission. Final Spec and Quality
review passed with C/I/M = 0/0/0.
All source uses apply_patch; read-only gofmt -d and git diff --check were clean.
Manual and independent static review check comments, logical stages, original
lock ownership and reuse of the existing call lifecycle. The code remains in
etcd because those private concurrency invariants are shared; it exports no new
capability and changes no dependency direction.

## Remaining integration

The worker still needs paid metadata phases, exact private transaction/base
composition, bounded Grant/guard/commit/Revoke envelopes and native retry/send
verification, retained-memory accounting, positive cold-origin recognition,
automatic settlement and terminal pending gates. No large-population capacity,
physical worker, Kubernetes, FUSE or Redis-default switch is claimed here.
