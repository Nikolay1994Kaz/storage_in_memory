package vector

import (
	"fmt"
	"math/rand"
	"slices"
	"testing"
	"time"
)

// =============================================================================
// Профит-бенч шага 4 (спайк «c», пороги назначены ДО прогона, N=5 — Николай):
//   порог 1: p99 REMEMBER-supersedes ≤ 5× p99 обычного REMEMBER
//            (честный худший случай: цель во frozen-сегменте, термы цели
//            восстанавливаются точечной выемкой из инвертированных постингов);
//   порог 2: регресс RECALL строго ноль — диф не трогает путей чтения,
//            проверяется A/B-прогоном TestVMEMRecallQPSProbe на master vs
//            спайке (пробник намеренно совместим с сигнатурами обеих сторон).
// Провал порога 1 → дверь «b» (слой валидности tombstone-образца).
// =============================================================================

// vmemSynthText — синтетический текст факта: zipf-ский словарь vocab слов,
// nTok токенов. Детерминированный rng — прогоны воспроизводимы.
func vmemSynthText(rng *rand.Rand, vocab, nTok int) string {
	buf := make([]byte, 0, nTok*8)
	for i := 0; i < nTok; i++ {
		if i > 0 {
			buf = append(buf, ' ')
		}
		// zipf-подобное смещение к частым словам: квадрат равномерной
		w := int(float64(vocab) * rng.Float64() * rng.Float64())
		buf = append(buf, fmt.Sprintf("word%05d", w)...)
	}
	return string(buf)
}

// vmemBenchP99 — p99 латентность op по nOps прогонов, наносекунды.
func vmemBenchP99(nOps int, op func(i int)) (p50, p99 time.Duration) {
	lat := make([]time.Duration, nOps)
	for i := 0; i < nOps; i++ {
		t0 := time.Now()
		op(i)
		lat[i] = time.Since(t0)
	}
	slices.Sort(lat)
	return lat[nOps/2], lat[nOps*99/100]
}

// TestSupersedeLatencyProfit — порог 1 спайка. Корпус фактов уезжает во
// frozen-сегмент, обычный REMEMBER и REMEMBER-supersedes (цель — случайный
// frozen-факт, каждый раз новый) меряются в одинаковых условиях.
func TestSupersedeLatencyProfit(t *testing.T) {
	if testing.Short() {
		t.Skip("профит-бенч: только полный прогон")
	}
	const (
		corpusN = 20000 // фактов во frozen-сегменте
		vocab   = 20000 // словарь → размер инвертированного слоя
		nTok    = 30    // токенов на факт
		nOps    = 1000  // замеров на операцию (p99 = 10 худших)
	)
	cfg := bm25TestConfig()
	cfg.DeltaMax = corpusN + 4*nOps // бенч не должен споткнуться о flush
	lvs := NewLeveledVectorStore(cfg)
	defer lvs.Close()

	rng := rand.New(rand.NewSource(42))
	for i := 0; i < corpusN; i++ {
		req := RememberRequest{
			ID:    fmt.Sprintf("fact:%05d", i),
			Scope: fmt.Sprintf("user:%03d", i%100),
			Text:  vmemSynthText(rng, vocab, nTok),
		}
		if _, err := lvs.Remember(req, 1000); err != nil {
			t.Fatalf("Remember корпуса: %v", err)
		}
	}
	lvs.FlushDeltaSync() // корпус (и все цели) — во frozen-слое

	plainP50, plainP99 := vmemBenchP99(nOps, func(i int) {
		req := RememberRequest{
			ID:    fmt.Sprintf("plain:%04d", i),
			Scope: fmt.Sprintf("user:%03d", i%100),
			Text:  vmemSynthText(rng, vocab, nTok),
		}
		if _, err := lvs.Remember(req, 2000); err != nil {
			t.Fatalf("plain Remember: %v", err)
		}
	})

	perm := rng.Perm(corpusN)[:nOps] // каждой замене — свежая frozen-цель
	supP50, supP99 := vmemBenchP99(nOps, func(i int) {
		target := perm[i]
		req := RememberRequest{
			ID:         fmt.Sprintf("sup:%04d", i),
			Scope:      fmt.Sprintf("user:%03d", target%100),
			Text:       vmemSynthText(rng, vocab, nTok),
			Supersedes: fmt.Sprintf("fact:%05d", target),
		}
		if _, err := lvs.Remember(req, 3000); err != nil {
			t.Fatalf("supersede Remember: %v", err)
		}
	})

	ratio := float64(supP99) / float64(plainP99)
	t.Logf("REMEMBER plain:      p50=%v p99=%v", plainP50, plainP99)
	t.Logf("REMEMBER supersedes: p50=%v p99=%v (цель во frozen, %d доков, словарь %d)", supP50, supP99, corpusN, vocab)
	t.Logf("отношение p99: %.2f× (порог ≤5×)", ratio)
	if ratio > 5 {
		t.Fatalf("порог 1 провален: p99 supersedes = %.2f× обычного REMEMBER (>5×) — дверь «b»", ratio)
	}
}

// TestVMEMRecallQPSProbe — пробник порога 2 (регресс RECALL = 0): меряет QPS
// Recall на корпусе с историей замен. НАМЕРЕННО совместим с сигнатурами и
// master (шаг 3: Remember без валидации цели), и спайка — один и тот же файл
// кладётся в оба worktree, A/B сравнивает числа одного пробника.
func TestVMEMRecallQPSProbe(t *testing.T) {
	if testing.Short() {
		t.Skip("профит-бенч: только полный прогон")
	}
	const (
		corpusN = 20000
		vocab   = 20000
		nTok    = 30
		nQ      = 40000 // фаза запросов ≫ шума планировщика: ~0.5–0.8с
	)
	cfg := bm25TestConfig()
	cfg.DeltaMax = corpusN
	lvs := NewLeveledVectorStore(cfg)
	defer lvs.Close()

	rng := rand.New(rand.NewSource(7))
	// Половина фактов — «заменённые»: у чётных есть наследник с supersedes.
	// На master закрытия интервала не происходит (шаг 3), на спайке — да;
	// пробник меряет ЧТЕНИЕ, различие корпусов и есть предмет замера:
	// закрытые интервалы не должны стоить чтению ничего.
	for i := 0; i < corpusN; i++ {
		id := fmt.Sprintf("fact:%05d", i)
		req := RememberRequest{ID: id, Scope: fmt.Sprintf("user:%03d", i%100), Text: vmemSynthText(rng, vocab, nTok)}
		if _, err := lvs.Remember(req, 1000); err != nil {
			t.Fatalf("Remember: %v", err)
		}
		if i%2 == 0 {
			heir := RememberRequest{
				ID:         id + ":v2",
				Scope:      req.Scope,
				Text:       vmemSynthText(rng, vocab, nTok),
				Supersedes: id,
			}
			if _, err := lvs.Remember(heir, 1500); err != nil {
				t.Fatalf("Remember наследника: %v", err)
			}
		}
	}
	lvs.FlushDeltaSync()

	queries := make([]RecallRequest, nQ)
	for i := range queries {
		queries[i] = RecallRequest{
			Scope: fmt.Sprintf("user:%03d", rng.Intn(100)),
			Query: vmemSynthText(rng, vocab, 4),
			K:     10,
		}
	}
	t0 := time.Now()
	hits := 0
	for _, q := range queries {
		res, err := lvs.Recall(q, 2000)
		if err != nil {
			t.Fatalf("Recall: %v", err)
		}
		hits += len(res)
	}
	el := time.Since(t0)
	t.Logf("Recall QPS: %.0f (%d запросов за %v, %d хитов)", float64(nQ)/el.Seconds(), nQ, el, hits)
}
