package vector

// =============================================================================
// Суд decay-кандидатов (23.07, продолжение П2): дефект «плоский RRF ×
// мультипликативный decay» подтверждён на реальных эмбеддингах (дельта 0.958),
// выбираем ФОРМУЛУ-ФИКС измерением, не мнением.
//
// Метод — симуляция до кода: плечи берутся через публичные API (SearchTextFilter
// / SearchFilter, те же depth=100 и фильтр, что строит Recall), все формулы
// считаются в бенче на известных возрастах фактов. В прод-Recall поедет только
// победитель. Вариант cur воспроизводит текущую формулу — сверка с живым
// Recall валидирует симуляцию.
//
// Кандидаты:
//   floor(f)   — множитель decay не падает ниже f: fused × max(2^(−a/HL), f);
//   prefusion  — каждое плечо пере-ранжируется по (скор плеча × decay) ДО RRF,
//                пост-множителя нет: возраст двигает ранги, не квантует скор;
//   rank(λ)    — возраст платит в ранговой шкале RRF: Σ 1/(rrfK + r + λ·a/HL).
//
// Критерии (утверждены до прогона, 23.07):
//   (а) hybrid known/>90d hit@10 ≥ 0.95 (возврат к уровню bm25+decay и выше);
//   (б) свежее не деградирует: hybrid known/<30d hit@1 ≥ 0.95;
//   (в) бонус-разделитель: BM25-only para/>90d hit@10 ≥ 0.90 (сейчас 0.762).
// =============================================================================

import (
	"fmt"
	"math"
	"slices"
	"strconv"
	"testing"
)

// vmemJudgeCell — метрики одной ячейки (сорт × корзина).
type vmemJudgeCell struct {
	n, hit1, hit10 int
	mrr            float64
}

func (c *vmemJudgeCell) add(rankGT int) { // rankGT: 0-based, -1 = мимо top-10
	c.n++
	if rankGT == 0 {
		c.hit1++
	}
	if rankGT >= 0 {
		c.hit10++
		c.mrr += 1.0 / float64(rankGT+1)
	}
}

func TestVMEMDecayCandidatesJudge(t *testing.T) {
	if testing.Short() {
		t.Skip("эксперимент: только полный прогон")
	}
	lvs, _, vecs, scopeOf, atOf, queries := vmemLifeSetup(t)
	defer lvs.Close()

	halfLives := func(fact int) float64 { // возраст факта в half-life'ах
		return float64(vmemLifeNow-atOf[fact]) / float64(vmemLifeHL)
	}
	decayOf := func(fact int) float64 { return math.Exp2(-halfLives(fact)) }
	factOf := func(key string) int { // "fact:%05d"
		n, err := strconv.Atoi(key[len("fact:"):])
		if err != nil {
			t.Fatalf("чужой ключ в выдаче: %q", key)
		}
		return n
	}

	// Кандидат = функция (плечи → топ-10 фактов). Плечи уже отсортированы
	// движком; ранги 1-based как в SearchHybridFilter.
	type arm struct {
		fact  int
		score float64 // text: BM25-скор; vec: похожесть 1−dist
	}
	rankTop10 := func(score map[int]float64) []int {
		type kv struct {
			fact int
			s    float64
		}
		all := make([]kv, 0, len(score))
		for f, s := range score {
			all = append(all, kv{f, s})
		}
		slices.SortFunc(all, func(a, b kv) int { // desc, tie-break по id (как по ключу)
			if a.s > b.s {
				return -1
			}
			if a.s < b.s {
				return 1
			}
			return a.fact - b.fact
		})
		if len(all) > 10 {
			all = all[:10]
		}
		out := make([]int, len(all))
		for i := range all {
			out[i] = all[i].fact
		}
		return out
	}
	rrf := func(arms ...[]arm) map[int]float64 {
		fused := make(map[int]float64)
		for _, a := range arms {
			for r, h := range a {
				fused[h.fact] += 1.0 / float64(rrfK+r+1)
			}
		}
		return fused
	}
	reorderByDecay := func(a []arm) []arm {
		out := slices.Clone(a)
		slices.SortFunc(out, func(x, y arm) int {
			sx, sy := x.score*decayOf(x.fact), y.score*decayOf(y.fact)
			if sx > sy {
				return -1
			}
			if sx < sy {
				return 1
			}
			return x.fact - y.fact
		})
		return out
	}

	type candidate struct {
		name   string
		hybrid func(text, vec []arm) []int
		bm25   func(text []arm) []int
	}
	mulVariant := func(floor float64) candidate {
		name := "cur(mult)"
		if floor > 0 {
			name = fmt.Sprintf("floor(%.3f)", floor)
		}
		mul := func(fused map[int]float64) map[int]float64 {
			for f := range fused {
				fused[f] *= math.Max(decayOf(f), floor)
			}
			return fused
		}
		return candidate{
			name:   name,
			hybrid: func(text, vec []arm) []int { return rankTop10(mul(rrf(text, vec))) },
			bm25: func(text []arm) []int {
				score := make(map[int]float64, len(text))
				for _, h := range text {
					score[h.fact] = h.score * math.Max(decayOf(h.fact), floor)
				}
				return rankTop10(score)
			},
		}
	}
	rankVariant := func(lambda float64) candidate {
		return candidate{
			name: fmt.Sprintf("rank(λ=%g)", lambda),
			hybrid: func(text, vec []arm) []int {
				fused := make(map[int]float64)
				for _, a := range [][]arm{text, vec} {
					for r, h := range a {
						fused[h.fact] += 1.0 / (float64(rrfK+r+1) + lambda*halfLives(h.fact))
					}
				}
				return rankTop10(fused)
			},
			bm25: func(text []arm) []int {
				score := make(map[int]float64, len(text))
				for r, h := range text {
					score[h.fact] = -(float64(r+1) + lambda*halfLives(h.fact)) // меньше штраф = выше
				}
				return rankTop10(score)
			},
		}
	}
	candidates := []candidate{
		mulVariant(0), // текущая формула — сверка симуляции с живым Recall
		mulVariant(0.125),
		mulVariant(0.25),
		{
			name:   "prefusion",
			hybrid: func(text, vec []arm) []int { return rankTop10(rrf(reorderByDecay(text), reorderByDecay(vec))) },
			bm25:   func(text []arm) []int { return rankTop10(rrf(reorderByDecay(text))) },
		},
		rankVariant(5),
		rankVariant(10),
		rankVariant(20),
	}

	// Прогон: плечи берём один раз на запрос, формулы считаем все разом.
	type cellMap map[string]*vmemJudgeCell
	hybRes := make([]cellMap, len(candidates))
	bmRes := make([]cellMap, len(candidates))
	for i := range candidates {
		hybRes[i], bmRes[i] = cellMap{}, cellMap{}
	}
	cellKey := func(q vmemLifeQuery) string { return q.Sort + "/" + vmemLifeBuckets[q.Bucket] }
	gtRank := func(top []int, want int) int {
		for r, f := range top {
			if f == want {
				return r
			}
		}
		return -1
	}

	for _, q := range queries {
		f, err := recallFilter(RecallRequest{Scope: scopeOf[q.Fact], Query: q.Text, K: 10}, vmemLifeNow)
		if err != nil {
			t.Fatalf("recallFilter: %v", err)
		}
		depth := vmemRecallOverfetch
		textRes, err := lvs.SearchTextFilter(q.Text, depth, f)
		if err != nil {
			t.Fatalf("text arm: %v", err)
		}
		vecRes, err := lvs.SearchFilter(vecs[q.Fact], depth, f)
		if err != nil {
			t.Fatalf("vec arm: %v", err)
		}
		text := make([]arm, len(textRes))
		for i, h := range textRes {
			text[i] = arm{factOf(h.Key), h.Score}
		}
		vec := make([]arm, len(vecRes))
		for i, h := range vecRes {
			vec[i] = arm{factOf(h.Key), 1 - float64(h.Distance)}
		}

		ck := cellKey(q)
		for i, c := range candidates {
			hc := hybRes[i][ck]
			if hc == nil {
				hc = &vmemJudgeCell{}
				hybRes[i][ck] = hc
			}
			hc.add(gtRank(c.hybrid(text, vec), q.Fact))
			bc := bmRes[i][ck]
			if bc == nil {
				bc = &vmemJudgeCell{}
				bmRes[i][ck] = bc
			}
			bc.add(gtRank(c.bm25(text), q.Fact))
		}
	}

	// Отчёт + вердикт по критериям.
	rate := func(m cellMap, key string, hit10 bool) float64 {
		c := m[key]
		if c == nil || c.n == 0 {
			return -1
		}
		if hit10 {
			return float64(c.hit10) / float64(c.n)
		}
		return float64(c.hit1) / float64(c.n)
	}
	for i, c := range candidates {
		for _, sortName := range []string{"known", "para"} {
			for _, bn := range vmemLifeBuckets {
				k := sortName + "/" + bn
				hc, bc := hybRes[i][k], bmRes[i][k]
				if hc == nil || bc == nil {
					continue
				}
				t.Logf("%-14s %-5s %-6s: hybrid hit@1=%.3f hit@10=%.3f MRR=%.3f | bm25 hit@1=%.3f hit@10=%.3f",
					c.name, sortName, bn,
					float64(hc.hit1)/float64(hc.n), float64(hc.hit10)/float64(hc.n), hc.mrr/float64(hc.n),
					float64(bc.hit1)/float64(bc.n), float64(bc.hit10)/float64(bc.n))
			}
		}
		critA := rate(hybRes[i], "known/>90d", true)
		critB := rate(hybRes[i], "known/<30d", false)
		critC := rate(bmRes[i], "para/>90d", true)
		pass := func(v, thr float64) string {
			if v >= thr {
				return "✓"
			}
			return "✗"
		}
		t.Logf("ВЕРДИКТ %-14s: (а) hyb known/>90d hit@10=%.3f%s≥0.95  (б) hyb known/<30d hit@1=%.3f%s≥0.95  (в) bm25 para/>90d hit@10=%.3f%s≥0.90",
			c.name, critA, pass(critA, 0.95), critB, pass(critB, 0.95), critC, pass(critC, 0.90))
	}
}
