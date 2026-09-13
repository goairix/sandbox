# Helm External Sentinel Authentication Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Render independent external Redis/Sentinel client credentials consistently for API and drain without changing built-in Redis behavior.

**Architecture:** Keep existing Redis env construction and add optional external Sentinel fields next to data-node authentication. Sentinel username stays a literal; Sentinel password uses the release Redis Secret's separate `sentinel-password` key. Create the external Secret when either password is set, emit only configured keys, and never substitute either password for the other. Preserve the installed Deployment env-copy branch unchanged.

**Tech Stack:** Helm templates, Bash, Python 3/PyYAML structured render assertions.

---

## Ownership and exclusions

Modify only `redis.external` authentication values in `deploy/helm/sandbox/values.yaml`, Redis Secret authentication in `templates/secret.yaml`, Redis authentication env in `templates/deployment.yaml` and `templates/_helpers.tpl`, and create `scripts/test-helm-sentinel-auth.sh`. This plan is separate from built-in Sentinel, AppArmor, selectors, native config, and CLI wiring. No commits, image builds, deployment, or live cluster checks.

### Task 1: Deterministic render assertions (RED)

- [x] Create `scripts/test-helm-sentinel-auth.sh` using Python subprocess Helm rendering and `yaml.safe_load_all`. Cover default built-in, ignored external credentials while built-in enabled, external Sentinel with distinct credentials, Sentinel password only, data password only, username only, and no authentication. Use dummy credentials containing spaces, quotes, backslash, and Unicode.
- [x] For each render, obtain the API env and all drain Job env, require unique env names, and compare the authentication entries exactly. Require usernames as `{name, value}` and passwords as `{name, valueFrom: {secretKeyRef: {name: sandbox-redis, key: password|sentinel-password}}}`. Decode Secret data and require exactly the configured password keys; require no release Redis Secret when no password applies.
- [x] Verify the pre-upgrade installed Deployment env branch remains the existing `hasKey $installedContainer "env"` / `toYaml (get $installedContainer "env")` path using `rg -Fq`, without editing that template.
- [x] Run `bash scripts/test-helm-sentinel-auth.sh`. Expected RED: external Sentinel auth env lacks the newly requested username/password; record exact failure before template edits.

### Task 2: Minimal values, Secret and env integration (GREEN)

- [x] Add Chinese-commented external values next to username/password:

```yaml
    # 客户端连接 Sentinel 的独立认证；不复用 Redis 数据节点凭据。
    sentinelUsername: ""
    # 通过 Redis Secret 的 sentinel-password 键注入；两端密码互不回退。
    sentinelPassword: ""
```

- [x] Extend only the external Secret condition and keys:

```gotemplate
{{- else if and (not .Values.redis.enabled) (or .Values.redis.external.password .Values.redis.external.sentinelPassword) }}
```

```gotemplate
data:
  {{- if .Values.redis.external.password }}
  password: {{ .Values.redis.external.password | b64enc | quote }}
  {{- end }}
  {{- if .Values.redis.external.sentinelPassword }}
  sentinel-password: {{ .Values.redis.external.sentinelPassword | b64enc | quote }}
  {{- end }}
```

- [x] Add these entries after existing data-node authentication in `sandbox.drainEnv` and with existing env indentation in `deployment.yaml`:

```gotemplate
{{- if and (not .Values.redis.enabled) .Values.redis.external.sentinelUsername }}
- name: SANDBOX_STORAGE_STATE_REDIS_SENTINEL_USERNAME
  value: {{ .Values.redis.external.sentinelUsername | quote }}
{{- end }}
{{- if and (not .Values.redis.enabled) .Values.redis.external.sentinelPassword }}
- name: SANDBOX_STORAGE_STATE_REDIS_SENTINEL_PASSWORD
  valueFrom:
    secretKeyRef:
      name: {{ printf "%s-redis" .Release.Name }}
      key: sentinel-password
{{- end }}
```

- [x] Run `bash scripts/test-helm-sentinel-auth.sh`. Expected GREEN: all structured auth, Secret, uniqueness, default compatibility, and preserved env-branch assertions pass. Notify parent shared auth fragments are finished before parent edits loader fragments.

### Task 3: Existing Helm regression checks and handoff

- [x] Run `bash scripts/test-helm-chart.sh` and `HELM_BACKEND_SWITCH_RUN_CLUSTER=0 bash scripts/test-helm-backend-switch.sh`; expected local render/lint PASS, no cluster operations.
- [x] Run `bash -n scripts/test-helm-sentinel-auth.sh` and `git diff --check`; expected exit 0.
- [x] Report exact RED/GREEN evidence and touched fragments; keep current checkout/branch as-is without commits.

## Self-review

This bounded plan satisfies only approved external client credential plumbing and local rendered consistency. Installed env handling is deliberately untouched and checked structurally, not claimed as a live-upgrade validation. Built-in Sentinel, existing-Secret configuration, topology construction, real authentication links and production HA remain separate implementation/acceptance scopes. Neither authentication field is added to selector/LSM/bootstrap helpers.

## Execution evidence

- Test harness correction before feature RED: `--set-string redis.enabled=false` produced a bool/string mismatch; corrected all overrides to `--set-json` with `json.dumps`, preserving booleans and special characters rather than treating that harness error as feature evidence.
- RED: `bash scripts/test-helm-sentinel-auth.sh` exited 1 with `AssertionError: ('distinct external credentials', 'API', 'authentication env mismatch', ...)`; actual env contained only data-node username/password and expected env additionally required both Sentinel entries. No production templates had been edited yet.
- GREEN: the same command exited 0 with `helm external Sentinel auth tests: PASS` after the bounded auth changes.
- Regression: `bash scripts/test-helm-chart.sh` exited 0 with `1 chart(s) linted, 0 chart(s) failed` and `helm chart tests: PASS` (only the existing informational icon recommendation).
- Regression: `HELM_BACKEND_SWITCH_RUN_CLUSTER=0 bash scripts/test-helm-backend-switch.sh` exited 0 with `helm backend switch tests: PASS`; cluster branch explicitly disabled.
- `bash -n scripts/test-helm-sentinel-auth.sh` and `git diff --check` exited 0.
- Shared `_helpers.tpl` and `deployment.yaml` authentication fragments handed back to parent immediately after GREEN for loader integration. No later auth-fragment changes are planned; no commits, images, deployments, or live Redis operations.
