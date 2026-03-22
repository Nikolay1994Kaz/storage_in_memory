package store

import "sync"

type NaiveStore struct {
	mu   sync.RWMutex
	data map[string][]byte
}

func NewNaiveStore() *NaiveStore {
	return &NaiveStore{
		data: make(map[string][]byte),
	}

}

func (s *NaiveStore) Set(key string, value []byte) {
	s.mu.Lock()
	s.data[key] = value
	s.mu.Unlock()
}

func (s *NaiveStore) Get(key string) ([]byte, bool) {
	s.mu.RLock()
	val, ok := s.data[key]
	s.mu.RUnlock()
	return val, ok
}

func (s *NaiveStore) Del(key string) bool {
	s.mu.Lock()
	_, ok := s.data[key]
	delete(s.data, key)
	s.mu.Unlock()
	return ok
}

func (s *NaiveStore) Len() int {
	s.mu.RLock()
	n := len(s.data)
	s.mu.RUnlock()
	return n
}

func (s *NaiveStore) Flush() {
	s.mu.Lock()
	s.data = make(map[string][]byte)
	s.mu.Unlock()
}
