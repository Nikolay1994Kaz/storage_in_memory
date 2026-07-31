package vector

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// =============================================================================
// Золотые снапшоты формата v8 — единственный честный способ проверить чтение
// старых файлов с SQ8- и hnsw-сегментами.
//
// ПОЧЕМУ НЕ СИНТЕЗ. Соседний тест (snapshot_version_compat_test.go) получает
// валидный v8 понижением байта версии — но только для frozen-сегмента, формат
// которого между v8 и v9 не менялся. У SQ8 и hnsw секции в v8 НЕ БЫЛО, разница
// не сводится к одному байту, и подделать такой файл нельзя. Эти два сняты
// настоящей сборкой на коммите 531057a (последний до v9) через git worktree.
//
// ЧТО ОНИ ДОКАЗЫВАЮТ. Три разные вещи, и каждая важна по-своему:
//
//	1. Старый файл ЧИТАЕТСЯ. Гейт version>=9 не должен ломать чужие данные при
//	   обновлении — это единственное место, где до сих пор мы опирались на
//	   чтение кода, а не на прогон.
//	2. В старом файле ФАКТЫ ОТКРЫТЫ. Экспозиция, которую закрыл v9, здесь не
//	   утверждение в документации, а вещдок в testdata. Заодно это контроль
//	   фикстуры: если бы файл вдруг оказался запечатанным, все проверки ниже
//	   были бы бессмысленны.
//	3. ПЕРЕСОХРАНЕНИЕ ЛЕЧИТ. Именно это советуют и доки, и предупреждение при
//	   старте, и до сих пор совет никем не проверялся.
// =============================================================================

// goldenV8 читает золотой снапшот из testdata.
func goldenV8(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", "snapshots", name))
	if err != nil {
		t.Fatalf("золотой снапшот %s недоступен: %v", name, err)
	}
	if len(b) < 6 {
		t.Fatalf("золотой снапшот %s слишком мал (%d Б)", name, len(b))
	}
	// КОНТРОЛЬ ФИКСТУРЫ: файл обязан быть именно v8. Иначе тест «чтения
	// старого формата» читал бы новый и был бы зелёным ни о чём.
	if b[5] != 8 {
		t.Fatalf("золотой снапшот %s имеет версию %d, ожидалась 8", name, b[5])
	}
	return b
}

// goldenCases — какой конфиг нужен каждому файлу, чтобы сегмент восстановился
// тем же типом, каким был записан.
var goldenCases = []struct {
	file string
	cfg  func() LeveledConfig
}{
	{"v8_sq.bin", sqSealedConfig},     // UseSQ ⇒ frozenSQSegment
	{"v8_hnsw.bin", hnswSealedConfig}, // dim=300 без SQ ⇒ hnswSegment
}

// TestGoldenV8Readable — обновление не ломает чужие данные.
func TestGoldenV8Readable(t *testing.T) {
	for _, gc := range goldenCases {
		t.Run(gc.file, func(t *testing.T) {
			fc := newFakeCrypto()
			dst := NewLeveledVectorStore(gc.cfg())
			dst.SetSnapshotCrypto(fc.crypto())
			defer dst.Clear()
			if err := dst.LoadBinary(bytes.NewReader(goldenV8(t, gc.file))); err != nil {
				t.Fatalf("LoadBinary(v8): %v", err)
			}
			if got := dst.SnapshotFormatVersion(); got != 8 {
				t.Errorf("версия прочитанного снапшота %d, ожидалась 8 — оболочка не предупредит оператора", got)
			}
			for _, key := range []string{"f-alice-1", "f-alice-2", "f-bob-1", "plain-1"} {
				if _, ok := dst.Get(key); !ok {
					t.Errorf("документ %s не восстановился из v8-снапшота", key)
				}
			}
			// Слои тоже: без термов и атрибутов факт перестаёт быть находимым,
			// то есть «прочиталось» означало бы «прочиталось наполовину».
			assertScope(t, dst, "f-alice-1", "alice")
			assertTerm(t, dst, "f-alice-1", "aurora")
			assertTerm(t, dst, "plain-1", "weather")
		})
	}
}

// TestGoldenV8HoldsPlaintext — вещдок: до v9 факты лежали открытыми.
//
// Проверка сформулирована как УТВЕРЖДЕНИЕ О НАЛИЧИИ, а не об отсутствии, и
// потому не может «позеленеть по пустому буферу»: она падает ровно тогда,
// когда файл перестал быть тем, за что мы его выдаём.
func TestGoldenV8HoldsPlaintext(t *testing.T) {
	for _, gc := range goldenCases {
		t.Run(gc.file, func(t *testing.T) {
			raw := string(goldenV8(t, gc.file))
			for _, secret := range []string{"aurora", "steering", "standup", "chan-zephyr-alice"} {
				if !strings.Contains(raw, secret) {
					t.Errorf("в v8-снапшоте нет %q — файл не тот, за который выдан, и остальные проверки на нём бессмысленны", secret)
				}
			}
		})
	}
}

// TestGoldenV8ResaveSeals — совет «пересохраните снапшот» проверен, а не обещан.
func TestGoldenV8ResaveSeals(t *testing.T) {
	for _, gc := range goldenCases {
		t.Run(gc.file, func(t *testing.T) {
			fc := newFakeCrypto()
			dst := NewLeveledVectorStore(gc.cfg())
			dst.SetSnapshotCrypto(fc.crypto())
			defer dst.Clear()
			if err := dst.LoadBinary(bytes.NewReader(goldenV8(t, gc.file))); err != nil {
				t.Fatalf("LoadBinary(v8): %v", err)
			}

			var resaved bytes.Buffer
			if err := dst.SaveBinary(&resaved); err != nil {
				t.Fatalf("SaveBinary: %v", err)
			}
			if got := resaved.Bytes()[5]; got != SealedSegmentsFormatVersion {
				t.Fatalf("пересохранённый снапшот версии %d, ожидалась %d", got, SealedSegmentsFormatVersion)
			}
			raw := resaved.String()

			// ПАРНЫЙ ПОЛОЖИТЕЛЬНЫЙ КОНТРОЛЬ: посторонний документ шифровать
			// нечем, он обязан остаться открытым и после пересохранения.
			if !strings.Contains(raw, "weather") {
				t.Fatal("терм постороннего документа исчез — пересохранение потеряло данные либо буфер пуст")
			}
			for _, secret := range []string{"aurora", "steering", "standup", "chan-zephyr-alice"} {
				if strings.Contains(raw, secret) {
					t.Errorf("после пересохранения %q всё ещё лежит открытым — совет из документации не работает", secret)
				}
			}
		})
	}
}

// TestGoldenV8ShredCannotReach — обратная сторона того же: пока файл не
// пересохранён, уничтожение ключа до его фактов НЕ достаёт.
//
// Тест закрепляет ограничение, а не дефект: починка forward-only, и притворяться
// иначе значило бы обещать стирание там, где его нет.
func TestGoldenV8ShredCannotReach(t *testing.T) {
	for _, gc := range goldenCases {
		t.Run(gc.file, func(t *testing.T) {
			fc := newFakeCrypto()
			fc.destroyed["alice"] = true // ключ скоупа уничтожен ДО загрузки

			dst := NewLeveledVectorStore(gc.cfg())
			dst.SetSnapshotCrypto(fc.crypto())
			defer dst.Clear()
			if err := dst.LoadBinary(bytes.NewReader(goldenV8(t, gc.file))); err != nil {
				t.Fatalf("LoadBinary(v8): %v", err)
			}
			for _, key := range []string{"f-alice-1", "f-alice-2"} {
				if _, ok := dst.Get(key); !ok {
					t.Errorf("факт %s исчез из v8-снапшота при уничтоженном ключе — значит стирание туда ДОСТАЁТ, и документация про forward-only неверна", key)
				}
			}
		})
	}
}
