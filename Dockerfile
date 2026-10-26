# Stage 1: Build
FROM golang:1.26-alpine AS builder
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG GIT_SHA=dev
ARG BUILD_TIME=unknown
RUN CGO_ENABLED=0 go build -trimpath -ldflags "-X github.com/matthewjhunter/herald/internal/web.version=${GIT_SHA} -X github.com/matthewjhunter/herald/internal/web.buildTime=${BUILD_TIME}" -o /herald ./cmd/herald

# Stage 2: Runtime
# Distroless static: CA bundle, tzdata and the nonroot user, nothing else.
# Suite pinned in the tag; the floating "static" tag trails a Debian release.
# The binary stays on PATH at /usr/local/bin so a bare `herald ...` command
# in compose or a docker run still resolves.
FROM gcr.io/distroless/static-debian13:nonroot
COPY --from=builder /herald /usr/local/bin/herald
# Bake the default container config so `herald serve --config
# /etc/herald/config.docker.toml` works in images without a mounted config
# (e.g. the PR-preview container). docker-compose mounts ./config over this,
# so a real deployment still wins.
COPY config/config.docker.toml /etc/herald/config.docker.toml
EXPOSE 8080
USER nonroot:nonroot
