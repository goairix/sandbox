# Inactive enrolled FUSE static profile verification

The new `kubecontract` contract renders and compares one fixed FUSE PodSpec against retained original input. It does not enable an etcd FUSE service, enroll a helper, validate a running Pod, grant mount/flush/cleanup authority, or change a production profile selector. Existing plain validation and selectors remain unchanged.

## Scope and boundaries

- `internal/kubecontract/enrolled_fuse.go`: inactive `sandbox-enrolled-fuse/v1`, immutable image-pair/AppArmor-name digest, public input and wire limits, structural comparison against original intent.
- `internal/kubecontract/enrolled_fuse_spec.go`: reuses the existing plain renderer for the protected launcher/base template, then adds only the fixed helper/volumes/propagation/probes. Returned state owns its backing data.
- `internal/kubecontract/enrolled_fuse_test.go`: six new groups, 104 terminal leaves after review fix R1, covering literal protected shape, observed mutations, invalid original inputs, original-intent mismatch, ownership, immutable image/LSM binding and unchanged plain rejection.
- The privileged helper is a trusted system component. Pinning its AppArmor policy name does not prove actual kernel policy enforcement or confinement. No helper-run/cache/device mount enters the launcher in this inactive template.
- User UID/GID is fixed to 1000/1000 to match the current compiled s3fs arguments. Only the existing HTTPS MinIO profile and system CA trust are represented; optional CA/Secret mounts and arbitrary storage profiles are rejected.
- Public launcher configuration uses the existing strict 16 KiB format; bootstrap JSON is capped at 4 KiB and complete rendered PodSpec JSON at 64 KiB. CPU/memory requests and limits, workspace/tmp/cache sizes and helper run/control volumes are explicit.
- Native helper-to-Pod/kernel binding, enrolled attempt-1 ready evidence, original flush-and-unmount workflow and both process-owner cleanup remain separate prerequisites. The best-effort preStop hook is not terminal evidence.

## Initial executed checks (before review fix R1)

All epochs used the isolated worktree and Root-provisioned `.superpowers/sdd/2026-10-17-fuse-resident-readiness/check.sh`. Each completed epoch passed its full independent audit before the next Go epoch or source mutation. The audits checked all 17 Go source fields, raw full go-list JSON, pre/post/current source and tool hashes, and every archived source/tool member.

| Epoch | Mode and selector (`./internal/kubecontract`) | Result | Actual source inputs | Raw log SHA256 |
| --- | --- | --- | --- | --- |
| `static-fuse-red-01` | host, `^TestEnrolledFUSEStatic.*$` | exit 1: only approved missing API/type compilation failures; retained | 7003 | `09f8372ef58c50eeef549db3d28c2e6161e29978a94153eadd8b9760cbf1a9d8` |
| `static-fuse-host-01` | host, `^Test.*$` | exit 0; entire package, 12 groups/166 terminal leaves, 1.301s | 7005 | `8c23112ac3316a889e66de04fe67fcc96fd9994098319f76fef4e5c75fcc4985` |
| `static-fuse-race-01` | race, `^Test(EnrolledFUSEStatic.*\|EnrolledProfile.*)$` | exit 0; 10 groups/146 terminal leaves, 2.390s | 7010 | `b45df36d4fb0ee60b82920aa7985573cf1f49fa4bc4a86b692f793386a5a1063` |
| `static-fuse-vet-01` | Linux arm64 vet | exit 0, empty log | 7038 | `e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855` |
| `static-fuse-build-01` | Linux arm64 test runner build | exit 0, empty log | 7038 | `e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855` |

The race epoch used `-race` for both go-list and test. Successful host/race runs had no SKIP, race warning or test failure. Linux vet/build are cross-platform compilation/static checks; the Linux binary was not executed by this agent. No Docker, real FUSE mount, Kubernetes admission or object-storage flush test was performed for this static slice.

The runner pinned Go 1.25.6 (`GOTOOLCHAIN=local`), `GOMAXPROCS=2`, its private OWN/go-cache, the established isolated module cache and `GOPROXY=https://goproxy.cn,direct`; host/race were darwin/arm64 CGO=1 and vet/build linux/arm64 CGO=0. Exact command/environment and raw exits are retained per epoch. Runner SHA256: `d7178cbd2fbac40097b08190f8347f20d0f970474d74045e735bca2acff69b11`.

## Initial committed source inputs (`f63803d`, before R1)

- `enrolled_fuse.go` (168 lines): `e9948001eb779f71f32fa7d1d39fcf4351ccbefa8218e4de3d4fef31d81a26b3`.
- `enrolled_fuse_spec.go` (88 lines): `930a6f0e3506961f76bcb1addd63cd8fdaba3baad399bc9704a11ad7f32901ed`.
- `enrolled_fuse_test.go` (423 lines): `e702bd49a8c528474f74b911176a62f493c02dca23dd2069c1f667795c70a9e4`.
- Linux test runner: `7429841893ee0d711904cd61ad001400e098b346bdd59b8c71dd2675e19e005c`.

All three new Go files have empty gofmt-d output. Review covered exported API constraints, fixed privilege/resource/mount boundaries, separation from actual authority, logical sections and package responsibility. This remains a structural prerequisite, not completed collector or production FUSE delivery.

## Review fix R1: preserve endpoint encoding before JSON

Independent review identified that URL parsing accepts a hostname containing Unicode control U+0085 and a raw invalid UTF-8 byte. The actual mounter rejects these inputs, while JSON encoding could replace an invalid byte with U+FFFD. Two new invalid-original cases reproduced acceptance before the fix. The only product change rejects invalid UTF-8 and Unicode control characters in Endpoint before parsing or encoding; it does not import the mounter package or change any other profile behavior.

The initial epochs and archives above remain historical evidence of their original source. All fresh R1 epochs again passed full source17/raw-go-list/prepost/current/tool/archive audits before subsequent Go or mutation. The focused RED was audited before the product fix; its two assertion failures are retained.

| R1 epoch | Mode and selector (`./internal/kubecontract`) | Result | Actual source inputs | Raw log SHA256 |
| --- | --- | --- | --- | --- |
| `static-fuse-r1-red-01` | host, `^TestEnrolledFUSEStaticRejectsInvalidOriginal/endpoint_(invalid_utf8\|unicode_control)$` | exit 1: both cases expected error but got nil | 7005 | `0baa8270d096cabf7ba4f5150ee87468c30af98513c1d4aa1db253c68a0ce76d` |
| `static-fuse-r1-host-01` | host, `^Test.*$` | exit 0; entire package, 12 groups/168 terminal leaves, 1.667s | 7005 | `64ef8639bd2a34f01c0a58ddb1872f29bc807aa0d4d52822321d1e705171985a` |
| `static-fuse-r1-race-01` | race, `^Test(EnrolledFUSEStatic.*\|EnrolledProfile.*)$` | exit 0; 10 groups/148 terminal leaves, 2.863s | 7010 | `0ba5ee9ece3a5c684b1a3183197c0b553dea31b46984c7bf2ab0a841125e5a1a` |
| `static-fuse-r1-vet-01` | Linux arm64 vet | exit 0, empty log | 7038 | `e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855` |
| `static-fuse-r1-build-01` | Linux arm64 test runner build | exit 0, empty log | 7038 | `e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855` |

The runner/environment and verification scope remain unchanged; `-race` applied to both list and test. Successful R1 host/race logs contain no SKIP, race warning or test failure. No actual Linux binary execution, FUSE, Kubernetes or object-storage test is claimed.

Current frozen R1 source: `enrolled_fuse.go` (180 lines) SHA256 `c401189169cbc0578a46bc6cb2ce9bffa7aa73f62cf3317c9f02cf673b5c1dbc`; `enrolled_fuse_test.go` (429 lines) `cd107f13093e783243dbe0796e03026c0d23ba1cd1afba1748797dda1040f445`; unchanged renderer `930a6f0e3506961f76bcb1addd63cd8fdaba3baad399bc9704a11ad7f32901ed`. R1 Linux runner SHA256: `58e257e0bf7cf7d3513e38d1ef0a4c939c30817254ca3d7b6f1ed9002bf5c133`. All three Go files still have empty gofmt-d output; R1 adds only two standard-library imports, the endpoint check/comment and two rejection cases.
