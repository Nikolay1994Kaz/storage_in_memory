package zset

import (
	"sync"
	"testing"

	"kvstore/kvstore/internal/store/tcmalloc"
)

// TestZAdd_ConcurrentSameMember_NoDuplicates — страж S3.
//
// ZAdd/ZRem — check-then-act (Get(__zidx) → DeleteMember → Insert → Set). Без
// per-set сериализации два конкурентных ZAdd одного member оба читают один
// oldScore, оба DeleteMember(old) (один впустую) и оба Insert(разный new) →
// ДУБЛЬ в дереве + сирота в обратном индексе (ZCARD раздут, призрак в range).
//
// Каждая горутина использует свой workerID (в проде ZADD одного member
// приходит с РАЗНЫХ соединений/воркеров) — иначе поймали бы отдельную гонку
// single-writer mcache, а не S3.
//
// Тест на ОДИН set: per-set мьютекс сериализует все ZAdd этого set, поэтому и
// логика RMW, и аллокации B+Tree-узлов безопасны. (Cross-set alloc-гонка
// caches[0] — отдельная известная проблема «ключи не партиционированы по
// воркерам», требует выделенного cache; здесь не проверяется.)
func TestZAdd_ConcurrentSameMember_NoDuplicates(t *testing.T) {
	const (
		g     = 8
		iters = 500
	)
	store := tcmalloc.NewTCMallocStore(g)
	defer store.Close()
	reg := New(store)

	// Пред-создаём set+member, чтобы getOrCreate шёл по lock-free Load-пути
	// (иначе конкурентный первый ZAdd словил бы отдельную create-гонку btree.New).
	reg.ZAdd(0, "s", 0, "m")

	var wg sync.WaitGroup
	for w := 0; w < g; w++ {
		wg.Add(1)
		go func(worker int) {
			defer wg.Done()
			for i := 0; i < iters; i++ {
				// Разные score → каждый апдейт реально делает DeleteMember+Insert.
				score := float64(worker*iters + i + 1)
				reg.ZAdd(worker, "s", score, "m")
			}
		}(w)
	}
	wg.Wait()

	if n := reg.ZCard("s"); n != 1 {
		t.Fatalf("S3: ZCard=%d, want 1 — конкурентный ZAdd одного member создал дубли в дереве", n)
	}
	got := reg.ZRangeByScore("s", -1e18, 1e18)
	if len(got) != 1 {
		t.Fatalf("S3: range вернул %d записей для одного member, want 1 (призраки/сироты)", len(got))
	}
	sc, ok := reg.ZScore("s", "m")
	if !ok {
		t.Fatal("S3: ZScore не нашёл member — рассинхрон обратного индекса")
	}
	if got[0].Member != "m" || got[0].Score != sc {
		t.Fatalf("S3: рассинхрон дерево↔обратный индекс: range=(%q,%v) zscore=%v",
			got[0].Member, got[0].Score, sc)
	}
}
