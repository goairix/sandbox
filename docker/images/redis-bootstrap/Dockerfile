# syntax=docker/dockerfile:1
FROM --platform=$BUILDPLATFORM golang:1.25.4 AS builder
ARG TARGETOS=linux
ARG TARGETARCH
WORKDIR /src
COPY go.mod go.sum ./
RUN GOPROXY=https://goproxy.cn,direct go mod download
COPY cmd/redis-bootstrap ./cmd/redis-bootstrap
COPY internal ./internal
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -trimpath -ldflags='-s -w' -o /out/redis-bootstrap ./cmd/redis-bootstrap

FROM alpine:3.22
RUN apk add --no-cache ca-certificates
WORKDIR /app
COPY --from=builder /out/redis-bootstrap /app/redis-bootstrap
ENTRYPOINT ["/app/redis-bootstrap"]
