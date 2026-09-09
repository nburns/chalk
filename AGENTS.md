# Agent guide

Read this before touching any code.

## What this is

A shared blackboard MCP server. One process, one SQLite database, many agents. Agents coordinate by posting to and reading from the board rather than through direct communication.

## Structure

```
chalk/
├── main.go       - everything: DB setup, tools, HTTP server, reaper
├── go.mod
├── go.sum
├── README.md
└── AGENTS.md
```

Single-file implementation. Do not split into packages unless there is a concrete reason that outweighs the cost of navigation overhead.

## Stack

- **Go** - use `go vet` and `staticcheck` before considering any change done
- **`github.com/modelcontextprotocol/go-sdk`** - official MCP SDK, streamable HTTP transport
- **`modernc.org/sqlite`** - pure Go SQLite, no CGO
- **SQLite WAL mode** - concurrent readers, one writer, `busy_timeout=5000` to retry on contention

## Schema

Two tables:

```sql
entries (
    id          INTEGER PRIMARY KEY AUTOINCREMENT,
    agent_id    TEXT NOT NULL DEFAULT '',
    source      TEXT NOT NULL DEFAULT 'agent',  -- 'human' | 'agent'
    type        TEXT NOT NULL DEFAULT 'note',
    status      TEXT NOT NULL DEFAULT 'active', -- 'active' | 'done' | 'archived'
    content     TEXT NOT NULL,
    tags        TEXT NOT NULL DEFAULT '',
    expires_at  DATETIME,                       -- required when type='working_on'
    created_at  DATETIME DEFAULT (datetime('now')),
    updated_at  DATETIME DEFAULT (datetime('now'))
)

entry_relations (
    id          INTEGER PRIMARY KEY AUTOINCREMENT,
    from_id     INTEGER NOT NULL REFERENCES entries(id),
    to_id       INTEGER NOT NULL REFERENCES entries(id),
    kind        TEXT NOT NULL,  -- 'responds_to' | 'refutes' | 'supports' | 'follows_from' | 'blocks'
    created_at  DATETIME DEFAULT (datetime('now')),
    updated_at  DATETIME DEFAULT (datetime('now'))
)
```

FTS5 virtual table `entries_fts` mirrors `content`, `tags`, and `agent_id` from `entries`, kept in sync via insert/update/delete triggers. Never write to `entries_fts` directly.

Indexes on `entries`: `(type, status)`, `status`, `agent_id`, `source`, `created_at`, `expires_at`.
Indexes on `entry_relations`: `from_id`, `to_id`.

## Tools exposed via MCP

### post
Required: `content`. Optional: `type`, `agent_id`, `source`, `tags`, `expires_at`.
`expires_at` is required when `type=working_on` - the tool rejects the call without it.

### read
Optional filters: `type`, `source`, `agent_id`, `status` (default: active), `since` (ISO8601 timestamp), `limit` (default 20).
Returns entries newest-first.

### search
Required: `query`. Optional: `limit` (default 10).
Uses FTS5 `snippet()` to return context around matches, not just the full entry.

### update
Required: `id`. Optional: `content`, `tags`, `status`.
Any agent may update any entry.

### mark_done
Required: `id`. Sets `status=done`. Convenience wrapper around update.

### delete
Required: `id`. Required: `hard` (bool).
- `hard=false`: sets `status=archived` regardless of caller.
- `hard=true`: permanently deletes the row. Agents may only hard-delete when explicitly instructed to by a human (pass `instructed_by_human=true`). Humans may always hard-delete.

### relate
Required: `from_id`, `to_id`, `kind`.
Valid kinds: `responds_to`, `refutes`, `supports`, `follows_from`, `blocks`.

### relations
Required: `id`. Returns all relation rows where `from_id=id` or `to_id=id`.
Fetch the actual entries separately with `read`.

### stats
No parameters. Returns counts by type, counts by status, list of active agent IDs.
Backed by indexed queries only - no full scans.

## Reaper

A background goroutine runs every `BLACKBOARD_REAPER_INTERVAL` (default 60s) and archives any `working_on` entries where `expires_at < now()`. This prevents phantom task locks from crashed or incomplete agents. Reaper logs each archival to stderr.

## Error handling

- Return errors to the MCP caller via `CallToolResult{IsError: true}` with a plain-English message describing what failed and why.
- Never swallow errors silently. Log unexpected DB errors to stderr before returning them.
- Tool validation errors (missing required field, invalid type value, etc.) return immediately without touching the DB.

## Code guidelines

**No comments that describe what the code does.** Identifiers carry that. Only comment when the why is non-obvious: a SQLite quirk, a WAL constraint, an invariant the reader would not expect.

**No sentinel return values for errors.** Return a typed error; let the caller handle it explicitly.

**Null vs empty vs missing are distinct.** An absent filter parameter means "no filter". An empty string means something was passed but is blank - treat as a validation error, not as "no filter".

**No duplicate code.** If you find yourself writing the same DB query construction twice, extract it. Surface the duplication first rather than silently copying.

**Structured data over string construction.** Build result structs, marshal to JSON. Do not concatenate JSON strings.

**Check HTTP status on every outbound call.** Not applicable to the server itself, but if any tool makes an outbound request in future, never treat a non-2xx response as success.

## Static analysis

Before any commit:

```sh
go vet ./...
staticcheck ./...
```

Install staticcheck if missing: `go install honnef.co/go/tools/cmd/staticcheck@latest`
