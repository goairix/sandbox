#!/usr/bin/env bash
set -euo pipefail

repo_root="$(CDPATH= cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)"
rendered="$(helm template sandbox "$repo_root/deploy/helm/sandbox")"

grep -A1 'name: SANDBOX_SECURITY_MAX_UPLOAD_BYTES' <<<"$rendered" \
  | grep -Fq 'value: "2147483648"'
! grep -Fq '2.147483648e+09' <<<"$rendered"
helm lint "$repo_root/deploy/helm/sandbox"
printf 'helm chart tests: PASS\n'
