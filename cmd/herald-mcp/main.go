// herald-mcp is a standalone MCP server for the Herald content engine.
// It connects directly to Herald's SQLite database, serving article and
// feed tools over JSON-RPC stdio. Designed to run as a per-persona MCP
// server alongside majordomo-mcp.
package main

import (
	"flag"
	"log"
	"os"
	"path/filepath"

	"github.com/matthewjhunter/herald"
)

func main() {
	home, _ := os.UserHomeDir()
	defaultDB := filepath.Join(home, ".local", "share", "majordomo", "herald.db")

	dbPath := flag.String("db", defaultDB, "path to herald database")
	ollamaURL := flag.String("ollama", "http://localhost:11434", "Ollama base URL")
	userID := flag.Int64("user", 1, "user ID for article operations")
	flag.Parse()

	engine, err := herald.NewEngine(herald.EngineConfig{
		DBPath:        *dbPath,
		OllamaBaseURL: *ollamaURL,
	})
	if err != nil {
		log.Fatalf("create herald engine: %v", err)
	}
	defer engine.Close()

	srv := newServer(engine, *userID)
	if err := srv.run(); err != nil {
		log.Fatalf("server error: %v", err)
	}
}
