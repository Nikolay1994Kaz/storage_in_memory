package vector

import (
	"math"
	"slices"
	"strings"
)

// =============================================================================
// Store-уровень текстового поиска BM25 (шаг 4 спринта, docs/BM25_HYBRID_DESIGN.md):
// живой путь записи (AddDoc → термы в дельте → flush → segmentText) и SearchText
// поверх memtables+сегментов с ТЕМИ ЖЕ правилами видимости, что у векторного
// search: затенение Contains, провенанс-дедуп, tombstone-фильтр.
//
// Сырой текст в хранилище не существует: AddDoc токенизирует на входе, дальше
// по конвейеру едут только (терм, tf). Durability термов (WAL/снапшот) — шаг 5:
// до него текстовый слой живёт до рестарта. Команды RESP — шаг 7.
// =============================================================================

// VTextResult — результат текстового поиска: ключ и BM25-скор (больше = лучше).
type VTextResult struct {
	Key   string
	Score float64
}

// AddDoc вставляет док: вектор + атрибуты + текст. Текст токенизируется здесь
// (единственное место, где он существует) и дальше живёт термами. Upsert —
// полная замена состояния дока: прежний текст перезаписывается; пустой text
// снимает текст (та же семантика, что у attrs).
func (lvs *LeveledVectorStore) AddDoc(key string, vec []float32, attrs Attributes, text string) error {
	return lvs.addEntry(key, vec, attrs, bm25CountTerms(bm25Tokenize(text)))
}

// SearchText — глобальный BM25 top-K: memtable-дельты (активная + flushing) +
// сегменты. Запрос токенизируется тем же токенайзером, что доки (симметрия
// док/запрос); дубли термов запроса сохраняются — семантика эталона.
//
// Порядок работы зеркалит векторный search:
//  1. под lvs.mu.RLock снимаем состав источников, tombstones и собираем
//     глобальную статистику (N/avgdl/df по термам запроса) СРАЗУ по сегментам
//     и дельтам — свежие доки обязаны скориться той же статистикой;
//  2. memtables (mutable) скорим под RLock, сегменты (immutable) — после;
//  3. merge: затенение + провенанс-дедуп + tombstones (см. ниже).
func (lvs *LeveledVectorStore) SearchText(query string, K int) ([]VTextResult, error) {
	terms := bm25Tokenize(query)
	if len(terms) == 0 || K <= 0 {
		return nil, nil
	}

	lvs.mu.RLock()
	// memtables свежесть-first: активная дельта, затем flushing новее→старше —
	// тот же порядок-провенанс, что в векторном search (memtable[i] свежее
	// memtable[j>i] и свежее любого сегмента).
	memtables := make([]*DeltaSegment, 0, 1+len(lvs.flushing))
	if lvs.delta != nil {
		memtables = append(memtables, lvs.delta)
	}
	for i := len(lvs.flushing) - 1; i >= 0; i-- {
		memtables = append(memtables, lvs.flushing[i])
	}
	nMem := len(memtables)
	segs := make([]segment, 0, 8)
	segRank := make([]int64, 0, 8)
	for lvl := range lvs.levels {
		for pos := range lvs.levels[lvl] {
			segs = append(segs, lvs.levels[lvl][pos])
			segRank = append(segRank, int64(lvl)<<40-int64(pos))
		}
	}
	tombSnap := lvs.tombstones.Load()

	// Глобальная статистика: сумма вкладов сегментов и дельт по термам запроса
	// (O(термы × источники) лукапов). Затенённые upsert-копии завышают df до
	// компакции — принятая семантика (как count в Info()).
	var n, totalLen uint64
	df := make([]uint64, len(terms))
	for _, seg := range segs {
		st := seg.Text()
		if st == nil {
			continue
		}
		n += uint64(st.nText)
		totalLen += st.totalLen
		for i, t := range terms {
			df[i] += st.df(t)
		}
	}
	for _, mt := range memtables {
		mn, ml, mdf := mt.TextStats(terms)
		n += mn
		totalLen += ml
		for i := range df {
			df[i] += mdf[i]
		}
	}
	if n == 0 {
		lvs.mu.RUnlock()
		return nil, nil // в корпусе нет ни одного дока с текстом
	}
	avgdl := float64(totalLen) / float64(n)
	idf := make([]float64, len(terms))
	for i := range terms {
		idf[i] = bm25IDF(n, df[i])
	}

	// tombstone-фильтр (аналог composedFilter; пользовательских фильтров у
	// SEARCHTEXT v1 нет). Snapshot снят под RLock — консистентен с источниками.
	var tombFilter func(string) bool
	if tombSnap != nil {
		ts := *tombSnap
		tombFilter = func(key string) bool {
			_, deleted := ts[key]
			return !deleted
		}
	}

	// Memtables — MUTABLE, скорим под lvs.mu.RLock (та же дисциплина, что у
	// векторного пути: активная — конкурентный Add, flushing — Close в
	// applyResult). Сегменты immutable — обойдём после RUnlock.
	memHits := make([][]deltaTextResult, nMem)
	for i, mt := range memtables {
		memHits[i] = mt.SearchText(terms, idf, avgdl, K, tombFilter)
	}
	lvs.mu.RUnlock()

	// Merge — зеркало векторного (см. search, flat merge):
	//   - хит сегмента гасится, если ЛЮБАЯ memtable содержит ключ: свежая версия
	//     дока могла изменить или снять текст, stale-копия не должна всплыть,
	//     даже когда свежая по запросу не матчится вовсе;
	//   - хит memtable[i] гасится более свежей memtable[j<i];
	//   - коллизия ключа среди выживших решается ПРОВЕНАНСОМ (min rank), не
	//     скором — ближе-но-старше не должен победить свежего;
	//   - сегмент-vs-сегмент затеняется только дедупом среди хитов (как у
	//     векторов): stale-копия старого сегмента, чья свежая копия не попала в
	//     свой top-K, может всплыть до merge — принятая семантика, компакция
	//     нормализует (шаг 6).
	dedup := make(map[string]int)
	combined := make([]VTextResult, 0, K)
	ranks := make([]int64, 0, K)
	addHit := func(key string, score float64, rank int64, shadowLimit int) {
		for j := 0; j < shadowLimit; j++ {
			if memtables[j].Contains(key) {
				return
			}
		}
		if pos, ok := dedup[key]; ok {
			if rank < ranks[pos] {
				combined[pos] = VTextResult{Key: key, Score: score}
				ranks[pos] = rank
			}
			return
		}
		dedup[key] = len(combined)
		combined = append(combined, VTextResult{Key: key, Score: score})
		ranks = append(ranks, rank)
	}
	for i, hits := range memHits {
		for _, h := range hits {
			addHit(h.key, h.score, math.MinInt64+int64(i), i)
		}
	}
	for si, seg := range segs {
		st := seg.Text()
		if st == nil {
			continue
		}
		for _, h := range st.search(terms, idf, avgdl, 0, uint32(len(st.docLen)), K) {
			key := seg.TextKey(h.doc)
			// "" — док удалён in-place внутри сегмента (graph-delete), маскируем
			// как векторный путь; затем tombstone-маска на живые ключи.
			if key == "" || (tombFilter != nil && !tombFilter(key)) {
				continue
			}
			addHit(key, h.score, segRank[si], nMem)
		}
	}

	slices.SortFunc(combined, func(a, b VTextResult) int {
		if a.Score > b.Score {
			return -1
		}
		if a.Score < b.Score {
			return 1
		}
		return strings.Compare(a.Key, b.Key)
	})
	if len(combined) > K {
		combined = combined[:K]
	}
	// Ключи из сегментов — zero-copy views в blob; клонируем выживший top-K,
	// чтобы результат не удерживал сегмент (K мал, O(K) клонов).
	for i := range combined {
		combined[i].Key = strings.Clone(combined[i].Key)
	}
	return combined, nil
}
