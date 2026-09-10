#!/usr/bin/env bash
set -euo pipefail

repo_root="$(CDPATH= cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)"
compose="$repo_root/docker/docker-compose.yml"
chart="$repo_root/deploy/helm/sandbox"
compose_doc="$repo_root/docs/deployment/docker-compose-deployment-upgrade.md"
helm_doc="$repo_root/docs/deployment/helm-deployment-upgrade.md"

grep -Fq 'SANDBOX_STORAGE_FILESYSTEM_ACCESS_KEY=${STORAGE_ACCESS_KEY:-}' "$compose"
grep -Fq 'SANDBOX_STORAGE_FILESYSTEM_SECRET_KEY=${STORAGE_SECRET_KEY:-}' "$compose"
! grep -Eq 'CREDENTIAL_FILES|WORKSPACE_CREDENTIAL_DIR|workspace-credentials' "$compose"
grep -Fq 'SANDBOX_WORKSPACE_BACKEND_LSM_PROFILE=${FUSE_LSM_PROFILE:-}' "$compose"
! grep -Fq 'FUSE_LSM_PROFILE=' "$repo_root/docker/.env.example"

rendered="$(helm template sandbox "$chart" --values "$repo_root/testdata/values-fuse-minio.yaml")"
grep -A1 'name: SANDBOX_STORAGE_FILESYSTEM_ACCESS_KEY' <<<"$rendered" | grep -Fq 'value: "test-access-key"'
grep -A1 'name: SANDBOX_STORAGE_FILESYSTEM_SECRET_KEY' <<<"$rendered" | grep -Fq 'value: "test-secret-key"'
! grep -Eq 'CREDENTIAL_FILES|workspace-credentials|path: accessKey|path: secretKey' <<<"$rendered"

grep -Fq 'STORAGE_ACCESS_KEY' "$compose_doc"
grep -Fq 'STORAGE_SECRET_KEY' "$compose_doc"
grep -Fq 'docker compose --env-file docker/.env -f docker/docker-compose.yml up -d' "$compose_doc"
grep -Fq 'config.storage.filesystem.accessKey/secretKey' "$helm_doc"
grep -Fq -- '--reset-values' "$helm_doc"
! grep -Fq '首次安装使用 `docker/prepare.sh`' "$compose_doc"
! grep -Fq 'workspaceCredentials:' "$helm_doc"

printf 'inline credential deployment tests: PASS\n'
