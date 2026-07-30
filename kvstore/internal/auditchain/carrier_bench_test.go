// Носителя цепи на диске ещё нет: это ЗАМЕР ЦЕНЫ ДО КОДА (дисциплина
// «эксперимент до реализации с порогами»). Решается ровно один вопрос —
// fsync-политика носителя, и он тот же самый, что уже решался для DEK на факт:
// событие цепи можно писать синхронно (дорого, но неотрицаемо) или батчем
// (дёшево, но с окном).
//
// ⭐ЧЕМ ЭТОТ ВЫБОР ОТЛИЧАЕТСЯ ОТ ВЫБОРА ДЛЯ WAL. У WAL батч в 100 мс стоит
// ДАННЫХ: при отказе теряется до 100 мс записей, и это осознанно принятый RPO.
// У цепи батч стоит ГАРАНТИИ. Голова, отставшая от журнала на k записей, после
// аварии выглядит в точности как обрезанный хвост, и наоборот: владелец,
// отрезавший k последних записей, неотличим от машины, которую выключили в
// середине батча. То есть окно батча — это не окно потери, это окно
// НЕДОКАЗУЕМОСТИ. Поэтому число нужно не «чтобы было быстро», а чтобы знать,
// за какую цену покупается доказуемость каждого события, и назвать окно вслух,
// если платить не станем.
//
// ПОРОГИ, НАЗНАЧЕННЫЕ ЗАРАНЕЕ (иначе замер бессмыслен):
//
//	Бюджет вставки факта — 188 мкс (канон docs/VMEM_DESIGN.md, ≈5324 вставки/с).
//	Шифрование заняло на нём 1.9 мкс ≈ 1% и было принято.
//
//	ПОРОГ A — синхронно на горячем пути (REMEMBER): полная цена события
//	(журнал + голова) < 5% бюджета вставки = 9.4 мкс. Те же 5%, что применялись
//	к RECALL в cost_bench_test.go. Не уложились — REMEMBER синхронно писать
//	нельзя, и окно недоказуемости придётся назвать в доках.
//
//	ПОРОГ B — батч: амортизированная цена события < 1.9 мкс. Цепь не должна
//	стоить дороже конверта, который на этом же пути уже принят.
//
//	SHRED — порога по скорости НЕТ: команда редкая, оператор её ждёт, и она уже
//	платит keyring.persistLocked (tmp → fsync → rename → fsync каталога). Число
//	нужно, чтобы назвать цену, а не чтобы отвергнуть вариант.
//
// ⚠ЧЕСТНОСТЬ ЗАМЕРА. На tmpfs fsync бесплатен, и весь бенч был бы зелёным по
// неверной причине — тот самый класс ошибки, что ловили три раза за два дня.
// Поэтому каталог проверяется через statfs и на tmpfs/ramfs бенч ПАДАЕТ, а тип
// ФС и модель устройства печатаются в вывод: число без них не воспроизводимо.
//
// ⭐ЧЕГО ЭТОТ БЕНЧ НЕ МЕРЯЕТ (называю, чтобы числом не воспользовались шире):
//   - групповой коммит: под несколькими писателями fsync-и склеиваются, и
//     реальная амортизированная цена будет ЛУЧШЕ измеренной здесь однопоточной;
//   - вариант «писать события внутрь WAL» отвергнут не ценой, а назначением:
//     WAL компактится, и след существования факта исчезает вместе с сегментом —
//     ровно то, ради чего цепь и заводится. Отдельный файл обязателен.
//
// ===========================================================================
// ИЗМЕРЕНО 28.07.2026. i7-9750H, SHA-NI на этом CPU НЕТ; ext4 rw,relatime
// (барьеры включены), NVMe WDC SN530, write_cache=write back → fsync реально
// идёт на устройство.
//
// ⭐СНЯТО ДВАЖДЫ, И ЭТО ОКАЗАЛОСЬ НЕ ЛИШНИМ. Первый прогон был на батарее, а
// канон 188 мкс снимался ОТ СЕТИ ([[vector-real-e2e-numbers]], перемер 11.07).
// От сети EPP переключается balance_power → balance_performance, частота идёт
// 1.7 → 4.1 ГГц. Канонная колонка — СЕТЬ, то есть условие, в котором снят сам
// бюджет; батарея оставлена как контраст, она показывает, какая часть цены чем
// оплачена.
//
//	                               СЕТЬ (канон)     батарея
//	Link+Hash remember             571 нс           1952 нс  (3.4×)
//	Link+Hash shred                725 нс           2230 нс
//	кадр записи                    246 нс           523 нс
//	журнал 178 Б, nosync           572 нс           775 нс
//	журнал, fdatasync              0.75…0.79 мс     1.06 мс
//	журнал, fsync                  0.65…0.80 мс     0.97 мс
//	голова 48 Б, 2 слота+fdatasync 0.42 мс          0.46 мс
//	голова, 2 слота+fsync          0.82 мс          0.91 мс
//	голова, tmp→rename→fsync кат.  2.16 мс          2.25 мс
//	СОБЫТИЕ синхронно, максимум    3.21 мс          3.53 мс
//	СОБЫТИЕ синхронно, дёшево      1.49 мс          1.47 мс
//	батч 8                         179 мкс          214 мкс
//	батч 64                        32.5 мкс         33.0 мкс
//	батч 532 (тик 100 мс)          5.9…7.3 мкс      9.6…10.2 мкс
//	nosync (CPU + кэш страниц)     1.39…1.43 мкс    4.4…4.8 мкс
//	SHRED синхронно                2.78 мс          3.26 мс
//
// ⭐ПИТАНИЕ РАЗДЕЛИЛО ЦЕНУ НАДВОЕ, И ЭТО САМО ПО СЕБЕ РЕЗУЛЬТАТ: диск не
// сдвинулся вовсе (синхронно дёшево 1.47 → 1.49 мс; батч 64 — 33.0 → 32.5), а
// CPU упал в 3.2 раза (nosync 4.6 → 1.42 мкс). Диск и процессор в цене события
// разделяются чисто, и рычаги к ним разные. Диагноз подтвердился и по приборам:
// у NVMe runtime PM = unsupported (накопитель не усыпляется между I/O и не
// платит пробуждение), vm.laptop_mode = 0 — батарея душила только процессор.
//
// Снято на /tmp (nvme0n1p4). Перепроверено на томе /home (nvme0n1p5), где
// реально живёт dataDir — картина та же, число не артефакт раздела. Страж от
// tmpfs проверен прогоном на /dev/shm: падает, как обещано. ⚠Разброс дисковых
// чисел ~±20% от прогона к прогону — ни один вердикт ниже не опирается на
// зазор такого порядка.
//
// ВЕРДИКТ ПО ПОРОГАМ.
//
// ПОРОГ A ПРОВАЛЕН НА ДВА С ПОЛОВИНОЙ ПОРЯДКА, не «на чуть-чуть»: 1.49 мс
// против 9.4 мкс — в 158 раз, а с максимальной гарантией в 341 раз. Одно
// событие цепи стоит 8–17 ПОЛНЫХ бюджетов вставки: темп упал бы с 5324/с до
// 310–670/с. Синхронная цепь на REMEMBER — не «дорого», это другой продукт.
// Питание здесь ничего не решает: зазор такого порядка им не закрывается, и на
// батарее вердикт был тот же.
//
// ⚠ПОРОГ B БЫЛ НАЗНАЧЕН НЕВЕРНО, И ЭТО ОШИБКА ПОСТАНОВКИ, А НЕ РЕЗУЛЬТАТ.
// «Не дороже конверта (1.9 мкс)» — негодный ориентир: конверт шифрует байты,
// которые и так пишутся, а цепь ДОБАВЛЯЕТ второй файл, второй fsync и хеш на
// запись. Разные классы работы сравнивать было нечем. Правильный ориентир —
// тот же, что у A: 5% бюджета вставки, 9.4 мкс. (От сети порог B формально
// берётся — 1.42 мкс nosync, — но это совпадение, а не оправдание постановки:
// на батарее он не брался даже при полностью выключенной долговечности, то есть
// никакая политика fsync его взять и не могла.)
//
// ⭐ПО ИСПРАВЛЕННОМУ ПОРОГУ БАТЧ ПРОХОДИТ: 5.9…7.3 мкс против 9.4 — это
// 3.1–3.9% бюджета вставки, ниже порога все 5 прогонов, худший из них 78% от
// порога. Политика для REMEMBER — батч на тике WAL-синкера.
//
// ⚠ЭТОТ ВЫВОД ЖИВЁТ ТОЛЬКО НА СЕТЕВОМ ЗАМЕРЕ. На батарее тот же батч давал
// 9.6…10.2 мкс и порог НЕ проходил — потому что больше половины цены здесь CPU
// (хеш, кадрирование, 3 аллокации), а батарея душит именно его. Абсолютное
// число сравнивается с порогом от сетевого канона, поэтому и снимать его надо
// от сети. Отношения (голова, fdatasync/fsync) этой оговорки не требуют:
// по канону проекта «absolute QPS conservative, ratios reliable».
//
// ГОЛОВА: образец keyring.persistLocked копировать НЕ НАДО. Два слота с CRC в
// файле постоянного размера + fdatasync дают ту же защиту от отказа питания
// (правило «валидный с большим seq») за 0.42 мс против 2.16 мс — дешевле в 5.1
// раза. Отношение к питанию устойчиво (4.9× на батарее), как и положено
// отношению. Rename нужен там, где переписывается файл ПЕРЕМЕННОГО размера
// целиком; голова — 48 байт фиксированной длины.
//
// FDATASYNC ВЫИГРЫВАЕТ ТОЛЬКО НА ФАЙЛЕ ПОСТОЯННОГО РАЗМЕРА: на голове 0.42
// против 0.82 мс (вдвое), на РАСТУЩЕМ журнале выигрыша нет — 0.75…0.79 против
// 0.65…0.80 мс, диапазоны перекрываются. Причина содержательная: при
// дописывании меняется размер файла, а он метаданное, без которого данные не
// достать, и fdatasync сбрасывает его всё равно.
//
// ⚠ЭТУ СТРОКУ ЕДВА НЕ ЗАПИСАЛИ НЕВЕРНО. Прогон подряд дал у fsync на журнале
// преимущество 29% (0.83 против 1.06 мс) и выглядел как основание рекомендовать
// fsync журналу и fdatasync голове. Контрольный прогон с ОБРАТНЫМ порядком и в
// изоляции разрыв схлопнул — это был артефакт очерёдности подтестов, а не
// свойство. Урок: сравнение двух режимов внутри одного BenchmarkXxx проверять
// перестановкой, иначе порядок запуска попадает в вывод как результат.
//
// SHRED: 2.78 мс, порога не было и не нужно. Команда редкая, оператор её ждёт,
// и она уже платит keyring.persistLocked того же порядка — удвоение цены
// редкой команды покупает неотрицаемость её же квитанции.
// ===========================================================================
package auditchain

import (
	"crypto/rand"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/sys/unix"
)

// Пороги в наносекундах — из шапки.
const (
	insertBudgetNs   = 188_000             // канон: бюджет вставки факта
	syncThresholdNs  = insertBudgetNs / 20 // порог A: 5% бюджета
	batchThresholdNs = 1_900               // порог B: не дороже конверта
)

// batchAtCanonRate — сколько событий накопится за один тик WAL-синкера
// (100 мс) при канонном темпе вставки 5324/с. Это и есть реальный размер
// батча, если цепь синхронизировать на том же тике.
const batchAtCanonRate = 532

// benchDir возвращает каталог для замера и ПАДАЕТ, если он в памяти.
//
// Переопределяется через AUDITCHAIN_BENCH_DIR — чтобы число можно было снять
// на том томе, где реально лежит dataDir, а не только на /tmp.
func benchDir(b *testing.B) string {
	b.Helper()
	dir := os.Getenv("AUDITCHAIN_BENCH_DIR")
	if dir == "" {
		dir = b.TempDir()
	} else {
		var err error
		dir, err = os.MkdirTemp(dir, "auditchain-bench-")
		if err != nil {
			b.Fatal(err)
		}
		b.Cleanup(func() { os.RemoveAll(dir) })
	}

	var st unix.Statfs_t
	if err := unix.Statfs(dir, &st); err != nil {
		b.Fatalf("statfs %s: %v", dir, err)
	}
	const (
		tmpfsMagic = 0x01021994
		ramfsMagic = 0x858458f6
	)
	switch st.Type {
	case tmpfsMagic, ramfsMagic:
		b.Fatalf("каталог %s в памяти (tmpfs/ramfs) — fsync там бесплатен, "+
			"и замер был бы зелёным по неверной причине; задайте AUDITCHAIN_BENCH_DIR "+
			"на дисковом томе", dir)
	}
	return dir
}

// reportEnv печатает, на чём снято число: без этого оно не воспроизводимо.
func reportEnv(b *testing.B, dir string) {
	b.Helper()
	fsType := "?"
	var st unix.Statfs_t
	if err := unix.Statfs(dir, &st); err == nil {
		fsType = fmt.Sprintf("0x%x", uint64(st.Type)) // 0xef53 = ext4
	}
	cache, _ := os.ReadFile("/sys/block/nvme0n1/queue/write_cache")
	b.Logf("замер в %s (fs type %s, write_cache=%q)", dir, fsType, trimNL(string(cache)))
}

func trimNL(s string) string {
	for len(s) > 0 && (s[len(s)-1] == '\n' || s[len(s)-1] == '\r') {
		s = s[:len(s)-1]
	}
	return s
}

// Формы записей взяты из фактического пути записи VMEM.
//
// remember: scope вида "user:nikolay", subject — ULID факта (26 симв.),
// payload — источник и хеш содержимого (то, что позволяет доказать, что
// подсунули не этот факт).
//
// shred: payload — поля квитанции (scope/kek_id/facts_removed/destroyed_at).
func sampleRecord(typ EventType) Record {
	switch typ {
	case EventShred:
		return Record{
			Seq:      1,
			UnixNano: 1_753_700_000_000_000_000,
			Type:     EventShred,
			Scope:    "user:nikolay",
			Subject:  "",
			Payload:  make([]byte, 128), // квитанция
		}
	default:
		return Record{
			Seq:      1,
			UnixNano: 1_753_700_000_000_000_000,
			Type:     EventRemember,
			Scope:    "user:nikolay",
			Subject:  "01J9ZQKF7YQ8H3M2N4P6R8T0VW",
			Payload:  make([]byte, 64), // источник + хеш содержимого
		}
	}
}

// ⭐КАДРИРОВАНИЕ, ГОЛОВА И ЕЁ РАЗМЕР ЖИВУТ В carrier.go, А НЕ ЗДЕСЬ.
//
// Пока носителя не было, бенч держал их у себя. Теперь носитель написан, и
// копии удалены намеренно: бенч, меряющий собственную копию кода, меряет не
// то, что выполняется в проде, и расходится с ним молча. Ровно этот разрыв
// чинил П11 (`applyEntry` против зеркала `replaySealedWAL`). Все числа в шапке
// сняты с этих же frameRecord/encodeHead/headSlotSize.

// ---------------------------------------------------------------------------
// 1. CPU: связывание и хеширование. Нужно, чтобы видеть, что дальше меряется
//    диск, а не наша арифметика.
// ---------------------------------------------------------------------------

func BenchmarkLinkAndHash(b *testing.B) {
	for _, tc := range []struct {
		name string
		typ  EventType
	}{{"remember", EventRemember}, {"shred", EventShred}} {
		b.Run(tc.name, func(b *testing.B) {
			src := sampleRecord(tc.typ)
			rand.Read(src.Payload)
			var head Head
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				r := Link(head, src.UnixNano, src.Type, src.Scope, src.Subject, src.Payload)
				head = Head{Seq: r.Seq, Hash: Hash(r)}
			}
			_ = head
		})
	}
}

// BenchmarkFrame — кадрирование записи (длина + CRC) поверх хеширования.
func BenchmarkFrame(b *testing.B) {
	r := sampleRecord(EventRemember)
	rand.Read(r.Payload)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = frameRecord(r)
	}
}

// ---------------------------------------------------------------------------
// 2. Журнал: append-only. Три режима долговечности одной записи.
// ---------------------------------------------------------------------------

func BenchmarkJournalAppend(b *testing.B) {
	frame := frameRecord(sampleRecord(EventRemember))
	b.Logf("кадр записи remember: %d Б", len(frame))

	modes := []struct {
		name string
		sync func(*os.File) error
	}{
		{"nosync", func(*os.File) error { return nil }},
		{"fdatasync", func(f *os.File) error { return unix.Fdatasync(int(f.Fd())) }},
		{"fsync", func(f *os.File) error { return f.Sync() }},
	}
	for _, m := range modes {
		b.Run(m.name, func(b *testing.B) {
			dir := benchDir(b)
			reportEnv(b, dir)
			f, err := os.OpenFile(filepath.Join(dir, "chain.log"),
				os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
			if err != nil {
				b.Fatal(err)
			}
			defer f.Close()
			b.SetBytes(int64(len(frame)))
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				if _, err := f.Write(frame); err != nil {
					b.Fatal(err)
				}
				if err := m.sync(f); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

// ---------------------------------------------------------------------------
// 3. Голова: она обновляется на КАЖДОЕ событие, поэтому её цена входит в цену
//    события целиком. Два способа сделать это переживающим отказ питания.
// ---------------------------------------------------------------------------

func BenchmarkHeadWrite(b *testing.B) {
	b.Run("inplace_2slot_fdatasync", func(b *testing.B) {
		benchHeadInplace(b, func(f *os.File) error { return unix.Fdatasync(int(f.Fd())) })
	})
	b.Run("inplace_2slot_fsync", func(b *testing.B) {
		benchHeadInplace(b, func(f *os.File) error { return f.Sync() })
	})
	b.Run("atomic_rename", benchHeadAtomicRename)
}

// benchHeadInplace — два слота в одном файле, запись попеременно по смещению.
// Файл создаётся один раз, размер не меняется → метаданные inode не трогаются.
func benchHeadInplace(b *testing.B, sync func(*os.File) error) {
	dir := benchDir(b)
	reportEnv(b, dir)
	path := filepath.Join(dir, "chain.head")
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		b.Fatal(err)
	}
	defer f.Close()
	if err := f.Truncate(2 * headSlotSize); err != nil {
		b.Fatal(err)
	}
	if err := f.Sync(); err != nil {
		b.Fatal(err)
	}
	var head Head
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		head.Seq++
		buf := encodeHead(head)
		off := int64(head.Seq%2) * headSlotSize
		if _, err := f.WriteAt(buf, off); err != nil {
			b.Fatal(err)
		}
		if err := sync(f); err != nil {
			b.Fatal(err)
		}
	}
}

// benchHeadAtomicRename — образец keyring.persistLocked: tmp → fsync → rename
// → fsync каталога. Максимальная гарантия; здесь меряется её цена.
func benchHeadAtomicRename(b *testing.B) {
	dir := benchDir(b)
	reportEnv(b, dir)
	path := filepath.Join(dir, "chain.head")
	var head Head
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		head.Seq++
		if err := writeHeadAtomic(path, dir, encodeHead(head)); err != nil {
			b.Fatal(err)
		}
	}
}

func writeHeadAtomic(path, dir string, buf []byte) error {
	tmp := path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	if _, err := f.Write(buf); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		return err
	}
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	if err := d.Sync(); err != nil {
		d.Close()
		return err
	}
	return d.Close()
}

// ---------------------------------------------------------------------------
// 4. ГЛАВНОЕ ЧИСЛО: цена одного события целиком — то, что решает вопрос.
//    Событие = связать + кадрировать + дописать в журнал + обновить голову,
//    причём журнал обязан стать долговечным ДО головы (иначе голова ссылается
//    на запись, которой после аварии не будет).
// ---------------------------------------------------------------------------

func BenchmarkEvent(b *testing.B) {
	b.Run("sync_max_guarantee", func(b *testing.B) { benchEvent(b, syncMax, 1) })
	b.Run("sync_cheap", func(b *testing.B) { benchEvent(b, syncCheap, 1) })
	for _, n := range []int{8, 64, batchAtCanonRate} {
		b.Run(fmt.Sprintf("batch_%d", n), func(b *testing.B) { benchEvent(b, syncCheap, n) })
	}
	b.Run("nosync", func(b *testing.B) { benchEvent(b, syncNone, 1) })
}

type syncMode int

const (
	syncNone  syncMode = iota // ничего не гарантируем — нижняя граница цены
	syncCheap                 // fdatasync журнала + голова на месте
	syncMax                   // fsync журнала + голова через rename с fsync каталога
)

// benchEvent гоняет b.N событий, синхронизируя раз в batchN штук.
// ns/op — амортизированная цена ОДНОГО события, её и сравниваем с порогами.
func benchEvent(b *testing.B, mode syncMode, batchN int) {
	dir := benchDir(b)
	reportEnv(b, dir)

	jf, err := os.OpenFile(filepath.Join(dir, "chain.log"),
		os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		b.Fatal(err)
	}
	defer jf.Close()

	headPath := filepath.Join(dir, "chain.head")
	hf, err := os.OpenFile(headPath, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		b.Fatal(err)
	}
	defer hf.Close()
	if err := hf.Truncate(2 * headSlotSize); err != nil {
		b.Fatal(err)
	}

	src := sampleRecord(EventRemember)
	rand.Read(src.Payload)

	flush := func(head Head) {
		switch mode {
		case syncNone:
			return
		case syncCheap:
			if err := unix.Fdatasync(int(jf.Fd())); err != nil {
				b.Fatal(err)
			}
			off := int64(head.Seq%2) * headSlotSize
			if _, err := hf.WriteAt(encodeHead(head), off); err != nil {
				b.Fatal(err)
			}
			if err := unix.Fdatasync(int(hf.Fd())); err != nil {
				b.Fatal(err)
			}
		case syncMax:
			if err := jf.Sync(); err != nil {
				b.Fatal(err)
			}
			if err := writeHeadAtomic(headPath, dir, encodeHead(head)); err != nil {
				b.Fatal(err)
			}
		}
	}

	var head Head
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		r := Link(head, src.UnixNano, src.Type, src.Scope, src.Subject, src.Payload)
		if _, err := jf.Write(frameRecord(r)); err != nil {
			b.Fatal(err)
		}
		head = Head{Seq: r.Seq, Hash: Hash(r)}
		if (i+1)%batchN == 0 {
			flush(head)
		}
	}
	flush(head) // хвост батча
	b.StopTimer()
}

// ---------------------------------------------------------------------------
// 5. SHRED: редкая команда, порога нет. Меряется полная цена одного события с
//    максимальной гарантией — чтобы в доках стояло число, а не «дорого».
// ---------------------------------------------------------------------------

func BenchmarkShredEvent(b *testing.B) {
	dir := benchDir(b)
	reportEnv(b, dir)

	jf, err := os.OpenFile(filepath.Join(dir, "chain.log"),
		os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		b.Fatal(err)
	}
	defer jf.Close()
	headPath := filepath.Join(dir, "chain.head")

	src := sampleRecord(EventShred)
	rand.Read(src.Payload)

	var head Head
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		r := Link(head, src.UnixNano, src.Type, src.Scope, src.Subject, src.Payload)
		if _, err := jf.Write(frameRecord(r)); err != nil {
			b.Fatal(err)
		}
		if err := jf.Sync(); err != nil {
			b.Fatal(err)
		}
		head = Head{Seq: r.Seq, Hash: Hash(r)}
		if err := writeHeadAtomic(headPath, dir, encodeHead(head)); err != nil {
			b.Fatal(err)
		}
	}
}
