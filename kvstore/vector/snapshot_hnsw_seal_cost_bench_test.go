// ЗАМЕР ЦЕНЫ ПОСЛЕ КОДА — и это долг, а не норма.
//
// Запечатывание hnsw-сегмента (v9) переписало его запись: раньше цикл писал
// документы на лету, теперь живые документы сначала собираются списком, потому
// что скоуп факта не узнать, не прочитав его атрибуты. Атрибуты декодировались
// и до правки, а вот УДЕРЖАНИЕ n структур DeltaEntry разом — новое. hnsw — путь
// больших размерностей, и на нём пик памяти снапшота важен.
//
// ⚠ПОРОГ БЫЛ НАЗНАЧЕН НЕВЕРНО И ЗАМЕНЁН ПОСЛЕ ПЕРВОГО ПРОГОНА. Записываю это
// прямо, потому что смена порога после взгляда на число — ровно тот приём, к
// которому надо относиться с подозрением, и оправдана она может быть только
// ошибкой в ЭТАЛОНЕ, а не неудобством результата.
//
// Первый порог — 100 мс на 10k — был взят из замера ЗАГРУЗКИ (33–76 мс) и
// приложен к ЗАПИСИ. Эталон не тот: запись того же корпуса БЕЗ всякого
// запечатывания стоит 99 мс, то есть порог не оставлял фиче ни миллисекунды и
// мерил не её, а базовую цену снапшота, существовавшую до v9.
//
// Что здесь на самом деле надо измерить — ЦЕНУ САМОГО ЗАПЕЧАТЫВАНИЯ. Поэтому
// эталоном стал тот же корпус без скоупов (не-VMEM-стор, которому изымать
// нечего), а порог — относительный:
//
//	время   ≤ 2× базовой записи
//	память  ≤ 3× базовой записи
//
// Измерено 31.07 на 10k фактов dim=300, 30 термов на факт:
//
//	без скоупов   99 мс / 135 МБ   ← базовая цена снапшота, НЕ от v9
//	со скоупами  139 мс / 309 МБ   ← 1.40× времени, 2.29× памяти
//
// ⭐Базовая цена сама по себе велика (135 МБ на корпус, чьи векторы весят 12 МБ)
// и к запечатыванию отношения не имеет: её дают decodeTerms по 300k термов и
// decodeAt на каждый документ. Это отдельный вопрос, не этой правки.
package vector

import (
	"bytes"
	"fmt"
	"runtime"
	"testing"
	"time"
)

const (
	hnswCostN   = 10_000
	hnswCostDim = 300 // > csrDimThreshold=256 ⇒ hnswSegment без UseSQ

	// Пороги — на ЦЕНУ ЗАПЕЧАТЫВАНИЯ, кратно записи того же корпуса без скоупов.
	hnswCostMaxTimeRatio  = 2.0
	hnswCostMaxAllocRatio = 3.0
)

// hnswCostStore — стор из n фактов на большой размерности. withScope=false даёт
// не-VMEM-корпус: те же документы, но без атрибута scope, то есть запечатывать
// в них нечего.
func hnswCostStore(t *testing.T, n, dim int, withScope bool) *LeveledVectorStore {
	t.Helper()
	lvs := NewLeveledVectorStore(hnswSealedConfig())
	for i := 0; i < n; i++ {
		key, vec, attrs, terms := replayCostFact(i, dim)
		if !withScope {
			delete(attrs.Cat, vmemAttrScope)
		}
		if err := lvs.AddDocTerms(key, vec, attrs, terms); err != nil {
			t.Fatalf("AddDocTerms(%d): %v", i, err)
		}
	}
	lvs.FlushDeltaSync()
	return lvs
}

// measureSave — время и суммарно выделенные байты одного SaveBinary.
func measureSave(t *testing.T, lvs *LeveledVectorStore) (time.Duration, uint64) {
	t.Helper()
	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)

	var buf bytes.Buffer
	start := time.Now()
	if err := lvs.SaveBinary(&buf); err != nil {
		t.Fatalf("SaveBinary: %v", err)
	}
	elapsed := time.Since(start)

	runtime.ReadMemStats(&after)
	return elapsed, after.TotalAlloc - before.TotalAlloc
}

func TestSnapshotHnswSealCost(t *testing.T) {
	if testing.Short() {
		t.Skip("замер цены записи запечатанного hnsw: гоняется без -short")
	}

	run := func(withScope bool) (time.Duration, uint64) {
		lvs := hnswCostStore(t, hnswCostN, hnswCostDim, withScope)
		defer lvs.Clear()
		requireHnswSegment(t, lvs)
		return measureSave(t, lvs)
	}

	// Базовая цена ПЕРВОЙ: без неё число со скоупами не с чем сравнивать.
	baseTime, baseAlloc := run(false)
	sealTime, sealAlloc := run(true)

	t.Logf("без скоупов (базовая цена снапшота): %v / %.1f МБ",
		baseTime.Round(time.Millisecond), float64(baseAlloc)/(1<<20))
	t.Logf("со скоупами (запечатывание работает): %v / %.1f МБ",
		sealTime.Round(time.Millisecond), float64(sealAlloc)/(1<<20))

	if baseTime <= 0 || baseAlloc == 0 {
		t.Fatal("базовый замер пуст — сравнивать не с чем, пороги ниже прошли бы по неверной причине")
	}
	timeRatio := float64(sealTime) / float64(baseTime)
	allocRatio := float64(sealAlloc) / float64(baseAlloc)
	t.Logf("цена запечатывания: %.2f× времени, %.2f× памяти (на %d фактов dim=%d)",
		timeRatio, allocRatio, hnswCostN, hnswCostDim)

	if timeRatio > hnswCostMaxTimeRatio {
		t.Errorf("запечатывание стоит %.2f× времени при пороге %.1f×", timeRatio, hnswCostMaxTimeRatio)
	}
	if allocRatio > hnswCostMaxAllocRatio {
		t.Errorf("запечатывание стоит %.2f× памяти при пороге %.1f×", allocRatio, hnswCostMaxAllocRatio)
	}
	fmt.Println()
}
