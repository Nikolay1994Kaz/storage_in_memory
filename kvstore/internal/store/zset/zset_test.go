package zset

import (
	"fmt"
	"math"
	"math/rand"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
)

// ── Функциональные тесты ────────────────────────────────────

func TestZAddZScore(t *testing.T) {
	reg := New(nil)
	if !reg.ZAdd("prices", 9.99, "apple") {
		t.Fatal("ожидали новый member")
	}
	if score, ok := reg.ZScore("prices", "apple"); !ok || score != 9.99 {
		t.Fatalf("ZScore = (%v, %v), want (9.99, true)", score, ok)
	}
	if reg.ZAdd("prices", 19.99, "apple") {
		t.Fatal("смена score — не новый member")
	}
	if reg.ZAdd("prices", 19.99, "apple") {
		t.Fatal("тот же score — не новый member")
	}
	if score, ok := reg.ZScore("prices", "apple"); !ok || score != 19.99 {
		t.Fatalf("ZScore после смены = (%v, %v), want (19.99, true)", score, ok)
	}
	// Старая позиция в дереве убрана: в диапазоне один member.
	if got := reg.ZRangeByScore("prices", 0, 100); len(got) != 1 || got[0].Score != 19.99 {
		t.Fatalf("после смены score в диапазоне %v", got)
	}
}

func TestZRem(t *testing.T) {
	reg := New(nil)
	reg.ZAdd("prices", 9.99, "apple")
	reg.ZAdd("prices", 19.99, "banana")
	if !reg.ZRem("prices", "apple") || reg.ZRem("prices", "apple") {
		t.Fatal("ZRem: первый вызов true, второй false")
	}
	if _, ok := reg.ZScore("prices", "apple"); ok {
		t.Fatal("apple остался в обратном индексе")
	}
	if reg.ZCard("prices") != 1 {
		t.Fatalf("ZCard = %d, want 1", reg.ZCard("prices"))
	}
	if reg.ZRem("nosuchset", "x") {
		t.Fatal("ZRem из несуществующего множества")
	}
}

func TestZRangeByScore(t *testing.T) {
	reg := New(nil)
	reg.ZAdd("prices", 5.0, "cheap")
	reg.ZAdd("prices", 50.0, "mid")
	reg.ZAdd("prices", 500.0, "expensive")
	reg.ZAdd("prices", 5000.0, "luxury")
	results := reg.ZRangeByScore("prices", 10, 1000)
	if len(results) != 2 || results[0].Member != "mid" || results[1].Member != "expensive" {
		t.Fatalf("ZRangeByScore(10,1000) = %v", results)
	}
}

// Регресс к найденному 03.10: равные score, растянутые на несколько листов
// дерева. Прежняя реализация отдавала 1 из 300 на ZRANGEBYSCORE 5 5, 51 из 350
// на 5 9 — и так же резала фильтр VSIM.SEARCHRANGE.
func TestEqualScoresSpanningLeaves(t *testing.T) {
	reg := New(nil)
	for i := 0; i < 300; i++ {
		reg.ZAdd("z", 5, "m"+strconv.Itoa(i))
	}
	for i := 0; i < 50; i++ {
		reg.ZAdd("z", 1, "low"+strconv.Itoa(i))
		reg.ZAdd("z", 9, "high"+strconv.Itoa(i))
	}
	if n := len(reg.ZRangeByScore("z", 5, 5)); n != 300 {
		t.Fatalf("ZRANGEBYSCORE 5 5 = %d, ожидали 300", n)
	}
	if n := len(reg.ZRangeByScore("z", 5, 9)); n != 350 {
		t.Fatalf("ZRANGEBYSCORE 5 9 = %d, ожидали 350", n)
	}
	if n := len(reg.MembersInRange("z", 5, 5)); n != 300 {
		t.Fatalf("MembersInRange(5,5) = %d, ожидали 300", n)
	}
	// Внутри одного score — алфавитный порядок member, как в Redis.
	res := reg.ZRangeByScore("z", 5, 5)
	for i := 1; i < len(res); i++ {
		if res[i-1].Member >= res[i].Member {
			t.Fatalf("порядок нарушен: %q перед %q", res[i-1].Member, res[i].Member)
		}
	}
}

func TestMembersInRange(t *testing.T) {
	reg := New(nil)
	for i := 0; i < 1000; i++ {
		reg.ZAdd("prices", float64(i), fmt.Sprintf("item:%d", i))
	}
	allowed := reg.MembersInRange("prices", 100, 200)
	if len(allowed) != 101 {
		t.Fatalf("MembersInRange(100,200) = %d, want 101", len(allowed))
	}
	if _, ok := allowed["item:150"]; !ok {
		t.Fatal("item:150 не попал в диапазон")
	}
	if _, ok := allowed["item:201"]; ok {
		t.Fatal("item:201 попал в диапазон")
	}
	if reg.MembersInRange("prices", 5000, 6000) != nil || reg.MembersInRange("nosuchset", 0, 1) != nil {
		t.Fatal("пустой диапазон — nil")
	}
}

func TestMultipleSortedSets(t *testing.T) {
	reg := New(nil)
	reg.ZAdd("prices", 9.99, "apple")
	reg.ZAdd("ratings", 4.5, "apple")
	if s, _ := reg.ZScore("prices", "apple"); s != 9.99 {
		t.Fatal("prices:apple должен быть 9.99")
	}
	if s, _ := reg.ZScore("ratings", "apple"); s != 4.5 {
		t.Fatal("ratings:apple должен быть 4.5")
	}
}

// NaN не попадает в дерево: в упорядоченном индексе он ломает сравнение.
func TestZAddRejectsNaN(t *testing.T) {
	reg := New(nil)
	if reg.ZAdd("z", math.NaN(), "m") {
		t.Fatal("NaN принят")
	}
	if reg.ZCard("z") != 0 {
		t.Fatal("NaN попал в множество")
	}
	reg.ZAdd("z", 1, "m")
	if reg.ZAdd("z", math.NaN(), "m") {
		t.Fatal("NaN принят как новый score")
	}
	if s, _ := reg.ZScore("z", "m"); s != 1 {
		t.Fatalf("NaN перезаписал score: %v", s)
	}
}

// Опустевшее множество снимается с реестра: имя не живёт вечно, а снапшот не
// тащит пустые записи. Повторный ZADD создаёт множество заново.
func TestEmptySetIsRemoved(t *testing.T) {
	reg := New(nil)
	reg.ZAdd("tmp", 1, "a")
	reg.ZAdd("tmp", 2, "b")
	reg.ZRem("tmp", "a")
	reg.ZRem("tmp", "b")
	reg.ForEachSet(func(name string) {
		t.Fatalf("пустое множество %q осталось в реестре", name)
	})
	if !reg.ZAdd("tmp", 3, "c") || reg.ZCard("tmp") != 1 {
		t.Fatal("после опустошения множество не создаётся заново")
	}
}

// Учёт памяти возвращается в ноль, когда всё удалено, — тот же счётчик, что у
// гейта maxmemory.
func TestMemoryAccounting(t *testing.T) {
	var mem atomic.Int64
	reg := New(&mem)
	for i := 0; i < 1000; i++ {
		reg.ZAdd("s"+strconv.Itoa(i%7), float64(i), "member-"+strconv.Itoa(i))
	}
	if mem.Load() <= 1000*memberOverhead {
		t.Fatalf("учтено %d байт на 1000 member — подозрительно мало", mem.Load())
	}
	for i := 0; i < 1000; i++ {
		reg.ZAdd("s"+strconv.Itoa(i%7), float64(i+1), "member-"+strconv.Itoa(i)) // смена score не меняет объём
	}
	for i := 0; i < 1000; i++ {
		reg.ZRem("s"+strconv.Itoa(i%7), "member-"+strconv.Itoa(i))
	}
	if mem.Load() != 0 {
		t.Fatalf("после удаления всего учтено %d байт, ожидали 0", mem.Load())
	}
}

func TestForEachInOrder(t *testing.T) {
	reg := New(nil)
	for _, i := range rand.New(rand.NewSource(1)).Perm(500) {
		reg.ZAdd("z", float64(i%50), "m"+strconv.Itoa(i))
	}
	var prev RangeResult
	n := 0
	reg.ForEach("z", func(score float64, member string) {
		if n > 0 && (score < prev.Score || (score == prev.Score && member <= prev.Member)) {
			t.Fatalf("порядок нарушен: (%v,%q) после (%v,%q)", score, member, prev.Score, prev.Member)
		}
		prev = RangeResult{score, member}
		n++
	})
	if n != 500 {
		t.Fatalf("ForEach обошёл %d, ожидали 500", n)
	}
}

// ── Конкурентность (гонять под -race) ───────────────────────

// Страж S3: ZADD — check-then-act. Без сериализации на множество два ZADD
// одного member оба видят «старый score» и оба вставляют → дубль в дереве и
// рассинхрон с обратным индексом.
func TestZAdd_ConcurrentSameMember_NoDuplicates(t *testing.T) {
	const g, iters = 8, 500
	reg := New(nil)
	reg.ZAdd("s", 0, "m")
	var wg sync.WaitGroup
	for w := 0; w < g; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := 0; i < iters; i++ {
				reg.ZAdd("s", float64(w*iters+i+1), "m")
			}
		}(w)
	}
	wg.Wait()
	got := reg.ZRangeByScore("s", math.Inf(-1), math.Inf(1))
	sc, ok := reg.ZScore("s", "m")
	if reg.ZCard("s") != 1 || len(got) != 1 || !ok || got[0].Score != sc {
		t.Fatalf("ZCard=%d, в дереве %v, ZScore=(%v,%v)", reg.ZCard("s"), got, sc, ok)
	}
}

// Опустошение множества гоняется с ZADD в него же: ZADD, взявший запись до
// снятия с реестра, не должен писать в запись, которой уже нет.
func TestConcurrentEmptyAndRefill(t *testing.T) {
	reg := New(nil)
	var wg sync.WaitGroup
	const rounds = 2000
	added := make([]atomic.Bool, rounds)
	wg.Add(2)
	go func() {
		defer wg.Done()
		for i := 0; i < rounds; i++ {
			reg.ZAdd("flip", float64(i), "m"+strconv.Itoa(i))
			added[i].Store(true)
		}
	}()
	go func() {
		defer wg.Done()
		for i := 0; i < rounds; i++ {
			for !added[i].Load() {
				// ждём, пока member появится, и сразу удаляем: множество
				// постоянно пустеет и снимается с реестра
			}
			if !reg.ZRem("flip", "m"+strconv.Itoa(i)) {
				t.Errorf("member m%d потерян: ZADD ушёл в снятую запись", i)
				return
			}
		}
	}()
	wg.Wait()
	if n := reg.ZCard("flip"); n != 0 {
		t.Fatalf("ZCard=%d после удаления всех", n)
	}
}

// Разные множества с читателями поверх — под -race.
func TestConcurrentSetsAndReaders(t *testing.T) {
	reg := New(nil)
	const writers, iters = 6, 2000
	var wg sync.WaitGroup
	stop := make(chan struct{})
	for r := 0; r < 4; r++ {
		wg.Add(1)
		go func(r int) {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				set := "set" + strconv.Itoa(r%writers)
				reg.ZRangeByScore(set, 10, 500)
				reg.MembersInRange(set, 0, 100)
				reg.ZScore(set, "m7")
				reg.ZCard(set)
			}
		}(r)
	}
	var ww sync.WaitGroup
	for w := 0; w < writers; w++ {
		ww.Add(1)
		go func(w int) {
			defer ww.Done()
			set := "set" + strconv.Itoa(w)
			for i := 0; i < iters; i++ {
				reg.ZAdd(set, float64(i%700), "m"+strconv.Itoa(i))
				if i%3 == 0 {
					reg.ZRem(set, "m"+strconv.Itoa(i/2))
				}
			}
		}(w)
	}
	ww.Wait()
	close(stop)
	wg.Wait()
	for w := 0; w < writers; w++ {
		set := "set" + strconv.Itoa(w)
		if a, b := reg.ZCard(set), len(reg.ZRangeByScore(set, math.Inf(-1), math.Inf(1))); a != b {
			t.Fatalf("%s: ZCard=%d, в дереве %d", set, a, b)
		}
	}
}

// ── Бенчмарки ──────────────────────────────────────────────

func benchReg(n int) (*ZSetRegistry, []string) {
	reg := New(nil)
	keys := make([]string, n)
	for i := range keys {
		keys[i] = "item:" + strconv.Itoa(i)
		reg.ZAdd("bench", float64(i), keys[i])
	}
	return reg, keys
}

func BenchmarkZAdd(b *testing.B) {
	reg := New(nil)
	keys := make([]string, 1<<16)
	for i := range keys {
		keys[i] = "item:" + strconv.Itoa(i)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		reg.ZAdd("bench", float64(i), keys[i&(1<<16-1)])
	}
}

func BenchmarkZScoreParallel(b *testing.B) {
	reg, keys := benchReg(100_000)
	b.ReportAllocs()
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		i := 0
		for pb.Next() {
			reg.ZScore("bench", keys[i%len(keys)])
			i++
		}
	})
}

func BenchmarkZRangeByScore100(b *testing.B) {
	reg, _ := benchReg(100_000)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		start := float64(i % 99_000)
		reg.ZRangeByScore("bench", start, start+99)
	}
}

func BenchmarkMembersInRangeParallel(b *testing.B) {
	reg, _ := benchReg(100_000)
	b.ReportAllocs()
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		rng := rand.New(rand.NewSource(42))
		for pb.Next() {
			start := float64(rng.Intn(99_000))
			reg.MembersInRange("bench", start, start+100)
		}
	})
}
