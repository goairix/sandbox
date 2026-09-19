# HCE AppArmor AF_UNIX Isolated Validation Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Determine whether the FUSE mounter can run on the authorized HCE test node with the `network unix stream` allowance removed, without relaxing AppArmor warnings or changing business policy.

**Architecture:** A small deterministic renderer derives a uniquely named test-only profile from the current Chart template and rejects any unexpected source structure. The candidate is strictly compiled against the target node's captured AppArmor features without loading it. Only after node maintenance prerequisites are met may a separate temporary workload load and exercise the candidate; failure leaves the formal Chart unchanged.

**Tech Stack:** Python 3.10 standard library, existing Helm AppArmor profile, AppArmor parser 4.1.7, HCE 2.0, Kubernetes 1.29+.

---

### Task 1: Deterministic test-only profile

**Files:**
- Create: `tools/apparmor-hce/lib/unix_probe.py`
- Create: `tools/apparmor-hce/tests/test_unix_probe.py`
- Modify: `tools/apparmor-hce/tests/contract.sh`

- [ ] **Step 1: Write a failing renderer test**

```python
def test_candidate_removes_only_one_unix_allowance():
    from unix_probe import candidate_bytes
    original = b"profile __SANDBOX_PROFILE_NAME__ {\n  network inet stream,\n  network unix stream,\n}\n"
    name, result = candidate_bytes(original)
    assert name.startswith("sandbox-fuse-unix-probe-")
    assert b"  network inet stream,\n" in result
    assert b"  network unix stream,\n" not in result
    assert result == original.replace(b"  network unix stream,\n", b"").replace(b"__SANDBOX_PROFILE_NAME__", name.encode())
```

- [ ] **Step 2: Run the focused test and observe the expected missing-module failure**

```sh
PYTHONPATH=tools/apparmor-hce/lib:tools/apparmor-hce/tests python3 -m unittest test_unix_probe -v
```

- [ ] **Step 3: Implement the renderer with one exact-line removal and content-bound name**

```python
"""Derive an isolated stricter AF_UNIX test profile; never mutate the Chart."""
import hashlib
from profile_policy import MARKER, normalize

UNIX_ALLOW = b"  network unix stream,\n"


def candidate_bytes(template):
    original = normalize(template)
    if original.count(UNIX_ALLOW) != 1:
        raise ValueError("Chart profile must contain exactly one expected AF_UNIX allowance")
    candidate = original.replace(UNIX_ALLOW, b"", 1)
    name = "sandbox-fuse-unix-probe-" + hashlib.sha256(candidate).hexdigest()
    return name, candidate.replace(MARKER.encode(), name.encode())
```

- [ ] **Step 4: Add negative tests and run the contract suite**

```python
def test_candidate_rejects_missing_or_duplicate_unix_rule():
    from unix_probe import candidate_bytes
    for source in (b"profile __SANDBOX_PROFILE_NAME__ {}\n",
                   b"profile __SANDBOX_PROFILE_NAME__ {\n  network unix stream,\n  network unix stream,\n}\n"):
        with self.assertRaises(ValueError):
            candidate_bytes(source)
```

```sh
tools/apparmor-hce/tests/contract.sh
git diff --check
```

- [ ] **Step 5: Commit the renderer and tests**

```sh
git add tools/apparmor-hce/lib/unix_probe.py tools/apparmor-hce/tests/test_unix_probe.py tools/apparmor-hce/tests/contract.sh
git commit -m "test: derive strict HCE UNIX probe profile"
```

### Task 2: Reproducible offline target-kernel compile

**Files:**
- Modify: `tools/apparmor-hce/lib/unix_probe.py`
- Modify: `tools/apparmor-hce/tests/test_unix_probe.py`
- Modify: `docs/testing/2026-09-18-hce-apparmor-parser-build-validation.md`

- [ ] **Step 1: Add a failing test for a no-load, no-cache parser invocation**

```python
def test_compile_candidate_uses_strict_flags_and_both_target_feature_inputs(self):
    from unix_probe import compile_candidate
    with patch("unix_probe.subprocess.run") as run:
        run.return_value.returncode = 0
        run.return_value.stdout = b""
        run.return_value.stderr = b""
        compile_candidate("/tmp/parser", "/tmp/candidate.profile", "/tmp/features", "/tmp/candidate.bin")
        args = run.call_args.args[0]
        self.assertEqual(args[:6], ["/tmp/parser", "-Q", "-K", "--config-file=/dev/null", "--Werror", "--warn=all"])
        self.assertEqual(args[args.index("--policy-features") + 1], "/tmp/features")
        self.assertEqual(args[args.index("--kernel-features") + 1], "/tmp/features")
```

- [ ] **Step 2: Run the focused test and observe the expected missing-function failure**

```sh
PYTHONPATH=tools/apparmor-hce/lib:tools/apparmor-hce/tests python3 -m unittest test_unix_probe -v
```

- [ ] **Step 3: Implement `compile_candidate` as a strict wrapper around the existing compile checker**

```python
from parser_checks import compile_profile


def compile_candidate(parser, profile, features, output):
    return compile_profile(parser, profile, output, features=features, policy_features=features)
```

The test must patch `parser_checks.subprocess.run` through the existing checker rather than substitute a weaker parser call. Require the checker report to show `kernel_load=false`, `passed=true`, and zero stdout/stderr; a failure remains a failure.

- [ ] **Step 4: Run tests and record the exact on-node offline outcome when maintenance access is available**

```sh
tools/apparmor-hce/tests/contract.sh
git diff --check
```

On node `172.16.30.166`, verify hostname/UID and the trusted RPM SHA-256 from the design record before using the unpacked parser. Compile the candidate using the captured 29-file target feature directory. Do not run parser without `-Q -K`, install RPM, or load a profile in this step. Record candidate SHA-256, feature snapshot SHA-256, parser version, exit code, diagnostics and binary size in the testing report. If maintenance access is absent, mark the field command not run rather than inventing a result.

- [ ] **Step 5: Commit code and evidence**

```sh
git add tools/apparmor-hce/lib/unix_probe.py tools/apparmor-hce/tests/test_unix_probe.py docs/testing/2026-09-18-hce-apparmor-parser-build-validation.md
git commit -m "test: compile strict UNIX probe against target features"
```

### Task 3: Conditional live test and clean handoff

**Files:**
- Modify: `docs/testing/2026-09-18-hce-apparmor-parser-build-validation.md`

- [ ] **Step 1: Reconfirm maintenance prerequisites**

Read-only check node and namespace UIDs, parser/RPM provenance, node health, current CRI AppArmor state, workload placement, temporary namespace availability, recovery plan and a non-sensitive MinIO test prefix. If any prerequisite is missing, stop before installing software, reconfiguring CRI or loading policy.

- [ ] **Step 2: Only with the separately authorized maintenance window, run an isolated positive/negative test**

Load the unique candidate name without replacing a business profile; pin temporary mounter and probe workloads to `172.16.30.166`. Verify `/proc/*/attr/current` for mounter and s3fs is the candidate in enforce mode; exercise FUSE mount/read/write/flush/unmount/destroy against the dedicated test prefix. In a confined probe, `socket(AF_UNIX, SOCK_STREAM, 0)` must be denied with a matching AppArmor kernel audit event. A missing audit event or another LSM/permission as cause is not a pass.

- [ ] **Step 3: Clean only registered test objects and recheck prior state**

Remove the unique test profile, namespace, workloads and test prefix after confirming exact ownership. Record any remaining Pod, NetworkPolicy, mount, PVC/PV or process; do not touch unrelated business resources. Recheck node readiness and business profile state.

- [ ] **Step 4: Record outcome and commit**

```sh
git add docs/testing/2026-09-18-hce-apparmor-parser-build-validation.md
git commit -m "test: record HCE UNIX probe field outcome"
```

If all checks pass, request a separate review of the production Chart change. If any check fails, retain the Chart and the strict downgrade blocker; do not accept a warning automatically.
