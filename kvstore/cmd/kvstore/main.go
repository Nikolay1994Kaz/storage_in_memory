package main

import (
	"bufio"
	"context"
	"crypto/tls"
	"encoding/binary"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"kvstore/kvstore/internal/ai"
	"kvstore/kvstore/internal/cluster"
	"kvstore/kvstore/internal/compute"
	"kvstore/kvstore/internal/protocol"
	"kvstore/kvstore/internal/pubsub"
	"kvstore/kvstore/internal/server"
	"kvstore/kvstore/internal/store"
	"kvstore/kvstore/internal/store/tcmalloc"
	"kvstore/kvstore/internal/wal"
	"kvstore/kvstore/vector"
)

const (
	dataDir      = "data"
	syncInterval = 100 * time.Millisecond
)

var globalTxMu sync.Mutex

func main() {
	// CLI-флаги
	port := flag.Int("port", 6380, "порт для клиентов")
	maxMemoryMB := flag.Int("maxmemory", 0, "лимит памяти в МБ (0 = без лимита)")
	clusterEnabled := flag.Bool("cluster", false, "включить кластерный режим")
	clusterSlotStart := flag.Int("slot-start", 0, "начало диапазона слотов")
	clusterSlotEnd := flag.Int("slot-end", 16383, "конец диапазона слотов")
	ollamaURL := flag.String("ollama-url", "http://localhost:11434", "URL Ollama API")
	requirePass := flag.String("requirepass", "", "пароль для AUTH (пусто = без аутентификации)")
	tlsCert := flag.String("tls-cert", "", "путь к TLS сертификату (PEM)")
	tlsKey := flag.String("tls-key", "", "путь к TLS ключу (PEM)")
	flag.Parse()

	// TCMallocStore: per-worker MCache (lock-free alloc) + lock-free HashTable (GET)
	s := tcmalloc.NewTCMallocStore(runtime.NumCPU())

	// Лимит памяти
	if *maxMemoryMB > 0 {
		s.SetMaxMemory(int64(*maxMemoryMB) * 1024 * 1024)
		log.Printf("Max memory: %d MB", *maxMemoryMB)
	}

	os.MkdirAll(dataDir, 0755)

	// === 1. TTL Manager ===
	ttl := store.NewTTLManager(tcmalloc.NewEvictor(s))
	defer ttl.Stop()

	// === 2. Инициализация хранилища векторов и загрузка бинарного снапшота ===
	vecStore := vector.NewVectorStore(vector.EuclideanDistance)
	graphPath := filepath.Join(dataDir, "graph.bin")
	graphLoaded := false

	if _, err := os.Stat(graphPath); err == nil {
		log.Printf("Loading HNSW graph from binary snapshot %s...", graphPath)
		f, err := os.Open(graphPath)
		if err == nil {
			if err := vecStore.LoadBinary(f); err != nil {
				log.Printf("WARNING: failed to load HNSW graph from binary snapshot: %v. Will rebuild from WAL.", err)
			} else {
				log.Printf("HNSW graph loaded successfully from binary snapshot!")
				graphLoaded = true
			}
			f.Close()
		}
	}

	// === 3. Восстановление состояния из WAL ===
	restored := 0
	vecRestored := 0

	// Шаг A: Сначала читаем и накатываем snapshot.wal (если есть)
	snapshotPath := filepath.Join(dataDir, "snapshot.wal")
	snapshotEntries, err := wal.ReadEntries(snapshotPath)
	if err != nil {
		log.Fatalf("Failed to read snapshot.wal: %v", err)
	}

	applyEntry := func(entry wal.Entry, isFromSnapshot bool) {
		switch entry.Op {
		case wal.OpSet:
			s.Set(0, entry.Key, entry.Value)
			restored++
		case wal.OpDel:
			s.Del(0, entry.Key)
			ttl.OnDelete(entry.Key)
			restored++
		case wal.OpExpire:
			if len(entry.Value) == 8 {
				expiresAt := time.Unix(0, int64(binary.BigEndian.Uint64(entry.Value)))
				remaining := time.Until(expiresAt)
				if remaining > 0 {
					ttl.Set(entry.Key, remaining)
				} else {
					s.Del(0, entry.Key)
					ttl.OnDelete(entry.Key)
				}
			}
			restored++
		case wal.OpPersist:
			ttl.Remove(entry.Key)
			restored++
		case wal.OpVSimAdd:
			// Если граф загружен из бинарного снапшота, мы пропускаем векторные операции из snapshot.wal
			if isFromSnapshot && graphLoaded {
				return
			}
			vec := vector.DeserializeVector(entry.Value)
			if err := vecStore.Add(entry.Key, vec); err != nil {
				log.Printf("WARNING: failed to restore vector %s: %v", entry.Key, err)
			}
			vecRestored++
			restored++
		case wal.OpVSimDel:
			if isFromSnapshot && graphLoaded {
				return
			}
			vecStore.Delete(entry.Key)
			restored++
		}
	}

	for _, entry := range snapshotEntries {
		applyEntry(entry, true)
	}

	// Шаг B: Потом читаем и накатываем все wal_*.log файлы в правильном порядке
	matches, _ := filepath.Glob(filepath.Join(dataDir, "wal_*.log"))
	sort.Strings(matches) // Сортируем по имени (по времени создания)

	for _, path := range matches {
		logEntries, err := wal.ReadEntries(path)
		if err != nil {
			log.Fatalf("Failed to read WAL log %s: %v", path, err)
		}
		for _, entry := range logEntries {
			applyEntry(entry, false)
		}
	}

	if restored > 0 {
		log.Printf("Restored %d operations from WAL (%d vectors)", restored, vecRestored)
	}

	// === 3. WAL ===
	walPath := filepath.Join(dataDir, fmt.Sprintf("wal_%s.log", time.Now().Format("20060102_150405")))
	rawWAL, err := wal.Open(walPath)
	if err != nil {
		log.Fatalf("Failed to open WAL: %v", err)
	}

	bw := wal.NewBatchWAL(rawWAL)
	defer bw.Close()

	// === 4. Syncer ===
	// iterateAll — обход только KV данных для snapshot.wal (векторы хранятся отдельно)
	iterateAll := func(fn func(op byte, key string, value []byte)) {
		s.ForEach(func(key string, value []byte) {
			fn(wal.OpSet, key, value)
		})
	}

	// saveVectors — сохраняет HNSW граф в graph.bin
	saveVectors := func() error {
		graphPath := filepath.Join(dataDir, "graph.bin")
		tmpPath := graphPath + ".tmp"
		f, err := os.Create(tmpPath)
		if err != nil {
			return err
		}
		writer := bufio.NewWriterSize(f, 256*1024)
		if err := vecStore.SaveBinary(writer); err != nil {
			f.Close()
			os.Remove(tmpPath)
			return err
		}
		if err := writer.Flush(); err != nil {
			f.Close()
			os.Remove(tmpPath)
			return err
		}
		if err := f.Sync(); err != nil {
			f.Close()
			os.Remove(tmpPath)
			return err
		}
		f.Close()
		return os.Rename(tmpPath, graphPath)
	}

	syncer := wal.NewSyncer(rawWAL, syncInterval, dataDir, iterateAll, saveVectors)
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
			s.Del(0, key)
			ttl.OnDelete(key)
		}

		cl.Repl.StoreForEach = func(fn func(key string, value []byte)) {
			s.ForEach(fn)
		}
		cl.Repl.VecStoreAdd = func(key string, vec []float32) {
			vecStore.Add(key, vec)
		}
		cl.Repl.VecStoreDel = func(key string) {
			vecStore.Delete(key)
		}
		cl.Repl.VecStoreForEach = func(fn func(key string, vec []float32)) {
			vecStore.ForEach(fn)
		}
		cl.Repl.StoreSet = func(key string, value []byte) {
			s.Set(0, key, value)
		}
		cl.Repl.StoreDel = func(key string) {
			s.Del(0, key)
		}
		cl.Repl.StoreClear = func() {
			s.Clear()
		}
		cl.Repl.VecStoreClear = func() {
			vecStore.Clear()
		}

		if err := cl.StartGossip(); err != nil {
			log.Fatalf("Failed to start gossip: %v", err)
		}
		defer cl.StopGossip()
	}

	// === 7. WASM Compute Engine ===
	wasm := compute.NewEngine()
	defer wasm.Close()

	wasm.GlobalLock = func() { globalTxMu.Lock() }
	wasm.GlobalUnlock = func() { globalTxMu.Unlock() }

	wasm.StoreGet = func(key string) ([]byte, bool) {
		return s.Get(key)
	}
	wasm.StoreSet = func(key string, value []byte) {
		s.Set(0, key, value)
	}
	wasm.StoreDel = func(key string) {
		s.Del(0, key)
	}
	wasm.Publish = func(channel, message string) {
		hub.Publish(channel, message)
	}
	wasm.VSimSearch = func(workerID int, query []float32, K int) []struct {
		Key      string
		Distance float32
	} {
		results, err := vecStore.Search(query, K)
		if err != nil {
			return nil
		}
		out := make([]struct {
			Key      string
			Distance float32
		}, len(results))
		for i, r := range results {
			out[i].Key = r.Key
			out[i].Distance = r.Distance
		}
		return out
	}

	wasm.StoreSetWithWAL = func(key string, value []byte) error {
		bw.Write(wal.Entry{Op: wal.OpSet, Key: key, Value: value})
		s.Set(0, key, value)
		return nil
	}
	wasm.StoreDelWithWAL = func(key string) error {
		bw.Write(wal.Entry{Op: wal.OpDel, Key: key})
		s.Del(0, key)
		ttl.OnDelete(key)
		return nil
	}

	// Загрузка WASM модулей с диска
	triggers := compute.NewTriggerManager(wasm)
	compute.LoadAll(dataDir, wasm, triggers)



	// === 8. AI Engine (Ollama) ===
	var aiClient *ai.Client
	var aiWorker *ai.Worker

	aiClient = ai.NewClient(*ollamaURL, "nomic-embed-text", "gemma4:e2b")
	if err := aiClient.Ping(context.Background()); err != nil {
		log.Printf("WARNING: Ollama not available (%v), AI commands disabled", err)
		aiClient = nil
	} else {
		log.Println("Ollama connected: nomic-embed-text + gemma4:e2b")

		// Подключаем AI к WASM Engine — WASM-модули получают доступ к Ollama
		wasm.AIEmbed = func(ctx context.Context, text string) ([]float32, error) {
			return aiClient.Embed(ctx, text)
		}
		wasm.AIChat = func(ctx context.Context, prompt string) (string, error) {
			return aiClient.Chat(ctx, prompt)
		}

		// Background Worker: асинхронный embedding с PubSub-нотификациями
		aiWorker = ai.NewWorker(aiClient, 256)
		aiWorker.VecStoreAdd = func(key string, vec []float32) error {
			// WAL: без этого AI-проиндексированные векторы терялись при рестарте.
			// Записываем сериализованный вектор — при восстановлении OpVSimAdd
			// вставит его обратно в HNSW без повторного вызова Ollama.
			walValue := vector.SerializeVector(vec)
			bw.Write(wal.Entry{Op: wal.OpVSimAdd, Key: key, Value: walValue})
			return vecStore.Add(key, vec)
		}
		aiWorker.KVStoreSet = func(key string, value []byte) {
			s.Set(0, key, value)
		}
		aiWorker.Publish = func(channel, message string) {
			hub.Publish(channel, message)
		}
		aiWorker.Start(2) // 2 горутины (Ollama сама батчит)
		defer aiWorker.Stop()
	}

	// ═══════════════════════════════════════════════════
	// HANDLER — zero-alloc: args = [][]byte из ring buffer
	// ═══════════════════════════════════════════════════

	handler := func(cs *server.ConnState, args [][]byte) {
		if len(args) == 0 {
			cs.Buf.WriteError("ERR empty command")
			return
		}

		cmd := strings.ToUpper(string(args[0]))
		cmdArgs := args[1:]

		// ─── AUTH ───────────────────────────────────────────
		// Если --requirepass задан, клиент должен пройти AUTH
		// перед выполнением любых команд (кроме AUTH и PING).
		if *requirePass != "" {
			if cmd == "AUTH" {
				if len(cmdArgs) != 1 {
					cs.Buf.WriteError("ERR wrong number of arguments for 'AUTH' command")
					return
				}
				if string(cmdArgs[0]) == *requirePass {
					cs.Authenticated = true
					cs.Buf.WriteSimpleString("OK")
				} else {
					cs.Buf.WriteError("ERR invalid password")
				}
				return
			}
			if !cs.Authenticated && cmd != "PING" {
				cs.Buf.WriteError("NOAUTH Authentication required")
				return
			}
		}

		// Транзакции
		switch cmd {
		case "MULTI":
			cs.InTx = true
			cs.Buf.WriteSimpleString("OK")
			return
		case "DISCARD":
			if !cs.InTx {
				cs.Buf.WriteError("ERR DISCARD without MULTI")
				return
			}
			cs.InTx = false
			cs.TxQueue = nil
			cs.Buf.WriteSimpleString("OK")
			return
		case "EXEC":
			if !cs.InTx {
				cs.Buf.WriteError("ERR EXEC without MULTI")
				return
			}
			globalTxMu.Lock()
			cs.Buf.WriteArrayHeader(len(cs.TxQueue))
			for _, queuedArgs := range cs.TxQueue {
				qCmd := strings.ToUpper(string(queuedArgs[0]))
				qCmdArgs := queuedArgs[1:]
				executeCommand(s, bw, ttl, hub, cl, wasm, triggers, vecStore, aiClient, aiWorker, iterateAll, saveVectors, cs, qCmd, qCmdArgs)
			}
			globalTxMu.Unlock()
			cs.InTx = false
			cs.TxQueue = nil
			return
		}

		if cs.InTx {
			// Копируем args — ring buffer будет перезаписан!
			argsCopy := make([][]byte, len(args))
			for i, a := range args {
				argsCopy[i] = append([]byte(nil), a...)
			}
			cs.TxQueue = append(cs.TxQueue, argsCopy)
			cs.Buf.WriteSimpleString("QUEUED")
			return
		}

		executeCommand(s, bw, ttl, hub, cl, wasm, triggers, vecStore, aiClient, aiWorker, iterateAll, saveVectors, cs, cmd, cmdArgs)
	}

	// === 8. Сервер ===
	listenAddr := fmt.Sprintf(":%d", *port)
	srv := server.NewServer(listenAddr, handler)

	// TLS: если указаны сертификат и ключ — включаем шифрование.
	if *tlsCert != "" && *tlsKey != "" {
		cert, err := tls.LoadX509KeyPair(*tlsCert, *tlsKey)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Failed to load TLS cert/key: %v\n", err)
			os.Exit(1)
		}
		srv.TLSConfig = &tls.Config{Certificates: []tls.Certificate{cert}}
	}

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

// arg — helper: безопасное получение string из args.
func arg(args [][]byte, i int) string {
	if i >= len(args) {
		return ""
	}
	return string(args[i])
}

func executeCommand(s *tcmalloc.TCMallocStore, bw *wal.BatchWAL, ttl *store.TTLManager,
	hub *pubsub.Hub, cl *cluster.Cluster, wasm *compute.Engine,
	triggers *compute.TriggerManager, vecStore *vector.VectorStore,
	aiClient *ai.Client, aiWorker *ai.Worker,
	iterateAll func(fn func(op byte, key string, value []byte)),
	saveVectors func() error,
	cs *server.ConnState, cmd string, args [][]byte) {

	buf := cs.Buf
	workerID := cs.WorkerID

	switch cmd {
	case "PING":
		buf.WriteSimpleString("PONG")

	// === Cluster ===
	case "CLUSTER":
		if cl != nil {
			// Конвертируем [][]byte → []protocol.Value для legacy cluster API
			pArgs := make([]protocol.Value, len(args))
			for i, a := range args {
				pArgs[i] = protocol.Value{Typ: '$', Str: string(a)}
			}
			writeValue(buf, cl.HandleClusterCommand(pArgs))
		} else {
			buf.WriteError("ERR cluster mode is not enabled")
		}

	case "MIGRATE":
		if cl == nil {
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
		writeValue(buf, cl.MigrateKey(host, port, key))

	case "PSYNC":
		if cl == nil {
			buf.WriteError("ERR cluster mode is not enabled")
			return
		}
		if len(args) < 1 {
			buf.WriteError("ERR wrong number of arguments for 'PSYNC'")
			return
		}
		replicaID := string(args[0])
		cl.Repl.HandlePsync(cs.Conn, replicaID)

	case "SET":
		if len(args) < 2 {
			buf.WriteError("ERR wrong number of arguments for 'SET'")
			return
		}
		key := string(args[0])
		if cl != nil {
			if moved := cl.CheckKey(key); moved != nil {
				writeValue(buf, *moved)
				return
			}
		}
		// Проверка лимита памяти (OOM protection)
		if s.IsOOM() {
			buf.WriteError("OOM command not allowed when used memory > 'maxmemory'")
			return
		}
		// args[1] — слайс ring buffer. Нужна копия для TCMalloc (буфер будет перезаписан).
		value := make([]byte, len(args[1]))
		copy(value, args[1])

		bw.Write(wal.Entry{Op: wal.OpSet, Key: key, Value: value})
		s.Set(workerID, key, value)

		if cl != nil && cl.Repl != nil {
			cl.Repl.ForwardWrite(fmt.Sprintf("SET %s %s", key, string(value)))
		}
		triggers.Fire(compute.OnSet, key, workerID)

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
			bw.Write(wal.Entry{Op: wal.OpExpire, Key: key, Value: b[:]})
			ttl.Set(key, dur)
		}

		buf.WriteSimpleString("OK")

	case "GET":
		if len(args) < 1 {
			buf.WriteError("ERR wrong number of arguments for 'GET'")
			return
		}
		key := string(args[0])
		if cl != nil && cl.State.Self.Role != cluster.RoleReplica {
			if moved := cl.CheckKey(key); moved != nil {
				writeValue(buf, *moved)
				return
			}
		}
		if ttl.IsExpired(key) {
			buf.WriteNull()
			return
		}
		val, ok := s.Get(key)
		if !ok {
			if cl != nil {
				if ask := cl.CheckKeyAsk(key); ask != nil {
					writeValue(buf, *ask)
					return
				}
			}
			buf.WriteNull()
			return
		}
		buf.WriteBulk(val)

	case "DEL":
		if len(args) < 1 {
			buf.WriteError("ERR wrong number of arguments for 'DEL'")
			return
		}
		key := string(args[0])
		if cl != nil {
			if moved := cl.CheckKey(key); moved != nil {
				writeValue(buf, *moved)
				return
			}
		}
		bw.Write(wal.Entry{Op: wal.OpDel, Key: key})
		ok := s.Del(workerID, key)
		ttl.OnDelete(key)
		if cl != nil && cl.Repl != nil {
			cl.Repl.ForwardWrite(fmt.Sprintf("DEL %s", key))
		}
		triggers.Fire(compute.OnDel, key, workerID)
		if ok {
			buf.WriteInt(1)
		} else {
			buf.WriteInt(0)
		}

	case "EXPIRE":
		if len(args) < 2 {
			buf.WriteError("ERR wrong number of arguments for 'EXPIRE'")
			return
		}
		key := string(args[0])
		if cl != nil {
			if moved := cl.CheckKey(key); moved != nil {
				writeValue(buf, *moved)
				return
			}
		}
		if _, ok := s.Get(key); !ok {
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
		bw.Write(wal.Entry{Op: wal.OpExpire, Key: key, Value: b[:]})
		ttl.Set(key, dur)
		buf.WriteInt(1)

	case "TTL":
		if len(args) < 1 {
			buf.WriteError("ERR wrong number of arguments for 'TTL'")
			return
		}
		key := string(args[0])
		if cl != nil {
			if moved := cl.CheckKey(key); moved != nil {
				writeValue(buf, *moved)
				return
			}
		}
		if _, ok := s.Get(key); !ok {
			buf.WriteInt(-2)
			return
		}
		remaining := ttl.TTL(key)
		if remaining == -1 {
			buf.WriteInt(-1)
			return
		}
		buf.WriteInt(int(remaining.Seconds()))

	case "PERSIST":
		if len(args) < 1 {
			buf.WriteError("ERR wrong number of arguments for 'PERSIST'")
			return
		}
		key := string(args[0])
		if cl != nil {
			if moved := cl.CheckKey(key); moved != nil {
				writeValue(buf, *moved)
				return
			}
		}
		if ttl.Remove(key) {
			bw.Write(wal.Entry{Op: wal.OpPersist, Key: key})
			buf.WriteInt(1)
		} else {
			buf.WriteInt(0)
		}

	// === Pub/Sub ===
	case "SUBSCRIBE":
		if len(args) < 1 {
			buf.WriteError("ERR wrong number of arguments for 'SUBSCRIBE'")
			return
		}
		channels := make([]string, len(args))
		for i, a := range args {
			channels[i] = string(a)
		}
		hub.Subscribe(cs.Conn, channels)
		// writePump отправляет подтверждения, не пишем в buf

	case "UNSUBSCRIBE":
		channels := make([]string, len(args))
		for i, a := range args {
			channels[i] = string(a)
		}
		hub.Unsubscribe(cs.Conn, channels)
		buf.WriteSimpleString("OK")

	case "PUBLISH":
		if len(args) < 2 {
			buf.WriteError("ERR wrong number of arguments for 'PUBLISH'")
			return
		}
		count := hub.Publish(string(args[0]), string(args[1]))
		buf.WriteInt(count)

	case "DBSIZE":
		buf.WriteInt(s.Len())

	case "COMPACT":
		wal.BackgroundCompact(bw.RawWAL(), dataDir, iterateAll, saveVectors)
		buf.WriteSimpleString("OK compaction started")

	// === WASM ===
	case "WASM.LOAD":
		if len(args) < 2 {
			buf.WriteError("ERR wrong number of arguments for 'WASM.LOAD'")
			return
		}
		name := string(args[0])
		wasmBytes := make([]byte, len(args[1]))
		copy(wasmBytes, args[1])
		if err := wasm.LoadModule(name, wasmBytes); err != nil {
			buf.WriteError(fmt.Sprintf("ERR %v", err))
			return
		}
		compute.SaveModule(dataDir, name, wasmBytes)
		buf.WriteSimpleString("OK")

	case "WASM.LOADFILE":
		if len(args) < 2 {
			buf.WriteError("ERR wrong number of arguments for 'WASM.LOADFILE'")
			return
		}
		name := string(args[0])
		filePath := string(args[1])
		wasmBytes, err := os.ReadFile(filePath)
		if err != nil {
			buf.WriteError(fmt.Sprintf("ERR cannot read file: %v", err))
			return
		}
		if err := wasm.LoadModule(name, wasmBytes); err != nil {
			buf.WriteError(fmt.Sprintf("ERR %v", err))
			return
		}
		compute.SaveModule(dataDir, name, wasmBytes)
		buf.WriteSimpleString(fmt.Sprintf("OK loaded %d bytes", len(wasmBytes)))

	case "WASM.DROP":
		if len(args) < 1 {
			buf.WriteError("ERR wrong number of arguments for 'WASM.DROP'")
			return
		}
		name := string(args[0])
		if err := wasm.DropModule(name); err != nil {
			buf.WriteError(fmt.Sprintf("ERR %v", err))
			return
		}
		compute.DeleteModule(dataDir, name)
		buf.WriteSimpleString("OK")

	case "WASM.LIST":
		names := wasm.ListModules()
		buf.WriteArrayHeader(len(names))
		for _, n := range names {
			buf.WriteBulkString(n)
		}

	case "WASM.EXEC":
		if len(args) < 2 {
			buf.WriteError("ERR wrong number of arguments for 'WASM.EXEC'")
			return
		}
		results, err := wasm.ExecFunction(string(args[0]), string(args[1]))
		if err != nil {
			buf.WriteError(fmt.Sprintf("ERR %v", err))
			return
		}
		if len(results) > 0 {
			buf.WriteInt(int(results[0]))
		} else {
			buf.WriteSimpleString("OK")
		}

	case "WASM.INFO":
		if len(args) < 1 {
			buf.WriteError("ERR wrong number of arguments for 'WASM.INFO'")
			return
		}
		name := string(args[0])
		loadedAt, execCount, found := wasm.ModuleInfo(name)
		if !found {
			buf.WriteError(fmt.Sprintf("ERR module '%s' not found", name))
			return
		}
		info := fmt.Sprintf("module:%s loaded_at:%s exec_count:%d",
			name, loadedAt.Format(time.RFC3339), execCount)
		buf.WriteBulkString(info)

	case "WASM.TRIGGER":
		if len(args) < 4 {
			buf.WriteError("ERR usage: WASM.TRIGGER <SET|DEL> <pattern> <module> <func>")
			return
		}
		event := compute.TriggerEvent(strings.ToUpper(string(args[0])))
		pattern := string(args[1])
		moduleName := string(args[2])
		funcName := string(args[3])
		id := triggers.AddTrigger(event, pattern, moduleName, funcName)
		compute.SaveTriggers(dataDir, triggers)
		buf.WriteSimpleString(id)

	case "WASM.UNTRIGGER":
		if len(args) < 1 {
			buf.WriteError("ERR wrong number of arguments for 'WASM.UNTRIGGER'")
			return
		}
		if triggers.RemoveTrigger(string(args[0])) {
			compute.SaveTriggers(dataDir, triggers)
			buf.WriteSimpleString("OK")
		} else {
			buf.WriteError("ERR trigger not found")
		}

	case "WASM.TRIGGERS":
		all := triggers.ListTriggers()
		buf.WriteArrayHeader(len(all))
		for _, t := range all {
			buf.WriteBulkString(fmt.Sprintf("%s %s %s %s.%s", t.ID, t.Event, t.Pattern, t.ModuleName, t.FuncName))
		}

	// === Vector Search ===
	case "VSIM.ADD":
		if len(args) < 2 {
			buf.WriteError("ERR usage: VSIM.ADD <key> <v1> <v2> ... <vN>")
			return
		}
		key := string(args[0])
		vec := make([]float32, len(args)-1)
		for i := 1; i < len(args); i++ {
			f, err := strconv.ParseFloat(string(args[i]), 32)
			if err != nil {
				buf.WriteError(fmt.Sprintf("ERR invalid float at position %d: %s", i, string(args[i])))
				return
			}
			vec[i-1] = float32(f)
		}
		walValue := vector.SerializeVector(vec)
		bw.Write(wal.Entry{Op: wal.OpVSimAdd, Key: key, Value: walValue})
		if err := vecStore.Add(key, vec); err != nil {
			buf.WriteError(fmt.Sprintf("ERR %v", err))
			return
		}
		if cl != nil && cl.Repl != nil {
			// Формат: VSIM.ADD key 0.1 0.2 0.3 ...
			var sb strings.Builder
			sb.WriteString("VSIM.ADD ")
			sb.WriteString(key)
			for _, v := range vec {
				sb.WriteByte(' ')
				sb.WriteString(strconv.FormatFloat(float64(v), 'f', -1, 32))
			}
			cl.Repl.ForwardWrite(sb.String())
		}
		buf.WriteSimpleString("OK")

	case "VSIM.DEL":
		if len(args) < 1 {
			buf.WriteError("ERR usage: VSIM.DEL <key>")
			return
		}
		key := string(args[0])
		if vecStore.Delete(key) {
			bw.Write(wal.Entry{Op: wal.OpVSimDel, Key: key})
			if cl != nil && cl.Repl != nil {
				cl.Repl.ForwardWrite("VSIM.DEL " + key)
			}
			buf.WriteInt(1)
		} else {
			buf.WriteInt(0)
		}

	case "VSIM.SEARCH":
		if len(args) < 2 {
			buf.WriteError("ERR usage: VSIM.SEARCH <K> <v1> <v2> ... <vN>")
			return
		}
		K, err := strconv.Atoi(string(args[0]))
		if err != nil || K <= 0 {
			buf.WriteError("ERR invalid K (must be positive integer)")
			return
		}
		query := make([]float32, len(args)-1)
		for i := 1; i < len(args); i++ {
			f, err := strconv.ParseFloat(string(args[i]), 32)
			if err != nil {
				buf.WriteError(fmt.Sprintf("ERR invalid float at position %d: %s", i, string(args[i])))
				return
			}
			query[i-1] = float32(f)
		}
		results, err := vecStore.Search(query, K)
		if err != nil {
			buf.WriteError(fmt.Sprintf("ERR %v", err))
			return
		}
		buf.WriteArrayHeader(len(results) * 2)
		for _, r := range results {
			buf.WriteBulkString(r.Key)
			buf.WriteBulkString(fmt.Sprintf("%.6f", r.Distance))
		}

	case "VSIM.INFO":
		count, dim, maxLevel := vecStore.Info()
		info := fmt.Sprintf("vectors:%d dimension:%d max_level:%d", count, dim, maxLevel)
		buf.WriteBulkString(info)


	// === AI Commands ===
	case "AI.EMBED":
		if aiClient == nil {
			buf.WriteError("ERR Ollama not available")
			return
		}
		if len(args) < 1 {
			buf.WriteError("ERR usage: AI.EMBED <text>")
			return
		}
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		embedding, err := aiClient.Embed(ctx, string(args[0]))
		if err != nil {
			buf.WriteError(fmt.Sprintf("ERR %v", err))
			return
		}
		buf.WriteArrayHeader(len(embedding))
		for _, v := range embedding {
			buf.WriteBulkString(fmt.Sprintf("%.6f", v))
		}

	case "AI.SEARCH":
		if aiClient == nil {
			buf.WriteError("ERR Ollama not available")
			return
		}
		if len(args) < 2 {
			buf.WriteError("ERR usage: AI.SEARCH <K> <text>")
			return
		}
		K, err := strconv.Atoi(string(args[0]))
		if err != nil || K <= 0 {
			buf.WriteError("ERR invalid K")
			return
		}
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		embedding, err := aiClient.Embed(ctx, string(args[1]))
		if err != nil {
			buf.WriteError(fmt.Sprintf("ERR embed: %v", err))
			return
		}
		results, err := vecStore.Search(embedding, K)
		if err != nil {
			buf.WriteError(fmt.Sprintf("ERR search: %v", err))
			return
		}
		buf.WriteArrayHeader(len(results) * 2)
		for _, r := range results {
			buf.WriteBulkString(r.Key)
			buf.WriteBulkString(fmt.Sprintf("%.6f", r.Distance))
		}

	case "AI.ASK":
		if aiClient == nil {
			buf.WriteError("ERR Ollama not available")
			return
		}
		if len(args) < 1 {
			buf.WriteError("ERR usage: AI.ASK <question>")
			return
		}
		question := string(args[0])
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()

		// RAG Step 1: вопрос → embedding
		embedding, err := aiClient.Embed(ctx, question)
		if err != nil {
			buf.WriteError(fmt.Sprintf("ERR embed: %v", err))
			return
		}

		// RAG Step 2: поиск похожих документов
		results, err := vecStore.Search(embedding, 3)
		if err != nil {
			buf.WriteError(fmt.Sprintf("ERR search: %v", err))
			return
		}

		// RAG Step 3: достаём оригинальные тексты из KV Store
		var contextParts []string
		for _, r := range results {
			if val, ok := s.Get(r.Key); ok {
				contextParts = append(contextParts, fmt.Sprintf("[%s]: %s", r.Key, string(val)))
			}
		}
		if len(contextParts) == 0 {
			buf.WriteError("ERR no documents found for context")
			return
		}

		// RAG Step 4: собираем промпт и отправляем в LLM
		prompt := fmt.Sprintf("Контекст:\n%s\n\nВопрос: %s\n\nОтветь кратко на основе контекста выше.",
			strings.Join(contextParts, "\n"), question)

		answer, err := aiClient.Chat(ctx, prompt)
		if err != nil {
			buf.WriteError(fmt.Sprintf("ERR chat: %v", err))
			return
		}
		buf.WriteBulkString(answer)

	case "AI.INGEST":
		if aiWorker == nil {
			buf.WriteError("ERR Ollama not available")
			return
		}
		if len(args) < 2 {
			buf.WriteError("ERR usage: AI.INGEST <key> <text>")
			return
		}
		if err := aiWorker.Submit(string(args[0]), string(args[1])); err != nil {
			buf.WriteError(fmt.Sprintf("ERR %v", err))
			return
		}
		buf.WriteSimpleString("QUEUED")

	default:
		buf.WriteError(fmt.Sprintf("ERR unknown command '%s'", cmd))
	}
}
