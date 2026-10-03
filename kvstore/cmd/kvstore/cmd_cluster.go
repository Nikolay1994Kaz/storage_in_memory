package main

// Команды кластера: CLUSTER, MIGRATE, PSYNC. Вне кластерного режима
// отвечают ошибкой; сама кластерная логика — за тегом сборки experimental.

import (
	"strconv"

	"kvstore/kvstore/internal/protocol"
	"kvstore/kvstore/internal/server"
)

// cmdCluster — CLUSTER <подкоманда>; только в кластерном режиме.
func (c *commands) cmdCluster(cs *server.ConnState, args [][]byte) {
	buf := cs.Buf
	if c.cluster != nil {
		// Конвертируем [][]byte → []protocol.Value для legacy cluster API
		pArgs := make([]protocol.Value, len(args))
		for i, a := range args {
			pArgs[i] = protocol.Value{Typ: '$', Str: string(a)}
		}
		writeValue(buf, c.cluster.HandleClusterCommand(pArgs))
	} else {
		buf.WriteError("ERR cluster mode is not enabled")
	}
}

// cmdMigrate — MIGRATE host port key; только в кластерном режиме.
func (c *commands) cmdMigrate(cs *server.ConnState, args [][]byte) {
	buf := cs.Buf
	if c.cluster == nil {
		buf.WriteError("ERR cluster mode is not enabled")
		return
	}
	if len(args) < 3 {
		buf.WriteError("ERR wrong number of arguments for 'MIGRATE'")
		return
	}
	host := string(args[0])
	port, err := strconv.Atoi(string(args[1]))
	if err != nil {
		buf.WriteError("ERR invalid port")
		return
	}
	key := string(args[2])
	writeValue(buf, c.cluster.MigrateKey(host, port, key))
}

// cmdPsync — PSYNC replicaID: поток репликации; только в кластерном режиме.
func (c *commands) cmdPsync(cs *server.ConnState, args [][]byte) {
	buf := cs.Buf
	if c.cluster == nil {
		buf.WriteError("ERR cluster mode is not enabled")
		return
	}
	if len(args) < 1 {
		buf.WriteError("ERR wrong number of arguments for 'PSYNC'")
		return
	}
	replicaID := string(args[0])
	c.cluster.HandlePsync(cs.Conn, replicaID)
}

// writeValue — helper для записи protocol.Value в ConnBuf.
// Используется для cluster API (CheckKey, MigrateKey), которые
// всё ещё возвращают protocol.Value.
func writeValue(buf *server.ConnBuf, v protocol.Value) {
	switch v.Typ {
	case '+':
		buf.WriteSimpleString(v.Str)
	case '-':
		buf.WriteError(v.Str)
	case ':':
		buf.WriteInt(v.Num)
	case '$':
		if v.Num == -1 {
			buf.WriteNull()
		} else {
			buf.WriteBulkString(v.Str)
		}
	case '*':
		buf.WriteArrayHeader(len(v.Array))
		for _, item := range v.Array {
			writeValue(buf, item)
		}
	case 0:
		// пустой ответ (SUBSCRIBE — writePump сам отправляет)
	}
}
