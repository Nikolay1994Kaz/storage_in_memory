package vector

import (
	"bytes"
	"strings"
	"testing"
)

// =============================================================================
// SQ8-сегмент мимо конверта: дыра в запечатанном снапшоте v8.
//
// ЧТО ПРОВЕРЯЕТСЯ. frozenSegment (float32) пишется через WriteGraphToMasked +
// writeSealedDocs: векторы скоупа занулены в открытом слэбе, атрибуты и термы
// пересобраны из ПУБЛИЧНОГО остатка, содержание уехало под конверт скоупа.
// У frozenSQSegment в соседнем case того же switch (leveled_store.go:2296)
// нет ни маски, ни writeSealedDocs — s.attrs и s.text пишутся целиком, а
// WriteGraphToSQ маску не принимает вовсе (frozen_sq.go:617, маскирующего
// варианта не существует). Значит факт, записанный под шифрованием, но
// замёрзший в SQ8, лежит на диске открытым, и VMEM.SHRED до него не достаёт.
//
// ПОЧЕМУ ЭТО ДОСТИЖИМО. Флаг -hnsw-use-sq выключен по умолчанию, но
// документирован как «рекомендуется для dim>256», а реальные эмбеддинги —
// 768 и 1536. Запрета на связку с -encrypt-at-rest нет: флаги выставляются
// независимо (main.go:385 и :413). То есть открытый текст получает тот, кто
// последовал нашей же рекомендации, — и получает вместе с квитанцией SHRED,
// говорящей об успехе.
//
// Ключи и позиции проверять нельзя: они лежат вне конверта СОЗНАТЕЛЬНО (см.
// шапку snapshot_sealed_docs.go — иначе мёртвый документ нечем пометить).
// Поэтому уликой служат термы: это буквальные слова факта.
// =============================================================================

// sqSealedConfig — конфиг тестовой фикстуры плюс SQ8. Metric не задаём:
// MetricAuto выводится из Distance (distance.go:87).
func sqSealedConfig() LeveledConfig {
	cfg := bm25TestConfig()
	cfg.UseSQ = true // = флаг -hnsw-use-sq
	return cfg
}

// sealedSQStore — фикстура sealedSegmentStore на конфиге с UseSQ.
//
// У фактов есть и атрибуты, и термы → canFreeze=false (leveled_store.go:2967
// требует !HasAttrs() && !HasText()) → путь rebuild → buildSegmentWithAllocator
// собирает frozenSQSegment{fg, cat, attrs, text}, то есть сегмент С
// содержанием. Это и есть боевой путь VMEM, а не краевой.
func sealedSQStore(t *testing.T, crypto *SnapshotCrypto) *LeveledVectorStore {
	t.Helper()
	return sealedStoreWithCfg(t, sqSealedConfig(), crypto)
}

// sealedStoreWithCfg — та же фикстура на произвольном конфиге. Вынесена, чтобы
// float32- и SQ8-варианты отличались РОВНО одним флагом: иначе сравнение путей
// доказывало бы разницу фикстур, а не разницу форматов.
func sealedStoreWithCfg(t *testing.T, cfg LeveledConfig, crypto *SnapshotCrypto) *LeveledVectorStore {
	t.Helper()
	lvs := NewLeveledVectorStore(cfg)
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
			attrs.Cat[vmemAttrSealed] = "1" // как ставит путь записи, vmem.go:220
		} else {
			attrs.Cat["lang"] = "en"
		}
		if err := lvs.AddDocTerms(d.key, vecOfDoc(i), attrs, []TermTF{{Term: d.term, TF: 1}}); err != nil {
			t.Fatalf("AddDocTerms(%s): %v", d.key, err)
		}
	}
	lvs.FlushDeltaSync()
	return lvs
}

// requireSQSegment — КОНТРОЛЬ ДИАГНОЗА, а не факта ошибки. Без него тесты ниже
// могли бы позеленеть просто оттого, что SQ8-сегмент не родился (freeze ушёл в
// другую ветку, порог не тот, конфиг не долетел), и мы бы прочли это как
// «дыры нет». Проверяем, что предмет разговора вообще существует.
func requireSQSegment(t *testing.T, lvs *LeveledVectorStore) {
	t.Helper()
	lvs.mu.RLock()
	defer lvs.mu.RUnlock()
	for _, level := range lvs.levels {
		for _, seg := range level {
			if _, ok := seg.(*frozenSQSegment); ok {
				return
			}
		}
	}
	t.Fatal("frozenSQSegment не создан — любая проверка ниже прошла бы по неверной причине")
}

// TestSealedSQSegmentHidesContent — содержания фактов в SQ8-снапшоте быть не
// должно, ровно как в TestSealedSegmentRoundTrip для float32.
func TestSealedSQSegmentHidesContent(t *testing.T) {
	fc := newFakeCrypto()
	src := sealedSQStore(t, fc.crypto())
	defer src.Clear()
	requireSQSegment(t, src)

	var buf bytes.Buffer
	if err := src.SaveBinary(&buf); err != nil {
		t.Fatalf("SaveBinary: %v", err)
	}
	raw := buf.String()

	// ПАРНЫЙ ПОЛОЖИТЕЛЬНЫЙ КОНТРОЛЬ — первым: посторонний документ шифровать
	// нечем, он обязан лежать открыто. Если его нет, снапшот пуст или устроен
	// иначе, и отрицательные проверки ниже ничего не значат.
	if !strings.Contains(raw, "weather") {
		t.Fatal("терм постороннего документа не найден — снапшот пуст, проверки ниже бессмысленны")
	}
	for _, secret := range []string{"aurora", "steering", "standup"} {
		if strings.Contains(raw, secret) {
			t.Errorf("терм %q факта лежит в SQ8-снапшоте открытым текстом", secret)
		}
	}
}

// TestSealedSQSegmentRoundTripMatchesPlain — самое рискованное место правки.
//
// У SQ8 вектор не хранится, а вычисляется из кода. Запечатывание проводит его
// через деквантование → конверт → переквантование, и если формула
// восстановления разойдётся с формулой заморозки хоть на округление, факт
// вернётся СДВИНУТЫМ. Тихо: поиск деградирует только для запечатанных скоупов,
// на глаз это не видно.
//
// Поэтому сравнение не с исходным вектором (он и так теряется при квантовании),
// а с тем же стором БЕЗ шифрования: шифрование не должно менять ничего, кроме
// читаемости байтов на диске. Эталон — поведение, а не константа.
func TestSealedSQSegmentRoundTripMatchesPlain(t *testing.T) {
	load := func(crypto *SnapshotCrypto) *LeveledVectorStore {
		t.Helper()
		src := sealedStoreWithCfg(t, sqSealedConfig(), crypto)
		defer src.Clear()
		requireSQSegment(t, src)
		var buf bytes.Buffer
		if err := src.SaveBinary(&buf); err != nil {
			t.Fatalf("SaveBinary: %v", err)
		}
		dst := NewLeveledVectorStore(sqSealedConfig())
		dst.SetSnapshotCrypto(crypto)
		if err := dst.LoadBinary(bytes.NewReader(buf.Bytes())); err != nil {
			t.Fatalf("LoadBinary: %v", err)
		}
		return dst
	}

	fc := newFakeCrypto()
	sealed := load(fc.crypto())
	defer sealed.Clear()
	plain := load(nil)
	defer plain.Clear()

	for _, key := range []string{"f-alice-1", "f-alice-2", "f-bob-1", "plain-1"} {
		want, okPlain := plain.Get(key)
		got, okSealed := sealed.Get(key)
		if !okPlain || !okSealed {
			t.Errorf("документ %s: без шифрования ok=%v, с шифрованием ok=%v", key, okPlain, okSealed)
			continue
		}
		if len(got) != len(want) {
			t.Errorf("документ %s: длина вектора %d, ожидалась %d", key, len(got), len(want))
			continue
		}
		for j := range want {
			if got[j] != want[j] {
				t.Errorf("вектор %s[%d] = %v, без шифрования %v — переквантование разошлось с заморозкой",
					key, j, got[j], want[j])
				break
			}
		}
	}
	// Термы и скоуп тоже обязаны пережить дорогу: без них факт перестаёт быть
	// находимым, то есть шифрование обернулось бы потерей.
	assertScope(t, sealed, "f-alice-1", "alice")
	assertTerm(t, sealed, "f-alice-1", "aurora")
	assertTerm(t, sealed, "f-bob-1", "standup")
}

// TestSealedSQSegmentShredDoesNotResurrect — следствие, ради которого всё:
// после уничтожения ключа скоупа его факты не должны подниматься из снапшота.
// Парный к TestSealedSegmentShredDoesNotResurrect, который для float32 зелёный.
func TestSealedSQSegmentShredDoesNotResurrect(t *testing.T) {
	fc := newFakeCrypto()
	src := sealedSQStore(t, fc.crypto())
	defer src.Clear()
	requireSQSegment(t, src)

	var buf bytes.Buffer
	if err := src.SaveBinary(&buf); err != nil {
		t.Fatalf("SaveBinary: %v", err)
	}

	fc.destroyed["alice"] = true // VMEM.SHRED уничтожил ключ скоупа

	dst := NewLeveledVectorStore(sqSealedConfig())
	dst.SetSnapshotCrypto(fc.crypto())
	defer dst.Clear()
	if err := dst.LoadBinary(bytes.NewReader(buf.Bytes())); err != nil {
		t.Fatalf("LoadBinary: %v", err)
	}

	for _, key := range []string{"f-alice-1", "f-alice-2"} {
		if _, ok := dst.Get(key); ok {
			t.Errorf("факт стёртого скоупа %s воскрес из SQ8-снапшота", key)
		}
	}
	// Выборочность: соседний скоуп и посторонний документ целы, иначе это была
	// бы не проверка стирания, а проверка потери данных.
	for _, key := range []string{"f-bob-1", "plain-1"} {
		if _, ok := dst.Get(key); !ok {
			t.Errorf("документ %s не пережил стирание чужого скоупа", key)
		}
	}
}

// TestKeyCoverageNotBlindToSQLeak — метрика, на которой стоит квитанция.
//
// KeyCoverage считает атрибут sealed, проставленный в МОМЕНТ ЗАПИСИ
// (vmem_key_coverage.go:68), и не смотрит, в сегмент какого типа факт потом
// уехал. Поэтому SQ8-путь даёт sealed=1.0000 при открытых байтах на диске —
// то самое «соврал в НАШУ пользу», от которого шапка того файла отгораживалась
// выбором атрибута вместо кейринга. Отгородилась на слой выше, чем нужно.
//
// Проверка условная: она замолкает сама, когда течь заделают, — и остаётся
// стражем на случай, если запечатанный путь снова разойдётся с метрикой.
func TestKeyCoverageNotBlindToSQLeak(t *testing.T) {
	fc := newFakeCrypto()
	src := sealedSQStore(t, fc.crypto())
	defer src.Clear()
	requireSQSegment(t, src)

	var buf bytes.Buffer
	if err := src.SaveBinary(&buf); err != nil {
		t.Fatalf("SaveBinary: %v", err)
	}
	leaked := strings.Contains(buf.String(), "aurora")

	reps := src.KeyCoverage("alice")
	if len(reps) != 1 {
		t.Fatalf("KeyCoverage вернул %d отчётов, ожидался 1 — скоуп не посчитан", len(reps))
	}
	share := reps[0].SealedShare()

	if leaked && share == 1.0 {
		t.Errorf("COVERAGE рапортует sealed=%.4f (%d из %d), а терм факта лежит в снапшоте открытым — квитанция SHRED подтверждает то, чего не произошло",
			share, reps[0].Sealed, reps[0].Total)
	}
}
