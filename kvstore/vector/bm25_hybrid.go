package vector

import (
	"slices"
	"strings"
)

// =============================================================================
// Гибридный поиск BM25+вектор (шаг 7 спринта, docs/BM25_HYBRID_DESIGN.md):
// Reciprocal Rank Fusion поверх двух независимых top-списков.
//
// Почему ранги, а не скоры: BM25 (неограниченная сумма IDF-вкладов) и
// векторная дистанция живут в несравнимых шкалах — любая линейная комбинация
// требовала бы калибровки под конкретный корпус. RRF шкалы выбрасывает целиком:
//   score(док) = Σ по спискам, где док встретился: 1/(rrfK + rank), rank с 1.
// Консенсус ретриверов бьёт уверенность одного: два средних ранга дают больше,
// чем одно первое место (1/62+1/62 > 1/61).
// =============================================================================

const (
	// rrfK — классический демпфер RRF (Cormack et al.): не даёт первому месту
	// одного списка задавить всё остальное. Зафиксирован дизайном, не param.
	rrfK = 60
	// rrfFusionDepth — глубина обоих top-списков до слияния. 100 хватает:
	// хвосты глубже вносят <1/160 на док и финальный top-K не двигают.
	rrfFusionDepth = 100
)

// SearchHybrid — гибридный top-K: лексический top-100 (SearchText) + векторный
// top-100 (Search) → RRF. Score в результате — RRF-сумма; она НЕ сравнима ни с
// BM25-скором, ни с дистанцией, полезен только порядок (и стабильный tie-break
// по ключу: равные суммы — частый случай, см. симметричные ранги выше).
//
// Два списка — два независимых RLock-снимка: док, уехавший delta→segment между
// ними, виден в обоих консистентно (каждый путь сам держит правила видимости),
// атомарность ПАРЫ снимков не нужна — RRF читает только ранги.
//
// Пустой текстовый запрос (нет термов после токенизации) деградирует до
// векторного ранжирования в RRF-шкале; вектор обязателен всегда.
func (lvs *LeveledVectorStore) SearchHybrid(query string, vec []float32, K int) ([]VTextResult, error) {
	return lvs.SearchHybridFilter(query, vec, K, Filter{})
}

// SearchHybridFilter — гибрид с мульти-атрибутным фильтром, применённым к
// ОБОИМ плечам ДО RRF (filter-then-fuse, правило шага 3а VMEM_DESIGN):
// каждое плечо формирует свой top-depth уже из прошедших фильтр доков.
// Пост-фьюжн отсев недопустим: top-depth плеча был бы занят чужими доками,
// и после отсева маленькому scope доставался бы огрызок или пусто
// (голодание; та же грабля, что post-hoc фильтр дельты).
func (lvs *LeveledVectorStore) SearchHybridFilter(query string, vec []float32, K int, f Filter) ([]VTextResult, error) {
	if K <= 0 {
		return nil, nil
	}
	depth := max(rrfFusionDepth, K)
	textHits, err := lvs.SearchTextFilter(query, depth, f)
	if err != nil {
		return nil, err
	}
	// Пустой фильтр — прежний глобальный путь Search (без обвязки SearchFilter):
	// SearchHybrid остаётся бит-в-бит прежним.
	var vecHits []VSearchResult
	if f.empty() {
		vecHits, err = lvs.Search(vec, depth, nil)
	} else {
		vecHits, err = lvs.SearchFilter(vec, depth, f)
	}
	if err != nil {
		return nil, err
	}

	fused := make(map[string]float64, len(textHits)+len(vecHits))
	for i := range textHits {
		fused[textHits[i].Key] += 1.0 / float64(rrfK+i+1)
	}
	for i := range vecHits {
		fused[vecHits[i].Key] += 1.0 / float64(rrfK+i+1)
	}

	out := make([]VTextResult, 0, len(fused))
	for key, score := range fused {
		out = append(out, VTextResult{Key: key, Score: score})
	}
	slices.SortFunc(out, func(a, b VTextResult) int {
		if a.Score > b.Score {
			return -1
		}
		if a.Score < b.Score {
			return 1
		}
		return strings.Compare(a.Key, b.Key)
	})
	if len(out) > K {
		out = out[:K]
	}
	return out, nil
}
