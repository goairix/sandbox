# Supervisor workspace archive acceptance

## Task 1 accepted: transport APIs

Source `b5bdca25f22397dc24780797fa1ab2618c2f0b0c`, base `321e850`, exact eight transport paths, 610 insertions/11 deletions. Canonical finite archive/query routing, original sealed send/response mode, bounded frames and consumed authenticated EOF check, private archive-only raw EOF protection, and original-verifier full-session destination helpers are implemented. Real pinned mutual TLS covers expired admission context, mode isolation, authenticated close_notify, abrupt socket close, trailing data and bounded blocked reads.

Author reports final affected host race/vet/Linux arm64 CGO0 compile exit0. Root checked eleven retained artifact hashes, eight current/Git source hashes, exact committed paths/empty index and actual compiled behavior RED/final race GREEN. Vet/compile logs are empty; their exit status is recorded by the author, not independently reconstructed from empty logs. Race log SHA256 `e2dfab3cd84e4ef5b481a0bfaaf634b8576777e87c02e18ddddf26c3b36e6e26`; report SHA256 `30314e28d81c2c1aa99e62ae96b778a2a7a4e8eedf904fa5e445d79148bc430b`. Sole independent task review: spec PASS, quality APPROVED, zero Critical/Important/Minor findings; no duplicate suite runs.

Root accepts this transport slice. Go 1.25.6 normalizes abrupt TLS record-boundary EOF, so the approved private raw-read wrapper preserves truncation while leaving existing modes and optional adapter half-close behavior intact. Trusted adapters remain a seam. Host race/cross-compile are not Linux physical execution, Linux race or power-loss evidence. These APIs do not establish cumulative received bytes/TAR entries/digest, current backend ownership, remote request settlement or safe release.

Task 2 Supervisor/source/transport owner integration remains pending. Backend sender/receiver, production main wiring, partition scheduler, FUSE/collector, existing-sandbox capacity, deployment/recovery and Redis removal remain required. Main still uses Redis.
