# Stage 1: Build
FROM golang:1.26-alpine AS builder
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG GIT_SHA=dev
ARG BUILD_TIME=unknown
RUN go build -ldflags "-X github.com/matthewjhunter/herald/internal/web.version=${GIT_SHA} -X github.com/matthewjhunter/herald/internal/web.buildTime=${BUILD_TIME}" -o /herald ./cmd/herald

# Stage 2: Runtime
FROM alpine:3.21
RUN apk add --no-cache ca-certificates tzdata
COPY --from=builder /herald /usr/local/bin/
RUN mkdir -p /data /etc/herald
# Bake the default container config so `herald serve --config
# /etc/herald/config.docker.toml` works in images without a mounted config
# (e.g. the PR-preview container). docker-compose mounts ./config over this,
# so a real deployment still wins.
COPY config/config.docker.toml /etc/herald/config.docker.toml
VOLUME ["/data"]
EXPOSE 8080
