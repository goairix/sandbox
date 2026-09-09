ARG WORKSPACE_PROBE_BUILDER=golang:1.25-alpine
FROM ${WORKSPACE_PROBE_BUILDER} AS workspace-probe-builder

WORKDIR /src
COPY go.mod go.sum ./
COPY cmd/workspace-probe ./cmd/workspace-probe
COPY internal/fuseprotocol ./internal/fuseprotocol
COPY internal/workspaceprobe ./internal/workspaceprobe
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags='-s -w' \
    -o /out/workspace-probe ./cmd/workspace-probe

FROM python:3.13-slim-bookworm

COPY docker/images/sandbox/install-runtime.sh /usr/local/bin/install-sandbox-runtime
RUN /usr/local/bin/install-sandbox-runtime \
 && rm -f /usr/local/bin/install-sandbox-runtime

ENV PLAYWRIGHT_BROWSERS_PATH=/opt/playwright-browsers
ENV NODE_PATH=/opt/sandbox-libs/node_modules

# Non-privileged fixed-path workspace health/quiesce probe. The ordinary image
# intentionally does not contain s3fs or the root workspace supervisor.
COPY --from=workspace-probe-builder /out/workspace-probe /usr/local/bin/workspace-probe
RUN chown root:root /usr/local/bin/workspace-probe \
 && chmod 0755 /usr/local/bin/workspace-probe

WORKDIR /workspace
USER sandbox

CMD ["sleep", "infinity"]
