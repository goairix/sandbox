# Cold quota v3 manifest groundwork

## Result and boundary

The deployment package has a separate private, offline v3 parser returning
owned cleanup options and a fixed preparation inventory. This is configuration
groundwork for a future automatic quota consumer. Existing v1/v2 parsers,
mounted constructors and the worker CLI are unchanged and still reject v3;
there is no premature execution through the old v2 consumer.

The original input must explicitly contain version 3, automatic_cleanup=true,
all original mounted fields and preparation.quota_slots with exactly 256
non-null unsigned counts. Short arrays cannot silently acquire trailing zeroes.
Each count is at most 1000000 and the aggregate must be nonzero, using the
existing storage validator. Every partition reaching its limit is a valid
profile; this is a configuration limit, not proven runtime capacity.

The parser rejects duplicate/case-alias fields, unknown fields, missing or null
values, invalid UTF-8, trailing JSON and input above 65536 bytes. It accepts no
caller-supplied readiness, digest, extra restore epoch, quota partition or warm
inventory. Original-byte presence and strict shape checks precede a private
in-memory v2 validation projection; the returned cleanup version remains 3.

PreparationInventoryOptions.Validate forwards to the existing single storage
validator. Its nil receiver preserves legacy unconfigured-backend semantics.
It performs no RPC or metadata initialization and proves no inventory readiness.
Future production wiring must retain the existing ready/digest/restore fences.

## Verification

Tests cover complete, sparse and maximum profiles; all-zero/short/long/null/
negative/fractional/overflow counts; strict original and new fields; mode
isolation; legacy v1/v2 acceptance and refusal of new v3 fields. Clearing the
input after successful parsing preserves the owned result.

Root owns verification with GOMAXPROCS=2 and a workspace-private module cache.
All accepted epochs receive independent checks of 17 Go input categories,
source/tool archives, before/after/current hashes and artifacts.

- quota-v3-red-03: expected compile RED, only the missing planned parser;
  9086 inputs, 1089-byte log, SHA-256
  `559810f91caafce462cb9df43033d9e1436cc2a9c12e8c6643181f87a0c97448`.
- quota-v3-host-01: PASS; new profile/shape/bounds/isolation groups and the
  original entrypoint-version group, 9087 inputs; 6841-byte log, SHA-256
  `e7c287e37e3b5437efa9fe86dea89aebe16345ea6f4e983b6e87e0818c150a9e`.

- quota-v3-race-01: PASS; 9092 inputs; 13479-byte log, SHA-256
  `ddda08305c66516825c6f1b098b8bcc740be9ab714530454f430482abf7ff864`.
  Its selector also includes original local and automatic-local strict/bounds
  tests, which the first host selector alone did not include.

- quota-v3-vet-01: PASS; Linux arm64, 9178 inputs and empty output.

Root completed each accepted epoch's complete input/source/tool/archive/current
audit. The unique task reviewer received final source, this report and the
additional race/Linux evidence before submission. Final Spec and Quality
review passed with C/I/M = 0/0/0.

## Environment interruption and code quality

The first RED attempt encountered disappearing files in the shared Library
module cache, rather than the expected missing API. A subsequent dependency
capture was interrupted before a test command ran. Neither is counted as normal
API RED or acceptance. Their original artifacts remain. Root restored a
workspace-private module cache; 6179 relevant module inputs exactly matched the
earlier source hashes before accepted verification continued. HTTP/1 dependency
fetching is a verification-environment choice, not runtime performance evidence.

Read-only gofmt -d and git diff --check were clean. Manual review covers comments,
shape-check ordering, logical sections and package responsibilities. Deployment
owns wire interpretation; storage retains the single inventory rule source,
without new reverse dependencies or execution authority. Automatic settlement,
worker transport, terminal deletion gates, capacity and production switch remain
open.
