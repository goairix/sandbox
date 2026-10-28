ARG WORKSPACE_PROBE_BUILDER=golang:1.25-alpine
FROM ${WORKSPACE_PROBE_BUILDER} AS workspace-probe-builder

ARG TARGETARCH

WORKDIR /src
ENV GOPROXY=https://goproxy.cn,direct
COPY go.mod go.sum ./
COPY cmd/workspace-probe ./cmd/workspace-probe
COPY cmd/sandbox-file-op ./cmd/sandbox-file-op
COPY internal/fuseprotocol ./internal/fuseprotocol
COPY internal/filev2 ./internal/filev2
COPY internal/workspaceprobe ./internal/workspaceprobe
COPY docker/images/common/install-ripgrep-14.1.1.sh /usr/local/bin/install-ripgrep
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags='-s -w' \
    -o /out/workspace-probe ./cmd/workspace-probe \
 && CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags='-s -w' \
    -o /out/sandbox-file-op ./cmd/sandbox-file-op
RUN apk add --no-cache curl \
 && TARGETARCH="$TARGETARCH" sh /usr/local/bin/install-ripgrep

FROM python:3.13-slim-bookworm

COPY docker/images/sandbox/install-runtime.sh /usr/local/bin/install-sandbox-runtime
RUN /usr/local/bin/install-sandbox-runtime \
 && rm -f /usr/local/bin/install-sandbox-runtime

ENV PLAYWRIGHT_BROWSERS_PATH=/opt/playwright-browsers
ENV NODE_PATH=/opt/sandbox-libs/node_modules

# Non-privileged fixed-path workspace health/quiesce probe. The ordinary image
# intentionally does not contain s3fs or the root workspace supervisor.
COPY --from=workspace-probe-builder /out/workspace-probe /usr/local/bin/workspace-probe
COPY --from=workspace-probe-builder /out/sandbox-file-op /usr/local/bin/sandbox-file-op
COPY --from=workspace-probe-builder /out/rg /usr/local/bin/rg
RUN chown root:root /usr/local/bin/workspace-probe /usr/local/bin/sandbox-file-op /usr/local/bin/rg \
 && chmod 0755 /usr/local/bin/workspace-probe /usr/local/bin/sandbox-file-op /usr/local/bin/rg

WORKDIR /workspace
USER sandbox

CMD ["sleep", "infinity"]
