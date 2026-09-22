# Quickstart — real embeddings, tenants, filtered search

End-to-end tour of the product path in ~200 lines of Go with **zero client
dependencies** (the wire protocol is plain RESP):

1. client-side embeddings — real vectors (`nomic-embed-text` via a local Ollama, 768-dim), computed by the example itself; the server only ever sees `float32`s (BYO-embeddings);
2. `VSIM.ADDATTR` — vector ingest with columnar attributes (`tenant`, `topic`, numeric `year`), WAL-durable;
3. `VSIM.SEARCH` — global ANN search;
4. `VSIM.FILTER` — the same query scoped to a tenant (`EQ`) and a numeric range (`RANGE`).

The demo corpus is two tenants with deliberately different domains (a space
company and a restaurant), so you can *see* tenant isolation working: the same
query returns different results once scoped.

## Run

```bash
# 1. An embedder for the CLIENT (the server never talks to it):
#    https://ollama.com/download, then
ollama pull nomic-embed-text

# 2. The server. --build rebuilds from the current sources so you never run a
#    cached image that predates a command like VSIM.ADDATTR.
docker compose up -d --build          # or: make build && ./kvstore-server --port 6380

# 3. Run the example
go run ./kvstore/examples/quickstart

# Non-default addresses / model:
go run ./kvstore/examples/quickstart -addr 127.0.0.1:6380 -ollama http://localhost:11434 -model nomic-embed-text
```

Expected output (distances will vary slightly):

```
Query: "how does a spacecraft survive the heat of returning to Earth"
  VSIM.SEARCH 3 (all tenants):
    doc:acme:5    dist=...  "Ablative heat shields protect capsules during atmospheric re-entry"  (tenant=acme year=2026)
    ...
  VSIM.FILTER 3 EQ tenant globex (same query, tenant-scoped):
    doc:globex:4  dist=...  "Char-grilled salmon fillet with lemon butter and asparagus"  (tenant=globex year=2026)
    ...
```

## Notes

- **Durability**: everything ingested here survives a server restart (WAL +
  snapshots) — restart and re-run only the search commands to verify.
- **Tenant layout at scale**: for many-tenant workloads start the server with
  `--partition-attr tenant` — vectors are then laid out contiguously per tenant
  and small-tenant filtered queries drop to brute-force over the tenant block
  (orders of magnitude faster than graph traversal; see
  [docs/BENCHMARKS.md](../../../docs/BENCHMARKS.md)).
- **No Ollama?** The engine never needs it — ingest pre-computed embeddings with
  `VSIM.ADD`/`VSIM.ADDBIN`/`VSIM.ADDATTR` from any embedding provider. Ollama is
  used *by this example* only so the tour runs without API keys; swap the
  `embed` function for any provider and nothing on the server side changes.
- The full command reference lives in [docs/COMMANDS.md](../../../docs/COMMANDS.md).
