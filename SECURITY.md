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

In scope — the default build: the RESP surface (KV, `VSIM.*`, `VMEM.*`, Pub/Sub,
transactions), `AUTH` and TLS/mTLS handling, WAL and snapshot handling
including replay of untrusted files, WAL-shipping, envelope encryption and
keyring handling (`-encrypt-at-rest`), the audit chain and its Ed25519
statements (`-audit-chain`), the `vmem-mcp` adapter, and memory-safety or
denial-of-service issues reachable from a connected client.

Out of scope:

- **WASM compute and cluster mode.** Both are compiled out of the default build
  behind the `experimental` build tag and are explicitly not hardened to the
  same bar; issues there are ordinary bugs, not advisories.
- **`AI.*` / Ollama integration.** Off by default, opt-in, and it talks to a
  third-party runtime you supply.
- **Running the server exposed without `AUTH`.** The default bind is loopback
  by design. Binding to `0.0.0.0` without authentication is a deployment
  choice, not a vulnerability in the engine.
- Anything requiring existing write access to the data directory or the host.

## Design notes a reviewer should know first

These are deliberate decisions, already documented, and reporting them as
findings will only cost you time:

- **`MULTI`/`EXEC` is grouping, not isolation.** Commands from other
  connections can interleave between queued commands. The reasoning is in
  README ("Isolation contract") and `docs/COMMANDS.md`.
- **Erasure beats time travel.** `VMEM.FORGET` and TTL expiry hide a fact from
  every read mode including `ASOF`; a quarantined fact, by contrast, stays
  visible to `ASOF` before the revocation *on purpose* — the record of what an
  agent believed is evidence. See `docs/VMEM_DESIGN.md`.
- **`FORGET` is revocation, not cryptographic erasure — and we say so.** The
  fact is unreachable immediately, but its bytes can survive in sealed
  segments until the next consolidation, in the WAL, in snapshots taken
  earlier, and in shipped archives, which retention keeps by generation count
  and never by content. Restoring to an LSN before the call brings the fact
  back. This is inherent to a journalled store, the full horizon is written
  down in `docs/VMEM_DESIGN.md` ("Erasure guarantee"), and no part of the
  project claims GDPR Art. 17 compliance. Reporting the gap is not a finding;
  reporting a case where the engine claims *more* than that section does is.
- **`VMEM.SHRED` is the cryptographic path, with its own stated limits.** With
  `-encrypt-at-rest` the VMEM payload is sealed at the persistence boundary,
  and destroying the scope's KEK makes the journal, the snapshots and the
  shipped archives unreadable at once. What it deliberately does *not* claim:
  a live process holds plaintext in memory by design (a core dump of a running
  server contains facts); facts written before the keyring existed are under no
  key at all and cannot be shredded (`VMEM.RESEAL` moves them forward,
  `VMEM.COVERAGE` shows the gap, and neither can reach copies that already
  left); `frozenSQ` and flat-HNSW segments are not yet covered in the binary
  snapshot, so with `-hnsw-use-sq` or fp32 dimensions above 256 facts stay
  readable in a snapshot taken *before* the shred. The receipt asserts only
  *this key id was destroyed*, never "the data is gone". All of this is in
  `docs/VMEM_DESIGN.md`; a finding is a way to recover sealed payload without
  the KEK, or a claim the engine makes beyond these.
- **The audit chain is tamper-*evident*, not tamper-proof.** Whoever owns both
  the journal and the head file can truncate the tail and recompute the head;
  Ed25519 proves *this key signed this head*, and its value is that an auditor
  can check without holding a secret and can detect a swapped instance against
  a previously pinned key. Reporting "the owner could rewrite their own
  journal" is not a finding — it is written down in `docs/COMMANDS.md`.
- **Provenance is an input, not a verdict.** `SOURCE` records what the writer
  claims about origin; the engine never derives trust from content.
- **Snapshot/WAL files are trusted input.** They are CRC-checked and load
  all-or-nothing, but the data directory is assumed to be under the operator's
  control. Feeding a hostile data directory is out of scope.
