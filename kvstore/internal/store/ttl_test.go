package store

import (
	"fmt"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// ─── Mock Evictor ──────────────────────────────────────────
//
// Подставка вместо реального TCMallocStore.
// Считает сколько раз каждый ключ был удалён.
type mockEvictor struct {
	mu      sync.Mutex
	deleted map[string]int
}

func newMockEvictor() *mockEvictor {
	return &mockEvictor{deleted: make(map[string]int)}
}

func (m *mockEvictor) Del(key string) bool {
	m.mu.Lock()
	m.deleted[key]++
	m.mu.Unlock()
	return true
}

func (m *mockEvictor) delCount(key string) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.deleted[key]
}

func (m *mockEvictor) totalDeleted() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	total := 0
	for _, v := range m.deleted {
		total += v
	}
	return total
}

// ─── Тесты ─────────────────────────────────────────────────

// TestTTL_SetAndExpire — базовый сценарий:
// Устанавливаем TTL → ждём → ключ должен быть просрочен.
//
// Почему TTL = 50ms, а ждём 100ms?
// В тестах нельзя ждать секунды — тесты должны быть быстрыми.
// 50ms достаточно, чтобы гарантировать истечение,
// а 100ms ожидания даёт запас на нестабильность CI.
func TestTTL_SetAndExpire(t *testing.T) {
	ev := newMockEvictor()
	ttl := NewTTLManager(ev)
	defer ttl.Stop()

	ttl.Set("key1", 50*time.Millisecond)

	// Сразу после установки — не просрочен
	if ttl.IsExpired("key1") {
		t.Fatal("key1 should NOT be expired immediately")
	}

	// Ждём истечения
	time.Sleep(100 * time.Millisecond)

	// Теперь должен быть просрочен
	if !ttl.IsExpired("key1") {
		t.Fatal("key1 SHOULD be expired after 100ms")
	}

	// IsExpired должен был вызвать store.Del
	if ev.delCount("key1") != 1 {
		t.Fatalf("expected Del called 1 time, got %d", ev.delCount("key1"))
	}
}

// TestTTL_NotExpired — ключ с длинным TTL не должен истекать.
func TestTTL_NotExpired(t *testing.T) {
	ev := newMockEvictor()
	ttl := NewTTLManager(ev)
	defer ttl.Stop()

	ttl.Set("key1", 10*time.Second)

	if ttl.IsExpired("key1") {
		t.Fatal("key1 with 10s TTL should NOT be expired")
	}

	// store.Del НЕ должен вызываться
	if ev.delCount("key1") != 0 {
		t.Fatal("Del should NOT be called for non-expired key")
	}
}

// TestTTL_Remove — PERSIST: убираем TTL, ключ больше не истекает.
func TestTTL_Remove(t *testing.T) {
	ev := newMockEvictor()
	ttl := NewTTLManager(ev)
	defer ttl.Stop()

	ttl.Set("key1", 50*time.Millisecond)

	// PERSIST — убираем TTL
	ok := ttl.Remove("key1")
	if !ok {
		t.Fatal("Remove should return true for existing TTL")
	}

	// Ждём "истечения"
	time.Sleep(100 * time.Millisecond)

	// Ключ НЕ должен быть просрочен — TTL снят
	if ttl.IsExpired("key1") {
		t.Fatal("key1 should NOT be expired after PERSIST")
	}

	// Remove на несуществующий ключ
	ok = ttl.Remove("nonexistent")
	if ok {
		t.Fatal("Remove should return false for non-existing TTL")
	}
}

// TestTTL_OnDelete — при ручном DEL ключа TTL должен очиститься.
func TestTTL_OnDelete(t *testing.T) {
	ev := newMockEvictor()
	ttl := NewTTLManager(ev)
	defer ttl.Stop()

	ttl.Set("key1", 10*time.Second)
	if ttl.Len() != 1 {
		t.Fatalf("expected Len=1, got %d", ttl.Len())
	}

	// Имитируем DEL key1 из основного store
	ttl.OnDelete("key1")

	if ttl.Len() != 0 {
		t.Fatalf("expected Len=0 after OnDelete, got %d", ttl.Len())
	}

	// IsExpired не должен срабатывать — записи уже нет
	if ttl.IsExpired("key1") {
		t.Fatal("key1 should NOT be expired after OnDelete")
	}
}

// TestTTL_TTLRemaining — проверка оставшегося времени.
func TestTTL_TTLRemaining(t *testing.T) {
	ev := newMockEvictor()
	ttl := NewTTLManager(ev)
	defer ttl.Stop()

	// Ключ без TTL
	if ttl.TTL("nokey") != -1 {
		t.Fatal("TTL of non-existing key should be -1")
	}

	// Ключ с TTL
	ttl.Set("key1", 5*time.Second)
	remaining := ttl.TTL("key1")
	if remaining < 4*time.Second || remaining > 5*time.Second {
		t.Fatalf("TTL should be ~5s, got %v", remaining)
	}

	// Просроченный ключ
	ttl.Set("key2", 1*time.Millisecond)
	time.Sleep(10 * time.Millisecond)
	if ttl.TTL("key2") != 0 {
		t.Fatalf("TTL of expired key should be 0, got %v", ttl.TTL("key2"))
	}
}

// TestTTL_OverwriteTTL — перезапись TTL продлевает жизнь ключа.
func TestTTL_OverwriteTTL(t *testing.T) {
	ev := newMockEvictor()
	ttl := NewTTLManager(ev)
	defer ttl.Stop()

	// Ставим короткий TTL
	ttl.Set("key1", 50*time.Millisecond)

	// Перезаписываем на длинный
	ttl.Set("key1", 10*time.Second)

	// Ждём "короткого" TTL
	time.Sleep(100 * time.Millisecond)

	// Ключ НЕ должен быть просрочен — TTL обновлён
	if ttl.IsExpired("key1") {
		t.Fatal("key1 should NOT be expired — TTL was overwritten to 10s")
	}
}

// TestTTL_ActiveExpiry — фоновая горутина удаляет просроченные ключи.
//
// Без вызова IsExpired! Мы проверяем, что activeExpiry
// сам находит и удаляет просроченные ключи.
func TestTTL_ActiveExpiry(t *testing.T) {
	ev := newMockEvictor()
	ttl := NewTTLManager(ev)
	defer ttl.Stop()

	// Создаём 100 ключей с коротким TTL
	for i := 0; i < 100; i++ {
		ttl.Set(fmt.Sprintf("key:%d", i), 50*time.Millisecond)
	}

	if ttl.Len() != 100 {
		t.Fatalf("expected 100 keys, got %d", ttl.Len())
	}

	// Ждём: activeExpiry тикает каждые 100ms
	// За 500ms должен пройти несколько циклов и удалить всё
	time.Sleep(7 * time.Second)

	remaining := ttl.Len()
	if remaining > 10 {
		t.Fatalf("activeExpiry should have cleaned most keys, but %d remain", remaining)
	}

	// store.Del должен быть вызван для удалённых ключей
	if ev.totalDeleted() < 90 {
		t.Fatalf("expected at least 90 deletions, got %d", ev.totalDeleted())
	}
}

// TestTTL_ShardDistribution — ключи распределяются по разным шардам.
//
// Если бы FNV-хеш был сломан и все ключи падали в один шард,
// мы бы потеряли весь смысл шардирования.
func TestTTL_ShardDistribution(t *testing.T) {
	ev := newMockEvictor()
	ttl := NewTTLManager(ev)
	defer ttl.Stop()

	// Вставляем 10K ключей
	for i := 0; i < 10000; i++ {
		ttl.Set(fmt.Sprintf("user:%d", i), 10*time.Second)
	}

	// Считаем сколько шардов непустые
	nonEmpty := 0
	for i := range ttl.shards {
		ttl.shards[i].mu.RLock()
		if len(ttl.shards[i].expires) > 0 {
			nonEmpty++
		}
		ttl.shards[i].mu.RUnlock()
	}

	// При хорошем хеше 10K ключей должны попасть минимум в 200+ шардов из 256
	if nonEmpty < 200 {
		t.Fatalf("poor shard distribution: only %d/256 shards used", nonEmpty)
	}
}

// TestTTL_Concurrent — гонки данных.
//
// 🔴 ЗАПУСКАТЬ С: go test -race
//
// 10 горутин параллельно: Set, IsExpired, Remove, TTL, OnDelete.
// Если есть data race — go test -race поймает.
func TestTTL_Concurrent(t *testing.T) {
	ev := newMockEvictor()
	ttl := NewTTLManager(ev)
	defer ttl.Stop()

	const goroutines = 10
	const opsPerGoroutine = 1000

	var wg sync.WaitGroup
	wg.Add(goroutines)

	for g := 0; g < goroutines; g++ {
		go func(id int) {
			defer wg.Done()
			for i := 0; i < opsPerGoroutine; i++ {
				key := fmt.Sprintf("key:%d:%d", id, i%100)
				switch i % 5 {
				case 0:
					ttl.Set(key, time.Duration(50+i%100)*time.Millisecond)
				case 1:
					ttl.IsExpired(key)
				case 2:
					ttl.Remove(key)
				case 3:
					ttl.TTL(key)
				case 4:
					ttl.OnDelete(key)
				}
			}
		}(g)
	}

	wg.Wait()
	// Если дошли сюда без паники и -race не ругнулся — тест пройден
}

// TestTTL_IsExpiredDoubleCheck — два воркера одновременно вызывают IsExpired.
//
// Проверяем, что store.Del вызывается РОВНО 1 раз,
// а не 2 (как было бы без double-check в WLock).
func TestTTL_IsExpiredDoubleCheck(t *testing.T) {
	ev := newMockEvictor()
	ttl := NewTTLManager(ev)
	defer ttl.Stop()

	const key = "race-key"

	// Повторяем 100 раз для надёжности
	for round := 0; round < 100; round++ {
		ttl.Set(key, 1*time.Millisecond)
		time.Sleep(5 * time.Millisecond) // гарантируем истечение

		var wg sync.WaitGroup
		var expiredCount atomic.Int32

		// Два воркера одновременно проверяют один ключ
		wg.Add(2)
		for w := 0; w < 2; w++ {
			go func() {
				defer wg.Done()
				if ttl.IsExpired(key) {
					expiredCount.Add(1)
				}
			}()
		}
		wg.Wait()

		// Оба могут вернуть true (это нормально — ключ действительно был expired).
		// Но store.Del должен быть вызван МАКСИМУМ 1 раз за раунд.
	}

	// За 100 раундов Del должен быть вызван ≤ 100 раз (по 1 на раунд)
	total := ev.delCount(key)
	if total > 100 {
		t.Fatalf("Del called %d times for 100 rounds — double deletion detected!", total)
	}
}

// TestTTL_ZeroAndNegative — граничные значения TTL.
//
// Кривой клиент может прислать: SET key val EX 0 или EX -5.
// time.Now().Add(0) = сейчас, time.Now().Add(-5s) = прошлое.
// Оба случая должны давать IsExpired() == true немедленно.
func TestTTL_ZeroAndNegative(t *testing.T) {
	ev := newMockEvictor()
	ttl := NewTTLManager(ev)
	defer ttl.Stop()

	// Нулевой TTL — время истечения = time.Now()
	ttl.Set("zero", 0)
	time.Sleep(1 * time.Millisecond) // чтобы Now() гарантированно прошёл
	if !ttl.IsExpired("zero") {
		t.Fatal("zero TTL should be expired immediately")
	}

	// Отрицательный TTL — время истечения в прошлом
	ttl.Set("negative", -1*time.Second)
	if !ttl.IsExpired("negative") {
		t.Fatal("negative TTL should be expired immediately")
	}

	// Оба должны вызвать store.Del
	if ev.delCount("zero") != 1 {
		t.Fatalf("expected 1 Del for 'zero', got %d", ev.delCount("zero"))
	}
	if ev.delCount("negative") != 1 {
		t.Fatalf("expected 1 Del for 'negative', got %d", ev.delCount("negative"))
	}
}

// TestTTL_DoubleStop — двойной Stop не должен вызывать панику.
//
// Проблема: close(ch) на уже закрытый канал = panic.
// Решение: sync.Once гарантирует однократное закрытие.
//
// Реальный сценарий:
//
//	defer ttl.Stop()    // в main()
//	ttl.Stop()          // в SIGTERM handler
//	→ без sync.Once — паника, приложение падает при shutdown
func TestTTL_DoubleStop(t *testing.T) {
	ev := newMockEvictor()
	ttl := NewTTLManager(ev)

	// Не должно паниковать
	ttl.Stop()
	ttl.Stop()
	ttl.Stop()
}

// TestTTL_EmptyKey — пустой ключ не должен ломать хеширование.
func TestTTL_EmptyKey(t *testing.T) {
	ev := newMockEvictor()
	ttl := NewTTLManager(ev)
	defer ttl.Stop()

	ttl.Set("", 50*time.Millisecond)

	if ttl.Len() != 1 {
		t.Fatalf("expected Len=1, got %d", ttl.Len())
	}

	remaining := ttl.TTL("")
	if remaining <= 0 || remaining > 50*time.Millisecond {
		t.Fatalf("unexpected TTL for empty key: %v", remaining)
	}

	time.Sleep(100 * time.Millisecond)

	if !ttl.IsExpired("") {
		t.Fatal("empty key should be expired")
	}

	ttl.Set("", 10*time.Second)
	ttl.OnDelete("")
	if ttl.Len() != 0 {
		t.Fatal("OnDelete for empty key should work")
	}
}

// TestTTL_GoroutineLeak — Stop() должен завершать activeExpiry горутину.
//
// Без этого теста утечка горутин незаметна:
// 100 вызовов NewTTLManager без Stop = 100 зомби-горутин.
func TestTTL_GoroutineLeak(t *testing.T) {
	// Даём время на завершение горутин от предыдущих тестов
	time.Sleep(50 * time.Millisecond)
	runtime.GC()
	before := runtime.NumGoroutine()

	// Создаём и уничтожаем 10 TTLManager'ов
	for i := 0; i < 10; i++ {
		ev := newMockEvictor()
		ttl := NewTTLManager(ev)
		ttl.Stop()
	}

	// Даём горутинам время завершиться
	time.Sleep(50 * time.Millisecond)
	runtime.GC()
	after := runtime.NumGoroutine()

	// Допускаем +/- 2 горутины (runtime jitter)
	if after > before+2 {
		t.Fatalf("goroutine leak: before=%d, after=%d (leaked %d)",
			before, after, after-before)
	}
}

// ─── Бенчмарки ─────────────────────────────────────────────

// BenchmarkTTL_IsExpired_Parallel — нагрузочный тест IsExpired.
//
// b.RunParallel запускает GOMAXPROCS горутин (12 на твоём CPU).
// Если шардирование работает — throughput растёт линейно с ядрами.
// Если бы был один мьютекс — throughput упирался бы в потолок.
//
// Запуск: go test ./internal/store/ -bench BenchmarkTTL -benchtime 3s
func BenchmarkTTL_IsExpired_Parallel(b *testing.B) {
	ev := newMockEvictor()
	ttl := NewTTLManager(ev)
	defer ttl.Stop()

	// Подготовка: 10K ключей (половина с TTL, половина без)
	for i := 0; i < 5000; i++ {
		ttl.Set(fmt.Sprintf("ttl:%d", i), 1*time.Hour)
	}

	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		i := 0
		for pb.Next() {
			// Чередуем: ключ с TTL и ключ без TTL
			key := fmt.Sprintf("ttl:%d", i%10000)
			ttl.IsExpired(key)
			i++
		}
	})
}

// BenchmarkTTL_Set_Parallel — нагрузка на запись TTL.
func BenchmarkTTL_Set_Parallel(b *testing.B) {
	ev := newMockEvictor()
	ttl := NewTTLManager(ev)
	defer ttl.Stop()

	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		i := 0
		for pb.Next() {
			ttl.Set(fmt.Sprintf("key:%d", i%10000), 1*time.Hour)
			i++
		}
	})
}
