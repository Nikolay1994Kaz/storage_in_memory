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

// TokenizeDoc — единственный публичный вход токенизации: текст → (терм, tf).
// Нужен write-site'у VSIM.ADDDOC: одни и те же термы обязаны уйти и в дельту
// (AddDocTerms), и в WAL (SerializeVectorWithDoc) — журнал везёт РЕЗУЛЬТАТ
// токенизации, реплей не перетокенизирует (бит-в-бит воспроизводимость
// независимо от версии стеммера, см. wal.go про OpVSimAddDoc).
func TokenizeDoc(text string) []TermTF {
	return bm25CountTerms(bm25Tokenize(text))
}

// bm25TitleWeight — вес заголовка в «бедном BM25F»: термы TITLE повторяются
// W раз перед токенизацией, tf растёт в W раз — эффект пер-полевого веса без
// поля в формате сегмента. Подобран экспериментом 19.07 (dbpedia100k,
// bm25_boost_experiment_test.go): ×3 даёт known-item hit@1 0.846→0.906,
// MRR 0.907→0.941 без просадки полнотекстового recall. Вес вшивается на
// ингесте (термы в WAL уже взвешены) — смена веса = re-ingest, как у стеммера.
const bm25TitleWeight = 3

// bm25PruneMinN — минимальный размер корпуса (N доков с текстом), с которого
// включается отсечение общеупотребимых термов запроса в SearchText. Ниже
// порога выигрыша нет (постинги короткие), а сдвиг скоров ломал бы
// golden-оракул на микрокорпусах.
const bm25PruneMinN = 1000

// TokenizeDocTitled — токенизация дока с бустом заголовка: title×W + text.
// Пустой title вырождается в TokenizeDoc(text).
func TokenizeDocTitled(title, text string) []TermTF {
	if strings.TrimSpace(title) == "" {
		return TokenizeDoc(text)
	}
	return TokenizeDoc(strings.Repeat(title+" ", bm25TitleWeight) + text)
}

// AddDoc вставляет док: вектор + атрибуты + текст. Текст токенизируется здесь
// (единственное место, где он существует) и дальше живёт термами. Upsert —
// полная замена состояния дока: прежний текст перезаписывается; пустой text
// снимает текст (та же семантика, что у attrs).
func (lvs *LeveledVectorStore) AddDoc(key string, vec []float32, attrs Attributes, text string) error {
	return lvs.AddDocTerms(key, vec, attrs, TokenizeDoc(text))
}

// AddDocTitled — AddDoc с выделенным заголовком (буст bm25TitleWeight).
func (lvs *LeveledVectorStore) AddDocTitled(key string, vec []float32, attrs Attributes, title, text string) error {
	return lvs.AddDocTerms(key, vec, attrs, TokenizeDocTitled(title, text))
}

// AddDocTerms — вставка дока с УЖЕ готовыми термами: реплей WAL (OpVSimAddDoc)
// и будущий командный путь после токенизации. Реплей обязан идти сюда, а не
// через AddDoc: журнал везёт результат токенизации, повторная токенизация
// сломала бы бит-в-бит воспроизводимость (см. wal.go). Тот же choke-point
// addEntry, что у Add/AddWithAttrs — вся upsert/затенение-семантика едина.
func (lvs *LeveledVectorStore) AddDocTerms(key string, vec []float32, attrs Attributes, terms []TermTF) error {
	return lvs.addEntry(key, vec, attrs, terms)
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
	return lvs.SearchTextFilter(query, K, Filter{})
}

// SearchTextFilter — SearchText с мульти-атрибутным фильтром (Eq/Range,
// зеркало SearchFilter). Фильтр судится ДО формирования top-K каждого
// источника: memtables — по снимку атрибутов (свежие выигрывают при коллизии
// ключа — семантика полной замены upsert), сегменты — idx-предикатом по
// uint-колонкам (sa.compile) внутри скоринга постингов. Пре-фильтр
// обязателен: пост-отсев готового топа морил бы маленький scope голодом.
//
// Статистика BM25 (N/df/avgdl) остаётся ГЛОБАЛЬНОЙ, не пер-scope — решение
// 20.07: дёшево (статистика уже собрана), стабильно на крошечных scope
// (N=5 даёт вырожденный IDF), консистентно с SearchText/golden-оракулом.
// Цена: локально-частое слово тенанта скорится как глобально-редкое.
// Пересмотр — по измеренному профиту (бенч шага 8 VMEM), не по «честнее».
func (lvs *LeveledVectorStore) SearchTextFilter(query string, K int, f Filter) ([]VTextResult, error) {
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

	// Отсечение общеупотребимых термов запроса (эксперимент 19.07,
	// bm25_boost_experiment_test.go: QPS коротких запросов 1192→7850 при
	// нулевом изменении hit@1/hit@10/MRR). Скоринг сканирует постинг-лист
	// каждого терма запроса ЦЕЛИКОМ; терм с df>N/2 стоит десятки тысяч доков
	// скана, а несёт idf<ln2≈0.69 — почти шум. Порог N/2 консервативен
	// сознательно: на dbpedia содержательное «county» имеет df=35% (idf≈1.0),
	// резать 25–35%-ную полосу опасно. Гейты: маленькие корпуса не трогаем
	// (golden-оракул), запрос целиком из общих термов ищется как есть.
	if n >= bm25PruneMinN {
		kept := 0
		for i := range terms {
			if df[i]*2 < n {
				kept++
			}
		}
		if kept > 0 && kept < len(terms) {
			prunedTerms := make([]string, 0, kept)
			prunedDF := make([]uint64, 0, kept)
			for i := range terms {
				if df[i]*2 < n {
					prunedTerms = append(prunedTerms, terms[i])
					prunedDF = append(prunedDF, df[i])
				}
			}
			terms, df = prunedTerms, prunedDF
		}
	}
	avgdl := float64(totalLen) / float64(n)
	idf := make([]float64, len(terms))
	for i := range terms {
		idf[i] = bm25IDF(n, df[i])
	}

	// tombstone-фильтр (аналог composedFilter). Snapshot снят под RLock —
	// консистентен с источниками.
	var tombFilter func(string) bool
	if tombSnap != nil {
		ts := *tombSnap
		tombFilter = func(key string) bool {
			_, deleted := ts[key]
			return !deleted
		}
	}

	// Атрибут-суд memtable-кандидатов — инлайн по СОБСТВЕННЫМ атрибутам копии,
	// которые подаёт шард из-под своего RLock (см. deltaFilterFn в delta.go):
	// ни снапшота, ни лукапов, ни замков. Стейл-копия, прошедшая суд по своим
	// атрибутам, гасится Contains-правилом merge (свежая memtable глушит) —
	// семантика полной замены upsert сохранена. История (E2E vmemload 23.07):
	// (1) полный снимок AttrsSnapshot всех дельт на КАЖДЫЙ запрос — O(|delta|)
	// map-аллокация: 80µs/запрос на непустой дельте (~5.5k фактов) и конвой на
	// runtime-локах аллокатора под конкуренцией (8 воркеров → p50 11ms, QPS
	// 6469→620; mutex-профиль: 88% в maps.newTable); (2) freshest-лукап
	// GetAttrs из фильтра — дедлок: рекурсивный RLock шарда при ждущем
	// писателе (mix-фаза). Инлайн-атрибуты закрывают обе ямы разом.
	fEmpty := f.empty()
	var memFilter deltaFilterFn
	if tombFilter != nil || !fEmpty {
		memFilter = func(key string, a Attributes) bool {
			if tombFilter != nil && !tombFilter(key) {
				return false
			}
			return fEmpty || matchAttrs(a, f, "")
		}
	}

	// Memtables — MUTABLE, скорим под lvs.mu.RLock (та же дисциплина, что у
	// векторного пути: активная — конкурентный Add, flushing — Close в
	// applyResult). Сегменты immutable — обойдём после RUnlock.
	memHits := make([][]deltaTextResult, nMem)
	for i, mt := range memtables {
		memHits[i] = mt.SearchText(terms, idf, avgdl, K, memFilter)
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
	//     нормализует (шаг 6). При StrictSegShadow (П3-прототип) сегментный хит
	//     дополнительно гасится HasKey более свежего сегмента — стейл не
	//     всплывает и в переходных состояниях.
	strictShadow := lvs.cfg.StrictSegShadow
	dedup := make(map[string]int)
	// Ёмкость по факту, не по K: RECALL шага 5 оверфетчит K=100 при типичных
	// единицах хитов на scope-запрос — преаллокация полного K дала бы ~4КБ
	// мусора на каждый запрос (GC-просадка QPS, замер шага 5); рост append
	// амортизирован для редких больших выдач.
	capHint := min(K, 16)
	combined := make([]VTextResult, 0, capHint)
	ranks := make([]int64, 0, capHint)
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
		// Компиляция фильтра в idx-предикат по колонкам сегмента (то же
		// пространство индексов, что у text-слоя). matchable=false — фильтр
		// в этом сегменте заведомо никого не пропустит (значения нет в dict /
		// атрибута нет) → сегмент пропускается целиком.
		var pred func(int) bool
		if !f.empty() {
			var matchable bool
			pred, matchable = seg.Attrs().compile(f, "")
			if !matchable {
				continue
			}
		}
		for _, h := range st.search(terms, idf, avgdl, 0, uint32(len(st.docLen)), K, pred) {
			key := seg.TextKey(h.doc)
			// "" — док удалён in-place внутри сегмента (graph-delete), маскируем
			// как векторный путь; затем tombstone-маска на живые ключи.
			if key == "" || (tombFilter != nil && !tombFilter(key)) {
				continue
			}
			addHit(key, h.score, segRank[si], nMem)
		}
	}

	cmpHits := func(a, b VTextResult) int {
		if a.Score > b.Score {
			return -1
		}
		if a.Score < b.Score {
			return 1
		}
		return strings.Compare(a.Key, b.Key)
	}
	if strictShadow {
		// Ленивый пере-суд топа (StrictSegShadow): хиты сортируются ВМЕСТЕ с
		// рангом-провенансом (плоская структура, не пермутация — сортировка с
		// двойной индирекцией по combined[idxs[i]] стоила ~13% QPS в замере №2/3
		// П3), затем спуск по убыванию скора гасит сегментные хиты, чей ключ
		// живёт в более свежем сегменте. Судятся только кандидаты до наполнения
		// top-K (обычно ровно K) — та же экономия, что пере-суд VMEM.Recall.
		type rankedHit struct {
			hit  VTextResult
			rank int64
		}
		hits := make([]rankedHit, len(combined))
		for i := range combined {
			hits[i] = rankedHit{combined[i], ranks[i]}
		}
		slices.SortFunc(hits, func(a, b rankedHit) int { return cmpHits(a.hit, b.hit) })
		memBound := int64(math.MinInt64) + int64(nMem)
		out := make([]VTextResult, 0, min(K, len(hits)))
		for i := range hits {
			if len(out) == K {
				break
			}
			if r := hits[i].rank; r >= memBound { // хит сегмента (memtable-ранги ниже)
				stale := false
				for j := range segs {
					if segRank[j] < r && segs[j].HasKey(hits[i].hit.Key) {
						stale = true
						break
					}
				}
				if stale {
					continue
				}
			}
			out = append(out, VTextResult{Key: strings.Clone(hits[i].hit.Key), Score: hits[i].hit.Score})
		}
		return out, nil
	}
	slices.SortFunc(combined, cmpHits)
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
