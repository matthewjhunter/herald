# Herald

AI-powered RSS/Atom feed reader with security screening and content curation. Monitors feeds, scores articles for relevance using Ollama models, and surfaces high-interest items as formatted notification output.

Single binary `herald` with subcommands (including `herald serve` for the web UI; `herald daemon` for polling).

## Build

```bash
go build -o herald ./cmd/herald

# Or:
task build
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
