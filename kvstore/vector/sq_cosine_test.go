package vector

import (
	"math/rand"
	"sort"
	"strconv"
	"testing"

	"kvstore/kvstore/internal/store/tcmalloc"
)

// TestSQ8_CosineDotModeRecall — регрессия на dot-ADC режим SQ8 (cosine/dot метрика).
//
// Два бага, найденные валидацией на реальных эмбеддингах (dbpedia ada-002, cosine):
//  1. (ИСТОРИЧЕСКИ) детекция dot-режима шла сравнением funcval-указателя с
//     внутренним dotProductImpl → ВСЕГДА false → SQ8 уходил в euclidean-ADC.
//     Класс багов устранён: метрика теперь first-class enum (Metric), dot-режим =
//     Metric.IsDot(), а legacy-вывод из DistanceFunc централизован в inferMetric.
//  2. dot-режим sqApproxDist возвращал СХОДСТВО (q·approx), а обход ждёт РАССТОЯНИЕ
//     → инверсия порядка → recall=0.
//
// Тест защёлкивает оба: dot-режим включён И recall высокий (не ноль).
func TestSQ8_CosineDotModeRecall(t *testing.T) {
	// Защёлка #1: dot-метрики дают dot-режим, euclidean — нет (через enum и мост).
	if !MetricDot.IsDot() || !MetricCosine.IsDot() {
		t.Fatal("MetricDot/MetricCosine.IsDot()=false — SQ8 уйдёт в euclidean-ADC на cosine")
	}
	if MetricEuclidean.IsDot() {
		t.Fatal("MetricEuclidean.IsDot()=true — euclidean ошибочно считается dot")
	}
	if inferMetric(DotProductDistance) != MetricDot || inferMetric(CosineDistance) != MetricCosine {
		t.Fatal("inferMetric не распознаёт публичные dot/cosine обёртки")
	}
	if inferMetric(EuclideanDistance) != MetricEuclidean {
		t.Fatal("inferMetric(EuclideanDistance) != MetricEuclidean")
	}

	const (
		dim = 64
		N   = 3000
		K   = 10
	)
	rng := rand.New(rand.NewSource(20260629))
	mkVec := func() []float32 {
		v := make([]float32, dim)
		for d := range v {
			v[d] = rng.Float32()*2 - 1
		}
		Normalize(v) // cosine-контракт: нормализованный вход
		return v
	}
	train := make([][]float32, N)
	for i := range train {
		train[i] = mkVec()
	}
	queries := make([][]float32, 100)
	for i := range queries {
		queries[i] = mkVec()
	}

	// Точный GT по DotProductDistance (1-dot) над train.
	gt := make([][]int, len(queries))
	for qi, q := range queries {
		type cand struct {
			id int
			d  float32
		}
		cs := make([]cand, N)
		for i, v := range train {
			cs[i] = cand{i, DotProductDistance(q, v)}
		}
		sort.Slice(cs, func(a, b int) bool { return cs[a].d < cs[b].d })
		ids := make([]int, K)
		for k := 0; k < K; k++ {
			ids[k] = cs[k].id
		}
		gt[qi] = ids
	}

	cfg := LeveledConfig{
		Distance:  DotProductDistance,
		Allocator: tcmalloc.NewTCMallocStore(4),
		DeltaMax:  N + 1, // один SQ-сегмент
		Fanout:    4,
		EfSearch:  128,
		UseSQ:     true,
	}
	lvs := NewLeveledVectorStore(cfg)
	for i, v := range train {
		if err := lvs.Add(strconv.Itoa(i), v); err != nil {
			t.Fatalf("Add: %v", err)
		}
	}
	settle(lvs)

	var recall float64
	for qi, q := range queries {
		res, _ := lvs.Search(q, K, nil)
		want := make(map[int]struct{}, K)
		for _, id := range gt[qi] {
			want[id] = struct{}{}
		}
		hit := 0
		for _, r := range res {
			id, _ := strconv.Atoi(r.Key)
			if _, ok := want[id]; ok {
				hit++
			}
		}
		recall += float64(hit) / float64(K)
	}
	recall /= float64(len(queries))

	// Защёлка #2: recall высокий. До фикса dot-инверсии тут было ~0.
	if recall < 0.85 {
		t.Fatalf("SQ8 cosine recall@%d=%.3f < 0.85 — dot-ADC сломан (инверсия?)", K, recall)
	}
	t.Logf("SQ8 cosine recall@%d=%.3f (dot-ADC корректен)", K, recall)
}
