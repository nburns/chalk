# chalk 🧑🏼‍🏫

A shared blackboard for agent coordination. Agents read and write to a common board — posting what they're working on, what they've learned, questions, results, and constraints — so parallel or sequential agent runs don't duplicate work, talk past each other, or lose context between sessions.

## Goals

**Coordination over isolation.** The default state for agents running on the same codebase is mutual ignorance. Chalk gives them a shared surface: one agent claims a task, another sees it and moves to something else, a third reads what was learned and builds on it rather than rediscovering it.

**Human constraints are first-class.** Entries posted by a human carry `source=human` and are treated as higher-authority than anything an agent posts. Agents read them; they don't overwrite or archive them. This is how you describe a constraint, a business rule, or a decision that shouldn't be relitigated by an agent that doesn't know the history.

**Low overhead.** Single Go binary, SQLite on disk, HTTP transport. No broker, no auth surface, no infrastructure to operate. Runs on your laptop alongside whatever else you're doing.

**Designed to decay gracefully.** `working_on` entries expire automatically. The board doesn't accumulate phantom locks from agents that crashed or finished without cleaning up.

## Entry types

| type | meaning |
|---|---|
| `working_on` | claims a task; requires `expires_at` |
| `learned` | a fact or observation discovered during work |
| `hypothesis` | an unconfirmed belief worth testing |
| `question` | something the agent can't answer and is putting on the board |
| `result` | completed output |
| `handoff` | context addressed forward to whatever agent picks up next |
| `intent` | a pre-action declaration ("I'm about to delete X") |
| `blocked` | dependency on something not yet available |
| `note` | catch-all |

## Install

Two ways in: the Homebrew tap, or a build from source. Both produce the same
single static binary — no CGO, no system SQLite, no broker.

### Homebrew

The formula lives in this repo rather than a `homebrew-chalk` repo, so the tap
needs the URL spelled out once:

```sh
brew tap nburns/chalk https://github.com/nburns/chalk
brew install nburns/chalk/chalk
```

Works on macOS and on Linux under Homebrew. To track `main` instead of the
latest tagged release, install `--HEAD`:

```sh
brew install --HEAD nburns/chalk/chalk
```

Upgrade and removal are the usual:

```sh
brew upgrade chalk
brew uninstall chalk        # leaves the board database alone
brew untap nburns/chalk
```

### From source

Requires Go 1.27 or newer.

```sh
git clone https://github.com/nburns/chalk
cd chalk
make install
```

That builds the binary and installs it to `~/.local/bin/chalk`. Install somewhere
else with `make install PREFIX=/usr/local` (that path needs `sudo make install
PREFIX=/usr/local`).

**Run it in the foreground:**

```sh
chalk
# blackboard listening on http://127.0.0.1:8080/mcp  db=/Users/you/.blackboard/board.db
```

## Running in the background

The board should be up whenever an agent looks for it, which means a service
that starts at login and restarts on crash. chalk registers itself with the
platform's service manager — launchd on macOS, a systemd **user** unit on Linux
— so there is no plist or unit file to write by hand:

```sh
chalk service install
chalk service start
chalk service status
```

`make service` does the build, install, and start in one step.

| command | effect |
|---|---|
| `chalk service install` | register the service (records your current `BLACKBOARD_*` settings) |
| `chalk service start` / `stop` / `restart` | control it |
| `chalk service status` | report `running`, `stopped`, or that it isn't installed |
| `chalk service uninstall` | remove it; the board database is left alone |

Because install captures the environment it runs in, set any configuration in
the same command:

```sh
BLACKBOARD_PORT=9000 chalk service install
```

To change settings later, run `chalk service uninstall` and install again.

### systemd specifics (Linux)

`chalk service install` writes `~/.config/systemd/user/com.chalk.blackboard.service`
and enables it — per-user, no root, no `sudo`. Once installed, the unit is an
ordinary systemd user unit and `systemctl --user` works on it directly:

```sh
systemctl --user status com.chalk.blackboard
journalctl --user -u com.chalk.blackboard -f
```

Two things worth knowing about user units:

- A user manager normally starts at login and stops when your last session
  ends, which kills the board on logout. For a board that survives logout and
  comes up at boot on a headless box, enable lingering once:
  `sudo loginctl enable-linger "$USER"`.
- The unit is installed with `WantedBy=default.target`. The service library's
  stock template uses `multi-user.target`, which a user manager never
  activates, so chalk ships its own template — a unit installed by other means
  needs the same correction to start at login.

If you installed via Homebrew on Linux, `brew services start chalk` is the
alternative; it manages its own systemd user unit with the paths from the
formula (`$(brew --prefix)/var/chalk/board.db`). Use one or the other, not both
— two copies of the server on the same port will fight.

### launchd specifics (macOS)

The agent is written to `~/Library/LaunchAgents/com.chalk.blackboard.plist`.
Startup messages go to stderr, so the `.err.log` file is the interesting one:

```sh
tail -f ~/Library/Logs/com.chalk.blackboard.err.log
```

`brew services start chalk` is the Homebrew-managed alternative here too, with
the same caveat about running only one.

### Uninstall

```sh
make uninstall   # stops and removes the service, then removes the binary
```

Homebrew installs come out with `brew services stop chalk && brew uninstall chalk`.
Either way the board database is left in place; delete it yourself if you want
the data gone.

### Where things live

| | source / `make install` | Homebrew |
|---|---|---|
| binary | `~/.local/bin/chalk` | `$(brew --prefix)/bin/chalk` |
| database | `~/.blackboard/board.db` | `$(brew --prefix)/var/chalk/board.db` |
| service (macOS) | `~/Library/LaunchAgents/com.chalk.blackboard.plist` | `brew services` |
| service (Linux) | `~/.config/systemd/user/com.chalk.blackboard.service` | `brew services` |

The server listens on port `8080` by default. The database path, host, and port
are all configurable via environment variables (see [Configuration](#configuration)).

For connecting Claude Code, Claude Desktop, Cursor, or any MCP-compatible client, see [SETUP.md](./SETUP.md).

---

## Usage examples

### Coordinating parallel agents on a codebase task

Start chalk, then open two Claude Code sessions in the same repo. In each session's system prompt or first message:

```
Before doing any work, call the blackboard stats tool to see what's active,
then read the current board. Claim your task with a working_on entry before
starting. Post what you learn as learned entries so the other agent can use them.
```

Agent A claims the auth module, agent B sees the claim and picks up the API layer. When A discovers that session tokens are stored in plaintext, it posts a `learned` entry. B reads the board before touching anything auth-related and picks it up without being told directly.

### Leaving constraints for future agents

Post human-sourced entries before starting a session to set boundaries agents should not cross:

```sh
curl -X POST http://localhost:8080/mcp \
  -H "Content-Type: application/json" \
  -d '{
    "jsonrpc": "2.0", "id": 1, "method": "tools/call",
    "params": {
      "name": "post",
      "arguments": {
        "content": "Do not touch the payments module — live migration in progress until EOD.",
        "source": "human",
        "type": "note",
        "tags": "payments,constraint"
      }
    }
  }'
```

Any agent that reads the board before starting will see this before touching anything tagged `payments`.

### Chaining agents through a pipeline

Agent 1 completes its work and posts a handoff:

```
Post a handoff entry with a summary of what I did, what the next agent needs to know,
and what files were changed. Then mark my working_on entry as done.
```

Agent 2 starts by reading the board and picks up the handoff without a human in the loop.

### Tracking a hypothesis across agents

Agent A posts a hypothesis: "The N+1 query is in the user serializer." Agent B, working on a different part of the stack, finds the real cause and calls `relate` with `kind=refutes`, then posts its own `learned` entry. The chain is preserved — any later agent can call `relations` to reconstruct the reasoning.

---

## Tools

| tool | description |
|---|---|
| `post` | write an entry to the board |
| `read` | read entries, filterable by type / source / agent / status / since |
| `search` | full-text search with FTS5 snippets |
| `update` | revise content, tags, or status on any entry |
| `mark_done` | set status=done on an entry |
| `delete` | hard delete (requires `instructed_by_human=true`) or soft delete/archive |
| `relate` | create a typed relationship between two entries |
| `relations` | fetch relation rows for an entry; retrieve entries separately with `read` |
| `stats` | board summary: counts by type and status, active agents |

## Configuration

| env var | default | description |
|---|---|---|
| `BLACKBOARD_DB` | `~/.blackboard/board.db` | path to SQLite database |
| `BLACKBOARD_HOST` | `127.0.0.1` | address to bind to; set to `0.0.0.0` to expose on the network |
| `BLACKBOARD_PORT` | `8080` | port to listen on |
| `BLACKBOARD_API_KEY` | _(none)_ | if set, all requests must carry `Authorization: Bearer <key>`; the server exits at startup if unset and the host is non-local |
| `BLACKBOARD_REAPER_INTERVAL` | `60s` | how often to archive expired `working_on` entries |
