#!/usr/bin/env bash
set -euo pipefail

repo_root="$(CDPATH= cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)"
compose="$repo_root/docker/docker-compose.yml"
chart="$repo_root/deploy/helm/sandbox"

grep -Fq 'SANDBOX_STORAGE_FILESYSTEM_ACCESS_KEY=${STORAGE_ACCESS_KEY:-}' "$compose"
grep -Fq 'SANDBOX_STORAGE_FILESYSTEM_SECRET_KEY=${STORAGE_SECRET_KEY:-}' "$compose"
! grep -Eq 'CREDENTIAL_FILES|WORKSPACE_CREDENTIAL_DIR|workspace-credentials' "$compose"

rendered="$(helm template sandbox "$chart" --values "$repo_root/testdata/values-fuse-minio.yaml")"
grep -A1 'name: SANDBOX_STORAGE_FILESYSTEM_ACCESS_KEY' <<<"$rendered" | grep -Fq 'value: "test-access-key"'
grep -A1 'name: SANDBOX_STORAGE_FILESYSTEM_SECRET_KEY' <<<"$rendered" | grep -Fq 'value: "test-secret-key"'
! grep -Eq 'CREDENTIAL_FILES|workspace-credentials|path: accessKey|path: secretKey' <<<"$rendered"

printf 'inline credential deployment tests: PASS\n'
