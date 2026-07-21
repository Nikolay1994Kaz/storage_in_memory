package vector

import (
	"errors"
	"fmt"
	"hash/fnv"
	"maps"
	"math"
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
// Ещё НЕ сделано: FORGET/TTL-жнец (шаг 6), RESP-команда VMEM.* (шаг 7).
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
	vmemAttrImp        = "imp"
	vmemAttrValidFrom  = "valid_from"
	vmemAttrValidTo    = "valid_to"
	vmemAttrExpiresAt  = "expires_at"
)

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
	// ErrVMEMQuery — запрос RECALL обязателен: recall — это поиск, не скан scope.
	ErrVMEMQuery = errors.New("vmem: query is required")
	// ErrVMEMK — top-K RECALL должен быть положительным.
	ErrVMEMK = errors.New("vmem: k must be positive")
	// ErrVMEMAsOf — as_of вне (0, сентинел) либо задан вместе с all.
	ErrVMEMAsOf = errors.New("vmem: as_of must be positive unix seconds below the sentinel and is mutually exclusive with all")
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

	attrs := Attributes{
		Cat: map[string]string{vmemAttrScope: req.Scope},
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

// RecallRequest — вход VMEM.RECALL до кухни фильтра (шаг 3: правильное
// множество, глупый порядок — скоринг свежесть×важность придёт в шаге 5).
type RecallRequest struct {
	Scope  string    // чья память; обязателен
	Query  string    // текстовый запрос (лексическое плечо); обязателен
	Vector []float32 // эмбеддинг запроса; nil → BM25-only (ступень 0: placeholder-запрос был бы шумовым плечом в RRF)
	K      int       // top-K
	AsOf   *int64    // история: валидность на момент ts вместо now (application time, не transaction time)
	All    bool      // отключить фильтр валидности; erasure (TTL/FORGET) скрыт ВСЕГДА
	TypeEq string    // опциональный фильтр вида факта
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
	f := Filter{
		Eq: map[string]string{vmemAttrScope: req.Scope},
		Range: map[string][2]float64{
			vmemAttrExpiresAt: {float64(now + 1), float64(vmemOpenValidTo)},
		},
	}
	if req.TypeEq != "" {
		f.Eq[vmemAttrType] = req.TypeEq
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
// + filter-then-fuse поверх готовых плеч. Без вектора запроса гибрид осознанно
// вырождается в BM25-only (SearchTextFilter): RRF слеп к скорам, и шумовое
// плечо из placeholder-векторов голосовало бы наравне с осмысленным.
// Порядок выдачи на шаге 3 «глупый» (BM25/RRF); decay×importance — шаг 5.
func (lvs *LeveledVectorStore) Recall(req RecallRequest, now int64) ([]VTextResult, error) {
	f, err := recallFilter(req, now)
	if err != nil {
		return nil, err
	}
	if req.Vector == nil {
		return lvs.SearchTextFilter(req.Query, req.K, f)
	}
	return lvs.SearchHybridFilter(req.Query, req.Vector, req.K, f)
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
				for i := 0; i < fg.n; i++ {
					if fg.keys.view(i) == key {
						vec := make([]float32, fg.dim)
						copy(vec, fg.data[i*fg.dim:(i+1)*fg.dim])
						return DeltaEntry{Key: key, Vec: vec, Attrs: s.attrs.decodeAt(i), Terms: s.text.termsForDoc(uint32(i))}, true
					}
				}
			case *frozenSQSegment:
				fg := s.fg
				for i := 0; i < fg.n; i++ {
					if fg.keys.view(i) == key {
						vec := make([]float32, fg.dim)
						base := i * fg.dim
						for d := 0; d < fg.dim; d++ {
							vec[d] = fg.sqMin[d] + float32(fg.codes[base+d])*fg.sqScale[d]
						}
						return DeltaEntry{Key: key, Vec: vec, Attrs: s.attrs.decodeAt(i), Terms: s.text.termsForDoc(uint32(i))}, true
					}
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
