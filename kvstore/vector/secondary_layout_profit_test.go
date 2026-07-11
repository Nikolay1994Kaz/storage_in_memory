package vector

import (
	"fmt"
	"math/rand"
	"sort"
	"strconv"
	"testing"
	"time"

	"kvstore/kvstore/internal/store/tcmalloc"
)

// =============================================================================
// БЕНЧИ ДО КОДА: вторичная contiguous-раскладка внутри тенанта.
//
// Гипотеза ([[vector-large-tenant-ceiling-20260630]], tenant.go:69): разрыв
// 288 vs 2530 QPS на «крупный тенант × селективный фильтр» — это cache-локальность
// чтения векторов (matched разбросаны по блоку → каждая дистанция = холодный
// вектор). Лечится расширением comparator'а компакции: tenant → tenant,region,price.
//
// ТРЮК ЗАМЕРА БЕЗ ПРОДУКТОВОГО КОДА: sortEntriesByTenant стабильная, дельта
// (1 шард) отдаёт entries в порядке вставки → если вставлять предсортированным
// по (tenant,region,price), итоговый сегмент получает ровно ту раскладку,
// которую даст будущий comparator. Обе раскладки строятся СЕГОДНЯШНИМ кодом.
//
// Что меряем:
//   Часть 1 (слаб-микробенч, механика): один и тот же блок t4 (40k×1536) в двух
//     порядках; путь A = семантика bruteRangeAttr (проход O(block), dist на
//     разбросанных matched); путь B = будущий роутинг (binsearch по сортированным
//     колонкам → dist по СМЕЖНОМУ подотрезку). Результаты обязаны совпадать.
//   Часть 2 (e2e, боевой стор ×2): те же данные/код, различие ТОЛЬКО в раскладке.
//     ВЫИГРЫШ: сценарии matched ≤ residual-бюджета (ef*32=4096) идут через
//       СУЩЕСТВУЮЩИЙ residual-brute — на раскладке B matched физически смежны,
//       ускорение видно без единой правки кода.
//     НИЧЕГО НЕ ТЕРЯЕМ: глобальный поиск (recall/QPS), канон matched=5000
//       (graph-путь), tenant-only, не-префиксный предикат, малый тенант —
//       все обязаны быть A≈B.
//   Часть 3: цена расширенного comparator'а на компакции (sort 100k entries).
//
//	go test ./kvstore/vector -run TestSecondaryLayout_Profit -v -timeout 3600s
//
// Требует /tmp/dbpedia100k.bin (python3 convert_dbpedia.py).
// =============================================================================

func TestSecondaryLayout_Profit(t *testing.T) {
	if testing.Short() {
		t.Skip("long: строит 100k×1536 SQ8 HNSW дважды + слаб-микробенч")
	}
	train, test, gtGlobal, err := loadDBpediaRaw("/tmp/dbpedia100k.bin")
	if err != nil {
		t.Skipf("нет данных: %v (python3 convert_dbpedia.py)", err)
	}
	dim := len(train[0])

	const (
		K   = 10
		ef  = 128
		W   = 12
		dur = 2 * time.Second
	)
	blocks := []int{1000, 16000, 30000, 40000} // t1..t4, хвост = фоновый t0
	nQ := min(200, len(test))
	queries := test[:nQ]

	tenantOfId := func(i int) string { tn, _, _ := attrLayout(i, blocks); return tn }
	regionOfId := func(i int) string { _, rg, _ := attrLayout(i, blocks); return rg }
	priceOfId := func(i int) float64 { _, _, pr := attrLayout(i, blocks); return pr }

	// ════════ Часть 1: слаб-микробенч (механика выигрыша, изолированно) ════════
	// t4 = глобальные id [47000, 87000), B=40000, graph-маршрут в боевом сторе.
	const t4lo, t4sz = 47000, 40000

	// Раскладка A («сегодня»): порядок внутри блока случайный (как после шафла вставок).
	idsA := make([]int, t4sz)
	for j := range idsA {
		idsA[j] = t4lo + j
	}
	rand.New(rand.NewSource(20260630)).Shuffle(t4sz, func(a, b int) { idsA[a], idsA[b] = idsA[b], idsA[a] })
	// Раскладка B («будущая»): sort (region, price, id) — то, что даст comparator.
	idsB := append([]int(nil), idsA...)
	sort.SliceStable(idsB, func(a, b int) bool {
		ra, rb := idsB[a]%4, idsB[b]%4 // region-код (attrLayout: r = i%4)
		if ra != rb {
			return ra < rb
		}
		pa, pb := priceOfId(idsB[a]), priceOfId(idsB[b])
		if pa != pb {
			return pa < pb
		}
		return idsB[a] < idsB[b]
	})

	buildSlab := func(ids []int) (flat []float32, reg []uint8, price []float64) {
		flat = make([]float32, t4sz*dim)
		reg = make([]uint8, t4sz)
		price = make([]float64, t4sz)
		for j, id := range ids {
			copy(flat[j*dim:(j+1)*dim], train[id])
			reg[j] = uint8(id % 4)
			price[j] = priceOfId(id)
		}
		return
	}
	flatA, regA, priceA := buildSlab(idsA)
	flatB, regB, priceB := buildSlab(idsB)
	// Граница r0 в раскладке B: region — ведущий ключ, r0 = префикс.
	r0cnt := sort.Search(t4sz, func(j int) bool { return regB[j] > 0 })

	// Путь A — семантика bruteRangeAttr: проход всего блока, предикат по колонкам,
	// dist на matched (разбросаны).
	scanTopK := func(flat []float32, reg []uint8, price []float64, ids []int, priceHi float64, q []float32) []int {
		type de struct {
			d  float32
			id int
		}
		best := make([]de, 0, K+1)
		for j := 0; j < t4sz; j++ {
			if reg[j] != 0 {
				continue
			}
			if priceHi >= 0 && price[j] > priceHi {
				continue
			}
			d := EuclideanDistance(q, flat[j*dim:(j+1)*dim])
			if len(best) < K {
				best = append(best, de{d, ids[j]})
				for k := len(best) - 1; k > 0 && best[k].d < best[k-1].d; k-- {
					best[k], best[k-1] = best[k-1], best[k]
				}
			} else if d < best[K-1].d {
				best[K-1] = de{d, ids[j]}
				for k := K - 1; k > 0 && best[k].d < best[k-1].d; k-- {
					best[k], best[k-1] = best[k-1], best[k]
				}
			}
		}
		out := make([]int, len(best))
		for i := range best {
			out[i] = best[i].id
		}
		return out
	}
	// Путь B — будущий роутинг: binsearch верхней границы price внутри r0-префикса,
	// dist по смежному подотрезку [0, up).
	contigTopK := func(priceHi float64, q []float32) []int {
		up := r0cnt
		if priceHi >= 0 {
			up = sort.Search(r0cnt, func(j int) bool { return priceB[j] > priceHi })
		}
		type de struct {
			d  float32
			id int
		}
		best := make([]de, 0, K+1)
		for j := 0; j < up; j++ {
			d := EuclideanDistance(q, flatB[j*dim:(j+1)*dim])
			if len(best) < K {
				best = append(best, de{d, idsB[j]})
				for k := len(best) - 1; k > 0 && best[k].d < best[k-1].d; k-- {
					best[k], best[k-1] = best[k-1], best[k]
				}
			} else if d < best[K-1].d {
				best[K-1] = de{d, idsB[j]}
				for k := K - 1; k > 0 && best[k].d < best[k-1].d; k-- {
					best[k], best[k-1] = best[k-1], best[k]
				}
			}
		}
		out := make([]int, len(best))
		for i := range best {
			out[i] = best[i].id
		}
		return out
	}

	type scen struct {
		name    string
		priceHi float64 // -1 => r0 целиком
	}
	scens := []scen{
		{"r0∧price[0,124]", 124},
		{"r0∧price[0,249]", 249},
		{"r0∧price[0,499]", 499}, // канон ~5000
		{"r0 (all price)", -1},
	}

	fmt.Println()
	fmt.Printf("ЧАСТЬ 1 — СЛАБ-МИКРОБЕНЧ (блок t4=40000×%d fp32, механика раскладки, W=%d, K=%d)\n", dim, W, K)
	fmt.Printf("%-18s %-8s | %-11s %-8s | %-11s %-8s | %-8s\n",
		"predicate(t4∧)", "matched", "scan_QPS", "s_rec", "contig_QPS", "c_rec", "gain")
	fmt.Println("────────────────────────────────────────────────────────────────────────────────")
	for _, sc := range scens {
		matchId := func(i int) bool {
			if tenantOfId(i) != "t4" || regionOfId(i) != "r0" {
				return false
			}
			return sc.priceHi < 0 || (priceOfId(i) >= 0 && priceOfId(i) <= sc.priceHi)
		}
		matched := 0
		for i := t4lo; i < t4lo+t4sz; i++ {
			if matchId(i) {
				matched++
			}
		}
		gt := filterGTByPred(train, queries, K, matchId)
		sRec := recallPred(queries, gt, K, func(q []float32) []int { return scanTopK(flatA, regA, priceA, idsA, sc.priceHi, q) })
		cRec := recallPred(queries, gt, K, func(q []float32) []int { return contigTopK(sc.priceHi, q) })
		sQPS := runQPS(W, dur, queries, func(q []float32) { _ = scanTopK(flatA, regA, priceA, idsA, sc.priceHi, q) })
		cQPS := runQPS(W, dur, queries, func(q []float32) { _ = contigTopK(sc.priceHi, q) })
		fmt.Printf("%-18s %-8d | %-11.0f %-8.3f | %-11.0f %-8.3f | %-8.2f×\n",
			sc.name, matched, sQPS, sRec, cQPS, cRec, cQPS/sQPS)
	}
	flatA, flatB = nil, nil // отпустить 490MB перед стором
	_ = flatA
	_ = flatB

	// ════════ Часть 2: e2e — два боевых стора, различие только в раскладке ════════
	type kv struct {
		key   string
		vec   []float32
		attrs Attributes
	}
	mkAll := func() []kv {
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
		return all
	}

	// GT по сценариям (не зависят от раскладки) — считаем один раз.
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
	type escen struct {
		name   string
		filt   Filter
		pred   func(int) bool
		expect string // какой путь/что ждём
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
	escens := []escen{
		{"t4∧r0∧price≤124", Filter{Eq: map[string]string{"tenant": "t4", "region": "r0"}, Range: map[string][2]float64{"price": {0, 124}}}, mkPred("t4", "r0", 0, 124), "residual-brute: ЖДЁМ ВЫИГРЫШ B"},
		{"t4∧r0∧price≤249", Filter{Eq: map[string]string{"tenant": "t4", "region": "r0"}, Range: map[string][2]float64{"price": {0, 249}}}, mkPred("t4", "r0", 0, 249), "residual-brute: ЖДЁМ ВЫИГРЫШ B"},
		{"t4∧r0∧price≤499", Filter{Eq: map[string]string{"tenant": "t4", "region": "r0"}, Range: map[string][2]float64{"price": {0, 499}}}, mkPred("t4", "r0", 0, 499), "graph (5000>бюджета): паритет"},
		{"t4 целиком", Filter{Eq: map[string]string{"tenant": "t4"}}, mkPred("t4", "", 0, -1), "graph tenant-only: паритет"},
		{"t4∧price≤62 (не-префикс)", Filter{Eq: map[string]string{"tenant": "t4"}, Range: map[string][2]float64{"price": {0, 62}}}, mkPred("t4", "", 0, 62), "residual-brute БЕЗ region: паритет A/B по маршруту"},
		{"t1∧r0 (малый тенант)", Filter{Eq: map[string]string{"tenant": "t1", "region": "r0"}}, mkPred("t1", "r0", 0, -1), "blocked-brute: паритет"},
	}
	egt := make([][][]int, len(escens))
	for si, sc := range escens {
		egt[si] = filterGTByPred(train, queries, K, sc.pred)
	}

	type mrow struct {
		qps, rec float64
	}
	type storeRes struct {
		build    time.Duration
		segs     int
		globQPS  float64
		globRec  float64
		filt     []mrow
	}

	runStore := func(name string, all []kv) storeRes {
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
				t.Fatalf("%s AddWithAttrs: %v", name, err)
			}
		}
		settle(lvs)
		res := storeRes{build: time.Since(tB)}
		st := lvs.Stats()
		for _, n := range st.SegmentsByLevel {
			res.segs += n
		}
		// Глобальный нефильтрованный поиск (recall vs официальный GT файла).
		res.globRec = recallStorePred(queries, gtInt, K, func(q []float32) []VSearchResult {
			r, _ := lvs.Search(q, K, make([]VSearchResult, 0, K))
			return r
		})
		res.globQPS = runQPS(W, dur, queries, func(q []float32) {
			_, _ = lvs.Search(q, K, make([]VSearchResult, 0, K))
		})
		// Фильтрованные сценарии.
		for si := range escens {
			filt := escens[si].filt
			rec := recallStorePred(queries, egt[si], K, func(q []float32) []VSearchResult {
				r, _ := lvs.SearchFilter(q, K, filt)
				return r
			})
			qps := runQPS(W, dur, queries, func(q []float32) { _, _ = lvs.SearchFilter(q, K, filt) })
			res.filt = append(res.filt, mrow{qps, rec})
		}
		return res
	}

	all := mkAll()
	// Стор A — сегодняшняя раскладка: вставка в случайном порядке.
	ordA := append([]kv(nil), all...)
	rand.New(rand.NewSource(20260630)).Shuffle(len(ordA), func(i, j int) { ordA[i], ordA[j] = ordA[j], ordA[i] })
	// Стор B — будущая раскладка: вставка в порядке (tenant,region,price,id);
	// стабильный тенант-сорт компакции сохранит вторичный порядок внутри блока.
	ordB := append([]kv(nil), ordA...)
	sort.SliceStable(ordB, func(a, b int) bool {
		ta, tb := ordB[a].attrs.Cat["tenant"], ordB[b].attrs.Cat["tenant"]
		if ta != tb {
			return ta < tb
		}
		ra, rb := ordB[a].attrs.Cat["region"], ordB[b].attrs.Cat["region"]
		if ra != rb {
			return ra < rb
		}
		pa, pb := ordB[a].attrs.Num["price"], ordB[b].attrs.Num["price"]
		if pa != pb {
			return pa < pb
		}
		return ordB[a].key < ordB[b].key
	})

	fmt.Println()
	fmt.Println("ЧАСТЬ 2 — E2E: боевой SQ8-стор, A=хаотичная раскладка vs B=вторичная (tenant,region,price)")
	resA := runStore("A", ordA)
	fmt.Printf("A: build=%.0fs segs=%d\n", resA.build.Seconds(), resA.segs)
	resB := runStore("B", ordB)
	fmt.Printf("B: build=%.0fs segs=%d\n", resB.build.Seconds(), resB.segs)

	fmt.Println()
	fmt.Printf("%-26s | %-9s %-7s | %-9s %-7s | %-7s | %s\n", "сценарий", "A_QPS", "A_rec", "B_QPS", "B_rec", "B/A", "ожидание")
	fmt.Println("──────────────────────────────────────────────────────────────────────────────────────────────")
	fmt.Printf("%-26s | %-9.0f %-7.4f | %-9.0f %-7.4f | %-7.2f | глобальный recall: ПАРИТЕТ\n",
		"глобальный Search", resA.globQPS, resA.globRec, resB.globQPS, resB.globRec, resB.globQPS/resA.globQPS)
	for si, sc := range escens {
		a, b := resA.filt[si], resB.filt[si]
		fmt.Printf("%-26s | %-9.0f %-7.4f | %-9.0f %-7.4f | %-7.2f | %s\n",
			sc.name, a.qps, a.rec, b.qps, b.rec, b.qps/a.qps, sc.expect)
	}

	// ════════ Часть 3: цена расширенного comparator'а на компакции ════════
	entries := make([]DeltaEntry, len(all))
	for i, e := range all {
		entries[i] = DeltaEntry{Key: e.key, Vec: e.vec, Attrs: e.attrs}
	}
	pd := newAttrDict()
	tenantCode := func(e DeltaEntry) uint64 { return uint64(pd.intern(e.Attrs.Cat["tenant"])) }
	e1 := append([]DeltaEntry(nil), entries...)
	t1 := time.Now()
	sortEntriesByTenant(e1, tenantCode)
	curSort := time.Since(t1)
	e2 := append([]DeltaEntry(nil), entries...)
	t2 := time.Now()
	sort.SliceStable(e2, func(a, b int) bool {
		ca, cb := tenantCode(e2[a]), tenantCode(e2[b])
		if ca != cb {
			return ca < cb
		}
		ra, rb := e2[a].Attrs.Cat["region"], e2[b].Attrs.Cat["region"]
		if ra != rb {
			return ra < rb
		}
		return e2[a].Attrs.Num["price"] < e2[b].Attrs.Num["price"]
	})
	extSort := time.Since(t2)

	fmt.Println()
	fmt.Printf("ЧАСТЬ 3 — цена comparator'а на компакции (%d entries): tenant-only %v → tenant,region,price %v (build сегмента для сравнения: %.0fs)\n",
		len(entries), curSort.Round(time.Millisecond), extSort.Round(time.Millisecond), resA.build.Seconds())
	fmt.Println()
	fmt.Println("ЧТЕНИЕ: Часть 1 = механика (contig/scan — потолок будущего роутинга при равном recall).")
	fmt.Println("Часть 2: строки «ЖДЁМ ВЫИГРЫШ B» идут через СУЩЕСТВУЮЩИЙ residual-brute — выигрыш от одной")
	fmt.Println("раскладки, без правок кода. Остальные строки + глобальный Search обязаны быть A≈B (ничего не теряем).")
	fmt.Println("Часть 3: доплата за расширенный ключ сортировки — должна тонуть в стоимости build.")
}
