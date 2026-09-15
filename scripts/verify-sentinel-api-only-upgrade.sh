#!/usr/bin/env bash
set -euo pipefail

usage() {
  printf '%s\n' \
    'usage:' \
    '  verify-sentinel-api-only-upgrade.sh snapshot --context CONTEXT --namespace NAMESPACE --release RELEASE --output FILE' \
    '  verify-sentinel-api-only-upgrade.sh verify   --context CONTEXT --namespace NAMESPACE --release RELEASE --snapshot FILE' >&2
}

fail() {
  printf 'sentinel API-only upgrade verification failed: %s\n' "$1" >&2
  exit 1
}

[[ $# -ge 1 ]] || { usage; exit 2; }
mode="$1"
shift
context=""
namespace=""
release=""
output_file=""
snapshot_file=""
while [[ $# -gt 0 ]]; do
  case "$1" in
  --context) [[ $# -ge 2 ]] || { usage; exit 2; }; context="$2"; shift 2 ;;
  --namespace) [[ $# -ge 2 ]] || { usage; exit 2; }; namespace="$2"; shift 2 ;;
  --release) [[ $# -ge 2 ]] || { usage; exit 2; }; release="$2"; shift 2 ;;
  --output) [[ $# -ge 2 ]] || { usage; exit 2; }; output_file="$2"; shift 2 ;;
  --snapshot) [[ $# -ge 2 ]] || { usage; exit 2; }; snapshot_file="$2"; shift 2 ;;
  *) usage; exit 2 ;;
  esac
done

[[ "$mode" == "snapshot" || "$mode" == "verify" ]] || { usage; exit 2; }
[[ -n "$context" && -n "$namespace" && -n "$release" ]] || { usage; exit 2; }
[[ "$namespace" =~ ^[a-z0-9]([-a-z0-9]*[a-z0-9])?$ ]] || fail "invalid namespace"
[[ "$release" =~ ^[a-z0-9]([-a-z0-9]*[a-z0-9])?$ ]] || fail "invalid release"
command -v jq >/dev/null 2>&1 || fail "jq is required"
kubectl_bin="${KUBECTL_BIN:-kubectl}"
command -v "$kubectl_bin" >/dev/null 2>&1 || fail "kubectl is required"

collect() {
  local deployment statefulset pods jobs
  deployment="$("$kubectl_bin" --context "$context" -n "$namespace" get deployment "${release}-api" -o json)"
  statefulset="$("$kubectl_bin" --context "$context" -n "$namespace" get statefulset "${release}-redis-sentinel" -o json)"
  pods="$("$kubectl_bin" --context "$context" -n "$namespace" get pods -l "app=sandbox-redis-sentinel,release=${release}" -o json)"
  jobs="$("$kubectl_bin" --context "$context" -n "$namespace" get jobs -l "release=${release}" -o json)"
  jq -S -n \
    --arg schema "sandbox-sentinel-api-upgrade/v1" \
    --arg context "$context" \
    --arg namespace "$namespace" \
    --arg release "$release" \
    --argjson deployment "$deployment" \
    --argjson statefulset "$statefulset" \
    --argjson pods "$pods" \
    --argjson jobs "$jobs" '
      {
        schema: $schema,
        context: $context,
        namespace: $namespace,
        release: $release,
        api: {
          image: ($deployment.spec.template.spec.containers[] | select(.name == "sandbox") | .image),
          generation: $deployment.metadata.generation,
          observedGeneration: $deployment.status.observedGeneration,
          desiredReplicas: ($deployment.spec.replicas // 1),
          replicas: ($deployment.status.replicas // 0),
          readyReplicas: ($deployment.status.readyReplicas // 0),
          updatedReplicas: ($deployment.status.updatedReplicas // 0),
          availableReplicas: ($deployment.status.availableReplicas // 0)
        },
        redis: {
          generation: $statefulset.metadata.generation,
          observedGeneration: $statefulset.status.observedGeneration,
          desiredReplicas: ($statefulset.spec.replicas // 0),
          replicas: ($statefulset.status.replicas // 0),
          readyReplicas: ($statefulset.status.readyReplicas // 0),
          currentRevision: ($statefulset.status.currentRevision // ""),
          updateRevision: ($statefulset.status.updateRevision // ""),
          podTemplate: $statefulset.spec.template,
          pods: ([$pods.items[] | {
            name: .metadata.name,
            uid: .metadata.uid,
            ready: any(.status.conditions[]?; .type == "Ready" and .status == "True"),
            statuses: ([.status.initContainerStatuses[]?, .status.containerStatuses[]?]
              | map({name: .name, imageID: (.imageID // ""), restartCount: (.restartCount // 0)})
              | sort_by(.name))
          }] | sort_by(.name))
        },
        jobs: ([$jobs.items[]
          | select(.metadata.labels.app == "sandbox-redis-identity" or .metadata.labels.app == "sandbox-redis-bootstrap")
          | .metadata.name] | sort)
      }'
}

assert_healthy() {
  local document="$1"
  jq -e '
    .api.observedGeneration >= .api.generation and
    .api.replicas == .api.desiredReplicas and
    .api.readyReplicas == .api.desiredReplicas and
    .api.updatedReplicas == .api.desiredReplicas and
    .api.availableReplicas == .api.desiredReplicas
  ' >/dev/null <<<"$document" || fail "API Deployment is not fully rolled out"
  jq -e '
    .redis.desiredReplicas == 3 and
    .redis.observedGeneration >= .redis.generation and
    .redis.replicas == 3 and
    .redis.readyReplicas == 3 and
    .redis.currentRevision != "" and
    .redis.currentRevision == .redis.updateRevision and
    (.redis.pods | length) == 3 and
    all(.redis.pods[]; .ready)
  ' >/dev/null <<<"$document" || fail "Redis StatefulSet is not 3/3 Ready"
}

if [[ "$mode" == "snapshot" ]]; then
  [[ -n "$output_file" && -z "$snapshot_file" ]] || { usage; exit 2; }
  [[ -d "$(dirname -- "$output_file")" ]] || fail "snapshot output directory does not exist"
  current="$(collect)"
  assert_healthy "$current"
  umask 077
  temporary_output="$(mktemp "${output_file}.tmp.XXXXXX")"
  trap 'rm -f -- "$temporary_output"' EXIT
  printf '%s\n' "$current" >"$temporary_output"
  mv -f -- "$temporary_output" "$output_file"
  trap - EXIT
  printf 'sentinel API-only upgrade snapshot written: %s\n' "$output_file"
  exit 0
fi

[[ -n "$snapshot_file" && -z "$output_file" ]] || { usage; exit 2; }
[[ -f "$snapshot_file" ]] || fail "snapshot file does not exist"
baseline="$(<"$snapshot_file")"
jq -e --arg context "$context" --arg namespace "$namespace" --arg release "$release" '
  .schema == "sandbox-sentinel-api-upgrade/v1" and
  .context == $context and .namespace == $namespace and .release == $release
' >/dev/null <<<"$baseline" || fail "snapshot scope does not match context/namespace/release"

current="$(collect)"
assert_healthy "$current"
compare() {
  local expression="$1"
  local message="$2"
  jq -e -n --argjson before "$baseline" --argjson after "$current" "$expression" >/dev/null || fail "$message"
}

compare '$before.api.image != $after.api.image' 'API image did not change'
compare '$before.redis.currentRevision == $after.redis.currentRevision and $before.redis.updateRevision == $after.redis.updateRevision' 'Redis StatefulSet revision changed'
compare '$before.redis.podTemplate == $after.redis.podTemplate' 'Redis StatefulSet PodTemplate changed'
compare '$before.redis.pods == $after.redis.pods' 'Redis Pod identity changed'
compare '(($after.jobs - $before.jobs) | length) == 0' 'new Sentinel one-shot Job appeared'

printf 'sentinel API-only upgrade verification: PASS\n'
