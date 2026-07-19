package vector

// Тесты шага 4 BM25-спринта: живой путь записи (AddDoc → дельта → flush →
// segmentText) и правила видимости SearchText. Оракул тот же, что у шага 3
// (testdata/bm25/golden.json), но корпус едет через НАСТОЯЩИЙ конвейер LSM,
// а не собранный вручную segmentText.

import (
	"fmt"
	"math"
	"testing"
)

func bm25TestConfig() LeveledConfig {
	return LeveledConfig{
		Distance:       EuclideanDistance,
		DeltaMax:       1000,
		Fanout:         8,
		EfSearch:       64,
		M:              16,
		EfConstruction: 100,
		NumBuilders:    1,
	}
}

// bm25AddCorpus добавляет доки корпуса [from, to) через живой AddDoc.
// Векторы — фиктивные (текстовый путь их не читает), но валидные для LSM;
// dim задаёт размерность (8 = frozen-CSR путь, >256 = hnswSegment).
func bm25AddCorpus(t *testing.T, lvs *LeveledVectorStore, corpus bm25Corpus, from, to, dim int) {
	t.Helper()
	for i := from; i < to; i++ {
		d := corpus.Docs[i]
		if err := lvs.AddDoc(d.ID, mkVecN(dim, float32(i)), Attributes{}, d.Text); err != nil {
			t.Fatalf("AddDoc %s: %v", d.ID, err)
		}
	}
}

// bm25AssertGolden прогоняет все golden-запросы через SearchText и сверяет
// ранжирование/скоры с эталоном.
func bm25AssertGolden(t *testing.T, lvs *LeveledVectorStore, corpus bm25Corpus, golden bm25Golden) {
	t.Helper()
	for _, q := range golden.Queries {
		res, err := lvs.SearchText(q.Text, len(corpus.Docs))
		if err != nil {
			t.Fatalf("%s: SearchText: %v", q.ID, err)
		}
		got := make([]bm25GoldenHit, len(res))
		for i, r := range res {
			got[i] = bm25GoldenHit{Doc: r.Key, Score: r.Score}
		}
		assertBM25Ranking(t, q.ID, q.Ranking, got)
	}
}

// TestBM25LivePath — оракул через живой конвейер в трёх состояниях LSM:
// весь корпус в дельте; весь во frozen-сегменте (переживает flush, гейт
// HasText увёл flush с freeze-fast-path на rebuild); смешанное состояние
// (глобальная статистика дельта+сегмент).
func TestBM25LivePath(t *testing.T) {
	corpus, golden := loadBM25Golden(t)

	t.Run("delta_only", func(t *testing.T) {
		lvs := NewLeveledVectorStore(bm25TestConfig())
		defer lvs.Clear()
		bm25AddCorpus(t, lvs, corpus, 0, len(corpus.Docs), 8)
		bm25AssertGolden(t, lvs, corpus, golden)
	})

	t.Run("after_flush", func(t *testing.T) {
		lvs := NewLeveledVectorStore(bm25TestConfig())
		defer lvs.Clear()
		bm25AddCorpus(t, lvs, corpus, 0, len(corpus.Docs), 8)
		lvs.FlushDeltaSync()
		bm25AssertGolden(t, lvs, corpus, golden)
	})

	t.Run("mixed_segment_plus_delta", func(t *testing.T) {
		lvs := NewLeveledVectorStore(bm25TestConfig())
		defer lvs.Clear()
		split := len(corpus.Docs) / 2
		bm25AddCorpus(t, lvs, corpus, 0, split, 8)
		lvs.FlushDeltaSync()
		bm25AddCorpus(t, lvs, corpus, split, len(corpus.Docs), 8)
		bm25AssertGolden(t, lvs, corpus, golden)
	})
}

// bm25Keys — ключи выдачи в порядке ранжирования.
func bm25Keys(res []VTextResult) []string {
	out := make([]string, len(res))
	for i, r := range res {
		out[i] = r.Key
	}
	return out
}

// bm25Find возвращает (скор, найден) ключа в выдаче.
func bm25Find(res []VTextResult, key string) (float64, bool) {
	for _, r := range res {
		if r.Key == key {
			return r.Score, true
		}
	}
	return 0, false
}

// TestBM25Shadowing_UpsertOverSegment — механизм «дельта затеняет сегмент»:
// после upsert с новым текстом старая frozen-копия не матчится по стёртым
// термам, новая ищется, ключ в выдаче ровно один раз.
func TestBM25Shadowing_UpsertOverSegment(t *testing.T) {
	lvs := NewLeveledVectorStore(bm25TestConfig())
	defer lvs.Clear()

	if err := lvs.AddDoc("doc", mkVecN(8, 1), Attributes{}, "зелёный чай из Астаны"); err != nil {
		t.Fatal(err)
	}
	// Сосед с текстом, чтобы корпус не пустел после переписывания doc.
	if err := lvs.AddDoc("other", mkVecN(8, 2), Attributes{}, "кофе и молоко"); err != nil {
		t.Fatal(err)
	}
	lvs.FlushDeltaSync()

	// Upsert в дельту: термы старой версии («чай») исчезли из дока.
	if err := lvs.AddDoc("doc", mkVecN(8, 3), Attributes{}, "вечерний кофе"); err != nil {
		t.Fatal(err)
	}

	res, _ := lvs.SearchText("чай", 10)
	if _, found := bm25Find(res, "doc"); found {
		t.Errorf("стёртый терм всё ещё матчит doc (stale frozen-копия не затенена): %v", bm25Keys(res))
	}
	res, _ = lvs.SearchText("кофе", 10)
	cnt := 0
	for _, r := range res {
		if r.Key == "doc" {
			cnt++
		}
	}
	if cnt != 1 {
		t.Errorf("doc в выдаче %d раз, ждём ровно 1: %v", cnt, bm25Keys(res))
	}
}

// TestBM25Shadowing_PlainAddDropsText — upsert БЕЗ текста (обычный Add) снимает
// текст дока: полная замена состояния, как у attrs. Старая frozen-копия с
// текстом обязана быть затенённой.
func TestBM25Shadowing_PlainAddDropsText(t *testing.T) {
	lvs := NewLeveledVectorStore(bm25TestConfig())
	defer lvs.Clear()

	if err := lvs.AddDoc("doc", mkVecN(8, 1), Attributes{}, "зелёный чай"); err != nil {
		t.Fatal(err)
	}
	if err := lvs.AddDoc("other", mkVecN(8, 2), Attributes{}, "чай с лимоном"); err != nil {
		t.Fatal(err)
	}
	lvs.FlushDeltaSync()

	if err := lvs.Add("doc", mkVecN(8, 3)); err != nil { // upsert без текста
		t.Fatal(err)
	}

	res, _ := lvs.SearchText("чай", 10)
	if _, found := bm25Find(res, "doc"); found {
		t.Errorf("doc без текста матчится по термам стёртой версии: %v", bm25Keys(res))
	}
	if _, found := bm25Find(res, "other"); !found {
		t.Errorf("сосед other пропал из выдачи: %v", bm25Keys(res))
	}
}

// TestBM25Shadowing_Delete — tombstone-механизм: удалённый док не всплывает в
// текстовой выдаче ни из дельты (постинги сняты), ни из сегмента (маска).
func TestBM25Shadowing_Delete(t *testing.T) {
	t.Run("delete_in_delta", func(t *testing.T) {
		lvs := NewLeveledVectorStore(bm25TestConfig())
		defer lvs.Clear()
		if err := lvs.AddDoc("doc", mkVecN(8, 1), Attributes{}, "зелёный чай"); err != nil {
			t.Fatal(err)
		}
		if err := lvs.AddDoc("other", mkVecN(8, 2), Attributes{}, "чай с лимоном"); err != nil {
			t.Fatal(err)
		}
		lvs.Delete("doc")
		res, _ := lvs.SearchText("чай", 10)
		if _, found := bm25Find(res, "doc"); found {
			t.Errorf("удалённый в дельте doc всплыл: %v", bm25Keys(res))
		}
	})

	t.Run("delete_after_flush", func(t *testing.T) {
		lvs := NewLeveledVectorStore(bm25TestConfig())
		defer lvs.Clear()
		if err := lvs.AddDoc("doc", mkVecN(8, 1), Attributes{}, "зелёный чай"); err != nil {
			t.Fatal(err)
		}
		if err := lvs.AddDoc("other", mkVecN(8, 2), Attributes{}, "чай с лимоном"); err != nil {
			t.Fatal(err)
		}
		lvs.FlushDeltaSync()
		lvs.Delete("doc")
		res, _ := lvs.SearchText("чай", 10)
		if _, found := bm25Find(res, "doc"); found {
			t.Errorf("tombstoned doc всплыл из сегмента: %v", bm25Keys(res))
		}
		if _, found := bm25Find(res, "other"); !found {
			t.Errorf("сосед other пропал из выдачи: %v", bm25Keys(res))
		}
	})
}

// TestBM25Shadowing_TwoSegmentsProvenance — «новый сегмент затеняет старый»:
// две frozen-копии одного ключа после двух flush'ей. Провенанс-дедуп обязан
// оставить копию свежего сегмента, даже когда stale-копия скорится ВЫШЕ
// (tf=3 против tf=1) — выбор по свежести, не по скору.
func TestBM25Shadowing_TwoSegmentsProvenance(t *testing.T) {
	lvs := NewLeveledVectorStore(bm25TestConfig())
	defer lvs.Clear()

	if err := lvs.AddDoc("doc", mkVecN(8, 1), Attributes{}, "чай чай чай"); err != nil {
		t.Fatal(err)
	}
	lvs.FlushDeltaSync()
	if err := lvs.AddDoc("doc", mkVecN(8, 2), Attributes{}, "чай"); err != nil {
		t.Fatal(err)
	}
	lvs.FlushDeltaSync()

	res, _ := lvs.SearchText("чай", 10)
	if len(res) != 1 || res[0].Key != "doc" {
		t.Fatalf("ждём ровно один хит doc, получили %v", bm25Keys(res))
	}

	// Обе копии физически в постингах → n=2, df=2, avgdl=(3+1)/2 — принятая
	// семантика завышения до компакции. Считаем оба кандидатских скора той же
	// формулой и требуем скор СВЕЖЕЙ копии (tf=1, dl=1), хотя stale выше.
	idf := bm25IDF(2, 2)
	avgdl := 2.0
	score := func(tf, dl float64) float64 {
		return idf * tf * (bm25K1 + 1) / (tf + bm25K1*(1-bm25B+bm25B*dl/avgdl))
	}
	fresh, stale := score(1, 1), score(3, 3)
	if stale <= fresh {
		t.Fatalf("тест потерял смысл: stale-скор %.6f должен быть выше fresh %.6f", stale, fresh)
	}
	if math.Abs(res[0].Score-fresh) > bm25ScoreEps {
		t.Errorf("скор %.6f — похоже, победила stale-копия (fresh=%.6f stale=%.6f)", res[0].Score, fresh, stale)
	}
}

// TestBM25DFShadowedInflation — принятая семантика статистики: затенённая
// upsert-копия завышает df до компакции (как count в Info()), но на ВЫДАЧУ
// не влияет — ключ ровно один раз.
func TestBM25DFShadowedInflation(t *testing.T) {
	lvs := NewLeveledVectorStore(bm25TestConfig())
	defer lvs.Clear()

	if err := lvs.AddDoc("doc", mkVecN(8, 1), Attributes{}, "чай"); err != nil {
		t.Fatal(err)
	}
	lvs.FlushDeltaSync()
	if err := lvs.AddDoc("doc", mkVecN(8, 2), Attributes{}, "чай"); err != nil {
		t.Fatal(err)
	}

	// df терма по всем источникам — как его видит SearchText. Терм берём из
	// токенайзера (в индексе лежит стем, не сырое слово).
	terms := bm25Tokenize("чай")
	var df uint64
	lvs.mu.RLock()
	for lvl := range lvs.levels {
		for _, seg := range lvs.levels[lvl] {
			if st := seg.Text(); st != nil {
				df += st.df(terms[0])
			}
		}
	}
	if lvs.delta != nil {
		_, _, mdf := lvs.delta.TextStats(terms)
		df += mdf[0]
	}
	lvs.mu.RUnlock()
	if df != 2 {
		t.Errorf("df=%d, ждём 2 (затенённая копия должна считаться до компакции)", df)
	}

	res, _ := lvs.SearchText("чай", 10)
	if len(res) != 1 || res[0].Key != "doc" {
		t.Errorf("выдача обязана быть {doc}: %v", bm25Keys(res))
	}
}

// TestBM25DeltaTextMaintenance — white-box инвертед-мапы дельты: upsert
// переписывает постинги (стёртые термы уходят, df не течёт), delete снимает
// вклад дока из статистики целиком. Шардированная дельта — проверяем роутинг.
func TestBM25DeltaTextMaintenance(t *testing.T) {
	d := NewDeltaSegmentSharded(4, 100, EuclideanDistance, 0, 0, 4)
	defer d.Close()

	// Термы берём из самого токенайзера (стемы — его контракт, не тестовый).
	tea := bm25Tokenize("зелёный чай") // 2 терма
	coffee := bm25Tokenize("кофе")     // 1 терм
	queryTerms := []string{tea[0], tea[1], coffee[0]}

	vec := mkVecN(4, 1)
	for i := 0; i < 10; i++ {
		d.AppendDoc(fmt.Sprintf("k%d", i), vec, Attributes{}, bm25CountTerms(tea))
	}
	n, totalLen, df := d.TextStats(queryTerms)
	if n != 10 || totalLen != 20 || df[0] != 10 || df[1] != 10 || df[2] != 0 {
		t.Fatalf("после вставок: n=%d len=%d df=%v, ждём 10/20/[10 10 0]", n, totalLen, df)
	}

	// Upsert половины на другой текст: df чайных термов падает, кофейного растёт.
	for i := 0; i < 5; i++ {
		d.AppendDoc(fmt.Sprintf("k%d", i), vec, Attributes{}, bm25CountTerms(coffee))
	}
	n, totalLen, df = d.TextStats(queryTerms)
	if n != 10 || totalLen != 15 || df[0] != 5 || df[1] != 5 || df[2] != 5 {
		t.Fatalf("после upsert: n=%d len=%d df=%v, ждём 10/15/[5 5 5]", n, totalLen, df)
	}

	// Delete снимает вклад целиком.
	for i := 0; i < 10; i++ {
		d.Delete(fmt.Sprintf("k%d", i))
	}
	n, totalLen, df = d.TextStats(queryTerms)
	if n != 0 || totalLen != 0 || df[0] != 0 || df[1] != 0 || df[2] != 0 {
		t.Fatalf("после delete: n=%d len=%d df=%v, ждём нули", n, totalLen, df)
	}
}
