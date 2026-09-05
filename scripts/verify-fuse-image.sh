#!/usr/bin/env bash
set -euo pipefail

usage() {
  printf 'usage: %s <kubernetes|docker> <package-check|release-check> <profile-id>\n' "$0" >&2
  exit 2
}

die() {
  printf 'verify-fuse-image: %s\n' "$*" >&2
  exit 1
}

require_digest_image() {
  image=$1
  label=$2
  case "$image" in
    *@sha256:*) ;;
    *) die "$label must be pinned by sha256 digest" ;;
  esac
  name=${image%@sha256:*}
  digest=${image##*@sha256:}
  test -n "$name" || die "$label has an empty repository name"
  case "$name" in *@*) die "$label has an invalid repository name" ;; esac
  test "${#digest}" -eq 64 || die "$label has an invalid sha256 digest"
  case "$digest" in *[!0-9a-f]*) die "$label has an invalid sha256 digest" ;; esac
}

test "$#" -eq 3 || usage
runtime=$1
check=$2
profile_id=$3

case "$runtime" in kubernetes|docker) ;; *) usage ;; esac
case "$check" in package-check|release-check) ;; *) usage ;; esac
case "$profile_id" in
  ''|*[!a-z0-9.-]*|.*|*..*|*.) die "invalid profile ID" ;;
esac

command -v docker >/dev/null 2>&1 || die "docker is required"

case "$runtime" in
  kubernetes)
    : "${FUSE_IMAGE:?set FUSE_IMAGE to the digest-pinned mounter image}"
    : "${SANDBOX_IMAGE:?set SANDBOX_IMAGE to the digest-pinned ordinary sandbox image}"
    require_digest_image "$FUSE_IMAGE" FUSE_IMAGE
    require_digest_image "$SANDBOX_IMAGE" SANDBOX_IMAGE
    package_image=$FUSE_IMAGE
    probe_image=$SANDBOX_IMAGE
    ;;
  docker)
    : "${SANDBOX_IMAGE:?set SANDBOX_IMAGE to the digest-pinned special sandbox image}"
    require_digest_image "$SANDBOX_IMAGE" SANDBOX_IMAGE
    package_image=$SANDBOX_IMAGE
    probe_image=$SANDBOX_IMAGE
    ;;
esac

docker run --rm --entrypoint /usr/local/bin/workspace-mounter \
  "$package_image" health prepared --self-check-image
docker run --rm --user 1000:1000 --entrypoint /usr/local/bin/workspace-probe \
  "$probe_image" self-check

docker run --rm --entrypoint /bin/sh "$package_image" -ceu '
  # image package contract
  profile_id=$1
  test -x /usr/bin/s3fs
  test -x /usr/bin/fusermount3
  test -x /usr/local/bin/workspace-mounter
  test -f /etc/fuse.conf
  test "$(grep -c "^user_allow_other$" /etc/fuse.conf)" -eq 1
  test "$(stat -c %a /workspace)" = 555
  test "$(stat -c %u:%g /workspace)" = 0:0
  test "$(stat -c %a /run/s3fs)" = 700
  test "$(stat -c %u:%g /run/s3fs)" = 0:0
  test "$(stat -c %a /var/cache/s3fs)" = 700
  test "$(stat -c %a /var/cache/s3fs/tmp)" = 700
  test "$(stat -c %u:%g /var/cache/s3fs/tmp)" = 0:0
  test "$(stat -c %a /etc/workspace-fuse/profile.json)" = 444
  test "$(stat -c %u:%g /etc/workspace-fuse/profile.json)" = 0:0
  cd /etc/workspace-fuse
  sha256sum -c profile.sha256
  stored_s3fs_sha=$(cat s3fs-package.sha256)
  test "${#stored_s3fs_sha}" -eq 64
  case "$stored_s3fs_sha" in *[!0-9a-f]*) exit 1;; esac
  test "$(sha256sum /usr/bin/s3fs | cut -d " " -f 1)" = "$stored_s3fs_sha"
  base_ref=$(cat base-image.digest)
  case "$base_ref" in *@sha256:*) ;;
    *) exit 1;;
  esac
  base_sha=${base_ref##*@sha256:}
  test "${#base_sha}" -eq 64
  case "$base_sha" in *[!0-9a-f]*) exit 1;; esac
  grep -Fq "\"id\": \"$profile_id\"" profile.json
  grep -Eq "\"mount_parameters\": \"(verified|candidate|unverified)\"" profile.json
  grep -Eq "\"durable_flush\": \"(verified|blocked-pending-flush-spike)\"" profile.json
' image-contract "$profile_id"

if test "$runtime" = kubernetes; then
  docker run --rm --entrypoint /bin/sh "$SANDBOX_IMAGE" -ceu '
    # ordinary sandbox contract
    test -x /usr/local/bin/workspace-probe
    test ! -e /usr/local/bin/workspace-mounter
    test ! -e /usr/bin/s3fs
    test ! -e /etc/workspace-fuse/profile.json
  '
else
  docker run --rm --entrypoint /bin/sh "$SANDBOX_IMAGE" -ceu '
    # docker special sandbox contract
    test -x /usr/local/bin/workspace-probe
    test -x /usr/local/bin/workspace-mounter
    test -x /usr/bin/s3fs
  '
fi

if test "$check" = release-check; then
	docker run --rm --entrypoint /usr/local/bin/workspace-mounter \
		"$package_image" health prepared --release-check-image
fi

printf 'verified %s image contract for %s (%s)\n' "$runtime" "$profile_id" "$check"
