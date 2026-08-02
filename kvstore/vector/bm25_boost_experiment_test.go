//go:build datasets

// Замер на внешнем датасете. Тег `datasets` намеренно держит его ВНЕ обычной
// сборки тестов: без файла в /tmp такой тест скипался молча, и «ok» пакета
// означал в том числе «тридцать функций даже не пытались запуститься».
// Запуск и получение данных — docs/BENCHMARKS.md, раздел Reproducing:
//
//	make test-datasets

package vector

// =============================================================================
// Эксперимент ПЕРЕД движковым кодом (методика «бенчи до кода», 19.07):
// проверяем два кандидата-улучшения на живом движке БЕЗ изменений ядра.
//
//  1. «Бедный BM25F» — буст заголовка повтором термов при ингесте
//     (title×W + text): tf заголовочных термов растёт в W раз — эффект
//     пер-полевого веса без формата полей. Гипотеза: known-item hit@1
//     0.846 → 0.92+; риск — рост doclen сдвигает нормализацию.
//  2. Стоп-слова на стороне ЗАПРОСА: скоринг сканирует постинги только
//     термов запроса ⇒ выкидывание общеупотребимых термов из запроса должно
//     вернуть основную часть скорости без ре-ингеста и смены токенайзера.
//     Гипотеза: QPS коротких запросов растёт кратно; риск — падение hit@k
//     на тайтлах из стоп-слов («The Who»).
//
// По итогам решается, что кодировать в движке (TITLE-поле / df-отсечение).
//
//	go test ./kvstore/vector -run TestBM25BoostExperiment -v -timeout 3600s
// =============================================================================

import (
	"fmt"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"kvstore/kvstore/internal/store/tcmalloc"
)

// Топ общеупотребимых английских термов; в проде это был бы df-порог
// (df > N/4 при N ≥ 1000) — список приближает его для эксперимента.
var bm25ExpStopwords = map[string]bool{
	"the": true, "a": true, "an": true, "of": true, "in": true, "on": true,
	"at": true, "to": true, "and": true, "or": true, "is": true, "was": true,
	"for": true, "with": true, "by": true, "from": true, "as": true,
	"it": true, "its": true, "be": true, "are": true, "were": true,
}

func bm25StripStop(q string) string {
	fields := strings.Fields(q)
	kept := fields[:0]
	for _, f := range fields {
		if !bm25ExpStopwords[strings.ToLower(f)] {
			kept = append(kept, f)
		}
	}
	if len(kept) == 0 {
		return q // запрос целиком из стоп-слов — не трогаем
	}
	return strings.Join(kept, " ")
}

func TestBM25BoostExperiment(t *testing.T) {
	if testing.Short() {
		t.Skip("long: три сборки 100k×1536 с текстовым слоем")
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

	const (
		K   = 10
		ef  = 128
		W   = 12
		dur = 2 * time.Second
		nKI = 500
	)
	stride := len(docs) / nKI

	recallKeys := func(res []VTextResult, gtRow []int32) float64 {
		want := make(map[int]struct{}, K)
		for i := 0; i < K && i < len(gtRow); i++ {
			want[int(gtRow[i])] = struct{}{}
		}
		hit := 0
		for i := 0; i < K && i < len(res); i++ {
			id, _ := strconv.Atoi(res[i].Key)
			if _, ok := want[id]; ok {
				hit++
			}
		}
		return float64(hit) / float64(K)
	}
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

	// knownItem меряет hit@1/hit@10/MRR по 500 тайтлам; queryFn позволяет
	// прогонять тот же пул через варианты запроса (сырой / без стоп-слов).
	knownItem := func(lvs *LeveledVectorStore, queryFn func(string) string) (h1, h10, mrr float64) {
		for s := range nKI {
			d := docs[s*stride]
			res, _ := lvs.SearchText(queryFn(d.Title), K)
			target := strconv.Itoa(d.I)
			for r, h := range res {
				if h.Key == target {
					if r == 0 {
						h1++
					}
					h10++
					mrr += 1.0 / float64(r+1)
					break
				}
			}
		}
		return h1 / nKI, h10 / nKI, mrr / nKI
	}

	fmt.Println()
	fmt.Printf("BM25 BOOST EXPERIMENT — dbpedia100k, ef=%d, K=%d, W=%d (baseline спринта: hit@1=0.846 hit@10=0.990 QPS_title=934)\n", ef, K, W)
	fmt.Println("---------------------------------------------------------------------------------------")

	for _, variant := range []struct {
		name        string
		titleWeight int
	}{
		{"baseline", 1},
		{"title×2", 2},
		{"title×3", 3},
	} {
		lvs := NewLeveledVectorStore(LeveledConfig{
			Distance:       DotProductDistance,
			Allocator:      tcmalloc.NewTCMallocStore(8),
			DeltaMax:       len(train) + 1,
			Fanout:         4,
			EfConstruction: arEfConstruction,
			EfSearch:       ef,
		})
		t0 := time.Now()
		for i, v := range train {
			text := strings.Repeat(docs[i].Title+" ", variant.titleWeight) + docs[i].Text
			if err := lvs.AddDoc(strconv.Itoa(i), v, Attributes{}, text); err != nil {
				t.Fatalf("[%s] AddDoc %d: %v", variant.name, i, err)
			}
		}
		settle(lvs)
		build := time.Since(t0)

		h1, h10, mrr := knownItem(lvs, func(q string) string { return q })
		// Семантика текстового плеча vs cosine-GT — страж, что буст не ломает
		// обычный полнотекстовый поиск (ожидаем ±пару пп от 0.300).
		var recText float64
		for qi := range test {
			rt, _ := lvs.SearchText(queries[qi].Title+" "+queries[qi].Text, K)
			recText += recallKeys(rt, gt[qi])
		}
		recText /= float64(len(test))
		qpsTitle := runQPSIdx(func(qi int) { _, _ = lvs.SearchText(queries[qi].Title, K) })

		fmt.Printf("%-9s known-item: hit@1=%.3f hit@10=%.3f MRR=%.3f | text(full) recall=%.4f | QPS_title=%.0f | build=%.0fs\n",
			variant.name, h1, h10, mrr, recText, qpsTitle, build.Seconds())

		if variant.titleWeight == 1 {
			// Эксперимент 2 на той же сборке: стоп-слова из ЗАПРОСА.
			sh1, sh10, smrr := knownItem(lvs, bm25StripStop)
			sqps := runQPSIdx(func(qi int) { _, _ = lvs.SearchText(bm25StripStop(queries[qi].Title), K) })
			fmt.Printf("%-9s known-item: hit@1=%.3f hit@10=%.3f MRR=%.3f |                      | QPS_title=%.0f (запросы без стоп-слов, тот же индекс)\n",
				"-stopq", sh1, sh10, smrr, sqps)
		}
		lvs.Clear()
	}
	fmt.Println("---------------------------------------------------------------------------------------")
	fmt.Println("Решение: буст в движок (TITLE) только если hit@1 растёт ≥3 пп без просадки text(full);")
	fmt.Println("df-отсечение в SearchText только если -stopq даёт кратный QPS без просадки hit@k.")
}
