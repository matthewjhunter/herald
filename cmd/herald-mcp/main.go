// herald-mcp is a standalone, read-only MCP server for the Herald content
// engine. It connects directly to Herald's SQLite database, serving article
// and feed tools over JSON-RPC stdio.
//
// It does no feed fetching or AI processing -- that is the `herald daemon`'s
// job. herald-mcp only reads the database the daemon populates.
package main

import (
	"context"
	"flag"
	"log"
	"os"
	"path/filepath"
	"strings"

	"github.com/matthewjhunter/herald"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func main() {
	home, _ := os.UserHomeDir()
	defaultDB := filepath.Join(home, ".local", "share", "herald", "herald.db")

	dbPath := flag.String("db", defaultDB, "path to herald database")
	ollamaURL := flag.String("ollama", "http://localhost:11434", "Ollama base URL")
	userID := flag.Int64("user", 1, "user ID for article operations")
	threshold := flag.Float64("threshold", 8.0, "high-interest score threshold")
	securityModel := flag.String("security-model", "gemma3:4b", "Ollama model for security scoring")
	curationModel := flag.String("curation-model", "llama3.1:8b", "Ollama model for interest scoring")
	securityThreshold := flag.Float64("security-threshold", 7.0, "security score threshold")
	keywords := flag.String("keywords", "", "comma-separated interest keywords")
	maxParallel := flag.Int("max-parallel", 1, "max concurrent AI pipeline workers")
	flag.Parse()

	var kwList []string
	if *keywords != "" {
		for kw := range strings.SplitSeq(*keywords, ",") {
			if trimmed := strings.TrimSpace(kw); trimmed != "" {
				kwList = append(kwList, trimmed)
			}
		}
	}

	engineCfg := herald.EngineConfig{
		DBPath:            *dbPath,
		OllamaBaseURL:     *ollamaURL,
		SecurityModel:     *securityModel,
		CurationModel:     *curationModel,
		InterestThreshold: *threshold,
		SecurityThreshold: *securityThreshold,
		Keywords:          kwList,
		UserID:            *userID,
		MaxParallel:       *maxParallel,
	}

	engine, err := herald.NewEngine(engineCfg)
	if err != nil {
		log.Fatalf("create herald engine: %v", err)
	}
	defer engine.Close()

	log.SetOutput(os.Stderr)

	hs := newHeraldServer(engine, *userID)

	log.Printf("herald-mcp starting (user=%d, read-only)", hs.userID)

	mcpSrv := newMCPServer(hs)
	if err := mcpSrv.Run(context.Background(), &mcp.StdioTransport{}); err != nil {
		log.Fatalf("server error: %v", err)
	}
}
