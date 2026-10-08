# Base cleanup and quiesce fixes integration

User explicitly requested original commits `52b4d4f6d031a45b6e75bcb76c89b7a53adde5d3` and `d543cf32bb4cb3f9fae183f558de6fa8d7e8993d` from feat/workspace-fuse-mount. Incoming range after merge-base `b99b823` contains exactly those two commits. Merge `7bca73a4589a07d7c709e9a98d59c38bdfad68d2` has parents `f522c259b6be6a4e8890eb91642c5ec9b117b8f2` and `d543cf32bb4cb3f9fae183f558de6fa8d7e8993d`; both original fixes verified as ancestors. No conflicts or additional resolution source changes.

Integration adds bounded workspace cleanup/controller waits, retained quiesce diagnostics, confirmed stop-failure retry versus ambiguous poison, and proc thread-count handling that does not discard live sibling threads. Exact18 incoming paths merged/staged; unrelated dirty Sentinel document excluded and preserved. Native etcd source unchanged directly.

Fresh merge-candidate validation with Go1.25.6: `go test -race -count=1 ./internal/runtime/kubernetes ./internal/sandbox ./internal/workspaceprobe` exit0, package times10.913s/15.283s/4.430s. `go vet` for the same three packages exit0. Linuxarm64CGO0 `go test -c` for each package exit0; retained binaries cross-compiled, not executed. These are affected-package checks, not provider, production, Linux kernel or capacity acceptance. Root observed commands/exits directly; no existing accepted native etcd selectors replayed.

Empty index before merge; commit gate fcntl LOCK_EX verified exact incoming staged18path union, no unresolved index, staged diff check and expected MERGE_HEAD/current HEAD. Post-commit index empty. No push, deployment, Redis removal or trust changes. Resume original registered archive consumer and per-wire settlement work from merged HEAD.
