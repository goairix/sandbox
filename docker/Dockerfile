FROM golang:1.25-alpine AS builder

WORKDIR /app
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -o /sandbox ./cmd/sandbox/

FROM alpine:3.20
RUN apk add --no-cache ca-certificates docker-cli
COPY --from=builder /sandbox /usr/local/bin/sandbox
COPY configs/config.yaml /etc/sandbox/config.yaml

EXPOSE 8080

ENTRYPOINT ["sandbox"]
CMD ["--config", "/etc/sandbox/config.yaml"]
