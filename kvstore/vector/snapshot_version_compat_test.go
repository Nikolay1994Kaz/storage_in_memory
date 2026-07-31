package vector

import (
	"bytes"
	"encoding/binary"
	"hash/crc32"
	"testing"
)

// =============================================================================
// Совместимость со старым форматом снапшота.
//
// ЗАЧЕМ ОТДЕЛЬНЫЙ ТЕСТ. Версия v9 добавила документную секцию SQ8- и
// hnsw-сегментам, и чтение старых файлов держится ровно на трёх гейтах
// `if version >= N`. Гейт — это код, который никогда не исполняется на свежих
// данных: все наши тесты пишут и читают v9, поэтому ошибка в нём не проявится
// ни в одном из них, а проявится у того, кто обновится со снапшотом на диске.
// Ровно тот случай, про который шапка формата говорит «там ломается тихо».
//
// КАК СИНТЕЗИРУЕТСЯ v8. Формат frozen-сегмента между v8 и v9 не менялся —
// секцию под конвертом он получил ещё в v8. Значит снапшот стора, где есть
// только такие сегменты, отличается от валидного v8 РОВНО байтом версии.
// Понижаем его и пересчитываем CRC (магия входит в чексум).
//
// ⚠ЧЕГО ЭТОТ ТЕСТ НЕ ПОКРЫВАЕТ. v8-файл с SQ8- или hnsw-сегментом этим приёмом
// не получить: у них секции в v8 не было, и разница не сводится к одному байту.
// Для тех двух гейтов совместимость проверена только чтением кода. Честная
// проверка требует золотого файла, снятого сборкой до v9.
// =============================================================================

// downgradeToV8 понижает версию снапшота в байте [5] и чинит CRC32-трейлер.
func downgradeToV8(t *testing.T, snap []byte) []byte {
	t.Helper()
	out := append([]byte(nil), snap...)
	// КОНТРОЛЬ: правим то, что думаем. Если версия уже не 9, тест молча
	// проверял бы совместимость версии с самой собой.
	if out[5] != 9 {
		t.Fatalf("версия в снапшоте = %d, ожидалась 9 — понижать нечего", out[5])
	}
	out[5] = 8
	crc := crc32.ChecksumIEEE(out[:len(out)-4])
	binary.LittleEndian.PutUint32(out[len(out)-4:], crc)
	return out
}

// TestLoadV8FrozenSnapshot — снапшот, записанный до v9, читается полностью.
func TestLoadV8FrozenSnapshot(t *testing.T) {
	fc := newFakeCrypto()
	src := sealedStoreWithCfg(t, bm25TestConfig(), fc.crypto()) // dim=8 → frozenSegment
	defer src.Clear()

	var buf bytes.Buffer
	if err := src.SaveBinary(&buf); err != nil {
		t.Fatalf("SaveBinary: %v", err)
	}
	old := downgradeToV8(t, buf.Bytes())

	dst := NewLeveledVectorStore(bm25TestConfig())
	dst.SetSnapshotCrypto(fc.crypto())
	defer dst.Clear()
	if err := dst.LoadBinary(bytes.NewReader(old)); err != nil {
		t.Fatalf("LoadBinary(v8): %v", err)
	}

	for i, key := range []string{"f-alice-1", "f-alice-2", "f-bob-1", "plain-1"} {
		got, ok := dst.Get(key)
		if !ok {
			t.Errorf("документ %s не восстановился из v8-снапшота", key)
			continue
		}
		want := vecOfDoc(i)
		for j := range want {
			if got[j] != want[j] {
				t.Errorf("вектор %s[%d] = %v, ожидалось %v", key, j, got[j], want[j])
				break
			}
		}
	}
	assertScope(t, dst, "f-alice-1", "alice")
	assertTerm(t, dst, "f-alice-1", "aurora")
}

// TestSnapshotFormatVersionReported — условие, по которому оболочка решает,
// предупреждать ли оператора о снапшоте с открытыми фактами.
//
// Сам slog.Warn живёт в main и тестом отсюда не достаётся, но его ТРИГГЕР —
// достаётся, и он единственное, что тут можно ошибиться. Без этой проверки
// предупреждение молча перестало бы срабатывать от любой правки в фазе коммита
// LoadBinary, и никто бы не заметил: отсутствие лога выглядит как «всё хорошо».
func TestSnapshotFormatVersionReported(t *testing.T) {
	fc := newFakeCrypto()
	src := sealedStoreWithCfg(t, bm25TestConfig(), fc.crypto())
	defer src.Clear()

	var buf bytes.Buffer
	if err := src.SaveBinary(&buf); err != nil {
		t.Fatalf("SaveBinary: %v", err)
	}

	load := func(snap []byte) int {
		t.Helper()
		dst := NewLeveledVectorStore(bm25TestConfig())
		dst.SetSnapshotCrypto(fc.crypto())
		defer dst.Clear()
		if err := dst.LoadBinary(bytes.NewReader(snap)); err != nil {
			t.Fatalf("LoadBinary: %v", err)
		}
		return dst.SnapshotFormatVersion()
	}

	if got := load(buf.Bytes()); got != SealedSegmentsFormatVersion {
		t.Errorf("свежий снапшот: версия %d, ожидалась %d — оболочка предупредила бы зря",
			got, SealedSegmentsFormatVersion)
	}
	if got := load(downgradeToV8(t, buf.Bytes())); got != 8 {
		t.Errorf("старый снапшот: версия %d, ожидалась 8 — оболочка промолчала бы там, где факты открыты", got)
	}

	// Стор без загрузки обязан давать 0: «снапшота не было» — не то же самое,
	// что «снапшот старый», и предупреждать в этом случае не о чем.
	fresh := NewLeveledVectorStore(bm25TestConfig())
	defer fresh.Clear()
	if got := fresh.SnapshotFormatVersion(); got != 0 {
		t.Errorf("стор без снапшота: версия %d, ожидался 0", got)
	}
}

// TestV8SnapshotStillShreds — стирание работает и по старому файлу: у frozen
// документная секция была уже в v8, и гейт v9 не должен был её отобрать.
func TestV8SnapshotStillShreds(t *testing.T) {
	fc := newFakeCrypto()
	src := sealedStoreWithCfg(t, bm25TestConfig(), fc.crypto())
	defer src.Clear()

	var buf bytes.Buffer
	if err := src.SaveBinary(&buf); err != nil {
		t.Fatalf("SaveBinary: %v", err)
	}
	old := downgradeToV8(t, buf.Bytes())

	fc.destroyed["alice"] = true

	dst := NewLeveledVectorStore(bm25TestConfig())
	dst.SetSnapshotCrypto(fc.crypto())
	defer dst.Clear()
	if err := dst.LoadBinary(bytes.NewReader(old)); err != nil {
		t.Fatalf("LoadBinary(v8): %v", err)
	}
	for _, key := range []string{"f-alice-1", "f-alice-2"} {
		if _, ok := dst.Get(key); ok {
			t.Errorf("факт стёртого скоупа %s воскрес из v8-снапшота", key)
		}
	}
	if _, ok := dst.Get("f-bob-1"); !ok {
		t.Error("документ соседнего скоупа не пережил стирание — это потеря данных, а не стирание")
	}
}
