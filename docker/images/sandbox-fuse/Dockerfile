ARG GO_BUILDER_IMAGE=golang:1.25-alpine
FROM ${GO_BUILDER_IMAGE} AS workspace-tools-builder

WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY cmd/workspace-mounter ./cmd/workspace-mounter
COPY cmd/workspace-probe ./cmd/workspace-probe
COPY internal/fuseprotocol ./internal/fuseprotocol
COPY internal/mounter ./internal/mounter
COPY internal/workspaceprobe ./internal/workspaceprobe
RUN mkdir -p /out \
 && CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags='-s -w' \
    -o /out/workspace-mounter ./cmd/workspace-mounter \
 && CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags='-s -w' \
    -o /out/workspace-probe ./cmd/workspace-probe

FROM debian:bookworm-slim AS s3fs-builder
ENV S3FS_COMMIT=561ce1e20600bab42133b5e51511483800d3ef76
RUN apt-get update \
 && apt-get install -y --no-install-recommends \
    automake autotools-dev g++ git libcurl4-openssl-dev libfuse3-dev \
    libssl-dev libxml2-dev make pkg-config ca-certificates \
 && rm -rf /var/lib/apt/lists/*
COPY docker/images/common/build-s3fs.sh /usr/local/bin/build-s3fs
RUN /usr/local/bin/build-s3fs /out

FROM python:3.13-slim-bookworm

USER root

COPY docker/images/sandbox/install-runtime.sh /usr/local/bin/install-sandbox-runtime
RUN /usr/local/bin/install-sandbox-runtime \
 && rm -f /usr/local/bin/install-sandbox-runtime \
 && apt-get update \
 && apt-get install -y --no-install-recommends \
    ca-certificates fuse3 libcurl4 libssl3 libxml2 \
 && rm -rf /var/lib/apt/lists/*

ENV PLAYWRIGHT_BROWSERS_PATH=/opt/playwright-browsers
ENV NODE_PATH=/opt/sandbox-libs/node_modules

COPY docker/images/workspace-mounter/profile-bundle.json /tmp/workspace-fuse-profile-bundle.json
COPY --from=s3fs-builder /out/usr/bin/s3fs /usr/bin/s3fs
COPY --from=workspace-tools-builder /out/workspace-mounter /usr/local/bin/workspace-mounter
COPY --from=workspace-tools-builder /out/workspace-probe /usr/local/bin/workspace-probe

RUN command -v python3 >/dev/null \
 && command -v node >/dev/null \
 && test -x /usr/sbin/ip \
 && test "$(id -u sandbox)" = 1000 \
 && test "$(id -g sandbox)" = 1000 \
 && chown root:root /usr/local/bin/workspace-mounter /usr/local/bin/workspace-probe \
 && chmod 0755 /usr/local/bin/workspace-mounter /usr/local/bin/workspace-probe \
 && install -d -o root -g root -m 0555 /workspace \
 && install -d -o root -g root -m 0700 /run/s3fs /var/cache/s3fs /var/cache/s3fs/tmp \
 && install -d -o root -g root -m 0755 /etc/workspace-fuse \
 && test "$(grep -Fc 0000000000000000000000000000000000000000000000000000000000000000 /tmp/workspace-fuse-profile-bundle.json)" -eq 1 \
 && s3fs_sha="$(sha256sum /usr/bin/s3fs | cut -d ' ' -f 1)" \
 && sed "s/0000000000000000000000000000000000000000000000000000000000000000/$s3fs_sha/" /tmp/workspace-fuse-profile-bundle.json > /etc/workspace-fuse/profile-bundle.json \
 && chown root:root /etc/workspace-fuse/profile-bundle.json \
 && chmod 0444 /etc/workspace-fuse/profile-bundle.json \
 && printf 'user_allow_other\n' > /etc/fuse.conf \
 && chown root:root /etc/fuse.conf \
 && chmod 0444 /etc/fuse.conf \
 && printf '%s\n' "$s3fs_sha" > /etc/workspace-fuse/s3fs-package.sha256 \
 && cd /etc/workspace-fuse \
 && sha256sum profile-bundle.json > profile-bundle.sha256 \
 && chown root:root s3fs-package.sha256 profile-bundle.sha256 \
 && chmod 0444 s3fs-package.sha256 profile-bundle.sha256 \
 && find / -xdev -type f \( -perm -4000 -o -perm -2000 \) -exec chmod a-s {} + \
 && rm -f /tmp/workspace-fuse-profile-bundle.json

RUN /usr/local/bin/workspace-mounter health prepared --self-check-image

WORKDIR /workspace
ENTRYPOINT ["/usr/local/bin/workspace-mounter", "supervise"]
