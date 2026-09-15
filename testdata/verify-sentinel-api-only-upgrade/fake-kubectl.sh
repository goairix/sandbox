#!/usr/bin/env bash
set -euo pipefail

phase="${FIXTURE_PHASE:-before}"
resource=""
for ((index = 1; index <= $#; index++)); do
  argument="${!index}"
  if [[ "$argument" == "get" ]]; then
    next=$((index + 1))
    resource="${!next}"
    break
  fi
done

case "$resource" in
deployment)
  image="registry.example/sandbox-api:v1"
  generation=1
  if [[ "$phase" != "before" && "$phase" != "same-api" ]]; then
    image="registry.example/sandbox-api:v2"
    generation=2
  fi
  ready=3
  [[ "$phase" == "api-not-ready" ]] && ready=2
  printf '{"metadata":{"generation":%d},"spec":{"replicas":3,"template":{"metadata":{"labels":{"app":"sandbox"}},"spec":{"containers":[{"name":"sandbox","image":"%s"}]}}},"status":{"observedGeneration":%d,"replicas":3,"readyReplicas":%d,"updatedReplicas":%d,"availableReplicas":%d}}\n' \
    "$generation" "$image" "$generation" "$ready" "$ready" "$ready"
  ;;
statefulset)
  ready=3
  [[ "$phase" == "redis-not-ready" ]] && ready=2
  printf '{"metadata":{"generation":4},"spec":{"replicas":3,"template":{"metadata":{"labels":{"app":"sandbox-redis-sentinel"}},"spec":{"containers":[{"name":"redis","image":"redis:7"},{"name":"sentinel","image":"redis:7"}]}}},"status":{"observedGeneration":4,"replicas":3,"readyReplicas":%d,"currentRevision":"redis-rev-a","updateRevision":"redis-rev-a"}}\n' "$ready"
  ;;
pods)
  uid0="uid-0"
  restart0=0
  image0="sha256:redis-a"
  if [[ "$phase" == "redis-pod-changed" ]]; then
    uid0="uid-0-replaced"
    restart0=1
    image0="sha256:redis-b"
  fi
  printf '{"items":[
    {"metadata":{"name":"sandbox-redis-sentinel-0","uid":"%s"},"status":{"conditions":[{"type":"Ready","status":"True"}],"containerStatuses":[{"name":"redis","restartCount":%d,"imageID":"%s"},{"name":"sentinel","restartCount":0,"imageID":"sha256:redis-a"}]}},
    {"metadata":{"name":"sandbox-redis-sentinel-1","uid":"uid-1"},"status":{"conditions":[{"type":"Ready","status":"True"}],"containerStatuses":[{"name":"redis","restartCount":0,"imageID":"sha256:redis-a"},{"name":"sentinel","restartCount":0,"imageID":"sha256:redis-a"}]}},
    {"metadata":{"name":"sandbox-redis-sentinel-2","uid":"uid-2"},"status":{"conditions":[{"type":"Ready","status":"True"}],"containerStatuses":[{"name":"redis","restartCount":0,"imageID":"sha256:redis-a"},{"name":"sentinel","restartCount":0,"imageID":"sha256:redis-a"}]}}
  ]}\n' "$uid0" "$restart0" "$image0"
  ;;
jobs)
  if [[ "$phase" == "new-job" ]]; then
    printf '{"items":[{"metadata":{"name":"sandbox-redis-sentinel-identity-2","labels":{"app":"sandbox-redis-identity","release":"sandbox"}}}]}\n'
  else
    printf '{"items":[]}\n'
  fi
  ;;
*)
  printf 'unsupported fake kubectl invocation: %s\n' "$*" >&2
  exit 2
  ;;
esac
