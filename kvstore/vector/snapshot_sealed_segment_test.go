package vector

import (
	"bytes"
	"encoding/binary"
	"strings"
	"testing"
)

// sealedSegmentStore — стор с фактами двух скоупов и посторонним вектором,
// сфлашенный в frozen-сегмент (dim ≤ csrDimThreshold).
func sealedSegmentStore(t *testing.T, crypto *SnapshotCrypto) *LeveledVectorStore {
	t.Helper()
	lvs := NewLeveledVectorStore(bm25TestConfig())
	lvs.SetSnapshotCrypto(crypto)

	docs := []struct {
		key   string
		scope string
		term  string
	}{
		{"f-alice-1", "alice", "aurora"},
		{"f-alice-2", "alice", "steering"},
		{"f-bob-1", "bob", "standup"},
		{"plain-1", "", "weather"},
	}
	for i, d := range docs {
		attrs := Attributes{Cat: map[string]string{}}
		if d.scope != "" {
			attrs.Cat[vmemAttrScope] = d.scope
		} else {
			attrs.Cat["lang"] = "en"
		}
		vec := vecOfDoc(i)
		if err := lvs.AddDocTerms(d.key, vec, attrs, []TermTF{{Term: d.term, TF: 1}}); err != nil {
			t.Fatalf("AddDocTerms(%s): %v", d.key, err)
		}
	}
	lvs.FlushDeltaSync()
	return lvs
}

// TestSealedSegmentRoundTrip — снапшот с фактами читается обратно ПОЛНОСТЬЮ:
// векторы, атрибуты и термы. Иначе «зашифровали» означало бы «потеряли».
func TestSealedSegmentRoundTrip(t *testing.T) {
	fc := newFakeCrypto()
	src := sealedSegmentStore(t, fc.crypto())
	defer src.Clear()

	var buf bytes.Buffer
	if err := src.SaveBinary(&buf); err != nil {
		t.Fatalf("SaveBinary: %v", err)
	}

	// Отрицательная проверка: содержания фактов в снапшоте нет.
	raw := buf.String()
	for _, secret := range []string{"aurora", "steering", "standup"} {
		if strings.Contains(raw, secret) {
			t.Errorf("терм %q факта найден в снапшоте открытым текстом", secret)
		}
	}
	// ПАРНЫЙ ПОЛОЖИТЕЛЬНЫЙ КОНТРОЛЬ: посторонний документ шифровать нечем, и
	// он лежит открыто — значит проверка выше искала в правильном файле.
	if !strings.Contains(raw, "weather") {
		t.Error("терм постороннего документа не найден — снапшот пуст, проверка прошла бы по неверной причине")
	}
	// Вектор — тоже содержание: эмбеддинг раскрывает факт не хуже термов.
	// Байты вектора факта в снапшоте присутствовать не должны, а вектор
	// постороннего документа — должен (тот же парный контроль).
	if bytes.Contains(buf.Bytes(), vecBytes(vecOfDoc(0))) {
		t.Error("вектор факта уехал в снапшот открытым — маскирование слэба не сработало")
	}
	if !bytes.Contains(buf.Bytes(), vecBytes(vecOfDoc(3))) {
		t.Error("вектор постороннего документа не найден — проверка выше прошла бы по неверной причине")
	}

	dst := NewLeveledVectorStore(bm25TestConfig())
	dst.SetSnapshotCrypto(fc.crypto())
	defer dst.Clear()
	if err := dst.LoadBinary(bytes.NewReader(buf.Bytes())); err != nil {
		t.Fatalf("LoadBinary: %v", err)
	}

	for _, key := range []string{"f-alice-1", "f-alice-2", "f-bob-1", "plain-1"} {
		if _, ok := dst.Get(key); !ok {
			t.Errorf("документ %s не восстановился из снапшота", key)
		}
	}
	// Восстановиться должны не только ключи: без атрибутов и термов факт
	// перестаёт быть находимым, то есть шифрование обернулось бы потерей.
	assertScope(t, dst, "f-alice-1", "alice")
	assertScope(t, dst, "f-bob-1", "bob")
	assertTerm(t, dst, "f-alice-1", "aurora")
	assertTerm(t, dst, "f-bob-1", "standup")

	// Значения векторов, а не только наличие ключа: занулённый в снапшоте
	// вектор обязан вернуться из документной секции ТЕМ ЖЕ. Без этой проверки
	// «документ восстановился» было бы верно и для пустышки из нулей.
	for i, key := range []string{"f-alice-1", "f-alice-2", "f-bob-1", "plain-1"} {
		got, ok := dst.Get(key)
		if !ok {
			continue // уже отмечено выше
		}
		want := vecOfDoc(i)
		for j := range want {
			if got[j] != want[j] {
				t.Errorf("вектор %s[%d] = %v, ожидалось %v", key, j, got[j], want[j])
				break
			}
		}
	}
}

// vecOfDoc — вектор i-го документа фикстуры (та же формула, что при записи).
func vecOfDoc(i int) []float32 {
	return []float32{float32(i + 1), float32(i + 2), float32(i + 3), float32(i + 4),
		float32(i), float32(i), float32(i), float32(i)}
}

func vecBytes(v []float32) []byte {
	var b bytes.Buffer
	_ = binary.Write(&b, binary.LittleEndian, v)
	return b.Bytes()
}

// TestSealedSegmentShredDoesNotResurrect — то, ради чего всё делается.
func TestSealedSegmentShredDoesNotResurrect(t *testing.T) {
	fc := newFakeCrypto()
	src := sealedSegmentStore(t, fc.crypto())
	defer src.Clear()

	var buf bytes.Buffer
	if err := src.SaveBinary(&buf); err != nil {
		t.Fatalf("SaveBinary: %v", err)
	}

	fc.destroyed["alice"] = true // VMEM.SHRED уничтожил ключ скоупа

	dst := NewLeveledVectorStore(bm25TestConfig())
	dst.SetSnapshotCrypto(fc.crypto())
	defer dst.Clear()
	if err := dst.LoadBinary(bytes.NewReader(buf.Bytes())); err != nil {
		t.Fatalf("LoadBinary: %v", err)
	}

	for _, key := range []string{"f-alice-1", "f-alice-2"} {
		if _, ok := dst.Get(key); ok {
			t.Errorf("факт стёртого скоупа %s воскрес из снапшота", key)
		}
	}
	// Выборочность: соседний скоуп и посторонний документ целы, иначе это не
	// стирание, а потеря данных.
	for _, key := range []string{"f-bob-1", "plain-1"} {
		if _, ok := dst.Get(key); !ok {
			t.Errorf("документ %s не пережил стирание чужого скоупа", key)
		}
	}
	assertTerm(t, dst, "f-bob-1", "standup")
}

// TestSealedSegmentWithoutCrypto — без шифрования формат обязан работать как
// раньше: снапшот пишется и читается, ничего не теряется.
func TestSealedSegmentWithoutCrypto(t *testing.T) {
	src := sealedSegmentStore(t, nil)
	defer src.Clear()

	var buf bytes.Buffer
	if err := src.SaveBinary(&buf); err != nil {
		t.Fatalf("SaveBinary: %v", err)
	}
	dst := NewLeveledVectorStore(bm25TestConfig())
	defer dst.Clear()
	if err := dst.LoadBinary(bytes.NewReader(buf.Bytes())); err != nil {
		t.Fatalf("LoadBinary: %v", err)
	}
	for _, key := range []string{"f-alice-1", "f-bob-1", "plain-1"} {
		if _, ok := dst.Get(key); !ok {
			t.Errorf("документ %s не восстановился без шифрования", key)
		}
	}
	assertTerm(t, dst, "f-alice-1", "aurora")
}

func assertScope(t *testing.T, lvs *LeveledVectorStore, key, want string) {
	t.Helper()
	got := lvs.catForKeys([]string{key}, vmemAttrScope)
	if len(got) != 1 || got[0] != want {
		t.Errorf("scope документа %s = %q, ожидался %q", key, got, want)
	}
}

func assertTerm(t *testing.T, lvs *LeveledVectorStore, key, term string) {
	t.Helper()
	res, err := lvs.SearchText(term, 10)
	if err != nil {
		t.Fatalf("SearchText(%s): %v", term, err)
	}
	for _, r := range res {
		if r.Key == key {
			return
		}
	}
	t.Errorf("документ %s не находится по своему терму %q — текстовый слой не восстановлен", key, term)
}
