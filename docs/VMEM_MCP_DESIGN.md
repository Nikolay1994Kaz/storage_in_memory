# VMEM MCP Adapter — design (sprint 3)

Goal: make VMEM usable as persistent agent memory from any MCP-capable host
(Claude Code, Claude Desktop, Cursor, …) with a **2-minute install**. The
adapter is deliberately thin: it adds naming and typing on top of the frozen
`VMEM.*` RESP contract (`docs/COMMANDS.md`) and **never semantics** — every
tool call maps 1:1 to one RESP command against a running `kvstore-server`.

## Architecture: a thin RESP sidecar, not an embedded store

`cmd/vmem-mcp` is a stdio JSON-RPC process that dials the server over RESP.

Why not embed the engine in the adapter (single-process, engram-style):
MCP stdio servers are **spawned per host** — Claude Code and Claude Desktop
each launch their own instance. Two embedded stores on one data dir means two
WAL writers — corruption by design. The server process stays the single
writer; adapters are stateless clients, so any number of agents share one
memory and one durability story (WAL, snapshots, S3 shipping, AUTH — all
already exist server-side).

Consequences:
- The adapter **never starts the server itself** (hidden lifecycles create
  surprise WAL dirs). If the server is unreachable, every tool call returns
  an actionable `isError` result telling the agent/user how to start it.
- One reconnect attempt per call (agent sessions are long-lived; the server
  may restart under them).
- Logs go to **stderr** (stdout is the protocol channel).

## Tool surface

Three tools. Descriptions in the code are written *for the agent* — in MCP
they function as prompts and carry the usage policy (recall before answering
questions about the user/project; remember decisions and stable facts, not
transcripts; supersede instead of duplicating when a fact changes).

| tool | arguments | returns (JSON in text content) |
|---|---|---|
| `memory_remember` | `text` (req), `scope?`, `type?`, `importance?` 0..1, `ttl_seconds?`, `supersedes?` (id) | `{"id": "..."}` |
| `memory_recall` | `query` (req), `scope?`, `k?` (default 5), `as_of?` (unix sec), `all?` (bool), `type?`, `half_life_seconds?` | `{"facts": [{"id","score","text"}]}` |
| `memory_forget` | `id` (req), `scope?` | `{"erased": true/false}` |

Decisions:
- **`scope` defaults to the `-default-scope` flag** (the agent's identity,
  configured once at install time); a tool argument overrides per call. This
  keeps the common case zero-thought for the agent while allowing
  multi-profile setups.
- **`source` is adapter-configured (`-source`) and deliberately absent from the
  tool schema.** Provenance declared by the writing agent is worth exactly as
  much as trust in that agent, and the whole point of this layer is to remain
  useful in the case where the agent is already acting on injected
  instructions — an attacker who can steer the agent must not be able to sign
  their fact with someone else's origin. Left empty, the adapter sends no
  `SOURCE` and the server stamps `unknown`; the adapter never invents an origin
  it does not know. Agent-claimed sub-provenance ("this came from an email") is
  genuinely useful information, but it belongs *beside* the trusted channel as
  its own field, never instead of it.
- **No vector argument in v1.** MCP hosts have no embeddings to pass; the
  product default is the embedding ladder's stage 0 (BM25-only). Adding an
  optional `vector` param later is additive — the door stays open at the RESP
  level (`VEC`).
- `half_life_seconds` is exposed because decay *policy* belongs to the client
  (design rule from `VMEM_DESIGN.md`); default stays server-side (30 d).
- Tool-level failures (server down, cross-scope forget, bad importance) are
  MCP `isError` results with the server's message verbatim — never JSON-RPC
  protocol errors (the host would retry those; a semantic rejection is an
  answer, not a transport fault).

## MCP protocol subset (hand-rolled, zero new deps)

Newline-delimited JSON-RPC 2.0 over stdio — `encoding/json` is enough; an SDK
dependency is not justified for four methods:

- `initialize` → echo the client's `protocolVersion` if recognized (else our
  latest known), `capabilities: {tools: {}}`, `serverInfo`.
- `notifications/initialized` and all other notifications — consumed, never
  answered.
- `ping` → `{}`.
- `tools/list` → the three tools with JSON-Schema `inputSchema`.
- `tools/call` → RESP round-trip → `content: [{type:"text", text:"<json>"}]`.
- Unknown method with an id → `-32601`; malformed JSON → `-32700`.

Flags: `-addr` (default `127.0.0.1:6380`), `-auth`, `-default-scope`
(default `default`), `-source` (default `mcp`), `-log-level`. TLS: demand-driven door, same flags as
other clients when it comes.

## Non-goals (v1)

No `resources`/`prompts` capabilities (tools only), no HTTP/SSE transport,
no server auto-start, no LLM-side summarization (upper floor — see
`VMEM_DESIGN.md` non-goals).

## Sprint-3 step criteria

1. **Skeleton** — scripted stdio session in a Go test: initialize handshake,
   `tools/list` returns 3 tools, unknown method → `-32601`, notification gets
   no reply.
2. **Tools E2E** — integration test spawns a real `kvstore-server` and the
   adapter as a subprocess: remember → recall (anchor text comes back) →
   supersedes chain judged through `as_of` → forget → gone everywhere;
   scope isolation (fact from scope A invisible from B); server-down →
   `isError` with the start hint. Zero protocol errors.
3. **Live demo** — `docs/QUICKSTART_MCP.md` (client config for Claude
   Code/Desktop) + `INSTALL_FOR_AGENTS.md` at repo root (agent-driven
   install, GBrain distribution lesson); scenario: fact remembered in one
   session is recalled in a fresh session.
4. **README reframe** — the shop window leads with "self-hosted memory engine
   for agents" (VMEM + MCP first, engine internals as the proof), wedges:
   RESP drop-in + single binary + sovereign/on-prem. Positioning per the
   17.07 market decision; not "a faster engine" (GBrain lesson).
