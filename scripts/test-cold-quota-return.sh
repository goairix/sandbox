#!/usr/bin/env bash
set -euo pipefail
repo=$(cd "$(dirname "$0")/.." && pwd)
own="$repo/.superpowers/sdd/2026-10-16-etcd-cold-runtime-quota-return"
go_bin=/Users/dysodeng/go/go1.25.6/bin/go
epoch=${COLD_QUOTA_EPOCH:?set a fresh epoch}
mode=${1:?host, race or vet}
target=${COLD_QUOTA_TARGET:-./internal/storage/state/etcd}
selector=${COLD_QUOTA_SELECTOR:-'^TestColdRuntimeQuotaReturn'}
[[ "$epoch" =~ ^[a-z0-9-]{1,40}$ ]] || exit 1
[[ "$mode" == host || "$mode" == race || "$mode" == vet ]] || exit 1
case "$target" in ./internal/storage/state/etcd|./internal/runtime/controlrunner) ;; *) exit 2;; esac
cd "$repo"
collector="$repo/scripts/test-cold-quota-return.sh"
helper="$repo/scripts/cold-create-native-common.sh"
base="$own/$mode-$epoch"
[[ ! -e "$base-inputs.txt.json" && ! -e "$base.log" ]] || exit 1
shasum -a 256 "$collector" "$helper" > "$base-script.sha256"
shasum -a 256 -c "$base-script.sha256"
source "$helper"
collect_inputs() {
  local artifact=$1 platform=darwin
  if [[ "$mode" == vet ]]; then platform=linux; fi
  if [[ "$mode" == race ]]; then
    env GOOS=darwin GOARCH=arm64 CGO_ENABLED=1 "$go_bin" list -race -deps -test -compiled -json "$target" > "$artifact.json"
  else
    env GOOS="$platform" GOARCH=arm64 CGO_ENABLED=0 "$go_bin" list -deps -test -compiled -json "$target" > "$artifact.json"
  fi
  native_input_paths "$artifact"
}
collect_inputs "$base-inputs.txt"
xargs shasum -a 256 < "$base-inputs.txt" > "$base-before.sha256"
set +e
if [[ "$mode" == vet ]]; then
  env GOOS=linux GOARCH=arm64 CGO_ENABLED=0 "$go_bin" vet "$target" > "$base.log" 2>&1
elif [[ "$mode" == race ]]; then
  env GOOS=darwin GOARCH=arm64 CGO_ENABLED=1 "$go_bin" test -race "$target" -run "$selector" -count=1 -v -timeout=60s > "$base.log" 2>&1
else
  env GOOS=darwin GOARCH=arm64 CGO_ENABLED=0 "$go_bin" test "$target" -run "$selector" -count=1 -v -timeout=60s > "$base.log" 2>&1
fi
status=$?
set -e
collect_inputs "$base-post-inputs.txt"
cmp "$base-inputs.txt" "$base-post-inputs.txt"
xargs shasum -a 256 < "$base-post-inputs.txt" > "$base-after.sha256"
cmp "$base-before.sha256" "$base-after.sha256"
COPYFILE_DISABLE=1 tar -czPf "$base-source.tar.gz" -T "$base-inputs.txt"
native_verify_archive "$base-source.tar.gz" "$base-before.sha256"
shasum -a 256 "$collector" "$helper" > "$base-script-post.sha256"
cmp "$base-script.sha256" "$base-script-post.sha256"
COPYFILE_DISABLE=1 tar -czPf "$base-script.tar.gz" "$collector" "$helper"
native_verify_archive "$base-script.tar.gz" "$base-script.sha256"
shasum -a 256 -c "$base-script.sha256"
shasum -a 256 "$base-source.tar.gz" "$base-script.tar.gz" > "$base-artifacts.sha256"
printf 'COLD_QUOTA_%s epoch=%s status=%s actual17-inputs-stable\n' "$mode" "$epoch" "$status"
exit "$status"
