#!/usr/bin/env bash
set -euo pipefail
repo=$(cd "$(dirname "$0")/.." && pwd)
own="$repo/.superpowers/sdd/2026-10-16-enrolled-kubernetes-bridge"
go_bin=/Users/dysodeng/go/go1.25.6/bin/go
epoch=${ENROLLED_BRIDGE_EPOCH:?set unique epoch}
mode=${1:-host}
selector=${ENROLLED_BRIDGE_SELECTOR:-'^TestEnrolledBridge'}
[[ "$epoch" =~ ^[a-z0-9-]{1,40}$ ]]
[[ "$mode" == host || "$mode" == race || "$mode" == vet ]]
cd "$repo"
base="$own/$mode-$epoch"
[[ ! -e "$base.log" && ! -e "$base-inputs.json" ]]
race_flag=
cgo=0
if [[ "$mode" == race ]]; then race_flag=-race; cgo=1; fi
inputs() {
  local prefix=$1
  env GOOS=darwin GOARCH=arm64 CGO_ENABLED="$cgo" "$go_bin" list $race_flag -deps -test -compiled -json ./internal/runtime/kubernetes > "$prefix-inputs.json"
  python3 -c 'import sys,json,os
s=open(sys.argv[1]).read(); decoder=json.JSONDecoder(); paths=set()
fields="GoFiles CgoFiles CompiledGoFiles TestGoFiles XTestGoFiles SFiles CFiles CXXFiles MFiles HFiles FFiles SwigFiles SwigCXXFiles SysoFiles EmbedFiles TestEmbedFiles XTestEmbedFiles".split()
while s.strip():
 o,n=decoder.raw_decode(s.lstrip()); s=s.lstrip()[n:]
 for field in fields:
  for f in o.get(field,[]): paths.add(f if os.path.isabs(f) else os.path.join(o["Dir"],f))
for p in sorted(paths): print(p)' "$prefix-inputs.json" > "$prefix-inputs.txt"
  xargs shasum -a 256 < "$prefix-inputs.txt" > "$prefix.sha256"
}
inputs "$base-before"
set +e
if [[ "$mode" == vet ]]; then
  env GOOS=darwin GOARCH=arm64 CGO_ENABLED=0 "$go_bin" vet ./internal/runtime/kubernetes > "$base.log" 2>&1
else
  env GOOS=darwin GOARCH=arm64 CGO_ENABLED="$cgo" "$go_bin" test $race_flag ./internal/runtime/kubernetes -run "$selector" -count=1 -timeout=60s -v > "$base.log" 2>&1
fi
status=$?
set -e
inputs "$base-after"
cmp "$base-before-inputs.txt" "$base-after-inputs.txt"
cmp "$base-before.sha256" "$base-after.sha256"
printf 'mode=%s epoch=%s original_exit=%s selector=%s actual17_inputs_stable\n' "$mode" "$epoch" "$status" "$selector" | tee "$base-result.txt"
exit "$status"
