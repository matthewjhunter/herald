# Herald

AI-powered RSS/Atom feed reader with security screening and content curation. Monitors feeds, scores articles for relevance using Ollama models, and surfaces high-interest items as formatted notification output.

Two binaries: `herald` (CLI) and `herald-web` (web interface).

## Build

```bash
go build -o herald ./cmd/herald
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
