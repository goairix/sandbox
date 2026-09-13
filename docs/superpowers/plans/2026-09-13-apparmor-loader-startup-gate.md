# AppArmor Loader Startup Gate and Scheduling Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Enable opt-in native config, shared node selection and bounded exact-revision AppArmor loader startup verification without changing disabled defaults or drain cleanup.

**Architecture:** Native config rejects non-string node selectors and merges Linux only when the release loader is enabled. Runtime options clone scheduling inputs and include a stable selector digest only for nonempty selectors. Startup polls the exact named DaemonSet and owned current-template Ready Pods with a total deadline; per-Pod private enforcement remains owned by the existing guard.

**Tech Stack:** Go 1.25, Kubernetes client-go fake clients, strict JSON/YAML parsing, focused tests and race detector.

---

Scope: `internal/config/config.go`, new focused config helpers/tests, native YAML config relevant fields, `cmd/sandbox/main.go` runtime options, runtime.go selector/New option fragments, new `apparmor_loader_gate.go` and tests. No existing guard edits, Chart edits, commits, images or live operations.

### Task 1: Native selector/config validation

**Files:** `internal/config/kubernetes_apparmor.go`, focused tests, config.go and configs/config.yaml.

- [x] Test defaults remain empty/empty/180; strict JSON rejects null/non-string/duplicate values; YAML non-string labels fail; valid labels retain exact case; loader requires Kubernetes, FUSE, immutable LSM and kind bypass disabled; Windows selector conflicts.
- [x] Run `go test ./internal/config -run 'Test(KubernetesNodeSelector|AppArmorLoader)'` and observe RED before implementation.
- [x] Implement strict selector parsing and clone+Linux normalization after workspace selection; add field/default/native YAML documentation. Effective selectors are capped at 64 entries; enabled timeout is 1..600 seconds, default 180.
- [x] Run focused config tests GREEN.

Contract:

```go
cfg.Runtime.Kubernetes.NodeSelector = map[string]string{"kubernetes.io/os":"windows"}
if err := cfg.Validate(); err == nil { t.Fatal("enabled loader cannot target Windows") }
```

### Task 2: Bounded exact current DaemonSet gate

**Files:** `internal/runtime/kubernetes/apparmor_loader_gate.go`, focused tests and runtime.go option/New fragments.

- [x] Add fake-client fixtures: desired=3/ready=1 accepts one current Ready Pod; stale observedGeneration/zero desired/empty UID/foreign owner/stale digest/image/args/labels/deleting Pod fail; recreated DS UID invalidates in-flight observation; failures/deadline return errors; disabled option no API gate.
- [x] Run focused gate tests and observe RED.
- [x] Implement `WithAppArmorLoader`, bounded wait and pure DS/Pod validation. Compare DS metadata/template digest and the exact expected profile args; compare all template labels/annotations and full Kubernetes-defaulted PodSpec against current owned Ready Pod. Only exact known DaemonSet scheduling/admission mutations are normalized. No Node or policy writes.
- [x] Run focused gate tests GREEN.

Contract:

```go
if err := r.waitForAppArmorLoader(ctx); err != nil { t.Fatal(err) }
```

### Task 3: Selector propagation and main options

**Files:** runtime.go selector construction/WarmPoolContract, focused runtime selector tests, cmd/sandbox runtime option wiring and focused tests.

- [x] Add RED tests for cloned map ownership, stable selector hash, disabled empty contract unchanged, ordinary/FUSE Pod selector propagation, inspection/drain excluding loader enforcement.
- [x] Implement `WithNodeSelector`, selector validation/clone and apply to ordinary/prepared construction; include selector stable JSON SHA256 only when nonempty. Main passes loader option only for normal startup, never inspection or drain.
- [x] Run `go test -race ./internal/config ./internal/runtime/kubernetes ./cmd/sandbox`, `go vet` and incremental lint; independent review returned two P2s, both reproduced RED and fixed GREEN. Follow-up review is delegated to parent because the agent-thread limit prevented resuming the reviewer.

Contract:

```go
input := map[string]string{"zone":"one"}
WithNodeSelector(input)(r)
input["zone"] = "two"
if r.nodeSelector["zone"] != "one" { t.Fatal("caller mutation changed runtime scheduling") }
```

### Helm contract

Env: `SANDBOX_RUNTIME_KUBERNETES_NODE_SELECTOR` strict JSON, `SANDBOX_RUNTIME_KUBERNETES_APPARMOR_LOADER_NAME`, `SANDBOX_RUNTIME_KUBERNETES_APPARMOR_LOADER_NAMESPACE`, `SANDBOX_RUNTIME_KUBERNETES_APPARMOR_LOADER_TIMEOUT_SECONDS`. Effective profile remains `SANDBOX_WORKSPACE_BACKEND_LSM_PROFILE`. Digest key is `sandbox.apparmor.profile-digest`. Parent owns release-scoped DS get and loader-namespace Pod get/list permissions plus loader Chart deployment.

DS metadata annotation and PodTemplate/Pod annotation contain the complete 64-hex digest; same-key PodTemplate/Pod label contains `digest[:63]` because Kubernetes label values cannot hold 64 characters. Gate verifies both forms. Effective Linux selector length remains at most 64: a 64-entry explicit selector must already contain the Linux OS key. Main/runtime/profile args follow the parent-confirmed contract.

YAML label keys preserve original case, while structural `runtime/kubernetes/node_selector` keys follow Viper's case-insensitive convention. YAML merge keys on a structural path with an effective selector are explicitly rejected, as are merge/non-string entries in the selector itself. Use explicit YAML selector mappings or JSON env rather than silently changing scheduling.

Current-template comparison defaults both PodSpecs and retains complete container probes/security/resources/env, mounts, volumes and Pod security/service-account fields. Exact controller-injected node-name affinity and fixed automatic health tolerations are normalized; custom required node affinity is not supported because the controller overwrites it in the Pod, so the original intent cannot be reconstructed from the permitted API reads. The Chart currently exposes only the shared nodeSelector, not custom loader affinity. Namespace service-account imagePullSecret name-only admission and default zero priority/preemption are normalized using existing bounded helpers.

### Verification boundary

Pure/client-fake tests do not prove installed kernel rules or actual privileged mounter enforcement. Existing guard remains mandatory for enabled loaders. No all-node startup dependency, new cleanup dependency, automatic profile replacement or unload is introduced.

### Verification ledger

- Missing native fields/helper, gate APIs and main option helper each observed RED before implementation.
- Label contract correction to 63 characters observed RED against the earlier 64-character gate, then GREEN after full annotation + short label comparison.
- 65-entry effective selector and 601-second timeout boundary tests each observed assertion RED in both config/runtime; caps corrected and GREEN.
- Independent review reproduced four assertion REDs for structural case/YAML merged effective selection, and nine assertion REDs for old template-only Ready Pods; both fixes passed focused tests.
- Focused known-controller normalization tests pass; wrong-node affinity, unknown toleration, extra Pod affinity and unreconstructable required template affinity reject.
- Full scoped race tests and vet have passed; full scoped golangci-lint reports 28 pre-existing issues in unchanged code. Incremental `golangci-lint run --new-from-rev=HEAD` passed before the final cap change; a new capitalization warning was corrected and final verification is recorded at handoff.
- No commits, image builds, cluster operations or guard edits.

### Follow-up: explicit trusted PriorityClass admission

Parent approved the explicit configuration boundary: the Chart owns optional `apparmorLoader.priorityClassName`, default empty. This follow-up changes only `apparmor_loader_gate.go`, a new `apparmor_loader_priority_test.go`, and this plan. It does not introduce PriorityClass API reads or silently accept global-default priority changes.

- [x] Add tests where a template declares `PriorityClassName: "loader-trusted"` and its same-class Pod receives `Priority: 1000` and `PreemptionPolicy: Never`; verify `appArmorLoaderObservationReady` accepts admission-only values. Cover one field at a time, class drift, explicitly set template values, empty-class global admission and security/volume/probe drift.
- [x] Run `go test -count=1 ./internal/runtime/kubernetes -run TestAppArmorLoaderGateExplicitPriorityClass -v`; observed five assertion REDs for same-class admission-only values; existing rejection cases remained passing.
- [x] When the desired class is nonempty and equals the current class, set `current.Spec.Priority = nil` only if desired priority is nil; set current preemption policy nil only if desired policy is nil. Keep both class names and all explicitly supplied fields intact for full PodSpec equality.

```go
if desired.Spec.PriorityClassName != "" && current.Spec.PriorityClassName == desired.Spec.PriorityClassName {
    if desired.Spec.Priority == nil { current.Spec.Priority = nil }
    if desired.Spec.PreemptionPolicy == nil { current.Spec.PreemptionPolicy = nil }
}
```

- [x] Run the focused command GREEN, then `go test -race -count=1 ./internal/runtime/kubernetes` and `go vet ./internal/runtime/kubernetes`; all exited 0. Verify the diff changes no other normalization or Chart/deployment files. Parent owns the documentation requiring an explicit loader class on clusters with a global-default PriorityClass.

The 19-case regression covers separate/both admission fields, partial explicit template pinning, exact explicit values, class drift/missing class, empty-class global/default-zero/nondefault fields, and security/hostPath volume/probe/API-token/mount drift. The last five remain rejected after allowed priority normalization succeeds. This boundary trusts standard API-server Priority admission and the administrator-selected class; it does not independently attest PriorityClass contents or grant system-critical scheduling. No commits, image builds or live operations occurred.
