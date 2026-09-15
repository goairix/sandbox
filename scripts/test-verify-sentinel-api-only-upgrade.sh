#!/usr/bin/env bash
set -euo pipefail

repo_root="$(CDPATH= cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)"
temporary_dir="$(mktemp -d)"
trap 'rm -rf -- "$temporary_dir"' EXIT
fake_kubectl="$repo_root/testdata/verify-sentinel-api-only-upgrade/fake-kubectl.sh"
snapshot="$temporary_dir/before.json"

verifier="$repo_root/scripts/verify-sentinel-api-only-upgrade.sh"

KUBECTL_BIN="$fake_kubectl" FIXTURE_PHASE=before "$verifier" snapshot \
  --context test --namespace ns --release sandbox --output "$snapshot"
KUBECTL_BIN="$fake_kubectl" FIXTURE_PHASE=after "$verifier" verify \
  --context test --namespace ns --release sandbox --snapshot "$snapshot"

expect_failure() {
  local phase="$1"
  local expected="$2"
  local output
  if output="$(KUBECTL_BIN="$fake_kubectl" FIXTURE_PHASE="$phase" "$verifier" verify \
      --context test --namespace ns --release sandbox --snapshot "$snapshot" 2>&1)"; then
    printf 'fixture %s unexpectedly passed\n' "$phase" >&2
    exit 1
  fi
  grep -Fq "$expected" <<<"$output"
}

expect_failure same-api 'API image did not change'
expect_failure api-not-ready 'API Deployment is not fully rolled out'
expect_failure redis-pod-changed 'Redis Pod identity changed'
expect_failure redis-not-ready 'Redis StatefulSet is not 3/3 Ready'
expect_failure new-job 'new Sentinel one-shot Job appeared'

printf 'sentinel API-only upgrade verifier tests: PASS\n'
