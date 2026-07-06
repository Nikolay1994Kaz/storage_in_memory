package vector

import (
	"fmt"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"kvstore/kvstore/internal/monitoring"
)

// TestFlushDeltaSync_NoStormUnderConcurrentWrites — страж flush-storm.
//
// Дефект (06.07, найден soak-профилем): FlushDeltaSync крутился, пока дельта не
// станет ПУСТОЙ И inFlightBuilds==0 в один момент. Под непрерывной конкурентной
// записью дельта почти никогда не пуста на проверке → applyResult пере-триггерил
// flush на каждом build'е, дельта флашилась тысячи раз по 1-2 вектора, каждый flush
// аллоцировал свежий дельта-сегмент (~100MB апфронт до fix #2) → сотни ГБ churn.
//
// Фикс: FlushDeltaSync ждёт публикации лишь PRE-CALL набора дельт (target + уже
// флашащиеся), а не полного опустошения. Тест держит писателей активными ВСЁ время
// вызова: на баговом коде FlushDeltaSync не вернулся бы (спин до остановки записи),
// на исправленном — возвращается за единицы flush.
func TestFlushDeltaSync_NoStormUnderConcurrentWrites(t *testing.T) {
	const dim = 8
	cfg := leveledConfigForVisibility() // DeltaMax=1000, NumBuilders=1
	lvs := NewLeveledVectorStore(cfg)
	defer lvs.Clear()

	// Непрерывные писатели уникальными ключами — держат дельту постоянно непустой
	// (ровно то, на чём старый drain-to-empty спинил вечно).
	stop := make(chan struct{})
	var wg sync.WaitGroup
	var keyGen atomic.Int64
	var added atomic.Int64
	for w := 0; w < 4; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				id := keyGen.Add(1)
				if err := lvs.Add(fmt.Sprintf("k%d", id), mkVecN(dim, float32(id))); err == nil {
					added.Add(1)
				}
				runtime.Gosched()
			}
		}()
	}
	defer func() { close(stop); wg.Wait() }()

	// Дадим накопиться дельте.
	waitUntil(t, func() bool { return added.Load() > 200 })

	flushesBefore := int64(monitoring.VectorFlushDeltaTotal.Get())

	// FlushDeltaSync обязан ВЕРНУТЬСЯ, хотя писатели продолжают долбить (дельта не
	// пустеет). На баговом коде — busy-loop до таймаута.
	done := make(chan struct{})
	go func() { lvs.FlushDeltaSync(); close(done) }()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("FlushDeltaSync не вернулся за 10с под конкурентной записью — busy-loop/hang (fix flush-storm)")
	}

	flushesDuring := int64(monitoring.VectorFlushDeltaTotal.Get()) - flushesBefore

	// Anti-storm: один вызов под нагрузкой должен породить единицы flush, не тысячи.
	// Порог 50 с огромным запасом (норма — 1-2); баговый код давал сотни-тысячи.
	if flushesDuring > 50 {
		t.Fatalf("flush-storm: один FlushDeltaSync породил %d flush (ожидали единицы)", flushesDuring)
	}
	t.Logf("FlushDeltaSync вернулся, flush за вызов: %d (added≈%d)", flushesDuring, added.Load())
}

// TestFlushDeltaSync_FlushesPreCallVectors — контракт корректности: после возврата
// все векторы, добавленные ДО вызова, находятся в опубликованных сегментах (levels),
// т.е. дельта их больше не держит. Проверяем БЕЗ конкурентных писателей: single-swap
// обязан слить весь снимок дельты.
func TestFlushDeltaSync_FlushesPreCallVectors(t *testing.T) {
	const dim = 8
	cfg := leveledConfigForVisibility()
	lvs := NewLeveledVectorStore(cfg)
	defer lvs.Clear()

	const n = 300
	for i := 0; i < n; i++ {
		if err := lvs.Add(fmt.Sprintf("k%d", i), mkVecN(dim, float32(i)+1)); err != nil {
			t.Fatalf("Add: %v", err)
		}
	}

	lvs.FlushDeltaSync()

	// Дельта пуста (нет конкурентной записи) — все pre-call векторы в сегментах.
	lvs.mu.RLock()
	deltaLen := 0
	if lvs.delta != nil {
		deltaLen = lvs.delta.Len()
	}
	nSegs := 0
	for i := range lvs.levels {
		nSegs += len(lvs.levels[i])
	}
	lvs.mu.RUnlock()
	if deltaLen != 0 {
		t.Fatalf("после FlushDeltaSync без конкурентной записи дельта не пуста: Len=%d", deltaLen)
	}
	if nSegs == 0 {
		t.Fatalf("после FlushDeltaSync нет опубликованных сегментов (векторы потеряны?)")
	}

	// Каждый pre-call вектор должен находиться поиском (top-1 своего же вектора).
	for i := 0; i < n; i++ {
		vec := mkVecN(dim, float32(i)+1)
		res, err := lvs.Search(vec, 1, nil)
		if err != nil {
			t.Fatalf("Search: %v", err)
		}
		if len(res) == 0 || res[0].Key != fmt.Sprintf("k%d", i) {
			t.Fatalf("pre-call вектор k%d не найден после flush: res=%+v", i, res)
		}
	}
}
