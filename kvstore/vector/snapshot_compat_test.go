package vector

import (
	"bytes"
	"errors"
	"math"
	"os"
	"path/filepath"
	"testing"
)

// =============================================================================
// Совместимость со снапшотами прежних версий.
//
// Эталоны в testdata/snapshots сняты настоящей сборкой прежних версий:
//   - v8_sq.bin, v8_hnsw.bin — коммит 531057a, последний до формата v9;
//   - v9_docs_*.bin — коммит debb774, последний со слоем памяти агентов. В
//     них документы с атрибутом scope лежат в документной секции сегмента
//     открытыми группами (frozen, SQ8 и hnsw — по эталону на тип);
//   - v9_docs_sealed.bin — та же фикстура под шифрованием (-encrypt-at-rest).
//
// Фикстура во всех файлах одна: факты f-alice-1, f-alice-2 (scope=alice),
// f-bob-1 (scope=bob) и документ plain-1 без scope; у каждого один терм.
// Проверяется, что обновление не теряет данные: документы, векторы, атрибуты
// и текст читаются, а снапшот, который прочитать нельзя, отклоняется целиком.
// =============================================================================

// Термы записаны в индекс как есть (AddDocTerms), а запрос SearchText проходит
// стемминг, поэтому текст проверяется только по словам, которые стемминг не
// меняет: "steering" в индексе лежит целиком, а запрос превращается в "steer".
var compatDocs = []struct {
	key, scope, term string
	textCheck        bool
}{
	{"f-alice-1", "alice", "aurora", true},
	{"f-alice-2", "alice", "steering", false},
	{"f-bob-1", "bob", "standup", true},
	{"plain-1", "", "weather", true},
}

// compatVec — вектор i-го документа фикстуры frozen/SQ8 (dim=8), та же формула,
// что при записи эталонов.
func compatVec(i int) []float32 {
	return []float32{float32(i + 1), float32(i + 2), float32(i + 3), float32(i + 4),
		float32(i), float32(i), float32(i), float32(i)}
}

// compatHnswVec — вектор i-го документа фикстуры hnsw (dim=300 > порога CSR,
// поэтому без SQ8 сегмент замерзает в hnswSegment).
func compatHnswVec(i int) []float32 {
	return mkVecN(300, float32(i+1)*1.5)
}

func sqCompatConfig() LeveledConfig {
	cfg := bm25TestConfig()
	cfg.UseSQ = true
	return cfg
}

func golden(t *testing.T, name string, wantVersion byte) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", "snapshots", name))
	if err != nil {
		t.Fatalf("эталон %s недоступен: %v", name, err)
	}
	// Контроль фикстуры: тест чтения старого формата, читающий новый, был бы
	// зелёным ни о чём.
	if len(b) < 6 || b[5] != wantVersion {
		t.Fatalf("эталон %s: версия формата не %d", name, wantVersion)
	}
	return b
}

// assertCompatDocs — все документы фикстуры восстановлены: ключ, вектор,
// атрибут scope и терм текста. Без атрибутов и термов документ перестаёт
// находиться, то есть «прочиталось» значило бы «прочиталось наполовину».
func assertCompatDocs(t *testing.T, lvs *LeveledVectorStore, vecOf func(int) []float32, tol float64) {
	t.Helper()
	for i, d := range compatDocs {
		got, ok := lvs.Get(d.key)
		if !ok {
			t.Errorf("документ %s не восстановился", d.key)
			continue
		}
		if vecOf != nil {
			want := vecOf(i)
			if len(got) != len(want) {
				t.Errorf("вектор %s: dim %d, ожидалась %d", d.key, len(got), len(want))
				continue
			}
			for j := range want {
				if math.Abs(float64(got[j]-want[j])) > tol {
					t.Errorf("вектор %s[%d] = %v, ожидалось %v", d.key, j, got[j], want[j])
					break
				}
			}
		}
		if d.textCheck {
			res, err := lvs.SearchText(d.term, 10)
			if err != nil {
				t.Fatalf("SearchText(%s): %v", d.term, err)
			}
			if !hasTextKey(res, d.key) {
				t.Errorf("терм %q не находит %s", d.term, d.key)
			}
		}
		if d.scope == "" {
			continue
		}
		// Атрибут — фильтром векторного поиска: документ обязан найтись по
		// собственному вектору среди документов своего scope и не найтись
		// среди чужого.
		for _, scope := range []string{"alice", "bob"} {
			res, err := lvs.SearchFilter(got, 10, Filter{Eq: map[string]string{"scope": scope}})
			if err != nil {
				t.Fatalf("SearchFilter(scope=%s): %v", scope, err)
			}
			if found := hasVecKey(res, d.key); found != (scope == d.scope) {
				t.Errorf("фильтр scope=%s: %s найден=%v, документ имеет scope=%s", scope, d.key, found, d.scope)
			}
		}
	}
}

func hasTextKey(res []VTextResult, key string) bool {
	for _, r := range res {
		if r.Key == key {
			return true
		}
	}
	return false
}

func hasVecKey(res []VSearchResult, key string) bool {
	for _, r := range res {
		if r.Key == key {
			return true
		}
	}
	return false
}

// TestGoldenV8Readable — снапшот формата v8 (до документной секции у SQ8 и
// hnsw) читается целиком.
func TestGoldenV8Readable(t *testing.T) {
	for _, gc := range []struct {
		file string
		cfg  LeveledConfig
	}{
		{"v8_sq.bin", sqCompatConfig()},
		{"v8_hnsw.bin", bm25TestConfig()},
	} {
		t.Run(gc.file, func(t *testing.T) {
			dst := NewLeveledVectorStore(gc.cfg)
			defer dst.Close()
			if err := dst.LoadBinary(bytes.NewReader(golden(t, gc.file, 8))); err != nil {
				t.Fatalf("LoadBinary: %v", err)
			}
			assertCompatDocs(t, dst, nil, 0)
		})
	}
}

// TestGoldenV9DocsSectionReadable — документы, которые прежняя версия вынесла
// в документную секцию, возвращаются в сегмент: вектор вписывается обратно в
// граф, атрибуты и текст пересобираются.
func TestGoldenV9DocsSectionReadable(t *testing.T) {
	for _, gc := range []struct {
		file  string
		cfg   LeveledConfig
		vecOf func(int) []float32
		tol   float64
	}{
		{"v9_docs_frozen.bin", bm25TestConfig(), compatVec, 0},
		// SQ8: в секции лежат деквантованные коды, сравнение — с точностью
		// кванта (диапазон значений 0..7, 256 уровней).
		{"v9_docs_sq.bin", sqCompatConfig(), compatVec, 0.05},
		{"v9_docs_hnsw.bin", bm25TestConfig(), compatHnswVec, 0},
	} {
		t.Run(gc.file, func(t *testing.T) {
			dst := NewLeveledVectorStore(gc.cfg)
			defer dst.Close()
			if err := dst.LoadBinary(bytes.NewReader(golden(t, gc.file, 9))); err != nil {
				t.Fatalf("LoadBinary: %v", err)
			}
			assertCompatDocs(t, dst, gc.vecOf, gc.tol)
		})
	}
}

// TestGoldenV9EncryptedRefused — снапшот с документами под шифрованием эта
// версия прочитать не может и обязана отказаться целиком: загрузка без части
// документов была бы тихой потерей данных.
func TestGoldenV9EncryptedRefused(t *testing.T) {
	dst := NewLeveledVectorStore(bm25TestConfig())
	defer dst.Close()
	err := dst.LoadBinary(bytes.NewReader(golden(t, "v9_docs_sealed.bin", 9)))
	if !errors.Is(err, ErrEncryptedSnapshot) {
		t.Fatalf("LoadBinary(зашифрованный) = %v, ожидалась ErrEncryptedSnapshot", err)
	}
	// Всё-или-ничего: отказ не оставляет полузагруженного состояния.
	if n, _, _ := dst.Info(); n != 0 {
		t.Fatalf("после отказа в сторе %d векторов, ожидалось 0", n)
	}
	if _, ok := dst.Get("plain-1"); ok {
		t.Fatal("после отказа документ из отклонённого снапшота виден")
	}
}

// TestResaveLegacySnapshot — пересохранение снапшота прежней версии даёт тот же
// формат v9 с пустой документной секцией, и данные переживают круг
// «загрузить → сохранить → загрузить».
func TestResaveLegacySnapshot(t *testing.T) {
	src := NewLeveledVectorStore(bm25TestConfig())
	defer src.Close()
	if err := src.LoadBinary(bytes.NewReader(golden(t, "v9_docs_frozen.bin", 9))); err != nil {
		t.Fatalf("LoadBinary: %v", err)
	}
	var buf bytes.Buffer
	if err := src.SaveBinary(&buf); err != nil {
		t.Fatalf("SaveBinary: %v", err)
	}
	if v := buf.Bytes()[5]; v != 9 {
		t.Fatalf("пересохранённый снапшот версии %d, ожидалась 9", v)
	}
	dst := NewLeveledVectorStore(bm25TestConfig())
	defer dst.Close()
	if err := dst.LoadBinary(bytes.NewReader(buf.Bytes())); err != nil {
		t.Fatalf("LoadBinary(пересохранённый): %v", err)
	}
	assertCompatDocs(t, dst, compatVec, 0)
}

// TestScopeAttributeIsOrdinary — атрибут scope больше ничем не особенный:
// документ с ним остаётся в слоях сегмента, а секция пишется пустой. Прежняя
// версия уводила такие документы в отдельную секцию снапшота у любого
// пользователя, назвавшего свой атрибут scope.
func TestScopeAttributeIsOrdinary(t *testing.T) {
	for _, gc := range []struct {
		name  string
		cfg   LeveledConfig
		vecOf func(int) []float32
		tol   float64
	}{
		{"frozen", bm25TestConfig(), compatVec, 0},
		{"sq8", sqCompatConfig(), compatVec, 0.05},
		{"hnsw", bm25TestConfig(), compatHnswVec, 0},
	} {
		t.Run(gc.name, func(t *testing.T) {
			src := NewLeveledVectorStore(gc.cfg)
			defer src.Close()
			for i, d := range compatDocs {
				attrs := Attributes{Cat: map[string]string{}}
				if d.scope != "" {
					attrs.Cat["scope"] = d.scope
				}
				if err := src.AddDocTerms(d.key, gc.vecOf(i), attrs, []TermTF{{Term: d.term, TF: 1}}); err != nil {
					t.Fatalf("AddDocTerms(%s): %v", d.key, err)
				}
			}
			src.FlushDeltaSync()

			var buf bytes.Buffer
			if err := src.SaveBinary(&buf); err != nil {
				t.Fatalf("SaveBinary: %v", err)
			}
			dst := NewLeveledVectorStore(gc.cfg)
			defer dst.Close()
			if err := dst.LoadBinary(bytes.NewReader(buf.Bytes())); err != nil {
				t.Fatalf("LoadBinary: %v", err)
			}
			assertCompatDocs(t, dst, gc.vecOf, gc.tol)
		})
	}
}
