package vector

// =============================================================================
// VMEM — EXPLAIN: разложение выдачи RECALL (примитив 4 слоя восстановления,
// 27.07). Недостающее звено цепочки «обнаружили → ЛОКАЛИЗОВАЛИ → отозвали».
//
// ЗАЧЕМ ОТДЕЛЬНАЯ КОМАНДА, А НЕ ЛОГИ. Порча памяти проявляется как неверный
// ОТВЕТ, а не как неверная запись: пользователь видит «проект отменён», а не
// строку в сторе. Между записью и ответом стоит машина — два плеча поиска,
// RRF, возрастной штраф, importance, три оси фильтров. Пока эта машина
// непрозрачна, оператор умеет только гадать, какой факт испортил ответ, и
// вопрос «кого отзывать» решается интуицией. Отзыв же — операция по
// ПРОИСХОЖДЕНИЮ, то есть до неё обязателен шаг «покажи, кто это сказал».
// EXPLAIN отвечает на два вопроса сразу:
//   - почему этот факт В выдаче (разложение скора по множителям);
//   - почему тот факт НЕ в выдаче (какая именно ось его отсекла).
// Второй вопрос не менее важен: «улику не видно» и «улики нет» — разные
// диагнозы, и путать их в разборе инцидента нельзя.
//
// ПОЧЕМУ ЭТО НЕ СВОЙ СКОРИНГ. Соблазн — написать честную «объясняющую»
// функцию рядом с боевой. Так делать нельзя: две реализации одной формулы
// расходятся молча, и первым, кто наступит на расхождение, будет человек,
// разбирающий инцидент, — то есть расхождение вылезет ровно там, где цена
// максимальна. Поэтому EXPLAIN не повторяет скоринг, а СМОТРИТ, как считает
// сам Recall: трасса заполняется по ходу боевого пути (recallTrace), а nil
// вместо трассы означает обычный RECALL без единой лишней аллокации.
// Тест TestVMEMExplainMatchesRecall стережёт это равенство.
//
// ГРАНИЦА ЧЕСТНОСТИ. EXPLAIN показывает арифметику ранжирования и оси
// фильтров — то, что система знает о себе. Он НЕ говорит, какой факт ложен:
// это суждение, а система суждений о правде не выносит (см. границу trust —
// вход, а не вывод). Он сокращает «весь стор» до «вот эти пять фактов
// сформировали ответ, вот их источники» — дальше решает человек.
// =============================================================================

import (
	"math"
)

// DropReason — почему кандидат не попал в выдачу. Пустая строка = попал.
type DropReason string

const (
	// DropErasure — стёрт (TTL/FORGET). Судится против now во всех режимах:
	// право быть забытым побеждает машину времени.
	DropErasure DropReason = "erasure"
	// DropValidity — прикладное время не накрывает момент запроса: факт ещё
	// не начался или уже закрыт более новой версией (supersession).
	DropValidity DropReason = "validity"
	// DropQuarantine — убеждение отозвано по происхождению. Отдельная ось:
	// ASOF до момента отзыва этот факт ПОКАЖЕТ (история веры не переписана).
	DropQuarantine DropReason = "quarantine"
	// DropType, DropSource — не прошёл явный фильтр запроса.
	DropType   DropReason = "type"
	DropSource DropReason = "source"
	// DropBelowK — факт валиден и отскорен, но проиграл ранжирование и не влез
	// в top-K. Отдельный диагноз, а не «отфильтрован»: причина невидимости тут
	// не в осях времени и не в отзыве, а в том, что его переспорили. В разборе
	// это ровно тот случай, когда правда есть в памяти, но её не слышно.
	DropBelowK DropReason = "below_k"
)

// ГРАНИЦА: чего EXPLAIN не покажет — и почему это не недоделка.
// Валидность, erasure, тип и источник судятся ДВАЖДЫ: сначала пре-фильтром на
// уровне индекса (Filter в recallFilter), затем пере-судом по свежайшей версии
// среди кандидатов. Факт, снятый пре-фильтром, кандидатом не становится и в
// разложении не появляется ВОВСЕ. Отсюда точная картина достижимости причин:
//   - DropQuarantine и DropBelowK — в обычном сторе всегда: отзыв и обрезка по
//     K живут только в пере-суде и в сортировке;
//   - DropValidity / DropErasure / DropType / DropSource — только для
//     СТЕЙЛ-КОПИИ: старая копия прошла пре-фильтр внутри своего сегмента, а
//     свежайшая версия ключа говорит иное (тот самый класс расхождения, что
//     дважды ловил нас — регресс шага 8 и пропуск SourceEq).
// Для всех прочих «почему не видно» ответ — отсутствие записи, и это честнее
// придуманного вердикта: на этом запросе система про такой факт ничего не
// считала. Увидеть их можно, сняв то, что их сняло: ALL — для закрытых новой
// версией, отказ от TYPE/SOURCE — для отфильтрованных. Стёртый (TTL/FORGET) не
// покажет НИ ОДИН режим: право быть забытым сильнее и машины времени, и
// объяснимости. Это решение, а не недосмотр.

// ExplainedFact — один кандидат с полным разложением: как считался его скор и
// что система знает о его происхождении.
type ExplainedFact struct {
	Key string
	// Rank — место в финальной выдаче (1-based); 0 = не в выдаче.
	Rank int
	// TextRank, VecRank — место в плечах ДО слияния (1-based); 0 = плечо этот
	// факт не вернуло. Расхождение плеч само по себе диагноз: факт, живущий
	// только в лексическом плече, попал по совпадению слов, а не по смыслу.
	TextRank, VecRank int
	// Base — скор до множителей памяти: BM25-скор (без вектора) или fused
	// после RRF (гибрид; возрастной штраф уже внутри, см. AgePenalty).
	Base float64
	// AgeSec — возраст факта на момент, О КОТОРОМ спрашивают (tEff − valid_from);
	// NaN = valid_from нет (не-VMEM док в scope).
	AgeSec float64
	// AgePenalty — слагаемое λ·age/halfLife в знаменателе RRF (только гибрид).
	// DecayMul — множитель 2^(−age/HL) с полом (только BM25-only путь).
	AgePenalty, DecayMul float64
	// ImpMul — множитель importance (0.5+imp), нейтральный 1.0 при imp=0.5.
	ImpMul float64
	// Final — итоговый скор, по которому шла сортировка; NaN у отсеянных
	// (у них множители памяти не считались вовсе).
	Final float64
	// Провенанс и оси — сырьё решения «кого отзывать».
	Source, Type                                 string
	ValidFrom, ValidTo, ExpiresAt, QuarantinedAt float64
	// Drop — причина отсева; пустая = факт в выдаче.
	Drop DropReason
}

// ExplainResult — разложение одного запроса целиком.
type ExplainResult struct {
	Hybrid   bool  // шли оба плеча или только лексическое
	TEff     int64 // момент, на который судились время и возраст (as_of|now)
	HalfLife int64 // период полураспада, применённый к этому запросу
	// WeightText / WeightVec — веса плеч, ФАКТИЧЕСКИ применённые к этому
	// запросу (1.0, если клиент их не задавал). Печатаются всегда, а не только
	// при отклонении от единицы: разбор инцидента начинается с вопроса «почему
	// этот факт наверху», и вес, о котором объяснение молчит, — первое место,
	// где оно разойдётся с ранжированием. На BM25-only пути веса не действуют
	// и обе величины равны 1.
	WeightText, WeightVec float64
	Facts                 []ExplainedFact
}

// recallTrace — сырьё разложения, заполняемое боевым путём Recall. Все методы
// безопасны на nil-приёмнике: обычный RECALL зовёт их же и не платит ничем,
// кроме проверки указателя.
type recallTrace struct {
	keys     []string
	nums     [][]float64
	base     []float64
	final    []float64
	drops    []DropReason
	textRank map[string]int
	vecRank  map[string]int
	order    []string
	hybrid   bool
	tEff     int64
	halfLife int64
	// Веса, применённые слиянием. Берутся из боевого пути, а не пересчитываются
	// из запроса: пересчёт — это вторая реализация, которая разойдётся молча.
	wText, wVec float64
}

func (tr *recallTrace) begin(keys []string, nums [][]float64, cands []VTextResult,
	textArm []VTextResult, vecArm []VSearchResult, hybrid bool, tEff, halfLife int64,
	wText, wVec float64) {
	if tr == nil {
		return
	}
	tr.keys, tr.nums, tr.hybrid, tr.tEff, tr.halfLife = keys, nums, hybrid, tEff, halfLife
	tr.wText, tr.wVec = wText, wVec
	tr.base = make([]float64, len(cands))
	tr.final = make([]float64, len(cands))
	tr.drops = make([]DropReason, len(cands))
	for i := range cands {
		tr.base[i] = cands[i].Score
		tr.final[i] = math.NaN() // отсеянные так и останутся с NaN
	}
	tr.textRank = make(map[string]int, len(keys))
	tr.vecRank = make(map[string]int, len(vecArm))
	if hybrid {
		for r := range textArm {
			tr.textRank[textArm[r].Key] = r + 1
		}
		for r := range vecArm {
			tr.vecRank[vecArm[r].Key] = r + 1
		}
	} else {
		// BM25-only: кандидаты И ЕСТЬ лексическое плечо в порядке ранга.
		for r, k := range keys {
			tr.textRank[k] = r + 1
		}
	}
}

// drop — кандидат отсеян осью why.
func (tr *recallTrace) drop(i int, why DropReason) {
	if tr == nil {
		return
	}
	tr.drops[i] = why
}

// keep — кандидат пережил фильтры; score уже с множителями памяти.
func (tr *recallTrace) keep(i int, score float64) {
	if tr == nil {
		return
	}
	tr.final[i] = score
}

// finish — финальный порядок после сортировки и обрезки по K.
func (tr *recallTrace) finish(out []VTextResult) {
	if tr == nil {
		return
	}
	tr.order = make([]string, len(out))
	for i := range out {
		tr.order[i] = out[i].Key
	}
}

// Explain — VMEM.EXPLAIN: тот же запрос, что RECALL, но с разложением. Порядок
// фактов в ответе — сначала попавшие в выдачу (по рангу), затем отсеянные (по
// убыванию базового скора): в разборе инцидента сперва смотрят на то, что
// реально сформировало ответ.
func (lvs *LeveledVectorStore) Explain(req RecallRequest, now int64) (ExplainResult, error) {
	tr := &recallTrace{}
	if _, err := lvs.recall(req, now, tr); err != nil {
		return ExplainResult{}, err
	}
	res := ExplainResult{Hybrid: tr.hybrid, TEff: tr.tEff, HalfLife: tr.halfLife,
		WeightText: tr.wText, WeightVec: tr.wVec}
	if len(tr.keys) == 0 {
		return res, nil // пре-фильтр не дал ни одного кандидата
	}
	rank := make(map[string]int, len(tr.order))
	for i, k := range tr.order {
		rank[k] = i + 1
	}
	sources := lvs.catForKeys(tr.keys, vmemAttrSource)
	types := lvs.catForKeys(tr.keys, vmemAttrType)

	res.Facts = make([]ExplainedFact, len(tr.keys))
	for i, k := range tr.keys {
		vf, imp := tr.nums[i][0], tr.nums[i][1]
		f := ExplainedFact{
			Key: k, Rank: rank[k],
			TextRank: tr.textRank[k], VecRank: tr.vecRank[k],
			Base: tr.base[i], Final: tr.final[i], Drop: tr.drops[i],
			ImpMul: vmemImpFactor(imp),
			Source: sources[i], Type: types[i],
			ValidFrom: vf, ValidTo: tr.nums[i][2],
			ExpiresAt: tr.nums[i][3], QuarantinedAt: tr.nums[i][4],
			AgeSec: math.NaN(),
		}
		// Возраст и его цена. Обе величины считаются теми же формулами, что в
		// боевом пути (vmemRankLambda / vmemDecayImp), но здесь они РАЗЛОЖЕНЫ:
		// в гибриде возраст платит внутри ранговой шкалы (слагаемое в
		// знаменателе RRF), в BM25-only — множителем с полом. Это не две
		// реализации одного, это две разные формулы боевого пути, и путать их
		// в объяснении нельзя.
		if !math.IsNaN(vf) {
			age := float64(tr.tEff) - vf
			if age < 0 {
				age = 0 // факт из будущего: штраф нейтрален, как в скоринге
			}
			f.AgeSec = age
			if tr.hybrid {
				f.AgePenalty = vmemRankLambda * age / float64(tr.halfLife)
				f.DecayMul = 1
			} else {
				f.DecayMul = math.Max(math.Exp2(-age/float64(tr.halfLife)), vmemDecayFloor)
			}
		} else {
			f.DecayMul = 1
		}
		// Пережил фильтры, но не попал в выдачу — значит, обрезан по K.
		if f.Drop == "" && f.Rank == 0 {
			f.Drop = DropBelowK
		}
		res.Facts[i] = f
	}
	// Попавшие — по рангу; отсеянные — следом, по убыванию базового скора.
	ordered := make([]ExplainedFact, 0, len(res.Facts))
	for r := 1; r <= len(tr.order); r++ {
		for i := range res.Facts {
			if res.Facts[i].Rank == r {
				ordered = append(ordered, res.Facts[i])
				break
			}
		}
	}
	dropped := make([]ExplainedFact, 0, len(res.Facts)-len(ordered))
	for i := range res.Facts {
		if res.Facts[i].Rank == 0 {
			dropped = append(dropped, res.Facts[i])
		}
	}
	for i := 1; i < len(dropped); i++ { // вставками: кандидатов десятки
		f := dropped[i]
		j := i - 1
		for j >= 0 && dropped[j].Base < f.Base {
			dropped[j+1] = dropped[j]
			j--
		}
		dropped[j+1] = f
	}
	res.Facts = append(ordered, dropped...)
	return res, nil
}

// catForKeys — батч-чтение CAT-атрибута свежайшей версии (провенанс тот же,
// что у catEqForKeys: factCatAttrLocked ≡ Get). Отсутствующий атрибут даёт
// пустую строку: у фактов, записанных ДО появления провенанса, колонки
// source нет физически, и это не то же самое, что явный "unknown".
func (lvs *LeveledVectorStore) catForKeys(keys []string, name string) []string {
	out := make([]string, len(keys))
	lvs.mu.RLock()
	defer lvs.mu.RUnlock()
	for i, key := range keys {
		if v, ok := lvs.factCatAttrLocked(key, name); ok {
			out[i] = v
		}
	}
	return out
}
