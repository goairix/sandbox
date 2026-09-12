# Kubernetes Metadata Prefix Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Replace the environment-specific `sandbox.huaxisy.com` Kubernetes metadata namespace with stable `goairix.github.io/sandbox-*` annotation and finalizer names for fresh Helm installations.

**Architecture:** Add one small Go package that owns the Kubernetes metadata contract and keep package-local aliases where existing code benefits from short names. Add one Helm helper for the DNS prefix and render every Chart key from that helper. This is a clean cutover: new code neither reads nor writes the old prefix.

**Tech Stack:** Go 1.25, Kubernetes `apimachinery`/`client-go`, Helm templates, Bash rendering tests.

---

### Task 1: Define the Go metadata contract

**Files:**
- Create: `internal/kubecontract/metadata.go`
- Create: `internal/kubecontract/metadata_test.go`

- [x] **Step 1: Write the failing contract test**

Create `internal/kubecontract/metadata_test.go` with exact-value assertions and Kubernetes qualified-name validation:

```go
package kubecontract

import (
	"testing"

	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/util/validation"
)

func TestMetadataNamesUseProjectNamespace(t *testing.T) {
	tests := map[string]string{
		"backend fingerprint": BackendFingerprintAnnotation,
		"drain protocol":      DrainProtocolAnnotation,
		"cleanup protocol":    CleanupProtocolAnnotation,
		"FUSE cleanup":        FUSERuntimeCleanupFinalizer,
	}
	want := map[string]string{
		"backend fingerprint": "goairix.github.io/sandbox-backend-fingerprint",
		"drain protocol":      "goairix.github.io/sandbox-drain-protocol",
		"cleanup protocol":    "goairix.github.io/sandbox-cleanup-protocol",
		"FUSE cleanup":        "goairix.github.io/sandbox-fuse-runtime-cleanup",
	}

	for name, value := range tests {
		t.Run(name, func(t *testing.T) {
			require.Equal(t, want[name], value)
			require.Empty(t, validation.IsQualifiedName(value))
		})
	}
}
```

- [x] **Step 2: Run the test to verify it fails**

Run: `go test ./internal/kubecontract -count=1`

Expected: FAIL because package constants and `metadata.go` do not exist.

- [x] **Step 3: Implement the contract constants**

Create `internal/kubecontract/metadata.go`:

```go
package kubecontract

const (
	MetadataPrefix = "goairix.github.io"

	BackendFingerprintAnnotation = MetadataPrefix + "/sandbox-backend-fingerprint"
	DrainProtocolAnnotation      = MetadataPrefix + "/sandbox-drain-protocol"
	CleanupProtocolAnnotation    = MetadataPrefix + "/sandbox-cleanup-protocol"
	FUSERuntimeCleanupFinalizer  = MetadataPrefix + "/sandbox-fuse-runtime-cleanup"
)
```

- [x] **Step 4: Run the contract test**

Run: `go test ./internal/kubecontract -count=1`

Expected: PASS.

- [x] **Step 5: Commit the contract**

```bash
git add internal/kubecontract/metadata.go internal/kubecontract/metadata_test.go
git commit -m "feat: define kubernetes metadata contract"
```

### Task 2: Move Go consumers to the project namespace

**Files:**
- Modify: `cmd/sandbox/drain.go`
- Modify: `cmd/sandbox/drain_test.go`
- Modify: `internal/runtime/kubernetes/pod.go`
- Modify: `internal/runtime/kubernetes/runtime_test.go`

- [x] **Step 1: Add literal consumer assertions**

Add this test to `cmd/sandbox/drain_test.go`:

```go
func TestDrainMetadataAnnotationsUseProjectNamespace(t *testing.T) {
	require.Equal(t, "goairix.github.io/sandbox-backend-fingerprint", backendFingerprintAnnotation)
	require.Equal(t, "goairix.github.io/sandbox-drain-protocol", drainProtocolAnnotation)
	require.Equal(t, "goairix.github.io/sandbox-cleanup-protocol", cleanupProtocolAnnotation)
}
```

Add this test near the FUSE lifecycle tests in `internal/runtime/kubernetes/runtime_test.go`:

```go
func TestFUSERuntimeCleanupFinalizerUsesProjectNamespace(t *testing.T) {
	require.Equal(t, "goairix.github.io/sandbox-fuse-runtime-cleanup", fuseRuntimeCleanupFinalizer)
}
```

- [x] **Step 2: Run the consumer tests to verify they fail**

Run:

```bash
go test ./cmd/sandbox ./internal/runtime/kubernetes -run 'TestDrainMetadataAnnotationsUseProjectNamespace|TestFUSERuntimeCleanupFinalizerUsesProjectNamespace' -count=1
```

Expected: FAIL showing the current `sandbox.huaxisy.com/*` values.

- [x] **Step 3: Replace local hardcoded values with contract aliases**

Import `github.com/goairix/sandbox/internal/kubecontract` in both production files.

Replace the constants in `cmd/sandbox/drain.go` with:

```go
const (
	backendFingerprintAnnotation = kubecontract.BackendFingerprintAnnotation
	drainProtocolAnnotation      = kubecontract.DrainProtocolAnnotation
	cleanupProtocolAnnotation    = kubecontract.CleanupProtocolAnnotation
)
```

Replace the finalizer constant in `internal/runtime/kubernetes/pod.go` with:

```go
const fuseRuntimeCleanupFinalizer = kubecontract.FUSERuntimeCleanupFinalizer
```

- [x] **Step 4: Run all affected Go package tests**

Run:

```bash
go test ./cmd/sandbox ./internal/runtime/kubernetes ./internal/kubecontract -count=1
```

Expected: PASS.

- [x] **Step 5: Commit the Go consumer cutover**

```bash
git add cmd/sandbox/drain.go cmd/sandbox/drain_test.go internal/runtime/kubernetes/pod.go internal/runtime/kubernetes/runtime_test.go
git commit -m "refactor: use project kubernetes metadata namespace"
```

### Task 3: Move Helm resources to the project namespace

**Files:**
- Modify: `deploy/helm/sandbox/templates/_helpers.tpl`
- Modify: `deploy/helm/sandbox/templates/deployment.yaml`
- Modify: `deploy/helm/sandbox/templates/backend-fingerprint.yaml`
- Modify: `deploy/helm/sandbox/templates/pre-backend-change-drain.yaml`
- Modify: `deploy/helm/sandbox/templates/post-backend-change-resume.yaml`
- Modify: `scripts/test-helm-chart.sh`
- Modify: `scripts/test-helm-backend-switch.sh`

- [x] **Step 1: Change Helm tests to require only the new keys**

In `scripts/test-helm-chart.sh`, replace the fingerprint assertion and add an old-prefix rejection:

```bash
grep -Fq 'goairix.github.io/sandbox-backend-fingerprint:' <<<"$rendered"
! grep -Fq 'sandbox.huaxisy.com/' <<<"$rendered"
```

In `scripts/test-helm-backend-switch.sh`:

- make `fingerprint()` match `goairix.github.io/sandbox-backend-fingerprint`;
- require `goairix.github.io/sandbox-drain-protocol: "v1"`;
- require `goairix.github.io/sandbox-cleanup-protocol: "v2"`;
- reject any rendered `sandbox.huaxisy.com/` key;
- update the optional cluster patch and JSONPath to use the new drain-protocol key.

The key assertions must be:

```bash
grep -Fq 'goairix.github.io/sandbox-drain-protocol: "v1"' <<<"$rendered"
grep -Fq 'goairix.github.io/sandbox-cleanup-protocol: "v2"' <<<"$rendered"
! grep -Fq 'sandbox.huaxisy.com/' <<<"$rendered"
```

- [x] **Step 2: Run Helm tests to verify they fail**

Run:

```bash
bash scripts/test-helm-chart.sh
bash scripts/test-helm-backend-switch.sh
```

Expected: both scripts FAIL because the templates still render the old keys.

- [x] **Step 3: Add the Helm metadata prefix helper**

Add to `deploy/helm/sandbox/templates/_helpers.tpl`:

```gotemplate
{{- define "sandbox.metadataPrefix" -}}goairix.github.io{{- end -}}
```

- [x] **Step 4: Render all Chart annotations from the helper**

Replace each old annotation key in the four templates with the matching expression:

```gotemplate
{{ include "sandbox.metadataPrefix" . }}/sandbox-backend-fingerprint
{{ include "sandbox.metadataPrefix" . }}/sandbox-drain-protocol
{{ include "sandbox.metadataPrefix" . }}/sandbox-cleanup-protocol
```

For example, the Deployment annotations become:

```gotemplate
      annotations:
        {{ include "sandbox.metadataPrefix" . }}/sandbox-backend-fingerprint: {{ include "sandbox.backend.fingerprint" . | quote }}
        {{ include "sandbox.metadataPrefix" . }}/sandbox-drain-protocol: "v1"
        {{ include "sandbox.metadataPrefix" . }}/sandbox-cleanup-protocol: "v2"
```

Do not add the prefix to `values.yaml`; it remains a stable Chart/controller protocol identifier.

- [x] **Step 5: Run Helm lint and rendering tests**

Run:

```bash
helm lint deploy/helm/sandbox
bash scripts/test-helm-chart.sh
bash scripts/test-helm-backend-switch.sh
```

Expected: Helm lint reports `0 chart(s) failed`; both scripts print `PASS`.

- [x] **Step 6: Commit the Helm cutover**

```bash
git add deploy/helm/sandbox/templates/_helpers.tpl deploy/helm/sandbox/templates/deployment.yaml deploy/helm/sandbox/templates/backend-fingerprint.yaml deploy/helm/sandbox/templates/pre-backend-change-drain.yaml deploy/helm/sandbox/templates/post-backend-change-resume.yaml scripts/test-helm-chart.sh scripts/test-helm-backend-switch.sh
git commit -m "refactor: rename helm kubernetes metadata keys"
```

### Task 4: Align documentation and verify the clean cutover

**Files:**
- Modify: `docs/deployment/helm-deployment-upgrade.md`
- Modify: `docs/deployment/workspace-fuse.md`
- Modify: `docs/superpowers/plans/2026-09-12-kubernetes-fuse-cleanup-recovery.md`

- [x] **Step 1: Replace active documentation examples**

Use these exact names in deployment documentation:

```text
goairix.github.io/sandbox-backend-fingerprint
goairix.github.io/sandbox-drain-protocol
goairix.github.io/sandbox-cleanup-protocol
goairix.github.io/sandbox-fuse-runtime-cleanup
```

Update the earlier cleanup implementation plan so its code snippets match the final project namespace. Preserve the metadata-prefix design document's references to the old key because those sentences explain the reason for the migration.

- [x] **Step 2: Verify active sources contain no old prefix**

Run:

```bash
if rg -n 'sandbox\.huaxisy\.com' cmd internal deploy docs/deployment docs/superpowers/plans/2026-09-12-kubernetes-fuse-cleanup-recovery.md; then
  exit 1
fi
test "$(rg -n 'sandbox\.huaxisy\.com' scripts | wc -l | tr -d ' ')" = "2"
```

Expected: no active-source matches, exactly two negative test assertions, and exit status 0.

- [x] **Step 3: Run repository verification**

Run:

```bash
gofmt -w internal/kubecontract/metadata.go internal/kubecontract/metadata_test.go cmd/sandbox/drain.go cmd/sandbox/drain_test.go internal/runtime/kubernetes/pod.go internal/runtime/kubernetes/runtime_test.go
git diff --check
go vet ./...
go test ./... -count=1
helm lint deploy/helm/sandbox
bash scripts/test-helm-chart.sh
bash scripts/test-helm-backend-switch.sh
```

Expected: formatting produces no unexpected diff; `git diff --check`, vet, and tests exit 0; Helm lint reports `0 chart(s) failed`; both scripts print `PASS`.

- [x] **Step 4: Inspect the final diff and commit documentation**

Run:

```bash
git diff --stat HEAD~3
git status --short
```

Confirm the diff contains only the metadata namespace cutover and its tests/documentation, then commit:

```bash
git add docs/deployment/helm-deployment-upgrade.md docs/deployment/workspace-fuse.md docs/superpowers/plans/2026-09-12-kubernetes-fuse-cleanup-recovery.md
git commit -m "docs: use project kubernetes metadata namespace"
```

The worktree must be clean after the commit.
