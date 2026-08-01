package vector

// =============================================================================
// Сторож пропускной способности вставки — на ШАРДИРОВАННОЙ дельте.
//
// ЗАЧЕМ ОТДЕЛЬНЫЙ ТЕСТ. Порог скорости раньше жил в TestLeveledStore_500k и
// противоречил его же конфигурации: там DeltaShards не задан, все писатели
// строят один hnsw-граф по очереди, и порог мерил худший из возможных путей
// вставки. Убрав его оттуда, надо было поставить сторожа туда, где он осмыслен.
//
// ⭐ПОЧЕМУ ОТНОШЕНИЕ, А НЕ АБСОЛЮТНОЕ ЧИСЛО. Тот же вывод, что и в
// snapshot_hnsw_seal_cost_bench_test.go: абсолютный порог пропускной
// способности меряет железо пополам с кодом и падает от посторонней нагрузки.
// Отношение «шардированная вставка / односегментная» устойчиво: обе руки едут
// на одной машине в одном прогоне. Дефект, который тут ловится, — возвращение
// глобальной сериализации на путь вставки; она обрушит отношение к единице, а
// не подвинет его на проценты.
//
// ⭐ПОЧЕМУ БЕЗ ВНЕШНИХ ДАННЫХ. Родственный TestStep5_ShardedAddScaling меряет то
// же на MNIST-784 — и МОЛЧА СКИПАЕТСЯ, если /tmp/mnist784.bin нет, а его там
// нет. Пропущенный тест в выводе неотличим от пройденного, то есть свойство
// шесть недель никто не стерёг. Здесь векторы синтетические, поэтому тест
// исполняется ВСЕГДА и не загейтован -short: он идёт в CI.
//
// Измерено 01.08 на dim=128 (12 писателей, 12 ядер):
//
//	DeltaShards=1  →   465 вект/с   (100k, с флашами)
//	DeltaShards=12 →  9105 вект/с
//	                   ⇒ 19.6×
//
// Порог взят 3.0× — вчетверо ниже измеренного и ниже канона из справки флага
// -delta-shards (~4.25× при 12 шардах на MNIST-784). Запас нужен потому, что
// отношение зависит от размерности и от того, доходит ли дело до флашей.
// =============================================================================

import (
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

const (
	// Размер подобран так, чтобы ОБЕ руки уложились в бюджет CI (у vector 5
	// минут, весь пакет сейчас ~48 с): медленная рука здесь и определяет время.
	shardScaleN   = 6_000
	shardScaleDim = 128

	// Отношение шардированной вставки к односегментной.
	shardScaleMinRatio = 3.0
)

// shardScaleInsert — вставка n векторов writers горутинами при заданном числе
// шардов дельты. Возвращает скорость и число векторов, реально осевших в сторе.
func shardScaleInsert(t *testing.T, shards, n, writers int, vecs [][]float32) (float64, int) {
	t.Helper()
	lvs := NewLeveledVectorStore(LeveledConfig{
		DeltaMax:       0, // авто, как в проде
		EfConstruction: 200,
		M:              32,
		EfSearch:       100,
		Distance:       EuclideanDistance,
		NumBuilders:    writers,
		Fanout:         4,
		UseSQ:          true,
		DeltaShards:    shards, // ← единственная разница между руками
	})
	defer lvs.Close()

	if got := lvs.deltaShardCount(); got != max(shards, 1) {
		t.Fatalf("шардов %d, ожидалось %d — руки сравнения не различаются, "+
			"отношение ниже вышло бы ≈1 по неверной причине", got, shards)
	}

	var wg sync.WaitGroup
	var failed atomic.Bool
	chunk := n / writers
	start := time.Now()
	for w := 0; w < writers; w++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			lo, hi := id*chunk, (id+1)*chunk
			if id == writers-1 {
				hi = n
			}
			for i := lo; i < hi; i++ {
				if err := lvs.Add(fmt.Sprintf("key-%d", i), vecs[i]); err != nil {
					failed.Store(true)
					return
				}
			}
		}(w)
	}
	wg.Wait()
	elapsed := time.Since(start)
	if failed.Load() {
		t.Fatalf("Add вернул ошибку при shards=%d", shards)
	}
	lvs.FlushDeltaSync()
	return float64(n) / elapsed.Seconds(), lvs.Stats().TotalVectors
}

// TestShardedInsertScaling — шардирование дельты обязано давать кратный выигрыш
// на вставке. Обрушение отношения к единице означает, что писатели снова
// сериализуются на общем локе.
func TestShardedInsertScaling(t *testing.T) {
	const writers = 12
	vecs := makeRandVecs(shardScaleN, shardScaleDim, 42)

	// Порядок рук: медленная первой — если она упадёт, быстрая не успеет
	// «доказать» отношение делением на мусор.
	oneRate, oneTotal := shardScaleInsert(t, 1, shardScaleN, writers, vecs)
	manyRate, manyTotal := shardScaleInsert(t, writers, shardScaleN, writers, vecs)

	t.Logf("DeltaShards=1  → %7.0f вект/с (осело %d)", oneRate, oneTotal)
	t.Logf("DeltaShards=%d → %7.0f вект/с (осело %d)", writers, manyRate, manyTotal)

	// КОНТРОЛЬ ДИАГНОЗА: обе руки обязаны сохранить все векторы. Рука,
	// потерявшая половину, «выиграла» бы по скорости, и отношение оказалось бы
	// зелёным ровно за счёт дефекта, который тест обязан ловить.
	if oneTotal != shardScaleN || manyTotal != shardScaleN {
		t.Fatalf("потеря векторов: 1 шард=%d, %d шардов=%d, ожидалось по %d",
			oneTotal, writers, manyTotal, shardScaleN)
	}
	if oneRate <= 0 {
		t.Fatal("односегментный замер пуст — делить не на что")
	}

	ratio := manyRate / oneRate
	t.Logf("⇒ шардирование даёт %.2f× на вставке (порог %.1f×)", ratio, shardScaleMinRatio)

	if ratio < shardScaleMinRatio {
		t.Errorf("шардирование даёт всего %.2f× при пороге %.1f× — похоже, писатели "+
			"снова сериализуются на общем локе (ожидалось ~19× на dim=%d)",
			ratio, shardScaleMinRatio, shardScaleDim)
	}
}
