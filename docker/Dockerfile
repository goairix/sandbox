FROM golang:1.25.4 AS builder

WORKDIR /app
COPY go.mod go.sum ./
RUN export GOPROXY=https://goproxy.cn,direct && go mod download
COPY . .
RUN CGO_ENABLED=0 go build -o /sandbox ./cmd/sandbox/

FROM alpine:3.22
WORKDIR /app
RUN apk add --no-cache ca-certificates docker-cli
COPY --from=builder /sandbox /app/sandbox
COPY --from=builder /app/var /app/var
COPY configs/config.yaml /etc/sandbox/config.yaml

EXPOSE 8080

ENTRYPOINT ["/app/sandbox"]
CMD ["--config", "/etc/sandbox/config.yaml"]
