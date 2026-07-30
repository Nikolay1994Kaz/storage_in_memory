# INSTALL_FOR_AGENTS.md

Instructions for an AI agent installing persistent memory (this repo's VMEM
engine + MCP adapter) on the user's machine. Execute the steps top to bottom;
each has a verification command — do not proceed past a failed verification.

Ask the user first if anything here conflicts with their setup (ports, data
locations, Docker vs binary).

## What you are installing

Two components:
1. `kvstore-server` — the memory engine (a database process the user owns).
   Linux-only binary; on macOS/Windows run it in Docker.
2. `vmem-mcp` — an MCP stdio adapter your host launches; it connects to the
   server at `-addr` and exposes `memory_remember` / `memory_recall` /
   `memory_forget`.

## Step 1 — get the binaries

Preferred: download the latest release archives for the user's OS/arch from
`https://github.com/Nikolay1994Kaz/storage_in_memory/releases`
(`kvstore-server_*_linux_*.tar.gz`, `vmem-mcp_*_<os>_*.tar.gz`), unpack into
`~/.local/bin` (or a directory the user prefers).

From source (Go ≥ 1.25 present): clone this repo, then
`go build -o ~/.local/bin/kvstore-server ./kvstore/cmd/kvstore` and
`go build -o ~/.local/bin/vmem-mcp ./kvstore/cmd/vmem-mcp`.

Verify: `vmem-mcp -h` prints flags; on Linux `kvstore-server -h` prints flags.

## Step 2 — start the server (idempotent)

Check whether something already listens on port 6380 before starting a new
one; if yes, and it is a kvstore-server, reuse it — one server is the point
(shared durable memory), two servers on one data dir are forbidden.

Linux:

```bash
mkdir -p ~/.vmem
cd ~/.vmem && nohup kvstore-server --port 6380 >> ~/.vmem/server.log 2>&1 &
```

macOS/Windows (Docker; the image builds from this repo):

```bash
git clone https://github.com/Nikolay1994Kaz/storage_in_memory && cd storage_in_memory
docker build -t kvstore . && docker run -d --name vmem -p 6380:6380 -v vmem-data:/app/data kvstore
```

Verify: `redis-cli -p 6380 PING` replies `PONG` (or open a TCP connection to
127.0.0.1:6380 — it must accept). For a durable setup suggest the user add a
systemd unit later; do not create one without asking.

**Two optional flags worth naming to the user — do not enable either without
asking.** Both are one-way in practice: turning them on later does not cover
what was already written.

- `--encrypt-at-rest` — seals stored facts under a per-scope key in
  `<data-dir>/keyring.dat` and enables `VMEM.SHRED` (erase a whole scope by
  destroying its key, reaching backups too). The trade-off is real: **lose
  `keyring.dat` and those facts are unrecoverable from any backup**, and the
  file must be backed up separately from the data directory to be worth
  anything (`docs/BACKUP.md`).
- `--audit-chain` — journals every memory-changing command, hash-chained, so
  erasure receipts can be produced later. It is never compacted and grows
  roughly **3.6 GB/year** regardless of traffic. Do not switch this on for a
  laptop install without saying that out loud.

## Step 3 — register the MCP server

Choose a scope: the project name for a project-bound agent, or `personal`.

Claude Code:

```bash
claude mcp add memory -- ~/.local/bin/vmem-mcp -addr 127.0.0.1:6380 -default-scope <scope>
```

Other hosts: add to the host's MCP config:

```json
{"mcpServers": {"memory": {"command": "<abs path to vmem-mcp>",
  "args": ["-addr", "127.0.0.1:6380", "-default-scope", "<scope>"]}}}
```

Verify: restart the host session; the tools `memory_remember`,
`memory_recall`, `memory_forget` are listed.

## Step 4 — round-trip test

Call `memory_remember` with text `"vmem install self-test"`, then
`memory_recall` with query `"install self-test"` — the fact must come back
with its id and text. Then `memory_forget` that id (leave no test residue),
and confirm a second recall returns nothing.

Report to the user: server location and data dir, adapter path, chosen scope,
and that memory survives restarts (WAL). Done.
