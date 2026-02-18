// herald-mcp is a standalone MCP server for the Herald content engine.
// It connects directly to Herald's SQLite database, serving article and
// feed tools over JSON-RPC stdio. Designed to run as a per-persona MCP
// server alongside majordomo-mcp.
//
// With --poll, it also runs a background goroutine that fetches feeds and
// scores articles on a timer, replacing the polling loop previously
// embedded in the majordomo daemon.
package main

import (
	"context"
	"flag"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/matthewjhunter/herald"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func main() {
	home, _ := os.UserHomeDir()
	defaultDB := filepath.Join(home, ".local", "share", "majordomo", "mcp", "herald", "herald.db")

	dbPath := flag.String("db", defaultDB, "path to herald database")
	ollamaURL := flag.String("ollama", "http://localhost:11434", "Ollama base URL")
	userID := flag.Int64("user", 1, "user ID for article operations")
	poll := flag.Bool("poll", false, "enable background feed polling")
	pollInterval := flag.Duration("poll-interval", 10*time.Minute, "polling frequency")
	threshold := flag.Float64("threshold", 8.0, "high-interest score threshold")
	securityModel := flag.String("security-model", "gemma3:4b", "Ollama model for security scoring")
	curationModel := flag.String("curation-model", "llama3", "Ollama model for interest scoring")
	securityThreshold := flag.Float64("security-threshold", 7.0, "security score threshold")
	keywords := flag.String("keywords", "", "comma-separated interest keywords")
	flag.Parse()

	var kwList []string
	if *keywords != "" {
		for _, kw := range strings.Split(*keywords, ",") {
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
	}

	engine, err := herald.NewEngine(engineCfg)
	if err != nil {
		log.Fatalf("create herald engine: %v", err)
	}
	defer engine.Close()

	log.SetOutput(os.Stderr)

	hs := newHeraldServer(engine, *userID)

	if *poll {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		p := newPoller(engine, *userID, *pollInterval, *threshold)
		p.start(ctx)
		defer p.stop()

		hs.poller = p
	}

	log.Printf("herald-mcp starting (user=%d)", hs.userID)

	mcpSrv := newMCPServer(hs)
	if err := mcpSrv.Run(context.Background(), &mcp.StdioTransport{}); err != nil {
		log.Fatalf("server error: %v", err)
	}
}
