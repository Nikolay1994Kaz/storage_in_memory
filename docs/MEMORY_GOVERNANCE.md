# Memory governance — what this engine implements, and what it does not

This document maps the engine onto the framework proposed in
[*A Survey on the Security of Long-Term Memory in LLM Agents: Toward Mnemonic
Sovereignty*](https://arxiv.org/abs/2604.16548). The survey's vocabulary is
used here in preference to our own, because a category that has a name in the
literature is easier to evaluate than one described in a vendor's private
terms.

The survey defines **mnemonic sovereignty** as *"a system's verifiable,
recoverable governance over what may be written, who may read, when updates are
authorized, and which states may be forgotten"*, organizes the attack surface
into six lifecycle phases and four security objectives, and lists nine
governance primitives (§11).

Status vocabulary below is deliberately narrow: **implemented** means there is a
command and a test; **partial** means part of the primitive is missing and the
missing part is named; **out of scope by design** means we removed the failure
mode instead of managing it, and say what that costs; **gap** means we know and
have not built it.

## Where the engine sits in the six phases

| phase | this engine |
|---|---|
| **Write** | partial — provenance is *retained*, not *validated* |
| **Store & Manage** | implemented — LSM, versioning, supersession, TTL, decay |
| **Retrieve** | implemented and measured — see [BENCHMARKS.md §8](BENCHMARKS.md) |
| **Execute** | **not ours** — how a retrieved memory enters planning is the agent's business |
| **Share & Propagate** | **not ours** — the engine does not govern inter-agent channels |
| **Forget / Rollback** | implemented — the phase this engine was built for |

Against the four objectives: **Confidentiality** (encryption at rest, scope
isolation, cryptographic erasure) and **Availability** (WAL, snapshots, shipping,
soak-tested) are covered; **Governance** is the centre of gravity;
**Integrity** is deliberately partial — the engine detects divergence between
memory and journal but never judges whether a fact is *true*.

## The nine governance primitives

### 1. Write-gate validation — *"validate provenance before write; retain explicit source metadata"*

**Partial.** Retention is implemented: `VMEM.REMEMBER … SOURCE <s>`, and when
the caller omits it the engine writes an explicit `unknown` rather than nothing
— revocation by origin must also see the facts nobody signed for. Validation is
**not** implemented and will not be: the engine does not decide whether a write
is legitimate, because it does not decide which fact is true.

### 2. Provenance tracking — *chain-of-custody attribution*

**Implemented, with one named gap.** Every fact carries its origin;
`VMEM.EXPLAIN` shows which arm and which terms produced a given answer; the
optional audit chain covers every memory-changing command.

⚠ **Gap — derived facts.** When an agent restates a poisoned fact in its own
words, the restatement carries a *new* origin and custody breaks there. The
cost of transitive revocation was measured before writing any code
(`scripts/derived_from_probe.py`): ancestry policy turned out not to be the
lever — even the narrowest policy removes the same legitimate facts — because
the price is set by how widely the agent reads memory before writing, not by
our design. Left unbuilt on purpose.

### 3. Versioning and snapshots — *record state at intervals, enable rollback*

**Implemented, past what the primitive asks.** WAL plus snapshots;
`-restore-to-lsn` reconstructs the state as of any LSN and serves it
**read-only**, leaving the data directory untouched — forensic restore rather
than a rewrite (`-wal-inspect` finds the LSN). Supersession chains version a
fact; `ASOF` answers point-in-time queries with no restore at all.

⚠ **Evidence against relying on rollback alone**, from our own comparison of
five recovery strategies: time-based rollback leaves the lie in place in **63%**
of cases and destroys 15 of 30 innocent neighbours, because it uses *time of
write* as the predicate and time of write has no causal link to the incident.
Rollback is a substrate, not a remedy.

### 4. Compression auditing — *summarization must not silently amplify toxins*

**Out of scope by design.** There is no summarization, no consolidation and no
derived layer: a fact is one short text (see "Deliberately NOT built" in
[VMEM_DESIGN.md](VMEM_DESIGN.md)). This removes the failure mode instead of
auditing it — and, stated plainly, it means the engine offers nothing to a
system that *does* summarize. If summary consolidation is ever added, this
primitive becomes a requirement, not a footnote.

### 5. Principal-scoped retrieval — *least sharing, scope isolation*

**Implemented and measured.** Every fact belongs to a scope and retrieval never
crosses one; checked end-to-end on every returned id — **0 violations in
9 219** ([BENCHMARKS.md §7](BENCHMARKS.md)).

### 6. Access-control policy — *who may read*

⚠ **Partial, and this is the weakest primitive here.** AUTH
(`-requirepass-file` or env; the flag form is documented as unsafe) and TLS
exist, and the server binds to loopback by default. But authentication is a
**single shared secret**: there is no principal→scope mapping, so "who may
read" is enforced at the connection, not per principal. Scope isolation
protects agents from each other's data; it does not express that principal A
may read scope X and not scope Y.

### 7. Post-deletion verification — *verified forgetting across substrates, not claimed deletion*

**Implemented; the strongest primitive in this engine.** Two levels:
`VMEM.FORGET` — immediate revocation with its physical horizon stated — and
`VMEM.SHRED` — cryptographic erasure of an entire scope that reaches snapshots
and copies already shipped to an archive. `VMEM.COVERAGE` reports coverage
**fail-closed**. `VMEM.QUARANTINE` answers with a **receipt** rather than a
count — `revoked · still_trusted · outside_window · over_limit ·
other_origins` — so the engine names what it did *not* treat.

Two lessons are worth carrying out of this primitive, because both were learned
the expensive way:

- **A metric that reads someone else's flag is a declaration, not a
  measurement.** That is how a sealing hole survived while the report said
  `sealed=2, unsealed=0`: two of three segment types were writing raw fp32 in
  the clear. It was found by a fail-closed metric, not by reading the code.
- **The remainder must be counted by a full pass *after* the verdict.** A
  counter maintained during the candidate scan is free, and reports a remainder
  of zero exactly when the work was truncated by `LIMIT` — an error in the
  operator's favour, invisible from outside.

⚠ `still_trusted: 0` is honest **even when a lie is still live**, because the
predicate is narrow: if the lie arrived through a channel the operator did not
name, nothing in that scope contradicts the receipt. This is why every receipt
carries `other_origins: not_covered`, and not only the alarming ones — a zero
otherwise reads as "incident closed".

### 8. Audit-retention semantics — *audit logs preserved, user memory deletable*

**Implemented.** The chain outlives the erased fact: the entry is gone, the
record that it existed and was removed is not. `VMEM.RESEAL` re-seals after key
rotation; `EXPORT` and `PROVE` hand a third party a verifiable extract.

### 9. Forensic traceback — *reconstruct the modification path of every entry*

**Implemented, with stated limits.** Audit chain at 113 bytes per link, Ed25519
signature verifiable by a **foreign process** using the public key taken from
the server's startup log — an out-of-band channel, because a key quoted inside
the document it signs proves nothing. `RECONCILE` detects divergence between
memory and journal; `VMEM.AUDIT` walks the record.

⚠ Two limits, both measured:

- **Backdating defeats `ASOF`.** A `VALIDFROM` set in the past makes
  point-in-time reconstruction return truth-*as-recorded*, not what the agent
  actually saw at that moment.
- **The chain proves the journal's integrity, not an answer's correctness.**
  Work on zero-knowledge proof-of-retrieval (V3DB, VeriRAG, VeriANN) targets the
  stronger claim; if it reaches production, an audit chain becomes the weaker
  form of evidence.

## The boundary

The engine **does not determine which fact is true.** It states where a fact
came from, shows what produced an answer, removes by a predicate the operator
names, reports what it did not reach, and proves all of that afterwards to
someone who does not trust it. Deciding that something is a lie is a human act,
and any claim that the system understands which fact is correct is a claim this
product does not support.

That boundary is also why the MCP surface exposes exactly three tools to an
agent — remember, recall, forget. `VMEM.QUARANTINE` is not reachable by the
agent at all: it is an operator's command, which is why its receipt is written
for a human reader.
