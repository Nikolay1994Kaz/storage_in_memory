package store

import (
	"sync/atomic"
	"testing"
	"time"
)

// ─── L4: TTL на монотонных часах (невосприимчивость к скачкам wall-clock) ───
//
// ПРОБЛЕМА (до фикса): дедлайн хранился как time.Now().Add(ttl).UnixNano()
// (wall-clock), а IsExpired сравнивал с time.Now().UnixNano(). Скачок системных
// часов (NTP-коррекция, ручной перевод, DST) сдвигал ВСЕ дедлайны разом:
//   - прыжок вперёд  → массовое ЛОЖНОЕ истечение живых ключей;
//   - прыжок назад   → ключи становятся «бессмертными» (истекают позже срока).
//
// ФИКС: внутри дедлайны в МОНОТОННОМ домене (monoNow), который не прыгает.
// Wall-clock участвует только на границе WAL (ForEach → OpExpire).

// fakeClock — управляемые часы для теста. mono и wall независимы, что позволяет
// смоделировать NTP-скачок: сдвиг ТОЛЬКО wall при неподвижном mono.
type fakeClock struct {
	mono atomic.Int64 // монотонные нс с «старта»
	wall atomic.Int64 // wall-clock UnixNano
}

func newFakeClock() *fakeClock {
	c := &fakeClock{}
	// Реалистичная стартовая wall-точка (2026-01-01), mono с нуля.
	c.wall.Store(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC).UnixNano())
	return c
}

func (c *fakeClock) monoNow() int64     { return c.mono.Load() }
func (c *fakeClock) wallNow() time.Time { return time.Unix(0, c.wall.Load()) }

// advance — обычный ход времени: и монотонные, и wall идут вперёд синхронно.
func (c *fakeClock) advance(d time.Duration) {
	c.mono.Add(int64(d))
	c.wall.Add(int64(d))
}

// stepWall — скачок системных часов (NTP): двигается ТОЛЬКО wall, mono стоит.
func (c *fakeClock) stepWall(d time.Duration) { c.wall.Add(int64(d)) }

// TestTTL_L4_WallClockJumpForward_NoFalseExpiry — прыжок часов ВПЕРЁД на час
// НЕ должен досрочно убивать ключ с TTL=1h.
//
// На старом (wall-clock) коде тест ПАДАЛ бы: expiresAt = now+1h, а после
// stepWall(+2h) сравнение now'(=+2h) > expiresAt → ложное истечение.
func TestTTL_L4_WallClockJumpForward_NoFalseExpiry(t *testing.T) {
	ev := newMockEvictor()
	clk := newFakeClock()
	m := newTTLManager(ev, clk.monoNow, clk.wallNow)
	defer m.Stop()

	m.Set("key", 1*time.Hour)

	// Системные часы прыгнули на 2 часа вперёд (NTP скорректировал), но
	// реального времени прошло лишь 1 минута.
	clk.stepWall(2 * time.Hour)
	clk.advance(1 * time.Minute)

	if m.IsExpired("key") {
		t.Fatal("ключ ложно истёк из-за скачка wall-clock вперёд (регрессия L4)")
	}
	if got := ev.delCount("key"); got != 0 {
		t.Fatalf("store.Del не должен вызываться, вызван %d раз", got)
	}

	// Остаток жизни считается по монотонному времени: ~59 минут.
	rem := m.TTL("key")
	if rem < 58*time.Minute || rem > 1*time.Hour {
		t.Fatalf("TTL должен быть ~59m (монотонный остаток), got %v", rem)
	}
}

// TestTTL_L4_WallClockJumpBackward_NoImmortality — прыжок часов НАЗАД не должен
// продлевать жизнь: ключ обязан истечь ровно через свой TTL монотонного времени.
func TestTTL_L4_WallClockJumpBackward_NoImmortality(t *testing.T) {
	ev := newMockEvictor()
	clk := newFakeClock()
	m := newTTLManager(ev, clk.monoNow, clk.wallNow)
	defer m.Stop()

	m.Set("key", 100*time.Millisecond)

	// Часы уехали на час назад, но реально прошло 200мс — ключ должен истечь.
	clk.stepWall(-1 * time.Hour)
	clk.advance(200 * time.Millisecond)

	if !m.IsExpired("key") {
		t.Fatal("ключ не истёк: скачок wall-clock назад сделал его бессмертным (регрессия L4)")
	}
}

// TestTTL_L4_ForEachEmitsWallClock — на границе WAL монотонный дедлайн должен
// конвертироваться обратно в АБСОЛЮТНЫЙ wall-clock, иначе снапшот компакции
// (OpExpire) на реплее после рестарта даст бессмыслицу (монотонный эпох сброшен).
//
// Проверяем round-trip: ForEach(wall) → time.Until на «recovery» → ~исходный TTL.
func TestTTL_L4_ForEachEmitsWallClock(t *testing.T) {
	ev := newMockEvictor()
	clk := newFakeClock()
	m := newTTLManager(ev, clk.monoNow, clk.wallNow)
	defer m.Stop()

	// Прошло реального времени + был скачок часов — оба не должны исказить
	// абсолютный дедлайн, отдаваемый в WAL.
	clk.advance(30 * time.Minute)
	clk.stepWall(3 * time.Hour)
	m.Set("key", 1*time.Hour)

	var gotWall int64
	var seen int
	m.ForEach(func(key string, expiresAtUnixNano int64) {
		if key == "key" {
			gotWall = expiresAtUnixNano
			seen++
		}
	})
	if seen != 1 {
		t.Fatalf("ForEach должен отдать ключ ровно 1 раз, got %d", seen)
	}

	// Эмулируем recovery-сторону main.go: remaining = time.Until(expiresAt).
	// Здесь «сейчас» = текущее wall фейковых часов.
	remaining := gotWall - clk.wallNow().UnixNano()
	want := int64(1 * time.Hour)
	// Допуск на дрожание базовой пары (wall/mono снимаются подряд).
	if diff := remaining - want; diff < -int64(time.Second) || diff > int64(time.Second) {
		t.Fatalf("wall-дедлайн из ForEach даёт remaining=%v, ожидалось ~1h",
			time.Duration(remaining))
	}
}
