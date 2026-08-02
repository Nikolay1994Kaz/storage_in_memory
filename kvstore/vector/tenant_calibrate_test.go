//go:build datasets

// Замер на внешнем датасете. Тег `datasets` намеренно держит его ВНЕ обычной
// сборки тестов: без файла в /tmp такой тест скипался молча, и «ok» пакета
// означал в том числе «тридцать функций даже не пытались запуститься».
// Запуск и получение данных — docs/BENCHMARKS.md, раздел Reproducing:
//
//	make test-datasets

package vector

import (
	"fmt"
	"math/rand"
	"strconv"
	"testing"
	"time"

	"kvstore/kvstore/internal/store/tcmalloc"
)

// =============================================================================
// Калибровка порога #6 (BruteThreshold) — открытый шаг #1 после Вещи 1.
//
// Вопрос: при каком АБСОЛЮТНОМ размере блока тенанта B линейный перебор (brute,
// O(B) по непрерывному диапазону) перестаёт выигрывать у HNSW-обхода всего
// сегмента с тенант-предикатом (graph-фильтр)? Точка пересечения латентностей и
// есть калиброванный BruteThreshold (DefaultBruteThreshold=16384 — стартовая
// догадка, здесь проверяется на реальной геометрии).
//
// Постановка, верная по памяти:
//   * РЕАЛЬНЫЙ SIFT-128 (случайные данные врут — vector-real-e2e-numbers).
//   * ОДИН консолидированный сегмент N=200k (фрагментация ломает честность brute;
//     консолидация — главный рычаг QPS, vector-segment-consolidation-lever).
//   * target-тенант — малая доля корпуса (реалистичная селективность фильтра);
//     фоновый тенант держит остальные ~99%, как у настоящего мультитенанта.
//   * оба пути меряются через те же примитивы, что зовёт прод SearchTenant:
//     bruteRange(range, tenantPred) и Search(ef, tenantPred).
// =============================================================================

// TestTenant_CalibrateBruteThreshold меряет кроссовер brute/graph по размеру блока.
// Запуск: go test ./kvstore/vector -run CalibrateBruteThreshold -v (НЕ -short).
func TestTenant_CalibrateBruteThreshold(t *testing.T) {
	if testing.Short() {
		t.Skip("long: строит 200k HNSW + sweep")
	}
	train, test, err := loadSIFTRaw("/tmp/sift200k.bin")
	if err != nil {
		t.Skipf("no dataset: %v (regen: scripts/convert_annbench.py sift-128-euclidean.hdf5 /tmp/sift200k.bin --train 200000 --test 500)", err)
	}

	const (
		dim = 128
		K   = 10
		ef  = 128 // прод EfSearch
	)

	// target-тенанты геометрически растущих размеров + фоновый тенант 0 на остаток.
	blockSizes := []int{500, 1000, 2000, 4000, 8000, 16000, 32000, 64000}
	totalTargets := 0
	for _, b := range blockSizes {
		totalTargets += b
	}
	if totalTargets >= len(train) {
		t.Fatalf("сумма блоков %d ≥ корпуса %d", totalTargets, len(train))
	}

	// Раскладка корпуса по тенантам реальными SIFT-векторами (последовательно).
	// tenant id: 1..len(blockSizes) — targets; 0 — background (остаток).
	vecsByTenant := make(map[uint64][][]float32)
	type kv struct {
		key string
		vec []float32
	}
	all := make([]kv, 0, len(train))
	off := 0
	for i, b := range blockSizes {
		tid := uint64(i + 1)
		for j := range b {
			v := train[off]
			off++
			vecsByTenant[tid] = append(vecsByTenant[tid], v)
			all = append(all, kv{strconv.FormatUint(tid, 10) + ":" + strconv.Itoa(j), v})
		}
	}
	for ; off < len(train); off++ { // фоновый тенант 0
		j := len(vecsByTenant[0])
		vecsByTenant[0] = append(vecsByTenant[0], train[off])
		all = append(all, kv{"0:" + strconv.Itoa(j), train[off]})
	}

	// Стор в тенант-режиме, ОДНА дельта → ОДИН консолидированный frozen-сегмент.
	cfg := LeveledConfig{
		Distance:       EuclideanDistance,
		Allocator:      tcmalloc.NewTCMallocStore(8),
		DeltaMax:       len(train) + 1, // без авто-flush: всё в одной дельте
		Fanout:         4,
		EfConstruction: arEfConstruction,
		EfSearch:       ef,
		TenantOf:       tenantOfKey,
		BruteThreshold: 1, // не важно: ранги читаем напрямую, Kind не используем
	}
	lvs := NewLeveledVectorStore(cfg)
	rng := rand.New(rand.NewSource(20260629))
	rng.Shuffle(len(all), func(i, j int) { all[i], all[j] = all[j], all[i] })
	tBuild := time.Now()
	for _, e := range all {
		if err := lvs.Add(e.key, e.vec); err != nil {
			t.Fatalf("Add: %v", err)
		}
	}
	settle(lvs)
	buildDur := time.Since(tBuild)

	// Достаём единственный консолидированный frozen-сегмент.
	lvs.mu.RLock()
	var fseg *frozenSegment
	nseg := 0
	for _, lvl := range lvs.levels {
		for _, s := range lvl {
			nseg++
			if fs, ok := s.(*frozenSegment); ok {
				fseg = fs
			}
		}
	}
	st := lvs.Stats()
	lvs.mu.RUnlock()
	if fseg == nil {
		t.Fatalf("нет frozenSegment (segs=%v)", st.SegmentsByLevel)
	}
	if nseg != 1 {
		t.Logf("ВНИМАНИЕ: сегментов %d, не 1 — калибровка предполагает консолидацию (segs=%v)",
			nseg, st.SegmentsByLevel)
	}
	if fseg.cat == nil {
		t.Fatal("frozenSegment без каталога — тенант-режим не сработал")
	}

	// nQ ограничен: graph-фильтр на крошечном тенанте (доля ~0.25%) патологически
	// медленный (HNSW бродит, ища K прошедших предикат) — это сам по себе сигнал в
	// пользу brute, но при 500×rounds запросов sweep длится десятки минут. 100
	// запросов достаточно для стабильного сравнения порядков латентности.
	nQ := min(100, len(test))
	queries := test[:nQ]
	dst := make([]FrozenResult, 0, K)

	fmt.Println()
	fmt.Printf("КАЛИБРОВКА BruteThreshold — РЕАЛЬНЫЙ SIFT-128, N=%d (1 сегмент), ef=%d, K=%d, build=%.1fs\n",
		st.TotalVectors, ef, K, buildDur.Seconds())
	fmt.Printf("%-9s %-12s %-12s %-9s %-9s %-9s\n",
		"block_B", "brute_us/q", "graph_us/q", "speedup", "brute_rec", "graph_rec")

	crossover := -1
	for i := range blockSizes {
		tid := uint64(i + 1)
		r, ok := fseg.cat.Lookup(tid)
		if !ok {
			t.Fatalf("тенант %d не в каталоге", tid)
		}
		B := r.Len()
		tenantPred := func(key string) bool { return tenantOfKey(key) == tid }
		gtVecs := vecsByTenant[tid]

		// GT считаем ОДИН раз вне тайминга через bounded top-K O(B·K) (НЕ tenantGT —
		// тот делает полную insertion-sort O(B²), для B=64k это секунды). Держать GT
		// в timed-цикле = мерить GT, а не search — ровно эта ошибка дала абсурд.
		pref := strconv.FormatUint(tid, 10) + ":"
		gtKeys := make([]map[string]struct{}, len(queries))
		for qi, q := range queries {
			top := make([]FrozenResult, 0, K)
			for id, v := range gtVecs {
				top = insertTopK(top, K, strconv.Itoa(id), EuclideanDistance(q, v))
			}
			m := make(map[string]struct{}, len(top))
			for _, r := range top {
				m[pref+r.Key] = struct{}{}
			}
			gtKeys[qi] = m
		}
		recallOf := func(res []FrozenResult, qi int) float64 {
			if len(gtKeys[qi]) == 0 {
				return 0
			}
			hit := 0
			for _, rr := range res {
				if _, ok := gtKeys[qi][rr.Key]; ok {
					hit++
				}
			}
			return float64(hit) / float64(len(gtKeys[qi]))
		}

		// brute: O(B) по диапазону [Start,End) (прод передаёт tenantPred — избыточно, #2).
		// Тайминг — ТОЛЬКО search; recall считаем отдельным untimed-проходом.
		tb := time.Now()
		for _, q := range queries {
			_ = fseg.fg.bruteRange(q, K, r.Start, r.End, dst[:0], tenantPred)
		}
		bruteUs := float64(time.Since(tb).Microseconds()) / float64(len(queries))
		var bruteRec float64
		for qi, q := range queries {
			bruteRec += recallOf(fseg.fg.bruteRange(q, K, r.Start, r.End, dst[:0], tenantPred), qi)
		}
		bruteRec /= float64(len(queries))

		// graph: HNSW-обход всего сегмента с тенант-предикатом.
		tg := time.Now()
		for _, q := range queries {
			_ = fseg.fg.Search(q, K, ef, dst[:0], tenantPred)
		}
		graphUs := float64(time.Since(tg).Microseconds()) / float64(len(queries))
		var graphRec float64
		for qi, q := range queries {
			graphRec += recallOf(fseg.fg.Search(q, K, ef, dst[:0], tenantPred), qi)
		}
		graphRec /= float64(len(queries))

		fmt.Printf("%-9d %-12.1f %-12.1f %-9.2f %-9.3f %-9.3f\n",
			B, bruteUs, graphUs, graphUs/bruteUs, bruteRec, graphRec)
		if crossover == -1 && bruteUs > graphUs {
			crossover = B
		}
	}

	fmt.Println("---")
	if crossover == -1 {
		fmt.Printf("ВЫВОД: brute быстрее graph на ВСЁМ диапазоне до %d → BruteThreshold ≥ %d\n",
			blockSizes[len(blockSizes)-1], blockSizes[len(blockSizes)-1])
	} else {
		fmt.Printf("ВЫВОД: кроссовер около B=%d (brute дороже graph выше него) → BruteThreshold ≈ %d\n",
			crossover, crossover)
	}
	fmt.Printf("(текущий DefaultBruteThreshold=%d)\n", DefaultBruteThreshold)
}
