# Kubernetes AppArmor Intent Normalization Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Allow prepared FUSE Pods created by Kubernetes 1.30+ to pass the security-intent check when the apiserver adds an AppArmor field exactly equivalent to the requested legacy annotation.

**Architecture:** Keep rendering the legacy per-container annotation for Kubernetes 1.29 compatibility. Normalize only the apiserver-generated, confined `Localhost` AppArmor field on deep-copied Pods immediately before the existing semantic comparison, leaving all other security-context differences visible and rejected.

**Tech Stack:** Go, Kubernetes core/v1 API types, Kubernetes semantic equality, client-go fake admission reactors, Testify.

---

## File Structure

- Modify `internal/runtime/kubernetes/runtime_test.go`: reproduce Kubernetes AppArmor annotation-to-field synchronization and cover accepted and rejected profiles.
- Modify `internal/runtime/kubernetes/runtime.go`: implement bounded AppArmor admission normalization in the prepared-Pod intent comparison.

### Task 1: Reproduce AppArmor Admission Synchronization

**Files:**
- Test: `internal/runtime/kubernetes/runtime_test.go`

- [ ] **Step 1: Add failing intent and integration tests**

Add a helper that models the Kubernetes 1.30+ conversion:

```go
func syncMounterAppArmorField(pod *corev1.Pod, profileType corev1.AppArmorProfileType, profile string) {
	security := pod.Spec.InitContainers[0].SecurityContext
	security.AppArmorProfile = &corev1.AppArmorProfile{Type: profileType}
	if profile != "" {
		security.AppArmorProfile.LocalhostProfile = &profile
	}
}
```

Extend `TestPreparedPodIntentRejectsSecurityExpansions` to verify that the
equivalent `Localhost/sandbox-fuse` field matches without mutating either input,
while `Localhost/other-profile` and `Unconfined` remain rejected. Add
`TestPrepareSandboxAcceptsEquivalentAppArmorFieldFromAdmission` with a fake Pod
create reactor that calls the helper with `Localhost/sandbox-fuse`, then assert
that `PrepareSandbox` returns no error.

- [ ] **Step 2: Run the focused tests and verify RED**

Run:

```bash
go test ./internal/runtime/kubernetes -run 'Test(PreparedPodIntentRejectsSecurityExpansions|PrepareSandboxAcceptsEquivalentAppArmorFieldFromAdmission)$' -count=1
```

Expected: FAIL because the existing intent matcher reports
`spec.InitContainers[0].SecurityContext` for the equivalent structured field.

- [ ] **Step 3: Commit the failing regression tests**

```bash
git add internal/runtime/kubernetes/runtime_test.go
git commit -m "test: reproduce kubernetes apparmor admission sync"
```

### Task 2: Normalize the Equivalent AppArmor Field

**Files:**
- Modify: `internal/runtime/kubernetes/runtime.go:1433-1509`
- Test: `internal/runtime/kubernetes/runtime_test.go`

- [ ] **Step 1: Add bounded normalization**

Add `normalizeAppArmorAdmission(current, desired *corev1.Pod)` and call it on
the deep copies in both `preparedPodIntentMatches` and
`preparedPodIntentMismatchReason`. The helper must visit regular, init, and
ephemeral containers in matching positions and clear the current structured
field only when it exactly matches the desired confined annotation:

```go
func normalizeAppArmorAdmission(current, desired *corev1.Pod) {
	normalize := func(name string, currentSecurity, desiredSecurity *corev1.SecurityContext) {
		if desiredSecurity == nil || desiredSecurity.AppArmorProfile != nil ||
			currentSecurity == nil || currentSecurity.AppArmorProfile == nil {
			return
		}
		annotation := desired.Annotations[corev1.DeprecatedAppArmorBetaContainerAnnotationKeyPrefix+name]
		if !strings.HasPrefix(annotation, corev1.DeprecatedAppArmorBetaProfileNamePrefix) {
			return
		}
		profile := strings.TrimPrefix(annotation, corev1.DeprecatedAppArmorBetaProfileNamePrefix)
		if validateLSMProfile(profile) != nil {
			return
		}
		expected := &corev1.AppArmorProfile{
			Type:             corev1.AppArmorProfileTypeLocalhost,
			LocalhostProfile: &profile,
		}
		if apiequality.Semantic.DeepEqual(currentSecurity.AppArmorProfile, expected) {
			currentSecurity.AppArmorProfile = nil
		}
	}
	for index := 0; index < len(current.Spec.Containers) && index < len(desired.Spec.Containers); index++ {
		if current.Spec.Containers[index].Name == desired.Spec.Containers[index].Name {
			normalize(current.Spec.Containers[index].Name, current.Spec.Containers[index].SecurityContext, desired.Spec.Containers[index].SecurityContext)
		}
	}
	for index := 0; index < len(current.Spec.InitContainers) && index < len(desired.Spec.InitContainers); index++ {
		if current.Spec.InitContainers[index].Name == desired.Spec.InitContainers[index].Name {
			normalize(current.Spec.InitContainers[index].Name, current.Spec.InitContainers[index].SecurityContext, desired.Spec.InitContainers[index].SecurityContext)
		}
	}
	for index := 0; index < len(current.Spec.EphemeralContainers) && index < len(desired.Spec.EphemeralContainers); index++ {
		if current.Spec.EphemeralContainers[index].Name == desired.Spec.EphemeralContainers[index].Name {
			normalize(current.Spec.EphemeralContainers[index].Name, current.Spec.EphemeralContainers[index].SecurityContext, desired.Spec.EphemeralContainers[index].SecurityContext)
		}
	}
}
```

Do not accept `RuntimeDefault`, `Unconfined`, empty localhost profiles, or any
other security-context mutation.

- [ ] **Step 2: Run focused tests and verify GREEN**

Run:

```bash
go test ./internal/runtime/kubernetes -run 'Test(PreparedPodIntentRejectsSecurityExpansions|PrepareSandboxAcceptsEquivalentAppArmorFieldFromAdmission)$' -count=1
```

Expected: PASS.

- [ ] **Step 3: Format and run the package test suite**

Run:

```bash
gofmt -w internal/runtime/kubernetes/runtime.go internal/runtime/kubernetes/runtime_test.go
go test -race ./internal/runtime/kubernetes -count=1
```

Expected: PASS with no failures.

- [ ] **Step 4: Review the exact diff and commit the fix**

```bash
git diff --check
git diff -- internal/runtime/kubernetes/runtime.go internal/runtime/kubernetes/runtime_test.go
git add internal/runtime/kubernetes/runtime.go internal/runtime/kubernetes/runtime_test.go
git commit -m "fix: accept kubernetes apparmor admission sync"
```
