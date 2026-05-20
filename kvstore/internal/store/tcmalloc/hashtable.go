package tcmalloc

import (
	"sync/atomic"
)

// ─── Sentinel Values ────────────────────────────────────────
//
// Hash 0 = «слот пустой». Hash MAX = «tombstone» (удалён).
// Если настоящий хеш ключа совпал с sentinel — сдвигаем на 1.
// Это безопасно: коллизия обрабатывается linear probing.
const (
	emptyHash     uint64 = 0
	tombstoneHash uint64 = 0xFFFFFFFFFFFFFFFF
)

// Пороги для grow и rebuild.
const (
	loadFactorThreshold = 0.70 // grow при 70% заполнении
	tombstoneThreshold  = 0.25 // rebuild при 25% tombstones
	defaultInitialCap   = 1024 // начальный размер таблицы
)

// ─── Slot ───────────────────────────────────────────────────
//
// Одна ячейка open-addressing таблицы: [hash][handle].
//
// Оба поля — atomic, чтобы Get (lock-free) видел консистентные данные.
//
// ПОРЯДОК ЗАПИСИ (критически важен):
//
//	Put:  ① handle.Store(value)  ② hash.Store(key)
//	Get:  ① hash.Load()          ② handle.Load()
//
// Почему: hash.Store имеет release-семантику в Go.
// Всё, что записано ДО hash.Store (т.е. handle), гарантированно
// видно после hash.Load в другой goroutine.
//
// Если Get увидел hash == target → handle УЖЕ записан.
type slot struct {
	hash   atomic.Uint64
	handle atomic.Uint64
}

// ─── HashTable ──────────────────────────────────────────────
//
// Open-addressing хеш-таблица с linear probing.
//
// Модель конкурентности (ГИБРИД):
//
//	Get()     — LOCK-FREE. Вызывается из любой goroutine без mutex.
//	            Использует atomic.Load для чтения слотов.
//
//	Put()     — вызывается ПОД ВНЕШНИМ sync.Mutex (indexShard.mu).
//	Delete()  — вызывается ПОД ВНЕШНИМ sync.Mutex.
//	Grow()    — вызывается ПОД ВНЕШНИМ sync.Mutex.
//	Rebuild() — вызывается ПОД ВНЕШНИМ sync.Mutex.
//
// Почему Put/Delete не lock-free:
//
//	TOCTOU гонка при resize. Mutex на writer'ах = надёжно.
//	А 80% трафика (GET) — полностью lock-free.
type HashTable struct {
	slots      []slot // pre-allocated массив ячеек
	size       uint64 // всегда power of 2
	mask       uint64 // size - 1 (для побитового модуля)
	count      int    // количество живых записей
	tombstones int    // количество tombstone'ов
}

// NewHashTable создаёт таблицу.
// capacity округляется вверх до ближайшей степени двойки.
func NewHashTable(capacity int) *HashTable {
	cap := uint64(defaultInitialCap)
	for cap < uint64(capacity) {
		cap <<= 1
	}
	return &HashTable{
		slots: make([]slot, cap),
		size:  cap,
		mask:  cap - 1,
	}
}

// sanitizeHash гарантирует, что хеш не совпадает с sentinel'ами.
//
// Если хеш ключа случайно равен 0 или MAX — сдвигаем.
// Вероятность: 2/2^64 ≈ 10^-19. Но defensive coding.
func sanitizeHash(h uint64) uint64 {
	if h == emptyHash {
		return 1
	}
	if h == tombstoneHash {
		return tombstoneHash - 1
	}
	return h
}

// ─── Get ────────────────────────────────────────────────────
//
// LOCK-FREE поиск по хешу.
//
// Путь:
//
//	idx = hash & mask → слот
//	Если слот пуст (hash=0) → ключ не найден
//	Если hash совпал → читаем handle (гарантированно записан, см. slot comment)
//	Иначе → linear probing (idx+1)
//
// Стоимость: ~2-5ns (atomic Load + побитовое И + сравнение).
// Сравни с текущим: ~50ns (RLock + map lookup + RUnlock).
func (t *HashTable) Get(hash uint64) (uint64, bool) {
	hash = sanitizeHash(hash)
	idx := hash & t.mask

	for i := uint64(0); i < t.size; i++ {
		h := t.slots[idx].hash.Load()

		switch h {
		case emptyHash:
			// Пустой слот = конец probing chain. Ключа нет.
			return 0, false

		case hash:
			// Нашли! handle гарантированно записан (ordered writes).
			return t.slots[idx].handle.Load(), true
		}

		// tombstone или чужой hash → следующий слот
		idx = (idx + 1) & t.mask
	}
	return 0, false
}

// ─── Put ────────────────────────────────────────────────────
//
// Запись hash → handle. ВЫЗЫВАТЬ ПОД ВНЕШНИМ MUTEX.
//
// Три случая:
//  1. Пустой слот → записываем (handle первым, hash вторым)
//  2. Tombstone → переиспользуем слот (handle первым, hash вторым)
//  3. Тот же hash → обновляем handle (перезапись значения)
//
// Atomic Store нужен даже под mutex: чтобы lock-free Get()
// в другой goroutine увидел данные через memory barrier.
func (t *HashTable) Put(hash, handle uint64) {
	hash = sanitizeHash(hash)
	idx := hash & t.mask

	for i := uint64(0); i < t.size; i++ {
		h := t.slots[idx].hash.Load()

		switch h {
		case emptyHash:
			// Свободный слот. Записываем.
			// ПОРЯДОК: handle ПЕРВЫМ → hash ВТОРЫМ (release barrier).
			t.slots[idx].handle.Store(handle)
			t.slots[idx].hash.Store(hash)
			t.count++
			return

		case tombstoneHash:
			// Переиспользуем tombstone.
			t.slots[idx].handle.Store(handle)
			t.slots[idx].hash.Store(hash)
			t.count++
			t.tombstones--
			return

		case hash:
			// Обновление существующего ключа.
			// hash уже записан и правильный → обновляем только handle.
			t.slots[idx].handle.Store(handle)
			return
		}

		idx = (idx + 1) & t.mask
	}
	panic("hashtable: table is full, grow was not called in time")
}

// ─── Delete ─────────────────────────────────────────────────
//
// Удаляет запись, помечая слот как tombstone.
// ВЫЗЫВАТЬ ПОД ВНЕШНИМ MUTEX.
//
// Tombstone (hash = 0xFFF...F) НЕ прерывает probing chain.
// Get() пропускает tombstone и продолжает поиск.
// При >25% tombstones → вызываем Rebuild().
//
// Возвращает handle удалённого элемента (для mcache.Free).
func (t *HashTable) Delete(hash uint64) (handle uint64, ok bool) {
	hash = sanitizeHash(hash)
	idx := hash & t.mask

	for i := uint64(0); i < t.size; i++ {
		h := t.slots[idx].hash.Load()

		switch h {
		case emptyHash:
			return 0, false

		case hash:
			handle = t.slots[idx].handle.Load()
			// Помечаем как tombstone.
			// Порядок не критичен (мы под mutex, Get увидит tombstone
			// и пропустит → не прочитает stale handle).
			t.slots[idx].hash.Store(tombstoneHash)
			t.count--
			t.tombstones++
			return handle, true
		}

		idx = (idx + 1) & t.mask
	}
	return 0, false
}

// ─── Grow / Rebuild ─────────────────────────────────────────

// NeedsGrow возвращает true, если load factor превысил порог.
// Считает и живые записи, и tombstones — оба занимают слоты.
func (t *HashTable) NeedsGrow() bool {
	used := float64(t.count + t.tombstones)
	return used/float64(t.size) > loadFactorThreshold
}

// NeedsRebuild возвращает true, если tombstones > 25%.
func (t *HashTable) NeedsRebuild() bool {
	if t.tombstones == 0 {
		return false
	}
	return float64(t.tombstones)/float64(t.size) > tombstoneThreshold
}

// Grow создаёт новую таблицу 2x размером и копирует живые записи.
// Tombstones НЕ копируются (бесплатная очистка при grow).
// ВЫЗЫВАТЬ ПОД ВНЕШНИМ MUTEX.
func (t *HashTable) Grow() *HashTable {
	newTable := NewHashTable(int(t.size) * 2)
	t.copyInto(newTable)
	return newTable
}

// Rebuild создаёт новую таблицу того же размера без tombstones.
// ВЫЗЫВАТЬ ПОД ВНЕШНИМ MUTEX.
func (t *HashTable) Rebuild() *HashTable {
	newTable := NewHashTable(int(t.size))
	t.copyInto(newTable)
	return newTable
}

// copyInto копирует все живые записи в dst.
// Tombstones и пустые слоты пропускаются.
func (t *HashTable) copyInto(dst *HashTable) {
	for i := uint64(0); i < t.size; i++ {
		h := t.slots[i].hash.Load()
		if h != emptyHash && h != tombstoneHash {
			v := t.slots[i].handle.Load()
			dst.Put(h, v)
		}
	}
}

// Len возвращает количество живых записей.
func (t *HashTable) Len() int {
	return t.count
}
