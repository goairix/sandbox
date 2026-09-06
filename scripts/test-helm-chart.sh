#!/usr/bin/env bash
set -euo pipefail

repo_root="$(CDPATH= cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)"
rendered="$(helm template sandbox "$repo_root/deploy/helm/sandbox")"

grep -A1 'name: SANDBOX_SECURITY_MAX_UPLOAD_BYTES' <<<"$rendered" \
  | grep -Fq 'value: "2147483648"'
! grep -Fq '2.147483648e+09' <<<"$rendered"
grep -A6 'readinessProbe:' <<<"$rendered" \
  | grep -Fq 'initialDelaySeconds: 10'
grep -A8 'startupProbe:' <<<"$rendered" \
  | grep -Fq 'failureThreshold: 120'
grep -A8 'startupProbe:' <<<"$rendered" \
  | grep -Fq 'periodSeconds: 5'
grep -A14 'name: wait-for-redis' <<<"$rendered" \
  | grep -Fq 'redis-cli -h "$REDIS_HOST" ping'
grep -A8 'name: sandbox-api-drain' <<<"$rendered" \
  | grep -Fq '"helm.sh/hook": pre-delete'
grep -Fq -- '--drain-kubernetes-deployment=sandbox-api' <<<"$rendered"
grep -A8 'name: sandbox-api-drain-controller' <<<"$rendered" \
  | grep -Fq 'resourceNames:'
grep -A12 'name: sandbox-api-drain-controller' <<<"$rendered" \
  | grep -Fq 'sandbox-api'

external_api_key_rendered="$(helm template sandbox "$repo_root/deploy/helm/sandbox" \
  --set config.security.apiKeySecretName=sandbox-existing-api-key)"
grep -A4 'name: SANDBOX_SECURITY_API_KEY' <<<"$external_api_key_rendered" \
  | grep -Fq 'name: sandbox-existing-api-key'
! grep -A8 '^kind: Secret$' <<<"$external_api_key_rendered" \
  | grep -Fq 'name: sandbox-existing-api-key'

private_obs_rendered="$(helm template sandbox "$repo_root/deploy/helm/sandbox" \
  --values "$repo_root/testdata/values-fuse-obs-private.yaml")"
grep -Fq 'value: "huawei-obs-private-2023-v1"' <<<"$private_obs_rendered"
grep -A1 'name: SANDBOX_WORKSPACE_PROVIDERS_OBS_SYSTEM_EGRESS_FQDNS' <<<"$private_obs_rendered" \
  | grep -Fq 'value: "obs.cn-southwest-268.shuanghuayun.com,sandbox-fuse-workspace.obs.cn-southwest-268.shuanghuayun.com"'
grep -Fq 'image: "registry.i.huaxisy.com/library/ai-infra/sandbox-fuse-redis:feat-workspace-fuse"' <<<"$private_obs_rendered"
if grep -Fq 'volumeClaimTemplates:' <<<"$private_obs_rendered"; then
  printf 'private OBS test overlay unexpectedly enables Redis persistence\n' >&2
  exit 1
fi

public_obs_rendered="$(helm template sandbox "$repo_root/deploy/helm/sandbox" \
  --values "$repo_root/testdata/values-fuse-obs-public.yaml")"
grep -Fq 'value: "huawei-obs-public-v1"' <<<"$public_obs_rendered"
grep -A1 'name: SANDBOX_WORKSPACE_PROVIDERS_OBS_SYSTEM_EGRESS_FQDNS' <<<"$public_obs_rendered" \
  | grep -Fq 'value: "obs.cn-southwest-2.myhuaweicloud.com,sandbox-fuse-workspace.obs.cn-southwest-2.myhuaweicloud.com"'
grep -A1 'name: SANDBOX_SECURITY_NETWORK_ENABLED' <<<"$public_obs_rendered" \
  | grep -Fq 'value: "false"'
grep -A1 'name: SANDBOX_SECURITY_NETWORK_BLOCK_PRIVATE' <<<"$public_obs_rendered" \
  | grep -Fq 'value: "true"'
grep -Fq 'image: "registry.i.huaxisy.com/library/ai-infra/sandbox-fuse-redis:feat-workspace-fuse"' <<<"$public_obs_rendered"
if grep -Fq 'volumeClaimTemplates:' <<<"$public_obs_rendered"; then
  printf 'public OBS test overlay unexpectedly enables Redis persistence\n' >&2
  exit 1
fi
helm lint "$repo_root/deploy/helm/sandbox"
printf 'helm chart tests: PASS\n'
