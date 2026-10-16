#!/usr/bin/env bash
set -euo pipefail
repo=$(cd "$(dirname "$0")/.." && pwd)
own="$repo/.superpowers/sdd/2026-10-16-etcd-cold-create-service"
go_bin=/Users/dysodeng/go/go1.25.6/bin/go
epoch=${COLD_CREATE_EPOCH:?set a fresh epoch}
mode=${1:-host}
target=${COLD_CREATE_TARGET:-./internal/storage/state/etcd}
selector=${COLD_CREATE_SELECTOR:-'^TestColdPreparation'}
[[ "$epoch" =~ ^[a-z0-9-]{1,40}$ ]] || exit 1
[[ "$mode" == host || "$mode" == race || "$mode" == vet ]] || exit 1
platform=darwin
if [[ "$mode" == vet ]]; then platform=linux; fi
case "$target" in ./internal/storage/state/etcd|./internal/runtime/kubernetes|./internal/provisioning|./internal/sandbox|./internal/runtime/controlrunner) ;; *) exit 2;; esac
cd "$repo"
base="$own/$mode-$epoch"
[[ ! -e "$base-inputs.txt.json" && ! -e "$base.log" ]] || exit 1
inputs() {
  local artifact=$1
  if [[ "$mode" == race ]]; then
    env GOOS=darwin GOARCH=arm64 CGO_ENABLED=1 "$go_bin" list -race -deps -test -compiled -json "$target" > "$artifact.json"
  else
    env GOOS="$platform" GOARCH=arm64 CGO_ENABLED=0 "$go_bin" list -deps -test -compiled -json "$target" > "$artifact.json"
  fi
  python3 -c 'import sys,json,os
s=open(sys.argv[1]).read(); d=json.JSONDecoder(); paths=set(); fields="GoFiles CgoFiles CompiledGoFiles TestGoFiles XTestGoFiles SFiles CFiles CXXFiles MFiles HFiles FFiles SwigFiles SwigCXXFiles SysoFiles EmbedFiles TestEmbedFiles XTestEmbedFiles".split()
while s.strip():
 o,n=d.raw_decode(s.lstrip()); s=s.lstrip()[n:]
 for field in fields:
  for f in o.get(field,[]): paths.add(f if os.path.isabs(f) else os.path.join(o["Dir"],f))
for p in sorted(paths): print(p)' "$artifact.json" > "$artifact"
}
inputs "$base-inputs.txt"
xargs shasum -a 256 < "$base-inputs.txt" > "$base-before.sha256"
set +e
if [[ "$mode" == race ]]; then
  env GOOS=darwin GOARCH=arm64 CGO_ENABLED=1 "$go_bin" test -race "$target" -run "$selector" -count=1 -timeout=60s > "$base.log" 2>&1
elif [[ "$mode" == vet ]]; then
  env GOOS="$platform" GOARCH=arm64 CGO_ENABLED=0 "$go_bin" vet "$target" > "$base.log" 2>&1
else
  env GOOS=darwin GOARCH=arm64 CGO_ENABLED=0 "$go_bin" test "$target" -run "$selector" -count=1 -timeout=60s > "$base.log" 2>&1
fi
status=$?
set -e
inputs "$base-post-inputs.txt"
cmp "$base-inputs.txt" "$base-post-inputs.txt"
xargs shasum -a 256 < "$base-post-inputs.txt" > "$base-after.sha256"
cmp "$base-before.sha256" "$base-after.sha256"
COPYFILE_DISABLE=1 tar -czPf "$base-source.tar.gz" -T "$base-inputs.txt"
python3 - "$base-source.tar.gz" "$base-before.sha256" <<'PY'
import hashlib,sys,tarfile
expected={}
for line in open(sys.argv[2]):
 digest,path=line.rstrip("\n").split("  ",1); expected[path]=digest
with tarfile.open(sys.argv[1]) as archive:
 regular=[m for m in archive.getmembers() if m.isfile()]
 members={m.name:m for m in regular}
 assert len(regular)==len(members)
 assert set(members)==set(expected)
 for path,digest in expected.items():
  assert hashlib.sha256(archive.extractfile(members[path]).read()).hexdigest()==digest,path
PY
shasum -a 256 "$base-source.tar.gz" "$repo/scripts/test-cold-create.sh" > "$base-artifacts.sha256"
printf 'COLD_CREATE_%s epoch=%s status=%s inputs-stable\n' "$mode" "$epoch" "$status"
exit "$status"
