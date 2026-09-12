# Kubernetes Shared Ordinary Pool Implementation Plan

1. Add failing ordinary-pool tests for two managers sharing one runtime and
   atomic store: global warm-up, cross-replica acquire, concurrent claim, stop,
   and release drain.
2. Add a durable shared-pool record/coordinator using `state.AtomicStore`, a
   scoped spec fingerprint, CAS claims, and a renewable refill lock.
3. Route `Pool` warm-up, acquire, refill, size, stop, reconciliation, and
   release drain through the shared coordinator when configured; retain the
   local implementation otherwise.
4. Replace Kubernetes startup's unconditional ordinary-pool deletion with
   durable reconciliation after session restoration.
5. Wire the Redis atomic store and Kubernetes namespace scope from the binary.
6. Run focused race tests, the complete Go test suite, static analysis, Helm
   rendering checks, and then verify the new image against each API replica in
   the target cluster.
