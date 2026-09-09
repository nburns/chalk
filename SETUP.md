# Connecting agents to chalk

## Security

**Chalk has no authentication.** It is designed to run locally and trusts all callers equally.

By default the server binds to `127.0.0.1` and is only reachable from the same machine. To bind to a non-local address (e.g. a shared team server on a trusted LAN), `BLACKBOARD_API_KEY` is required — the server will refuse to start without it.

**Hard delete** requires callers to pass `instructed_by_human=true`. This is a convention, not enforced authentication — any client can assert it. Hard deletes are logged to stderr. If you need a real audit trail, pipe the server's output to a log file.

**Content size** is capped at 256KB per entry. The MCP SDK caps the total request body at 4MB.

**DNS rebinding** is mitigated by the go-sdk's built-in localhost protection (enabled by default) and by the `127.0.0.1` bind address. If you change `BLACKBOARD_HOST`, you are responsible for your own network controls.

### API key for remote access

Set `BLACKBOARD_API_KEY` before starting the server:

```sh
BLACKBOARD_HOST=0.0.0.0 BLACKBOARD_API_KEY=your-secret-key ./chalk
```

The server checks `Authorization: Bearer <key>` on every request and returns 401 if it doesn't match. `BLACKBOARD_API_KEY` is required when `BLACKBOARD_HOST` is anything other than `127.0.0.1` or `localhost` — the server will exit at startup without it.

The key is compared in constant time to prevent timing attacks.

---

The blackboard server must be running before you connect any agents. See the README for build and run instructions.

## Docker

**Docker Compose (recommended):**

```sh
# optionally set an API key
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

### launchd (Mac)

Create `~/Library/LaunchAgents/com.chalk.blackboard.plist`:

```xml
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
  <key>Label</key>
  <string>com.chalk.blackboard</string>
  <key>ProgramArguments</key>
  <array>
    <string>/usr/local/bin/chalk</string>
  </array>
  <key>EnvironmentVariables</key>
  <dict>
    <key>BLACKBOARD_PORT</key>
    <string>8080</string>
    <key>BLACKBOARD_API_KEY</key>
    <string>your-secret-key</string>
  </dict>
  <key>RunAtLoad</key>
  <true/>
  <key>KeepAlive</key>
  <true/>
  <key>StandardOutPath</key>
  <string>/tmp/chalk.log</string>
  <key>StandardErrorPath</key>
  <string>/tmp/chalk.log</string>
</dict>
</plist>
```

Update `ProgramArguments` to the path where you installed the binary. Then load it:

```sh
launchctl load ~/Library/LaunchAgents/com.chalk.blackboard.plist
```

To stop or unload:

```sh
launchctl unload ~/Library/LaunchAgents/com.chalk.blackboard.plist
```

### systemd (Linux)

Create `/etc/systemd/system/chalk.service`:

```ini
[Unit]
Description=chalk blackboard MCP server
After=network.target

[Service]
ExecStart=/usr/local/bin/chalk
Environment=BLACKBOARD_PORT=8080
Environment=BLACKBOARD_API_KEY=your-secret-key
Restart=on-failure
RestartSec=5

[Install]
WantedBy=default.target
```

Update `ExecStart` to the path where you installed the binary. Then enable and start:

```sh
systemctl daemon-reload
systemctl enable chalk
systemctl start chalk
```

To check status or view logs:

```sh
systemctl status chalk
journalctl -u chalk -f
```

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

To orient Claude on the blackboard conventions before it starts work, pull the built-in prompt:

```
> Before starting, read the board-usage prompt from the blackboard MCP server, then check stats and read the current board.
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
