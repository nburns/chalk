package main

import (
	"context"
	"crypto/subtle"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	_ "modernc.org/sqlite"
)

func requireBearer(key string, next http.Handler) http.Handler {
	expected := []byte("Bearer " + key)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got := []byte(r.Header.Get("Authorization"))
		if subtle.ConstantTimeCompare(got, expected) != 1 {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func runReaper(ctx context.Context, d *DB, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			n, err := d.reap(ctx)
			if err != nil {
				log.Printf("reaper: %v", err)
			} else if n > 0 {
				log.Printf("reaper: archived %d expired working_on entries", n)
			}
		case <-ctx.Done():
			return
		}
	}
}

func main() {
	dbPath := os.Getenv("BLACKBOARD_DB")
	if dbPath == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			log.Fatalf("home dir: %v", err)
		}
		dbPath = filepath.Join(home, ".blackboard", "board.db")
	}

	host := def(os.Getenv("BLACKBOARD_HOST"), "127.0.0.1")
	port := def(os.Getenv("BLACKBOARD_PORT"), "8080")
	apiKey := os.Getenv("BLACKBOARD_API_KEY")

	reaperInterval := 60 * time.Second
	if s := os.Getenv("BLACKBOARD_REAPER_INTERVAL"); s != "" {
		d, err := time.ParseDuration(s)
		if err != nil {
			log.Fatalf("BLACKBOARD_REAPER_INTERVAL: %v", err)
		}
		reaperInterval = d
	}

	db, err := openDB(dbPath)
	if err != nil {
		log.Fatalf("db: %v", err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	go runReaper(ctx, db, reaperInterval)

	srv := mcp.NewServer(&mcp.Implementation{Name: "blackboard", Version: "1.0.0"}, nil)

	mcp.AddTool(srv, &mcp.Tool{
		Name: "post",
		Description: fmt.Sprintf(
			"Post an entry to the blackboard. "+
				"type: %s (default: note). "+
				"source: human|agent (default: agent). "+
				"expires_at (ISO8601) is required when type=working_on. "+
				"tags is a comma-separated string.",
			typeList(),
		),
	}, db.handlePost)

	mcp.AddTool(srv, &mcp.Tool{
		Name: "read",
		Description: "Read entries from the blackboard, newest first. " +
			"status defaults to 'active'; pass 'all' to include done and archived. " +
			"since accepts ISO8601 or 'YYYY-MM-DD HH:MM:SS' to return only entries created after that time. " +
			"limit defaults to 20.",
	}, db.handleRead)

	mcp.AddTool(srv, &mcp.Tool{
		Name: "search",
		Description: "Full-text search the blackboard over content, tags, and agent_id. " +
			"Returns matching entries with a snippet showing context around the match. " +
			"Excludes archived entries. limit defaults to 10.",
	}, db.handleSearch)

	mcp.AddTool(srv, &mcp.Tool{
		Name: "update",
		Description: "Update an existing entry. Any agent may update any entry. " +
			"Provide at least one of: content, tags, status (active|done|archived).",
	}, db.handleUpdate)

	mcp.AddTool(srv, &mcp.Tool{
		Name:        "mark_done",
		Description: "Mark an entry as done. Convenience wrapper for update with status=done.",
	}, db.handleMarkDone)

	mcp.AddTool(srv, &mcp.Tool{
		Name: "delete",
		Description: "Delete an entry. " +
			"hard=false archives it (soft delete, reversible via update). " +
			"hard=true permanently deletes the row and its relations; requires instructed_by_human=true.",
	}, db.handleDelete)

	mcp.AddTool(srv, &mcp.Tool{
		Name:        "relate",
		Description: "Create a typed relationship between two entries. kind: responds_to|refutes|supports|follows_from|blocks.",
	}, db.handleRelate)

	mcp.AddTool(srv, &mcp.Tool{
		Name:        "relations",
		Description: "Fetch all relation rows for an entry (both directions). Retrieve the actual entries separately with read.",
	}, db.handleRelations)

	mcp.AddTool(srv, &mcp.Tool{
		Name:        "stats",
		Description: "Board summary: entry counts by type (active entries only), counts by status across all entries, and list of distinct active agent IDs.",
	}, db.handleStats)

	srv.AddPrompt(&mcp.Prompt{
		Name:        "board-usage",
		Description: "Coordination conventions for using the blackboard — read this before posting.",
	}, handleBoardUsage)

	handler := mcp.NewStreamableHTTPHandler(func(_ *http.Request) *mcp.Server {
		return srv
	}, nil)

	mux := http.NewServeMux()
	mux.Handle("/mcp", handler)

	var rootHandler http.Handler = mux
	if apiKey != "" {
		rootHandler = requireBearer(apiKey, mux)
		log.Printf("API key authentication enabled")
	} else if host != "127.0.0.1" && host != "localhost" {
		log.Fatalf("BLACKBOARD_API_KEY must be set when binding to %s", host)
	}

	addr := host + ":" + port
	httpSrv := &http.Server{
		Addr:         addr,
		Handler:      rootHandler,
		ReadTimeout:  30 * time.Second,
		WriteTimeout: 60 * time.Second,
		IdleTimeout:  120 * time.Second,
	}

	log.Printf("blackboard listening on http://%s/mcp  db=%s", addr, dbPath)

	go func() {
		<-ctx.Done()
		shutCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := httpSrv.Shutdown(shutCtx); err != nil {
			log.Printf("shutdown: %v", err)
		}
	}()

	if err := httpSrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Fatalf("listen: %v", err)
	}
}
