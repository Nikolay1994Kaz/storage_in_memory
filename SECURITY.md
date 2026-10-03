# Security Policy

## Reporting a vulnerability

Report privately, not in a public issue:

- GitHub → **Security** → *Report a vulnerability* (private advisory), or
- email **dubovoinikolai@gmail.com** with `SECURITY` in the subject.

Please include the affected version or commit, the configuration that triggers
it (flags, whether `AUTH`/TLS are on, bind address), and the smallest way to
reproduce it.

**What to expect.** This is a single-maintainer project, and the honest
commitment is a small one: acknowledgement within **7 days**, an assessment
within **30 days**, and a fix or a documented mitigation before any public
disclosure. There is no paid support tier and no bounty. If you need faster or
contractual response times, that is a commercial arrangement — see the README.

Please give a fix a reasonable window before disclosing publicly. Credit is
given in the release notes unless you ask otherwise.

## Supported versions

Only the latest release and `master` receive fixes. There are no long-term
support branches.

## Scope

In scope — the default build: the RESP surface (KV, `VSIM.*`, Pub/Sub,
transactions), `AUTH` and TLS/mTLS handling, WAL and snapshot handling
including replay of untrusted files, WAL-shipping, and memory-safety or
denial-of-service issues reachable from a connected client.

Out of scope:

- **Cluster mode.** Compiled out of the default build behind the `experimental`
  build tag and explicitly not hardened to the same bar; issues there are
  ordinary bugs, not advisories.
- **Running the server exposed without `AUTH`.** The default bind is loopback
  by design. Binding to `0.0.0.0` without authentication is a deployment
  choice, not a vulnerability in the engine.
- Anything requiring existing write access to the data directory or the host.

## Design notes a reviewer should know first

These are deliberate decisions, already documented, and reporting them as
findings will only cost you time:

- **`AUTH` is one shared secret, and the trust boundary is the process.** There
  is no per-principal authorization: after a successful `AUTH` the connection
  carries a boolean, not an identity, and has full authority over all data.
  Everyone who holds the password shares one trust domain; run one process per
  trust domain. Tenant attributes (`-partition-attr`, `EQ` filters) are a
  retrieval feature, not an access-control boundary.
- **`MULTI`/`EXEC` is grouping, not isolation.** Commands from other
  connections can interleave between queued commands. The reasoning is in
  README ("Isolation contract") and `docs/COMMANDS.md`.
- **Deletion is logical first, physical later.** A deleted key is unreachable
  immediately, but its bytes can survive in immutable segments until the next
  merge, in the WAL, in earlier snapshots and in shipped archives. The engine
  makes no secure-erasure claim; there is no encryption at rest — use disk
  encryption if the data directory needs it.
- **Snapshot/WAL files are trusted input.** They are CRC-checked and load
  all-or-nothing, but the data directory is assumed to be under the operator's
  control. Feeding a hostile data directory is out of scope.
