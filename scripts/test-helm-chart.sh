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
helm lint "$repo_root/deploy/helm/sandbox"
printf 'helm chart tests: PASS\n'
