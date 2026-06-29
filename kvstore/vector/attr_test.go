package vector

import (
	"fmt"
	"math/rand"
	"sort"
	"strconv"
	"testing"

	"kvstore/kvstore/internal/store/tcmalloc"
)

// =============================================================================
// Unit-тесты колоночного слоя атрибутов (attr.go).
// =============================================================================

func TestAttr_DictRoundTrip(t *testing.T) {
	d := newAttrDict()
	a := d.intern("acme")
	b := d.intern("globex")
	if a == b {
		t.Fatal("разные значения получили одинаковый код")
	}
	if d.intern("acme") != a {
		t.Fatal("повторный intern того же значения дал другой код")
	}
	if c, ok := d.lookup("acme"); !ok || c != a {
		t.Fatalf("lookup acme = (%d,%v), ожидали (%d,true)", c, ok, a)
	}
	if _, ok := d.lookup("missing"); ok {
		t.Fatal("lookup несуществующего значения вернул ok=true")
	}
	if d.vals[a] != "acme" || d.vals[b] != "globex" {
		t.Fatalf("обратный декод неверен: %v", d.vals)
	}
}

func TestAttr_BuildColumnsAlignedAndDecode(t *testing.T) {
	entries := []DeltaEntry{
		{Key: "k0", Attrs: Attributes{Cat: map[string]string{"tenant": "a", "cat": "x"}, Num: map[string]float64{"price": 10}}},
		{Key: "k1", Attrs: Attributes{Cat: map[string]string{"tenant": "a"}, Num: map[string]float64{"price": 20}}}, // нет cat
		{Key: "k2", Attrs: Attributes{Cat: map[string]string{"tenant": "b", "cat": "y"}}},                           // нет price
	}
	sa := buildSegmentAttrs(entries, nil)
	if sa == nil {
		t.Fatal("buildSegmentAttrs вернул nil при наличии атрибутов")
	}
	// Выравнивание: decodeAt(i) должен вернуть исходные атрибуты entries[i].
	for i := range entries {
		got := sa.decodeAt(i)
		want := entries[i].Attrs
		for name, v := range want.Cat {
			if got.Cat[name] != v {
				t.Errorf("i=%d cat[%s]=%q, ожидали %q", i, name, got.Cat[name], v)
			}
		}
		for name, v := range want.Num {
			if got.Num[name] != v {
				t.Errorf("i=%d num[%s]=%v, ожидали %v", i, name, got.Num[name], v)
			}
		}
		// Отсутствующие атрибуты не должны появляться при декоде.
		if _, has := want.Cat["cat"]; !has {
			if _, got2 := got.Cat["cat"]; got2 {
				t.Errorf("i=%d: декод вернул отсутствующий cat", i)
			}
		}
	}
}

func TestAttr_CompileMultiAttr(t *testing.T) {
	entries := []DeltaEntry{
		{Key: "k0", Attrs: Attributes{Cat: map[string]string{"cat": "x"}, Num: map[string]float64{"price": 10}}},
		{Key: "k1", Attrs: Attributes{Cat: map[string]string{"cat": "x"}, Num: map[string]float64{"price": 50}}},
		{Key: "k2", Attrs: Attributes{Cat: map[string]string{"cat": "y"}, Num: map[string]float64{"price": 30}}},
	}
	sa := buildSegmentAttrs(entries, nil)

	// Eq cat=x AND price ∈ [0,20] → только k0.
	pred, matchable := sa.compile(Filter{
		Eq:    map[string]string{"cat": "x"},
		Range: map[string][2]float64{"price": {0, 20}},
	}, "")
	if !matchable {
		t.Fatal("matchable=false на присутствующем атрибуте")
	}
	got := []bool{pred(0), pred(1), pred(2)}
	want := []bool{true, false, false}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("pred(%d)=%v, ожидали %v", i, got[i], want[i])
		}
	}

	// Значение не встречалось в сегменте → matchable=false (сегмент пропускается).
	if _, m := sa.compile(Filter{Eq: map[string]string{"cat": "zzz"}}, ""); m {
		t.Error("matchable=true для несуществующего значения")
	}
	// Отсутствующий числовой атрибут → matchable=false.
	if _, m := sa.compile(Filter{Range: map[string][2]float64{"weight": {0, 1}}}, ""); m {
		t.Error("matchable=true для несуществующего числового атрибута")
	}
	// skipAttr исключает партиционный атрибут из предиката.
	predSkip, _ := sa.compile(Filter{Eq: map[string]string{"cat": "x"}}, "cat")
	if !predSkip(2) { // k2 имеет cat=y, но cat пропущен → проходит
		t.Error("skipAttr не исключил атрибут из предиката")
	}
}

// =============================================================================
// Интеграция: PartitionAttr + AddWithAttrs → flush+merge → SearchFilter.
// Проверяет: (1) атрибуты переживают flush и merge; (2) SearchFilter точен против
// brute-GT, посчитанного по тем же данным и тому же фильтру.
// =============================================================================

func TestAttr_SearchFilterCorrectnessThroughMerge(t *testing.T) {
	const (
		dim = 24
		N   = 6000
		K   = 10
	)
	rng := rand.New(rand.NewSource(20260629))

	cfg := LeveledConfig{
		Distance:       EuclideanDistance,
		Allocator:      tcmalloc.NewTCMallocStore(4),
		DeltaMax:       1500, // мелкая дельта → много flush + merge (проверяем выживание атрибутов)
		Fanout:         4,
		EfSearch:       64,
		PartitionAttr:  "tenant",
		BruteThreshold: 800, // часть тенантов brute, часть graph
	}
	lvs := NewLeveledVectorStore(cfg)

	// Эталон в памяти: сохраняем key→vec→attrs для brute-GT.
	type rec struct {
		key         string
		vec         []float32
		tenant, cat string
		price       float64
	}
	tenants := []string{"t1", "t2", "t3"}
	cats := []string{"books", "toys"}
	recs := make([]rec, 0, N)
	for i := 0; i < N; i++ {
		v := make([]float32, dim)
		for d := range v {
			v[d] = rng.Float32()
		}
		r := rec{
			key:    "k" + strconv.Itoa(i),
			vec:    v,
			tenant: tenants[rng.Intn(len(tenants))],
			cat:    cats[rng.Intn(len(cats))],
			price:  float64(rng.Intn(100)),
		}
		recs = append(recs, r)
		if err := lvs.AddWithAttrs(r.key, r.vec, Attributes{
			Cat: map[string]string{"tenant": r.tenant, "cat": r.cat},
			Num: map[string]float64{"price": r.price},
		}); err != nil {
			t.Fatalf("AddWithAttrs: %v", err)
		}
	}
	settle(lvs)

	st := lvs.Stats()
	nseg := 0
	for _, n := range st.SegmentsByLevel {
		nseg += n
	}
	t.Logf("построено N=%d, segs=%d (%v)", st.TotalVectors, nseg, st.SegmentsByLevel)

	// brute-GT: точный top-K среди записей, прошедших фильтр.
	bruteGT := func(q []float32, f Filter) map[string]struct{} {
		type cand struct {
			key string
			d   float32
		}
		var top []cand
		for i := range recs {
			r := &recs[i]
			a := Attributes{
				Cat: map[string]string{"tenant": r.tenant, "cat": r.cat},
				Num: map[string]float64{"price": r.price},
			}
			if !matchAttrs(a, f, "") {
				continue
			}
			d := EuclideanDistance(q, r.vec)
			top = append(top, cand{r.key, d})
		}
		sort.Slice(top, func(i, j int) bool { return top[i].d < top[j].d })
		if len(top) > K {
			top = top[:K]
		}
		m := make(map[string]struct{}, len(top))
		for _, c := range top {
			m[c.key] = struct{}{}
		}
		return m
	}

	filters := []struct {
		name string
		f    Filter
	}{
		{"tenant", Filter{Eq: map[string]string{"tenant": "t1"}}},
		{"tenant+cat", Filter{Eq: map[string]string{"tenant": "t2", "cat": "books"}}},
		{"tenant+cat+price", Filter{Eq: map[string]string{"tenant": "t3", "cat": "toys"}, Range: map[string][2]float64{"price": {0, 50}}}},
	}

	nQ := 100
	for _, fc := range filters {
		var sumRec float64
		for qi := 0; qi < nQ; qi++ {
			q := make([]float32, dim)
			for d := range q {
				q[d] = rng.Float32()
			}
			res, err := lvs.SearchFilter(q, K, fc.f)
			if err != nil {
				t.Fatalf("SearchFilter: %v", err)
			}
			gt := bruteGT(q, fc.f)
			if len(gt) == 0 {
				continue
			}
			// Проверка корректности фильтра: каждый результат ДОЛЖЕН проходить фильтр.
			for _, rr := range res {
				idx, _ := strconv.Atoi(rr.Key[1:])
				rc := &recs[idx]
				a := Attributes{Cat: map[string]string{"tenant": rc.tenant, "cat": rc.cat}, Num: map[string]float64{"price": rc.price}}
				if !matchAttrs(a, fc.f, "") {
					t.Fatalf("[%s] результат %s НЕ проходит фильтр (tenant=%s cat=%s price=%v)",
						fc.name, rr.Key, rc.tenant, rc.cat, rc.price)
				}
			}
			hit := 0
			for _, rr := range res {
				if _, ok := gt[rr.Key]; ok {
					hit++
				}
			}
			sumRec += float64(hit) / float64(len(gt))
		}
		rec := sumRec / float64(nQ)
		fmt.Printf("[%s] recall@%d = %.3f\n", fc.name, K, rec)
		if rec < 0.95 {
			t.Errorf("[%s] recall %.3f < 0.95 — фильтр/раскладка теряют результаты", fc.name, rec)
		}
	}
}
