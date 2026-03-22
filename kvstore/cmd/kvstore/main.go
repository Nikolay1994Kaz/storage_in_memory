package main

import (
	"fmt"
	"log"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"kvstore/kvstore/internal/protocol"
	"kvstore/kvstore/internal/server"
	"kvstore/kvstore/internal/store"
	"kvstore/kvstore/internal/wal"
)

const (
	walPath      = "data.wal"             // путь к WAL-файлу
	syncInterval = 100 * time.Millisecond // интервал fsync
)

func main() {
	s := store.NewArenaStore()

	// === 1. Восстановление из WAL ===
	// При старте читаем WAL и воспроизводим все операции в store
	entries, err := wal.ReadAll(walPath)
	if err != nil {
		log.Fatalf("Failed to read WAL: %v", err)
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

	// === 2. Открываем WAL для записи ===
	w, err := wal.Open(walPath)
	if err != nil {
		log.Fatalf("Failed to open WAL: %v", err)
	}
	defer w.Close()

	// === 3. Запускаем фоновую синхронизацию ===
	syncer := wal.NewSyncer(w, syncInterval)
	defer syncer.Stop()

	// === 4. Handler с WAL ===
	handler := func(args []protocol.Value) protocol.Value {
		cmd := strings.ToUpper(args[0].Str)
		cmdArgs := args[1:]
		return executeCommand(s, w, cmd, cmdArgs)
	}

	// === 5. Запуск сервера ===
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
	// defer'ы закроют syncer и WAL
}

func executeCommand(s *store.ArenaStore, w *wal.WAL, cmd string, args []protocol.Value) protocol.Value {
	switch cmd {
	case "PING":
		return protocol.Value{Typ: '+', Str: "PONG"}

	case "SET":
		if len(args) < 2 {
			return protocol.Value{Typ: '-', Str: "ERR wrong number of arguments for 'SET'"}
		}
		key := args[0].Str
		value := []byte(args[1].Str)

		// Сначала WAL, потом память!
		// Если crash между WAL и Set — при восстановлении операция повторится.
		// Если crash до WAL — операция потеряна, но клиент не получил OK, значит это ожидаемо.
		w.Write(wal.Entry{Op: wal.OpSet, Key: key, Value: value})
		s.Set(key, value)
		return protocol.Value{Typ: '+', Str: "OK"}

	case "GET":
		if len(args) < 1 {
			return protocol.Value{Typ: '-', Str: "ERR wrong number of arguments for 'GET'"}
		}
		val, ok := s.Get(args[0].Str)
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
		if ok {
			return protocol.Value{Typ: ':', Num: 1}
		}
		return protocol.Value{Typ: ':', Num: 0}

	case "DBSIZE":
		return protocol.Value{Typ: ':', Num: s.Len()}

	case "CONFIG":
		// redis-benchmark отправляет CONFIG GET save / CONFIG GET appendonly при старте.
		// Возвращаем пустой массив — "нет таких параметров".
		return protocol.Value{Typ: '*', Array: []protocol.Value{}}

	case "COMMAND":
		// redis-benchmark может отправлять COMMAND при старте.
		return protocol.Value{Typ: '+', Str: "OK"}

	default:
		return protocol.Value{Typ: '-', Str: fmt.Sprintf("ERR unknown command '%s'", cmd)}
	}
}
