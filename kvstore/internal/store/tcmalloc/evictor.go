package tcmalloc

// TCMallocEvictor — адаптер для интерфейса store.Evictor.
//
// TTLManager требует интерфейс:
//   Del(key string) bool
//
// TCMallocStore имеет:
//   Del(workerID int, key string) bool
//
// TCMallocEvictor оборачивает TCMallocStore и вызывает Del(0, key).
// Worker 0 используется потому что TTL eviction — холодный путь:
//   - Вызывается из фоновой goroutine (activeExpiry) каждые 100ms
//   - Обрабатывает ~20 ключей за цикл (sampleSize)
//   - Contention на worker 0 = ноль (TTL не конкурирует с hot path)
type TCMallocEvictor struct {
	store *TCMallocStore
}

// NewEvictor создаёт адаптер для TTLManager.
func NewEvictor(s *TCMallocStore) *TCMallocEvictor {
	return &TCMallocEvictor{store: s}
}

// Del реализует интерфейс store.Evictor.
func (e *TCMallocEvictor) Del(key string) bool {
	return e.store.Del(0, key)
}
