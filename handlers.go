package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log"
	"sort"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"
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

const maxContentBytes = 256 * 1024 // 256KB

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
