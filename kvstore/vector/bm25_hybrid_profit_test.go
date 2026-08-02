//go:build datasets

// Замер на внешнем датасете. Тег `datasets` намеренно держит его ВНЕ обычной
// сборки тестов: без файла в /tmp такой тест скипался молча, и «ok» пакета
// означал в том числе «тридцать функций даже не пытались запуститься».
// Запуск и получение данных — docs/BENCHMARKS.md, раздел Reproducing:
//
//	make test-datasets

package vector

// =============================================================================
// Profit-бенч шага 8 BM25-спринта (docs/BM25_HYBRID_DESIGN.md, Validation §2-3):
// dbpedia-openai 100k с оригинальными текстами, самодостаточный датасет HF
// (корпус=первые 100k строк, запросы=следующие 1000 held-out, GT=точный
// брутфорс cosine top-100). Данные: scripts/convert_dbpedia_hf.py →
// /tmp/dbpedia100k_text.{bin,jsonl} + /tmp/dbpedia100k_queries.jsonl.
//
// Две задачи качества + перф:
//  A. Семантика (1000 held-out запросов, текст+вектор у каждого):
//     recall@10 vs GT для vector-only / text-only / hybrid.
//     Критерий спринта: hybrid ≥ vector-only.
//  B. Known-item (запрос = точный title корпусного дока; тайтлы уникальны):
//     hit@1/hit@10/MRR@10 текстового пути. Векторного baseline НЕТ по
//     построению: у ad-hoc keyword-запроса нет эмбеддинга (нет эмбеддера) —
//     это ровно та дыра, которую закрывает embedder-free SEARCHTEXT.
//  Перф: QPS трёх путей на одном консолидированном сегменте; ожидание
//     дизайна — SEARCHTEXT сильно дешевле векторного, HYBRID ≈ вектор+10-20%.
//
//	go test ./kvstore/vector -run TestBM25HybridProfit -v -timeout 3600s
// =============================================================================

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"kvstore/kvstore/internal/store/tcmalloc"
)

type bm25BenchDoc struct {
	I     int    `json:"i"`
	Title string `json:"title"`
	Text  string `json:"text"`
}

func loadBM25JSONL(path string) ([]bm25BenchDoc, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var out []bm25BenchDoc
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	for sc.Scan() {
		var d bm25BenchDoc
		if err := json.Unmarshal(sc.Bytes(), &d); err != nil {
			return nil, fmt.Errorf("%s строка %d: %w", path, len(out), err)
		}
		out = append(out, d)
	}
	return out, sc.Err()
}

func TestBM25HybridProfit(t *testing.T) {
	if testing.Short() {
		t.Skip("long: строит 100k×1536 HNSW + текстовый слой")
	}
	train, test, gt, err := loadDBpediaRaw("/tmp/dbpedia100k_text.bin")
	if err != nil {
		t.Skipf("нет данных: %v (scratch/bm25_venv/bin/python scripts/convert_dbpedia_hf.py)", err)
	}
	docs, err := loadBM25JSONL("/tmp/dbpedia100k_text.jsonl")
	if err != nil {
		t.Skipf("нет текстов корпуса: %v", err)
	}
	queries, err := loadBM25JSONL("/tmp/dbpedia100k_queries.jsonl")
	if err != nil {
		t.Skipf("нет текстов запросов: %v", err)
	}
	if len(docs) != len(train) || len(queries) != len(test) {
		t.Fatalf("рассинхрон sidecar: docs=%d train=%d, queries=%d test=%d",
			len(docs), len(train), len(queries), len(test))
	}

	const (
		K   = 10
		ef  = 128
		W   = 12
		dur = 2 * time.Second
	)

	cfg := LeveledConfig{
		Distance:       DotProductDistance, // нормализованные → cosine == dot
		Allocator:      tcmalloc.NewTCMallocStore(8),
		DeltaMax:       len(train) + 1, // один консолидированный сегмент
		Fanout:         4,
		EfConstruction: arEfConstruction,
		EfSearch:       ef,
	}
	lvs := NewLeveledVectorStore(cfg)
	t0 := time.Now()
	for i, v := range train {
		// Продовый путь ингеста: TITLE-буст (бедный BM25F, вес ×3) + текст —
		// как VSIM.ADDDOC ... TITLE <t> TEXT <text>.
		if err := lvs.AddDocTitled(strconv.Itoa(i), v, Attributes{}, docs[i].Title, docs[i].Text); err != nil {
			t.Fatalf("AddDocTitled %d: %v", i, err)
		}
	}
	settle(lvs)
	buildDur := time.Since(t0)
	st := lvs.Stats()
	nseg := 0
	for _, n := range st.SegmentsByLevel {
		nseg += n
	}

	// recall@K по ключам-индексам train против GT top-K (как dbpedia_validation).
	recallKeys := func(keys []string, gtRow []int32) float64 {
		want := make(map[int]struct{}, K)
		for i := 0; i < K && i < len(gtRow); i++ {
			want[int(gtRow[i])] = struct{}{}
		}
		hit := 0
		for i := 0; i < K && i < len(keys); i++ {
			id, _ := strconv.Atoi(keys[i])
			if _, ok := want[id]; ok {
				hit++
			}
		}
		return float64(hit) / float64(K)
	}
	vKeys := func(res []VSearchResult) []string {
		out := make([]string, len(res))
		for i, r := range res {
			out[i] = r.Key
		}
		return out
	}
	tKeys := func(res []VTextResult) []string {
		out := make([]string, len(res))
		for i, r := range res {
			out[i] = r.Key
		}
		return out
	}

	// --- Задача A: семантика, 1000 held-out запросов ---------------------------
	// ВАЖНО про эталон (вывод baseline-прогона 19.07): GT — точный брутфорс
	// cosine, т.е. по построению = идеал ВЕКТОРНОГО плеча. Гибрид против такого
	// GT не может быть ≥ vector-only: при слабо пересекающихся списках RRF
	// интерливит (v1,t1,v2,t2…) и в top-10 остаётся ~5-6 векторных доков →
	// потолок recall ~0.5-0.65. Это свойство эталона, не дефект RRF; критерий
	// «hybrid ≥ vector на семантике» требует независимой разметки (BEIR-класс),
	// которой у датасета нет. Меряем и пиннем ФАКТ деградации в предсказанной
	// полосе — страж, что слияние ведёт себя как задумано, а не хуже.
	// Два текстовых плеча: full (~52 терма, стресс) и title (продовый короткий).
	var recVec, recTextF, recHybF, recTextT, recHybT float64
	for qi := range test {
		qFull := queries[qi].Title + " " + queries[qi].Text
		rv, _ := lvs.Search(test[qi], K, nil)
		recVec += recallKeys(vKeys(rv), gt[qi])
		rt, _ := lvs.SearchText(qFull, K)
		recTextF += recallKeys(tKeys(rt), gt[qi])
		rh, _ := lvs.SearchHybrid(qFull, test[qi], K)
		recHybF += recallKeys(tKeys(rh), gt[qi])
		rtt, _ := lvs.SearchText(queries[qi].Title, K)
		recTextT += recallKeys(tKeys(rtt), gt[qi])
		rht, _ := lvs.SearchHybrid(queries[qi].Title, test[qi], K)
		recHybT += recallKeys(tKeys(rht), gt[qi])
	}
	nQ := float64(len(test))
	recVec, recTextF, recHybF = recVec/nQ, recTextF/nQ, recHybF/nQ
	recTextT, recHybT = recTextT/nQ, recHybT/nQ

	// --- Задача B: known-item по уникальным тайтлам ----------------------------
	const nKI = 500
	stride := len(docs) / nKI
	var hit1, hit10, mrr float64
	for s := range nKI {
		d := docs[s*stride]
		res, _ := lvs.SearchText(d.Title, K)
		target := strconv.Itoa(d.I)
		for r, h := range res {
			if h.Key == target {
				if r == 0 {
					hit1++
				}
				hit10++
				mrr += 1.0 / float64(r+1)
				break
			}
		}
	}
	hit1, hit10, mrr = hit1/nKI, hit10/nKI, mrr/nKI

	// --- Перф: QPS трёх путей (W воркеров, общий счётчик запросов) -------------
	runQPSIdx := func(fn func(qi int)) float64 {
		var next, done atomic.Int64
		var wg sync.WaitGroup
		stop := time.Now().Add(dur)
		for range W {
			wg.Go(func() {
				for time.Now().Before(stop) {
					fn(int(next.Add(1)) % len(test))
					done.Add(1)
				}
			})
		}
		wg.Wait()
		return float64(done.Load()) / dur.Seconds()
	}
	qpsVec := runQPSIdx(func(qi int) { _, _ = lvs.Search(test[qi], K, nil) })
	qpsText := runQPSIdx(func(qi int) {
		_, _ = lvs.SearchText(queries[qi].Title+" "+queries[qi].Text, K)
	})
	qpsHyb := runQPSIdx(func(qi int) {
		_, _ = lvs.SearchHybrid(queries[qi].Title+" "+queries[qi].Text, test[qi], K)
	})
	// Отдельно короткий keyword-запрос: перф-профиль SEARCHTEXT на 2-3 термах
	// (продовый случай known-item; полнотекстовый запрос ~50 термов — стресс).
	qpsTitle := runQPSIdx(func(qi int) { _, _ = lvs.SearchText(queries[qi].Title, K) })

	fmt.Println()
	fmt.Printf("BM25/HYBRID PROFIT — dbpedia-openai(HF) 1536d cosine, N=%d, q=%d, ef=%d, K=%d, segs=%d, build=%.0fs\n",
		len(train), len(test), ef, K, nseg, buildDur.Seconds())
	fmt.Println("---------------------------------------------------------------------------------------")
	fmt.Printf("A. семантика  recall@10 vs cosine-GT:  vector=%.4f\n", recVec)
	fmt.Printf("   text(full)=%.4f  hybrid(full)=%.4f   text(title)=%.4f  hybrid(title)=%.4f\n",
		recTextF, recHybF, recTextT, recHybT)
	fmt.Println("   (GT=cosine ⇒ hybrid<vector по построению: RRF-интерлив, потолок ~0.5-0.65 — см. комм.)")
	fmt.Printf("B. known-item (тайтл→док): hit@1=%.3f  hit@10=%.3f  MRR@10=%.3f  (вектор: N/A — нет эмбеддера)\n",
		hit1, hit10, mrr)
	fmt.Printf("C. QPS_%d:  vector=%.0f  text(full~52w)=%.0f  text(title)=%.0f  hybrid=%.0f\n",
		W, qpsVec, qpsText, qpsTitle, qpsHyb)
	fmt.Println("---------------------------------------------------------------------------------------")

	// Планки зафиксированы по прогонам 19.07: baseline без буста/отсечения —
	// hit@1=0.846, QPS_title=934-1192; с TITLE×3 и df-отсечением (эксперимент
	// bm25_boost_experiment_test.go) — hit@1=0.906, QPS_title=7850,
	// vector=0.993-0.994, hybrid(full)=0.533-0.534. Стражи — с запасом от шума.
	if recVec < 0.98 {
		t.Errorf("vector recall=%.4f < 0.98 — деградировал векторный путь", recVec)
	}
	if recHybF < 0.45 || recHybF > 0.75 {
		t.Errorf("hybrid(full) recall=%.4f вне RRF-полосы [0.45,0.75] — слияние ведёт себя не как задумано", recHybF)
	}
	if hit1 < 0.87 {
		t.Errorf("known-item hit@1=%.3f < 0.87 (baseline с TITLE-бустом 0.906)", hit1)
	}
	if hit10 < 0.97 {
		t.Errorf("known-item hit@10=%.3f < 0.97 (baseline с TITLE-бустом 0.994)", hit10)
	}
}
