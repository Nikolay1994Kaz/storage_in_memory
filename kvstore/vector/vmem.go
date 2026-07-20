package vector

import (
	"errors"
	"fmt"
	"hash/fnv"
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
// Шаг 2 сознательно НЕ делает: закрытие valid_to цели supersedes (шаг 4 —
// пока только CAT-провенанс, существование цели не проверяется), RECALL
// (шаг 3), FORGET/TTL-жнец (шаг 6), RESP-команду (шаг 7).
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

// Remember — внутренний путь VMEM.REMEMBER (RESP-команда — шаг 7): кухня
// полей + вставка через тот же choke-point AddDocTerms, что ADDDOC и реплей
// WAL. now — unix-секунды серверных часов; часы существуют только у
// вызывающего, кухня и реплей их не читают.
//
// Возвращает материализованный док: командный слой пишет в WAL именно его
// (Key=ID, Value=SerializeVectorWithDoc) — пересборка полей после вставки
// запрещена (дверь 1). Порядок «вставка до записи WAL» сохраняет
// watermark-safety, как у VSIM.ADD/ADDDOC.
func (lvs *LeveledVectorStore) Remember(req RememberRequest, now int64) (RememberedDoc, error) {
	lvs.mu.RLock()
	dim := lvs.dim
	lvs.mu.RUnlock()
	doc, err := rememberDoc(req, now, dim)
	if err != nil {
		return RememberedDoc{}, err
	}
	if err := lvs.AddDocTerms(doc.ID, doc.Vec, doc.Attrs, doc.Terms); err != nil {
		return RememberedDoc{}, err
	}
	return doc, nil
}
