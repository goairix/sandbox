# CCE Persistent etcd Validation Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Replace the disposable CCE test etcd backend with an isolated three-member PVC-backed backend, then prove sandbox state survives a member Pod recreation.

**Architecture:** Keep the existing `cce-test-etcd` StatefulSet and data untouched for rollback. Start `cce-pvc-etcd` on three independent `csi-disk` 10Gi PVCs with a fresh service, cluster token, and `/sandbox/cce-pvc-20261026/` prefix. Cut over only the `sandbox` Helm release in `sandbox-fuse-etcd` after confirming no active test sandbox; retain the old etcd until the new API and restart checks pass.

**Tech Stack:** Kubernetes StatefulSet/PVC, etcd 3.6.14, Helm, sandbox public HTTP API.

**Spec:** [Fresh etcd + FUSE deployment guide](../../deployment/etcd-fuse-fresh-install.md).

## Global Constraints

- Do not modify DS or `aiadp-sandbox-fuse`. Preserve CCE API key, MinIO and old etcd objects.
- Do not print Secret contents. Keep manifests and Helm values that include credentials out of Git.
- Stop if any active non-pool sandbox exists, any PVC is unbound, quorum is unhealthy, or rendered Helm resources differ unexpectedly.
- Revert the Helm release to revision 12 if cutover readiness or a public FUSE create fails; retain both etcd clusters until verified.

## Task 1: Establish a safe cutover baseline

- [ ] Record CCE Helm revision, API image/readiness, existing endpoints/prefix, active sandbox Pods and owner state.
- [ ] Confirm `csi-disk` supports the three intended nodes; capture a snapshot of the old test etcd before the backend change.
- [ ] Render the proposed Helm upgrade and review every changed resource; expect API backend settings and upgrade hooks, not unrelated network or storage changes.

## Task 2: Create the isolated persistent etcd cluster

- [ ] Install a headless Service and `cce-pvc-etcd` StatefulSet with three `10Gi` `csi-disk` claims, one member per node when schedulable, and the existing etcd image/probes/resources.
- [ ] Wait for all three claims Bound and members Ready; verify endpoint status and member list agree on one cluster ID and three members.
- [ ] Stop if placement, PVC attachment, or quorum fails; leave the original backend serving.

## Task 3: Cut over only the CCE test release

- [ ] Upgrade `sandbox` with the new three endpoints and fresh prefix while reusing all other installed values; check the pre-upgrade drain result.
- [ ] Verify API 2/2 Ready, pool refill, and a public FUSE create/read/write/delete to final 404.
- [ ] On failure, Helm rollback to revision 12 and verify the old API 2/2 Ready; do not delete either etcd cluster.

## Task 4: Prove persistent member recovery

- [ ] Create and hold a test FUSE sandbox with a workspace file; read it back through the public API.
- [ ] Recreate exactly one follower etcd Pod, then verify its PVC UID and member identity are retained and all three endpoints regain health.
- [ ] Verify the held sandbox ID, file bytes, and public DELETE to final 404; audit test Pod and owner residue.
- [ ] Record evidence and remaining limits. One-member PVC recovery is not whole-cluster restore or large-capacity acceptance.

## Review Focus

- Cloud disk claims must not use the existing release's PVCs.
- Backend cutover must not silently reuse old state or claim a data migration.
- A failed cutover must leave the old cluster and Helm rollback point intact.
- A member restart must test the same claim, not a newly provisioned disk.
