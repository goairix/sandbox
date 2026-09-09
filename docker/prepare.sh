#!/usr/bin/env bash
set -euo pipefail

docker_dir="$(CDPATH= cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
repo_root="$(CDPATH= cd -- "$docker_dir/.." && pwd)"
env_file="$docker_dir/.env"
credential_dir=""
access_key_file=""
secret_key_file=""
ca_file=""
staging_root=""
non_interactive=false

usage() {
  cat >&2 <<'USAGE'
usage: docker/prepare.sh [options]

Options:
  --env-file PATH          Compose environment file (default: docker/.env)
  --credential-dir PATH    Destination for accessKey, secretKey, and optional ca.crt
  --access-key-file PATH   Import the access key from an existing file
  --secret-key-file PATH   Import the secret key from an existing file
  --ca-file PATH           Import an optional private CA bundle
  --staging-root PATH      Docker FUSE temporary credential root
  --non-interactive        Fail instead of prompting for missing credentials
  -h, --help               Show this help

Credential values are deliberately not accepted as command-line arguments or
environment variables.
USAGE
  exit "${1:-2}"
}

die() {
  printf 'prepare: %s\n' "$*" >&2
  exit 1
}

while (($#)); do
  case "$1" in
    --env-file|--credential-dir|--access-key-file|--secret-key-file|--ca-file|--staging-root)
      (($# >= 2)) || usage
      case "$1" in
        --env-file) env_file=$2 ;;
        --credential-dir) credential_dir=$2 ;;
        --access-key-file) access_key_file=$2 ;;
        --secret-key-file) secret_key_file=$2 ;;
        --ca-file) ca_file=$2 ;;
        --staging-root) staging_root=$2 ;;
      esac
      shift 2
      ;;
    --non-interactive)
      non_interactive=true
      shift
      ;;
    -h|--help)
      usage 0
      ;;
    *)
      usage
      ;;
  esac
done

command -v docker >/dev/null 2>&1 || die "docker is required"
command -v openssl >/dev/null 2>&1 || die "openssl is required"
docker info >/dev/null 2>&1 || die "Docker daemon is unavailable"
docker compose version >/dev/null 2>&1 || die "docker compose is required"

if [[ ! -e "$env_file" ]]; then
  install -d -m 0700 "$(dirname -- "$env_file")"
  install -m 0600 "$docker_dir/.env.example" "$env_file"
elif [[ ! -f "$env_file" ]]; then
  die "environment path is not a regular file: $env_file"
fi

read_env() {
  local key=$1
  awk -v key="$key" '
    index($0, key "=") == 1 {
      value = substr($0, length(key) + 2)
      sub(/\r$/, "", value)
      print value
      exit
    }
  ' "$env_file"
}

write_env() {
  local key=$1 value=$2 temp found
  temp="$(mktemp "$(dirname -- "$env_file")/.env.prepare.XXXXXX")"
  found=false
  while IFS= read -r line || [[ -n "$line" ]]; do
    if [[ "$line" == "$key="* ]]; then
      printf '%s=%s\n' "$key" "$value" >>"$temp"
      found=true
    else
      printf '%s\n' "$line" >>"$temp"
    fi
  done <"$env_file"
  if [[ "$found" == false ]]; then
    printf '%s=%s\n' "$key" "$value" >>"$temp"
  fi
  chmod --reference="$env_file" "$temp" 2>/dev/null || chmod 0600 "$temp"
  mv -f "$temp" "$env_file"
}

absolute_dir() {
  local path=$1 base=$2
  if [[ "$path" != /* ]]; then
    path="$base/$path"
  fi
  install -d -m 0700 "$path"
  (CDPATH= cd -- "$path" && pwd -P)
}

if [[ -z "$credential_dir" ]]; then
  credential_dir=$(read_env WORKSPACE_CREDENTIAL_DIR)
  [[ -n "$credential_dir" ]] || credential_dir=./secrets/workspace
fi
credential_dir=$(absolute_dir "$credential_dir" "$docker_dir")
chmod 0700 "$credential_dir"
write_env WORKSPACE_CREDENTIAL_DIR "$credential_dir"

validate_credential() {
  local value=$1 label=$2 access=$3
  [[ -n "$value" ]] || die "$label is empty"
  ((${#value} <= 4096)) || die "$label is longer than 4096 bytes"
  [[ "$value" != *$'\r'* && "$value" != *$'\n'* ]] || die "$label contains a newline"
  if [[ "$access" == true && "$value" == *:* ]]; then
    die "$label contains ':'"
  fi
}

read_credential_file() {
  local source=$1 label=$2 access=$3 value line_count
  [[ -f "$source" && ! -L "$source" ]] || die "$label source is not a regular file: $source"
  line_count=$(awk 'END { print NR }' "$source")
  [[ "$line_count" == 1 ]] || die "$label source must contain exactly one line"
  IFS= read -r value <"$source" || [[ -n "$value" ]] || die "$label source is empty"
  validate_credential "$value" "$label" "$access"
  printf '%s' "$value"
}

publish_credential() {
  local destination=$1 value=$2 temp
  temp="$(mktemp "$credential_dir/.credential.XXXXXX")"
  printf '%s\n' "$value" >"$temp"
  chmod 0400 "$temp"
  mv -f "$temp" "$destination"
}

prepare_credential() {
  local destination=$1 source=$2 label=$3 access=$4 value
  if [[ -n "$source" ]]; then
    value=$(read_credential_file "$source" "$label" "$access")
    publish_credential "$destination" "$value"
  elif [[ -f "$destination" && ! -L "$destination" ]]; then
    value=$(read_credential_file "$destination" "$label" "$access")
    chmod 0400 "$destination"
  elif [[ "$non_interactive" == true ]]; then
    die "$label is missing; provide its source file"
  else
    if [[ "$label" == "secret key" ]]; then
      IFS= read -r -s -p 'Workspace secret key: ' value
      printf '\n' >&2
    else
      IFS= read -r -p 'Workspace access key: ' value
    fi
    validate_credential "$value" "$label" "$access"
    publish_credential "$destination" "$value"
  fi
}

prepare_credential "$credential_dir/accessKey" "$access_key_file" "access key" true
prepare_credential "$credential_dir/secretKey" "$secret_key_file" "secret key" false

if [[ -n "$ca_file" ]]; then
  [[ -f "$ca_file" && ! -L "$ca_file" && -s "$ca_file" ]] || die "CA source is not a non-empty regular file: $ca_file"
  grep -q '^-----BEGIN CERTIFICATE-----$' "$ca_file" || die "CA source contains no PEM certificate"
  grep -q '^-----END CERTIFICATE-----$' "$ca_file" || die "CA source contains an incomplete PEM certificate"
  ca_temp="$(mktemp "$credential_dir/.ca.XXXXXX")"
  cp "$ca_file" "$ca_temp"
  chmod 0400 "$ca_temp"
  mv -f "$ca_temp" "$credential_dir/ca.crt"
  write_env WORKSPACE_CA_SECRET_KEY ca.crt
elif [[ -f "$credential_dir/ca.crt" ]]; then
  chmod 0400 "$credential_dir/ca.crt"
  write_env WORKSPACE_CA_SECRET_KEY ca.crt
fi

api_key=$(read_env SECURITY_API_KEY)
if [[ -z "$api_key" || "$api_key" == change-me ]]; then
  api_key=$(openssl rand -base64 36 | tr '+/' '-_' | tr -d '=\n')
  [[ ${#api_key} -ge 32 ]] || die "failed to generate API key"
  write_env SECURITY_API_KEY "$api_key"
fi

preset=$(read_env STORAGE_PRESET)
case "$preset" in
  minio|huawei-obs-public|huawei-obs-private) ;;
  *) die "STORAGE_PRESET must be minio, huawei-obs-public, or huawei-obs-private" ;;
esac

require_or_prompt_env() {
  local key=$1 label=$2 value
  value=$(read_env "$key")
  if [[ -z "$value" ]]; then
    if [[ "$non_interactive" == true ]]; then
      die "$key is required"
    fi
    IFS= read -r -p "$label: " value
    [[ -n "$value" && "$value" != *$'\r'* && "$value" != *$'\n'* ]] || die "$key is required"
    write_env "$key" "$value"
  fi
}

require_or_prompt_env STORAGE_BUCKET 'Workspace bucket'
require_or_prompt_env STORAGE_ENDPOINT 'Object storage endpoint'

enabled_modes=$(read_env WORKSPACE_ENABLED_MOUNT_MODES)
case ",$enabled_modes," in
  *,fuse,*)
    [[ "$(uname -s)" == Linux ]] || die "Docker FUSE requires a Linux host with /dev/fuse"
    [[ -e /dev/fuse ]] || die "/dev/fuse is unavailable"
    [[ -n "$(read_env FUSE_MOUNTER_IMAGE)" ]] || die "FUSE_MOUNTER_IMAGE is required when fuse is enabled"
    [[ -n "$(read_env FUSE_SANDBOX_IMAGE)" ]] || die "FUSE_SANDBOX_IMAGE is required when fuse is enabled"
    if [[ -z "$staging_root" ]]; then
      staging_root=$(read_env WORKSPACE_SECRET_STAGING_ROOT)
    fi
    [[ "$staging_root" == /* ]] || die "WORKSPACE_SECRET_STAGING_ROOT must be an absolute path"
    if [[ $(id -u) -eq 0 ]]; then
      install -d -o root -g root -m 0700 "$staging_root"
    else
      command -v sudo >/dev/null 2>&1 || die "sudo is required to prepare the FUSE staging root"
      sudo install -d -o root -g root -m 0700 "$staging_root"
    fi
    staging_root=$(CDPATH= cd -- "$staging_root" && pwd -P)
    owner_mode=$(stat -c '%u:%g:%a' "$staging_root" 2>/dev/null || true)
    [[ "$owner_mode" == 0:0:700 ]] || die "FUSE staging root must be root:root mode 0700"
    write_env WORKSPACE_SECRET_STAGING_ROOT "$staging_root"
    ;;
  *,sync,*) ;;
  *) die "WORKSPACE_ENABLED_MOUNT_MODES must contain sync or fuse" ;;
esac

auth_config=$(read_env DOCKER_AUTH_CONFIG_FILE)
if [[ "$auth_config" != /* ]]; then
  auth_config="$docker_dir/$auth_config"
fi
[[ -f "$auth_config" ]] || die "Docker auth config is missing: $auth_config"

docker compose --env-file "$env_file" \
  -f "$docker_dir/docker-compose.yml" config >/dev/null

printf 'Compose preparation complete. Start with:\n'
printf 'docker compose --env-file %q -f %q up -d\n' "$env_file" "$docker_dir/docker-compose.yml"
