# syntax=docker/dockerfile:1.7

FROM golang:1.26-alpine AS builder
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build \
    -trimpath -ldflags='-s -w' \
    -o /out/deployd ./cmd/deployd

# docker:cli ships the docker client and the compose plugin.
FROM docker:28-cli
RUN apk add --no-cache ca-certificates tzdata
COPY --from=builder /out/deployd /usr/local/bin/deployd
EXPOSE 8080
ENTRYPOINT ["/usr/local/bin/deployd"]
CMD ["-config", "/etc/deployd/config.yaml"]
