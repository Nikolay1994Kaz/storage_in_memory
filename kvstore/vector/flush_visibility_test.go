package vector

import (
	"fmt"
	"runtime"
	"sync"
	"testing"
)

// TestFlushVisibility_NoGapDuringBuild — страж flush-visibility gap.
//
// Дефект (pre-existing): handleCompactSignal свапает дельту (oldDelta → новая
// пустая) и строит сегмент АСИНХРОННО. Пока applyResult не опубликовал сегмент в
// levels, сфлашиваемые векторы не видны НИ в новой дельте, НИ в levels → Search
// транзиентно теряет свежие данные (false negative).
//
// Окно открывается ДЕТЕРМИНИРОВАННО через buildSem: numBuilders=1, тест захватывает
// единственный build-слот, поэтому freeze/build-горутина блокируется ПОСЛЕ свапа
// дельты, но ДО публикации сегмента. Ровно состояние gap — и оно пришпилено, пока
// тест держит токен. На баговом коде Search в этом окне вернёт пусто; фикс держит
// flushing-дельту searchable до публикации.
func TestFlushVisibility_NoGapDuringBuild(t *testing.T) {
	// dim=8 → freeze-on-flush (CSR); dim=300 (>csrDimThreshold) → rebuild из slab.
	// Оба пути держат сфлашиваемую дельту searchable до публикации сегмента.
	t.Run("freeze_dim8", func(t *testing.T) { checkNoGapDuringBuild(t, 8) })
	t.Run("rebuild_dim300", func(t *testing.T) { checkNoGapDuringBuild(t, 300) })
}

func checkNoGapDuringBuild(t *testing.T, dim int) {
	cfg := leveledConfigForVisibility()
	lvs := NewLeveledVectorStore(cfg)
	defer lvs.Clear()

	// Захватываем единственный build-слот → следующая build/freeze-горутина
	// заблокируется на buildSem после свапа дельты (окно gap пришпилено).
	lvs.buildSem <- struct{}{}

	query := mkVecN(dim, 0)
	if err := lvs.Add("k", query); err != nil {
		t.Fatalf("Add: %v", err)
	}

	// Просим coordinator сфлашить дельту. Он под write-lock свапнет дельту на пустую
	// и запустит freeze-горутину, которая тут же встанет на занятом buildSem.
	lvs.triggerCompact()

	// Ждём момента свапа: активная дельта опустела, но сегмент ещё не опубликован
	// (build пришпилен на buildSem). Это и есть окно gap.
	waitUntil(t, func() bool {
		lvs.mu.RLock()
		defer lvs.mu.RUnlock()
		deltaEmpty := lvs.delta == nil || lvs.delta.Len() == 0
		nSegs := 0
		for i := range lvs.levels {
			nSegs += len(lvs.levels[i])
		}
		return deltaEmpty && nSegs == 0 && lvs.inFlightBuilds.Load() > 0
	})

	// В окне gap Search обязан всё ещё находить "k" (он лишь переезжает delta→segment).
	res, err := lvs.Search(query, 1, nil)
	if err != nil {
		// Освобождаем слот, чтобы горутина не висела на shutdown.
		<-lvs.buildSem
		t.Fatalf("Search: %v", err)
	}
	found := false
	for _, r := range res {
		if r.Key == "k" {
			found = true
		}
	}

	// Освобождаем build-слот → freeze завершится и опубликует сегмент.
	<-lvs.buildSem

	if !found {
		t.Fatalf("flush-visibility gap: \"k\" невидим во время build (delta свапнута, сегмент не опубликован); res=%+v", res)
	}
}

// TestFlushVisibility_ConcurrentAddSearch — стохастический дозорный: под фоновым
// флашем Add→(немедленный)Search свежий ключ НИКОГДА не должен транзиентно исчезать.
// Векторы РАЗЛИЧНЫ (ключ i = уникальный вектор), поэтому свежий ключ — единственный
// точный сосед своего запроса (dist=0): его отсутствие в top-1 = именно gap, а не
// вытеснение тай-брейком. Search идёт БЕЗ FlushDeltaSync → часть запросов попадает
// ровно в окно между swap дельты и публикацией сегмента. Баговый код теряет ключ;
// исправленный держит flushing-дельту searchable.
func TestFlushVisibility_ConcurrentAddSearch(t *testing.T) {
	if testing.Short() {
		t.Skip("skip stress под -short")
	}
	const dim = 8
	cfg := leveledConfigForVisibility()
	lvs := NewLeveledVectorStore(cfg)
	defer lvs.Clear()

	var wg sync.WaitGroup
	const iters = 500

	// Фоновый флашер — гоняет дельту в сегменты, порождая окна gap.
	stop := make(chan struct{})
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
				lvs.triggerCompact()
				runtime.Gosched()
			}
		}
	}()

	vecFor := func(i int) []float32 { return mkVecN(dim, float32(i)+1) } // уникален на ключ

	var misses int
	for i := 0; i < iters; i++ {
		key := fmt.Sprintf("k%d", i)
		vec := vecFor(i)
		if err := lvs.Add(key, vec); err != nil {
			t.Fatalf("Add: %v", err)
		}
		// Немедленный поиск ключом-запросом — свежий ключ обязан быть top-1 (dist=0).
		res, err := lvs.Search(vec, 1, nil)
		if err != nil {
			t.Fatalf("Search: %v", err)
		}
		seen := false
		for _, r := range res {
			if r.Key == key {
				seen = true
			}
		}
		if !seen {
			misses++
		}
	}
	close(stop)
	wg.Wait()

	if misses > 0 {
		t.Fatalf("flush-visibility gap: %d/%d свежих ключей транзиентно исчезли из выдачи", misses, iters)
	}
}

func leveledConfigForVisibility() LeveledConfig {
	return LeveledConfig{
		Distance:       EuclideanDistance,
		DeltaMax:       1000,
		Fanout:         8,
		EfSearch:       64,
		M:              16,
		EfConstruction: 100,
		NumBuilders:    1, // единственный build-слот → детерминированное окно через buildSem
	}
}

func waitUntil(t *testing.T, cond func() bool) {
	t.Helper()
	for i := 0; i < 1_000_000; i++ {
		if cond() {
			return
		}
		runtime.Gosched()
	}
	t.Fatal("waitUntil: условие не наступило")
}
