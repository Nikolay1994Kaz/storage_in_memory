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
// Значение map — МОНОТОННЫЕ наносекунды дедлайна (см. TTLManager.monoNow),
// а НЕ wall-clock UnixNano. Это делает истечение невосприимчивым к скачкам
// системных часов (NTP-коррекция, ручной перевод времени): дедлайн считается
// от монотонного старта менеджера, который не прыгает. Конвертация в
// абсолютный wall-clock происходит только на границе WAL (ForEach → OpExpire).
//
// Оптимизации:
//   - int64 вместо time.Time: 8 байт вместо 24, нет указателя на
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

	// Источники времени. Разделены на монотонный (истечение) и wall-clock
	// (граница WAL), чтобы истечение не зависело от скачков системных часов.
	// В проде — реальные часы; тесты подменяют оба, чтобы детерминированно
	// смоделировать NTP-скачок (см. ttl_monotonic_test.go).
	monoClock func() int64     // монотонные нс с момента старта (see monoNow)
	wallClock func() time.Time // абсолютное wall-clock время (для ForEach→WAL)
}

func NewTTLManager(store Evictor) *TTLManager {
	start := time.Now() // несёт монотонное показание → база для monoClock
	return newTTLManager(store, func() int64 { return int64(time.Since(start)) }, time.Now)
}

// newTTLManager — конструктор с инъекцией часов.
//
// Часы задаются ДО запуска activeExpiry: `go` устанавливает happens-before от
// создающей горутины к запущенной, поэтому фоновая горутина гарантированно
// видит переданные monoClock/wallClock без гонки (в отличие от подмены поля
// уже после старта). Прод зовёт NewTTLManager; тесты — этот вариант с fake-часами.
func newTTLManager(store Evictor, mono func() int64, wall func() time.Time) *TTLManager {
	m := &TTLManager{
		store:     store,
		stop:      make(chan struct{}),
		hashSeed:  maphash.MakeSeed(),
		monoClock: mono,
		wallClock: wall,
	}
	for i := range m.shards {
		m.shards[i].expires = make(map[string]int64)
	}
	go m.activeExpiry()
	return m
}

// monoNow — монотонные наносекунды с момента старта менеджера.
//
// time.Since использует МОНОТОННЫЕ часы (оба операнда несут монотонное
// показание) → результат невосприимчив к скачкам wall-clock: NTP-коррекция,
// ручной перевод времени или переход на зимнее/летнее не сдвигают дедлайны.
// До этого TTL хранился как time.Now().UnixNano() (wall-clock), и прыжок
// часов вперёд вызывал массовое ложное истечение, назад — «бессмертие» ключей.
//
// Монотонный эпох сбрасывается при рестарте процесса — поэтому в WAL
// (переживает рестарт) дедлайн пишется как абсолютный wall-clock (ForEach),
// а на реплее пересчитывается обратно в монотонный домен через Set(remaining).
func (m *TTLManager) monoNow() int64 {
	return m.monoClock()
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
	deadline := m.monoNow() + int64(ttl) // монотонный дедлайн (не wall-clock)
	s.mu.Lock()
	s.expires[key] = deadline
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

	remaining := time.Duration(expiresAt - m.monoNow())
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
	now := m.monoNow()

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
	if m.monoNow() < expiresAt {
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

	now := m.monoNow()

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

// ForEach отдаёт все ключи с их абсолютным временем смерти (wall-clock UnixNano).
//
// Для WAL-компакции (S1): TTL живёт ТОЛЬКО в реплее OpExpire. Компакция
// обязана переснять его в snapshot.wal — иначе после удаления старых WAL
// ключи становятся бессмертными (correctness + утечка памяти).
//
// КОНВЕРТАЦИЯ монотонный→wall: внутри дедлайны хранятся в монотонном домене
// (см. monoNow), а WAL переживает рестарт процесса, где монотонный эпох
// сбрасывается. Поэтому наружу отдаётся АБСОЛЮТНЫЙ wall-clock:
//
//	remaining     = deadline - monoNow()          (истинный остаток жизни)
//	wallDeadline  = time.Now().UnixNano() + remaining
//
// База (wallBase, monoBase) снимается двумя вызовами подряд → рассинхрон < мкс,
// для TTL несущественен. На реплее main.go делает обратное:
// time.Until(wallDeadline) → Set(remaining), перепривязывая дедлайн к
// монотонному эпоху нового процесса.
//
// Снимает содержимое шарда в локальный срез под RLock и вызывает fn ВНЕ
// замка: fn пишет в WAL (потенциально с flush на диск), а держать замок
// шарда во время I/O означало бы стопорить GET-hot-path на этом шарде.
func (m *TTLManager) ForEach(fn func(key string, expiresAtUnixNano int64)) {
	// Пара (wall, mono) снимается подряд → рассинхрон < мкс, для TTL неважен.
	wallBase := m.wallClock().UnixNano()
	monoBase := m.monoNow()

	type kv struct {
		key string
		exp int64
	}
	for i := range m.shards {
		s := &m.shards[i]
		s.mu.RLock()
		batch := make([]kv, 0, len(s.expires))
		for k, exp := range s.expires {
			batch = append(batch, kv{k, exp})
		}
		s.mu.RUnlock()
		for _, e := range batch {
			// монотонный дедлайн → абсолютный wall-clock
			wallDeadline := wallBase + (e.exp - monoBase)
			fn(e.key, wallDeadline)
		}
	}
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
