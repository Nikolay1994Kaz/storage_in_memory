package vector

// Тесты шага 6 BM25-спринта: merge пересобирает текстовый слой декодом
// постингов (decodeTerms → entries.Terms → buildSegmentText). Пиннят и
// семантику: компакция нормализует статистику (df/N/avgdl), затенённые
// копии и tombstoned-доки исчезают из слоя физически.

import (
	"math"
	"testing"
	"time"

	"kvstore/kvstore/internal/store/tcmalloc"
)

// grabL0 снимает сегменты уровня 0 (oldest→newest) для прямого merge.
// Точное число сегментов не фиксируем: ре-триггер компакции после публикации
// может нарезать те же доки на больше сегментов (перераспределение не меняет
// суммарную статистику) — проверяем лишь минимум.
func grabL0(t *testing.T, lvs *LeveledVectorStore, atLeast int) []segment {
	t.Helper()
	lvs.mu.RLock()
	segs := append([]segment(nil), lvs.levels[0]...)
	lvs.mu.RUnlock()
	if len(segs) < atLeast {
		t.Fatalf("ожидали ≥%d сегментов в L0, получили %d", atLeast, len(segs))
	}
	return segs
}

// TestBM25MergeKeepsText — E2E через idle-консолидацию: корпус двумя flush'ами
// → фоновый merge всех сегментов в один → golden сходится, текст не потерян.
func TestBM25MergeKeepsText(t *testing.T) {
	corpus, golden := loadBM25Golden(t)

	cfg := bm25TestConfig()
	cfg.IdleConsolidateAfter = 100 * time.Millisecond
	lvs := NewLeveledVectorStore(cfg)
	defer lvs.Clear()

	split := len(corpus.Docs) / 2
	bm25AddCorpus(t, lvs, corpus, 0, split, 8)
	lvs.FlushDeltaSync()
	bm25AddCorpus(t, lvs, corpus, split, len(corpus.Docs), 8)
	lvs.FlushDeltaSync()

	if !waitForState(t, lvs, 5*time.Second, func(st LeveledStats) bool {
		return st.DeltaLen == 0 && segCount(st) == 1
	}) {
		t.Fatal("idle-консолидация не собрала сегменты в один")
	}
	bm25AssertGolden(t, lvs, corpus, golden)
}

// TestBM25MergeNormalizesDF — трасса из разбора: upsert-фантом завышает df до
// компакции (принятая семантика, пиннится тестом шага 4), merge его физически
// выметает — df/N/avgdl возвращаются к правде, скор становится «честным» ln2.
func TestBM25MergeNormalizesDF(t *testing.T) {
	lvs := NewLeveledVectorStore(bm25TestConfig())
	defer lvs.Clear()

	if err := lvs.AddDoc("doc", mkVecN(8, 1), Attributes{}, "чай чай чай"); err != nil {
		t.Fatal(err)
	}
	lvs.FlushDeltaSync() // сегмент A: фантомная копия doc (tf=3, dl=3)
	if err := lvs.AddDoc("doc", mkVecN(8, 2), Attributes{}, "чай"); err != nil {
		t.Fatal(err)
	}
	if err := lvs.AddDoc("f2", mkVecN(8, 3), Attributes{}, "кофе"); err != nil {
		t.Fatal(err)
	}
	lvs.FlushDeltaSync() // сегмент B: свежий doc (tf=1, dl=1) + f2

	tea := bm25Tokenize("чай")[0]

	// До merge: обе копии в постингах → df=2, N=3, totalLen=5 —
	// независимо от того, на сколько сегментов их нарезал flush.
	segs := grabL0(t, lvs, 2)
	var dfBefore, nBefore, lenBefore uint64
	for _, s := range segs {
		st := s.Text()
		dfBefore += st.df(tea)
		nBefore += uint64(st.nText)
		lenBefore += st.totalLen
	}
	if dfBefore != 2 || nBefore != 3 || lenBefore != 5 {
		t.Fatalf("до merge: df=%d N=%d len=%d, ждём 2/3/5 (фантом должен считаться)", dfBefore, nBefore, lenBefore)
	}

	merged, _ := lvs.mergeSegmentsWithAllocator(segs, tcmalloc.NewTCMallocStore(1))
	if merged == nil {
		t.Fatal("merge вернул nil")
	}
	st := merged.Text()
	if st == nil {
		t.Fatal("merge потерял текстовый слой")
	}

	// После merge: фантом выметен — df=1, N=2, totalLen=2, avgdl=1.
	if st.df(tea) != 1 || st.nText != 2 || st.totalLen != 2 {
		t.Fatalf("после merge: df=%d N=%d len=%d, ждём 1/2/2", st.df(tea), st.nText, st.totalLen)
	}

	// Скор doc по «чай» на чистой статистике = IDF = ln2 (TF-часть ровно 1).
	idf := []float64{bm25IDF(uint64(st.nText), st.df(tea))}
	hits := st.search([]string{tea}, idf, 1.0, 0, uint32(len(st.docLen)), 10)
	if len(hits) != 1 {
		t.Fatalf("ждём 1 хит по «чай», получили %d", len(hits))
	}
	if math.Abs(hits[0].score-math.Ln2) > bm25ScoreEps {
		t.Errorf("скор %.6f, ждём ln2=%.6f (честная статистика после компакции)", hits[0].score, math.Ln2)
	}
	if merged.TextKey(hits[0].doc) != "doc" {
		t.Errorf("хит указывает на %q, ждём doc", merged.TextKey(hits[0].doc))
	}
}

// TestBM25MergeAppliesTombstone — удалённый док вырезается из текстового слоя
// физически: его термы исчезают из постингов нового сегмента.
func TestBM25MergeAppliesTombstone(t *testing.T) {
	lvs := NewLeveledVectorStore(bm25TestConfig())
	defer lvs.Clear()

	if err := lvs.AddDoc("doc", mkVecN(8, 1), Attributes{}, "зелёный чай"); err != nil {
		t.Fatal(err)
	}
	if err := lvs.AddDoc("other", mkVecN(8, 2), Attributes{}, "кофе"); err != nil {
		t.Fatal(err)
	}
	lvs.FlushDeltaSync()
	lvs.Delete("doc")

	segs := grabL0(t, lvs, 1)
	merged, applied := lvs.mergeSegmentsWithAllocator(segs, tcmalloc.NewTCMallocStore(1))
	if merged == nil {
		t.Fatal("merge вернул nil")
	}
	if _, ok := applied["doc"]; !ok {
		t.Error("tombstone doc не отмечен применённым")
	}
	st := merged.Text()
	if st == nil {
		t.Fatal("текстовый слой потерян")
	}
	tea := bm25Tokenize("чай")[0]
	coffee := bm25Tokenize("кофе")[0]
	if st.df(tea) != 0 || st.df(coffee) != 1 || st.nText != 1 {
		t.Errorf("df(чай)=%d df(кофе)=%d N=%d, ждём 0/1/1 — термы удалённого дока обязаны исчезнуть",
			st.df(tea), st.df(coffee), st.nText)
	}
}

// TestBM25MergeMixedAndNoText — сегмент с текстом + сегмент без: текст доезжает,
// доки без текста остаются без текста; все входы без текста → слой nil
// (zero overhead чистовекторного пути не ломается).
func TestBM25MergeMixedAndNoText(t *testing.T) {
	t.Run("mixed", func(t *testing.T) {
		lvs := NewLeveledVectorStore(bm25TestConfig())
		defer lvs.Clear()
		if err := lvs.AddDoc("txt", mkVecN(8, 1), Attributes{}, "зелёный чай"); err != nil {
			t.Fatal(err)
		}
		lvs.FlushDeltaSync()
		if err := lvs.Add("plain", mkVecN(8, 2)); err != nil {
			t.Fatal(err)
		}
		lvs.FlushDeltaSync()

		merged, _ := lvs.mergeSegmentsWithAllocator(grabL0(t, lvs, 2), tcmalloc.NewTCMallocStore(1))
		st := merged.Text()
		if st == nil {
			t.Fatal("текст потерян при смешанном merge")
		}
		tea := bm25Tokenize("чай")[0]
		if st.df(tea) != 1 || st.nText != 1 {
			t.Errorf("df=%d N=%d, ждём 1/1 (только txt с текстом)", st.df(tea), st.nText)
		}
	})

	t.Run("no_text_at_all", func(t *testing.T) {
		lvs := NewLeveledVectorStore(bm25TestConfig())
		defer lvs.Clear()
		for i := 0; i < 4; i++ {
			if err := lvs.Add(string(rune('a'+i)), mkVecN(8, float32(i))); err != nil {
				t.Fatal(err)
			}
			lvs.FlushDeltaSync()
		}
		lvs.mu.RLock()
		var segs []segment
		for lvl := range lvs.levels {
			segs = append(segs, lvs.levels[lvl]...)
		}
		lvs.mu.RUnlock()
		merged, _ := lvs.mergeSegmentsWithAllocator(segs, tcmalloc.NewTCMallocStore(1))
		if merged == nil {
			t.Fatal("merge вернул nil")
		}
		if merged.Text() != nil {
			t.Error("слой обязан быть nil для чистовекторного входа")
		}
	})
}
