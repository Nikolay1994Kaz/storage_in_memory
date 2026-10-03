package main

// Команды строкового KV и TTL: SET, GET, DEL, EXPIRE, TTL, PERSIST, а также
// PING, DBSIZE и COMPACT.

import (
	"encoding/binary"
	"fmt"
	"strconv"
	"strings"
	"time"

	"kvstore/kvstore/internal/server"
	"kvstore/kvstore/internal/store"
	"kvstore/kvstore/internal/wal"
)

// cmdPing — PING: проверка соединения, отвечает PONG.
func (c *commands) cmdPing(cs *server.ConnState, _ [][]byte) {
	buf := cs.Buf
	buf.WriteSimpleString("PONG")
}

// cmdSet — SET key value [EX seconds]. Голый SET снимает прежний срок жизни.
func (c *commands) cmdSet(cs *server.ConnState, args [][]byte) {
	buf := cs.Buf
	if len(args) < 2 {
		buf.WriteError("ERR wrong number of arguments for 'SET'")
		return
	}
	key := string(args[0])
	if c.cluster != nil {
		if moved := c.cluster.CheckKey(key); moved != nil {
			writeValue(buf, *moved)
			return
		}
	}
	// OOM защищён единым гейтом в начале execute.
	// args[1] — срез буфера соединения, он будет перезаписан следующей
	// командой. Копия одна на всех: её читает WAL и забирает во владение
	// стор (kv.Store не копирует), никто из них её не меняет.
	value := make([]byte, len(args[1]))
	copy(value, args[1])

	c.wal.Write(wal.Entry{Op: wal.OpSet, Key: key, Value: value})
	c.kv.Set(key, value)

	if c.cluster != nil {
		c.cluster.ForwardWrite(fmt.Sprintf("SET %s %s", key, string(value)))
	}

	if len(args) >= 4 && strings.ToUpper(string(args[2])) == "EX" {
		seconds, err := strconv.Atoi(string(args[3]))
		if err != nil || seconds <= 0 {
			buf.WriteError("ERR invalid expire time")
			return
		}
		dur := time.Duration(seconds) * time.Second
		expiresAt := time.Now().Add(dur)
		var b [8]byte
		binary.BigEndian.PutUint64(b[:], uint64(expiresAt.UnixNano()))
		c.wal.Write(wal.Entry{Op: wal.OpExpire, Key: key, Value: b[:]})
		c.ttl.Set(key, dur)
	} else {
		// Голый SET (без EX) снимает прежний TTL — семантика Redis без
		// KEEPTTL. Иначе новое значение унаследует старый таймер и умрёт
		// неожиданно. OpPersist пишется для durability (реплей/компакция).
		setClearTTL(c.ttl, c.wal, key)
	}

	buf.WriteSimpleString("OK")
}

// cmdGet — GET key.
func (c *commands) cmdGet(cs *server.ConnState, args [][]byte) {
	buf := cs.Buf
	if len(args) < 1 {
		buf.WriteError("ERR wrong number of arguments for 'GET'")
		return
	}
	key := string(args[0])
	if c.cluster != nil && !c.cluster.IsReplica() {
		if moved := c.cluster.CheckKey(key); moved != nil {
			writeValue(buf, *moved)
			return
		}
	}
	if c.ttl.IsExpired(key) {
		buf.WriteNull()
		return
	}
	val, ok := c.kv.Get(key)
	if !ok {
		if c.cluster != nil {
			if ask := c.cluster.CheckKeyAsk(key); ask != nil {
				writeValue(buf, *ask)
				return
			}
		}
		buf.WriteNull()
		return
	}
	buf.WriteBulk(val)
}

// cmdDel — DEL key: удаляет значение и вектор с тем же ключом.
func (c *commands) cmdDel(cs *server.ConnState, args [][]byte) {
	buf := cs.Buf
	if len(args) < 1 {
		buf.WriteError("ERR wrong number of arguments for 'DEL'")
		return
	}
	key := string(args[0])
	if c.cluster != nil {
		if moved := c.cluster.CheckKey(key); moved != nil {
			writeValue(buf, *moved)
			return
		}
	}
	c.wal.Write(wal.Entry{Op: wal.OpDel, Key: key})
	ok := c.kv.Del(key)
	c.vec.Delete(key) // Также удаляем вектор при ручном DEL
	c.ttl.OnDelete(key)
	if c.cluster != nil {
		c.cluster.ForwardWrite(fmt.Sprintf("DEL %s", key))
	}
	if ok {
		buf.WriteInt(1)
	} else {
		buf.WriteInt(0)
	}
}

// cmdExpire — EXPIRE key seconds.
func (c *commands) cmdExpire(cs *server.ConnState, args [][]byte) {
	buf := cs.Buf
	if len(args) < 2 {
		buf.WriteError("ERR wrong number of arguments for 'EXPIRE'")
		return
	}
	key := string(args[0])
	if c.cluster != nil {
		if moved := c.cluster.CheckKey(key); moved != nil {
			writeValue(buf, *moved)
			return
		}
	}
	if _, ok := c.kv.Get(key); !ok {
		buf.WriteInt(0)
		return
	}
	seconds, err := strconv.Atoi(string(args[1]))
	if err != nil || seconds <= 0 {
		buf.WriteError("ERR invalid expire time")
		return
	}
	dur := time.Duration(seconds) * time.Second
	expiresAt := time.Now().Add(dur)
	var b [8]byte
	binary.BigEndian.PutUint64(b[:], uint64(expiresAt.UnixNano()))
	c.wal.Write(wal.Entry{Op: wal.OpExpire, Key: key, Value: b[:]})
	c.ttl.Set(key, dur)
	buf.WriteInt(1)
}

// cmdTTL — TTL key: секунды до истечения; -1 — без срока, -2 — ключа нет.
func (c *commands) cmdTTL(cs *server.ConnState, args [][]byte) {
	buf := cs.Buf
	if len(args) < 1 {
		buf.WriteError("ERR wrong number of arguments for 'TTL'")
		return
	}
	key := string(args[0])
	if c.cluster != nil {
		if moved := c.cluster.CheckKey(key); moved != nil {
			writeValue(buf, *moved)
			return
		}
	}
	if _, ok := c.kv.Get(key); !ok {
		buf.WriteInt(-2)
		return
	}
	remaining := c.ttl.TTL(key)
	if remaining == -1 {
		buf.WriteInt(-1)
		return
	}
	buf.WriteInt(int(remaining.Seconds()))
}

// cmdPersist — PERSIST key: снимает срок жизни.
func (c *commands) cmdPersist(cs *server.ConnState, args [][]byte) {
	buf := cs.Buf
	if len(args) < 1 {
		buf.WriteError("ERR wrong number of arguments for 'PERSIST'")
		return
	}
	key := string(args[0])
	if c.cluster != nil {
		if moved := c.cluster.CheckKey(key); moved != nil {
			writeValue(buf, *moved)
			return
		}
	}
	if c.ttl.Remove(key) {
		c.wal.Write(wal.Entry{Op: wal.OpPersist, Key: key})
		buf.WriteInt(1)
	} else {
		buf.WriteInt(0)
	}
}

// cmdDBSize — DBSIZE: число строковых ключей.
func (c *commands) cmdDBSize(cs *server.ConnState, _ [][]byte) {
	buf := cs.Buf
	buf.WriteInt(c.kv.Len())
}

// cmdCompact — COMPACT: фоновая компакция журнала со снапшотом состояния.
func (c *commands) cmdCompact(cs *server.ConnState, _ [][]byte) {
	buf := cs.Buf
	wal.BackgroundCompact(c.wal.RawWAL(), dataDir, c.iterateAll, c.saveVectors)
	buf.WriteSimpleString("OK compaction started")
}

// setClearTTL снимает прежний TTL с ключа при голом SET (без EX) — семантика
// Redis без KEEPTTL: перезапись значения сбрасывает срок жизни, иначе новое
// значение умрёт по старому таймеру.
//
// OpPersist пишется в WAL ТОЛЬКО если TTL реально был (ttl.Remove вернул true) —
// без спама WAL на каждый SET и с durability: при реплее/компакции прежний
// OpExpire не воскресит удалённый TTL (реплей: OpSet(new) → OpPersist(снять)).
//
// Вынесено из inline-обработчика SET для тестируемости.
func setClearTTL(ttl *store.TTLManager, bw *wal.BatchWAL, key string) {
	if ttl.Remove(key) {
		bw.Write(wal.Entry{Op: wal.OpPersist, Key: key})
	}
}
