package main

import (
	"fmt"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"kvstore/kvstore/internal/protocol"
	"kvstore/kvstore/internal/server"
	"kvstore/kvstore/internal/store"
	"kvstore/kvstore/internal/wal"
)

const (
	dataDir      = "data"
	syncInterval = 100 * time.Millisecond
)

func main() {
	s := store.NewArenaStore()

	os.MkdirAll(dataDir, 0755)

	// === 1. Восстановление ===
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
			restored++
		}
	}

	if restored > 0 {
		log.Printf("Restored %d operations from WAL", restored)
	}

	// === 2. WAL ===
	walPath := filepath.Join(dataDir, fmt.Sprintf("wal_%s.log", time.Now().Format("20060102_150405")))
	w, err := wal.Open(walPath)
	if err != nil {
		log.Fatalf("Failed to open WAL: %v", err)
	}
	defer w.Close()

	// === 3. Syncer ===
	syncer := wal.NewSyncer(w, syncInterval, dataDir, s.ForEach)
	defer syncer.Stop()

	// === 4. TTL Manager ===
	ttl := store.NewTTLManager(s)
	defer ttl.Stop()

	// === 5. Handler ===
	handler := func(args []protocol.Value) protocol.Value {
		cmd := strings.ToUpper(args[0].Str)
		cmdArgs := args[1:]
		return executeCommand(s, w, ttl, cmd, cmdArgs)
	}

	// === 6. Сервер ===
	srv := server.NewServer(":6380", handler)
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

func executeCommand(s *store.ArenaStore, w *wal.WAL, ttl *store.TTLManager, cmd string, args []protocol.Value) protocol.Value {
	switch cmd {
	case "PING":
		return protocol.Value{Typ: '+', Str: "PONG"}

	case "SET":
		// SET key value [EX seconds]
		if len(args) < 2 {
			return protocol.Value{Typ: '-', Str: "ERR wrong number of arguments for 'SET'"}
		}
		key := args[0].Str
		value := []byte(args[1].Str)

		w.Write(wal.Entry{Op: wal.OpSet, Key: key, Value: value})
		s.Set(key, value)

		// Проверяем опцию EX (SET key value EX 60)
		if len(args) >= 4 && strings.ToUpper(args[2].Str) == "EX" {
			seconds, err := strconv.Atoi(args[3].Str)
			if err != nil || seconds <= 0 {
				return protocol.Value{Typ: '-', Str: "ERR invalid expire time"}
			}
			ttl.Set(key, time.Duration(seconds)*time.Second)
		}

		return protocol.Value{Typ: '+', Str: "OK"}

	case "GET":
		if len(args) < 1 {
			return protocol.Value{Typ: '-', Str: "ERR wrong number of arguments for 'GET'"}
		}
		key := args[0].Str

		// Lazy expiration: если просрочен — вернём NULL
		if ttl.IsExpired(key) {
			return protocol.Value{Typ: '$', Num: -1}
		}

		val, ok := s.Get(key)
		if !ok {
			return protocol.Value{Typ: '$', Num: -1}
		}
		return protocol.Value{Typ: '$', Str: string(val)}

	case "DEL":
		if len(args) < 1 {
			return protocol.Value{Typ: '-', Str: "ERR wrong number of arguments for 'DEL'"}
		}
		key := args[0].Str
		w.Write(wal.Entry{Op: wal.OpDel, Key: key})
		ok := s.Del(key)
		ttl.OnDelete(key) // убираем TTL для удалённого ключа
		if ok {
			return protocol.Value{Typ: ':', Num: 1}
		}
		return protocol.Value{Typ: ':', Num: 0}

	case "EXPIRE":
		// EXPIRE key seconds
		if len(args) < 2 {
			return protocol.Value{Typ: '-', Str: "ERR wrong number of arguments for 'EXPIRE'"}
		}
		key := args[0].Str

		// Проверяем что ключ существует
		if _, ok := s.Get(key); !ok {
			return protocol.Value{Typ: ':', Num: 0}
		}

		seconds, err := strconv.Atoi(args[1].Str)
		if err != nil || seconds <= 0 {
			return protocol.Value{Typ: '-', Str: "ERR invalid expire time"}
		}

		ttl.Set(key, time.Duration(seconds)*time.Second)
		return protocol.Value{Typ: ':', Num: 1}

	case "TTL":
		// TTL key — возвращает оставшееся время в секундах
		if len(args) < 1 {
			return protocol.Value{Typ: '-', Str: "ERR wrong number of arguments for 'TTL'"}
		}
		key := args[0].Str

		// Проверяем что ключ существует
		if _, ok := s.Get(key); !ok {
			return protocol.Value{Typ: ':', Num: -2} // ключ не существует
		}

		remaining := ttl.TTL(key)
		if remaining == -1 {
			return protocol.Value{Typ: ':', Num: -1} // ключ без TTL
		}

		return protocol.Value{Typ: ':', Num: int(remaining.Seconds())}

	case "PERSIST":
		// PERSIST key — убрать TTL
		if len(args) < 1 {
			return protocol.Value{Typ: '-', Str: "ERR wrong number of arguments for 'PERSIST'"}
		}
		if ttl.Remove(args[0].Str) {
			return protocol.Value{Typ: ':', Num: 1}
		}
		return protocol.Value{Typ: ':', Num: 0}

	case "DBSIZE":
		return protocol.Value{Typ: ':', Num: s.Len()}

	case "COMPACT":
		wal.BackgroundCompact(w, dataDir, s.ForEach)
		return protocol.Value{Typ: '+', Str: "OK compaction started"}

	default:
		return protocol.Value{Typ: '-', Str: fmt.Sprintf("ERR unknown command '%s'", cmd)}
	}
}
