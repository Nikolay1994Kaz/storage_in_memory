package vector

import (
	"fmt"
	"math/rand"
	"slices"
	"sync"
	"testing"
	"time"
)

// vmemRememberT — Remember с фатальным исходом на ошибке (шорткат тестов жнеца).
func vmemRememberT(t *testing.T, lvs *LeveledVectorStore, req RememberRequest, now int64) {
	t.Helper()
	if _, err := lvs.Remember(req, now); err != nil {
		t.Fatalf("Remember %s: %v", req.ID, err)
	}
}

// TestVMEMReapExpired — жнец по трём LSM-состояниям: истёкшие стёрты, живые и
// бессрочные нетронуты, повторная жатва пуста (идемпотентность: tombstoned
// копии в сегментах снова попадают в скан, но перепроверка их милует).
func TestVMEMReapExpired(t *testing.T) {
	for _, st := range vmemLSMStates {
		t.Run(st.name, func(t *testing.T) {
			lvs := NewLeveledVectorStore(bm25TestConfig())
			defer lvs.Close()
			ingest := []RememberRequest{
				{ID: "dead:0", Scope: "user:dana", Text: "old login token", TTL: 100},
				{ID: "dead:1", Scope: "user:dana", Text: "temp meeting room", TTL: 100},
				{ID: "dead:2", Scope: "user:bob", Text: "one time code", TTL: 500},
				{ID: "live:0", Scope: "user:dana", Text: "long project deadline", TTL: 50000},
				{ID: "live:1", Scope: "user:dana", Text: "favorite color green"},
				{ID: "live:2", Scope: "user:bob", Text: "works at clinic"},
			}
			flushAfter := st.flushAt(len(ingest))
			for i, req := range ingest {
				vmemRememberT(t, lvs, req, 1000)
				if i+1 == flushAfter {
					lvs.FlushDeltaSync()
				}
			}

			if n := lvs.ReapExpired(2000, vmemReapTickLimit); n != 3 {
				t.Fatalf("жатва: пожато %d, ожидалось 3", n)
			}
			for _, id := range []string{"dead:0", "dead:1", "dead:2"} {
				if _, ok := lvs.Get(id); ok {
					t.Errorf("%s: истёкший факт жив после жатвы", id)
				}
			}
			for _, id := range []string{"live:0", "live:1", "live:2"} {
				if _, ok := lvs.Get(id); !ok {
					t.Errorf("%s: живой факт задет жнецом", id)
				}
			}
			if n := lvs.ReapExpired(2000, vmemReapTickLimit); n != 0 {
				t.Fatalf("повторная жатва: пожато %d, ожидалось 0", n)
			}
		})
	}
}

// TestVMEMReapBatchLimit — limit ограничивает батч, хвост дожинается следующим
// проходом (контракт idle-тика: фоновая работа уступает порциями).
func TestVMEMReapBatchLimit(t *testing.T) {
	lvs := NewLeveledVectorStore(bm25TestConfig())
	defer lvs.Close()
	for i := 0; i < 5; i++ {
		vmemRememberT(t, lvs, RememberRequest{
			ID: fmt.Sprintf("d%d", i), Scope: "s", Text: "ephemeral note", TTL: 10,
		}, 1000)
	}
	lvs.FlushDeltaSync()
	if n := lvs.ReapExpired(2000, 2); n != 2 {
		t.Fatalf("батч 2: пожато %d", n)
	}
	if n := lvs.ReapExpired(2000, 100); n != 3 {
		t.Fatalf("хвост: пожато %d, ожидалось 3", n)
	}
}

// TestVMEMReapRecheckWins — гонка «жнец собрал жертв → параллельный Remember
// освежил факт»: перепроверка под замком обязана помиловать освежённых
// («поздний прав»). Интерливинг детерминированный: фазы скана и приговора
// вызываются раздельно, освежение вклинивается между ними; затенённая старая
// копия в сегменте (освежение ДО скана) милуется тем же механизмом.
func TestVMEMReapRecheckWins(t *testing.T) {
	lvs := NewLeveledVectorStore(bm25TestConfig())
	defer lvs.Close()
	vmemRememberT(t, lvs, RememberRequest{ID: "f1", Scope: "s", Text: "session alpha", TTL: 100}, 1000)
	vmemRememberT(t, lvs, RememberRequest{ID: "f2", Scope: "s", Text: "session beta", TTL: 100}, 1000)
	vmemRememberT(t, lvs, RememberRequest{ID: "f3", Scope: "s", Text: "session gamma", TTL: 100}, 1000)
	lvs.FlushDeltaSync() // все истёкшие копии — во frozen-колонках

	// f2 освежён ДО скана: старая истёкшая копия в сегменте затенена свежей.
	vmemRememberT(t, lvs, RememberRequest{ID: "f2", Scope: "s", Text: "session beta", TTL: 50000}, 2000)

	cands := lvs.collectExpired(2000, vmemReapTickLimit)
	slices.Sort(cands)
	if !slices.Equal(cands, []string{"f1", "f2", "f3"}) {
		t.Fatalf("скан: кандидаты %v (затенённая копия f2 обязана попасть в черновик)", cands)
	}

	// f1 освежён ПОСЛЕ скана — окно гонки между фазами.
	vmemRememberT(t, lvs, RememberRequest{ID: "f1", Scope: "s", Text: "session alpha", TTL: 50000}, 2000)

	if n := lvs.reapKeys(cands, 2000); n != 1 {
		t.Fatalf("приговор: пожато %d, ожидалось 1 (только f3)", n)
	}
	for _, id := range []string{"f1", "f2"} {
		if _, ok := lvs.Get(id); !ok {
			t.Errorf("%s: освежённый факт убит жнецом («поздний прав» нарушен)", id)
		}
	}
	if _, ok := lvs.Get("f3"); ok {
		t.Error("f3: истёкший факт пережил приговор")
	}
}

// TestVMEMReapOracleParity — инвариант шага 6: агрессивный жнец (жатва после
// КАЖДОЙ операции ленты и перед КАЖДЫМ запросом) не меняет ни одного
// golden-ответа — erasure решается фильтром чтения, жнец лишь возвращает
// ресурсы. Зеркало TestVMEMOracleParity ×3 LSM-состояния.
func TestVMEMReapOracleParity(t *testing.T) {
	all := loadVMEMScenarios(t)
	for _, sc := range all.Scenarios {
		for _, st := range vmemLSMStates {
			t.Run(sc.Name+"/"+st.name, func(t *testing.T) {
				lvs := NewLeveledVectorStore(bm25TestConfig())
				defer lvs.Close()
				flushAfter := st.flushAt(len(sc.Ops))
				for i, op := range sc.Ops {
					switch op.Op {
					case "remember":
						vmemRememberT(t, lvs, RememberRequest{
							ID: op.ID, Scope: op.Scope, Text: op.Text, Type: op.Type,
							Importance: op.Importance, TTL: op.TTL, Supersedes: op.Supersedes,
						}, op.At)
					case "forget":
						if !lvs.Forget(op.ID) {
							t.Fatalf("op[%d] forget %s: Forget=false", i, op.ID)
						}
					}
					lvs.ReapExpired(op.At, vmemReapTickLimit)
					if i+1 == flushAfter {
						lvs.FlushDeltaSync()
					}
				}

				facts := vmemReplay(t, sc.Name, sc.Ops, true)
				K := len(facts) + 8
				for _, q := range sc.Queries {
					lvs.ReapExpired(q.Now, vmemReapTickLimit)
					req := RecallRequest{
						Scope: q.Scope, Query: q.Query, K: K,
						AsOf: q.AsOf, All: q.All, TypeEq: q.TypeEq,
					}
					res, err := lvs.Recall(req, q.Now)
					if err != nil {
						t.Fatalf("%s: Recall: %v", q.ID, err)
					}
					if q.ExpectFirst != "" {
						if len(res) == 0 || res[0].Key != q.ExpectFirst {
							t.Errorf("%s: первый в выдаче %v, golden ожидает %q", q.ID, res, q.ExpectFirst)
						}
					}
					got := make([]string, 0, len(res))
					for _, r := range res {
						got = append(got, r.Key)
					}
					slices.Sort(got)
					want := vmemModelRecall(facts, q)
					if !slices.Equal(got, want) {
						t.Errorf("%s: живой Recall с жнецом даёт %v, модель — %v", q.ID, got, want)
					}
				}
			})
		}
	}
}

// TestVMEMSupersedeExpiredTarget — успех supersedes не зависит от расписания
// жнеца: цель, истёкшая по TTL, отвергается ОДИНАКОВО до жатвы (физически
// жива, но erasure судится правилом чтения) и после (стёрта).
func TestVMEMSupersedeExpiredTarget(t *testing.T) {
	lvs := NewLeveledVectorStore(bm25TestConfig())
	defer lvs.Close()
	vmemRememberT(t, lvs, RememberRequest{ID: "old", Scope: "s", Text: "expiring fact", TTL: 100}, 1000)
	lvs.FlushDeltaSync()

	heir := RememberRequest{ID: "new", Scope: "s", Text: "the replacement", Supersedes: "old"}
	if _, err := lvs.Remember(heir, 2000); err != ErrVMEMSupersedesTarget {
		t.Fatalf("до жатвы: err=%v, ожидался ErrVMEMSupersedesTarget", err)
	}
	if n := lvs.ReapExpired(2000, vmemReapTickLimit); n != 1 {
		t.Fatalf("жатва: пожато %d", n)
	}
	if _, err := lvs.Remember(heir, 2000); err != ErrVMEMSupersedesTarget {
		t.Fatalf("после жатвы: err=%v, ожидался ErrVMEMSupersedesTarget", err)
	}
	// А до истечения цели замена легальна.
	vmemRememberT(t, lvs, RememberRequest{ID: "old2", Scope: "s", Text: "expiring fact two", TTL: 100}, 1000)
	if _, err := lvs.Remember(RememberRequest{ID: "new2", Scope: "s", Text: "replacement two", Supersedes: "old2"}, 1050); err != nil {
		t.Fatalf("замена живой цели: %v", err)
	}
}

// TestVMEMReapFlushingWindow — жатва в окне flush-visibility (дельта свапнута,
// сегмент не опубликован): истёкший факт живёт только во flushing-дельте, жнец
// обязан найти его сканом flushing и стереть tombstone'ом, который переживает
// публикацию сегмента. Пришпиливание окна — buildSem-приём.
func TestVMEMReapFlushingWindow(t *testing.T) {
	lvs := NewLeveledVectorStore(leveledConfigForVisibility())
	defer lvs.Clear()

	lvs.buildSem <- struct{}{}
	vmemRememberT(t, lvs, RememberRequest{ID: "f", Scope: "s", Text: "short lived", TTL: 100}, 1000)
	lvs.triggerCompact()
	waitUntil(t, func() bool {
		lvs.mu.RLock()
		defer lvs.mu.RUnlock()
		deltaEmpty := lvs.delta == nil || lvs.delta.Len() == 0
		nSegs := 0
		for i := range lvs.levels {
			nSegs += len(lvs.levels[i])
		}
		return deltaEmpty && nSegs == 0 && lvs.inFlightBuilds.Load() > 0
	})

	n := lvs.ReapExpired(2000, vmemReapTickLimit)
	<-lvs.buildSem
	if n != 1 {
		t.Fatalf("жатва в окне: пожато %d", n)
	}
	waitUntil(t, func() bool { return lvs.inFlightBuilds.Load() == 0 })
	if _, ok := lvs.Get("f"); ok {
		t.Fatal("факт воскрес после публикации сегмента (tombstone не пережил build)")
	}
}

// TestVMEMReapPhysicalSweep — «стёрто — значит стёрто, проверить, а не
// поверить»: жатва ставит tombstones (копии физически в сегментах), а
// idle-консолидация выметает их — после неё истёкших нет и физически.
func TestVMEMReapPhysicalSweep(t *testing.T) {
	lvs := NewLeveledVectorStore(bm25TestConfig())
	defer lvs.Close()
	for i := 0; i < 8; i++ {
		vmemRememberT(t, lvs, RememberRequest{
			ID: fmt.Sprintf("dead:%d", i), Scope: "s", Text: fmt.Sprintf("stale item %d", i), TTL: 100,
		}, 1000)
	}
	lvs.FlushDeltaSync()
	for i := 0; i < 8; i++ {
		vmemRememberT(t, lvs, RememberRequest{
			ID: fmt.Sprintf("live:%d", i), Scope: "s", Text: fmt.Sprintf("keeper item %d", i),
		}, 1000)
	}
	lvs.FlushDeltaSync() // два сегмента → есть что консолидировать

	if n := lvs.ReapExpired(2000, vmemReapTickLimit); n != 8 {
		t.Fatalf("жатва: пожато %d", n)
	}
	lvs.mu.RLock()
	phys := lvs.keyPhysicallyInSegmentsLocked("dead:0")
	lvs.mu.RUnlock()
	if !phys {
		t.Fatal("до консолидации копия обязана лежать в сегменте (жатва — это tombstone)")
	}

	lvs.handleIdleTick() // форс idle-консолидации (окно в тестовом cfg = 0)
	waitUntil(t, func() bool {
		lvs.mu.RLock()
		defer lvs.mu.RUnlock()
		nSegs := 0
		for i := range lvs.levels {
			nSegs += len(lvs.levels[i])
		}
		return nSegs == 1 && !lvs.consolidateInFlight
	})

	lvs.mu.RLock()
	phys = lvs.keyPhysicallyInSegmentsLocked("dead:0")
	lvs.mu.RUnlock()
	if phys {
		t.Fatal("после консолидации истёкший факт остался в сегменте физически")
	}
	for i := 0; i < 8; i++ {
		if _, ok := lvs.Get(fmt.Sprintf("live:%d", i)); !ok {
			t.Fatalf("live:%d потерян при жатве+консолидации", i)
		}
	}
}

// TestVMEMReapWritersRace — писатели против жнеца под -race: освежающие
// upsert'ы и жатва конкурируют за одни id; проверяется отсутствием гонок
// детектором, инвариант «поздний прав» — предыдущими тестами.
func TestVMEMReapWritersRace(t *testing.T) {
	lvs := NewLeveledVectorStore(bm25TestConfig())
	defer lvs.Close()
	const ids = 50
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		for i := 0; i < 400; i++ {
			_, _ = lvs.Remember(RememberRequest{
				ID:    fmt.Sprintf("f%d", i%ids),
				Scope: "s", Text: "contended fact", TTL: int64(1 + i%3),
			}, int64(1000+i))
		}
	}()
	go func() {
		defer wg.Done()
		for i := 0; i < 400; i++ {
			lvs.ReapExpired(int64(1000+i), 64)
		}
	}()
	wg.Wait()
}

// TestVMEMReapQPSProbe — порог 3 шага 6 (объявлен до прогона): непрерывная
// жатва (скан+приговор в цикле) на корпусе с 10k истёкших не роняет
// параллельный RECALL ниже 0.8× базовой медианы. A/B на идентичных сторах.
func TestVMEMReapQPSProbe(t *testing.T) {
	if testing.Short() {
		t.Skip("профит-бенч: только полный прогон")
	}
	const (
		corpusN = 20000
		vocab   = 20000
		nTok    = 30
		nQ      = 40000
	)
	build := func() *LeveledVectorStore {
		cfg := bm25TestConfig()
		cfg.DeltaMax = corpusN
		lvs := NewLeveledVectorStore(cfg)
		rng := rand.New(rand.NewSource(7))
		for i := 0; i < corpusN; i++ {
			req := RememberRequest{
				ID:    fmt.Sprintf("fact:%05d", i),
				Scope: fmt.Sprintf("user:%03d", i%100),
				Text:  vmemSynthText(rng, vocab, nTok),
			}
			if i%2 == 0 {
				req.TTL = 500 // истекает к моменту запросов (now=2000)
			}
			if _, err := lvs.Remember(req, 1000); err != nil {
				t.Fatalf("Remember корпуса: %v", err)
			}
		}
		lvs.FlushDeltaSync()
		return lvs
	}
	mkQueries := func() []RecallRequest {
		rng := rand.New(rand.NewSource(11))
		qs := make([]RecallRequest, nQ)
		for i := range qs {
			qs[i] = RecallRequest{
				Scope: fmt.Sprintf("user:%03d", rng.Intn(100)),
				Query: vmemSynthText(rng, vocab, 4),
				K:     10,
			}
		}
		return qs
	}
	runQPS := func(lvs *LeveledVectorStore, qs []RecallRequest) float64 {
		t0 := time.Now()
		var tQuarter time.Duration
		for i := range qs {
			if _, err := lvs.Recall(qs[i], 2000); err != nil {
				t.Fatalf("Recall: %v", err)
			}
			if i == len(qs)/4 {
				tQuarter = time.Since(t0)
			}
		}
		el := time.Since(t0)
		t.Logf("  первая четверть: %.0f QPS, хвост: %.0f QPS",
			float64(nQ/4)/tQuarter.Seconds(), float64(3*nQ/4)/(el-tQuarter).Seconds())
		return float64(nQ) / el.Seconds()
	}

	base := build()
	baseQPS := runQPS(base, mkQueries())
	base.Close()

	lvs := build()
	defer lvs.Close()
	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	var passes, reaped int
	tHarvest := time.Duration(0)
	tStart := time.Now()
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
				n := lvs.ReapExpired(2000, 256) // после выжатия корпуса — непрерывные холостые сканы
				passes++
				reaped += n
				if n > 0 {
					tHarvest = time.Since(tStart)
				}
			}
		}
	}()
	reapQPS := runQPS(lvs, mkQueries())
	close(stop)
	wg.Wait()
	t.Logf("  жнец: %d проходов, пожато %d, жатва завершена за %v", passes, reaped, tHarvest)

	ratio := reapQPS / baseQPS
	t.Logf("RECALL без жнеца: %.0f QPS; под непрерывной жатвой: %.0f QPS; отношение %.2f× (порог ≥0.8×)", baseQPS, reapQPS, ratio)
	if ratio < 0.8 {
		t.Fatalf("порог 3 провален: RECALL под жатвой %.2f× базового (<0.8×)", ratio)
	}
}
