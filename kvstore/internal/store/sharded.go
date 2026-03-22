package store

import (
	"hash/fnv"
	"sync"
)

const numShards = 256

type shard struct {
	mu   sync.RWMutex
	data map[string][]byte
}

type ShardedStore struct {
	shards [numShards]shard
}

func NewShardedStore() *ShardedStore {
	s := &ShardedStore{}
	for i := 0; i < numShards; i++ {
		s.shards[i].data = make(map[string][]byte)
	}
	return s
}

func (s *ShardedStore) getShard(key string) *shard {
	h := fnv.New32a()
	h.Write([]byte(key))
	return &s.shards[h.Sum32()%numShards]
}

func (s *ShardedStore) Set(key string, value []byte) {
	sh := s.getShard(key)
	sh.mu.Lock()
	sh.data[key] = value
	sh.mu.Unlock()
}

func (s *ShardedStore) Get(key string) ([]byte, bool) {
	sh := s.getShard(key)
	sh.mu.RLock()
	val, ok := sh.data[key]
	sh.mu.RUnlock()
	return val, ok
}

func (s *ShardedStore) Del(key string) bool {
	sh := s.getShard(key)
	sh.mu.Lock()
	_, ok := sh.data[key]
	delete(sh.data, key)
	sh.mu.Unlock()
	return ok

}

func (s *ShardedStore) Len() int {
	total := 0
	for i := 0; i < numShards; i++ {
		s.shards[i].mu.RLock()
		total += len(s.shards[i].data)
		s.shards[i].mu.RUnlock()

	}
	return total
}

func (s *ShardedStore) Flush() {
	for i := 0; i < numShards; i++ {
		s.shards[i].mu.Lock()
		s.shards[i].data = make(map[string][]byte)
		s.shards[i].mu.Unlock()
	}
}
