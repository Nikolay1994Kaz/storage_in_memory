package vector

import (
	"testing"
)

// =============================================================================
// Покрытие ключом: вторая ось — умеет ли МЕСТО хранения писать факт под конверт.
//
// Первая ось (атрибут sealed) отвечает на «собирались ли мы запечатать этот
// факт». Между этим намерением и байтами на диске лежит конвейер заморозки и
// merge, и он может изменить ответ. Тесты ниже фиксируют, что отчёт считает
// пересечение обеих осей, а не одну первую.
//
// Пары строятся на sealedStoreWithCfg: фикстура, факты, крипто и термы у
// float32- и SQ8-варианта одинаковы до байта, отличается ровно cfg.UseSQ.
// Поэтому разница в отчёте доказывает разницу форматов, а не разницу тестов.
// =============================================================================

// TestKeyCoverageSealedPathStaysFull — ПАРНЫЙ ПОЛОЖИТЕЛЬНЫЙ КОНТРОЛЬ к
// ужесточению. Fail-closed обязан оставить исправный путь полностью покрытым:
// метрика, которая на всём показывает «не покрыт», честна и бесполезна.
func TestKeyCoverageSealedPathStaysFull(t *testing.T) {
	fc := newFakeCrypto()
	lvs := sealedStoreWithCfg(t, bm25TestConfig(), fc.crypto())
	defer lvs.Clear()

	reps := lvs.KeyCoverage("alice")
	if len(reps) != 1 {
		t.Fatalf("KeyCoverage вернул %d отчётов, ожидался 1", len(reps))
	}
	r := reps[0]
	if r.Total != 2 || r.Sealed != 2 || r.Exposed != 0 || r.Unsealed != 0 {
		t.Errorf("float32-путь: total=%d sealed=%d exposed=%d unsealed=%d, ожидалось 2/2/0/0",
			r.Total, r.Sealed, r.Exposed, r.Unsealed)
	}
	if got := r.SealedShare(); got != 1.0 {
		t.Errorf("SealedShare=%.4f на исправном пути, ожидалась 1.0000 — ужесточение задело здоровое", got)
	}
}

// unknownSegment — сегмент типа, о котором метрика не знает. Встроенный
// интерфейс делает заглушку валидным segment, не заставляя писать девять
// методов: segmentSealsScopes только различает тип и ничего не вызывает.
type unknownSegment struct{ segment }

// TestSegmentSealsScopesFailsClosed — главный страж белого списка.
//
// Все три известных типа сегмента запечатывать умеют — но каждый научился
// ОТДЕЛЬНОЙ правкой: frozen в v8, SQ8 и hnsw только в v9, и между этими
// версиями отчёт по двум последним показывал 1.0000. Четвёртый тип, добавленный
// так же тихо (новый case в switch чужие case не ломает), обязан провалиться в
// default и опустить долю, а не унаследовать доверие соседей.
func TestSegmentSealsScopesFailsClosed(t *testing.T) {
	for _, seg := range []segment{&frozenSegment{}, &frozenSQSegment{}, &hnswSegment{}} {
		if !segmentSealsScopes(seg) {
			t.Errorf("%T: известный тип обязан уметь запечатывать", seg)
		}
	}
	if segmentSealsScopes(unknownSegment{}) {
		t.Error("незнакомый тип сегмента получил доверие по умолчанию — это fail-open, ровно то, как проехали SQ8 и hnsw")
	}
}

// TestKeyCoverageSQPathStaysFull — SQ8 после v9 покрыт так же, как float32.
// Тот же набор фактов, отличается ровно cfg.UseSQ.
func TestKeyCoverageSQPathStaysFull(t *testing.T) {
	fc := newFakeCrypto()
	lvs := sealedSQStore(t, fc.crypto())
	defer lvs.Clear()
	requireSQSegment(t, lvs)

	reps := lvs.KeyCoverage("alice")
	if len(reps) != 1 {
		t.Fatalf("KeyCoverage вернул %d отчётов, ожидался 1", len(reps))
	}
	r := reps[0]
	if r.Total != 2 || r.Sealed != 2 || r.Exposed != 0 || r.Unsealed != 0 {
		t.Errorf("SQ8-путь: total=%d sealed=%d exposed=%d unsealed=%d, ожидалось 2/2/0/0",
			r.Total, r.Sealed, r.Exposed, r.Unsealed)
	}
}

// TestKeyCoverageKeepsUnsealedApartFromExposed — две болезни не должны
// схлопнуться в одну: у них разное лечение. Unsealed чинится VMEM.RESEAL,
// Exposed командой не чинится вовсе. Отчёт, сложивший их, скрыл бы, что часть
// фактов не спасти перезаписью.
func TestKeyCoverageKeepsUnsealedApartFromExposed(t *testing.T) {
	fc := newFakeCrypto()
	lvs := NewLeveledVectorStore(bm25TestConfig())
	lvs.SetSnapshotCrypto(fc.crypto())
	defer lvs.Clear()

	// Факт «до шифрования»: скоуп есть, атрибута sealed нет.
	legacy := Attributes{Cat: map[string]string{vmemAttrScope: "alice"}}
	if err := lvs.AddDocTerms("f-legacy", vecOfDoc(0), legacy, []TermTF{{Term: "old", TF: 1}}); err != nil {
		t.Fatalf("AddDocTerms(legacy): %v", err)
	}
	// Факт «под шифрованием».
	fresh := Attributes{Cat: map[string]string{vmemAttrScope: "alice", vmemAttrSealed: "1"}}
	if err := lvs.AddDocTerms("f-fresh", vecOfDoc(1), fresh, []TermTF{{Term: "new", TF: 1}}); err != nil {
		t.Fatalf("AddDocTerms(fresh): %v", err)
	}
	lvs.FlushDeltaSync()

	reps := lvs.KeyCoverage("alice")
	if len(reps) != 1 {
		t.Fatalf("KeyCoverage вернул %d отчётов, ожидался 1", len(reps))
	}
	r := reps[0]
	if r.Total != 2 || r.Sealed != 1 || r.Unsealed != 1 || r.Exposed != 0 {
		t.Errorf("total=%d sealed=%d exposed=%d unsealed=%d, ожидалось 2/1/0/1",
			r.Total, r.Sealed, r.Exposed, r.Unsealed)
	}
}

// TestKeyCoverageDeltaCountsSealed — факт, ещё не осевший в сегмент, считается
// покрытым, и это осознанное решение, а не недосмотр: в снапшот дельта не
// пишется вовсе, её единственная персистентная форма — журнал, запечатанный на
// границе записи. Риск для такого факта не текущий, а будущий — он появится на
// заморозке, и тогда же его покажет этот отчёт.
func TestKeyCoverageDeltaCountsSealed(t *testing.T) {
	fc := newFakeCrypto()
	lvs := NewLeveledVectorStore(sqSealedConfig())
	lvs.SetSnapshotCrypto(fc.crypto())
	defer lvs.Clear()

	attrs := Attributes{Cat: map[string]string{vmemAttrScope: "alice", vmemAttrSealed: "1"}}
	if err := lvs.AddDocTerms("f-hot", vecOfDoc(0), attrs, []TermTF{{Term: "hot", TF: 1}}); err != nil {
		t.Fatalf("AddDocTerms: %v", err)
	}
	// БЕЗ FlushDeltaSync — факт остаётся в дельте.

	reps := lvs.KeyCoverage("alice")
	if len(reps) != 1 {
		t.Fatalf("KeyCoverage вернул %d отчётов, ожидался 1", len(reps))
	}
	if r := reps[0]; r.Sealed != 1 || r.Exposed != 0 {
		t.Errorf("факт в дельте: sealed=%d exposed=%d, ожидалось 1/0", r.Sealed, r.Exposed)
	}

	// И после заморозки в SQ8 покрытие обязано СОХРАНИТЬСЯ. До v9 здесь факт
	// переезжал в Exposed — снапшот принимал его открытым; тест остаётся на
	// месте как страж, что заморозка снова не начнёт терять конверт по дороге.
	lvs.FlushDeltaSync()
	requireSQSegment(t, lvs)
	reps = lvs.KeyCoverage("alice")
	if len(reps) != 1 {
		t.Fatalf("KeyCoverage после заморозки вернул %d отчётов, ожидался 1", len(reps))
	}
	if r := reps[0]; r.Sealed != 1 || r.Exposed != 0 {
		t.Errorf("после заморозки в SQ8: sealed=%d exposed=%d, ожидалось 1/0", r.Sealed, r.Exposed)
	}
}
