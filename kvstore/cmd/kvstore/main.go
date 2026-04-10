package main

import (
	"encoding/binary"
	"flag"
	"fmt"
	"log"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"kvstore/kvstore/internal/cluster"
	"kvstore/kvstore/internal/protocol"
	"kvstore/kvstore/internal/pubsub"
	"kvstore/kvstore/internal/server"
	"kvstore/kvstore/internal/store"
	"kvstore/kvstore/internal/wal"
)

const (
	dataDir      = "data"
	syncInterval = 100 * time.Millisecond
)

func main() {
	// CLI-флаги
	port := flag.Int("port", 6380, "порт для клиентов")
	clusterEnabled := flag.Bool("cluster", false, "включить кластерный режим")
	clusterSlotStart := flag.Int("slot-start", 0, "начало диапазона слотов")
	clusterSlotEnd := flag.Int("slot-end", 16383, "конец диапазона слотов")
	flag.Parse()

	s := store.NewArenaStore()

	os.MkdirAll(dataDir, 0755)

	// === 1. TTL Manager (до восстановления, чтобы восстановить TTL из WAL) ===
	ttl := store.NewTTLManager(s)
	defer ttl.Stop()

	// === 2. Восстановление ===
	entries, err := wal.ReadAllWALs(dataDir)
	if err != nil {
		log.Fatalf("Failed to read WALs: %v", err)
	}

	restored := 0
	for _, entry := range entries {
		switch entry.Op {
		case wal.OpSet:
			s.Set(entry.Key, entry.Value)
			restored++
		case wal.OpDel:
			s.Del(entry.Key)
			ttl.OnDelete(entry.Key)
			restored++
		case wal.OpExpire:
			if len(entry.Value) == 8 {
				expiresAt := time.Unix(0, int64(binary.BigEndian.Uint64(entry.Value)))
				remaining := time.Until(expiresAt)
				if remaining > 0 {
					ttl.Set(entry.Key, remaining)
				} else {
					// Ключ уже просрочен — удаляем
					s.Del(entry.Key)
					ttl.OnDelete(entry.Key)
				}
			}
			restored++
		case wal.OpPersist:
			ttl.Remove(entry.Key)
			restored++
		}
	}

	if restored > 0 {
		log.Printf("Restored %d operations from WAL", restored)
	}

	// === 3. WAL ===
	walPath := filepath.Join(dataDir, fmt.Sprintf("wal_%s.log", time.Now().Format("20060102_150405")))
	w, err := wal.Open(walPath)
	if err != nil {
		log.Fatalf("Failed to open WAL: %v", err)
	}
	defer w.Close()

	// === 4. Syncer ===
	syncer := wal.NewSyncer(w, syncInterval, dataDir, s.ForEach)
	defer syncer.Stop()

	// === 5. Pub/Sub Hub ===
	hub := pubsub.NewHub()

	// === 6. Cluster (опционально) ===
	var cl *cluster.Cluster
	if *clusterEnabled {
		addr := fmt.Sprintf("127.0.0.1:%d", *port)
		cl = cluster.New(addr, *port+1)
		cl.State.Self.AssignSlots(*clusterSlotStart, *clusterSlotEnd)
		cl.State.RebuildSlotTable()
		log.Printf("Cluster mode: node %s, slots %d-%d",
			cl.State.Self.ID, *clusterSlotStart, *clusterSlotEnd)
		cl.GetKeysInSlotFunc = func(slot uint16, count int) []string {
			return s.GetKeysInSlot(slot, count, cluster.KeySlot)
		}
		cl.MigrateGetFunc = func(key string) ([]byte, bool) {
			return s.Get(key)
		}
		cl.MigrateDelFunc = func(key string) {
			s.Del(key)
			ttl.OnDelete(key)
		}

		// Callback-функции для репликации:
		cl.Repl.StoreForEach = func(fn func(key string, value []byte)) {
			s.ForEach(fn)
		}
		cl.Repl.StoreSet = func(key string, value []byte) {
			s.Set(key, value)
		}
		cl.Repl.StoreDel = func(key string) {
			s.Del(key)
		}

		// Запуск Gossip (PING/PONG между нодами)
		if err := cl.StartGossip(); err != nil {
			log.Fatalf("Failed to start gossip: %v", err)
		}
		defer cl.StopGossip()
	}

	// === 7. Handler ===
	handler := func(conn net.Conn, args []protocol.Value) protocol.Value {
		cmd := strings.ToUpper(args[0].Str)
		cmdArgs := args[1:]
		return executeCommand(s, w, ttl, hub, cl, conn, cmd, cmdArgs)
	}

	// === 8. Сервер ===
	listenAddr := fmt.Sprintf(":%d", *port)
	srv := server.NewServer(listenAddr, handler)
	if err := srv.Start(); err != nil {
		fmt.Fprintf(os.Stderr, "Failed to start: %v\n", err)
		os.Exit(1)
	}

	log.Println("KVStore is running. Press Ctrl+C to stop.")

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	<-sigCh

	log.Println("Shutting down...")
	srv.Stop()
}

func executeCommand(s *store.ArenaStore, w *wal.WAL, ttl *store.TTLManager, hub *pubsub.Hub, cl *cluster.Cluster, conn net.Conn, cmd string, args []protocol.Value) protocol.Value {
	switch cmd {
	case "PING":
		return protocol.Value{Typ: '+', Str: "PONG"}

	// === Cluster команды ===
	case "CLUSTER":
		if cl != nil {
			return cl.HandleClusterCommand(args)
		}
		return protocol.Value{Typ: '-', Str: "ERR cluster mode is not enabled"}
	case "MIGRATE":
		// MIGRATE <host> <port> <key>
		if cl == nil {
			return protocol.Value{Typ: '-', Str: "ERR cluster mode is not enabled"}
		}
		if len(args) < 3 {
			return protocol.Value{Typ: '-', Str: "ERR wrong number of arguments for 'MIGRATE'"}
		}
		host := args[0].Str
		port, err := strconv.Atoi(args[1].Str)
		if err != nil {
			return protocol.Value{Typ: '-', Str: "ERR invalid port"}
		}
		key := args[2].Str
		return cl.MigrateKey(host, port, key)

	case "PSYNC":
		if cl == nil {
			return protocol.Value{Typ: '-', Str: "ERR cluster mode is not enabled"}
		}
		if len(args) < 1 {
			return protocol.Value{Typ: '-', Str: "ERR wrong number of arguments for 'PSYNC'"}
		}
		replicaID := args[0].Str
		cl.Repl.HandlePsync(conn, replicaID)
		return protocol.Value{Typ: '+', Str: ""}

	case "SET":
		if len(args) < 2 {
			return protocol.Value{Typ: '-', Str: "ERR wrong number of arguments for 'SET'"}
		}
		key := args[0].Str
		// Кластерная маршрутизация: проверяем, наш ли слот
		if cl != nil {
			if moved := cl.CheckKey(key); moved != nil {
				return *moved
			}
		}
		value := []byte(args[1].Str)

		if err := w.Write(wal.Entry{Op: wal.OpSet, Key: key, Value: value}); err != nil {
			return protocol.Value{Typ: '-', Str: "ERR WAL write failed"}
		}
		s.Set(key, value)

		// Репликация: пересылаем всем репликам
		if cl != nil && cl.Repl != nil {
			cl.Repl.ForwardWrite(fmt.Sprintf("SET %s %s", key, string(value)))
		}

		if len(args) >= 4 && strings.ToUpper(args[2].Str) == "EX" {
			seconds, err := strconv.Atoi(args[3].Str)
			if err != nil || seconds <= 0 {
				return protocol.Value{Typ: '-', Str: "ERR invalid expire time"}
			}
			dur := time.Duration(seconds) * time.Second
			expiresAt := time.Now().Add(dur)
			var buf [8]byte
			binary.BigEndian.PutUint64(buf[:], uint64(expiresAt.UnixNano()))
			if err := w.Write(wal.Entry{Op: wal.OpExpire, Key: key, Value: buf[:]}); err != nil {
				return protocol.Value{Typ: '-', Str: "ERR WAL write failed"}
			}
			ttl.Set(key, dur)
		}

		return protocol.Value{Typ: '+', Str: "OK"}

	case "GET":
		if len(args) < 1 {
			return protocol.Value{Typ: '-', Str: "ERR wrong number of arguments for 'GET'"}
		}
		key := args[0].Str
		// Реплики отдают GET напрямую (read-only), без MOVED
		if cl != nil && cl.State.Self.Role != cluster.RoleReplica {
			if moved := cl.CheckKey(key); moved != nil {
				return *moved
			}
		}

		if ttl.IsExpired(key) {
			return protocol.Value{Typ: '$', Num: -1}
		}

		val, ok := s.Get(key)
		if !ok {
			// Ключа нет — может, он уже мигрировал?
			if cl != nil {
				if ask := cl.CheckKeyAsk(key); ask != nil {
					return *ask
				}
			}
			return protocol.Value{Typ: '$', Num: -1}
		}
		return protocol.Value{Typ: '$', Str: string(val)}

	case "DEL":
		if len(args) < 1 {
			return protocol.Value{Typ: '-', Str: "ERR wrong number of arguments for 'DEL'"}
		}
		key := args[0].Str
		if cl != nil {
			if moved := cl.CheckKey(key); moved != nil {
				return *moved
			}
		}
		if err := w.Write(wal.Entry{Op: wal.OpDel, Key: key}); err != nil {
			return protocol.Value{Typ: '-', Str: "ERR WAL write failed"}
		}
		ok := s.Del(key)
		ttl.OnDelete(key)

		// Репликация: пересылаем удаление
		if cl != nil && cl.Repl != nil {
			cl.Repl.ForwardWrite(fmt.Sprintf("DEL %s", key))
		}

		if ok {
			return protocol.Value{Typ: ':', Num: 1}
		}
		return protocol.Value{Typ: ':', Num: 0}

	case "EXPIRE":
		if len(args) < 2 {
			return protocol.Value{Typ: '-', Str: "ERR wrong number of arguments for 'EXPIRE'"}
		}
		key := args[0].Str
		if cl != nil {
			if moved := cl.CheckKey(key); moved != nil {
				return *moved
			}
		}
		if _, ok := s.Get(key); !ok {
			return protocol.Value{Typ: ':', Num: 0}
		}
		seconds, err := strconv.Atoi(args[1].Str)
		if err != nil || seconds <= 0 {
			return protocol.Value{Typ: '-', Str: "ERR invalid expire time"}
		}
		dur := time.Duration(seconds) * time.Second
		expiresAt := time.Now().Add(dur)
		var buf [8]byte
		binary.BigEndian.PutUint64(buf[:], uint64(expiresAt.UnixNano()))
		if err := w.Write(wal.Entry{Op: wal.OpExpire, Key: key, Value: buf[:]}); err != nil {
			return protocol.Value{Typ: '-', Str: "ERR WAL write failed"}
		}
		ttl.Set(key, dur)
		return protocol.Value{Typ: ':', Num: 1}

	case "TTL":
		if len(args) < 1 {
			return protocol.Value{Typ: '-', Str: "ERR wrong number of arguments for 'TTL'"}
		}
		key := args[0].Str
		if cl != nil {
			if moved := cl.CheckKey(key); moved != nil {
				return *moved
			}
		}
		if _, ok := s.Get(key); !ok {
			return protocol.Value{Typ: ':', Num: -2}
		}
		remaining := ttl.TTL(key)
		if remaining == -1 {
			return protocol.Value{Typ: ':', Num: -1}
		}
		return protocol.Value{Typ: ':', Num: int(remaining.Seconds())}

	case "PERSIST":
		if len(args) < 1 {
			return protocol.Value{Typ: '-', Str: "ERR wrong number of arguments for 'PERSIST'"}
		}
		if cl != nil {
			if moved := cl.CheckKey(args[0].Str); moved != nil {
				return *moved
			}
		}
		if ttl.Remove(args[0].Str) {
			if err := w.Write(wal.Entry{Op: wal.OpPersist, Key: args[0].Str}); err != nil {
				return protocol.Value{Typ: '-', Str: "ERR WAL write failed"}
			}
			return protocol.Value{Typ: ':', Num: 1}
		}
		return protocol.Value{Typ: ':', Num: 0}

	// === Pub/Sub ===
	case "SUBSCRIBE":
		if len(args) < 1 {
			return protocol.Value{Typ: '-', Str: "ERR wrong number of arguments for 'SUBSCRIBE'"}
		}
		channels := make([]string, len(args))
		for i, arg := range args {
			channels[i] = arg.Str
		}
		hub.Subscribe(conn, channels)
		// Подтверждения отправляются через writePump, не через обычный handler
		return protocol.Value{Typ: 0} // пустой ответ — writePump уже отправил

	case "UNSUBSCRIBE":
		channels := make([]string, len(args))
		for i, arg := range args {
			channels[i] = arg.Str
		}
		hub.Unsubscribe(conn, channels)
		return protocol.Value{Typ: '+', Str: "OK"}

	case "PUBLISH":
		if len(args) < 2 {
			return protocol.Value{Typ: '-', Str: "ERR wrong number of arguments for 'PUBLISH'"}
		}
		count := hub.Publish(args[0].Str, args[1].Str)
		return protocol.Value{Typ: ':', Num: count}

	case "DBSIZE":
		return protocol.Value{Typ: ':', Num: s.Len()}

	case "COMPACT":
		wal.BackgroundCompact(w, dataDir, s.ForEach)
		return protocol.Value{Typ: '+', Str: "OK compaction started"}

	default:
		return protocol.Value{Typ: '-', Str: fmt.Sprintf("ERR unknown command '%s'", cmd)}
	}
}
