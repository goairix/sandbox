ARG BASE_IMAGE
FROM ${BASE_IMAGE}

ARG BASE_IMAGE
ARG S3FS_PACKAGE_URL
ARG S3FS_PACKAGE_SHA256
ARG PROFILE_MANIFEST=profile.json
ARG PROFILE_ID

USER root

RUN base_sha="${BASE_IMAGE##*@sha256:}" \
 && test "$BASE_IMAGE" != "$base_sha" \
 && test "${#base_sha}" -eq 64 \
 && case "$base_sha" in *[!0-9a-f]*) exit 1;; esac \
 && case "$S3FS_PACKAGE_URL" in https://*@*|*\?*|*\#*) exit 1;; https://*) ;; *) exit 1;; esac \
 && test "${#S3FS_PACKAGE_SHA256}" -eq 64 \
 && case "$S3FS_PACKAGE_SHA256" in *[!0-9a-f]*) exit 1;; esac \
 && test "$S3FS_PACKAGE_SHA256" != 0000000000000000000000000000000000000000000000000000000000000000 \
 && case "$PROFILE_ID" in ''|*[!a-z0-9.-]*) exit 1;; esac \
 && command -v python3 >/dev/null \
 && command -v node >/dev/null \
 && test -x /usr/sbin/ip \
 && test "$(id -u sandbox)" = 1000 \
 && test "$(id -g sandbox)" = 1000

ADD ${S3FS_PACKAGE_URL} /tmp/s3fs-package
COPY ${PROFILE_MANIFEST} /tmp/workspace-fuse-profile.json
# CI must compile this binary with:
#   go build -ldflags "-X main.imageProfileID=${PROFILE_ID}"
# The build-time self-check below rejects an empty or mismatched binding.
COPY workspace-mounter /usr/local/bin/workspace-mounter
COPY workspace-probe /usr/local/bin/workspace-probe

RUN printf '%s  %s\n' "$S3FS_PACKAGE_SHA256" /tmp/s3fs-package | sha256sum -c - \
 && install -o root -g root -m 0755 /tmp/s3fs-package /usr/bin/s3fs \
 && chown root:root /usr/local/bin/workspace-mounter /usr/local/bin/workspace-probe \
 && chmod 0755 /usr/local/bin/workspace-mounter /usr/local/bin/workspace-probe \
 && install -d -o root -g root -m 0555 /workspace \
 && install -d -o root -g root -m 0700 /run/s3fs /var/cache/s3fs /var/cache/s3fs/tmp \
 && install -d -o root -g root -m 0755 /etc/workspace-fuse \
 && grep -Fq "\"id\": \"$PROFILE_ID\"" /tmp/workspace-fuse-profile.json \
 && test "$(grep -Fc 0000000000000000000000000000000000000000000000000000000000000000 /tmp/workspace-fuse-profile.json)" -eq 1 \
 && sed "s/0000000000000000000000000000000000000000000000000000000000000000/$S3FS_PACKAGE_SHA256/" /tmp/workspace-fuse-profile.json > /etc/workspace-fuse/profile.json \
 && chown root:root /etc/workspace-fuse/profile.json \
 && chmod 0444 /etc/workspace-fuse/profile.json \
 && printf 'user_allow_other\n' > /etc/fuse.conf \
 && chown root:root /etc/fuse.conf \
 && chmod 0444 /etc/fuse.conf \
 && printf '%s\n' "$BASE_IMAGE" > /etc/workspace-fuse/base-image.digest \
 && printf '%s\n' "$S3FS_PACKAGE_SHA256" > /etc/workspace-fuse/s3fs-package.sha256 \
 && cd /etc/workspace-fuse \
 && sha256sum profile.json > profile.sha256 \
 && chown root:root base-image.digest s3fs-package.sha256 profile.sha256 \
 && chmod 0444 base-image.digest s3fs-package.sha256 profile.sha256 \
 && rm -f /tmp/s3fs-package /tmp/workspace-fuse-profile.json

RUN /usr/local/bin/workspace-mounter health prepared --self-check-image

WORKDIR /workspace
ENTRYPOINT ["/usr/local/bin/workspace-mounter", "supervise"]
