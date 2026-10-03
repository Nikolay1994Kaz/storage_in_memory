package main

// Команды sorted set: ZADD, ZSCORE, ZREM, ZRANGEBYSCORE, ZCARD.

import (
	"math"
	"strconv"
	"strings"

	"kvstore/kvstore/internal/server"
	"kvstore/kvstore/internal/store/zset"
	"kvstore/kvstore/internal/wal"
)

// cmdZAdd — ZADD key score member.
func (c *commands) cmdZAdd(cs *server.ConnState, args [][]byte) {
	buf := cs.Buf
	// ZADD <setName> <score> <member>
	if len(args) < 3 {
		buf.WriteError("ERR usage: ZADD <key> <score> <member>")
		return
	}
	setName := string(args[0])
	score, err := strconv.ParseFloat(unsafeString(args[1]), 64)
	// NaN ParseFloat принимает молча, а в упорядоченном индексе он ломает
	// сравнение — Redis тоже отвечает на него ошибкой.
	if err != nil || math.IsNaN(score) {
		buf.WriteError("ERR invalid score (must be a float)")
		return
	}
	member := string(args[2])
	isNew := c.zsets.ZAdd(setName, score, member)
	// WAL
	c.wal.Write(wal.Entry{Op: wal.OpZAdd, Key: setName, Value: zset.EncodeZAddValue(score, member)})
	if isNew {
		buf.WriteInt(1)
	} else {
		buf.WriteInt(0)
	}
}

// cmdZScore — ZSCORE key member.
func (c *commands) cmdZScore(cs *server.ConnState, args [][]byte) {
	buf := cs.Buf
	// ZSCORE <setName> <member>
	if len(args) < 2 {
		buf.WriteError("ERR usage: ZSCORE <key> <member>")
		return
	}
	setName := string(args[0])
	member := string(args[1])
	score, ok := c.zsets.ZScore(setName, member)
	if !ok {
		buf.WriteNull()
	} else {
		buf.WriteBulkString(strconv.FormatFloat(score, 'f', -1, 64))
	}
}

// cmdZRem — ZREM key member.
func (c *commands) cmdZRem(cs *server.ConnState, args [][]byte) {
	buf := cs.Buf
	// ZREM <setName> <member>
	if len(args) < 2 {
		buf.WriteError("ERR usage: ZREM <key> <member>")
		return
	}
	setName := string(args[0])
	member := string(args[1])
	ok := c.zsets.ZRem(setName, member)
	if ok {
		c.wal.Write(wal.Entry{Op: wal.OpZRem, Key: setName, Value: []byte(member)})
		buf.WriteInt(1)
	} else {
		buf.WriteInt(0)
	}
}

// cmdZRangeByScore — ZRANGEBYSCORE key min max [WITHSCORES].
func (c *commands) cmdZRangeByScore(cs *server.ConnState, args [][]byte) {
	buf := cs.Buf
	// ZRANGEBYSCORE <setName> <min> <max> [WITHSCORES]
	if len(args) < 3 {
		buf.WriteError("ERR usage: ZRANGEBYSCORE <key> <min> <max> [WITHSCORES]")
		return
	}
	setName := string(args[0])
	minScore, err := strconv.ParseFloat(unsafeString(args[1]), 64)
	if err != nil || math.IsNaN(minScore) {
		buf.WriteError("ERR invalid min score")
		return
	}
	maxScore, err := strconv.ParseFloat(unsafeString(args[2]), 64)
	if err != nil || math.IsNaN(maxScore) {
		buf.WriteError("ERR invalid max score")
		return
	}
	withScores := len(args) >= 4 && strings.ToUpper(string(args[3])) == "WITHSCORES"

	results := c.zsets.ZRangeByScore(setName, minScore, maxScore)
	if withScores {
		buf.WriteArrayHeader(len(results) * 2)
		for _, r := range results {
			buf.WriteBulkString(r.Member)
			buf.WriteBulkString(strconv.FormatFloat(r.Score, 'f', -1, 64))
		}
	} else {
		buf.WriteArrayHeader(len(results))
		for _, r := range results {
			buf.WriteBulkString(r.Member)
		}
	}
}

// cmdZCard — ZCARD key.
func (c *commands) cmdZCard(cs *server.ConnState, args [][]byte) {
	buf := cs.Buf
	// ZCARD <setName>
	if len(args) < 1 {
		buf.WriteError("ERR usage: ZCARD <key>")
		return
	}
	setName := string(args[0])
	buf.WriteInt(c.zsets.ZCard(setName))
}
