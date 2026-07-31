package vector

import (
	"bytes"
	"strings"
	"testing"
)

// =============================================================================
// hnsw-сегмент под конвертом (v9).
//
// ПОЧЕМУ ЭТОТ ПУТЬ ВАЖНЕЕ SQ8. hnswSegment появляется при dim > 256 без
// -hnsw-use-sq, то есть в конфигурации ПО УМОЛЧАНИЮ на реальных эмбеддингах:
// 768 и 1536 больше 256, а флаг SQ выключен. До v9 он писал в снапшот ключ,
// СЫРОЙ float32-вектор прямо из арены, атрибуты и термы — всё открытым
// текстом. SQ8 хотя бы требовал осознанно включить флаг; этот не требовал
// ничего.
//
// Проверки те же, что у float32- и SQ8-пути, плюс одна своя: байты самого
// вектора. У SQ8 в снапшоте лежат коды, и сравнивать с исходным float32
// нечего; здесь вектор писался как есть, поэтому его отсутствие проверяется
// напрямую.
// =============================================================================

const hnswSealedDim = 300 // > csrDimThreshold=256 → hnswSegment без UseSQ

// hnswSealedConfig — конфиг без SQ: заморозка при dim>256 даст hnswSegment
// (leveled_store.go, buildSegmentWithAllocator).
func hnswSealedConfig() LeveledConfig {
	cfg := bm25TestConfig()
	cfg.UseSQ = false
	return cfg
}

// hnswVecOfDoc — вектор i-го документа фикстуры. Значения различны, чтобы
// проверка «байты вектора факта отсутствуют» не могла совпасть случайно с
// вектором соседа.
func hnswVecOfDoc(i int) []float32 {
	return mkVecN(hnswSealedDim, float32(i+1)*1.5)
}

// sealedHnswStore — фикстура sealedSegmentStore на большой размерности.
func sealedHnswStore(t *testing.T, crypto *SnapshotCrypto) *LeveledVectorStore {
	t.Helper()
	lvs := NewLeveledVectorStore(hnswSealedConfig())
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
			attrs.Cat[vmemAttrSealed] = "1"
			// Значение атрибута — тоже содержание. Строка не встречается в
			// ключе намеренно: ключ лежит открыто ПО ЗАМЫСЛУ, и проверка,
			// поймавшая его, ничего бы не доказала.
			attrs.Cat[vmemAttrSource] = "chan-zephyr-" + d.scope
		} else {
			attrs.Cat["lang"] = "lang-quokka" // парный контроль
		}
		if err := lvs.AddDocTerms(d.key, hnswVecOfDoc(i), attrs, []TermTF{{Term: d.term, TF: 1}}); err != nil {
			t.Fatalf("AddDocTerms(%s): %v", d.key, err)
		}
	}
	lvs.FlushDeltaSync()
	return lvs
}

// requireHnswSegment — КОНТРОЛЬ ДИАГНОЗА: без него тесты позеленели бы, если бы
// заморозка ушла в другой тип, и мы прочли бы это как «дыры нет».
func requireHnswSegment(t *testing.T, lvs *LeveledVectorStore) {
	t.Helper()
	lvs.mu.RLock()
	defer lvs.mu.RUnlock()
	for _, level := range lvs.levels {
		for _, seg := range level {
			if _, ok := seg.(*hnswSegment); ok {
				return
			}
		}
	}
	t.Fatal("hnswSegment не создан — любая проверка ниже прошла бы по неверной причине")
}

// TestSealedHnswSegmentHidesContent — содержания фактов в снапшоте быть не
// должно: ни термов, ни байтов вектора.
func TestSealedHnswSegmentHidesContent(t *testing.T) {
	fc := newFakeCrypto()
	src := sealedHnswStore(t, fc.crypto())
	defer src.Clear()
	requireHnswSegment(t, src)

	var buf bytes.Buffer
	if err := src.SaveBinary(&buf); err != nil {
		t.Fatalf("SaveBinary: %v", err)
	}
	raw := buf.String()

	// ПАРНЫЙ ПОЛОЖИТЕЛЬНЫЙ КОНТРОЛЬ — первым: посторонний документ шифровать
	// нечем, он обязан лежать открыто. Иначе отрицательные проверки ничего не
	// значат: пустой буфер прошёл бы их все.
	if !strings.Contains(raw, "weather") {
		t.Fatal("терм постороннего документа не найден — снапшот пуст, проверки ниже бессмысленны")
	}
	if !bytes.Contains(buf.Bytes(), vecBytes(hnswVecOfDoc(3))) {
		t.Fatal("вектор постороннего документа не найден — проверка байтов ниже прошла бы по неверной причине")
	}
	if !strings.Contains(raw, "lang-quokka") {
		t.Fatal("атрибут постороннего документа не найден — проверка атрибутов ниже прошла бы по неверной причине")
	}

	for _, secret := range []string{"aurora", "steering", "standup"} {
		if strings.Contains(raw, secret) {
			t.Errorf("терм %q факта лежит в hnsw-снапшоте открытым текстом", secret)
		}
	}
	// Атрибуты — отдельная ось от термов и вектора: изъять одно и оставить
	// другое легко и незаметно.
	for _, secret := range []string{"chan-zephyr-alice", "chan-zephyr-bob"} {
		if strings.Contains(raw, secret) {
			t.Errorf("атрибут %q факта лежит в hnsw-снапшоте открытым текстом", secret)
		}
	}
	for i, key := range []string{"f-alice-1", "f-alice-2", "f-bob-1"} {
		if bytes.Contains(buf.Bytes(), vecBytes(hnswVecOfDoc(i))) {
			t.Errorf("вектор факта %s уехал в снапшот открытым — маскирование не сработало", key)
		}
	}
}

// TestSealedHnswSegmentRoundTrip — «зашифровали» не должно означать «потеряли»:
// факты возвращаются целиком, включая значения векторов и термы.
func TestSealedHnswSegmentRoundTrip(t *testing.T) {
	fc := newFakeCrypto()
	src := sealedHnswStore(t, fc.crypto())
	defer src.Clear()
	requireHnswSegment(t, src)

	var buf bytes.Buffer
	if err := src.SaveBinary(&buf); err != nil {
		t.Fatalf("SaveBinary: %v", err)
	}

	dst := NewLeveledVectorStore(hnswSealedConfig())
	dst.SetSnapshotCrypto(fc.crypto())
	defer dst.Clear()
	if err := dst.LoadBinary(bytes.NewReader(buf.Bytes())); err != nil {
		t.Fatalf("LoadBinary: %v", err)
	}

	for i, key := range []string{"f-alice-1", "f-alice-2", "f-bob-1", "plain-1"} {
		got, ok := dst.Get(key)
		if !ok {
			t.Errorf("документ %s не восстановился из снапшота", key)
			continue
		}
		// Значения, а не только наличие ключа: занулённый на диске вектор
		// обязан вернуться из документной секции ТЕМ ЖЕ. Иначе «восстановился»
		// было бы верно и для пустышки из нулей.
		want := hnswVecOfDoc(i)
		for j := range want {
			if got[j] != want[j] {
				t.Errorf("вектор %s[%d] = %v, ожидалось %v", key, j, got[j], want[j])
				break
			}
		}
	}
	assertScope(t, dst, "f-alice-1", "alice")
	assertTerm(t, dst, "f-alice-1", "aurora")
	assertTerm(t, dst, "f-bob-1", "standup")
}

// TestSealedHnswSegmentShredDoesNotResurrect — то, ради чего всё делается.
func TestSealedHnswSegmentShredDoesNotResurrect(t *testing.T) {
	fc := newFakeCrypto()
	src := sealedHnswStore(t, fc.crypto())
	defer src.Clear()
	requireHnswSegment(t, src)

	var buf bytes.Buffer
	if err := src.SaveBinary(&buf); err != nil {
		t.Fatalf("SaveBinary: %v", err)
	}

	fc.destroyed["alice"] = true // VMEM.SHRED уничтожил ключ скоупа

	dst := NewLeveledVectorStore(hnswSealedConfig())
	dst.SetSnapshotCrypto(fc.crypto())
	defer dst.Clear()
	if err := dst.LoadBinary(bytes.NewReader(buf.Bytes())); err != nil {
		t.Fatalf("LoadBinary: %v", err)
	}

	for _, key := range []string{"f-alice-1", "f-alice-2"} {
		if _, ok := dst.Get(key); ok {
			t.Errorf("факт стёртого скоупа %s воскрес из hnsw-снапшота", key)
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

// TestKeyCoverageHnswPathStaysFull — покрытие по умолчанию для реальных
// размерностей. До v9 здесь было 1.0000 при открытых байтах.
func TestKeyCoverageHnswPathStaysFull(t *testing.T) {
	fc := newFakeCrypto()
	lvs := sealedHnswStore(t, fc.crypto())
	defer lvs.Clear()
	requireHnswSegment(t, lvs)

	reps := lvs.KeyCoverage("alice")
	if len(reps) != 1 {
		t.Fatalf("KeyCoverage вернул %d отчётов, ожидался 1", len(reps))
	}
	r := reps[0]
	if r.Total != 2 || r.Sealed != 2 || r.Exposed != 0 || r.Unsealed != 0 {
		t.Errorf("hnsw-путь: total=%d sealed=%d exposed=%d unsealed=%d, ожидалось 2/2/0/0",
			r.Total, r.Sealed, r.Exposed, r.Unsealed)
	}
}
