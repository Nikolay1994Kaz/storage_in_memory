package main

import (
	"context"
	"sync"

	"kvstore/kvstore/internal/pubsub"
	"kvstore/kvstore/internal/server"
	"kvstore/kvstore/internal/store"
	"kvstore/kvstore/internal/store/tcmalloc"
	"kvstore/kvstore/internal/wal"
	"kvstore/kvstore/vector"
)

// computeRuntime — узкий шов WASM compute-движка, которым пользуются hot-path
// команд и командный слой. Он же — шов сборки: реальная реализация (адаптер над
// *compute.Engine + TriggerManager) живёт в wasm_experimental.go под тегом
// experimental, а в обычной прод-сборке newComputeEngine (wasm_stub.go) возвращает
// no-op, и весь WASM/compute-код вместе с wazero-рантаймом НЕ линкуется в бинарь.
//
// Мотивация та же, что у clusterNode: полу-готовая, небезопасная подсистема
// (WASM.EXEC исполняет произвольный модуль — ACE-поверхность с незакрытыми
// Critical) не должна попадать в прод и включаться случайно. Включение — только
// осознанной пересборкой с -tags experimental.
type computeRuntime interface {
	// FireSet/FireDel — hot-path триггеры на запись/удаление ключа (no-op без WASM).
	FireSet(key string, workerID int)
	FireDel(key string, workerID int)
	// HandleCommand обслуживает все WASM.* команды, записывая ответ в buf.
	HandleCommand(cmd string, args [][]byte, buf *server.ConnBuf)
	// SetAI подключает Ollama-мостики для host-функций WASM (no-op без WASM/Ollama).
	SetAI(embed func(context.Context, string) ([]float32, error), chat func(context.Context, string) (string, error))
	// Close освобождает ресурсы движка.
	Close()
}

// computeDeps — сырые зависимости движка из main(). Замыкания-мостики строятся
// внутри реального адаптера (wasm_experimental.go), чтобы прод-стаб не тянул за
// собой типы пакета compute.
type computeDeps struct {
	store    *tcmalloc.TCMallocStore
	ttl      *store.TTLManager
	bw       *wal.BatchWAL
	hub      *pubsub.Hub
	vecStore vector.VectorIndex
	globalMu *sync.Mutex
}
