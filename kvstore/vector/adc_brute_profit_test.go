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
// E2E-профит fused ADC на brute-путях (спринт 11.07, микробенч — в
// adc_brute_microbench_test.go: euclid/dot 9–11.6×, cosine 6.8×).
//
// Рецепт стора и сценарии НАМЕРЕННО повторяют стор A из
// TestSecondaryLayout_Profit (хаотичная раскладка, seed 20260630) — цифры «до»
// записаны 11.07 тем же харнессом на этой же машине: residual-brute
// (t4∧r0∧price≤249, matched=2520) = 512 QPS, глобальный recall ~0.9845.
//
// Ждём: residual-brute строки — кратный рост (ядро cosine 6.8× минус предикаты
// и topk); graph-строки и глобальный Search — паритет (brute там не участвует);
// recall всех строк — без изменений (fused ≡ скалярному пути по скорам).
//
//	go test ./kvstore/vector -run TestADCBruteProfit -v -timeout 3600s
//
// Требует /tmp/dbpedia100k.bin (python3 convert_dbpedia.py).
// =============================================================================

func TestADCBruteProfit(t *testing.T) {
	if testing.Short() {
		t.Skip("long: строит 100k×1536 SQ8 HNSW")
	}
	train, test, gtGlobal, err := loadDBpediaRaw("/tmp/dbpedia100k.bin")
	if err != nil {
		t.Skipf("нет данных: %v (python3 convert_dbpedia.py)", err)
	}

	const (
		K   = 10
		ef  = 128
		W   = 12
		dur = 2 * time.Second
	)
	blocks := []int{1000, 16000, 30000, 40000}
	nQ := min(200, len(test))
	queries := test[:nQ]

	tenantOfId := func(i int) string { tn, _, _ := attrLayout(i, blocks); return tn }
	regionOfId := func(i int) string { _, rg, _ := attrLayout(i, blocks); return rg }
	priceOfId := func(i int) float64 { _, _, pr := attrLayout(i, blocks); return pr }

	type kv struct {
		key   string
		vec   []float32
		attrs Attributes
	}
	all := make([]kv, len(train))
	for i, v := range train {
		tn, rg, pr := attrLayout(i, blocks)
		all[i] = kv{
			key: strconv.Itoa(i),
			vec: v,
			attrs: Attributes{
				Cat: map[string]string{"tenant": tn, "region": rg},
				Num: map[string]float64{"price": pr},
			},
		}
	}
	rand.New(rand.NewSource(20260630)).Shuffle(len(all), func(i, j int) { all[i], all[j] = all[j], all[i] })

	gtInt := make([][]int, nQ)
	for qi := 0; qi < nQ; qi++ {
		row := make([]int, 0, K)
		for _, id := range gtGlobal[qi] {
			row = append(row, int(id))
			if len(row) == K {
				break
			}
		}
		gtInt[qi] = row
	}

	mkPred := func(tn, rg string, lo, hi float64) func(int) bool {
		return func(i int) bool {
			if tenantOfId(i) != tn {
				return false
			}
			if rg != "" && regionOfId(i) != rg {
				return false
			}
			return hi < 0 || (priceOfId(i) >= lo && priceOfId(i) <= hi)
		}
	}
	type escen struct {
		name   string
		filt   Filter
		pred   func(int) bool
		expect string
	}
	escens := []escen{
		{"t4∧r0∧price≤124", Filter{Eq: map[string]string{"tenant": "t4", "region": "r0"}, Range: map[string][2]float64{"price": {0, 124}}}, mkPred("t4", "r0", 0, 124), "residual-brute: ЖДЁМ КРАТНЫЙ РОСТ"},
		{"t4∧r0∧price≤249", Filter{Eq: map[string]string{"tenant": "t4", "region": "r0"}, Range: map[string][2]float64{"price": {0, 249}}}, mkPred("t4", "r0", 0, 249), "residual-brute (вчера 512): ЖДЁМ КРАТНЫЙ РОСТ"},
		{"t4∧r0∧price≤499", Filter{Eq: map[string]string{"tenant": "t4", "region": "r0"}, Range: map[string][2]float64{"price": {0, 499}}}, mkPred("t4", "r0", 0, 499), "graph (5000>бюджета): паритет"},
		{"t4 целиком", Filter{Eq: map[string]string{"tenant": "t4"}}, mkPred("t4", "", 0, -1), "graph tenant-only: паритет"},
		{"t4∧price≤62 (не-префикс)", Filter{Eq: map[string]string{"tenant": "t4"}, Range: map[string][2]float64{"price": {0, 62}}}, mkPred("t4", "", 0, 62), "residual-brute: ЖДЁМ КРАТНЫЙ РОСТ"},
		{"t1∧r0 (малый тенант)", Filter{Eq: map[string]string{"tenant": "t1", "region": "r0"}}, mkPred("t1", "r0", 0, -1), "blocked-brute малого блока: ЖДЁМ РОСТ"},
	}
	egt := make([][][]int, len(escens))
	for si, sc := range escens {
		egt[si] = filterGTByPred(train, queries, K, sc.pred)
	}
	matchedOf := func(pred func(int) bool) int {
		m := 0
		for i := range train {
			if pred(i) {
				m++
			}
		}
		return m
	}

	lvs := NewLeveledVectorStore(LeveledConfig{
		Metric:         MetricCosine,
		Allocator:      tcmalloc.NewTCMallocStore(8),
		DeltaMax:       len(train) + 1,
		Fanout:         4,
		EfConstruction: arEfConstruction,
		EfSearch:       ef,
		PartitionAttr:  "tenant",
		BruteThreshold: DefaultBruteThreshold,
		UseSQ:          true,
	})
	defer lvs.Close()
	tB := time.Now()
	for _, e := range all {
		if err := lvs.AddWithAttrs(e.key, e.vec, e.attrs); err != nil {
			t.Fatalf("AddWithAttrs: %v", err)
		}
	}
	settle(lvs)
	segs := 0
	for _, n := range lvs.Stats().SegmentsByLevel {
		segs += n
	}
	fmt.Printf("\nстор: build=%.0fs segs=%d (рецепт = стор A из TestSecondaryLayout_Profit)\n", time.Since(tB).Seconds(), segs)

	globRec := recallStorePred(queries, gtInt, K, func(q []float32) []VSearchResult {
		r, _ := lvs.Search(q, K, make([]VSearchResult, 0, K))
		return r
	})
	globQPS := runQPS(W, dur, queries, func(q []float32) {
		_, _ = lvs.Search(q, K, make([]VSearchResult, 0, K))
	})

	fmt.Println()
	fmt.Println("E2E ПОСЛЕ fused ADC (сравнивать со стором A от 11.07: residual-brute=512 QPS, glob rec~0.9845)")
	fmt.Printf("%-26s %-8s | %-9s %-7s | %s\n", "сценарий", "matched", "QPS", "recall", "ожидание")
	fmt.Println("──────────────────────────────────────────────────────────────────────────────────────")
	fmt.Printf("%-26s %-8s | %-9.0f %-7.4f | глобальный: паритет\n", "глобальный Search", "-", globQPS, globRec)
	for si, sc := range escens {
		rec := recallStorePred(queries, egt[si], K, func(q []float32) []VSearchResult {
			r, _ := lvs.SearchFilter(q, K, sc.filt)
			return r
		})
		qps := runQPS(W, dur, queries, func(q []float32) { _, _ = lvs.SearchFilter(q, K, sc.filt) })
		fmt.Printf("%-26s %-8d | %-9.0f %-7.4f | %s\n", sc.name, matchedOf(sc.pred), qps, rec, sc.expect)
	}
}
