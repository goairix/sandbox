#!/usr/bin/env bash
set -euo pipefail
repo=$(cd "$(dirname "$0")/.." && pwd)
own="$repo/.superpowers/sdd/2026-10-16-enrolled-kubernetes-profile"
go_bin=/Users/dysodeng/go/go1.25.6/bin/go
epoch=${ENROLLED_PROFILE_EPOCH:?set a unique owned epoch}
mode=${1:-host}
[[ "$epoch" =~ ^[a-z0-9-]{1,40}$ ]]
[[ "$mode" == host || "$mode" == race || "$mode" == vet ]]
target=${ENROLLED_PROFILE_TARGET:-./internal/runtime/launcherconfig}
case "$target" in ./internal/runtime/launcherconfig|./internal/kubecontract|./internal/runtime/kubernetes|./internal/nodeobserver|./cmd/sandbox-launcher) ;; *) exit 2;; esac
selector=${ENROLLED_PROFILE_SELECTOR:-'^TestEnrolled'}
cd "$repo"
inputs() {
  local artifact=$1
  if [[ "$mode" == race ]]; then
    env GOOS=darwin GOARCH=arm64 CGO_ENABLED=1 "$go_bin" list -race -deps -test -compiled -json "$target" > "$artifact.json"
  else
    env GOOS=darwin GOARCH=arm64 CGO_ENABLED=0 "$go_bin" list -deps -test -compiled -json "$target" > "$artifact.json"
  fi
  python3 -c 'import sys,json,os
s=open(sys.argv[1]).read(); d=json.JSONDecoder(); paths=set(); fields="GoFiles CgoFiles CompiledGoFiles TestGoFiles XTestGoFiles SFiles CFiles CXXFiles MFiles HFiles FFiles SwigFiles SwigCXXFiles SysoFiles EmbedFiles TestEmbedFiles XTestEmbedFiles".split()
while s.strip():
 o,n=d.raw_decode(s.lstrip()); s=s.lstrip()[n:]
 for field in fields:
  for f in o.get(field,[]): paths.add(f if os.path.isabs(f) else os.path.join(o["Dir"],f))
for p in sorted(paths): print(p)' "$artifact.json" > "$artifact"
}
base="$own/$mode-$epoch"
[[ ! -e "$base-inputs.txt.json" && ! -e "$base.log" ]]
inputs "$base-inputs.txt"
xargs shasum -a 256 < "$base-inputs.txt" > "$base-before.sha256"
set +e
if [[ "$mode" == race ]]; then
  env GOOS=darwin GOARCH=arm64 CGO_ENABLED=1 "$go_bin" test -race "$target" -run "$selector" -count=1 -timeout=45s > "$base.log" 2>&1
elif [[ "$mode" == vet ]]; then
  env GOOS=darwin GOARCH=arm64 CGO_ENABLED=0 "$go_bin" vet "$target" > "$base.log" 2>&1
else
  env GOOS=darwin GOARCH=arm64 CGO_ENABLED=0 "$go_bin" test "$target" -run "$selector" -count=1 -timeout=45s > "$base.log" 2>&1
fi
status=$?
set -e
inputs "$base-post-inputs.txt"
cmp "$base-inputs.txt" "$base-post-inputs.txt"
xargs shasum -a 256 < "$base-post-inputs.txt" > "$base-after.sha256"
cmp "$base-before.sha256" "$base-after.sha256"
printf 'ENROLLED_PROFILE_%s epoch=%s status=%s inputs-stable\n' "$mode" "$epoch" "$status"
exit "$status"
