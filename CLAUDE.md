# Herald

AI-powered RSS/Atom feed reader with security screening and content curation. Monitors feeds, scores articles for relevance using Ollama models, and announces high-interest items via Majordomo voice notifications.

Three binaries: `herald` (CLI), `herald-mcp` (MCP server), `herald-web` (web interface).

## Build

```bash
go build -o herald ./cmd/herald
go build -o herald-mcp ./cmd/herald-mcp
go build -o herald-web ./cmd/herald-web

# Or build all:
task build-all
```

## Test

```bash
go test -race -count=1 ./...
# or
task test
```

## Lint

```bash
golangci-lint run ./...
# or
task lint
```

## Vulnerability Check

```bash
govulncheck ./...
# or
task vulncheck
```

## All CI Checks

```bash
task check
```
