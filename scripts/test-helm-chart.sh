#!/usr/bin/env bash
set -euo pipefail

repo_root="$(CDPATH= cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)"
rendered="$(helm template sandbox "$repo_root/deploy/helm/sandbox")"

grep -A1 'name: SANDBOX_WORKSPACE_DEFAULT_MOUNT_MODE' <<<"$rendered" \
  | grep -Fq 'value: "sync"'
grep -A1 'name: SANDBOX_WORKSPACE_ENABLED_MOUNT_MODES' <<<"$rendered" \
  | grep -Fq 'value: "sync,fuse"'
grep -A1 'name: SANDBOX_WORKSPACE_BACKEND_PRESET' <<<"$rendered" \
  | grep -Fq 'value: "minio"'
grep -Fq 'sandbox.huaxisy.com/backend-fingerprint:' <<<"$rendered"
! grep -Fq 'SANDBOX_WORKSPACE_PROVIDERS_' <<<"$rendered"
! grep -Fq '0.0.0.0/0' <<<"$rendered"

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
grep -Fq -- '--drain-release' <<<"$rendered"
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
grep -A1 'name: SANDBOX_WORKSPACE_BACKEND_SYSTEM_EGRESS_FQDNS' <<<"$private_obs_rendered" \
  | grep -Fq 'value: "obs.cn-southwest-268.shuanghuayun.com,sandbox-fuse-workspace.obs.cn-southwest-268.shuanghuayun.com"'
grep -Fq 'image: "registry.i.huaxisy.com/library/redis:7.4.2"' <<<"$private_obs_rendered"
if grep -Fq 'volumeClaimTemplates:' <<<"$private_obs_rendered"; then
  printf 'private OBS test overlay unexpectedly enables Redis persistence\n' >&2
  exit 1
fi

public_obs_rendered="$(helm template sandbox "$repo_root/deploy/helm/sandbox" \
  --values "$repo_root/testdata/values-fuse-obs-public.yaml")"
grep -Fq 'value: "huawei-obs-public-v1"' <<<"$public_obs_rendered"
grep -A1 'name: SANDBOX_WORKSPACE_BACKEND_SYSTEM_EGRESS_FQDNS' <<<"$public_obs_rendered" \
  | grep -Fq 'value: "obs.cn-southwest-2.myhuaweicloud.com,sandbox-fuse-workspace.obs.cn-southwest-2.myhuaweicloud.com"'
grep -A1 'name: SANDBOX_SECURITY_NETWORK_ENABLED' <<<"$public_obs_rendered" \
  | grep -Fq 'value: "false"'
grep -A1 'name: SANDBOX_SECURITY_NETWORK_BLOCK_PRIVATE' <<<"$public_obs_rendered" \
  | grep -Fq 'value: "true"'
grep -Fq 'image: "registry.i.huaxisy.com/library/redis:7.4.2"' <<<"$public_obs_rendered"
if grep -Fq 'volumeClaimTemplates:' <<<"$public_obs_rendered"; then
  printf 'public OBS test overlay unexpectedly enables Redis persistence\n' >&2
  exit 1
fi

minio_rendered="$(helm template sandbox "$repo_root/deploy/helm/sandbox" \
  --values "$repo_root/testdata/values-fuse-minio.yaml")"
if grep -Fq 'registry.local/' <<<"$minio_rendered"; then
  printf 'MinIO ds-ai-research overlay still contains a local-registry placeholder\n' >&2
  exit 1
fi
grep -A1 'name: SANDBOX_WORKSPACE_BACKEND_SYSTEM_EGRESS_FQDNS' <<<"$minio_rendered" \
  | grep -Fq 'value: "minio.sandbox-storage.svc"'
! grep -Fq 'sandbox.minio.sandbox-storage.svc' <<<"$minio_rendered"
grep -Fq 'image: "registry.i.huaxisy.com/library/redis:7.4.2"' <<<"$minio_rendered"
if grep -Fq 'volumeClaimTemplates:' <<<"$minio_rendered"; then
  printf 'MinIO test overlay unexpectedly enables Redis persistence\n' >&2
  exit 1
fi

public_mounter="$(awk '/mounter:/{print $2; exit}' "$repo_root/testdata/values-fuse-obs-public.yaml")"
private_mounter="$(awk '/mounter:/{print $2; exit}' "$repo_root/testdata/values-fuse-obs-private.yaml")"
public_docker="$(awk '/docker:/{print $2; exit}' "$repo_root/testdata/values-fuse-obs-public.yaml")"
private_docker="$(awk '/docker:/{print $2; exit}' "$repo_root/testdata/values-fuse-obs-private.yaml")"
minio_mounter="$(awk '/mounter:/{print $2; exit}' "$repo_root/testdata/values-fuse-minio.yaml")"
minio_docker="$(awk '/docker:/{print $2; exit}' "$repo_root/testdata/values-fuse-minio.yaml")"
test -n "$public_mounter" && test "$public_mounter" = "$private_mounter"
test -n "$public_docker" && test "$public_docker" = "$private_docker"
test "$public_mounter" = "$minio_mounter"
test "$public_docker" = "$minio_docker"

if helm lint "$repo_root/deploy/helm/sandbox" --set config.storage.filesystem.preset=unknown >/dev/null 2>&1; then
  printf 'helm lint unexpectedly accepted an unknown backend preset\n' >&2
  exit 1
fi
if helm lint "$repo_root/deploy/helm/sandbox" --set config.storage.filesystem.provider=minio >/dev/null 2>&1; then
  printf 'helm lint unexpectedly accepted the legacy filesystem provider field\n' >&2
  exit 1
fi
if helm lint "$repo_root/deploy/helm/sandbox" \
  --set config.workspace.defaultMountMode=fuse \
  --set-json 'config.workspace.enabledMountModes=["sync"]' >/dev/null 2>&1; then
  printf 'helm lint unexpectedly accepted a disabled default mount mode\n' >&2
  exit 1
fi
helm lint "$repo_root/deploy/helm/sandbox"
printf 'helm chart tests: PASS\n'
