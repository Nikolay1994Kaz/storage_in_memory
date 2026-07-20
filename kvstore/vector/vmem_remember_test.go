package vector

// Тесты шага 2 VMEM-спринта (docs/VMEM_DESIGN.md): white-box кухни полей
// REMEMBER, WAL round-trip бит-в-бит (дверь 1) и частичное включение оракула —
// лента операций сценариев через живой путь Remember/Delete с проверкой
// следствий через реальные пути чтения (Get / SearchText / SearchFilter).
// RECALL-запросы сценариев остаются шагу 3 (TestVMEMOracleParity).

import (
	"bytes"
	"math"
	"reflect"
	"slices"
	"strings"
	"testing"
)

// ulidTimeMs декодирует 48-битное время (мс) из первых 10 символов ULID.
func ulidTimeMs(t *testing.T, s string) int64 {
	t.Helper()
	if len(s) != 26 {
		t.Fatalf("ULID %q: длина %d, ожидалось 26", s, len(s))
	}
	var ms int64
	for i := 0; i < 10; i++ {
		v := strings.IndexByte(ulidAlphabet, s[i])
		if v < 0 {
			t.Fatalf("ULID %q: символ %q вне алфавита", s, s[i])
		}
		ms = ms<<5 | int64(v)
	}
	return ms // 50 бит со старшими двумя нулями = 48-битное время
}

func TestULIDFormat(t *testing.T) {
	const ms = int64(1_753_000_000_123)
	a, err := NewULID(ms)
	if err != nil {
		t.Fatalf("NewULID: %v", err)
	}
	if got := ulidTimeMs(t, a); got != ms {
		t.Fatalf("время из ULID = %d, ожидалось %d", got, ms)
	}
	for i := 0; i < len(a); i++ {
		if strings.IndexByte(ulidAlphabet, a[i]) < 0 {
			t.Fatalf("ULID %q: символ %q вне Crockford base32", a, a[i])
		}
	}
	// Случайная часть: два ULID одного момента различны.
	b, err := NewULID(ms)
	if err != nil {
		t.Fatalf("NewULID: %v", err)
	}
	if a == b {
		t.Fatalf("два ULID совпали: %q", a)
	}
	// Лексикографический порядок следует времени (мотивация выбора ULID).
	later, err := NewULID(ms + 1)
	if err != nil {
		t.Fatalf("NewULID: %v", err)
	}
	if !(a < later) {
		t.Fatalf("ULID не сортируется по времени: %q !< %q", a, later)
	}
}

func TestRememberKitchenDefaults(t *testing.T) {
	const now = int64(1_753_000_000)
	doc, err := rememberDoc(RememberRequest{Scope: "user:dana", Text: "кофе без сахара"}, now, 0)
	if err != nil {
		t.Fatalf("rememberDoc: %v", err)
	}
	if ms := ulidTimeMs(t, doc.ID); ms != now*1000 {
		t.Errorf("время ULID = %d, ожидалось %d", ms, now*1000)
	}
	wantCat := map[string]string{vmemAttrScope: "user:dana"}
	if !reflect.DeepEqual(doc.Attrs.Cat, wantCat) {
		t.Errorf("Cat = %v, ожидалось %v (type/supersedes не заданы — ключей нет)", doc.Attrs.Cat, wantCat)
	}
	wantNum := map[string]float64{
		vmemAttrImp:       0.5,
		vmemAttrValidFrom: float64(now),
		vmemAttrValidTo:   float64(vmemOpenValidTo),
		vmemAttrExpiresAt: float64(vmemOpenValidTo),
	}
	if !reflect.DeepEqual(doc.Attrs.Num, wantNum) {
		t.Errorf("Num = %v, ожидалось %v", doc.Attrs.Num, wantNum)
	}
	if len(doc.Vec) != vmemPlaceholderDim {
		t.Errorf("placeholder на пустом сторе: dim %d, ожидалось %d", len(doc.Vec), vmemPlaceholderDim)
	}
	if want := TokenizeDoc("кофе без сахара"); !reflect.DeepEqual(doc.Terms, want) {
		t.Errorf("Terms = %v, ожидалось %v", doc.Terms, want)
	}
}

func TestRememberKitchenExplicitFields(t *testing.T) {
	const now = int64(1_753_000_000)
	imp := 1.0
	doc, err := rememberDoc(RememberRequest{
		ID:         "fact:1",
		Scope:      "user:dana",
		Text:       "Дана переехала в Алматы",
		Type:       "event",
		Importance: &imp,
		ValidFrom:  500,
		TTL:        600,
		Supersedes: "fact:0",
		Vector:     []float32{1, 2, 3},
	}, now, 3)
	if err != nil {
		t.Fatalf("rememberDoc: %v", err)
	}
	if doc.ID != "fact:1" {
		t.Errorf("клиентский ID потерян: %q", doc.ID)
	}
	if got := doc.Attrs.Num[vmemAttrValidFrom]; got != 500 {
		t.Errorf("valid_from = %v, ожидался клиентский override 500", got)
	}
	// TTL отсчитывается от ингеста (now), а не от override valid_from.
	if got, want := doc.Attrs.Num[vmemAttrExpiresAt], float64(now+600); got != want {
		t.Errorf("expires_at = %v, ожидалось now+ttl = %v", got, want)
	}
	if got := doc.Attrs.Cat[vmemAttrType]; got != "event" {
		t.Errorf("type = %q", got)
	}
	if got := doc.Attrs.Cat[vmemAttrSupersedes]; got != "fact:0" {
		t.Errorf("supersedes = %q", got)
	}
	if got := doc.Attrs.Num[vmemAttrImp]; got != 1.0 {
		t.Errorf("imp = %v", got)
	}
	if !reflect.DeepEqual(doc.Vec, []float32{1, 2, 3}) {
		t.Errorf("BYO-вектор подменён: %v", doc.Vec)
	}
}

func TestRememberKitchenValidation(t *testing.T) {
	const now = int64(1_753_000_000)
	neg, over, nan := -0.1, 1.1, math.NaN()
	ok := RememberRequest{Scope: "s", Text: "t"}
	cases := []struct {
		name string
		req  RememberRequest
		now  int64
		want error
	}{
		{"пустой scope", RememberRequest{Text: "t"}, now, ErrVMEMScope},
		{"пустой text", RememberRequest{Scope: "s"}, now, ErrVMEMText},
		{"importance < 0", RememberRequest{Scope: "s", Text: "t", Importance: &neg}, now, ErrVMEMImportance},
		{"importance > 1", RememberRequest{Scope: "s", Text: "t", Importance: &over}, now, ErrVMEMImportance},
		{"importance NaN", RememberRequest{Scope: "s", Text: "t", Importance: &nan}, now, ErrVMEMImportance},
		{"ttl < 0", RememberRequest{Scope: "s", Text: "t", TTL: -1}, now, ErrVMEMTTL},
		{"ttl достигает сентинела", RememberRequest{Scope: "s", Text: "t", TTL: vmemOpenValidTo - now}, now, ErrVMEMTTL},
		{"valid_from < 0", RememberRequest{Scope: "s", Text: "t", ValidFrom: -5}, now, ErrVMEMValidFrom},
		{"valid_from = сентинел", RememberRequest{Scope: "s", Text: "t", ValidFrom: vmemOpenValidTo}, now, ErrVMEMValidFrom},
		{"supersedes самого себя", RememberRequest{ID: "f", Scope: "s", Text: "t", Supersedes: "f"}, now, ErrVMEMSelfSupersedes},
		{"now = 0", ok, 0, ErrVMEMNow},
		{"now = сентинел", ok, vmemOpenValidTo, ErrVMEMNow},
	}
	for _, tc := range cases {
		if _, err := rememberDoc(tc.req, tc.now, 0); err != tc.want {
			t.Errorf("%s: err = %v, ожидалось %v", tc.name, err, tc.want)
		}
	}
	// Кривой клиентский id режется той же ValidateKey, что у всех ключей.
	if _, err := rememberDoc(RememberRequest{ID: strings.Repeat("x", MaxKeyLen+1), Scope: "s", Text: "t"}, now, 0); err == nil {
		t.Error("id длиннее MaxKeyLen прошёл кухню")
	}
	// Граничные значения importance легальны.
	for _, v := range []float64{0, 1} {
		v := v
		if _, err := rememberDoc(RememberRequest{Scope: "s", Text: "t", Importance: &v}, now, 0); err != nil {
			t.Errorf("importance=%v отвергнут: %v", v, err)
		}
	}
}

func TestVMEMPlaceholderVector(t *testing.T) {
	a := vmemPlaceholderVector("fact:1", 128)
	b := vmemPlaceholderVector("fact:1", 128)
	c := vmemPlaceholderVector("fact:2", 128)
	if !reflect.DeepEqual(a, b) {
		t.Error("placeholder недетерминирован для одного id")
	}
	if reflect.DeepEqual(a, c) {
		t.Error("разные id дали одинаковый placeholder")
	}
	var norm float64
	for _, v := range a {
		norm += float64(v) * float64(v)
	}
	if math.Abs(norm-1) > 1e-3 {
		t.Errorf("норма placeholder = %v, ожидалась ~1", math.Sqrt(norm))
	}
}

// TestRememberWALRoundTrip — прямая проверка двери 1: материализованный док
// переживает Serialize/Deserialize бит-в-бит (включая id в ключе и абсолютный
// expires_at), реплей в чистый стор через AddDocTerms даёт то же состояние,
// а повторный Remember того же запроса (ретрай с клиентским ID) — бит-в-бит
// тот же WAL-блоб (идемпотентность + детерминизм placeholder).
func TestRememberWALRoundTrip(t *testing.T) {
	const now = int64(1_753_000_000)
	lvs := NewLeveledVectorStore(bm25TestConfig())
	defer lvs.Close()

	imp := 0.9
	req := RememberRequest{
		ID:         "fact:wal",
		Scope:      "user:dana",
		Text:       "Дана переехала в Алматы",
		Type:       "event",
		Importance: &imp,
		TTL:        600,
		Supersedes: "fact:old",
	}
	doc, err := lvs.Remember(req, now)
	if err != nil {
		t.Fatalf("Remember: %v", err)
	}

	blob := SerializeVectorWithDoc(doc.Vec, doc.Attrs, doc.Terms)
	vec2, attrs2, terms2, err := DeserializeVectorWithDoc(blob)
	if err != nil {
		t.Fatalf("DeserializeVectorWithDoc: %v", err)
	}
	if len(vec2) != len(doc.Vec) {
		t.Fatalf("vec: длина %d != %d", len(vec2), len(doc.Vec))
	}
	for i := range vec2 {
		if math.Float32bits(vec2[i]) != math.Float32bits(doc.Vec[i]) {
			t.Fatalf("vec[%d]: битовое расхождение после round-trip", i)
		}
	}
	if !reflect.DeepEqual(attrs2, doc.Attrs) {
		t.Fatalf("attrs после round-trip: %v != %v", attrs2, doc.Attrs)
	}
	if got, want := attrs2.Num[vmemAttrExpiresAt], float64(now+600); got != want {
		t.Fatalf("expires_at приехал %v, ожидалось абсолютное %v (дверь 1)", got, want)
	}
	if !reflect.DeepEqual(terms2, doc.Terms) {
		t.Fatalf("terms после round-trip: %v != %v", terms2, doc.Terms)
	}

	// Реплей: чистый стор получает десериализованные значения тем же путём,
	// что и replay WAL (AddDocTerms, без перетокенизации/пересборки полей).
	replayed := NewLeveledVectorStore(bm25TestConfig())
	defer replayed.Close()
	if err := replayed.AddDocTerms(doc.ID, vec2, attrs2, terms2); err != nil {
		t.Fatalf("реплей AddDocTerms: %v", err)
	}
	orig, ok1 := lvs.Get(doc.ID)
	back, ok2 := replayed.Get(doc.ID)
	if !ok1 || !ok2 {
		t.Fatalf("Get после реплея: ok=%v/%v", ok1, ok2)
	}
	for i := range orig {
		if math.Float32bits(orig[i]) != math.Float32bits(back[i]) {
			t.Fatalf("вектор в реплей-сторе расходится в компоненте %d", i)
		}
	}
	res, err := replayed.SearchText("Алматы", 10)
	if err != nil || len(res) != 1 || res[0].Key != doc.ID {
		t.Fatalf("SearchText в реплей-сторе: %v, %v", res, err)
	}

	// Ретрай того же REMEMBER: тот же док и тот же блоб бит-в-бит.
	doc2, err := lvs.Remember(req, now)
	if err != nil {
		t.Fatalf("повторный Remember: %v", err)
	}
	if doc2.ID != doc.ID {
		t.Fatalf("ретрай сменил id: %q -> %q", doc.ID, doc2.ID)
	}
	if !bytes.Equal(blob, SerializeVectorWithDoc(doc2.Vec, doc2.Attrs, doc2.Terms)) {
		t.Fatal("ретрай с клиентским ID дал другой WAL-блоб — идемпотентность нарушена")
	}
}

// TestRememberUpsertSemantics — upsert по клиентскому ID = полная замена
// (старые термы затеняются), без ID каждый вызов = новый факт (дубль by
// design: ретрай неотличим от повторного события).
func TestRememberUpsertSemantics(t *testing.T) {
	lvs := NewLeveledVectorStore(bm25TestConfig())
	defer lvs.Close()

	if _, err := lvs.Remember(RememberRequest{ID: "fact:pref", Scope: "user:d", Text: "любит зелёный чай"}, 100); err != nil {
		t.Fatalf("Remember: %v", err)
	}
	if _, err := lvs.Remember(RememberRequest{ID: "fact:pref", Scope: "user:d", Text: "перешла на матчу"}, 200); err != nil {
		t.Fatalf("Remember upsert: %v", err)
	}
	if res, _ := lvs.SearchText("зелёный чай", 10); len(res) != 0 {
		t.Errorf("старый текст виден после upsert: %v", res)
	}
	res, _ := lvs.SearchText("матчу", 10)
	if len(res) != 1 || res[0].Key != "fact:pref" {
		t.Errorf("новый текст не найден: %v", res)
	}

	d1, err := lvs.Remember(RememberRequest{Scope: "user:d", Text: "абракадабра"}, 300)
	if err != nil {
		t.Fatalf("Remember: %v", err)
	}
	d2, err := lvs.Remember(RememberRequest{Scope: "user:d", Text: "абракадабра"}, 300)
	if err != nil {
		t.Fatalf("Remember: %v", err)
	}
	if d1.ID == d2.ID {
		t.Fatal("два REMEMBER без клиентского ID слились в один факт")
	}
	if res, _ := lvs.SearchText("абракадабра", 10); len(res) != 2 {
		t.Errorf("ожидались оба дубля, найдено: %v", res)
	}
}

// vmemIngestTruth — ожидаемое состояние ингеста одного факта: что обязано
// лежать в сторе после ленты операций (последний remember выигрывает).
type vmemIngestTruth struct {
	text, scope, typ, supersedes string
	imp                          float64
	validFrom, expiresAt         int64
	forgotten                    bool
}

// vmemIngestReplay — модель ингеста шага 2 (без семантики RECALL): дефолты и
// конвертации кухни в терминах ленты операций сценария.
func vmemIngestReplay(ops []vmemOp) map[string]*vmemIngestTruth {
	truth := make(map[string]*vmemIngestTruth)
	for _, op := range ops {
		switch op.Op {
		case "remember":
			f := &vmemIngestTruth{
				text: op.Text, scope: op.Scope, typ: op.Type, supersedes: op.Supersedes,
				imp: 0.5, validFrom: op.At, expiresAt: vmemOpenValidTo,
			}
			if op.Importance != nil {
				f.imp = *op.Importance
			}
			if op.TTL > 0 {
				f.expiresAt = op.At + op.TTL
			}
			truth[op.ID] = f
		case "forget":
			truth[op.ID].forgotten = true
		}
	}
	return truth
}

// TestVMEMOracleIngestParity — частичное включение оракула (шаг 2): лента
// операций каждого сценария через живой путь Remember/Delete в трёх
// состояниях LSM; следствия проверяются реальными путями чтения:
//   - Get: erasure (forget) невидим, остальные факты живы;
//   - SearchText по тексту факта: лексическая достижимость + затенение
//     upsert (эталонное множество — пересечение стемов, как в модели);
//   - SearchFilter точечными Eq/Range: атрибуты контракта (scope, type,
//     supersedes, imp, valid_from, valid_to, expires_at) лежат в колоночном
//     слое в точности как выдала кухня.
//
// Намеренные расхождения с полной моделью оракула (закрываются позже):
// valid_to у superseded-фактов здесь ещё сентинел (механизм закрытия — шаг 4),
// истёкший TTL физически жив (жнец — шаг 6), RECALL-запросы — шаг 3.
func TestVMEMOracleIngestParity(t *testing.T) {
	all := loadVMEMScenarios(t)
	states := []struct {
		name    string
		flushAt func(nOps int) int // после какой операции (1-based) звать FlushDeltaSync; 0 = никогда
	}{
		{"delta", func(int) int { return 0 }},
		{"flushed", func(n int) int { return n }},
		{"mixed", func(n int) int { return n / 2 }},
	}
	for _, sc := range all.Scenarios {
		for _, st := range states {
			t.Run(sc.Name+"/"+st.name, func(t *testing.T) {
				lvs := NewLeveledVectorStore(bm25TestConfig())
				defer lvs.Close()
				docs := make(map[string]RememberedDoc)
				flushAfter := st.flushAt(len(sc.Ops))
				for i, op := range sc.Ops {
					switch op.Op {
					case "remember":
						doc, err := lvs.Remember(RememberRequest{
							ID: op.ID, Scope: op.Scope, Text: op.Text, Type: op.Type,
							Importance: op.Importance, TTL: op.TTL, Supersedes: op.Supersedes,
						}, op.At)
						if err != nil {
							t.Fatalf("op[%d] Remember %s: %v", i, op.ID, err)
						}
						if doc.ID != op.ID {
							t.Fatalf("op[%d]: клиентский id %q подменён на %q", i, op.ID, doc.ID)
						}
						docs[op.ID] = doc
					case "forget":
						if !lvs.Delete(op.ID) {
							t.Fatalf("op[%d] forget %s: Delete=false", i, op.ID)
						}
					}
					if i+1 == flushAfter {
						lvs.FlushDeltaSync()
					}
				}

				truth := vmemIngestReplay(sc.Ops)
				K := len(truth) + 8
				for id, f := range truth {
					if _, ok := lvs.Get(id); ok == f.forgotten {
						t.Errorf("%s: Get=%v при forgotten=%v", id, ok, f.forgotten)
					}
					if f.forgotten {
						continue
					}

					res, err := lvs.SearchText(f.text, K)
					if err != nil {
						t.Fatalf("%s: SearchText: %v", id, err)
					}
					got := make([]string, 0, len(res))
					for _, r := range res {
						got = append(got, r.Key)
					}
					slices.Sort(got)
					qTokens := bm25Tokenize(f.text)
					var want []string
					for jid, jf := range truth {
						if jf.forgotten {
							continue
						}
						for _, qt := range qTokens {
							if slices.Contains(bm25Tokenize(jf.text), qt) {
								want = append(want, jid)
								break
							}
						}
					}
					slices.Sort(want)
					if !slices.Equal(got, want) {
						t.Errorf("%s: SearchText(%q) = %v, модель даёт %v", id, f.text, got, want)
					}

					filt := Filter{
						Eq: map[string]string{vmemAttrScope: f.scope},
						Range: map[string][2]float64{
							vmemAttrValidFrom: {float64(f.validFrom), float64(f.validFrom)},
							vmemAttrValidTo:   {float64(vmemOpenValidTo), float64(vmemOpenValidTo)},
							vmemAttrExpiresAt: {float64(f.expiresAt), float64(f.expiresAt)},
							vmemAttrImp:       {f.imp, f.imp},
						},
					}
					if f.typ != "" {
						filt.Eq[vmemAttrType] = f.typ
					}
					if f.supersedes != "" {
						filt.Eq[vmemAttrSupersedes] = f.supersedes
					}
					fres, err := lvs.SearchFilter(docs[id].Vec, K, filt)
					if err != nil {
						t.Fatalf("%s: SearchFilter: %v", id, err)
					}
					if !slices.ContainsFunc(fres, func(r VSearchResult) bool { return r.Key == id }) {
						t.Errorf("%s: точечный фильтр по атрибутам кухни не нашёл факт: %v", id, fres)
					}
					nres, err := lvs.SearchFilter(docs[id].Vec, K, Filter{Eq: map[string]string{vmemAttrScope: "no-such-scope-zzz"}})
					if err != nil {
						t.Fatalf("%s: SearchFilter (негатив): %v", id, err)
					}
					if slices.ContainsFunc(nres, func(r VSearchResult) bool { return r.Key == id }) {
						t.Errorf("%s: найден в чужом scope", id)
					}
				}
			})
		}
	}
}
