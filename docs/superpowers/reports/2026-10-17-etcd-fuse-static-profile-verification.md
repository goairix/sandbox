# Inactive enrolled FUSE static profile verification

The new `kubecontract` contract renders and compares one fixed FUSE PodSpec against retained original input. It does not enable an etcd FUSE service, enroll a helper, validate a running Pod, grant mount/flush/cleanup authority, or change a production profile selector. Existing plain validation and selectors remain unchanged.

## Scope and boundaries

- `internal/kubecontract/enrolled_fuse.go`: inactive `sandbox-enrolled-fuse/v1`, immutable image-pair/AppArmor-name digest, public input and wire limits, structural comparison against original intent.
- `internal/kubecontract/enrolled_fuse_spec.go`: reuses the existing plain renderer for the protected launcher/base template, then adds only the fixed helper/volumes/propagation/probes. Returned state owns its backing data.
- `internal/kubecontract/enrolled_fuse_test.go`: six new groups, 102 terminal leaves covering literal protected shape, observed mutations, invalid original inputs, original-intent mismatch, ownership, immutable image/LSM binding and unchanged plain rejection.
- The privileged helper is a trusted system component. Pinning its AppArmor policy name does not prove actual kernel policy enforcement or confinement. No helper-run/cache/device mount enters the launcher in this inactive template.
- User UID/GID is fixed to 1000/1000 to match the current compiled s3fs arguments. Only the existing HTTPS MinIO profile and system CA trust are represented; optional CA/Secret mounts and arbitrary storage profiles are rejected.
- Public launcher configuration uses the existing strict 16 KiB format; bootstrap JSON is capped at 4 KiB and complete rendered PodSpec JSON at 64 KiB. CPU/memory requests and limits, workspace/tmp/cache sizes and helper run/control volumes are explicit.
- Native helper-to-Pod/kernel binding, enrolled attempt-1 ready evidence, original flush-and-unmount workflow and both process-owner cleanup remain separate prerequisites. The best-effort preStop hook is not terminal evidence.

## Executed checks

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

## Frozen product inputs

- `enrolled_fuse.go` (168 lines): `e9948001eb779f71f32fa7d1d39fcf4351ccbefa8218e4de3d4fef31d81a26b3`.
- `enrolled_fuse_spec.go` (88 lines): `930a6f0e3506961f76bcb1addd63cd8fdaba3baad399bc9704a11ad7f32901ed`.
- `enrolled_fuse_test.go` (423 lines): `e702bd49a8c528474f74b911176a62f493c02dca23dd2069c1f667795c70a9e4`.
- Linux test runner: `7429841893ee0d711904cd61ad001400e098b346bdd59b8c71dd2675e19e005c`.

All three new Go files have empty gofmt-d output. Review covered exported API constraints, fixed privilege/resource/mount boundaries, separation from actual authority, logical sections and package responsibility. This remains a structural prerequisite, not completed collector or production FUSE delivery.
