#!/usr/bin/env bash
set -euo pipefail

repo_root="$(CDPATH= cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)"
chart="$repo_root/deploy/helm/sandbox"

fingerprint() {
  helm template sandbox "$chart" "$@" \
    | awk '/name: sandbox-backend-fingerprint/{found=1} found && /sandbox.huaxisy.com\/backend-fingerprint:/{gsub(/"/, "", $2); print $2; exit}'
}

base_fingerprint="$(fingerprint)"
image_only_fingerprint="$(fingerprint --set image.tag=image-only-upgrade)"
changed_fingerprint="$(fingerprint --set config.storage.filesystem.storageIdentity=another-physical-store)"

test -n "$base_fingerprint"
test "$base_fingerprint" = "$image_only_fingerprint"
test "$base_fingerprint" != "$changed_fingerprint"

rendered="$(helm template sandbox "$chart")"
grep -Fq '"helm.sh/hook": pre-upgrade,pre-rollback' <<<"$rendered"
grep -Fq -- '--drain-release' <<<"$rendered"
grep -Fq 'lookup "v1" "ConfigMap"' "$chart/templates/pre-backend-change-drain.yaml"
grep -Fq 'lookup "apps/v1" "Deployment"' "$chart/templates/pre-backend-change-drain.yaml"

if [[ "${HELM_BACKEND_SWITCH_RUN_CLUSTER:-0}" == "1" ]]; then
  kube_context="${HELM_BACKEND_SWITCH_CONTEXT:-}"
  kubectl_args=()
  helm_args=()
  if [[ -n "$kube_context" ]]; then
    kubectl_args+=(--context "$kube_context")
    helm_args+=(--kube-context "$kube_context")
  fi
  test_namespace="sandbox-helm-switch-$PPID"
  release="sandbox-switch"
  upgrade_pid=""
  upgrade_log=""
  cleanup() {
    if [[ -n "$upgrade_pid" ]]; then
      kill "$upgrade_pid" >/dev/null 2>&1 || true
      wait "$upgrade_pid" >/dev/null 2>&1 || true
    fi
    if [[ -n "$upgrade_log" ]]; then
      rm -f "$upgrade_log"
    fi
    helm "${helm_args[@]}" uninstall "$release" --namespace "$test_namespace" --no-hooks >/dev/null 2>&1 || true
    kubectl "${kubectl_args[@]}" delete namespace "$test_namespace" --wait=true --timeout=60s >/dev/null 2>&1 || true
  }
  trap cleanup EXIT

  kubectl "${kubectl_args[@]}" create namespace "$test_namespace" >/dev/null
  helm "${helm_args[@]}" install "$release" "$chart" --namespace "$test_namespace" \
    --set replicaCount=0 \
    --set autoscaling.enabled=false \
    --set redis.enabled=false \
    --set redis.external.addr=redis.invalid:6379 >/dev/null

  if ! helm upgrade --help | grep -Fq -- '--dry-run string'; then
    helm "${helm_args[@]}" upgrade "$release" "$chart" --namespace "$test_namespace" \
      --set replicaCount=0 --set autoscaling.enabled=false \
      --set redis.enabled=false --set redis.external.addr=redis.invalid:6379 \
      --set image.tag=image-only-upgrade >/dev/null
    if kubectl "${kubectl_args[@]}" --namespace "$test_namespace" \
      get job sandbox-switch-backend-change-drain >/dev/null 2>&1; then
      printf 'same backend fingerprint unexpectedly created a drain hook\n' >&2
      exit 1
    fi
    upgrade_log="$(mktemp)"
    helm "${helm_args[@]}" upgrade "$release" "$chart" --namespace "$test_namespace" \
      --set replicaCount=0 --set autoscaling.enabled=false \
      --set redis.enabled=false --set redis.external.addr=redis.invalid:6379 \
      --set image.tag=image-only-upgrade \
      --set config.storage.filesystem.storageIdentity=another-physical-store >"$upgrade_log" 2>&1 &
    upgrade_pid=$!
    drain_created=false
    for _ in $(seq 1 20); do
      if kubectl "${kubectl_args[@]}" --namespace "$test_namespace" \
        get job sandbox-switch-backend-change-drain >/dev/null 2>&1; then
        drain_created=true
        break
      fi
      sleep 0.5
    done
    if [[ "$drain_created" != "true" ]]; then
      printf 'changed backend did not create the drain hook\n' >&2
      exit 1
    fi
    kill "$upgrade_pid" >/dev/null 2>&1 || true
    wait "$upgrade_pid" >/dev/null 2>&1 || true
    upgrade_pid=""
    printf 'cluster lookup check: PASS (Helm %s live-hook fallback)\n' "$(helm version --short)"
    printf 'helm backend switch tests: PASS\n'
    exit 0
  fi

  same_rendered="$(helm "${helm_args[@]}" upgrade "$release" "$chart" --namespace "$test_namespace" \
    --dry-run=server --set replicaCount=0 --set autoscaling.enabled=false \
    --set redis.enabled=false --set redis.external.addr=redis.invalid:6379 \
    --set image.tag=image-only-upgrade)"
  if grep -Fq 'name: sandbox-switch-backend-change-drain' <<<"$same_rendered"; then
    printf 'same backend fingerprint unexpectedly rendered a drain hook\n' >&2
    exit 1
  fi

  changed_rendered="$(helm "${helm_args[@]}" upgrade "$release" "$chart" --namespace "$test_namespace" \
    --dry-run=server --set replicaCount=0 --set autoscaling.enabled=false \
    --set redis.enabled=false --set redis.external.addr=redis.invalid:6379 \
    --set config.storage.filesystem.storageIdentity=another-physical-store)"
  grep -Fq 'name: sandbox-switch-backend-change-drain' <<<"$changed_rendered"
  grep -Fq -- '--drain-release' <<<"$changed_rendered"
fi

printf 'helm backend switch tests: PASS\n'
