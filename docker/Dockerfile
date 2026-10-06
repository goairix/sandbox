ARG GO_BUILDER_IMAGE=golang:1.25.4
ARG API_BASE_IMAGE=alpine:3.22
FROM --platform=$BUILDPLATFORM ${GO_BUILDER_IMAGE} AS builder
ARG TARGETOS
ARG TARGETARCH

WORKDIR /app
COPY go.mod go.sum ./
RUN export GOPROXY=https://goproxy.cn,direct && go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -o /sandbox ./cmd/sandbox/

FROM ${API_BASE_IMAGE}
WORKDIR /app
RUN apk add --no-cache ca-certificates docker-cli
COPY --from=builder /sandbox /app/sandbox
COPY --from=builder /app/var /app/var
COPY configs/config.yaml /etc/sandbox/config.yaml

EXPOSE 8080

ENTRYPOINT ["/app/sandbox"]
CMD ["--config", "/etc/sandbox/config.yaml"]
