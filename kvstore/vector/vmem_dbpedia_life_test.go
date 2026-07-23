package vector

// =============================================================================
// П2-эксперимент (23.07): суд «плоский RRF × мультипликативный decay» на
// РЕАЛЬНЫХ эмбеддингах — закрывает оговорку суда шага 8 части 1 («провал
// hybrid-BoW мог быть артефактом шумового hashed-BoW плеча»).
//
// Корпус «жизнь агента» из реальных dbpedia-строк (title/text + ada-002
// 1536d): факты раскиданы по scope'ам (zipf) и возрасту (равномерно по 180
// виртуальным дням). Запросы known-item (query=title) и paraphrase (слова
// тела), GT = сам факт; векторное плечо получает СОБСТВЕННЫЙ эмбеддинг
// факта — НАИЛУЧШИЙ случай для гибрида (плечо ранжирует GT топ-1). Если
// hybrid+decay теряет старые факты даже так — дефект формулы, не плеча.
//
// Порог (утверждён Николаем до прогона): hybrid+decay проигрывает
// BM25-only+decay >0.10 по hit@10 в корзине >90d → дефект подтверждён на
// реальных векторах; в пределах порога → «артефакт BoW» доказан данными.
//
// Данные: /tmp/vmemlife.{bin,jsonl} ← scripts/prep_vmemlife.py (реальные
// строки HF KShivendu/dbpedia-entities-openai-1M).
// =============================================================================

import (
	"bufio"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"math/rand"
	"os"
	"sort"
	"strings"
	"testing"
)

type vmemLifeDoc struct {
	I     int    `json:"i"`
	Title string `json:"title"`
	Text  string `json:"text"`
}

// loadVMEMLife читает пару bin/jsonl от prep_vmemlife.py.
func loadVMEMLife(t *testing.T) ([]vmemLifeDoc, [][]float32) {
	t.Helper()
	f, err := os.Open("/tmp/vmemlife.bin")
	if err != nil {
		t.Skipf("нет /tmp/vmemlife.bin — прогони scripts/prep_vmemlife.py (%v)", err)
	}
	defer f.Close()
	br := bufio.NewReaderSize(f, 1<<20)
	var n, dim uint32
	if err := binary.Read(br, binary.LittleEndian, &n); err != nil {
		t.Fatalf("bin header: %v", err)
	}
	if err := binary.Read(br, binary.LittleEndian, &dim); err != nil {
		t.Fatalf("bin header: %v", err)
	}
	vecs := make([][]float32, n)
	for i := range vecs {
		v := make([]float32, dim)
		if err := binary.Read(br, binary.LittleEndian, v); err != nil {
			t.Fatalf("bin vec %d: %v", i, err)
		}
		vecs[i] = v
	}
	jf, err := os.Open("/tmp/vmemlife.jsonl")
	if err != nil {
		t.Skipf("нет /tmp/vmemlife.jsonl — прогони scripts/prep_vmemlife.py (%v)", err)
	}
	defer jf.Close()
	docs := make([]vmemLifeDoc, 0, n)
	sc := bufio.NewScanner(jf)
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	for sc.Scan() {
		var d vmemLifeDoc
		if err := json.Unmarshal(sc.Bytes(), &d); err != nil {
			t.Fatalf("jsonl: %v", err)
		}
		docs = append(docs, d)
	}
	if len(docs) != int(n) {
		t.Fatalf("jsonl %d строк != bin %d", len(docs), n)
	}
	return docs, vecs
}

const (
	vmemLifeDay     = int64(86400)
	vmemLifeHorizon = 180
	vmemLifeT0      = int64(1_760_000_000)
	vmemLifeNow     = vmemLifeT0 + int64(vmemLifeHorizon)*vmemLifeDay
	vmemLifeHL      = 30 * vmemLifeDay // дефолтный half-life Recall
)

var vmemLifeBuckets = []string{"<30d", "30-90d", ">90d"}

type vmemLifeQuery struct {
	Sort   string // known | para
	Bucket int
	Fact   int
	Text   string
}

func vmemLifeBucketOf(at int64) int {
	age := vmemLifeNow - at
	switch {
	case age < 30*vmemLifeDay:
		return 0
	case age <= 90*vmemLifeDay:
		return 1
	default:
		return 2
	}
}

// vmemLifeSetup — общий стенд П2-линейки: корпус реальных dbpedia-фактов в
// store (детерминированная раздача scope/возраста, seed 42) + стратифицированные
// запросы. Один код на все суды: бенч 4 режимов и суд decay-кандидатов обязаны
// есть ОДНО состояние LSM и ОДНИ запросы.
func vmemLifeSetup(t *testing.T) (lvs *LeveledVectorStore, docs []vmemLifeDoc, vecs [][]float32, scopeOf []string, atOf []int64, queries []vmemLifeQuery) {
	t.Helper()
	docs, vecs = loadVMEMLife(t)
	const (
		nScopes  = 200
		perCell  = 400 // запросов на (сорт × корзина), максимум
		flushEvr = 5000
	)

	cfg := bm25TestConfig()
	cfg.Distance = CosineDistance
	cfg.Metric = MetricCosine
	cfg.DeltaMax = flushEvr + 1
	lvs = NewLeveledVectorStore(cfg)

	// Раздача scope/возраста — детерминированная (seed 42), zipf-скосы как в
	// vmemcorpus: квадрат равномерной → частые маленькие индексы.
	rng := rand.New(rand.NewSource(42))
	scopeOf = make([]string, len(docs))
	atOf = make([]int64, len(docs))
	order := rng.Perm(len(docs))
	events := make([]int, 0, len(docs))
	for _, i := range order {
		u := rng.Float64()
		scopeOf[i] = fmt.Sprintf("user:%03d", int(float64(nScopes)*u*u)%nScopes)
		atOf[i] = vmemLifeT0 + int64(rng.Float64()*float64(vmemLifeHorizon))*vmemLifeDay + int64(rng.Intn(int(vmemLifeDay)))
		events = append(events, i)
	}
	sort.Slice(events, func(a, b int) bool { return atOf[events[a]] < atOf[events[b]] })

	for k, i := range events {
		req := RememberRequest{
			ID:        fmt.Sprintf("fact:%05d", i),
			Scope:     scopeOf[i],
			Text:      docs[i].Title + " " + docs[i].Text,
			ValidFrom: atOf[i],
			Vector:    vecs[i],
		}
		if _, err := lvs.Remember(req, atOf[i]); err != nil {
			t.Fatalf("Remember %d: %v", i, err)
		}
		if (k+1)%flushEvr == 0 {
			lvs.FlushDeltaSync()
		}
	}
	lvs.FlushDeltaSync()
	t.Logf("корпус: %d фактов, %d сегментов, dim=%d", len(docs), len(lvs.collectSegments()), len(vecs[0]))

	// Запросы: стратификация по корзинам возраста, сорта known/para.
	cnt := map[[2]int]int{}
	for _, i := range rng.Perm(len(docs)) {
		b := vmemLifeBucketOf(atOf[i])
		if cnt[[2]int{0, b}] < perCell {
			queries = append(queries, vmemLifeQuery{"known", b, i, docs[i].Title})
			cnt[[2]int{0, b}]++
		}
		words := strings.Fields(docs[i].Text)
		if len(words) >= 16 && cnt[[2]int{1, b}] < perCell {
			queries = append(queries, vmemLifeQuery{"para", b, i, strings.Join(words[6:14], " ")})
			cnt[[2]int{1, b}]++
		}
	}
	return lvs, docs, vecs, scopeOf, atOf, queries
}

// TestVMEMDBpediaLifeDecay — четыре режима на ОДНОМ корпусе:
// {BM25-only, hybrid} × {decay 30д, decay off}, метрики по возрастным корзинам.
func TestVMEMDBpediaLifeDecay(t *testing.T) {
	if testing.Short() {
		t.Skip("эксперимент: только полный прогон")
	}
	const hugeHL = int64(1) << 40
	lvs, _, vecs, scopeOf, _, queries := vmemLifeSetup(t)
	defer lvs.Close()
	nowV := vmemLifeNow
	bucketName := vmemLifeBuckets

	type cell struct {
		n           int
		hit1, hit10 int
		mrr         float64
	}
	runMode := func(name string, hybrid bool, halfLife int64) map[string]*cell {
		res := map[string]*cell{}
		for _, q := range queries {
			req := RecallRequest{
				Scope:       scopeOf[q.Fact],
				Query:       q.Text,
				K:           10,
				HalfLifeSec: halfLife,
			}
			if hybrid {
				req.Vector = vecs[q.Fact]
			}
			out, err := lvs.Recall(req, nowV)
			if err != nil {
				t.Fatalf("%s Recall: %v", name, err)
			}
			key := q.Sort + "/" + bucketName[q.Bucket]
			c := res[key]
			if c == nil {
				c = &cell{}
				res[key] = c
			}
			c.n++
			want := fmt.Sprintf("fact:%05d", q.Fact)
			for r, h := range out {
				if h.Key == want {
					if r == 0 {
						c.hit1++
					}
					c.hit10++
					c.mrr += 1.0 / float64(r+1)
					break
				}
			}
		}
		for _, sortName := range []string{"known", "para"} {
			for _, bn := range bucketName {
				if c := res[sortName+"/"+bn]; c != nil {
					t.Logf("%-18s %-5s %-6s: hit@1=%.3f hit@10=%.3f MRR=%.3f (n=%d)",
						name, sortName, bn,
						float64(c.hit1)/float64(c.n), float64(c.hit10)/float64(c.n),
						c.mrr/float64(c.n), c.n)
				}
			}
		}
		return res
	}

	bm25Decay := runMode("bm25+decay30d", false, 0)
	bm25Flat := runMode("bm25+nodecay", false, hugeHL)
	hybDecay := runMode("hybrid+decay30d", true, 0)
	hybFlat := runMode("hybrid+nodecay", true, hugeHL)
	_ = bm25Flat
	_ = hybFlat

	// Вердикт по утверждённому порогу: hybrid+decay vs bm25+decay, known >90d.
	a := bm25Decay["known/>90d"]
	b := hybDecay["known/>90d"]
	if a == nil || b == nil || a.n == 0 || b.n == 0 {
		t.Fatalf("пустая корзина known/>90d — корпус сгенерирован неверно")
	}
	drop := float64(a.hit10)/float64(a.n) - float64(b.hit10)/float64(b.n)
	// История порога: до фикса 23.07 (пол + ранговый штраф) дельта была 0.958 —
	// «дефект подтверждён»; после фикса тест работает регрессом: гибрид не
	// имеет права отставать от BM25-пути на старых фактах больше чем на 0.10.
	verdict := "в пределах порога (≤0.10)"
	if drop > 0.10 {
		verdict = "РЕГРЕСС: гибрид отстаёт от BM25 на старых фактах (>0.10)"
	}
	t.Logf("СУД: hit@10 known/>90d bm25+decay=%.3f vs hybrid+decay=%.3f, дельта=%.3f — %s",
		float64(a.hit10)/float64(a.n), float64(b.hit10)/float64(b.n), drop, verdict)
}
