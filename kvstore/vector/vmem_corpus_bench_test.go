package vector

// =============================================================================
// Шаг 8 VMEM: корпус-бенч «жизнь агента» — суд КАЧЕСТВА (развилка А, 22.07:
// качество меряется на store-уровне со сжатым виртуальным временем; латентность
// — отдельным E2E-прогоном по RESP на реальных часах, cmd/vmemload).
//
// Критерии зафиксированы ДО кода (методика спринта 1); численные планки
// назначаются ПОСЛЕ первого бейзлайна (урок «hybrid≥vector» ill-posed):
//   - known-item hit@1/hit@10/MRR@10, с разбивкой по возрасту GT — это суд
//     отложенного риска «плоская шкала × мультипликативный decay»;
//   - paraphrase recall@10 (GT слабее, ранжирование под зипф-шумом);
//   - AS_OF accuracy: ровно та версия цепочки, соседи отсутствуют;
//   - ordering-пробы: порядок по множителю decay×imp (pairwise);
//   - ИНВАРИАНТЫ (ровно 0, не проценты): изоляция scope на каждом результате
//     каждого запроса; erasure (TTL/FORGET) невидим в default/ALL/AS_OF.
// Побочный выход: распределение длин RECALL-запросов — вход WAND-триггера.
//
// Жнец работает ПО ХОДУ ленты (каждые 15 виртуальных дней) — корректность
// выдачи от него не зависит (фильтр чтения), бенч это доказывает на корпусе.
// =============================================================================

import (
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"kvstore/kvstore/internal/vmemcorpus"
)

func TestVMEMCorpusBench(t *testing.T) {
	if testing.Short() {
		t.Skip("корпус-бенч: только полный прогон")
	}
	c := vmemcorpus.Generate(vmemcorpus.Default())
	t.Logf("корпус: %d событий, %d запросов, горизонт до NowV=%d", len(c.Events), len(c.Queries), c.NowV)
	for _, mode := range []struct {
		name   string
		hybrid bool
	}{
		{"stage0-bm25", false}, // ступень 0: факты без векторов, RECALL=BM25-only
		{"hybrid-bow", true},   // BYO: hashed-BoW → RRF-плечо (суд плоской шкалы)
	} {
		t.Run(mode.name, func(t *testing.T) { runVMEMCorpusBench(t, c, mode.hybrid) })
	}
}

func runVMEMCorpusBench(t *testing.T, c *vmemcorpus.Corpus, hybrid bool) {
	cfg := bm25TestConfig()
	cfg.DeltaMax = 4096 // живые flush/merge по ходу ленты, но без карликовых сегментов
	lvs := NewLeveledVectorStore(cfg)
	defer lvs.Close()

	// --- реплей ленты со сжатым временем + жнец каждые 15 виртуальных дней ---
	const day = int64(86400)
	reapEvery := 15 * day
	nextReap := c.Events[0].At + reapEvery
	reaped := 0
	for _, ev := range c.Events {
		for ev.At >= nextReap {
			reaped += lvs.ReapExpired(nextReap, 1<<20)
			nextReap += reapEvery
		}
		if ev.Fact == nil {
			if ok, err := lvs.ForgetInScope(ev.ForgetID, ev.ForgetScope); err != nil || !ok {
				t.Fatalf("Forget %s: ok=%v err=%v", ev.ForgetID, ok, err)
			}
			continue
		}
		f := ev.Fact
		req := RememberRequest{
			ID: f.ID, Scope: f.Scope, Text: f.Text, Type: f.Type,
			TTL: f.TTLSec, Supersedes: f.Supersedes,
		}
		if f.Imp >= 0 {
			imp := f.Imp
			req.Importance = &imp
		}
		if hybrid {
			req.Vector = f.Vec
		}
		if _, err := lvs.Remember(req, ev.At); err != nil {
			t.Fatalf("Remember %s: %v", f.ID, err)
		}
	}
	reaped += lvs.ReapExpired(c.NowV, 1<<20)
	lvs.FlushDeltaSync()

	// --- прогон запросов ------------------------------------------------------
	type sortStat struct {
		n, hit1, hit10, acc int
		mrr                 float64
		lat                 []time.Duration
	}
	stats := map[string]*sortStat{}
	// суд decay: known-item hit@1 по возрасту GT
	ageBuckets := map[string]*[2]int{"<30d": {}, "30-90d": {}, ">90d": {}} // [hit1, n]
	var pairOK, pairN, orderFull int
	isoViolations, erasureViolations, absentViolations := 0, 0, 0
	lenHist := map[int]int{}

	for _, q := range c.Queries {
		req := RecallRequest{Scope: q.Scope, Query: q.Text, K: q.K, All: q.All}
		if q.AsOf > 0 {
			asOf := q.AsOf
			req.AsOf = &asOf
		}
		if hybrid {
			req.Vector = q.Vec
		}
		t0 := time.Now()
		res, err := lvs.Recall(req, c.NowV)
		el := time.Since(t0)
		if err != nil {
			t.Fatalf("Recall %s %q: %v", q.Sort, q.Text, err)
		}
		st := stats[q.Sort]
		if st == nil {
			st = &sortStat{}
			stats[q.Sort] = st
		}
		st.n++
		st.lat = append(st.lat, el)
		lenHist[len(strings.Fields(q.Text))]++

		// инвариант изоляции: каждый результат каждого запроса — из scope запроса
		for _, r := range res {
			if sc, ok := c.ScopeOf[r.Key]; !ok || sc != q.Scope {
				isoViolations++
			}
		}

		switch q.Sort {
		case "known", "para":
			rank := 0
			for i, r := range res {
				if r.Key == q.WantID {
					rank = i + 1
					break
				}
			}
			if rank == 1 {
				st.hit1++
			}
			if rank >= 1 {
				st.hit10++
				st.mrr += 1 / float64(rank)
			}
			if q.Sort == "known" {
				b := "<30d"
				if q.AgeDays > 90 {
					b = ">90d"
				} else if q.AgeDays >= 30 {
					b = "30-90d"
				}
				ageBuckets[b][1]++
				if rank == 1 {
					ageBuckets[b][0]++
				}
			}
		case "asof", "nowchain":
			present := false
			leaked := false
			for _, r := range res {
				if r.Key == q.WantPresent[0] {
					present = true
				}
				if slices.Contains(q.WantAbsent, r.Key) {
					leaked = true
				}
			}
			if leaked {
				absentViolations++
			}
			if present && !leaked {
				st.acc++
			}
		case "order":
			pos := map[string]int{}
			for i, r := range res {
				pos[r.Key] = i + 1
			}
			full := true
			for i := 0; i < len(q.WantOrder); i++ {
				for j := i + 1; j < len(q.WantOrder); j++ {
					pi, iOK := pos[q.WantOrder[i]]
					pj, jOK := pos[q.WantOrder[j]]
					pairN++
					if iOK && jOK && pi < pj {
						pairOK++
					} else {
						full = false
					}
				}
			}
			if full {
				orderFull++
			}
		case "erasure":
			for _, r := range res {
				if slices.Contains(q.WantAbsent, r.Key) {
					erasureViolations++
				}
			}
		}
	}

	// --- отчёт ------------------------------------------------------------------
	pct := func(lat []time.Duration) (time.Duration, time.Duration) {
		slices.Sort(lat)
		return lat[len(lat)/2], lat[len(lat)*99/100]
	}
	t.Logf("жнец по ходу ленты: пожато %d фактов", reaped)
	for _, s := range []string{"known", "para", "asof", "nowchain", "order", "erasure"} {
		st := stats[s]
		if st == nil {
			continue
		}
		p50, p99 := pct(st.lat)
		switch s {
		case "known", "para":
			t.Logf("%-8s n=%d  hit@1=%.3f  hit@10=%.3f  MRR@10=%.3f  (store-lat p50=%v p99=%v)",
				s, st.n, float64(st.hit1)/float64(st.n), float64(st.hit10)/float64(st.n), st.mrr/float64(st.n), p50, p99)
		case "asof", "nowchain":
			t.Logf("%-8s n=%d  accuracy=%.3f  (store-lat p50=%v p99=%v)",
				s, st.n, float64(st.acc)/float64(st.n), p50, p99)
		case "order":
			t.Logf("%-8s n=%d  pairwise=%.3f  full-order=%d/%d  (store-lat p50=%v p99=%v)",
				s, st.n, float64(pairOK)/float64(pairN), orderFull, st.n, p50, p99)
		case "erasure":
			t.Logf("%-8s n=%d  (инвариант, см. ниже)", s, st.n)
		}
	}
	t.Logf("суд decay (known-item hit@1 по возрасту GT):")
	for _, b := range []string{"<30d", "30-90d", ">90d"} {
		v := ageBuckets[b]
		if v[1] > 0 {
			t.Logf("  возраст %-7s hit@1=%.3f (n=%d)", b, float64(v[0])/float64(v[1]), v[1])
		}
	}
	lens := make([]int, 0, len(lenHist))
	for l := range lenHist {
		lens = append(lens, l)
	}
	slices.Sort(lens)
	var lb strings.Builder
	for _, l := range lens {
		fmt.Fprintf(&lb, " %d:%d", l, lenHist[l])
	}
	t.Logf("длины RECALL-запросов (токенов:запросов, вход WAND-триггера):%s", lb.String())

	if isoViolations > 0 {
		t.Errorf("ИНВАРИАНТ изоляции scope нарушен: %d чужих результатов", isoViolations)
	}
	if erasureViolations > 0 {
		t.Errorf("ИНВАРИАНТ erasure нарушен: %d появлений стёртых фактов", erasureViolations)
	}
	if absentViolations > 0 {
		t.Errorf("темпоральность: %d запросов с просочившимися чужими версиями цепочки", absentViolations)
	}
}
