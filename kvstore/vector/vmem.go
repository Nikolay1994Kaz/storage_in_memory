package vector

import (
	"errors"
	"fmt"
	"hash/fnv"
	"maps"
	"math"
	"slices"
	"strings"
)

// =============================================================================
// VMEM — слой памяти агентов (шаг 2 спринта, docs/VMEM_DESIGN.md): внутренний
// Remember = «кухня полей» над готовым ADDDOC-конвейером. Факт — обычный док:
// ключ (ULID или клиентский id) + CAT/NUM-атрибуты контракта + термы текста;
// в формате движка НОЛЬ новых полей, durability наследуется от OpVSimAddDoc.
//
// Дверь 1 («реплей не смотрит на часы»): вся недетерминированность — часы,
// генератор ULID, токенайзер, placeholder-вектор — умирает ЗДЕСЬ, до WAL.
// Кухня возвращает материализованный док, и командный слой (шаг 7) обязан
// сериализовать в WAL именно его: журнал везёт следствия, не причины.
//
// Шаг 4 (supersession): REMEMBER f2 supersedes=f1 закрывает valid_to(f1) =
// valid_from(f2) re-ingest'ом цели («c» из VMEM_DESIGN: платим на редкой
// записи, ноль новых путей чтения). Гонка «прочитал цель → параллельный upsert
// цели → re-ingest затёр» исключена примитивом: весь read-modify-write идёт
// под эксклюзивным lvs.mu.Lock (обычные писатели держат RLock, swap дельты и
// публикация merge — Lock, поэтому под замком LSM-состояние неподвижно), и
// операции над одним ключом сериализуются целиком — побеждает поздний.
//
// Erasure (шаг 6) — vmem_forget.go: Forget по id + TTL-жнец на idle-механике.
// Ещё НЕ сделано: RESP-команда VMEM.* (шаг 7).
// =============================================================================

// vmemOpenValidTo — сентинел «интервал открыт» для valid_to/expires_at: 2^53,
// граница точных целых float64 (NUM-колонка не исказит значение). Валидность
// всегда проверяется Range-фильтром без ветки «атрибута нет».
const vmemOpenValidTo = int64(1) << 53

// vmemPlaceholderDim — размерность placeholder-векторов ступени 0 (факт без
// эмбеддинга). Движок v1 vector-aligned: дверь «has-vector» зарезервирована в
// BM25_HYBRID_DESIGN, но не реализована, поэтому безвекторный факт получает
// детерминированный юнит-вектор из id (рецепт той же двери). На пустом сторе
// это фиксирует dim=32; ⚠ловушка: BYO-эмбеддинги другой размерности после
// этого получат dimMismatch — переход со ступени 0 на BYO = re-ingest в новый
// стор. На сторе с реальными эмбеддингами placeholder берёт их размерность.
const vmemPlaceholderDim = 32

// Имена атрибутов контракта VMEM (таблица факта в docs/VMEM_DESIGN.md).
const (
	vmemAttrScope      = "scope"
	vmemAttrType       = "type"
	vmemAttrSupersedes = "supersedes"
	vmemAttrSource     = "source"
	vmemAttrImp        = "imp"
	vmemAttrValidFrom  = "valid_from"
	vmemAttrValidTo    = "valid_to"
	vmemAttrExpiresAt  = "expires_at"
)

// vmemSourceUnknown — источник факта, который клиент не объявил. Решение
// (26.07): «неизвестно» пишется ЯВНЫМ значением, а не отсутствием атрибута.
// Причина в том, ради чего провенанс вообще существует: отзыв по источнику.
// Отсутствующий атрибут не выбирается ни Eq, ни Range — факт без объявленного
// источника оказался бы невидим для массового отзыва и молча пережил бы
// карантин, то есть ровно тот сценарий, от которого мы защищаем. Явное
// значение делает «источник не объявлен» первоклассным, фильтруемым классом:
// `SOURCE unknown` находит их все.
//
// ⚠Граница честности: значение штампуется на ингесте, поэтому оно относится к
// фактам, записанным УЖЕ с провенансом. У фактов, записанных до появления
// этого атрибута, колонка отсутствует физически (attrMissing) — это НЕ то же
// самое, что unknown, и выдавать одно за другое нельзя: про них стор ничего не
// наблюдал. Отличать легаси от необъявленного — само по себе провенанс.
const vmemSourceUnknown = "unknown"

var (
	// ErrVMEMScope — scope обязателен: факт без владельца не имеет смысла
	// (изоляция памяти — первое свойство контракта).
	ErrVMEMScope = errors.New("vmem: scope is required")
	// ErrVMEMText — text обязателен: дословный якорь и есть факт; пустой text
	// в ADDDOC-семантике снял бы текстовый слой дока.
	ErrVMEMText = errors.New("vmem: text is required")
	// ErrVMEMImportance — importance вне [0,1] или NaN.
	ErrVMEMImportance = errors.New("vmem: importance must be within [0,1]")
	// ErrVMEMTTL — ttl отрицателен либо now+ttl достигает сентинела открытого
	// интервала (переполнение/коллизия с «без TTL»).
	ErrVMEMTTL = errors.New("vmem: ttl must be >= 0 and now+ttl below the open-interval sentinel")
	// ErrVMEMNow — серверные часы обязаны быть положительными unix-секундами
	// ниже сентинела: время едет в NUM-атрибуты и в WAL абсолютным числом.
	ErrVMEMNow = errors.New("vmem: now must be positive unix seconds below the sentinel")
	// ErrVMEMValidFrom — клиентский override valid_from вне (0, сентинел).
	ErrVMEMValidFrom = errors.New("vmem: valid_from must be positive unix seconds below the sentinel")
	// ErrVMEMSelfSupersedes — факт не может заменять сам себя.
	ErrVMEMSelfSupersedes = errors.New("vmem: fact cannot supersede itself")
	// ErrVMEMSupersedesTarget — цель supersedes не существует (или стёрта):
	// закрывать нечего, и молча превращать замену в обычный remember нельзя —
	// клиент явно утверждает связь, которой стор не может придать смысл.
	ErrVMEMSupersedesTarget = errors.New("vmem: supersedes target does not exist")
	// ErrVMEMSupersedesScope — цель supersedes живёт в другом scope (или не
	// является VMEM-фактом): замена через границу памяти запрещена контрактом.
	ErrVMEMSupersedesScope = errors.New("vmem: supersedes target belongs to a different scope")
	// ErrVMEMForgetScope — цель FORGET живёт в другом scope (или не является
	// VMEM-фактом с данным scope): стирание через границу памяти запрещено.
	ErrVMEMForgetScope = errors.New("vmem: forget target belongs to a different scope")
	// ErrVMEMQuery — запрос RECALL обязателен: recall — это поиск, не скан scope.
	ErrVMEMQuery = errors.New("vmem: query is required")
	// ErrVMEMK — top-K RECALL должен быть положительным.
	ErrVMEMK = errors.New("vmem: k must be positive")
	// ErrVMEMAsOf — as_of вне (0, сентинел) либо задан вместе с all.
	ErrVMEMAsOf = errors.New("vmem: as_of must be positive unix seconds below the sentinel and is mutually exclusive with all")
	// ErrVMEMHalfLife — период полураспада должен быть неотрицательным
	// (0 = дефолт движка).
	ErrVMEMHalfLife = errors.New("vmem: half_life must be >= 0 seconds")
)

// RememberRequest — вход VMEM.REMEMBER до кухни полей (см. таблицу контракта).
type RememberRequest struct {
	ID         string    // клиентский id (ретрай = upsert-замена, идемпотентность); "" → серверный ULID
	Scope      string    // чья память (тенант); обязателен
	Text       string    // сам факт, дословный якорь; обязателен
	Type       string    // вид факта (preference/event/task/…); опционален
	Importance *float64  // сопротивление затуханию, [0,1]; nil → 0.5
	ValidFrom  int64     // unix сек «истинно с»; 0 → серверный now (override — импорт старых логов)
	TTL        int64     // секунд до erasure, относительный; 0 → без TTL
	Supersedes string    // id заменяемого факта (провенанс; закрытие его интервала — шаг 4)
	Source     string    // откуда факт (канал/инструмент/документ); "" → vmemSourceUnknown
	Vector     []float32 // BYO-эмбеддинг; nil → ступень 0 (placeholder из id)
}

// RememberedDoc — материализованный факт после кухни: ровно то, что ушло в
// дельту и обязано уйти в WAL (Op=OpVSimAddDoc, Key=ID,
// Value=SerializeVectorWithDoc(Vec, Attrs, Terms)) без пересборки полей.
type RememberedDoc struct {
	ID    string
	Vec   []float32
	Attrs Attributes
	Terms []TermTF
}

// rememberDoc — чистая кухня полей REMEMBER: валидация входа + все серверные
// решения (ULID, дефолты, конвертация TTL→expires_at) в одном месте. dim —
// текущая размерность стора (0 = стор пуст), нужна только placeholder'у.
func rememberDoc(req RememberRequest, now int64, dim int) (RememberedDoc, error) {
	if now <= 0 || now >= vmemOpenValidTo {
		return RememberedDoc{}, ErrVMEMNow
	}
	if req.Scope == "" {
		return RememberedDoc{}, ErrVMEMScope
	}
	if req.Text == "" {
		return RememberedDoc{}, ErrVMEMText
	}
	imp := 0.5
	if req.Importance != nil {
		imp = *req.Importance
		if math.IsNaN(imp) || imp < 0 || imp > 1 {
			return RememberedDoc{}, ErrVMEMImportance
		}
	}
	validFrom := now
	if req.ValidFrom != 0 {
		if req.ValidFrom < 0 || req.ValidFrom >= vmemOpenValidTo {
			return RememberedDoc{}, ErrVMEMValidFrom
		}
		validFrom = req.ValidFrom
	}
	// Конвертация ДО WAL (дверь 1): в журнал едет абсолютный expires_at,
	// реплей не знает, «когда было сейчас». TTL отсчитывается от ингеста
	// (now), а не от valid_from: override valid_from — про истинность факта,
	// не про срок его хранения.
	expiresAt := vmemOpenValidTo
	if req.TTL < 0 || req.TTL >= vmemOpenValidTo-now {
		return RememberedDoc{}, ErrVMEMTTL
	}
	if req.TTL > 0 {
		expiresAt = now + req.TTL
	}
	id := req.ID
	if id == "" {
		var err error
		if id, err = NewULID(now * 1000); err != nil {
			return RememberedDoc{}, err
		}
	} else if err := ValidateKey(id); err != nil {
		return RememberedDoc{}, fmt.Errorf("vmem: bad id: %w", err)
	}
	if req.Supersedes != "" && req.Supersedes == id {
		return RememberedDoc{}, ErrVMEMSelfSupersedes
	}

	// Источник штампуется ВСЕГДА (см. vmemSourceUnknown): «не объявлен» — это
	// значение, а не дырка. Иначе массовый отзыв по источнику пропускал бы
	// ровно те факты, происхождение которых никто не подтверждал.
	source := req.Source
	if source == "" {
		source = vmemSourceUnknown
	}
	attrs := Attributes{
		Cat: map[string]string{vmemAttrScope: req.Scope, vmemAttrSource: source},
		Num: map[string]float64{
			vmemAttrImp:       imp,
			vmemAttrValidFrom: float64(validFrom),
			vmemAttrValidTo:   float64(vmemOpenValidTo),
			vmemAttrExpiresAt: float64(expiresAt),
		},
	}
	if req.Type != "" {
		attrs.Cat[vmemAttrType] = req.Type
	}
	if req.Supersedes != "" {
		attrs.Cat[vmemAttrSupersedes] = req.Supersedes
	}

	vec := req.Vector
	if vec == nil {
		d := dim
		if d <= 0 {
			d = vmemPlaceholderDim
		}
		vec = vmemPlaceholderVector(id, d)
	}
	// Факты без заголовков → TokenizeDoc, не Titled (контракт: no TITLE).
	return RememberedDoc{ID: id, Vec: vec, Attrs: attrs, Terms: TokenizeDoc(req.Text)}, nil
}

// vmemPlaceholderVector — детерминированный юнит-вектор из id (ступень 0):
// FNV-1a(id) → splitmix64-поток → нормализация. Детерминизм существенен для
// идемпотентности: ретрай с клиентским ID даёт бит-в-бит тот же док и тот же
// WAL-блоб. Случайные направления в высокой размерности почти ортогональны
// друг другу и реальным эмбеддингам — в векторном плече placeholder тонет,
// факт находит BM25-плечо (graceful degradation контракта).
func vmemPlaceholderVector(id string, dim int) []float32 {
	h := fnv.New64a()
	h.Write([]byte(id))
	state := h.Sum64()
	vec := make([]float32, dim)
	var norm float64
	for i := range vec {
		state += 0x9E3779B97F4A7C15
		z := state
		z ^= z >> 30
		z *= 0xBF58476D1CE4E5B9
		z ^= z >> 27
		z *= 0x94D049BB133111EB
		z ^= z >> 31
		v := float64(z)/math.MaxUint64*2 - 1
		vec[i] = float32(v)
		norm += v * v
	}
	if norm == 0 {
		vec[0] = 1
		norm = 1
	}
	inv := 1 / math.Sqrt(norm)
	for i := range vec {
		vec[i] = float32(float64(vec[i]) * inv)
	}
	return vec
}

// vmemDefaultHalfLifeSec — дефолтный период полураспада свежести (30 дней).
// Механизм наш, политика клиента: переопределяется per-request (HalfLifeSec).
// Per-type полураспады — открытая дверь (нужен проброс type кандидатам),
// добавляются спрос-driven, формат не трогают.
const vmemDefaultHalfLifeSec = int64(30 * 24 * 3600)

// vmemRecallOverfetch — глубина кандидатов до скоринга (шаг 5): свежий важный
// факт с рангом похожести > K обязан уметь всплыть в top-K; скоринг только
// top-K мог бы лишь переставлять внутри него. Совпадает с rrfFusionDepth.
const vmemRecallOverfetch = 100

// vmemDecayFloor / vmemRankLambda — фикс «decay против плоской шкалы» (суд
// 23.07, TestVMEMDecayCandidatesJudge на реальных ada-002 1536d): пост-фьюжн
// множитель 2^(−age/HL) математически занулял старые факты в ранговой шкале
// RRF (потолок 2/61 × 2^-4 < хвостовой 1/160 → hit@10 >90d = 0.003 при
// ИДЕАЛЬНОМ векторном плече). Композитный победитель по трём критериям:
//   - BM25-only (широкая шкала скоров): множитель с полом —
//     max(2^(−age/HL), 0.25) × (0.5+imp); para/>90d 0.762→0.985;
//   - hybrid (ранговая шкала): возраст платит рангом там же, где живёт скор —
//     Σ 1/(rrfK + rank + λ·age/HL), λ=5 (одна полураспад-жизнь = +5 рангов);
//     known/>90d 0.003→1.000, свежая корзина hit@1 0.917→0.995.
//
// Отвергнутые кандидаты (по метрике): один floor на оба пути (гибрид 0.938,
// hit@1 0.013 — квантование хвоста), один rank на оба пути (BM25 para 0.690 —
// ранговый штраф выбрасывает шкалу скоров), prefusion (BM25-хвост не лечит).
const vmemDecayFloor = 0.25
const vmemRankLambda = 5.0

// RecallRequest — вход VMEM.RECALL до кухни фильтра.
type RecallRequest struct {
	Scope  string    // чья память; обязателен
	Query  string    // текстовый запрос (лексическое плечо); обязателен
	Vector []float32 // эмбеддинг запроса; nil → BM25-only (ступень 0: placeholder-запрос был бы шумовым плечом в RRF)
	K      int       // top-K
	AsOf   *int64    // история: валидность на момент ts вместо now (application time, не transaction time)
	All    bool      // отключить фильтр валидности; erasure (TTL/FORGET) скрыт ВСЕГДА
	TypeEq string    // опциональный фильтр вида факта
	// SourceEq — опциональный фильтр по происхождению («что пришло из этого
	// канала»). Идёт по той же полосе, что TypeEq: точный CAT-Eq, который
	// колоночный слой судит батчем. Форензический вход: сначала посмотреть
	// глазами на всё от подозрительного источника, потом отзывать.
	SourceEq string
	// HalfLifeSec — период полураспада свежести в секундах (шаг 5);
	// 0 → vmemDefaultHalfLifeSec. Политика затухания принадлежит клиенту.
	HalfLifeSec int64
}

// recallFilter — чистая кухня фильтра RECALL: три режима валидности = один
// Filter с разными границами Range (контракт VMEM_DESIGN).
//   - дефолт: валидное-сейчас  ≡ AS_OF now;
//   - AS_OF ts: valid_from <= ts < valid_to — «что было истинно в ts»;
//   - ALL: интервал не судится вовсе.
//
// Erasure — отдельная ось и фильтруется во ВСЕХ режимах против now (не ts):
// «право быть забытым побеждает машину времени», а до TTL-жнеца (шаг 6)
// истёкший факт ещё физически жив — Range по expires_at скрывает его уже
// на чтении. Все времена — целые unix-секунды < 2^53, поэтому строгие
// неравенства модели (t < valid_to, expires_at > now) выражаются
// инклюзивным Range сдвигом границы на +1.
func recallFilter(req RecallRequest, now int64) (Filter, error) {
	if now <= 0 || now >= vmemOpenValidTo {
		return Filter{}, ErrVMEMNow
	}
	if req.Scope == "" {
		return Filter{}, ErrVMEMScope
	}
	if req.Query == "" {
		return Filter{}, ErrVMEMQuery
	}
	if req.K <= 0 {
		return Filter{}, ErrVMEMK
	}
	if req.AsOf != nil && (*req.AsOf <= 0 || *req.AsOf >= vmemOpenValidTo || req.All) {
		return Filter{}, ErrVMEMAsOf
	}
	if req.HalfLifeSec < 0 {
		return Filter{}, ErrVMEMHalfLife
	}
	f := Filter{
		Eq: map[string]string{vmemAttrScope: req.Scope},
		Range: map[string][2]float64{
			vmemAttrExpiresAt: {float64(now + 1), float64(vmemOpenValidTo)},
		},
	}
	if req.TypeEq != "" {
		f.Eq[vmemAttrType] = req.TypeEq
	}
	if req.SourceEq != "" {
		f.Eq[vmemAttrSource] = req.SourceEq
	}
	if !req.All {
		tEff := now
		if req.AsOf != nil {
			tEff = *req.AsOf
		}
		f.Range[vmemAttrValidFrom] = [2]float64{0, float64(tEff)}
		f.Range[vmemAttrValidTo] = [2]float64{float64(tEff + 1), float64(vmemOpenValidTo)}
	}
	return f, nil
}

// Recall — внутренний путь VMEM.RECALL (RESP-команда — шаг 7): кухня фильтра
// + filter-then-fuse поверх готовых плеч + скоринг памяти (шаг 5). Без вектора
// запроса гибрид осознанно вырождается в BM25-only (SearchTextFilter): RRF
// слеп к скорам, и шумовое плечо из placeholder-векторов голосовало бы наравне
// с осмысленным.
//
// Скоринг (шаг 5, формулы пересмотрены судом 23.07 — см. vmemDecayFloor /
// vmemRankLambda) — чистая арифметика ранг-слоя поверх top-overfetch
// кандидатов, ядро не тронуто. Пути скорятся ПО-РАЗНОМУ, потому что живут в
// разных шкалах:
//
//	BM25-only: final = score × max(2^(−age/halfLife), 0.25) × (0.5 + importance)
//	hybrid:    fused = Σ по плечам 1/(rrfK + rank + 5·age/halfLife);
//	           final = fused × (0.5 + importance)
//
// где age = tEff − valid_from, tEff = as_of|now: свежесть меряется на момент,
// О КОТОРОМ спрашивают, не на момент запроса («что было важно тогда»).
// Нейтральная importance 0.5 даёт множитель ровно 1; ни возраст, ни importance
// не обнуляют факт (пол/ранговый штраф) — затухание двигает порядок, но не
// прячет (прятать умеет только erasure). Риск «ранговая шкала RRF слишком
// плоская против decay» из шага 5 ПОДТВЕРДИЛСЯ бенчем (hit@10 >90d = 0.003)
// и закрыт ранговым штрафом: возраст платит там же, где живёт скор.
func (lvs *LeveledVectorStore) Recall(req RecallRequest, now int64) ([]VTextResult, error) {
	return lvs.recall(req, now, nil)
}

// recall — ОБЩЕЕ ядро RECALL и EXPLAIN. tr != nil → по дороге собирается
// разложение скора (vmem_explain.go). Отдельной реализации объяснения здесь
// быть не должно принципиально: объяснение, живущее своей функцией,
// расходится с настоящим ранжированием ровно тогда, когда им пользуются — в
// разборе инцидента, где цена ошибки максимальна. Поэтому EXPLAIN не
// «повторяет» скоринг, а смотрит, как считает он сам.
func (lvs *LeveledVectorStore) recall(req RecallRequest, now int64, tr *recallTrace) ([]VTextResult, error) {
	f, err := recallFilter(req, now)
	if err != nil {
		return nil, err
	}
	tEff := now
	if req.AsOf != nil {
		tEff = *req.AsOf
	}
	halfLife := req.HalfLifeSec
	if halfLife == 0 {
		halfLife = vmemDefaultHalfLifeSec
	}

	depth := max(vmemRecallOverfetch, req.K)
	hybrid := req.Vector != nil
	var cands []VTextResult // BM25-only: скоры плеча; hybrid: собирается ПОСЛЕ проекции
	var keys []string
	var textArm []VTextResult
	var vecArm []VSearchResult
	var pos map[string]int
	if !hybrid {
		cands, err = lvs.SearchTextFilter(req.Query, depth, f)
		if err != nil || len(cands) == 0 {
			return cands, err
		}
		keys = make([]string, len(cands))
		for i := range cands {
			keys[i] = cands[i].Key
		}
	} else {
		// Гибрид сливается ЗДЕСЬ, не в SearchHybridFilter: возрастной штраф
		// живёт в ранговой шкале (см. vmemRankLambda), а для него нужны
		// valid_from кандидатов ДО фьюжна. Плечи, глубина и filter-then-fuse —
		// бит-в-бит контракт SearchHybridFilter.
		if textArm, err = lvs.SearchTextFilter(req.Query, depth, f); err != nil {
			return nil, err
		}
		if vecArm, err = lvs.SearchFilter(req.Vector, depth, f); err != nil {
			return nil, err
		}
		if len(textArm)+len(vecArm) == 0 {
			return nil, nil
		}
		pos = make(map[string]int, len(textArm)+len(vecArm))
		add := func(k string) {
			if _, ok := pos[k]; !ok {
				pos[k] = len(keys)
				keys = append(keys, k)
			}
		}
		for i := range textArm {
			add(textArm[i].Key)
		}
		for i := range vecArm {
			add(vecArm[i].Key)
		}
	}
	// Пере-суд кандидатов по СВЕЖАЙШЕЙ версии ключа (найдено корпус-бенчем
	// шага 8, репро TestVMEMSupersedeTwoSegmentLeak): пре-фильтр судит каждую
	// копию дока внутри её источника, а затенение сегмент-vs-сегмент решается
	// только среди хитов (bm25_store.go, merge). Закрытая supersession-копия в
	// новом сегменте отсекается фильтром валидности ДО хитов — она и должна
	// быть невалидной, — и глушить старую ОТКРЫТУЮ копию в старом сегменте
	// некому: закрытая версия воскресала бы как «истинная сейчас» на любом
	// переходном мульти-сегментном состоянии LSM. Пере-суд тем же предикатом
	// по свежайшим атрибутам (провенанс numsForKeys ≡ Get) восстанавливает
	// контракт; цена — те же O(log n)-лукапы, что уже платит скоринг, +2
	// колонки в общей проекции. NaN expires_at → свежайшая версия не
	// VMEM-факт (upsert заменил факт не-фактом) → кандидат стейл, вон.
	nums := lvs.numsForKeys(keys, vmemAttrValidFrom, vmemAttrImp, vmemAttrValidTo, vmemAttrExpiresAt, vmemAttrQuarantinedAt)
	if hybrid {
		// Слияние в ранговой шкале с возрастным штрафом: 1/(rrfK + rank + λ·hl),
		// hl = возраст в полураспадах на момент tEff. NaN valid_from (не-VMEM
		// док в scope) и будущие факты — штраф нейтрален (0), как в vmemDecayImp.
		agePen := func(key string) float64 {
			vf := nums[pos[key]][0]
			if math.IsNaN(vf) {
				return 0
			}
			age := float64(tEff) - vf
			if age <= 0 {
				return 0
			}
			return vmemRankLambda * age / float64(halfLife)
		}
		fused := make([]float64, len(keys))
		for r := range textArm {
			fused[pos[textArm[r].Key]] += 1.0 / (float64(rrfK+r+1) + agePen(textArm[r].Key))
		}
		for r := range vecArm {
			fused[pos[vecArm[r].Key]] += 1.0 / (float64(rrfK+r+1) + agePen(vecArm[r].Key))
		}
		cands = make([]VTextResult, len(keys))
		for i, k := range keys {
			cands[i] = VTextResult{Key: k, Score: fused[i]}
		}
	}

	// Снимок ДО множителей памяти: дальше цикл домножает cands[i].Score на
	// месте и переиспользует тот же массив под kept (cands[:0]), так что после
	// него исходных скоров уже не существует.
	tr.begin(keys, nums, cands, textArm, vecArm, hybrid, tEff, halfLife)

	var typeOK []bool
	if req.TypeEq != "" {
		typeOK = lvs.catEqForKeys(keys, vmemAttrType, req.TypeEq)
	}
	// SourceEq пере-судится по той же причине, что TypeEq: стейл-копия из
	// старого сегмента могла пройти пре-фильтр по источнику, который свежайший
	// upsert уже сменил.
	var sourceOK []bool
	if req.SourceEq != "" {
		sourceOK = lvs.catEqForKeys(keys, vmemAttrSource, req.SourceEq)
	}
	kept := cands[:0]
	for i := range cands {
		vf, imp, vt, exp, quar := nums[i][0], nums[i][1], nums[i][2], nums[i][3], nums[i][4]
		if !(exp > float64(now)) { // erasure против now во ВСЕХ режимах; NaN → false
			tr.drop(i, DropErasure)
			continue
		}
		if !req.All && !(vf <= float64(tEff) && float64(tEff) < vt) {
			tr.drop(i, DropValidity)
			continue
		}
		// Карантин — ТРЕТЬЯ ось, отдельная и от валидности, и от erasure:
		// «убеждение отозвано в момент q». Судится против tEff, а не now
		// (в отличие от erasure): отзыв произошёл в конкретный момент истории,
		// и вопрос «во что агент верил в 14:32» обязан получить честный ответ
		// «верил вот в это» — даже если в 16:10 мы это отозвали. Стирать эту
		// запись значило бы уничтожать ровно ту улику, ради которой слой и
		// существует. NaN (атрибута нет) = никогда не карантинился — обычный
		// случай, и он не требует ни штампа, ни миграции старых данных.
		// ALL показывает всё: это форензический режим.
		if !req.All && !math.IsNaN(quar) && float64(tEff) >= quar {
			tr.drop(i, DropQuarantine)
			continue
		}
		if typeOK != nil && !typeOK[i] {
			tr.drop(i, DropType)
			continue
		}
		if sourceOK != nil && !sourceOK[i] {
			tr.drop(i, DropSource)
			continue
		}
		if hybrid {
			cands[i].Score *= vmemImpFactor(imp) // возраст уже уплачен рангом
		} else {
			cands[i].Score *= vmemDecayImp(vf, imp, tEff, halfLife)
		}
		tr.keep(i, cands[i].Score)
		kept = append(kept, cands[i])
	}
	cands = kept
	slices.SortFunc(cands, func(a, b VTextResult) int {
		if a.Score > b.Score {
			return -1
		}
		if a.Score < b.Score {
			return 1
		}
		return strings.Compare(a.Key, b.Key)
	})
	if len(cands) > req.K {
		cands = cands[:req.K]
	}
	tr.finish(cands)
	return cands, nil
}

// vmemDecayImp — множитель памяти поверх скора похожести (BM25-only путь).
// NaN (атрибута нет — не-VMEM док, попавший в scope) → фактор нейтрален.
// Пол vmemDecayFloor: затухание двигает порядок, но не квантует в ноль —
// широкой BM25-шкале хватает, чтобы старая точная находка пережила деление
// на 4, но не пережила бы 2^-6 (суд 23.07: para/>90d 0.762→0.985).
func vmemDecayImp(validFrom, imp float64, tEff, halfLife int64) float64 {
	m := 1.0
	if !math.IsNaN(validFrom) {
		if age := float64(tEff) - validFrom; age > 0 {
			m = math.Max(math.Exp2(-age/float64(halfLife)), vmemDecayFloor)
		}
	}
	return m * vmemImpFactor(imp)
}

// vmemImpFactor — множитель importance (0.5+imp); NaN → нейтрален. В гибриде
// применяется ОДИН (возраст уже уплачен ранговым штрафом): диапазон 0.5×–1.5×
// двигает порядок в ранговой шкале, не квантуя её (в отличие от 2^(−age/HL)).
func vmemImpFactor(imp float64) float64 {
	if math.IsNaN(imp) {
		return 1
	}
	return 0.5 + imp
}

// catEqForKeys — батч-проверка «CAT-атрибут СВЕЖАЙШЕЙ версии == want» по
// ключам кандидатов: пере-суд TypeEq в Recall (см. комментарий там) — стейл
// копия из старого сегмента могла пройти Eq по типу, который свежайший upsert
// уже сменил. Провенанс — factCatAttrLocked (≡ Get).
func (lvs *LeveledVectorStore) catEqForKeys(keys []string, name, want string) []bool {
	out := make([]bool, len(keys))
	lvs.mu.RLock()
	defer lvs.mu.RUnlock()
	for i, key := range keys {
		v, ok := lvs.factCatAttrLocked(key, name)
		out[i] = ok && v == want
	}
	return out
}

// numsForKeys — батч-проекция NUM-атрибутов по ключам кандидатов: значения
// СВЕЖАЙШЕЙ версии дока (провенанс Get: активная дельта → flushing
// новее-первым → сегменты свежее-первым), NaN = атрибута нет. Цена на ключ:
// O(1) в memtable (keyIdx), O(log n) во frozen/SQ (sorted-пермутация
// internedKeys), O(n карты) в hnswSegment — линейный обход id→key принят для
// v1 (высокоразмерный BYO-путь; обратная мапа под s.mu — открытая дверь,
// добавить по QPS-бенчу, не заранее).
func (lvs *LeveledVectorStore) numsForKeys(keys []string, names ...string) [][]float64 {
	out := make([][]float64, len(keys))
	todo := len(keys)
	for i := range out {
		row := make([]float64, len(names))
		for j := range row {
			row[j] = math.NaN()
		}
		out[i] = row
	}
	fillFromAttrs := func(i int, a Attributes) {
		for j, name := range names {
			if v, ok := a.Num[name]; ok {
				out[i][j] = v
			}
		}
	}

	lvs.mu.RLock()
	defer lvs.mu.RUnlock()
	memtables := make([]*DeltaSegment, 0, 1+len(lvs.flushing))
	if lvs.delta != nil {
		memtables = append(memtables, lvs.delta)
	}
	for i := len(lvs.flushing) - 1; i >= 0; i-- {
		memtables = append(memtables, lvs.flushing[i])
	}
	done := make([]bool, len(keys))
	for _, mt := range memtables {
		if todo == 0 {
			return out
		}
		for i, key := range keys {
			if done[i] {
				continue
			}
			if a, ok := mt.GetAttrs(key); ok {
				fillFromAttrs(i, a)
				done[i] = true
				todo--
			}
		}
	}
	fillFromCols := func(sa *segmentAttrs, i, idx int) {
		if sa == nil {
			return
		}
		for j, name := range names {
			if col, ok := sa.num[name]; ok {
				out[i][j] = col.vals[idx] // NaN, если атрибута нет у дока
			}
		}
	}
	for _, level := range lvs.levels {
		for pos := len(level) - 1; pos >= 0; pos-- {
			if todo == 0 {
				return out
			}
			switch s := level[pos].(type) {
			case *frozenSegment:
				for i, key := range keys {
					if done[i] {
						continue
					}
					if idx, ok := s.fg.keys.find(key); ok {
						fillFromCols(s.attrs, i, idx)
						done[i] = true
						todo--
					}
				}
			case *frozenSQSegment:
				for i, key := range keys {
					if done[i] {
						continue
					}
					if idx, ok := s.fg.keys.find(key); ok {
						fillFromCols(s.attrs, i, idx)
						done[i] = true
						todo--
					}
				}
			case *hnswSegment:
				s.mu.RLock()
				for id, k := range s.keys {
					if !s.g.nodes[id].Alive {
						continue
					}
					for i, key := range keys {
						if !done[i] && k == key {
							fillFromCols(s.attrs, i, int(id))
							done[i] = true
							todo--
							break
						}
					}
				}
				s.mu.RUnlock()
			}
		}
	}
	return out
}

// RememberResult — материализованный результат REMEMBER: командный слой
// (шаг 7) пишет в WAL именно эти доки (Key=ID, Value=SerializeVectorWithDoc)
// без пересборки полей (дверь 1). При supersedes в WAL обязана уехать ВСЯ
// пара одним атомарным батчем: полузаписанная пара после краша — это либо два
// «истинных сейчас» факта, либо факт, закрытый без наследника, и реплей
// честно воспроизведёт любую из этих лжей.
type RememberResult struct {
	Doc    RememberedDoc  // новый факт
	Closed *RememberedDoc // re-ingest цели supersedes с закрытым valid_to; nil без supersedes
}

// Remember — внутренний путь VMEM.REMEMBER (RESP-команда — шаг 7): кухня
// полей + вставка через тот же choke-point AddDocTerms, что ADDDOC и реплей
// WAL. now — unix-секунды серверных часов; часы существуют только у
// вызывающего, кухня и реплей их не читают. Порядок «вставка до записи WAL»
// сохраняет watermark-safety, как у VSIM.ADD/ADDDOC.
func (lvs *LeveledVectorStore) Remember(req RememberRequest, now int64) (RememberResult, error) {
	if req.Supersedes != "" {
		return lvs.rememberSupersede(req, now)
	}
	lvs.mu.RLock()
	dim := lvs.dim
	lvs.mu.RUnlock()
	doc, err := rememberDoc(req, now, dim)
	if err != nil {
		return RememberResult{}, err
	}
	if err := lvs.AddDocTerms(doc.ID, doc.Vec, doc.Attrs, doc.Terms); err != nil {
		return RememberResult{}, err
	}
	return RememberResult{Doc: doc}, nil
}

// rememberSupersede — REMEMBER с закрытием интервала цели (шаг 4, вариант «c»
// VMEM_DESIGN): цель перечитывается из стора и re-ingest'ится тем же
// содержимым с valid_to = valid_from наследника. Весь read-modify-write — под
// эксклюзивным lvs.mu.Lock: параллельный upsert цели либо целиком до (закроем
// уже его содержимое), либо целиком после (перезапишет закрытие заново
// открытым фактом — поздний прав); интерливинг «прочитал старое → затёр
// новое» исключён. Валидация цели — до любых вставок: ошибка не оставляет
// полузаписанной пары.
func (lvs *LeveledVectorStore) rememberSupersede(req RememberRequest, now int64) (RememberResult, error) {
	lvs.mu.Lock()
	doc, err := rememberDoc(req, now, lvs.dim)
	if err != nil {
		lvs.mu.Unlock()
		return RememberResult{}, err
	}
	if err := ValidateVector(doc.Vec); err != nil {
		lvs.mu.Unlock()
		return RememberResult{}, err
	}
	target, ok := lvs.getFactDocLocked(req.Supersedes)
	if !ok {
		lvs.mu.Unlock()
		return RememberResult{}, ErrVMEMSupersedesTarget
	}
	if target.Attrs.Cat[vmemAttrScope] != req.Scope {
		lvs.mu.Unlock()
		return RememberResult{}, ErrVMEMSupersedesScope
	}
	// Цель, истёкшая по TTL, — уже не цель, даже если жнец (шаг 6) её ещё не
	// пожал физически: иначе успех supersedes зависел бы от расписания жнеца.
	// Erasure судится тем же правилом, что фильтр чтения: expires_at <= now.
	if exp, ok := target.Attrs.Num[vmemAttrExpiresAt]; ok && exp <= float64(now) {
		lvs.mu.Unlock()
		return RememberResult{}, ErrVMEMSupersedesTarget
	}
	closed := closeFactDoc(target, doc.Attrs.Num[vmemAttrValidFrom])

	if lvs.delta == nil {
		if err := lvs.initDeltaLocked(len(doc.Vec)); err != nil {
			lvs.mu.Unlock()
			return RememberResult{}, err
		}
	}
	if len(doc.Vec) != lvs.dim {
		lvs.mu.Unlock()
		return RememberResult{}, &dimMismatchError{expected: lvs.dim, got: len(doc.Vec)}
	}
	fullA := lvs.delta.AppendDoc(closed.ID, closed.Vec, closed.Attrs, closed.Terms)
	fullB := lvs.delta.AppendDoc(doc.ID, doc.Vec, doc.Attrs, doc.Terms)
	lvs.touchMutation()
	lvs.removeTombstone(closed.ID)
	lvs.removeTombstone(doc.ID)
	lvs.mu.Unlock()
	if fullA || fullB {
		lvs.triggerCompact()
	}
	return RememberResult{Doc: doc, Closed: &closed}, nil
}

// closeFactDoc — док закрытия интервала: содержимое цели бит-в-бит (вектор,
// термы, остальные атрибуты), valid_to = valid_from наследника. Attrs —
// глубокая копия: мапы из дельты делятся с исходной вставкой, а из flushing-
// дельты их параллельно читает build-горутина — мутация на месте отравила бы
// строящийся сегмент.
func closeFactDoc(target DeltaEntry, validTo float64) RememberedDoc {
	attrs := Attributes{Num: make(map[string]float64, len(target.Attrs.Num)+1)}
	if target.Attrs.Cat != nil {
		attrs.Cat = maps.Clone(target.Attrs.Cat)
	}
	maps.Copy(attrs.Num, target.Attrs.Num)
	attrs.Num[vmemAttrValidTo] = validTo
	return RememberedDoc{ID: target.Key, Vec: target.Vec, Attrs: attrs, Terms: target.Terms}
}

// getFactDocLocked — материализованный док по ключу через все LSM-состояния:
// активная дельта → tombstone-маска → flushing-дельты новее-первым → сегменты
// свежее-первым (зеркало провенанса Get, см. комментарии там). Вызывать под
// lvs.mu (supersedes-путь держит Lock — состояние неподвижно на весь RMW).
// Термы из frozen-слоёв восстанавливаются точечно (termsForDoc).
func (lvs *LeveledVectorStore) getFactDocLocked(key string) (DeltaEntry, bool) {
	if lvs.delta != nil {
		if e, ok := lvs.delta.GetDoc(key); ok {
			return e, true
		}
	}
	if t := lvs.tombstones.Load(); t != nil {
		if _, deleted := (*t)[key]; deleted {
			return DeltaEntry{}, false
		}
	}
	for i := len(lvs.flushing) - 1; i >= 0; i-- {
		if e, ok := lvs.flushing[i].GetDoc(key); ok {
			return e, true
		}
	}
	for _, level := range lvs.levels {
		for pos := len(level) - 1; pos >= 0; pos-- {
			switch s := level[pos].(type) {
			case *frozenSegment:
				fg := s.fg
				if i, ok := fg.keys.find(key); ok {
					vec := make([]float32, fg.dim)
					copy(vec, fg.data[i*fg.dim:(i+1)*fg.dim])
					return DeltaEntry{Key: key, Vec: vec, Attrs: s.attrs.decodeAt(i), Terms: s.text.termsForDoc(uint32(i))}, true
				}
			case *frozenSQSegment:
				fg := s.fg
				if i, ok := fg.keys.find(key); ok {
					vec := make([]float32, fg.dim)
					base := i * fg.dim
					for d := 0; d < fg.dim; d++ {
						vec[d] = fg.sqMin[d] + float32(fg.codes[base+d])*fg.sqScale[d]
					}
					return DeltaEntry{Key: key, Vec: vec, Attrs: s.attrs.decodeAt(i), Terms: s.text.termsForDoc(uint32(i))}, true
				}
			case *hnswSegment:
				s.mu.RLock()
				for id, k := range s.keys {
					if k == key && s.g.nodes[id].Alive {
						raw := s.g.arena.Get(s.g.nodes[id].VectorOffset)
						vec := make([]float32, len(raw))
						copy(vec, raw)
						e := DeltaEntry{Key: key, Vec: vec, Attrs: s.attrs.decodeAt(int(id)), Terms: s.text.termsForDoc(uint32(id))}
						s.mu.RUnlock()
						return e, true
					}
				}
				s.mu.RUnlock()
			}
		}
	}
	return DeltaEntry{}, false
}
