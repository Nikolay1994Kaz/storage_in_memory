package vector

import (
	"math"
	"sort"
)

// =============================================================================
// Текстовый слой сегмента (BM25) — шаг 3 спринта, docs/BM25_HYBRID_DESIGN.md.
//
// Строится по образцу колоночного слоя attr.go: пер-сегментный словарь термов
// (тот же паттерн attrDict — без глобального мьютекса на горячем Add), постинги
// выровнены по frozen-порядку entries (после sortEntriesByTenant — тенант это
// непрерывный [lo,hi) диапазон доков, скоуп = бинпоиск в постингах), слой
// иммутабелен после build. Сырой текст НЕ хранится: merge пересоберёт слой
// декодом (term, tf) из постингов (шаг 6).
//
// Глобальный IDF (решение №3 дизайна): df нужен только термам запроса —
// bm25GlobalStats суммирует пер-сегментные df за O(термы × сегменты) лукапов.
//
// Формула запинена оракулом (scripts/bm25_oracle.py, golden.json):
//   idf   = ln(1 + (N - df + 0.5) / (df + 0.5))            — Lucene, всегда ≥ 0
//   score = Σ idf · tf·(k1+1) / (tf + k1·(1 - b + b·dl/avgdl))
// =============================================================================

const (
	bm25K1 = 1.2
	bm25B  = 0.75
)

// TermTF — терм и его частота в документе. Едет с DeltaEntry (как Attributes)
// от ADDDOC до build; постинги строятся при flush/merge. Инвариант: термы в
// слайсе уникальны и отсортированы (гарантирует bm25CountTerms).
type TermTF struct {
	Term string
	TF   uint16
}

// bm25CountTerms сворачивает поток токенов (выход bm25Tokenize) в
// отсортированный список (term, tf). tf насыщается на MaxUint16 — при таких
// частотах BM25-вклад терма давно на плато насыщения.
func bm25CountTerms(tokens []string) []TermTF {
	if len(tokens) == 0 {
		return nil
	}
	cnt := make(map[string]uint32, len(tokens))
	for _, t := range tokens {
		cnt[t]++
	}
	out := make([]TermTF, 0, len(cnt))
	for t, c := range cnt {
		if c > math.MaxUint16 {
			c = math.MaxUint16
		}
		out = append(out, TermTF{Term: t, TF: uint16(c)})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Term < out[j].Term })
	return out
}

// bm25Posting — вхождение терма: frozen-индекс дока + частота.
type bm25Posting struct {
	doc uint32
	tf  uint16
}

// segmentText — текстовый слой одного сегмента.
type segmentText struct {
	dict     *attrDict       // терм → локальный termID (паттерн словарей attr-колонок)
	postings [][]bm25Posting // postings[termID], отсортированы по doc (порядок build)
	docLen   []uint32        // docLen[i] = токенов в доке i; 0 = док без текста
	nText    int             // доков с текстом — вклад сегмента в глобальный N
	totalLen uint64          // сумма docLen — вклад в глобальный avgdl
}

// buildSegmentText строит слой по entries в ИХ ПОРЯДКЕ (frozen-индекс =
// позиция; вызывать ПОСЛЕ sortEntriesByTenant, как buildSegmentAttrs).
// Возвращает nil, если текстов нет (zero overhead для чистых векторов).
func buildSegmentText(entries []DeltaEntry) *segmentText {
	hasText := false
	for i := range entries {
		if len(entries[i].Terms) > 0 {
			hasText = true
			break
		}
	}
	if !hasText {
		return nil
	}
	st := &segmentText{
		dict:   newAttrDict(),
		docLen: make([]uint32, len(entries)),
	}
	for i := range entries {
		var dl uint32
		for _, tt := range entries[i].Terms {
			id := st.dict.intern(tt.Term)
			if int(id) == len(st.postings) {
				st.postings = append(st.postings, nil)
			}
			// доки идут по возрастанию i → каждый лист отсортирован по doc
			st.postings[id] = append(st.postings[id], bm25Posting{doc: uint32(i), tf: tt.TF})
			dl += uint32(tt.TF)
		}
		st.docLen[i] = dl
		if dl > 0 {
			st.nText++
			st.totalLen += uint64(dl)
		}
	}
	return st
}

// df — документная частота терма в сегменте. Это ДЛИНА постинг-листа:
// по записи (doc, tf) на документ, читать доки не нужно.
func (st *segmentText) df(term string) uint64 {
	if st == nil {
		return 0
	}
	if id, ok := st.dict.lookup(term); ok {
		return uint64(len(st.postings[id]))
	}
	return 0
}

// bm25IDF — глобальная обратная документная частота (Lucene-вариант, ≥ 0).
func bm25IDF(n, df uint64) float64 {
	return math.Log(1 + (float64(n)-float64(df)+0.5)/(float64(df)+0.5))
}

// bm25GlobalStats собирает корпусную статистику для ОДНОГО запроса поверх
// набора сегментов: N (доки с текстом), avgdl и df по каждому терму запроса.
// df выровнен с terms; дубли термов в запросе допустимы (посчитаются дважды —
// та же семантика, что у эталона). Затенённые upsert-копии завышают df до
// компакции — принятая семантика (как count в Info()).
func bm25GlobalStats(segs []*segmentText, terms []string) (n uint64, avgdl float64, df []uint64) {
	df = make([]uint64, len(terms))
	var totalLen uint64
	for _, st := range segs {
		if st == nil {
			continue
		}
		n += uint64(st.nText)
		totalLen += st.totalLen
		for i, t := range terms {
			df[i] += st.df(t)
		}
	}
	if n > 0 {
		avgdl = float64(totalLen) / float64(n)
	}
	return
}

// bm25Hit — результат текстового поиска в сегменте.
type bm25Hit struct {
	doc   uint32
	score float64
}

// search — BM25-скоринг сегмента по термам запроса в диапазоне frozen-индексов
// [lo, hi) (тенант-блок из каталога; весь сегмент = [0, len(docLen))).
// idf выровнен с terms и посчитан глобально (bm25GlobalStats) — сегмент
// применяет чужую статистику, не свою. Возвращает top-k по убыванию скора,
// при равенстве — по doc.
//
// pred (nil = без фильтра) — idx-предикат атрибут-фильтра (sa.compile),
// судится ДО скоринга и top-K: пре-фильтр, а не отсев готового топа —
// иначе топ, занятый чужими доками, морил бы маленький scope голодом
// (грабля дельта-пост-фильтра). Индексы text-слоя и attr-колонок — одно
// пространство (buildSegment* строят по одному порядку entries).
func (st *segmentText) search(terms []string, idf []float64, avgdl float64, lo, hi uint32, k int, pred func(int) bool) []bm25Hit {
	if st == nil || k <= 0 || avgdl == 0 {
		return nil
	}
	acc := make(map[uint32]float64)
	for qi, t := range terms {
		id, ok := st.dict.lookup(t)
		if !ok {
			continue // терма нет в сегменте — df-вклад уже учтён как 0
		}
		pl := st.postings[id]
		// скоуп: лист отсортирован по doc → левая граница бинпоиском,
		// правая — обрывом итерации (тенант-контигуозность, Вещь 1)
		start := sort.Search(len(pl), func(i int) bool { return pl[i].doc >= lo })
		for _, p := range pl[start:] {
			if p.doc >= hi {
				break
			}
			if pred != nil && !pred(int(p.doc)) {
				continue
			}
			tf := float64(p.tf)
			dl := float64(st.docLen[p.doc])
			acc[p.doc] += idf[qi] * tf * (bm25K1 + 1) / (tf + bm25K1*(1-bm25B+bm25B*dl/avgdl))
		}
	}
	hits := make([]bm25Hit, 0, len(acc))
	for doc, s := range acc {
		hits = append(hits, bm25Hit{doc: doc, score: s})
	}
	sort.Slice(hits, func(i, j int) bool {
		if hits[i].score != hits[j].score {
			return hits[i].score > hits[j].score
		}
		return hits[i].doc < hits[j].doc
	})
	if len(hits) > k {
		hits = hits[:k]
	}
	return hits
}
