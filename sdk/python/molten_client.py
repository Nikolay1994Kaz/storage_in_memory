"""
Molten KVStore — Python SDK
============================

A Python client for the Molten KVStore server (Redis-compatible with
HNSW vector search). Communicates via the RESP (REdis Serialization Protocol)
over TCP.

Usage::

    from molten_client import MoltenClient

    with MoltenClient("localhost", 6380) as client:
        client.ping()                             # → "PONG"
        client.set("name", "Alice")               # → "OK"
        client.get("name")                        # → "Alice"

        # Vector operations (HNSW graph)
        client.vsim_add("vec:1", [0.1, 0.2, 0.3])
        results = client.vsim_search([0.1, 0.2, 0.3], k=5)
        # → [("vec:1", 0.000000), ...]

Server commands reference (from kvstore/cmd/kvstore/main.go):

* ``VSIM.ADD <key> <v1> <v2> … <vN>``   — insert vector
* ``VSIM.DEL <key>``                     — delete vector
* ``VSIM.SEARCH <K> <v1> <v2> … <vN>``  — K-nearest-neighbour search
* ``VSIM.SEARCHFILTER <K> <field> <value> <v1> … <vN>`` — filtered KNN
* ``VSIM.SEARCHRANGE <K> <set> <min> <max> <v1> … <vN>``
* ``VSIM.INFO``                          — vector store stats
"""

from __future__ import annotations

import socket
import struct
from typing import Any, Callable, List, Optional, Sequence, Tuple, Union


# ──────────────────────────────────────────────────────────────────────
# RESP codec
# ──────────────────────────────────────────────────────────────────────

class RESPError(Exception):
    """Raised when the server returns a RESP error (-ERR …)."""


class RESPConnection:
    """Low-level RESP encoder / decoder over a TCP socket.

    This class handles:
    * Encoding commands as RESP arrays of bulk strings.
    * Decoding RESP responses (simple strings, errors, integers,
      bulk strings, arrays, and nulls).

    Parameters
    ----------
    host : str
        Server hostname or IP address.
    port : int
        Server TCP port (default 6380).
    timeout : float
        Socket timeout in seconds (default 5.0).
    buffer_size : int
        Receive buffer size in bytes (default 64 KiB).
    """

    def __init__(
        self,
        host: str = "localhost",
        port: int = 6380,
        timeout: float = 5.0,
        buffer_size: int = 65536,
    ) -> None:
        self.host = host
        self.port = port
        self.timeout = timeout
        self.buffer_size = buffer_size
        self._sock: Optional[socket.socket] = None
        self._buf = b""

    # ── connection lifecycle ────────────────────────────────────────

    def connect(self) -> None:
        """Open a TCP connection to the server."""
        self._sock = socket.create_connection(
            (self.host, self.port), timeout=self.timeout
        )
        self._sock.setsockopt(socket.IPPROTO_TCP, socket.TCP_NODELAY, 1)
        self._buf = b""

    def close(self) -> None:
        """Close the TCP connection."""
        if self._sock is not None:
            try:
                self._sock.close()
            except OSError:
                pass
            self._sock = None
            self._buf = b""

    @property
    def connected(self) -> bool:
        return self._sock is not None

    # ── RESP encoder ────────────────────────────────────────────────

    @staticmethod
    def encode(*args: Union[str, bytes, int, float]) -> bytes:
        """Encode a command as a RESP array of bulk strings.

        Each argument is converted to its byte representation:
        * ``str``   → UTF-8 encoded bytes
        * ``int``   → decimal string
        * ``float`` → ``repr()`` for full precision
        * ``bytes`` → used as-is

        Returns the full RESP frame ready to write to the socket.
        """
        parts: list[bytes] = []
        parts.append(f"*{len(args)}\r\n".encode())
        for a in args:
            if isinstance(a, bytes):
                raw = a
            elif isinstance(a, str):
                raw = a.encode()
            elif isinstance(a, int):
                raw = str(a).encode()
            elif isinstance(a, float):
                raw = repr(a).encode()
            else:
                raw = str(a).encode()
            parts.append(f"${len(raw)}\r\n".encode())
            parts.append(raw)
            parts.append(b"\r\n")
        return b"".join(parts)

    # ── RESP decoder ────────────────────────────────────────────────

    def _read_until(self, delimiter: bytes = b"\r\n") -> bytes:
        """Read from the socket until *delimiter* is found in the buffer."""
        while delimiter not in self._buf:
            chunk = self._sock.recv(self.buffer_size)
            if not chunk:
                raise ConnectionError("Server closed the connection")
            self._buf += chunk
        idx = self._buf.index(delimiter)
        line = self._buf[:idx]
        self._buf = self._buf[idx + len(delimiter):]
        return line

    def _read_exact(self, n: int) -> bytes:
        """Read exactly *n* bytes from the socket."""
        while len(self._buf) < n:
            chunk = self._sock.recv(self.buffer_size)
            if not chunk:
                raise ConnectionError("Server closed the connection")
            self._buf += chunk
        data = self._buf[:n]
        self._buf = self._buf[n:]
        return data

    def read_response(self) -> Any:
        """Read and decode one RESP value.

        Returns
        -------
        str
            For simple strings (``+``).
        int
            For integers (``:``).
        bytes | str | None
            For bulk strings (``$``).  ``None`` for null bulk string.
        list
            For arrays (``*``).

        Raises
        ------
        RESPError
            When the server returns a ``-ERR …`` response.
        """
        line = self._read_until(b"\r\n")
        if not line:
            raise ConnectionError("Empty response")

        prefix = chr(line[0])
        payload = line[1:]

        if prefix == "+":
            # Simple string
            return payload.decode()

        if prefix == "-":
            # Error
            raise RESPError(payload.decode())

        if prefix == ":":
            # Integer
            return int(payload)

        if prefix == "$":
            # Bulk string
            length = int(payload)
            if length == -1:
                return None  # NULL
            data = self._read_exact(length)
            self._read_exact(2)  # consume trailing \r\n
            # Return as str (the server always sends text in bulk strings)
            try:
                return data.decode()
            except UnicodeDecodeError:
                return data  # binary payload

        if prefix == "*":
            # Array
            count = int(payload)
            if count == -1:
                return None
            return [self.read_response() for _ in range(count)]

        raise ValueError(f"Unknown RESP prefix: {prefix!r}")

    # ── send + receive ──────────────────────────────────────────────

    def execute(self, *args: Union[str, bytes, int, float]) -> Any:
        """Send a RESP command and return the parsed response.

        This is the primary entry point used by :class:`MoltenClient`.
        """
        if self._sock is None:
            raise ConnectionError("Not connected — call connect() first")
        self._sock.sendall(self.encode(*args))
        return self.read_response()


# ──────────────────────────────────────────────────────────────────────
# High-level client
# ──────────────────────────────────────────────────────────────────────

class MoltenClient:
    """High-level client for the Molten KVStore.

    Wraps :class:`RESPConnection` with convenient methods that match
    the command set implemented in the Go server.

    Parameters
    ----------
    host : str
        Server hostname (default ``"localhost"``).
    port : int
        Server TCP port (default ``6380``).
    password : str | None
        If set, ``AUTH`` is sent immediately after connecting.
    timeout : float
        Socket timeout in seconds (default ``5.0``).

    Example
    -------
    >>> with MoltenClient() as c:
    ...     c.ping()
    'PONG'
    """

    def __init__(
        self,
        host: str = "localhost",
        port: int = 6380,
        password: Optional[str] = None,
        timeout: float = 5.0,
    ) -> None:
        self._conn = RESPConnection(host, port, timeout=timeout)
        self._password = password

    # ── context manager ─────────────────────────────────────────────

    def __enter__(self) -> "MoltenClient":
        self.connect()
        return self

    def __exit__(self, *exc: Any) -> None:
        self.close()

    def connect(self) -> None:
        """Open the connection (and authenticate if a password was given)."""
        self._conn.connect()
        if self._password:
            self.auth(self._password)

    def close(self) -> None:
        """Close the connection."""
        self._conn.close()

    def execute(self, *args: Union[str, bytes, int, float]) -> Any:
        """Execute a raw RESP command. Returns the parsed response."""
        return self._conn.execute(*args)

    # ── standard Redis-compatible commands ──────────────────────────

    def auth(self, password: str) -> str:
        """Authenticate with the server (``AUTH <password>``)."""
        return self.execute("AUTH", password)

    def ping(self) -> str:
        """Send a ``PING``.  Returns ``"PONG"``."""
        return self.execute("PING")

    def set(self, key: str, value: str, ex: Optional[int] = None) -> str:
        """``SET key value [EX seconds]``.

        Parameters
        ----------
        key : str
        value : str
        ex : int | None
            Optional TTL in seconds.
        """
        if ex is not None:
            return self.execute("SET", key, value, "EX", str(ex))
        return self.execute("SET", key, value)

    def get(self, key: str) -> Optional[str]:
        """``GET key``.  Returns ``None`` if the key does not exist."""
        return self.execute("GET", key)

    def delete(self, key: str) -> int:
        """``DEL key``.  Returns ``1`` if the key existed, else ``0``."""
        return self.execute("DEL", key)

    def expire(self, key: str, seconds: int) -> int:
        """``EXPIRE key seconds``.  Returns ``1`` on success."""
        return self.execute("EXPIRE", key, str(seconds))

    def ttl(self, key: str) -> int:
        """``TTL key``.  Returns remaining seconds, ``-1`` (no expiry),
        or ``-2`` (key not found)."""
        return self.execute("TTL", key)

    def persist(self, key: str) -> int:
        """``PERSIST key``.  Removes TTL.  Returns ``1`` on success."""
        return self.execute("PERSIST", key)

    def dbsize(self) -> int:
        """``DBSIZE``.  Returns the number of keys in the store."""
        return self.execute("DBSIZE")

    # ── Pub / Sub ───────────────────────────────────────────────────

    def publish(self, channel: str, message: str) -> int:
        """``PUBLISH channel message``.  Returns subscriber count."""
        return self.execute("PUBLISH", channel, message)

    # ── Vector operations (VSIM.*) ──────────────────────────────────

    @staticmethod
    def _floats_to_args(vector: Sequence[float]) -> list[str]:
        """Convert a float vector to a list of string arguments.

        The server expects each vector component as a separate RESP
        bulk-string argument (parsed with ``strconv.ParseFloat``).
        """
        return [repr(float(v)) for v in vector]

    def vsim_add(self, key: str, vector: Sequence[float]) -> str:
        """Insert or replace a vector in the HNSW index.

        ``VSIM.ADD <key> <v1> <v2> … <vN>``

        Parameters
        ----------
        key : str
            Unique identifier for this vector.
        vector : sequence of float
            The embedding (any dimensionality — must be consistent).

        Returns
        -------
        str
            ``"OK"`` on success.
        """
        return self.execute("VSIM.ADD", key, *self._floats_to_args(vector))

    def vsim_add_bin(self, key: str, vector: Sequence[float]) -> str:
        """Insert or replace a vector in the HNSW index using binary serialization.

        ``VSIM.ADDBIN <key> <binary_vec_bytes>``

        Parameters
        ----------
        key : str
            Unique identifier for this vector.
        vector : sequence of float
            The embedding (any dimensionality — must be consistent).

        Returns
        -------
        str
            ``"OK"`` on success.
        """
        if hasattr(vector, "tobytes"):
            # numpy array — zero-copy, ~1µs vs struct.pack(*vector) ~200µs
            if hasattr(vector, "dtype") and str(vector.dtype) != "float32":
                b_vec = vector.astype("float32").tobytes()
            else:
                b_vec = vector.tobytes()
        else:
            b_vec = struct.pack(f"<{len(vector)}f", *vector)
        return self.execute("VSIM.ADDBIN", key, b_vec)

    def vsim_del(self, key: str) -> int:
        """Delete a vector from the HNSW index.

        ``VSIM.DEL <key>``

        Returns ``1`` if the key existed, ``0`` otherwise.
        """
        return self.execute("VSIM.DEL", key)

    def vsim_search(
        self,
        query_vector: Sequence[float],
        k: int = 10,
    ) -> List[Tuple[str, float]]:
        """K-nearest-neighbour search over the HNSW index.

        ``VSIM.SEARCH <K> <v1> <v2> … <vN>``

        Parameters
        ----------
        query_vector : sequence of float
            The query embedding.
        k : int
            Number of nearest neighbours to return.

        Returns
        -------
        list of (key, distance) tuples
            Sorted by ascending distance (Euclidean).
        """
        raw = self.execute("VSIM.SEARCH", str(k), *self._floats_to_args(query_vector))
        return self._parse_search_results(raw)

    def vsim_search_bin(
        self,
        query_vector: Union[Sequence[float], Any],
        k: int = 10,
    ) -> List[Tuple[str, float]]:
        """K-nearest-neighbour search over the HNSW index using binary serialization.

        ``VSIM.SEARCHBIN <K> <binary_vec_bytes>``
        """
        if hasattr(query_vector, "tobytes"):
            if hasattr(query_vector, "dtype") and query_vector.dtype != "float32":
                b_vec = query_vector.astype("float32").tobytes()
            else:
                b_vec = query_vector.tobytes()
        else:
            b_vec = struct.pack(f"<{len(query_vector)}f", *query_vector)

        raw = self.execute("VSIM.SEARCHBIN", str(k), b_vec)
        return self._parse_search_results(raw)

    def vsim_search_filter(
        self,
        query_vector: Sequence[float],
        k: int,
        filter_field: str,
        filter_value: str,
    ) -> List[Tuple[str, float]]:
        """Filtered KNN search — metadata filter mode.

        ``VSIM.SEARCHFILTER <K> <field> <value> <v1> … <vN>``

        For each candidate vector with key ``X``, the server checks::

            GET "<filter_field>:<X>" == filter_value

        Parameters
        ----------
        query_vector : sequence of float
        k : int
        filter_field : str
            The metadata field name (e.g. ``"category"``).
        filter_value : str
            The required metadata value (e.g. ``"electronics"``).
        """
        raw = self.execute(
            "VSIM.SEARCHFILTER", str(k), filter_field, filter_value,
            *self._floats_to_args(query_vector),
        )
        return self._parse_search_results(raw)

    def vsim_search_prefix(
        self,
        query_vector: Sequence[float],
        k: int,
        prefix: str,
    ) -> List[Tuple[str, float]]:
        """Filtered KNN search — key prefix mode.

        ``VSIM.SEARCHFILTER <K> PREFIX <prefix> <v1> … <vN>``

        Only vectors whose key starts with *prefix* are considered.
        """
        raw = self.execute(
            "VSIM.SEARCHFILTER", str(k), "PREFIX", prefix,
            *self._floats_to_args(query_vector),
        )
        return self._parse_search_results(raw)

    def vsim_search_range(
        self,
        query_vector: Sequence[float],
        k: int,
        zset_name: str,
        min_score: float,
        max_score: float,
    ) -> List[Tuple[str, float]]:
        """Combined vector + sorted-set score range search.

        ``VSIM.SEARCHRANGE <K> <setName> <minScore> <maxScore> <v1> … <vN>``

        First the B+-Tree narrows the candidate set to members whose
        ZSet score is within ``[min_score, max_score]``, then HNSW finds
        the *K* nearest among those.
        """
        raw = self.execute(
            "VSIM.SEARCHRANGE", str(k), zset_name,
            repr(min_score), repr(max_score),
            *self._floats_to_args(query_vector),
        )
        return self._parse_search_results(raw)

    def vsim_info(self) -> dict:
        """Return vector store statistics.

        ``VSIM.INFO``

        Returns a dict like::

            {"vectors": 1000, "dimension": 640, "max_level": 4}
        """
        raw = self.execute("VSIM.INFO")
        result = {}
        for part in raw.split():
            if ":" in part:
                k, v = part.split(":", 1)
                try:
                    result[k] = int(v)
                except ValueError:
                    result[k] = v
        return result

    # ── Sorted Sets (ZSet) ──────────────────────────────────────────

    def zadd(self, key: str, score: float, member: str) -> int:
        """``ZADD key score member``.  Returns ``1`` if new."""
        return self.execute("ZADD", key, repr(score), member)

    def zscore(self, key: str, member: str) -> Optional[float]:
        """``ZSCORE key member``.  Returns the score or ``None``."""
        val = self.execute("ZSCORE", key, member)
        return float(val) if val is not None else None

    def zrem(self, key: str, member: str) -> int:
        """``ZREM key member``.  Returns ``1`` if removed."""
        return self.execute("ZREM", key, member)

    def zrangebyscore(
        self,
        key: str,
        min_score: float,
        max_score: float,
        withscores: bool = False,
    ) -> list:
        """``ZRANGEBYSCORE key min max [WITHSCORES]``."""
        args = ["ZRANGEBYSCORE", key, repr(min_score), repr(max_score)]
        if withscores:
            args.append("WITHSCORES")
        raw = self.execute(*args)
        if withscores and raw:
            return [(raw[i], float(raw[i + 1])) for i in range(0, len(raw), 2)]
        return raw or []

    def zcard(self, key: str) -> int:
        """``ZCARD key``.  Returns the cardinality of the sorted set."""
        return self.execute("ZCARD", key)

    # ── AI commands (require Ollama) ────────────────────────────────

    def ai_embed(self, text: str) -> List[float]:
        """``AI.EMBED <text>``.  Returns the embedding vector.

        Requires Ollama to be running (``nomic-embed-text``).
        """
        raw = self.execute("AI.EMBED", text)
        return [float(v) for v in raw]

    def ai_search(self, text: str, k: int = 5) -> List[Tuple[str, float]]:
        """``AI.SEARCH <K> <text>``.  Semantic search by natural language."""
        raw = self.execute("AI.SEARCH", str(k), text)
        return self._parse_search_results(raw)

    def ai_ask(self, question: str) -> str:
        """``AI.ASK <question>``.  RAG: embed → search → LLM answer."""
        return self.execute("AI.ASK", question)

    def ai_ingest(self, key: str, text: str) -> str:
        """``AI.INGEST <key> <text>``.  Async: enqueue text for embedding.

        Returns ``"QUEUED"``.
        """
        return self.execute("AI.INGEST", key, text)

    # ── helpers ─────────────────────────────────────────────────────

    @staticmethod
    def _parse_search_results(raw: Optional[list]) -> List[Tuple[str, float]]:
        """Parse the interleaved ``[key, distance, key, distance, …]``
        array returned by ``VSIM.SEARCH*``."""
        if not raw:
            return []
        results: list[tuple[str, float]] = []
        for i in range(0, len(raw), 2):
            key = raw[i]
            dist = float(raw[i + 1])
            results.append((key, dist))
        return results

    # ── batch / pipeline helper ─────────────────────────────────────

    def vsim_add_batch(
        self,
        items: Sequence[Tuple[str, Sequence[float]]],
    ) -> List[str]:
        """Add multiple vectors efficiently (sequential pipelining).

        Parameters
        ----------
        items : list of (key, vector) tuples

        Returns
        -------
        list of server responses (``"OK"`` per item).
        """
        results = []
        for key, vec in items:
            results.append(self.vsim_add(key, vec))
        return results

    def pipeline(self) -> "MoltenPipeline":
        """Create a pipeline context for batching multiple commands in a single round-trip."""
        return MoltenPipeline(self)


class MoltenPipeline:
    """A Redis-style pipeline for batching multiple commands in a single round-trip.

    Usage::

        with client.pipeline() as pipe:
            pipe.set("a", "1")
            pipe.get("a")
            pipe.vsim_search_bin(query, k=5)
            results = pipe.execute()
    """

    def __init__(self, client: MoltenClient) -> None:
        self.client = client
        self.commands: List[bytes] = []
        self.callbacks: List[Optional[Callable[[Any], Any]]] = []

    def __enter__(self) -> "MoltenPipeline":
        return self

    def __exit__(self, *exc: Any) -> None:
        self.reset()

    def reset(self) -> None:
        self.commands.clear()
        self.callbacks.clear()

    def queue(self, cmd: str, *args: Union[str, bytes, int, float], callback: Optional[Callable[[Any], Any]] = None) -> "MoltenPipeline":
        raw_cmd = self.client._conn.encode(cmd, *args)
        self.commands.append(raw_cmd)
        self.callbacks.append(callback)
        return self

    def set(self, key: str, value: str, ex: Optional[int] = None) -> "MoltenPipeline":
        if ex is not None:
            return self.queue("SET", key, value, "EX", str(ex))
        return self.queue("SET", key, value)

    def get(self, key: str) -> "MoltenPipeline":
        return self.queue("GET", key)

    def delete(self, key: str) -> "MoltenPipeline":
        return self.queue("DEL", key)

    def vsim_add(self, key: str, vector: Sequence[float]) -> "MoltenPipeline":
        return self.queue("VSIM.ADD", key, *self.client._floats_to_args(vector))

    def vsim_add_bin(self, key: str, vector: Sequence[float]) -> "MoltenPipeline":
        if hasattr(vector, "tobytes"):
            if hasattr(vector, "dtype") and str(vector.dtype) != "float32":
                b_vec = vector.astype("float32").tobytes()
            else:
                b_vec = vector.tobytes()
        else:
            b_vec = struct.pack(f"<{len(vector)}f", *vector)
        return self.queue("VSIM.ADDBIN", key, b_vec)

    def vsim_search(self, query_vector: Sequence[float], k: int = 10) -> "MoltenPipeline":
        return self.queue("VSIM.SEARCH", str(k), *self.client._floats_to_args(query_vector), callback=self.client._parse_search_results)

    def vsim_search_bin(self, query_vector: Union[Sequence[float], Any], k: int = 10) -> "MoltenPipeline":
        if hasattr(query_vector, "tobytes"):
            if hasattr(query_vector, "dtype") and query_vector.dtype != "float32":
                b_vec = query_vector.astype("float32").tobytes()
            else:
                b_vec = query_vector.tobytes()
        else:
            b_vec = struct.pack(f"<{len(query_vector)}f", *query_vector)
        return self.queue("VSIM.SEARCHBIN", str(k), b_vec, callback=self.client._parse_search_results)

    def execute(self) -> List[Any]:
        if not self.commands:
            return []

        conn = self.client._conn
        if conn._sock is None:
            raise ConnectionError("Not connected — call connect() first")

        payload = b"".join(self.commands)
        conn._sock.sendall(payload)

        results = []
        for callback in self.callbacks:
            response = conn.read_response()
            if callback is not None:
                response = callback(response)
            results.append(response)

        self.reset()
        return results


# ──────────────────────────────────────────────────────────────────────
# Convenience: run as script → quick smoke test
# ──────────────────────────────────────────────────────────────────────

if __name__ == "__main__":
    import sys
    import time

    host = sys.argv[1] if len(sys.argv) > 1 else "localhost"
    port = int(sys.argv[2]) if len(sys.argv) > 2 else 6380

    print(f"Connecting to Molten KVStore at {host}:{port} …")

    try:
        with MoltenClient(host, port, timeout=3.0) as c:
            # 1. PING
            print(f"  PING  → {c.ping()}")

            # 2. SET / GET
            c.set("sdk:test", "hello from Python SDK")
            print(f"  GET   → {c.get('sdk:test')}")

            # 2.5 Pipeline test
            print("  Testing Pipeline ...")
            with c.pipeline() as pipe:
                pipe.set("sdk:pipe:1", "val1")
                pipe.set("sdk:pipe:2", "val2")
                pipe.get("sdk:pipe:1")
                pipe.get("sdk:pipe:2")
                res = pipe.execute()
                print(f"    Pipeline execution results: {res}")
                if res != ["OK", "OK", "val1", "val2"]:
                    raise ValueError(f"Pipeline failed, got {res}")
            c.delete("sdk:pipe:1")
            c.delete("sdk:pipe:2")

            # 3. DBSIZE
            print(f"  DBSIZE → {c.dbsize()}")

            # 4. Vector add + search
            dim = 8
            import random
            random.seed(42)
            vecs = {
                f"sdk:vec:{i}": [random.gauss(0, 1) for _ in range(dim)]
                for i in range(20)
            }
            for key, vec in vecs.items():
                c.vsim_add(key, vec)

            query = list(vecs.values())[0]
            results = c.vsim_search(query, k=5)
            print(f"  VSIM.SEARCH (k=5):")
            for key, dist in results:
                print(f"    {key:20s}  dist={dist:.6f}")

            # 5. VSIM.INFO
            info = c.vsim_info()
            print(f"  VSIM.INFO → {info}")

            # 6. Cleanup
            for key in vecs:
                c.vsim_del(key)
            c.delete("sdk:test")

            print("\n✅  All smoke tests passed!")

    except ConnectionRefusedError:
        print(f"❌  Could not connect to {host}:{port}. Is the server running?")
        sys.exit(1)
