package store

import (
	"encoding/binary"
	"hash/fnv"
	"sync"
)

// Arena — непрерывный буфер байтов для хранения данных.
// Для GC это ОДИН объект, а не миллионы отдельных []byte.
type Arena struct {
	buf []byte
}

func NewArena() *Arena {
	return &Arena{
		buf: make([]byte, 0, 4*1024*1024),
	}
}

// Put записывает данные в арену и возвращает смещение.
// Формат записи: [4 байта длина ключа][ключ][4 байта длина значения][значение]
// Это позволяет потом прочитать и ключ, и значение по одному offset.
func (a *Arena) Put(key string, value []byte) uint32 {
	offset := uint32(len(a.buf))

	// Записываем длину ключа (4 байта, little-endian)
	keyLen := make([]byte, 4)
	binary.LittleEndian.PutUint32(keyLen, uint32(len(key)))
	a.buf = append(a.buf, keyLen...)

	// Записываем сам ключ
	a.buf = append(a.buf, key...)

	// Записываем длину значения (4 байта)
	valLen := make([]byte, 4)
	binary.LittleEndian.PutUint32(valLen, uint32(len(value)))
	a.buf = append(a.buf, valLen...)

	// Записываем значение
	a.buf = append(a.buf, value...)

	return offset
}

// GetValue читает ключ и значение по смещению.
// Возвращает (ключ, значение).
func (a *Arena) GetValue(offset uint32) (string, []byte) {
	pos := int(offset)

	// Читаем длину ключа
	keyLen := binary.LittleEndian.Uint32(a.buf[pos : pos+4])
	pos += 4

	// Читаем ключ
	key := string(a.buf[pos : pos+int(keyLen)])
	pos += int(keyLen)

	// Читаем длину значения
	valLen := binary.LittleEndian.Uint32(a.buf[pos : pos+4])
	pos += 4

	// Читаем значение
	value := make([]byte, valLen)
	copy(value, a.buf[pos:pos+int(valLen)])

	return key, value
}

// hashKey превращает строковый ключ в uint64.
// Это убирает string (который содержит указатель) из ключа map.
func hashKey(key string) uint64 {
	h := fnv.New64a()
	h.Write([]byte(key))
	return h.Sum64()
}

// arenaShard — шард для ArenaStore.
// map[uint64]uint32 — ноль указателей! GC не сканирует.
type arenaShard struct {
	mu    sync.RWMutex
	index map[uint64]uint32 // хэш ключа → смещение в арене
	arena *Arena
}

// ArenaStore — Zero-GC хранилище.
// Все данные лежат в одном непрерывном буфере (Arena).
// Индекс содержит только числа — GC его не трогает.
type ArenaStore struct {
	shards [numShards]arenaShard
}

func NewArenaStore() *ArenaStore {
	s := &ArenaStore{}
	for i := 0; i < numShards; i++ {
		s.shards[i].index = make(map[uint64]uint32)
		s.shards[i].arena = NewArena()
	}
	return s
}

func (s *ArenaStore) getShard(hash uint64) *arenaShard {
	return &s.shards[hash%numShards]
}

func (s *ArenaStore) Set(key string, value []byte) {
	hash := hashKey(key)
	sh := s.getShard(hash)
	sh.mu.Lock()
	offset := sh.arena.Put(key, value)
	sh.index[hash] = offset
	sh.mu.Unlock()
}

func (s *ArenaStore) Get(key string) ([]byte, bool) {
	hash := hashKey(key)
	sh := s.getShard(hash)

	sh.mu.RLock()
	offset, ok := sh.index[hash]
	if !ok {
		sh.mu.RUnlock()
		return nil, false
	}

	storedKey, value := sh.arena.GetValue(offset)
	sh.mu.RUnlock()

	if storedKey != key {
		return nil, false
	}
	return value, true
}

func (s *ArenaStore) Del(key string) bool {
	hash := hashKey(key)
	sh := s.getShard(hash)
	sh.mu.Lock()
	_, ok := sh.index[hash]
	delete(sh.index, hash)
	sh.mu.Unlock()
	return ok
}
func (s *ArenaStore) Len() int {
	total := 0
	for i := 0; i < numShards; i++ {
		s.shards[i].mu.RLock()
		total += len(s.shards[i].index)
		s.shards[i].mu.RUnlock()
	}
	return total
}

func (s *ArenaStore) ForEach(fn func(key string, value []byte)) {
	for i := 0; i < numShards; i++ {
		sh := &s.shards[i]
		sh.mu.RLock()
		for _, offset := range sh.index {
			key, value := sh.arena.GetValue(offset)
			fn(key, value)
		}
		sh.mu.RUnlock()
	}
}
