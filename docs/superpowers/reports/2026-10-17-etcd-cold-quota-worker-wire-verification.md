# Cold quota worker wire groundwork

## Scope

This tests-only slice measures the existing public quota protocol before adding
an automatic worker adapter. It changes no production code or admission limits.
Passing it does not implement automatic quota return or authorize pending-row
deletion. The public continuation still performs its post-commit reads before
deferred Revoke; a paid worker continuation needs a separate fixed lifecycle.

The sizing fixture derives canonical bounds from the actual record validators
and production request builders. Complex publication/release/return/journal
records retain their 8192/8192/16384/4096-byte codec caps. Namespace, UUID,
digest, numeric and ASCII segment bounds are independently maximized. Keys are
then widened to 1024 bytes for sizing only, after digest preparation. Widened
requests are never dispatched or used to recalculate an authority digest.

## Results for the existing public body

| Projection | Nodes | Request bytes | Success bytes | Failure bytes |
| --- | ---: | ---: | ---: | ---: |
| Fresh journal | 34 | 64384 | 96 | 11745 |
| Replacement journal | 41 | 76255 | 96 | 11745 |
| Stage guard | 9 | 14575 | 96 | 6523 |
| Refund commit | 42 | 89111 | 243 | 10844 |
| Resolve | 9 | 14565 | 96 | 8683 |

Each request fits 128 KiB and every projection stays within 64 nodes. The
Grant/guard/commit/Revoke cohort's worst canonical success prefix followed by
failure and cleanup totals 114748 bidirectional bytes, below 262144. Metadata
cohorts separately verify all target-call RPCs, rather than counting fixture
initialization. The largest projected metadata cohort is 203881 bidirectional
bytes. These are protobuf dimensions, not measured network throughput.

Model traces preserve journal→Grant→guard→refund→return-read→receipt-read→Revoke
order. Resolve uses two or three original-reference RPCs with no new Grant or
Revoke. The history in this model is already decoded; these traces do not prove
public history authentication, native execution or physical release.

## Deliberate counterexamples and remaining adapter work

- Real worker comparisons contain W20; adding the other original budget share
  produces W24. Blindly appending W24 to refund's 42 nodes gives 66, above 64.
  This passing counterexample prevents claiming that public-body dimensions
  alone prove a complete worker adapter budget. A future private composition
  must preserve all fences while handling the duplicated base identity checks.
- LeaseGrantResponse.Error has no schema length cap. A constructed 262144-byte
  Error produces a 262213-byte response. The counterexample test passes by
  exposing this gap; its RED log label is not a failing Go test. Actual bounded
  transport envelopes, retries and whole-cohort accounting remain unimplemented.
- No worker credit is consumed by these tests. Atomic dual-budget payment,
  original workerCall and enrolled-owner lifetime, finite metadata phases,
  encoded/decoded retained-memory bounds, automatic terminal gates and private
  continuation transport remain required before automatic quota acceptance.

## Verification record

Root executed the checks with GOMAXPROCS=2 and private build caches. The first
host run failed because the fixture's two initialization transactions were
included in a target-call assertion. Its evidence was fully audited before the
same author changed the observation boundary to the complete request[start:]
increment. The assertion still requires exactly one target RPC.

- quota-worker-wire-host-02: PASS; 9082 input files; raw 17854 bytes, SHA-256
  `480aa9127596fab4d648d145029ba2df434b5161ec4b9bb710085e01ef2d1dc1`.
- quota-worker-wire-race-01: PASS; 9087 inputs; raw 17854 bytes, SHA-256
  `6421408a060d3ad4b854f624af72facbcfeebb1ecef87900c0e2f65528b89856`.

Both accepted epochs passed independent checks of all 17 Go input categories,
source/tool archives, before/after/current hashes and artifacts. Linux vet-01
returned zero, but its current-input audit found files missing from the shared
module cache. Its archive and before/after evidence were verified; it is not
accepted as completed validation. A replacement check uses a workspace-local
module cache. Dependency-wait preludes were interrupted before any test/vet
command ran; their partial capture files remain, without a fabricated result.
The replacement module cache's 6179 relevant module inputs exactly match the
earlier manifest hashes. Download transport uses HTTP/1 to avoid stalled
dependency fetches; this is not a runtime performance measurement.

quota-worker-wire-vet-03 passed Linux arm64 vet with 9177 input files and empty
output. Root completed all 17 input categories, source/tool archives,
before/after/current hashes and artifact checks against the private cache.
The earlier environment-invalid vet is not substituted for this result.

Manual source review covers comments, logical sections and package boundaries.
The tests remain with the private etcd protocol they inspect, without exporting
test authority or changing dependency directions. No capacity, Kubernetes,
automatic worker, Redis removal or production switch is claimed.
