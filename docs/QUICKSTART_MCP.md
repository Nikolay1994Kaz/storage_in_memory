# Quickstart: persistent agent memory over MCP

Give Claude Code / Claude Desktop (or any MCP host) durable memory backed by a
`kvstore-server` you run yourself. Two processes:

- **`kvstore-server`** — the memory engine (Linux binary; Docker on
  macOS/Windows). One server = one shared, durable memory: facts survive
  restarts (WAL + snapshots) and can be shared by several agents.
- **`vmem-mcp`** — a thin MCP sidecar (Linux/macOS/Windows) that your agent
  host launches itself. It translates three tools — `memory_remember`,
  `memory_recall`, `memory_forget` — into `VMEM.*` RESP commands. It never
  starts or owns the server.

No embedding provider is required: out of the box facts are indexed with
BM25 (lexical search). Bring vectors later if you want hybrid recall — the
RESP surface (`docs/COMMANDS.md`) already takes them.

## 1. Start the server

Linux:

```bash
tar xzf kvstore-server_*_linux_amd64.tar.gz   # from the Releases page
mkdir -p ~/.vmem && cd ~/.vmem                # data dir: WAL + snapshots live here
/path/to/kvstore-server --port 6380
```

macOS / Windows (the server is epoll-based, so it runs in Docker; the image
builds from this repo's Dockerfile):

```bash
git clone https://github.com/Nikolay1994Kaz/storage_in_memory && cd storage_in_memory
docker compose up -d          # server on :6380, metrics on :9090, data in a named volume
# or without compose:
docker build -t kvstore . && docker run -d --name vmem -p 6380:6380 -v vmem-data:/app/data kvstore
```

## 2. Register the adapter in your host

Grab `vmem-mcp` for your OS from the Releases page (or `go build
./kvstore/cmd/vmem-mcp`).

Claude Code:

```bash
claude mcp add memory -- /path/to/vmem-mcp -addr 127.0.0.1:6380 -default-scope myproject
```

Claude Desktop (`claude_desktop_config.json`) or any host that takes the
standard MCP server config:

```json
{
  "mcpServers": {
    "memory": {
      "command": "/path/to/vmem-mcp",
      "args": ["-addr", "127.0.0.1:6380", "-default-scope", "personal"]
    }
  }
}
```

`-default-scope` is the agent's memory namespace — facts never leak across
scopes. Give each project (or each agent identity) its own scope; a tool
argument can override it per call. If the server runs with `-requirepass`,
add `-auth <password>`.

## 3. Try it

In one session:

> "Remember that our deploy target is the staging cluster."

The agent calls `memory_remember` and gets back a fact id. In a **new**
session (or a different agent on the same server):

> "What's our deploy target?"

The agent calls `memory_recall` and answers from memory. Restart the server —
the fact is still there (WAL replay).

## What the three tools give the agent

| tool | what it does |
|---|---|
| `memory_remember` | Store one durable fact. `supersedes=<id>` replaces an older fact **without destroying history**; `ttl_seconds` gives it a hard expiry; `importance` biases ranking. |
| `memory_recall` | Top-k facts valid **now**, ranked by relevance × recency × importance. `as_of=<unix>` answers "what was true then" (supersession is transparent; erased facts stay erased). `all=true` ignores validity. |
| `memory_forget` | Permanent erasure by id — gone from history and `as_of` too (right to be forgotten). |

Semantics are the engine's frozen `VMEM.*` contract — full details and the
design rationale: `docs/COMMANDS.md`, `docs/VMEM_DESIGN.md`.

## Troubleshooting

- Tool calls return *"kvstore-server is unreachable…"* — the server isn't
  running (step 1) or the `-addr` is wrong. The adapter never starts the
  server for you, by design: one server owns the data dir and the WAL.
- The adapter logs to stderr (visible in the host's MCP logs); raise
  verbosity with `-log-level debug`.
- Agent-driven install: point your agent at `INSTALL_FOR_AGENTS.md` in the
  repo root and let it do steps 1–3 itself.
