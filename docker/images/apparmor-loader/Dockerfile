# syntax=docker/dockerfile:1
ARG GO_BUILDER_IMAGE=golang:1.25-bookworm
ARG APPARMOR_BASE_IMAGE=debian:13-slim
FROM --platform=$BUILDPLATFORM ${GO_BUILDER_IMAGE} AS builder
ARG TARGETOS=linux
ARG TARGETARCH
WORKDIR /src
COPY go.mod go.sum ./
COPY internal/apparmorloader ./internal/apparmorloader
COPY cmd/apparmor-loader ./cmd/apparmor-loader
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath -ldflags="-s -w" -o /out/apparmor-loader ./cmd/apparmor-loader

FROM ${APPARMOR_BASE_IMAGE}
RUN apt-get update \
    && apt-get install -y --no-install-recommends apparmor \
    && apt-get clean \
    && rm -rf /var/lib/apt/lists/*
COPY --from=builder /out/apparmor-loader /usr/local/bin/apparmor-loader
ENTRYPOINT ["/usr/local/bin/apparmor-loader"]
