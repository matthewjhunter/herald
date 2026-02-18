# Stage 1: Build
FROM golang:1.24-alpine AS builder
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN go build -o /herald ./cmd/herald
RUN go build -o /herald-web ./cmd/herald-web

# Stage 2: Runtime
FROM alpine:3.21
RUN apk add --no-cache ca-certificates tzdata
COPY --from=builder /herald /usr/local/bin/
COPY --from=builder /herald-web /usr/local/bin/
RUN mkdir -p /data /etc/herald
VOLUME ["/data"]
EXPOSE 8080
