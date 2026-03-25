package store

import (
	"sync"
	"time"
)

const (
	sampleSize       = 20
	expiredThreshold = sampleSize / 4 // 5 из 20 = 25%
	maxLoops         = 16
)

// Evictor — минимальный интерфейс для удаления ключей.
// TTLManager не знает ничего о реализации хранилища.
// Это позволяет: подставить мок в тестах, сменить store без переписывания TTL.
type Evictor interface {
	Del(key string) bool
}

// TTLManager — управление временем жизни ключей.
// Вероятностный алгоритм Redis: random sampling + adaptive aggressiveness.
type TTLManager struct {
	mu      sync.Mutex
	expires map[string]time.Time
	store   Evictor
	stop    chan struct{}
}

func NewTTLManager(store Evictor) *TTLManager {
	m := &TTLManager{
		expires: make(map[string]time.Time),
		store:   store,
		stop:    make(chan struct{}),
	}
	go m.activeExpiry()
	return m
}

// Set устанавливает TTL для ключа.
func (m *TTLManager) Set(key string, ttl time.Duration) {
	m.mu.Lock()
	m.expires[key] = time.Now().Add(ttl)
	m.mu.Unlock()
}

// Remove убирает TTL (команда PERSIST).
func (m *TTLManager) Remove(key string) bool {
	m.mu.Lock()
	_, ok := m.expires[key]
	delete(m.expires, key)
	m.mu.Unlock()
	return ok
}

// TTL возвращает оставшееся время жизни.
func (m *TTLManager) TTL(key string) time.Duration {
	m.mu.Lock()
	expiresAt, ok := m.expires[key]
	m.mu.Unlock()

	if !ok {
		return -1
	}

	remaining := time.Until(expiresAt)
	if remaining <= 0 {
		return 0
	}
	return remaining
}

// IsExpired — lazy expiration.
// Проверяет и удаляет просроченный ключ при обращении.
func (m *TTLManager) IsExpired(key string) bool {
	m.mu.Lock()
	expiresAt, ok := m.expires[key]

	if !ok {
		m.mu.Unlock()
		return false
	}

	if time.Now().Before(expiresAt) {
		m.mu.Unlock()
		return false
	}

	delete(m.expires, key)
	m.mu.Unlock()

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

// expireCycle — адаптивный цикл: повторяет если >25% просрочены.
func (m *TTLManager) expireCycle() {
	for loop := 0; loop < maxLoops; loop++ {
		expired := m.sampleAndExpire()
		if expired < expiredThreshold {
			return
		}
	}
}

// sampleAndExpire — zero-alloc random sampling.
func (m *TTLManager) sampleAndExpire() int {
	m.mu.Lock()

	if len(m.expires) == 0 {
		m.mu.Unlock()
		return 0
	}

	now := time.Now()

	// Массив на СТЕКЕ — ноль давления на GC
	var expiredArr [sampleSize]string
	expiredKeys := expiredArr[:0]
	checked := 0

	// Один проход: итерация + проверка + удаление
	for key, expiresAt := range m.expires {
		checked++

		if now.After(expiresAt) {
			expiredKeys = append(expiredKeys, key)
			delete(m.expires, key)
		}

		if checked >= sampleSize {
			break
		}
	}

	m.mu.Unlock()

	// Удаляем из store ВНЕ лока TTLManager
	for _, key := range expiredKeys {
		m.store.Del(key)
	}

	return len(expiredKeys)
}

// OnDelete убирает TTL при ручном удалении ключа (DEL).
func (m *TTLManager) OnDelete(key string) {
	m.mu.Lock()
	delete(m.expires, key)
	m.mu.Unlock()
}

// Len возвращает количество ключей с TTL.
func (m *TTLManager) Len() int {
	m.mu.Lock()
	n := len(m.expires)
	m.mu.Unlock()
	return n
}

// Stop останавливает фоновую очистку.
func (m *TTLManager) Stop() {
	close(m.stop)
}
