package main

import (
	"context"
	"crypto/subtle"
	"database/sql"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	_ "modernc.org/sqlite"
)

var (
	validTypes = map[string]bool{
		"working_on": true, "learned": true, "hypothesis": true,
		"question": true, "result": true, "handoff": true,
		"intent": true, "blocked": true, "note": true,
	}
	validStatuses = map[string]bool{"active": true, "done": true, "archived": true}
	validSources  = map[string]bool{"human": true, "agent": true}
	validKinds    = map[string]bool{
		"responds_to": true, "refutes": true, "supports": true,
		"follows_from": true, "blocks": true,
	}
)

func typeList() string {
	keys := make([]string, 0, len(validTypes))
	for k := range validTypes {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return strings.Join(keys, "|")
}

func def(val, fallback string) string {
	if val == "" {
		return fallback
	}
	return val
}

// --- arg structs ---

type PostArgs struct {
	Content   string `json:"content"`
	Type      string `json:"type,omitempty"`
	AgentID   string `json:"agent_id,omitempty"`
	Source    string `json:"source,omitempty"`
	Tags      string `json:"tags,omitempty"`
	ExpiresAt string `json:"expires_at,omitempty"`
}

type ReadArgs struct {
	Type    string `json:"type,omitempty"`
	Source  string `json:"source,omitempty"`
	AgentID string `json:"agent_id,omitempty"`
	Status  string `json:"status,omitempty"`
	Since   string `json:"since,omitempty"`
	Limit   int    `json:"limit,omitempty"`
}

type SearchArgs struct {
	Query string `json:"query"`
	Limit int    `json:"limit,omitempty"`
}

type UpdateArgs struct {
	ID      int    `json:"id"`
	Content string `json:"content,omitempty"`
	Tags    string `json:"tags,omitempty"`
	Status  string `json:"status,omitempty"`
}

type MarkDoneArgs struct {
	ID int `json:"id"`
}

type DeleteArgs struct {
	ID                int  `json:"id"`
	Hard              bool `json:"hard"`
	InstructedByHuman bool `json:"instructed_by_human,omitempty"`
}

type RelateArgs struct {
	FromID int    `json:"from_id"`
	ToID   int    `json:"to_id"`
	Kind   string `json:"kind"`
}

type RelationsArgs struct {
	ID int `json:"id"`
}

type StatsArgs struct{}

// --- result types ---

type Entry struct {
	ID        int64   `json:"id"`
	AgentID   string  `json:"agent_id"`
	Source    string  `json:"source"`
	Type      string  `json:"type"`
	Status    string  `json:"status"`
	Content   string  `json:"content"`
	Tags      string  `json:"tags"`
	ExpiresAt *string `json:"expires_at,omitempty"`
	CreatedAt string  `json:"created_at"`
	UpdatedAt string  `json:"updated_at"`
}

type SearchResult struct {
	Entry
	Snippet string `json:"snippet"`
}

type Relation struct {
	ID        int64  `json:"id"`
	FromID    int64  `json:"from_id"`
	ToID      int64  `json:"to_id"`
	Kind      string `json:"kind"`
	CreatedAt string `json:"created_at"`
	UpdatedAt string `json:"updated_at"`
}

type Stats struct {
	ByType   map[string]int `json:"by_type"`
	ByStatus map[string]int `json:"by_status"`
	Agents   []string       `json:"active_agents"`
}

// --- DB ---

type DB struct {
	db *sql.DB
}

var schemaStmts = []string{
	`CREATE TABLE IF NOT EXISTS entries (
		id         INTEGER  PRIMARY KEY AUTOINCREMENT,
		agent_id   TEXT     NOT NULL DEFAULT '',
		source     TEXT     NOT NULL DEFAULT 'agent',
		type       TEXT     NOT NULL DEFAULT 'note',
		status     TEXT     NOT NULL DEFAULT 'active',
		content    TEXT     NOT NULL,
		tags       TEXT     NOT NULL DEFAULT '',
		expires_at DATETIME,
		created_at DATETIME NOT NULL DEFAULT (datetime('now')),
		updated_at DATETIME NOT NULL DEFAULT (datetime('now'))
	)`,
	`CREATE INDEX IF NOT EXISTS idx_entries_type_status ON entries(type, status)`,
	`CREATE INDEX IF NOT EXISTS idx_entries_status      ON entries(status)`,
	`CREATE INDEX IF NOT EXISTS idx_entries_agent_id    ON entries(agent_id)`,
	`CREATE INDEX IF NOT EXISTS idx_entries_source      ON entries(source)`,
	`CREATE INDEX IF NOT EXISTS idx_entries_created_at  ON entries(created_at)`,
	`CREATE INDEX IF NOT EXISTS idx_entries_expires_at  ON entries(expires_at)`,
	`CREATE TABLE IF NOT EXISTS entry_relations (
		id         INTEGER  PRIMARY KEY AUTOINCREMENT,
		from_id    INTEGER  NOT NULL REFERENCES entries(id) ON DELETE CASCADE,
		to_id      INTEGER  NOT NULL REFERENCES entries(id) ON DELETE CASCADE,
		kind       TEXT     NOT NULL,
		created_at DATETIME NOT NULL DEFAULT (datetime('now')),
		updated_at DATETIME NOT NULL DEFAULT (datetime('now'))
	)`,
	`CREATE INDEX IF NOT EXISTS idx_relations_from_id ON entry_relations(from_id)`,
	`CREATE INDEX IF NOT EXISTS idx_relations_to_id   ON entry_relations(to_id)`,
	`CREATE VIRTUAL TABLE IF NOT EXISTS entries_fts USING fts5(
		content, tags, agent_id,
		content=entries,
		content_rowid=id
	)`,
	`CREATE TRIGGER IF NOT EXISTS entries_ai AFTER INSERT ON entries BEGIN
		INSERT INTO entries_fts(rowid, content, tags, agent_id)
		VALUES (new.id, new.content, new.tags, new.agent_id);
	END`,
	`CREATE TRIGGER IF NOT EXISTS entries_ad AFTER DELETE ON entries BEGIN
		INSERT INTO entries_fts(entries_fts, rowid, content, tags, agent_id)
		VALUES ('delete', old.id, old.content, old.tags, old.agent_id);
	END`,
	// only rebuild FTS when the indexed columns actually change
	`CREATE TRIGGER IF NOT EXISTS entries_au AFTER UPDATE OF content, tags, agent_id ON entries BEGIN
		INSERT INTO entries_fts(entries_fts, rowid, content, tags, agent_id)
		VALUES ('delete', old.id, old.content, old.tags, old.agent_id);
		INSERT INTO entries_fts(rowid, content, tags, agent_id)
		VALUES (new.id, new.content, new.tags, new.agent_id);
	END`,
}

func openDB(path string) (*DB, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, fmt.Errorf("create db dir: %w", err)
	}
	sqldb, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("open: %w", err)
	}
	// single connection serializes writes; WAL still allows external concurrent reads
	sqldb.SetMaxOpenConns(1)

	for _, pragma := range []string{
		"PRAGMA journal_mode=WAL",
		"PRAGMA synchronous=NORMAL",
		"PRAGMA busy_timeout=5000",
		"PRAGMA foreign_keys=ON",
		"PRAGMA temp_store=MEMORY",
		"PRAGMA cache_size=-64000",
	} {
		if _, err := sqldb.Exec(pragma); err != nil {
			return nil, fmt.Errorf("pragma %q: %w", pragma, err)
		}
	}

	d := &DB{db: sqldb}
	if err := d.migrate(); err != nil {
		return nil, fmt.Errorf("migrate: %w", err)
	}
	return d, nil
}

func (d *DB) migrate() error {
	tx, err := d.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, stmt := range schemaStmts {
		if _, err := tx.Exec(stmt); err != nil {
			return fmt.Errorf("%.60q: %w", stmt, err)
		}
	}
	return tx.Commit()
}

// --- helpers ---

func toolErr(msg string) (*mcp.CallToolResult, any, error) {
	return &mcp.CallToolResult{
		Content: []mcp.Content{&mcp.TextContent{Text: msg}},
		IsError: true,
	}, nil, nil
}

func toolJSON(v any) (*mcp.CallToolResult, any, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return toolErr(fmt.Sprintf("marshal result: %v", err))
	}
	return &mcp.CallToolResult{
		Content: []mcp.Content{&mcp.TextContent{Text: string(b)}},
	}, nil, nil
}

func scanEntry(row *sql.Row) (Entry, error) {
	var e Entry
	err := row.Scan(&e.ID, &e.AgentID, &e.Source, &e.Type, &e.Status,
		&e.Content, &e.Tags, &e.ExpiresAt, &e.CreatedAt, &e.UpdatedAt)
	return e, err
}

func scanEntries(rows *sql.Rows) ([]Entry, error) {
	var entries []Entry
	for rows.Next() {
		var e Entry
		if err := rows.Scan(&e.ID, &e.AgentID, &e.Source, &e.Type, &e.Status,
			&e.Content, &e.Tags, &e.ExpiresAt, &e.CreatedAt, &e.UpdatedAt); err != nil {
			return nil, err
		}
		entries = append(entries, e)
	}
	return entries, rows.Err()
}

// queryKV runs a two-column (string, int) aggregate query and closes rows before returning.
func (d *DB) queryKV(ctx context.Context, q string) (map[string]int, error) {
	rows, err := d.db.QueryContext(ctx, q)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	m := make(map[string]int)
	for rows.Next() {
		var k string
		var v int
		if err := rows.Scan(&k, &v); err != nil {
			return nil, err
		}
		m[k] = v
	}
	return m, rows.Err()
}

// queryStrings runs a single-column string query and closes rows before returning.
func (d *DB) queryStrings(ctx context.Context, q string) ([]string, error) {
	rows, err := d.db.QueryContext(ctx, q)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			return nil, err
		}
		result = append(result, s)
	}
	return result, rows.Err()
}

// --- tool handlers ---

const maxContentBytes = 256 * 1024 // 256KB

func (d *DB) handlePost(ctx context.Context, _ *mcp.CallToolRequest, args PostArgs) (*mcp.CallToolResult, any, error) {
	if strings.TrimSpace(args.Content) == "" {
		return toolErr("content is required")
	}
	if len(args.Content) > maxContentBytes {
		return toolErr(fmt.Sprintf("content exceeds maximum size of %d bytes", maxContentBytes))
	}
	entryType := def(args.Type, "note")
	if !validTypes[entryType] {
		return toolErr(fmt.Sprintf("invalid type %q; valid: %s", entryType, typeList()))
	}
	source := def(args.Source, "agent")
	if !validSources[source] {
		return toolErr("source must be 'human' or 'agent'")
	}
	if entryType == "working_on" && strings.TrimSpace(args.ExpiresAt) == "" {
		return toolErr("expires_at is required when type is 'working_on'")
	}
	var expiresAt *string
	if args.ExpiresAt != "" {
		expiresAt = &args.ExpiresAt
	}
	row := d.db.QueryRowContext(ctx, `
		INSERT INTO entries (agent_id, source, type, status, content, tags, expires_at)
		VALUES (?, ?, ?, 'active', ?, ?, ?)
		RETURNING id, agent_id, source, type, status, content, tags, expires_at, created_at, updated_at`,
		args.AgentID, source, entryType, args.Content, args.Tags, expiresAt,
	)
	e, err := scanEntry(row)
	if err != nil {
		log.Printf("post: %v", err)
		return toolErr(fmt.Sprintf("post failed: %v", err))
	}
	return toolJSON(e)
}

func (d *DB) handleRead(ctx context.Context, _ *mcp.CallToolRequest, args ReadArgs) (*mcp.CallToolResult, any, error) {
	limit := args.Limit
	if limit <= 0 {
		limit = 20
	}
	status := def(args.Status, "active")
	if status != "all" && !validStatuses[status] {
		return toolErr("status must be one of: active|done|archived|all")
	}
	if args.Type != "" && !validTypes[args.Type] {
		return toolErr(fmt.Sprintf("invalid type %q; valid: %s", args.Type, typeList()))
	}
	if args.Source != "" && !validSources[args.Source] {
		return toolErr("source must be 'human' or 'agent'")
	}

	q := `SELECT id, agent_id, source, type, status, content, tags, expires_at, created_at, updated_at
	      FROM entries WHERE 1=1`
	var params []any

	if status != "all" {
		q += " AND status = ?"
		params = append(params, status)
	}
	if args.Type != "" {
		q += " AND type = ?"
		params = append(params, args.Type)
	}
	if args.Source != "" {
		q += " AND source = ?"
		params = append(params, args.Source)
	}
	if args.AgentID != "" {
		q += " AND agent_id = ?"
		params = append(params, args.AgentID)
	}
	if args.Since != "" {
		// normalize input to SQLite's stored format so ISO8601 with T/Z compares correctly
		q += " AND created_at > strftime('%Y-%m-%d %H:%M:%S', ?)"
		params = append(params, args.Since)
	}
	q += " ORDER BY created_at DESC LIMIT ?"
	params = append(params, limit)

	rows, err := d.db.QueryContext(ctx, q, params...)
	if err != nil {
		log.Printf("read: %v", err)
		return toolErr(fmt.Sprintf("read failed: %v", err))
	}
	defer rows.Close()

	entries, err := scanEntries(rows)
	if err != nil {
		log.Printf("read scan: %v", err)
		return toolErr(fmt.Sprintf("scan failed: %v", err))
	}
	if entries == nil {
		entries = []Entry{}
	}
	return toolJSON(entries)
}

func (d *DB) handleSearch(ctx context.Context, _ *mcp.CallToolRequest, args SearchArgs) (*mcp.CallToolResult, any, error) {
	if strings.TrimSpace(args.Query) == "" {
		return toolErr("query is required")
	}
	limit := args.Limit
	if limit <= 0 {
		limit = 10
	}
	rows, err := d.db.QueryContext(ctx, `
		SELECT e.id, e.agent_id, e.source, e.type, e.status, e.content, e.tags,
		       e.expires_at, e.created_at, e.updated_at,
		       snippet(entries_fts, 0, '[', ']', '...', 15) AS snippet
		FROM entries_fts
		JOIN entries e ON e.id = entries_fts.rowid
		WHERE entries_fts MATCH ?
		  AND e.status != 'archived'
		ORDER BY rank
		LIMIT ?`,
		args.Query, limit,
	)
	if err != nil {
		log.Printf("search: %v", err)
		return toolErr(fmt.Sprintf("search failed: %v", err))
	}
	defer rows.Close()

	var results []SearchResult
	for rows.Next() {
		var r SearchResult
		if err := rows.Scan(&r.ID, &r.AgentID, &r.Source, &r.Type, &r.Status,
			&r.Content, &r.Tags, &r.ExpiresAt, &r.CreatedAt, &r.UpdatedAt, &r.Snippet); err != nil {
			return toolErr(fmt.Sprintf("scan failed: %v", err))
		}
		results = append(results, r)
	}
	if err := rows.Err(); err != nil {
		return toolErr(fmt.Sprintf("rows error: %v", err))
	}
	if results == nil {
		results = []SearchResult{}
	}
	return toolJSON(results)
}

func (d *DB) handleUpdate(ctx context.Context, _ *mcp.CallToolRequest, args UpdateArgs) (*mcp.CallToolResult, any, error) {
	if args.ID == 0 {
		return toolErr("id is required")
	}
	if args.Content == "" && args.Tags == "" && args.Status == "" {
		return toolErr("at least one of content, tags, or status must be provided")
	}
	if args.Status != "" && !validStatuses[args.Status] {
		return toolErr("status must be one of: active|done|archived")
	}

	clauses := []string{"updated_at = datetime('now')"}
	var params []any
	if args.Content != "" {
		clauses = append(clauses, "content = ?")
		params = append(params, args.Content)
	}
	if args.Tags != "" {
		clauses = append(clauses, "tags = ?")
		params = append(params, args.Tags)
	}
	if args.Status != "" {
		clauses = append(clauses, "status = ?")
		params = append(params, args.Status)
	}
	params = append(params, args.ID)

	res, err := d.db.ExecContext(ctx,
		"UPDATE entries SET "+strings.Join(clauses, ", ")+" WHERE id = ?",
		params...,
	)
	if err != nil {
		log.Printf("update %d: %v", args.ID, err)
		return toolErr(fmt.Sprintf("update failed: %v", err))
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return toolErr(fmt.Sprintf("no entry with id %d", args.ID))
	}
	return toolJSON(map[string]any{"updated": args.ID})
}

func (d *DB) handleMarkDone(ctx context.Context, _ *mcp.CallToolRequest, args MarkDoneArgs) (*mcp.CallToolResult, any, error) {
	if args.ID == 0 {
		return toolErr("id is required")
	}
	res, err := d.db.ExecContext(ctx,
		"UPDATE entries SET status = 'done', updated_at = datetime('now') WHERE id = ?",
		args.ID,
	)
	if err != nil {
		log.Printf("mark_done %d: %v", args.ID, err)
		return toolErr(fmt.Sprintf("mark_done failed: %v", err))
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return toolErr(fmt.Sprintf("no entry with id %d", args.ID))
	}
	return toolJSON(map[string]any{"done": args.ID})
}

func (d *DB) handleDelete(ctx context.Context, _ *mcp.CallToolRequest, args DeleteArgs) (*mcp.CallToolResult, any, error) {
	if args.ID == 0 {
		return toolErr("id is required")
	}
	if args.Hard && !args.InstructedByHuman {
		return toolErr("hard delete requires instructed_by_human=true")
	}

	var (
		res sql.Result
		err error
	)
	if args.Hard {
		log.Printf("hard delete: entry %d", args.ID)
		res, err = d.db.ExecContext(ctx, "DELETE FROM entries WHERE id = ?", args.ID)
	} else {
		res, err = d.db.ExecContext(ctx,
			"UPDATE entries SET status = 'archived', updated_at = datetime('now') WHERE id = ?",
			args.ID,
		)
	}
	if err != nil {
		log.Printf("delete %d hard=%v: %v", args.ID, args.Hard, err)
		return toolErr(fmt.Sprintf("delete failed: %v", err))
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return toolErr(fmt.Sprintf("no entry with id %d", args.ID))
	}
	if args.Hard {
		return toolJSON(map[string]any{"deleted": args.ID})
	}
	return toolJSON(map[string]any{"archived": args.ID})
}

func (d *DB) handleRelate(ctx context.Context, _ *mcp.CallToolRequest, args RelateArgs) (*mcp.CallToolResult, any, error) {
	if args.FromID == 0 || args.ToID == 0 {
		return toolErr("from_id and to_id are required")
	}
	if !validKinds[args.Kind] {
		return toolErr("kind must be one of: responds_to|refutes|supports|follows_from|blocks")
	}
	var r Relation
	row := d.db.QueryRowContext(ctx, `
		INSERT INTO entry_relations (from_id, to_id, kind)
		VALUES (?, ?, ?)
		RETURNING id, from_id, to_id, kind, created_at, updated_at`,
		args.FromID, args.ToID, args.Kind,
	)
	if err := row.Scan(&r.ID, &r.FromID, &r.ToID, &r.Kind, &r.CreatedAt, &r.UpdatedAt); err != nil {
		log.Printf("relate: %v", err)
		return toolErr(fmt.Sprintf("relate failed: %v", err))
	}
	return toolJSON(r)
}

func (d *DB) handleRelations(ctx context.Context, _ *mcp.CallToolRequest, args RelationsArgs) (*mcp.CallToolResult, any, error) {
	if args.ID == 0 {
		return toolErr("id is required")
	}
	rows, err := d.db.QueryContext(ctx, `
		SELECT id, from_id, to_id, kind, created_at, updated_at
		FROM entry_relations
		WHERE from_id = ? OR to_id = ?
		ORDER BY created_at DESC`,
		args.ID, args.ID,
	)
	if err != nil {
		log.Printf("relations %d: %v", args.ID, err)
		return toolErr(fmt.Sprintf("relations failed: %v", err))
	}
	defer rows.Close()

	var relations []Relation
	for rows.Next() {
		var r Relation
		if err := rows.Scan(&r.ID, &r.FromID, &r.ToID, &r.Kind, &r.CreatedAt, &r.UpdatedAt); err != nil {
			return toolErr(fmt.Sprintf("scan failed: %v", err))
		}
		relations = append(relations, r)
	}
	if err := rows.Err(); err != nil {
		return toolErr(fmt.Sprintf("rows error: %v", err))
	}
	if relations == nil {
		relations = []Relation{}
	}
	return toolJSON(relations)
}

func (d *DB) handleStats(ctx context.Context, _ *mcp.CallToolRequest, _ StatsArgs) (*mcp.CallToolResult, any, error) {
	byType, err := d.queryKV(ctx, "SELECT type, COUNT(*) FROM entries WHERE status = 'active' GROUP BY type")
	if err != nil {
		log.Printf("stats by_type: %v", err)
		return toolErr(fmt.Sprintf("stats failed: %v", err))
	}
	byStatus, err := d.queryKV(ctx, "SELECT status, COUNT(*) FROM entries GROUP BY status")
	if err != nil {
		log.Printf("stats by_status: %v", err)
		return toolErr(fmt.Sprintf("stats failed: %v", err))
	}
	agents, err := d.queryStrings(ctx,
		"SELECT DISTINCT agent_id FROM entries WHERE status = 'active' AND agent_id != '' ORDER BY agent_id")
	if err != nil {
		log.Printf("stats agents: %v", err)
		return toolErr(fmt.Sprintf("stats failed: %v", err))
	}
	if agents == nil {
		agents = []string{}
	}
	return toolJSON(Stats{ByType: byType, ByStatus: byStatus, Agents: agents})
}

// --- prompt ---

const boardUsagePrompt = `# Blackboard usage guide

The blackboard is a shared coordination surface. Every agent in this session can read and write to it. Use it to avoid duplicating work, share discoveries, and leave context for agents that come after you.

## Orienting on arrival

Before starting any work, call stats to get a snapshot of board activity, then read with no filters to see recent active entries. If you are resuming or joining a task in progress, use search to find relevant prior work before proceeding.

## Claiming work

When you begin a task, post a working_on entry. This signals to other agents that the task is taken.

- Set agent_id to something that identifies you (e.g. "refactor-agent", "search-agent-2").
- Set expires_at to when your claim should lapse if you do not finish — use a duration that reflects the task size. A short sub-task might be 5 minutes; a large refactor might be 30. If you finish early, call mark_done.
- If you see a working_on entry for something you were about to start, check its expires_at before assuming it is stale. The reaper archives expired claims automatically.

## Entry types and when to use them

- working_on: active task claim. Requires expires_at.
- learned: a fact, observation, or constraint you discovered during work. Other agents should read these before making decisions in the same area.
- hypothesis: something you believe but have not confirmed. Post it so another agent can verify or refute.
- question: something you cannot answer that another agent might be able to. Check existing questions before posting a duplicate.
- result: the output of completed work — a summary, a diff, a filename, a count, whatever is useful to hand forward.
- handoff: context addressed to whatever agent picks up next. Use when you finish a step that has a clear continuation.
- intent: a pre-action declaration for a consequential or destructive action ("I am about to drop the staging table"). Post before acting, not after.
- blocked: you cannot proceed and are waiting on something external.
- note: anything that does not fit the above.

## Human-sourced entries

Entries with source=human come from the user directly. Treat them as authoritative constraints. Do not archive, overwrite, or contradict them without explicit human instruction. If a human entry conflicts with something you learned, post a question rather than acting on your own judgment.

## Relating entries

Use relate to connect entries that bear on each other. This lets a later agent reconstruct a chain of reasoning without reading the full board.

- responds_to: your entry directly answers or addresses another.
- refutes: your finding contradicts a prior hypothesis or learned entry.
- supports: your finding corroborates something already on the board.
- follows_from: your work is a direct continuation of another entry.
- blocks: one task cannot proceed until another is resolved.

Call relations to retrieve the graph for a given entry, then fetch the referenced entries by ID with read.

## Cleanup

When you finish, mark working_on entries as done. Archive notes or intermediate results that are no longer useful. Leave the board in a state where the next agent can orient quickly.`

func handleBoardUsage(_ context.Context, _ *mcp.GetPromptRequest) (*mcp.GetPromptResult, error) {
	return &mcp.GetPromptResult{
		Description: "How to use the blackboard for agent coordination",
		Messages: []*mcp.PromptMessage{
			{
				Role:    mcp.Role("user"),
				Content: &mcp.TextContent{Text: boardUsagePrompt},
			},
		},
	}, nil
}

// --- auth middleware ---

// requireBearer returns a middleware that enforces Authorization: Bearer <key>.
// Uses constant-time comparison to prevent timing attacks.
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

// --- reaper ---

func (d *DB) reap(ctx context.Context) (int64, error) {
	res, err := d.db.ExecContext(ctx, `
		UPDATE entries
		SET status = 'archived', updated_at = datetime('now')
		WHERE type = 'working_on' AND status = 'active' AND expires_at < datetime('now')`,
	)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
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

// --- main ---

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
		log.Printf("warning: BLACKBOARD_API_KEY is not set and server is bound to %s — all callers are trusted", host)
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
