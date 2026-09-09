#!/usr/bin/env bash
set -euo pipefail

repo_root="$(CDPATH= cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)"
prepare="$repo_root/docker/prepare.sh"
tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT

fail() {
  printf 'FAIL: %s\n' "$*" >&2
  exit 1
}

file_mode() {
  stat -c '%a' "$1" 2>/dev/null || stat -f '%Lp' "$1"
}

replace_env() {
  file=$1
  key=$2
  value=$3
  awk -v key="$key" -v value="$value" '
    BEGIN { found = 0 }
    index($0, key "=") == 1 { print key "=" value; found = 1; next }
    { print }
    END { if (!found) print key "=" value }
  ' "$file" >"$file.new"
  mv "$file.new" "$file"
}

expect_failure() {
  if "$@" >"$tmp/stdout" 2>"$tmp/stderr"; then
    fail "command unexpectedly succeeded: $*"
  fi
}

mkdir -p "$tmp/bin"
printf '%s\n' \
  '#!/bin/sh' \
  'set -eu' \
  'case "$*" in' \
  '  info|"compose version"|*" config") exit 0 ;;' \
  '  *) exit 0 ;;' \
  'esac' >"$tmp/bin/docker"
chmod +x "$tmp/bin/docker"

env_file="$tmp/sandbox.env"
credentials="$tmp/workspace"
access_source="$tmp/access-source"
secret_source="$tmp/secret-source"
cp "$repo_root/docker/.env.example" "$env_file"
replace_env "$env_file" STORAGE_BUCKET sandbox-workspace
replace_env "$env_file" STORAGE_ENDPOINT minio.example.com
replace_env "$env_file" SECURITY_API_KEY existing-api-key
printf '%s\n' test-access >"$access_source"
printf '%s\n' test-secret >"$secret_source"

PATH="$tmp/bin:$PATH" "$prepare" \
  --env-file "$env_file" \
  --credential-dir "$credentials" \
  --access-key-file "$access_source" \
  --secret-key-file "$secret_source" \
  --non-interactive >"$tmp/stdout" 2>"$tmp/stderr"

test "$(cat "$credentials/accessKey")" = test-access || fail "access key was not imported"
test "$(cat "$credentials/secretKey")" = test-secret || fail "secret key was not imported"
test "$(file_mode "$credentials")" = 700 || fail "credential directory mode is not 0700"
test "$(file_mode "$credentials/accessKey")" = 400 || fail "access key mode is not 0400"
test "$(file_mode "$credentials/secretKey")" = 400 || fail "secret key mode is not 0400"
grep -Fxq 'SECURITY_API_KEY=existing-api-key' "$env_file" || fail "existing API key was replaced"
canonical_credentials=$(CDPATH= cd -- "$credentials" && pwd -P)
grep -Fq "WORKSPACE_CREDENTIAL_DIR=$canonical_credentials" "$env_file" || fail "credential directory was not written to env"
! grep -Fq test-access "$tmp/stdout" "$tmp/stderr" || fail "access key leaked to output"
! grep -Fq test-secret "$tmp/stdout" "$tmp/stderr" || fail "secret key leaked to output"

first_access_hash=$(shasum -a 256 "$credentials/accessKey" | awk '{print $1}')
first_secret_hash=$(shasum -a 256 "$credentials/secretKey" | awk '{print $1}')
PATH="$tmp/bin:$PATH" "$prepare" \
  --env-file "$env_file" \
  --credential-dir "$credentials" \
  --non-interactive >"$tmp/stdout" 2>"$tmp/stderr"
test "$(shasum -a 256 "$credentials/accessKey" | awk '{print $1}')" = "$first_access_hash" || fail "idempotent run changed access key"
test "$(shasum -a 256 "$credentials/secretKey" | awk '{print $1}')" = "$first_secret_hash" || fail "idempotent run changed secret key"

generated_env="$tmp/generated.env"
generated_credentials="$tmp/generated-workspace"
cp "$repo_root/docker/.env.example" "$generated_env"
replace_env "$generated_env" STORAGE_BUCKET sandbox-workspace
replace_env "$generated_env" STORAGE_ENDPOINT minio.example.com
PATH="$tmp/bin:$PATH" "$prepare" \
  --env-file "$generated_env" \
  --credential-dir "$generated_credentials" \
  --access-key-file "$access_source" \
  --secret-key-file "$secret_source" \
  --non-interactive >"$tmp/stdout" 2>"$tmp/stderr"
grep -Eq '^SECURITY_API_KEY=[A-Za-z0-9_-]{32,}$' "$generated_env" || fail "API key was not generated"

interactive_env="$tmp/interactive.env"
interactive_credentials="$tmp/interactive-workspace"
env PATH="$tmp/bin:$PATH" "$prepare" \
  --env-file "$interactive_env" \
  --credential-dir "$interactive_credentials" \
  --access-key-file "$access_source" \
  --secret-key-file "$secret_source" \
  >"$tmp/stdout" 2>"$tmp/stderr" <<'INPUT'
sandbox-workspace
minio.example.com
INPUT
grep -Fxq 'STORAGE_BUCKET=sandbox-workspace' "$interactive_env" || fail "interactive setup did not write the bucket"
grep -Fxq 'STORAGE_ENDPOINT=minio.example.com' "$interactive_env" || fail "interactive setup did not write the endpoint"

invalid_ca="$tmp/invalid-ca.crt"
printf '%s\n' not-a-certificate >"$invalid_ca"
expect_failure env PATH="$tmp/bin:$PATH" "$prepare" \
  --env-file "$env_file" \
  --credential-dir "$credentials" \
  --ca-file "$invalid_ca" \
  --non-interactive

missing_env="$tmp/missing.env"
cp "$repo_root/docker/.env.example" "$missing_env"
replace_env "$missing_env" STORAGE_BUCKET sandbox-workspace
replace_env "$missing_env" STORAGE_ENDPOINT minio.example.com
expect_failure env PATH="$tmp/bin:$PATH" "$prepare" \
  --env-file "$missing_env" \
  --credential-dir "$tmp/missing-workspace" \
  --non-interactive

multiline_access="$tmp/multiline-access"
printf 'first-line\nsecond-line' >"$multiline_access"
expect_failure env PATH="$tmp/bin:$PATH" "$prepare" \
  --env-file "$env_file" \
  --credential-dir "$credentials" \
  --access-key-file "$multiline_access" \
  --non-interactive

printf 'compose preparation tests: PASS\n'
