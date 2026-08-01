// ЗАМЕР ЦЕНЫ ЗАПИСИ СНАПШОТА — и история о том, как он дважды соврал.
//
// Запечатывание hnsw-сегмента (v9) переписало его запись: раньше цикл писал
// документы на лету, теперь живые документы сначала собираются списком, потому
// что скоуп факта не узнать, не прочитав его атрибуты. hnsw — путь больших
// размерностей, и на нём пик памяти снапшота важен.
//
// ─── ЛОЖЬ ПЕРВАЯ (31.07): не тот эталон ──────────────────────────────────────
// ⚠ПОРОГ БЫЛ НАЗНАЧЕН НЕВЕРНО И ЗАМЕНЁН ПОСЛЕ ПЕРВОГО ПРОГОНА. Записано прямо,
// потому что смена порога после взгляда на число — ровно тот приём, к которому
// надо относиться с подозрением, и оправдана он может быть только ошибкой в
// ЭТАЛОНЕ, а не неудобством результата.
//
// Первый порог — 100 мс на 10k — был взят из замера ЗАГРУЗКИ и приложен к
// ЗАПИСИ. Эталон не тот. Заменён на относительный: тот же корпус без скоупов.
//
// ─── ЛОЖЬ ВТОРАЯ (01.08): не тот прибор ──────────────────────────────────────
// 🚨Относительный эталон правильный, а ПРИБОР был грязный. Замер писал в
// bytes.Buffer и не глушил фон, и оба этих числа попадали в результат:
//
//	приёмник bytes.Buffer удваивается до размера снапшота     → +48 МБ
//	  (прод пишет в bufio 256 КБ в файл и этого не платит вообще, main.go)
//	runtime.MemStats.TotalAlloc ПРОЦЕССНЫЙ, ловит фоновый merge → +48 МБ
//	  (maybeScheduleMergesLocked запускает его в go func)
//
// ⭐И вот почему это опаснее, чем кажется: примесь добавляется ОБЕИМ рукам
// примерно поровну, а значит ТЯНЕТ ОТНОШЕНИЕ К ЕДИНИЦЕ и МАСКИРУЕТ цену фичи.
// Интуиция «одинаковый шум в числителе и знаменателе сократится» здесь ровно
// неверна: сокращается множитель, а постоянное СЛАГАЕМОЕ занижает отношение.
// Измерено в тот день на одном и том же коде:
//
//	грязным прибором  2.24× памяти → порог 3.0 ПРОХОДИЛ
//	чистым прибором   5.79× памяти → порог 3.0 БЫЛ НАРУШЕН
//
// Дефект, который прятался за этим: буфер, выделяемый с нуля в горячем цикле,
// на двух этажах сразу (группа в writeSealedDocs + документ в
// SerializeVectorWithDoc). После починки — 1.34×.
//
// ─── ЧТО ЭТОТ ТЕСТ СТЕРЕЖЁТ ──────────────────────────────────────────────────
// ДВА порога, и второй — не дубль первого:
//
//  1. ОТНОСИТЕЛЬНЫЙ: цена запечатывания кратно записи того же корпуса без
//     скоупов. Ловит регресс В ФИЧЕ.
//  2. ⭐АБСОЛЮТНЫЙ: аллокации базовой записи кратно РАЗМЕРУ СНАПШОТА. Нужен
//     потому, что отношение слепо к просадке ОБЩЕГО пути: если завтра буфер
//     с нуля вернётся в код, который зовут обе руки, числитель и знаменатель
//     вырастут вместе и порог №1 промолчит. Якорь физический (байты на диске),
//     а не другое измерение того же прогона.
//
// Измерено 01.08 на 10k фактов dim=300, 30 термов на факт, чистым прибором:
//
//	без скоупов   28.8 МБ на снапшот 16.9 МБ  = 1.70× выхода
//	со скоупами   38.5 МБ                     = 1.34× базовой записи
package vector

import (
	"fmt"
	"runtime"
	"testing"
	"time"
)

const (
	hnswCostN   = 10_000
	hnswCostDim = 300 // > csrDimThreshold=256 ⇒ hnswSegment без UseSQ

	// Порог №1 — на ЦЕНУ ЗАПЕЧАТЫВАНИЯ, кратно записи корпуса без скоупов.
	// Замерено 1.34×; запас до 2.0 оставлен на рост числа скоупов в фикстуре.
	hnswCostMaxTimeRatio  = 2.0
	hnswCostMaxAllocRatio = 2.0

	// Порог №2 — на БАЗОВУЮ запись, кратно размеру получившегося снапшота.
	// Замерено 1.70×. Это якорь против регресса в общем пути, к которому
	// отношение выше слепо по построению.
	hnswCostMaxBaseAllocPerByte = 3.0

	// Порог №3 — на РАЗМЕР ФАЙЛА. Запечатанный снапшот больше открытого в
	// 1.72×, и это цена не шифрования, а РАЗДВОЕНИЯ вектора: в графе он
	// остаётся нулями (форма слоя обязана сохраниться), настоящий уезжает в
	// документную секцию. До 01.08 размер файла не мерил никто — прежний
	// приёмник bytes.Buffer его знал, но в квитанцию не выводил.
	hnswCostMaxSizeRatio = 2.5
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

// countingWriter — приёмник, который считает байты и НЕ аллоцирует. Именно то,
// чем не был bytes.Buffer: размер снапшота нужен как якорь, но платить за его
// материализацию замер не должен (прод пишет потоком в файл, не в память).
type countingWriter struct{ n int64 }

func (c *countingWriter) Write(p []byte) (int, error) {
	c.n += int64(len(p))
	return len(p), nil
}

func (c *countingWriter) WriteString(s string) (int, error) {
	c.n += int64(len(s))
	return len(s), nil
}

// awaitQuiet ждёт, пока фоновые merge/build закончатся: TotalAlloc процессный,
// и чужая горутина попадёт в замер, если её не дождаться.
func awaitQuiet(t *testing.T, lvs *LeveledVectorStore) {
	t.Helper()
	deadline := time.Now().Add(60 * time.Second)
	for lvs.anyMergeInFlight() || lvs.inFlightBuilds.Load() != 0 {
		if time.Now().After(deadline) {
			t.Fatal("фон не успокоился за 60с — замер считал бы чужие аллокации")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// measureSave — время, суммарно выделенные байты и размер снапшота одного
// SaveBinary. Приёмник не аллоцирует, фон заглушен до и проверен после.
func measureSave(t *testing.T, lvs *LeveledVectorStore) (time.Duration, uint64, int64) {
	t.Helper()
	awaitQuiet(t, lvs)

	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)

	var cw countingWriter
	start := time.Now()
	if err := lvs.SaveBinary(&cw); err != nil {
		t.Fatalf("SaveBinary: %v", err)
	}
	elapsed := time.Since(start)

	runtime.ReadMemStats(&after)

	// КОНТРОЛЬ: фон, проснувшийся ВО ВРЕМЯ записи, загрязнил бы число молча.
	// Без этой проверки тест иногда мерил бы merge и называл это снапшотом.
	if lvs.anyMergeInFlight() || lvs.inFlightBuilds.Load() != 0 {
		t.Fatal("фон проснулся во время замера — число негодно")
	}
	return elapsed, after.TotalAlloc - before.TotalAlloc, cw.n
}

func TestSnapshotHnswSealCost(t *testing.T) {
	if testing.Short() {
		t.Skip("замер цены записи запечатанного hnsw: гоняется без -short")
	}

	run := func(withScope bool) (time.Duration, uint64, int64) {
		lvs := hnswCostStore(t, hnswCostN, hnswCostDim, withScope)
		defer lvs.Clear()
		requireHnswSegment(t, lvs)
		return measureSave(t, lvs)
	}

	// Базовая цена ПЕРВОЙ: без неё число со скоупами не с чем сравнивать.
	baseTime, baseAlloc, baseBytes := run(false)
	sealTime, sealAlloc, sealBytes := run(true)

	mb := func(v uint64) float64 { return float64(v) / (1 << 20) }
	t.Logf("без скоупов (базовая цена снапшота): %v / %.1f МБ аллокаций / снапшот %.1f МБ",
		baseTime.Round(time.Millisecond), mb(baseAlloc), float64(baseBytes)/(1<<20))
	t.Logf("со скоупами (документная секция):    %v / %.1f МБ аллокаций / снапшот %.1f МБ",
		sealTime.Round(time.Millisecond), mb(sealAlloc), float64(sealBytes)/(1<<20))

	if baseTime <= 0 || baseAlloc == 0 || baseBytes == 0 {
		t.Fatal("базовый замер пуст — сравнивать не с чем, пороги ниже прошли бы по неверной причине")
	}

	// ─── Порог №1: цена самой фичи ───────────────────────────────────────────
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

	// ─── Порог №2: якорь против регресса в ОБЩЕМ пути ────────────────────────
	basePerByte := float64(baseAlloc) / float64(baseBytes)
	t.Logf("базовая запись: %.2f× аллокаций на байт снапшота (якорь, отношению выше не виден)",
		basePerByte)
	if basePerByte > hnswCostMaxBaseAllocPerByte {
		t.Errorf("базовая запись аллоцирует %.2f× размера снапшота при пороге %.1f× — "+
			"просадка ОБЩЕГО пути, отношение её не показывает",
			basePerByte, hnswCostMaxBaseAllocPerByte)
	}
	// ─── Порог №3: файл на диске ─────────────────────────────────────────────
	sizeRatio := float64(sealBytes) / float64(baseBytes)
	t.Logf("снапшот со скоупами больше открытого в %.2f× (вектор лежит дважды: нули в графе + документная секция)",
		sizeRatio)
	if sizeRatio > hnswCostMaxSizeRatio {
		t.Errorf("запечатанный снапшот больше открытого в %.2f× при пороге %.1f×", sizeRatio, hnswCostMaxSizeRatio)
	}
	fmt.Println()
}
