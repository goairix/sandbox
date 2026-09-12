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
grep -Fq 'goairix.github.io/sandbox-backend-fingerprint:' <<<"$rendered"
! grep -Fq 'sandbox.huaxisy.com/' <<<"$rendered"
! grep -Fq 'SANDBOX_WORKSPACE_PROVIDERS_' <<<"$rendered"
! grep -Eq 'SANDBOX_WORKSPACE_(SECRET_NAME|ALLOW_UNVERIFIED_DURABLE_FLUSH|BACKEND_(CA_SECRET_KEY|ENDPOINT_HOST_IPS|SYSTEM_EGRESS|DNS_CIDRS|ENDPOINT_PORTS))|SANDBOX_STORAGE_FILESYSTEM_CA_FILE' <<<"$rendered"
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
grep -Fq 'image: "registry.i.huaxisy.com/library/redis:7.4.2"' <<<"$private_obs_rendered"
grep -A1 'name: SANDBOX_SECURITY_NETWORK_ENABLED' <<<"$private_obs_rendered" \
  | grep -Fq 'value: "true"'
grep -A1 'name: SANDBOX_SECURITY_NETWORK_BLOCK_PRIVATE' <<<"$private_obs_rendered" \
  | grep -Fq 'value: "true"'
if grep -Fq 'volumeClaimTemplates:' <<<"$private_obs_rendered"; then
  printf 'private OBS test overlay unexpectedly enables Redis persistence\n' >&2
  exit 1
fi

public_obs_rendered="$(helm template sandbox "$repo_root/deploy/helm/sandbox" \
  --values "$repo_root/testdata/values-fuse-obs-public.yaml")"
grep -Fq 'value: "huawei-obs-public-v1"' <<<"$public_obs_rendered"
grep -A1 'name: SANDBOX_SECURITY_NETWORK_ENABLED' <<<"$public_obs_rendered" \
  | grep -Fq 'value: "true"'
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
grep -A1 'name: SANDBOX_SECURITY_NETWORK_ENABLED' <<<"$minio_rendered" \
  | grep -Fq 'value: "true"'
grep -A1 'name: SANDBOX_SECURITY_NETWORK_BLOCK_PRIVATE' <<<"$minio_rendered" \
  | grep -Fq 'value: "true"'
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
public_api_repository="$(awk '/^image:/{image=1; next} image && /repository:/{print $2; exit}' "$repo_root/testdata/values-fuse-obs-public.yaml")"
private_api_repository="$(awk '/^image:/{image=1; next} image && /repository:/{print $2; exit}' "$repo_root/testdata/values-fuse-obs-private.yaml")"
minio_api_repository="$(awk '/^image:/{image=1; next} image && /repository:/{print $2; exit}' "$repo_root/testdata/values-fuse-minio.yaml")"
public_api_tag="$(awk '/^image:/{image=1; next} image && /tag:/{print $2; exit}' "$repo_root/testdata/values-fuse-obs-public.yaml")"
private_api_tag="$(awk '/^image:/{image=1; next} image && /tag:/{print $2; exit}' "$repo_root/testdata/values-fuse-obs-private.yaml")"
minio_api_tag="$(awk '/^image:/{image=1; next} image && /tag:/{print $2; exit}' "$repo_root/testdata/values-fuse-minio.yaml")"
expected_api_repository='registry.i.huaxisy.com/library/ai-infra/sandbox-api'
expected_api_tag='v0.2.12'
expected_mounter='registry.i.huaxisy.com/library/ai-infra/sandbox-fuse-mounter:v0.2.12'
expected_docker='registry.i.huaxisy.com/library/ai-infra/sandbox-fuse-docker:v0.2.12'
test "$public_api_repository" = "$expected_api_repository"
test "$private_api_repository" = "$expected_api_repository"
test "$minio_api_repository" = "$expected_api_repository"
test "$public_api_tag" = "$expected_api_tag"
test "$private_api_tag" = "$expected_api_tag"
test "$minio_api_tag" = "$expected_api_tag"
test -n "$public_mounter" && test "$public_mounter" = "$private_mounter"
test -n "$public_docker" && test "$public_docker" = "$private_docker"
test "$public_mounter" = "$minio_mounter"
test "$public_docker" = "$minio_docker"
test "$minio_mounter" = "$expected_mounter"
test "$minio_docker" = "$expected_docker"

grep -A4 '^redis:' "$repo_root/deploy/helm/sandbox/values.yaml" \
  | grep -Fq 'repository: redis'
grep -A5 '^redis:' "$repo_root/deploy/helm/sandbox/values.yaml" \
  | grep -Fq 'tag: "7-alpine"'

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
