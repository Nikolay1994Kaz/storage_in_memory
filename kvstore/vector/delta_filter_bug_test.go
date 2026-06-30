package vector

import (
	"fmt"
	"math/rand"
	"strconv"
	"testing"

	"kvstore/kvstore/internal/store/tcmalloc"
)

// =============================================================================
// ОБНАЖАЮЩИЙ ТЕСТ: post-filter в ДЕЛЬТЕ ([[vector-delta-filter-postfilter-bug]]).
//
// Гипотеза: deltaShard.search (delta.go) делает Graph.Search БЕЗ фильтра → обрезает
// до топ-K по расстоянию → ПОТОМ выкидывает непрошедших фильтр (post-filter). При
// селективном фильтре из K кандидатов выживает ~K×selectivity → recall обваливается.
// Frozen-путь (frozen.go searchWithIdx) фильтрует ВО ВРЕМЯ обхода → recall держится.
//
// Дизайн: ОДНИ данные, ОДИН селективный фильтр (id%8==0, selectivity 12.5%), две фазы:
//   фаза DELTA  — без флаша, всё в живой дельте (nSegs==0)         → ожидаем НИЗКИЙ recall
//   фаза FROZEN — settle(), всё консолидировано во frozen (delta пуста) → ожидаем ~1.0
// Контраст между фазами = доказательство, что баг именно в дельта-пути.
//
// Синтетика намеренно (баг структурный, не зависит от геометрии); быстро и без
// внешних данных. Эталон recall — точный фильтрованный top-K (filterGTByPred).
//
//   go test ./kvstore/vector -run TestDelta_FilterPostFilterBug -v
// =============================================================================

func TestDelta_FilterPostFilterBug(t *testing.T) {
	const (
		N   = 5000
		dim = 64
		K   = 10
		ef  = 128
		nQ  = 100
		sel = 8 // фильтр id%sel==0 → selectivity 1/8 = 12.5%
	)

	rng := rand.New(rand.NewSource(20260630))
	train := make([][]float32, N)
	for i := range train {
		v := make([]float32, dim)
		for d := range v {
			v[d] = rng.Float32()
		}
		train[i] = v
	}
	queries := make([][]float32, nQ)
	for i := range queries {
		v := make([]float32, dim)
		for d := range v {
			v[d] = rng.Float32()
		}
		queries[i] = v
	}

	matchId := func(i int) bool { return i%sel == 0 }
	strPred := func(key string) bool { i, _ := strconv.Atoi(key); return matchId(i) }
	nPass := 0
	for i := 0; i < N; i++ {
		if matchId(i) {
			nPass++
		}
	}
	gt := filterGTByPred(train, queries, K, matchId)

	cfg := LeveledConfig{
		Distance:       EuclideanDistance,
		Allocator:      tcmalloc.NewTCMallocStore(4),
		DeltaMax:       N + 1, // дельта НЕ переполняется → авто-флаша нет
		Fanout:         4,
		EfConstruction: arEfConstruction,
		EfSearch:       ef,
	}
	lvs := NewLeveledVectorStore(cfg)
	for i, v := range train {
		if err := lvs.Add(strconv.Itoa(i), v); err != nil {
			t.Fatalf("Add: %v", err)
		}
	}

	segCount := func() int {
		n := 0
		for _, c := range lvs.Stats().SegmentsByLevel {
			n += c
		}
		return n
	}

	fmt.Println()
	fmt.Printf("DELTA POST-FILTER BUG — N=%d dim=%d K=%d ef=%d, фильтр id%%%d==0 (#pass=%d, sel=%.1f%%)\n",
		N, dim, K, ef, sel, nPass, 100.0/float64(sel))

	// ── ФАЗА DELTA: всё в живой дельте, без флаша ──
	if got := segCount(); got != 0 {
		t.Fatalf("ожидали 0 frozen-сегментов (всё в дельте), получили %d", got)
	}
	recDelta := recallStorePred(queries, gt, K, func(q []float32) []VSearchResult {
		r, _ := lvs.SearchFiltered(q, K, strPred, nil)
		return r
	})
	fmt.Printf("  DELTA  (segs=0, всё в дельте):   recall@%d = %.3f\n", K, recDelta)

	// ── ФАЗА FROZEN: консолидируем во frozen, дельта пустеет ──
	settle(lvs)
	if got := segCount(); got == 0 {
		t.Fatalf("ожидали ≥1 frozen-сегмент после settle, получили 0")
	}
	recFrozen := recallStorePred(queries, gt, K, func(q []float32) []VSearchResult {
		r, _ := lvs.SearchFiltered(q, K, strPred, nil)
		return r
	})
	fmt.Printf("  FROZEN (segs=%d, дельта пуста):   recall@%d = %.3f\n", segCount(), K, recFrozen)

	// ── ФАЗА MIXED: batch1 во frozen + batch2 в живой дельте (merge-путь) ──
	const M = 2000
	all := make([][]float32, N+M)
	copy(all, train)
	for i := N; i < N+M; i++ {
		v := make([]float32, dim)
		for d := range v {
			v[d] = rng.Float32()
		}
		all[i] = v
		if err := lvs.Add(strconv.Itoa(i), v); err != nil { // без settle → в дельту
			t.Fatalf("Add batch2: %v", err)
		}
	}
	gtMixed := filterGTByPred(all, queries, K, matchId) // GT над ВСЕМИ (frozen+delta)
	recMixed := recallStorePred(queries, gtMixed, K, func(q []float32) []VSearchResult {
		r, _ := lvs.SearchFiltered(q, K, strPred, nil)
		return r
	})
	fmt.Printf("  MIXED  (frozen %d + дельта %d):   recall@%d = %.3f\n", N, M, K, recMixed)
	fmt.Printf("  Δ(frozen-delta_было_до_фикса) = %.3f\n", recFrozen-recDelta)

	// После фикса (filtered brute на дельте) дельта-путь и merge-путь должны держать
	// recall на уровне frozen. Регресс-страж: если дельта снова станет post-filter,
	// recDelta/recMixed обвалятся ~до selectivity (1/sel) и тест упадёт.
	const tol = 0.05
	if recDelta < recFrozen-tol {
		t.Errorf("РЕГРЕСС дельта-фильтра: recall %.3f << frozen %.3f (post-filter вернулся?)", recDelta, recFrozen)
	}
	if recMixed < recFrozen-tol {
		t.Errorf("РЕГРЕСС merge-пути: mixed recall %.3f << frozen %.3f", recMixed, recFrozen)
	}
	if recDelta >= recFrozen-tol && recMixed >= recFrozen-tol {
		fmt.Println("  → OK: дельта-фильтр и merge-путь держат recall на уровне frozen (баг закрыт).")
	}
}
