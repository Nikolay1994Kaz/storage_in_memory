// Пакет vmemcorpus — детерминированный генератор корпуса «жизнь агента» для
// бенча шага 8 VMEM (docs/VMEM_DESIGN.md). Один и тот же генератор с одним
// seed обязан питать ОБА прогона развилки А: суд качества на store-уровне
// (сжатое виртуальное время, now параметром) и E2E-замер латентности по RESP
// (реальные часы) — иначе числа канона будут про два разных мира.
//
// Корпус несёт зашитую правду (ground truth) по построению:
//   - known-item: уникальный ent-токен факта → GT = сам факт;
//   - paraphrase: подвыборка обычных слов тела (без ent-токена) → GT слабее,
//     судит ранжирование под зипф-шумом;
//   - temporal: цепочки supersession делят chain-токен, лексически версии
//     неразличимы → правильную версию обязан выбрать ТОЛЬКО фильтр
//     валидности (AS_OF в середину интервала версии);
//   - ordering-пробы: одинаковый текст, разные возраст/importance → порядок
//     обязан диктоваться множителем decay×imp (суд «плоского» умножения);
//   - erasure-инварианты: истёкшие TTL и забытые FORGET обязаны отсутствовать
//     во ВСЕХ режимах (default/ALL/AS_OF-когда-был-жив) — не метрика, а ноль.
//
// Генератор не зависит от пакета vector: тексты и векторы (hashed
// bag-of-words для hybrid-прогона) строятся из собственных токенов корпуса.
package vmemcorpus

import (
	"fmt"
	"hash/fnv"
	"math"
	"math/rand"
	"sort"
	"strings"
)

const day = int64(86400)

// Params — размеры корпуса. Менять только вместе с пересмотром канона:
// числа BENCHMARKS.md привязаны к seed и размерам.
type Params struct {
	Seed        int64
	Scopes      int // scope'ов, зипф-размеры
	PlainFacts  int // обычных фактов (включая TTL- и FORGET-подмножества)
	TTLFacts    int // из них с TTL (половина истекает до NowV)
	ForgetFacts int // из них стираются FORGET по ходу ленты
	Chains      int // supersession-цепочек по 2–5 версий
	ProbeGroups int // ordering-проб (тройки одинакового текста)
	Vocab       int // зипф-словарь тел фактов
	HorizonDays int // длина виртуальной ленты

	KnownQ, ParaQ, AsOfQ, NowChainQ, ErasureQ int
}

// Default — базовые размеры бенча (канон шага 8).
func Default() Params {
	return Params{
		Seed:        42,
		Scopes:      200,
		PlainFacts:  24000,
		TTLFacts:    3000,
		ForgetFacts: 600,
		Chains:      800,
		ProbeGroups: 60,
		Vocab:       20000,
		HorizonDays: 180,
		KnownQ:      2000,
		ParaQ:       2000,
		AsOfQ:       1000,
		NowChainQ:   300,
		ErasureQ:    400,
	}
}

// Fact — одно событие REMEMBER виртуальной ленты.
type Fact struct {
	ID, Scope, Type string
	Text            string
	Imp             float64 // <0 → не задавать (серверный дефолт 0.5)
	At              int64   // виртуальный now ингеста (= valid_from)
	TTLSec          int64   // 0 = без TTL
	Supersedes      string
	Vec             []float32 // hashed-BoW для hybrid-прогона (nil не бывает)
}

// Event — элемент ленты: либо Fact, либо Forget.
type Event struct {
	At                    int64
	Fact                  *Fact // nil → FORGET
	ForgetID, ForgetScope string
}

// Query — запрос с зашитой правдой. Смысл полей по Sort:
//
//	known/para : WantID — GT-факт (метрики hit@1/hit@10/MRR); AgeDays — возраст
//	             GT на NowV (разбивка по возрасту = суд decay×score);
//	asof/nowchain: WantPresent — ровно та версия цепочки, WantAbsent — все
//	             остальные версии (accuracy: present ∧ ни одного absent);
//	order      : WantOrder — ожидаемый порядок id по множителю decay×imp;
//	erasure    : WantAbsent — стёртый id, обязан отсутствовать (инвариант=0).
type Query struct {
	Sort  string // known | para | asof | nowchain | order | erasure
	Scope string
	Text  string
	K     int
	AsOf  int64 // 0 = дефолт (валидное-сейчас)
	All   bool
	Vec   []float32

	WantID      string
	AgeDays     int
	WantPresent []string
	WantAbsent  []string
	WantOrder   []string
}

// Corpus — детерминированный выход генератора.
type Corpus struct {
	Events  []Event // лента, отсортирована по At
	NowV    int64   // «настоящее» виртуальной ленты (момент запросов)
	Queries []Query
	ScopeOf map[string]string // id → scope: проверка изоляции на КАЖДОМ результате
	Dim     int               // размерность hashed-BoW векторов
}

// bowDim — размерность hashed bag-of-words. Вектор нужен только чтобы плечо
// похожести коррелировало с текстом (суд «плоского RRF» — про арифметику
// ранговой шкалы, не про качество эмбеддера).
const bowDim = 64

// Generate — детерминированная сборка корпуса: один seed → бит-в-бит одна
// лента и одни запросы на обоих концах развилки А.
func Generate(p Params) *Corpus {
	rng := rand.New(rand.NewSource(p.Seed))
	t0 := int64(1_760_000_000) // произвольный положительный якорь ленты
	nowV := t0 + int64(p.HorizonDays)*day
	c := &Corpus{NowV: nowV, ScopeOf: make(map[string]string), Dim: bowDim}

	zipfWord := func() string {
		u := rng.Float64() * rng.Float64()
		return fmt.Sprintf("word%05d", int(float64(p.Vocab)*u))
	}
	zipfScope := func() string {
		u := rng.Float64() * rng.Float64()
		return fmt.Sprintf("user:%03d", int(float64(p.Scopes)*u))
	}
	types := []string{"preference", "profile", "event", "task", "note"}

	// --- обычные факты -----------------------------------------------------
	plainWords := make([][]string, p.PlainFacts)
	plain := make([]*Fact, p.PlainFacts)
	for i := range plain {
		n := 8 + rng.Intn(17)
		words := make([]string, n)
		for j := range words {
			words[j] = zipfWord()
		}
		plainWords[i] = words
		imp := -1.0
		switch r := rng.Float64(); {
		case r < 0.15:
			imp = 0.8 + 0.2*rng.Float64()
		case r < 0.30:
			imp = 0.3 * rng.Float64()
		}
		f := &Fact{
			ID:    fmt.Sprintf("fact:%05d", i),
			Scope: zipfScope(),
			Type:  types[rng.Intn(len(types))],
			Imp:   imp,
			At:    t0 + rng.Int63n((int64(p.HorizonDays)-1)*day),
		}
		f.Text = strings.Join(append(append([]string{}, words...), fmt.Sprintf("ent%05d", i)), " ")
		plain[i] = f
	}

	// TTL- и FORGET-подмножества — непересекающиеся (чистые инварианты).
	perm := rng.Perm(p.PlainFacts)
	expired := map[int]bool{} // истечёт ДО NowV
	ttlLive := map[int]bool{} // TTL есть, но переживает NowV
	forgotten := map[int]bool{}
	forgets := make([]Event, 0, p.ForgetFacts)
	for k, idx := range perm[:p.TTLFacts] {
		f := plain[idx]
		if k%2 == 0 {
			// истечение в [At+1ч, NowV-5д]: к моменту запросов мёртв
			span := nowV - 5*day - (f.At + 3600)
			if span <= 0 {
				ttlLive[idx] = true
				f.TTLSec = nowV - f.At + 30*day
				continue
			}
			f.TTLSec = 3600 + rng.Int63n(span)
			expired[idx] = true
		} else {
			f.TTLSec = nowV - f.At + (30+rng.Int63n(60))*day
			ttlLive[idx] = true
		}
	}
	for _, idx := range perm[p.TTLFacts : p.TTLFacts+p.ForgetFacts] {
		f := plain[idx]
		forgotten[idx] = true
		at := f.At + 3600 + rng.Int63n(max64(nowV-f.At-3600, 1))
		forgets = append(forgets, Event{At: at, ForgetID: f.ID, ForgetScope: f.Scope})
	}

	// --- supersession-цепочки ----------------------------------------------
	type chain struct {
		ids   []string
		times []int64 // At каждой версии; интервал версии j = [times[j], times[j+1])
		scope string
	}
	chains := make([]chain, p.Chains)
	for ci := range chains {
		nv := 2 + rng.Intn(4)
		gaps := make([]int64, nv-1)
		var span int64
		for g := range gaps {
			gaps[g] = (10 + rng.Int63n(31)) * day
			span += gaps[g]
		}
		start := t0 + rng.Int63n(max64(nowV-day-span-t0, 1))
		ch := chain{scope: zipfScope()}
		at := start
		token := fmt.Sprintf("chain%04d", ci)
		var prev string
		for v := 0; v < nv; v++ {
			if v > 0 {
				at += gaps[v-1]
			}
			n := 8 + rng.Intn(17)
			words := make([]string, n)
			for j := range words {
				words[j] = zipfWord()
			}
			f := &Fact{
				ID:         fmt.Sprintf("chain:%04d:v%d", ci, v),
				Scope:      ch.scope,
				Type:       "profile",
				Imp:        -1,
				At:         at,
				Supersedes: prev,
				Text:       strings.Join(append(words, token), " "),
			}
			prev = f.ID
			ch.ids = append(ch.ids, f.ID)
			ch.times = append(ch.times, at)
			plain = append(plain, f)
		}
		chains[ci] = ch
	}

	// --- ordering-пробы ------------------------------------------------------
	type probe struct {
		ids  []string
		want []string
	}
	probes := make([]probe, p.ProbeGroups)
	probeScopes := make([]string, p.ProbeGroups)
	for pi := range probes {
		token := fmt.Sprintf("probe%04d", pi)
		scope := zipfScope()
		probeScopes[pi] = scope
		n := 6
		words := make([]string, n)
		for j := range words {
			words[j] = zipfWord()
		}
		body := strings.Join(append(words, token), " ")
		ages := []int64{5, 45, 120}
		imps := []float64{-1, -1, -1}
		if pi%2 == 1 { // imp-проба: возраст общий, importance разная
			ages = []int64{30, 30, 30}
			imps = []float64{0.9, 0.5, 0.1}
		}
		type member struct {
			id string
			m  float64
		}
		members := make([]member, len(ages))
		for j := range ages {
			f := &Fact{
				ID:    fmt.Sprintf("probe:%04d:%d", pi, j),
				Scope: scope,
				Type:  "note",
				Imp:   imps[j],
				At:    nowV - ages[j]*day,
				Text:  body,
			}
			plain = append(plain, f)
			imp := imps[j]
			if imp < 0 {
				imp = 0.5
			}
			// ожидаемый множитель шага 5: 2^(−age/halfLife) × (0.5+imp)
			m := math.Exp2(-float64(ages[j]*day)/float64(30*day)) * (0.5 + imp)
			members[j] = member{id: f.ID, m: m}
			probes[pi].ids = append(probes[pi].ids, f.ID)
		}
		sort.Slice(members, func(a, b int) bool { return members[a].m > members[b].m })
		for _, mb := range members {
			probes[pi].want = append(probes[pi].want, mb.id)
		}
	}

	// --- лента событий -------------------------------------------------------
	events := make([]Event, 0, len(plain)+len(forgets))
	for _, f := range plain {
		f.Vec = bowVec(f.Text)
		c.ScopeOf[f.ID] = f.Scope
		events = append(events, Event{At: f.At, Fact: f})
	}
	events = append(events, forgets...)
	sort.SliceStable(events, func(a, b int) bool { return events[a].At < events[b].At })
	c.Events = events

	// --- запросы ---------------------------------------------------------------
	// Пул living: обычные факты, живые на NowV (GT known/para обязана быть видимой).
	living := make([]int, 0, p.PlainFacts)
	for i := 0; i < p.PlainFacts; i++ {
		if !expired[i] && !forgotten[i] {
			living = append(living, i)
		}
	}

	for q := 0; q < p.KnownQ; q++ {
		i := living[rng.Intn(len(living))]
		f := plain[i]
		words := []string{fmt.Sprintf("ent%05d", i)}
		for w := 0; w < 1+rng.Intn(2); w++ {
			words = append(words, plainWords[i][rng.Intn(len(plainWords[i]))])
		}
		text := strings.Join(words, " ")
		c.Queries = append(c.Queries, Query{
			Sort: "known", Scope: f.Scope, Text: text, K: 10, Vec: bowVec(text),
			WantID: f.ID, AgeDays: int((nowV - f.At) / day),
		})
	}
	for q := 0; q < p.ParaQ; q++ {
		i := living[rng.Intn(len(living))]
		f := plain[i]
		n := 3 + rng.Intn(4)
		words := make([]string, n)
		for w := range words {
			words[w] = plainWords[i][rng.Intn(len(plainWords[i]))]
		}
		text := strings.Join(words, " ")
		c.Queries = append(c.Queries, Query{
			Sort: "para", Scope: f.Scope, Text: text, K: 10, Vec: bowVec(text),
			WantID: f.ID, AgeDays: int((nowV - f.At) / day),
		})
	}
	for q := 0; q < p.AsOfQ; q++ {
		ch := chains[rng.Intn(len(chains))]
		v := rng.Intn(len(ch.ids) - 1) // не head: у head интервал открыт (это nowchain)
		ts := (ch.times[v] + ch.times[v+1]) / 2
		text := fmt.Sprintf("chain%04d", indexOfChain(ch.ids[0]))
		c.Queries = append(c.Queries, Query{
			Sort: "asof", Scope: ch.scope, Text: text, K: 10, AsOf: ts, Vec: bowVec(text),
			WantPresent: []string{ch.ids[v]}, WantAbsent: without(ch.ids, v),
		})
	}
	for q := 0; q < p.NowChainQ; q++ {
		ch := chains[rng.Intn(len(chains))]
		head := len(ch.ids) - 1
		text := fmt.Sprintf("chain%04d", indexOfChain(ch.ids[0]))
		c.Queries = append(c.Queries, Query{
			Sort: "nowchain", Scope: ch.scope, Text: text, K: 10, Vec: bowVec(text),
			WantPresent: []string{ch.ids[head]}, WantAbsent: without(ch.ids, head),
		})
	}
	for pi, pr := range probes {
		text := fmt.Sprintf("probe%04d", pi)
		c.Queries = append(c.Queries, Query{
			Sort: "order", Scope: probeScopes[pi], Text: text, K: 10, Vec: bowVec(text),
			WantOrder: pr.want,
		})
	}
	// erasure-инварианты: стёртое отсутствует в default / ALL / AS_OF-когда-жил.
	expIdx, forIdx := keysOf(expired), keysOf(forgotten)
	sort.Ints(expIdx)
	sort.Ints(forIdx)
	for q := 0; q < p.ErasureQ; q++ {
		var i int
		if q%2 == 0 {
			i = expIdx[rng.Intn(len(expIdx))]
		} else {
			i = forIdx[rng.Intn(len(forIdx))]
		}
		f := plain[i]
		text := fmt.Sprintf("ent%05d", i)
		qu := Query{Sort: "erasure", Scope: f.Scope, Text: text, K: 10, Vec: bowVec(text),
			WantAbsent: []string{f.ID}}
		switch q % 3 {
		case 1:
			qu.All = true
		case 2:
			qu.AsOf = f.At + 1800 // тогда факт был жив; erasure обязан победить машину времени
		}
		c.Queries = append(c.Queries, qu)
	}
	return c
}

// bowVec — hashed bag-of-words: детерминированный юнит-вектор текста, плечо
// похожести коррелирует с пересечением слов.
func bowVec(text string) []float32 {
	v := make([]float32, bowDim)
	for _, w := range strings.Fields(text) {
		h := fnv.New64a()
		h.Write([]byte(w))
		s := h.Sum64()
		idx := int(s % bowDim)
		if (s>>32)&1 == 0 {
			v[idx]++
		} else {
			v[idx]--
		}
	}
	var norm float64
	for _, x := range v {
		norm += float64(x) * float64(x)
	}
	if norm == 0 {
		v[0] = 1
		return v
	}
	inv := float32(1 / math.Sqrt(norm))
	for i := range v {
		v[i] *= inv
	}
	return v
}

func without(ids []string, skip int) []string {
	out := make([]string, 0, len(ids)-1)
	for i, id := range ids {
		if i != skip {
			out = append(out, id)
		}
	}
	return out
}

func keysOf(m map[int]bool) []int {
	out := make([]int, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// indexOfChain — номер цепочки из id первой версии "chain:%04d:v0".
func indexOfChain(id string) int {
	var ci, v int
	fmt.Sscanf(id, "chain:%04d:v%d", &ci, &v)
	return ci
}

func max64(a, b int64) int64 {
	if a > b {
		return a
	}
	return b
}
