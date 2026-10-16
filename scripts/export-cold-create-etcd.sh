#!/usr/bin/env bash
set -euo pipefail
repo=$(cd "$(dirname "$0")/.." && pwd)
own="$repo/.superpowers/sdd/2026-10-16-etcd-cold-create-service"
image=gcr.io/etcd-development/etcd:v3.6.15@sha256:5ed4e32061aa061f970d6500629213e8579ef17571dcf9789074966acb9afad1
run=cold-create-etcd-export-01
[[ ! -e "$own/etcd-linux-arm64" ]] || exit 1
[[ -z $(docker ps -aq --filter "label=sandbox.run=$run") ]] || exit 1
docker image inspect --format '{{.Id}} {{.Architecture}} {{json .RepoDigests}}' "$image"
id=$(docker create --name "$run" --label sandbox.task=cold-create --label "sandbox.run=$run" --network none --read-only --cap-drop ALL --security-opt no-new-privileges=true --entrypoint /usr/local/bin/etcd "$image")
cleanup() {
  local status=$?
  trap - EXIT
  docker inspect --format '{{.Id}} {{.State.Status}} {{.State.Running}} {{.State.StartedAt}} {{index .Config.Labels "sandbox.run"}}' "$id"
  [[ $(docker inspect --format '{{index .Config.Labels "sandbox.run"}} {{.State.Running}} {{.State.StartedAt}}' "$id") == "$run false 0001-01-01T00:00:00Z" ]] || exit 1
  docker rm "$id"
  [[ -z $(docker ps -aq --filter "label=sandbox.run=$run") ]] || exit 1
  exit "$status"
}
trap cleanup EXIT
docker cp "$id:/usr/local/bin/etcd" "$own/etcd-linux-arm64"
[[ $(shasum -a 256 "$own/etcd-linux-arm64" | awk '{print $1}') == 117cc09981c0f1eb378e7520be6969756c2d6a762f376944e53f7bc9800c0c59 ]] || exit 1
shasum -a 256 "$own/etcd-linux-arm64"
