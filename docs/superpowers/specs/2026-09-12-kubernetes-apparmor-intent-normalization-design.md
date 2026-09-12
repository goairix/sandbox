# Kubernetes AppArmor Intent Normalization Design

## Problem

Prepared Kubernetes FUSE Pods target Kubernetes 1.29 and newer. To preserve
1.29 compatibility, `workspace-mounter` selects its confined AppArmor profile
through the deprecated per-container annotation instead of the structured
`securityContext.appArmorProfile` field.

Kubernetes 1.30 and newer synchronize a valid AppArmor annotation into the
equivalent structured field during Pod creation. The successful Create response
therefore differs from the submitted Pod at
`spec.initContainers[0].securityContext.appArmorProfile`. The prepared-Pod
security intent check currently compares the complete Pod spec and rejects this
safe server normalization as a security-contract mutation.

## Requirements

- Keep Kubernetes 1.29 support and continue rendering the AppArmor annotation.
- Accept the structured AppArmor field only when Kubernetes derived it from the
  requested annotation without changing its meaning.
- Continue rejecting all actual security-context mutations, including a
  different profile, `Unconfined`, `RuntimeDefault`, a missing localhost name,
  or changes to unrelated security fields.
- Apply the same normalization to successful Create responses and ambiguous
  Create readbacks through the shared intent matcher.
- Preserve actionable mismatch reporting for rejected security contexts.

## Design

Before comparing the current and desired Pod specs, normalize one bounded
Kubernetes admission mutation. For each desired regular, init, and ephemeral
container:

1. Read the desired Pod annotation for that exact container name.
2. Require the desired container's structured AppArmor field to be absent.
3. Convert only a valid `localhost/<profile>` annotation to its expected
   structured representation.
4. If the current container's structured field is semantically equal to that
   expected representation, clear it from the current comparison copy.

The original Pod objects remain unchanged. After this normalization, the
existing semantic comparison remains authoritative. Although the present FUSE
Pod uses one restartable init container, applying the helper across all
container kinds makes the normalization match Kubernetes's own Pod-wide
annotation synchronization semantics without weakening any other field.

The normalization deliberately does not accept `unconfined` or
`runtime/default`. This runtime only permits a validated confined localhost
profile, so accepting broader profiles would expand the security contract.

## Testing

Add regression coverage proving that:

- a Kubernetes 1.30+ style Create response with the equivalent localhost
  `AppArmorProfile` matches the requested Pod;
- `PrepareSandbox` succeeds when a fake admission reactor performs that
  conversion;
- a different localhost profile and an unconfined profile are rejected;
- the input Pod objects are not mutated by comparison;
- existing prepared-Pod security-expansion tests continue to pass.

Run the focused Kubernetes runtime tests, then the complete Kubernetes runtime
package tests with the race detector.
