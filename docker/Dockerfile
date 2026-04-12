FROM golang:1.25.4 AS builder

WORKDIR /app
COPY go.mod go.sum ./
RUN export GOPROXY=https://goproxy.cn,direct && go mod download
COPY . .
RUN CGO_ENABLED=0 go build -o /sandbox ./cmd/sandbox/

FROM alpine:3.22
RUN apk add --no-cache ca-certificates docker-cli
COPY --from=builder /sandbox /usr/local/bin/sandbox
COPY configs/config.yaml /etc/sandbox/config.yaml

EXPOSE 8080

ENTRYPOINT ["sandbox"]
CMD ["--config", "/etc/sandbox/config.yaml"]
