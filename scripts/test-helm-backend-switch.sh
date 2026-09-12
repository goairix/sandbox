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
grep -Fq '"helm.sh/hook": pre-upgrade' <<<"$rendered"
grep -Fq '"helm.sh/hook": pre-rollback' <<<"$rendered"
grep -Fq '"helm.sh/hook": post-install,post-upgrade,post-rollback' <<<"$rendered"
grep -Fq -- '--drain-release' <<<"$rendered"
grep -Fq -- '--required-kubernetes-drain-protocol=v1' <<<"$rendered"
grep -Fq -- '--kubernetes-backend-fingerprint-deployment=sandbox-api' <<<"$rendered"
grep -Fq -- '--verify-kubernetes-backend-fingerprint' <<<"$rendered"
grep -Fq -- '--resume-kubernetes-deployment=sandbox-api' <<<"$rendered"
grep -Fq 'sandbox.huaxisy.com/drain-protocol: "v1"' <<<"$rendered"
grep -Fq 'sandbox.huaxisy.com/cleanup-protocol: "v2"' <<<"$rendered"
grep -Fq -- '--kubernetes-cleanup-protocol=v2' <<<"$rendered"
if helm template sandbox "$chart" --set autoscaling.enabled=true --show-only templates/deployment.yaml | grep -Eq '^  replicas:'; then
  printf 'HPA-managed Deployment unexpectedly renders spec.replicas\n' >&2
  exit 1
fi
grep -Fq 'lookup "apps/v1" "Deployment"' "$chart/templates/pre-backend-change-drain.yaml"

if [[ "${HELM_BACKEND_SWITCH_RUN_CLUSTER:-0}" == "1" ]]; then
	test_image_tag="${HELM_BACKEND_SWITCH_IMAGE_TAG:?set HELM_BACKEND_SWITCH_IMAGE_TAG to a sandbox-api image containing the drain protocol}"
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
    --set image.tag="$test_image_tag" >/dev/null
  kubectl "${kubectl_args[@]}" --namespace "$test_namespace" \
    wait --for=condition=Ready pod/"$release-redis-0" --timeout=120s >/dev/null

  kubectl "${kubectl_args[@]}" --namespace "$test_namespace" patch deployment "$release-api" --type=merge \
    -p '{"spec":{"template":{"metadata":{"annotations":{"sandbox.huaxisy.com/drain-protocol":null}}}}}' >/dev/null
  if helm "${helm_args[@]}" upgrade "$release" "$chart" --namespace "$test_namespace" \
    --set replicaCount=0 --set autoscaling.enabled=false --set image.tag="$test_image_tag" \
    --set config.storage.filesystem.storageIdentity=another-physical-store >/dev/null 2>&1; then
    printf 'backend change unexpectedly bypassed missing drain protocol\n' >&2
    exit 1
  fi

  helm "${helm_args[@]}" upgrade "$release" "$chart" --namespace "$test_namespace" \
    --set replicaCount=0 --set autoscaling.enabled=false --set image.tag="$test_image_tag" >/dev/null
  test "$(kubectl "${kubectl_args[@]}" --namespace "$test_namespace" get deployment "$release-api" \
    -o jsonpath='{.spec.template.metadata.annotations.sandbox\.huaxisy\.com/drain-protocol}')" = "v1"

  helm "${helm_args[@]}" upgrade "$release" "$chart" --namespace "$test_namespace" \
    --set replicaCount=0 --set autoscaling.enabled=false --set image.tag="$test_image_tag" \
    --set config.storage.filesystem.storageIdentity=another-physical-store >/dev/null
fi

printf 'helm backend switch tests: PASS\n'
