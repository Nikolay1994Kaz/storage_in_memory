// Package kv — строковое KV-хранилище сервера: 256 шардов map[string][]byte,
// каждый под своим RWMutex.
//
// Здесь раньше стоял свой аллокатор по мотивам TCMalloc (MHeap/MCentral/MCache,
// size classes, QSBR-освобождение), а индекс хранил хеш → хендл в арене. A/B
// 22.09.2026 против этой шардированной map показал: GET на арене в 4 раза
// медленнее в один поток и делает 2 аллокации на вызов (копия ключа и
// значения — без неё lock-free читатель смотрел бы в переиспользуемый слот).
// Единственный выигрыш арены — дешёвая сборка мусора при миллионах живых
// ключей — на живом сервере под нагрузкой не виден: GC забирает 0,09 % CPU.
// Цена же была высокой: MCache на воркер требовал стабильного WorkerID (отсюда
// свой epoll-слой), QSBR — дисциплины quiescent-состояний, а учёт памяти не
// уменьшался при удалении (OOM «залипал» до рестарта).
//
// Контракт значений: хранилище НЕ копирует значение. Set забирает срез во
// владение — вызывающий больше его не меняет. Get отдаёт тот же срез без копии:
// его можно читать (писать в ответ клиенту, декодировать), но нельзя менять.
// Перезапись ключа кладёт в map НОВЫЙ срез, старый никто не трогает, поэтому
// читатель, успевший взять прежнее значение, видит его целиком — без
// QSBR и без отложенного освобождения: срез живёт, пока на него есть ссылка.
package kv

import (
	"hash/maphash"
	"sync"
	"sync/atomic"
)

// numShards — число шардов. 256 — та же раскладка, что прошла A/B 22.09.
const numShards = 256

// entryOverhead — оценка служебных байт одной записи сверх самих ключа и
// значения: заголовки string (16 Б) и []byte (24 Б) в слоте map, управляющий
// байт и недозаполнение групп таблицы. Учёт приближённый и служит одному —
// гейту maxmemory; он НЕ равен RSS процесса (map не отдаёт память при удалении,
// аллокатор Go округляет размеры), зато, в отличие от прежнего учёта арены,
// он уменьшается, когда ключи удаляют.
const entryOverhead = 64

type shard struct {
	mu    sync.RWMutex
	m     map[string][]byte
	bytes int64 // учтённый объём записей шарда (под mu) — нужен для Clear за O(1)

	_ [64]byte // разносит соседние шарды по разным кэш-линиям
}

// Store — шардированное KV-хранилище. Все методы безопасны для конкурентного
// вызова; нулевое значение не годится, создавать через New.
type Store struct {
	seed   maphash.Seed
	shards [numShards]shard

	used      atomic.Int64 // приближённый объём данных (ключи, значения и служебные байты)
	maxMemory atomic.Int64 // 0 = без лимита
}

// New создаёт пустое хранилище.
func New() *Store {
	s := &Store{seed: maphash.MakeSeed()}
	for i := range s.shards {
		s.shards[i].m = make(map[string][]byte)
	}
	return s
}

func (s *Store) shardFor(key string) *shard {
	return &s.shards[maphash.String(s.seed, key)%numShards]
}

func entrySize(key string, value []byte) int64 {
	return int64(len(key)+len(value)) + entryOverhead
}

// Set записывает значение, забирая срез value во владение (см. контракт пакета).
func (s *Store) Set(key string, value []byte) {
	sh := s.shardFor(key)
	sh.mu.Lock()
	old, existed := sh.m[key]
	sh.m[key] = value
	delta := entrySize(key, value)
	if existed {
		delta -= entrySize(key, old)
	}
	sh.bytes += delta
	sh.mu.Unlock()
	s.used.Add(delta)
}

// Get возвращает значение без копии. Срез только для чтения.
func (s *Store) Get(key string) ([]byte, bool) {
	sh := s.shardFor(key)
	sh.mu.RLock()
	v, ok := sh.m[key]
	sh.mu.RUnlock()
	return v, ok
}

// Del удаляет ключ. Возвращает true, если ключ был.
// Подходит под интерфейс store.Evictor (TTL-эвикция зовёт его напрямую).
func (s *Store) Del(key string) bool {
	sh := s.shardFor(key)
	sh.mu.Lock()
	old, ok := sh.m[key]
	var delta int64
	if ok {
		delete(sh.m, key)
		delta = -entrySize(key, old)
		sh.bytes += delta
	}
	sh.mu.Unlock()
	if ok {
		s.used.Add(delta)
	}
	return ok
}

// Len возвращает число ключей.
func (s *Store) Len() int {
	n := 0
	for i := range s.shards {
		sh := &s.shards[i]
		sh.mu.RLock()
		n += len(sh.m)
		sh.mu.RUnlock()
	}
	return n
}

// ForEach обходит все ключи (порядок не определён). Нужен снапшоту и
// репликации — не горячему пути.
//
// Лок шарда держится только на время копирования его пар в буфер, а fn
// вызывается уже без лока: снапшот, пишущий на диск, не блокирует запись в
// шард, и fn может сам обращаться к хранилищу без риска самоблокировки.
// Значения в буфере — те же срезы, что в map; они неизменяемы (контракт Set),
// поэтому копировать байты не нужно. Ключ, изменённый во время обхода, может
// попасть в fn со старым или новым значением — снапшот это допускает: всё,
// что записано после начала обхода, есть и в WAL.
func (s *Store) ForEach(fn func(key string, value []byte)) {
	type kvPair struct {
		k string
		v []byte
	}
	var buf []kvPair
	for i := range s.shards {
		sh := &s.shards[i]
		sh.mu.RLock()
		buf = buf[:0]
		for k, v := range sh.m {
			buf = append(buf, kvPair{k, v})
		}
		sh.mu.RUnlock()
		for _, p := range buf {
			fn(p.k, p.v)
		}
	}
}

// Clear удаляет все ключи (FULLSYNC реплики в экспериментальном кластере).
func (s *Store) Clear() {
	for i := range s.shards {
		sh := &s.shards[i]
		sh.mu.Lock()
		freed := sh.bytes
		sh.m = make(map[string][]byte)
		sh.bytes = 0
		sh.mu.Unlock()
		s.used.Add(-freed)
	}
}

// ── Лимит памяти (гейт maxmemory) ───────────────────────────

// SetMaxMemory задаёт лимит в байтах; 0 — без лимита.
func (s *Store) SetMaxMemory(bytes int64) { s.maxMemory.Store(bytes) }

// MaxMemory возвращает лимит в байтах (0 — без лимита).
func (s *Store) MaxMemory() int64 { return s.maxMemory.Load() }

// UsedMemory возвращает учтённый объём: KV-записи плюс всё, что внешние
// структуры (sorted sets) добавили через MemoryCounter.
func (s *Store) UsedMemory() int64 { return s.used.Load() }

// IsOOM сообщает, достигнут ли лимит. Один atomic Load — зовётся на каждую
// растущую команду.
func (s *Store) IsOOM() bool {
	max := s.maxMemory.Load()
	return max > 0 && s.used.Load() >= max
}

// MemoryCounter отдаёт общий счётчик учтённой памяти, чтобы sorted sets
// учитывались в том же гейте maxmemory, что и KV.
func (s *Store) MemoryCounter() *atomic.Int64 { return &s.used }
