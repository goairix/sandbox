# Helm Max Upload Bytes Rendering Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Make the default Helm Chart render `SANDBOX_SECURITY_MAX_UPLOAD_BYTES` as the exact decimal string accepted by sandbox-api, then verify two clean kind deployments.

**Architecture:** Keep the Kubernetes environment-variable contract unchanged and prevent YAML numeric coercion at the values boundary. Add a shell regression test that renders the real Chart and asserts the exact Deployment value before exercising the release through its HTTP API.

**Tech Stack:** Helm 3, Bash, Kubernetes/kind, Go sandbox-api

---

### Task 1: Add the Helm rendering regression

**Files:**
- Create: `scripts/test-helm-chart.sh`
- Test: `scripts/test-helm-chart.sh`

- [ ] **Step 1: Write the failing render test**

```bash
#!/usr/bin/env bash
set -euo pipefail

repo_root="$(CDPATH= cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)"
rendered="$(helm template sandbox "$repo_root/deploy/helm/sandbox")"

grep -A1 'name: SANDBOX_SECURITY_MAX_UPLOAD_BYTES' <<<"$rendered" \
  | grep -Fq 'value: "2147483648"'
! grep -Fq '2.147483648e+09' <<<"$rendered"
helm lint "$repo_root/deploy/helm/sandbox"
printf 'helm chart tests: PASS\n'
```

- [ ] **Step 2: Run the test to verify RED**

Run: `bash scripts/test-helm-chart.sh`

Expected: non-zero exit because the rendered value is currently `"2.147483648e+09"`.

### Task 2: Preserve the large value as a decimal string

**Files:**
- Modify: `deploy/helm/sandbox/values.yaml:118`
- Test: `scripts/test-helm-chart.sh`

- [ ] **Step 1: Apply the minimal values fix**

Change:

```yaml
maxUploadBytes: 2147483648
```

to:

```yaml
maxUploadBytes: "2147483648"
```

- [ ] **Step 2: Run the render test to verify GREEN**

Run: `bash scripts/test-helm-chart.sh`

Expected: `helm chart tests: PASS` and Helm lint reports zero failed charts.

- [ ] **Step 3: Run repository checks and commit**

Run:

```bash
git diff --check
go test ./...
git add deploy/helm/sandbox/values.yaml scripts/test-helm-chart.sh
git commit -m "fix: preserve helm upload byte limit"
```

Expected: checks pass and the fix is committed without unrelated files.

### Task 3: Verify and leave a clean Helm deployment running

**Files:**
- No repository files modified.

- [ ] **Step 1: Install the release into `sandbox-helm`**

Run `helm upgrade --install sandbox deploy/helm/sandbox --namespace sandbox-helm --atomic --wait` with the local `helmtest` images, one API replica, one warm pool member, Redis persistence disabled, Kubernetes runtime, local sync storage, and immutable `imagePullPolicy=Never` for locally imported images.

Expected: release status `deployed`; API, Redis, and one `sandbox-pool-*` Pod are Running.

- [ ] **Step 2: Exercise the normal API flow**

Port-forward `service/sandbox-api` to `127.0.0.1:18080`, verify `/health`, create a persistent sandbox, execute code that confirms UID 1000, and destroy the sandbox.

Expected: all HTTP operations succeed, the acquired pool Pod name matches `sandbox-pool-[a-z0-9]{10}`, and the pool refills to one prepared Pod.

- [ ] **Step 3: Uninstall and confirm cleanup**

Run: `helm uninstall sandbox -n sandbox-helm` and wait until release-owned API, Redis, and managed sandbox Pods are absent.

Expected: no release remains in the namespace and no managed runtime resource from the first install remains.

- [ ] **Step 4: Reinstall and leave it running**

Repeat the exact atomic Helm install, wait for readiness and the warm pool, then verify `/health` from inside the cluster.

Expected: release revision 1 is `deployed`, API/Redis/pool Pods are Running, no CrashLoopBackOff or warning event exists, and the release remains installed for user inspection.
