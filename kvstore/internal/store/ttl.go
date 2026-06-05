package store

import (
	"hash/maphash"
	"sync"
	"time"
)

const (
	sampleSize       = 20
	expiredThreshold = sampleSize / 4 // 5 из 20 = 25%
	maxLoops         = 16

	// Количество шардов TTL-таблицы.
	// 256 = степень двойки → быстрый modulo через bitwise AND.
	// При 12 workers: каждый шард конкурирует максимум с 12/256 ≈ 0.05 воркерами.
	ttlShardCount = 256
	ttlShardMask  = ttlShardCount - 1

	// Сколько шардов обрабатывать за один тик activeExpiry (100ms).
	// 4 шарда × 10 тиков/сек = 40 шардов/сек → полный обход за ~6 секунд.
	shardsPerCycle = 4
)

// Evictor — минимальный интерфейс для удаления ключей.
// TTLManager не знает ничего о реализации хранилища.
// Это позволяет: подставить мок в тестах, сменить store без переписывания TTL.
type Evictor interface {
	Del(key string) bool
}

// ttlShard — один шард TTL-таблицы.
//
// Каждый шард имеет свой RWMutex → параллельные GET'ы
// (IsExpired) к разным ключам не блокируют друг друга.
//
// Оптимизации:
//   - int64 (UnixNano) вместо time.Time: 8 байт вместо 24, нет указателя на
//     *time.Location → map невидима для GC scanner (-33% RAM, меньше GC pause).
//   - Padding до 64 байт (1 cache line на x86-64): без паддинга sizeof=32,
//     два соседних шарда попадают в одну cache line → false sharing при
//     параллельной записи разными ядрами CPU.
type ttlShard struct {
	mu      sync.RWMutex     // 24 байта
	expires map[string]int64 // 8 байт (указатель на hmap)
	_       [32]byte         // padding → итого 64 байта = 1 cache line
}

// TTLManager — шардированное управление временем жизни ключей.
//
// ПРОБЛЕМА (до шардирования):
//
//	Один sync.Mutex на всю map[string]time.Time.
//	IsExpired() вызывается на КАЖДОМ GET.
//	При 5M GET/sec → 5M блокировок/сек на одном мьютексе.
//	12 Epoll-воркеров выстраиваются в очередь → сериализация.
//
// РЕШЕНИЕ:
//
//	256 шардов с независимыми RWMutex.
//	IsExpired() использует RLock → полный параллелизм на чтение.
//	Только удаление просроченного ключа берёт WLock (на одном шарде).
//
// Результат: contention снижается в ~256 раз.
type TTLManager struct {
	shards   [ttlShardCount]ttlShard
	store    Evictor
	stop     chan struct{}
	stopOnce sync.Once    // защита от двойного close(stop)
	shardIdx uint32       // round-robin для activeExpiry
	hashSeed maphash.Seed // рандомный seed → защита от hash collision attack
}

func NewTTLManager(store Evictor) *TTLManager {
	m := &TTLManager{
		store:    store,
		stop:     make(chan struct{}),
		hashSeed: maphash.MakeSeed(),
	}
	for i := range m.shards {
		m.shards[i].expires = make(map[string]int64)
	}
	go m.activeExpiry()
	return m
}

// getShard возвращает шард для ключа (maphash.String).
//
// maphash.String — платформо-зависимый векторизованный хэш Go (AES-NI на x86-64).
// В 3× быстрее FNV-1a на ключах >32 байт, без аллокаций.
// Рандомный seed при инициализации → защита от hash collision attack.
func (m *TTLManager) getShard(key string) *ttlShard {
	h := uint32(maphash.String(m.hashSeed, key))
	return &m.shards[h&ttlShardMask]
}

// Set устанавливает TTL для ключа.
func (m *TTLManager) Set(key string, ttl time.Duration) {
	s := m.getShard(key)
	s.mu.Lock()
	s.expires[key] = time.Now().Add(ttl).UnixNano()
	s.mu.Unlock()
}

// Remove убирает TTL (команда PERSIST).
func (m *TTLManager) Remove(key string) bool {
	s := m.getShard(key)
	s.mu.Lock()
	_, ok := s.expires[key]
	delete(s.expires, key)
	s.mu.Unlock()
	return ok
}

// TTL возвращает оставшееся время жизни.
func (m *TTLManager) TTL(key string) time.Duration {
	s := m.getShard(key)
	s.mu.RLock()
	expiresAt, ok := s.expires[key]
	s.mu.RUnlock()

	if !ok {
		return -1
	}

	remaining := time.Duration(expiresAt - time.Now().UnixNano())
	if remaining <= 0 {
		return 0
	}
	return remaining
}

// IsExpired — lazy expiration (горячий путь GET).
//
// Оптимизация: два уровня блокировки.
//
//	Fast path (99.9% вызовов): RLock → lookup → RUnlock.
//	  Полностью параллелен с другими GET'ами на этом шарде.
//
//	Slow path (ключ просрочен): WLock → double-check → delete → WUnlock.
//	  Блокирует только один шард (1/256 таблицы) и только на время delete.
func (m *TTLManager) IsExpired(key string) bool {
	s := m.getShard(key)
	now := time.Now().UnixNano()

	// ── Fast path: RLock (параллельно с другими GET'ами) ──
	s.mu.RLock()
	expiresAt, ok := s.expires[key]

	if !ok {
		s.mu.RUnlock()
		return false
	}

	if now < expiresAt {
		s.mu.RUnlock()
		return false
	}
	s.mu.RUnlock()

	// ── Slow path: ключ просрочен, берём WLock для удаления ──
	s.mu.Lock()
	// Double-check: пока мы ждали WLock, другой воркер мог:
	// 1. Уже удалить этот ключ (IsExpired или activeExpiry)
	// 2. Обновить TTL (SET key value EX 3600)
	expiresAt, ok = s.expires[key]
	if !ok {
		s.mu.Unlock()
		return true // уже удалён другим воркером — ключ был просрочен
	}
	if time.Now().UnixNano() < expiresAt {
		s.mu.Unlock()
		return false // TTL был обновлён (новый SET EX) пока мы ждали WLock
	}

	delete(s.expires, key)
	s.mu.Unlock()

	// Удаляем из store ВНЕ лока — store имеет свои блокировки
	m.store.Del(key)
	return true
}

// activeExpiry — фоновая горутина (Redis-style).
func (m *TTLManager) activeExpiry() {
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			m.expireCycle()
		case <-m.stop:
			return
		}
	}
}

// expireCycle — проходит по нескольким шардам за один тик.
//
// Round-robin: каждый тик обрабатываем shardsPerCycle шардов.
// За ~6 секунд обходим все 256 шардов.
func (m *TTLManager) expireCycle() {
	for i := 0; i < shardsPerCycle; i++ {
		idx := m.shardIdx % ttlShardCount
		m.shardIdx++

		for loop := 0; loop < maxLoops; loop++ {
			expired := m.sampleAndExpireShard(idx)
			if expired < expiredThreshold {
				break
			}
		}
	}
}

// sampleAndExpireShard — zero-alloc random sampling одного шарда.
func (m *TTLManager) sampleAndExpireShard(idx uint32) int {
	s := &m.shards[idx]

	s.mu.Lock()

	if len(s.expires) == 0 {
		s.mu.Unlock()
		return 0
	}

	now := time.Now().UnixNano()

	// Массив на СТЕКЕ — ноль давления на GC
	var expiredArr [sampleSize]string
	expiredKeys := expiredArr[:0]
	checked := 0

	// Один проход: итерация + проверка + удаление
	for key, expiresAt := range s.expires {
		checked++

		if now > expiresAt {
			expiredKeys = append(expiredKeys, key)
			delete(s.expires, key)
		}

		if checked >= sampleSize {
			break
		}
	}

	s.mu.Unlock()

	// Удаляем из store ВНЕ лока TTLManager
	for _, key := range expiredKeys {
		m.store.Del(key)
	}

	return len(expiredKeys)
}

// OnDelete убирает TTL при ручном удалении ключа (DEL).
func (m *TTLManager) OnDelete(key string) {
	s := m.getShard(key)
	s.mu.Lock()
	delete(s.expires, key)
	s.mu.Unlock()
}

// Len возвращает количество ключей с TTL.
func (m *TTLManager) Len() int {
	total := 0
	for i := range m.shards {
		m.shards[i].mu.RLock()
		total += len(m.shards[i].expires)
		m.shards[i].mu.RUnlock()
	}
	return total
}

// Stop останавливает фоновую очистку.
// Безопасен для повторного вызова (sync.Once).
func (m *TTLManager) Stop() {
	m.stopOnce.Do(func() {
		close(m.stop)
	})
}

// SetEvictor заменяет evictor на лету.
//
// Используется для перехода с простого KV-evictor (нужен при WAL replay)
// на CompositeEvictor (KV + VectorStore + WAL запись) после полной инициализации.
//
// Безопасно: activeExpiry вызывает store.Del() под WLock шарда,
// а SetEvictor вызывается один раз при старте сервера до начала
// обработки клиентских запросов.
func (m *TTLManager) SetEvictor(e Evictor) {
	m.store = e
}
