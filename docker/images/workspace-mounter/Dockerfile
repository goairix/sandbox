ARG GO_BUILDER_IMAGE=golang:1.25-alpine
ARG BASE_IMAGE
FROM ${GO_BUILDER_IMAGE} AS workspace-tools-builder

WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY cmd/workspace-mounter ./cmd/workspace-mounter
COPY internal/fuseprotocol ./internal/fuseprotocol
COPY internal/mounter ./internal/mounter
RUN mkdir -p /out \
 && CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags='-s -w' \
    -o /out/workspace-mounter ./cmd/workspace-mounter

FROM ${BASE_IMAGE}

ARG BASE_IMAGE
ARG S3FS_PACKAGE_URL
ARG S3FS_PACKAGE_SHA256

USER root

RUN base_sha="${BASE_IMAGE##*@sha256:}" \
 && test "$BASE_IMAGE" != "$base_sha" \
 && test "${#base_sha}" -eq 64 \
 && case "$base_sha" in *[!0-9a-f]*) exit 1;; esac \
 && case "$S3FS_PACKAGE_URL" in https://*@*|*\?*|*\#*) exit 1;; https://*) ;; *) exit 1;; esac \
 && test "${#S3FS_PACKAGE_SHA256}" -eq 64 \
 && case "$S3FS_PACKAGE_SHA256" in *[!0-9a-f]*) exit 1;; esac \
 && test "$S3FS_PACKAGE_SHA256" != 0000000000000000000000000000000000000000000000000000000000000000

ADD ${S3FS_PACKAGE_URL} /tmp/s3fs-package
COPY docker/images/workspace-mounter/profile-bundle.json /tmp/workspace-fuse-profile-bundle.json
COPY --from=workspace-tools-builder /out/workspace-mounter /usr/local/bin/workspace-mounter

RUN printf '%s  %s\n' "$S3FS_PACKAGE_SHA256" /tmp/s3fs-package | sha256sum -c - \
 && install -o root -g root -m 0755 /tmp/s3fs-package /usr/bin/s3fs \
 && install -o root -g root -m 0755 /usr/local/bin/workspace-mounter /usr/local/bin/workspace-mounter.checked \
 && mv /usr/local/bin/workspace-mounter.checked /usr/local/bin/workspace-mounter \
 && install -d -o root -g root -m 0555 /workspace \
 && install -d -o root -g root -m 0700 /run/s3fs /var/cache/s3fs /var/cache/s3fs/tmp \
 && install -d -o root -g root -m 0755 /etc/workspace-fuse \
 && test "$(grep -Fc 0000000000000000000000000000000000000000000000000000000000000000 /tmp/workspace-fuse-profile-bundle.json)" -eq 1 \
 && sed "s/0000000000000000000000000000000000000000000000000000000000000000/$S3FS_PACKAGE_SHA256/" /tmp/workspace-fuse-profile-bundle.json > /etc/workspace-fuse/profile-bundle.json \
 && chown root:root /etc/workspace-fuse/profile-bundle.json \
 && chmod 0444 /etc/workspace-fuse/profile-bundle.json \
 && printf 'user_allow_other\n' > /etc/fuse.conf \
 && chown root:root /etc/fuse.conf \
 && chmod 0444 /etc/fuse.conf \
 && printf '%s\n' "$BASE_IMAGE" > /etc/workspace-fuse/base-image.digest \
 && printf '%s\n' "$S3FS_PACKAGE_SHA256" > /etc/workspace-fuse/s3fs-package.sha256 \
 && cd /etc/workspace-fuse \
 && sha256sum profile-bundle.json > profile-bundle.sha256 \
 && chown root:root base-image.digest s3fs-package.sha256 profile-bundle.sha256 \
 && chmod 0444 base-image.digest s3fs-package.sha256 profile-bundle.sha256 \
 && find / -xdev -type f \( -perm -4000 -o -perm -2000 \) -exec chmod a-s {} + \
 && rm -f /tmp/s3fs-package /tmp/workspace-fuse-profile-bundle.json

RUN /usr/local/bin/workspace-mounter health prepared --self-check-image

ENTRYPOINT ["/usr/local/bin/workspace-mounter", "supervise"]
