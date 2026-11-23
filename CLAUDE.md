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

## Image builds

A push to `main` publishes `ghcr.io/matthewjhunter/herald:latest`, and a `v*` tag publishes the version tags. A push to `main` that changes nothing in the image builds nothing.

"Nothing in the image" means every changed path is excluded by `.dockerignore`; that file is the only list (`.github/scripts/deploy-scope` reads it). To stop a path from triggering builds, add it to `.dockerignore`, and only if the image really does not need it. Anything not listed builds. A change to `Dockerfile` or `.dockerignore` always builds, and so does a tag.

The PR preview workflow uses the same rule against everything the PR changes: a PR that touches nothing in the image gets no preview.

One consequence: the image stamps the commit it was built from, so after a skipped push `:latest` reports the last commit that changed the image, not the tip of `main`.
