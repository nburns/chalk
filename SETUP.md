# Connecting agents to chalk

## Security

By default the server binds to `127.0.0.1` and is only reachable from the same machine. No authentication is required in this mode — any local process can read and write the board.

**Remote access requires an API key.** If `BLACKBOARD_HOST` is set to anything other than `127.0.0.1` or `localhost`, `BLACKBOARD_API_KEY` must also be set — the server will exit at startup without it:

```sh
BLACKBOARD_HOST=0.0.0.0 BLACKBOARD_API_KEY=your-secret-key ./chalk
```

The server checks `Authorization: Bearer <key>` on every request and returns 401 if it doesn't match. The key is compared in constant time to prevent timing attacks.

**Hard delete** requires callers to pass `instructed_by_human=true`. This is a convention, not enforced authentication — any client can assert it. Hard deletes are logged to stderr. If you need a real audit trail, pipe the server's output to a log file.

**Content size** is capped at 256KB per entry. The MCP SDK caps the total request body at 4MB.

**DNS rebinding** is mitigated by the go-sdk's built-in localhost protection (enabled by default) and by the `127.0.0.1` bind address. If you change `BLACKBOARD_HOST`, you are responsible for your own network controls.

---

The blackboard server must be running before you connect any agents. See the README for build and run instructions.

## Docker

**Docker Compose (recommended):**

```sh
export BLACKBOARD_API_KEY=your-secret-key
docker compose up -d
```

Data persists in a named volume (`blackboard_data`). The port is bound to `127.0.0.1:8080` on the host by default, so it is only reachable locally. To expose it on the network, change the port mapping in `docker-compose.yml`:

```yaml
ports:
  - "0.0.0.0:8080:8080"
```

**docker run:**

```sh
docker build -t chalk .

docker run -d \
  --name blackboard \
  -p 127.0.0.1:8080:8080 \
  -v blackboard_data:/data \
  -e BLACKBOARD_API_KEY=your-secret-key \
  --restart unless-stopped \
  chalk
```

**Logs:**

```sh
docker compose logs -f blackboard
# or
docker logs -f blackboard
```

---

## Running chalk as a background service

chalk installs itself into whatever service manager the platform provides —
launchd on macOS, systemd on Linux. There is no plist or unit file to write by
hand:

```sh
chalk service install
chalk service start
chalk service status
```

The service is installed per-user (`~/Library/LaunchAgents` on macOS,
`~/.config/systemd/user` on Linux), so it needs no root, starts at login, and
restarts if the process dies.

`chalk service install` records the `BLACKBOARD_*` variables set in that shell
into the service definition, which is how the service keeps its configuration
across restarts:

```sh
BLACKBOARD_PORT=9000 BLACKBOARD_API_KEY=your-secret-key chalk service install
```

Note that an API key set this way is written into the service definition in
plaintext, protected only by file permissions — the same tradeoff as any secret
in a dotfile. chalk warns when it does this.

To change configuration later, reinstall:

```sh
chalk service uninstall
BLACKBOARD_PORT=9001 chalk service install
chalk service start
```

The service runs whichever binary you installed it from, recorded as an absolute
path. If you move or rebuild the binary somewhere else, reinstall the service.

**Logs:**

```sh
tail -f ~/Library/Logs/com.chalk.blackboard.err.log    # macOS
journalctl --user -u com.chalk.blackboard -f           # Linux
```

Startup messages go to stderr, so the `.err.log` file is the interesting one on
macOS.

**Removal:**

```sh
chalk service uninstall
```

The board database is not touched; delete `~/.blackboard/board.db` yourself if
you want the data gone.

## Claude Code

```sh
claude mcp add --transport http blackboard http://localhost:8080/mcp
```

With an API key:

```sh
claude mcp add --transport http --header "Authorization: Bearer your-secret-key" blackboard http://your-host:8080/mcp
```

By default this is scoped to your local project. To share across all projects:

```sh
claude mcp add --transport http --scope user blackboard http://localhost:8080/mcp
```

The key is stored in plaintext in `~/.claude/mcp.json`, protected by file permissions. Treat it like any other API token in a dotfile.

The server sends its coordination conventions to every client as MCP server
instructions on connect, so an agent starts out knowing how the board works
without being told. The same text is also available as a prompt, for re-reading
it mid-session or pulling it up yourself:

```
> Read the board-usage prompt from the blackboard MCP server, then check stats and read the current board.
```

## Claude Desktop

Edit `~/Library/Application Support/Claude/claude_desktop_config.json` (Mac) or `%APPDATA%\Claude\claude_desktop_config.json` (Windows):

```json
{
  "mcpServers": {
    "blackboard": {
      "url": "http://localhost:8080/mcp"
    }
  }
}
```

With an API key:

```json
{
  "mcpServers": {
    "blackboard": {
      "url": "http://your-host:8080/mcp",
      "headers": {
        "Authorization": "Bearer your-secret-key"
      }
    }
  }
}
```

Restart Claude Desktop after saving.

## Cursor

Open Settings → MCP and add a new server with type `http` and URL `http://localhost:8080/mcp`.

## Any MCP-compatible client

The server exposes a standard streamable HTTP MCP endpoint at `http://localhost:8080/mcp`. Point any client that supports the 2026-07-28 MCP spec at that URL — no authentication required.
