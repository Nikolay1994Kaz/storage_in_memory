package vector

import (
	"bytes"
	"encoding/binary"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"testing"
)

// corruptFixture — снапшот со всеми секциями, которые читаются по счётчикам:
// граф (fp32 / SQ8 / hnsw), колонки атрибутов, тенант-каталог, текстовый слой,
// tombstones.
func corruptFixture(t *testing.T, cfg LeveledConfig, dim, docs int) []byte {
	t.Helper()
	lvs := NewLeveledVectorStore(cfg)
	defer lvs.Close()
	for i := 0; i < docs; i++ {
		attrs := Attributes{
			Cat: map[string]string{"tenant": "t" + strconv.Itoa(i%3), "lang": "en"},
			Num: map[string]float64{"price": float64(i)},
		}
		terms := []TermTF{{Term: "alpha", TF: 1}, {Term: "w" + strconv.Itoa(i), TF: 2}}
		if err := lvs.AddDocTerms("k"+strconv.Itoa(i), mkVecN(dim, float32(i)+0.5), attrs, terms); err != nil {
			t.Fatalf("AddDocTerms: %v", err)
		}
	}
	lvs.FlushDeltaSync()
	lvs.Delete("k1") // tombstone на ключ в сегменте
	lvs.Delete("k2")
	var buf bytes.Buffer
	if err := lvs.SaveBinary(&buf); err != nil {
		t.Fatalf("SaveBinary: %v", err)
	}
	return buf.Bytes()
}

// TestLoadBinaryCorruptCountIsBounded — испорченное поле снапшота даёт ошибку
// загрузки, а не выделение памяти по мусорному числу.
//
// CRC проверяется в конце чтения, а выделения идут по счётчикам по ходу:
// число векторов, tombstone-ключей, длина строки. Без границы один
// испорченный счётчик превращался в make на гигабайты — процесс падал по OOM
// на старте вместо штатного «снапшот битый, восстанавливаемся из WAL», и
// падал так при каждом рестарте. Тест портит по очереди каждое 4-байтовое
// окно файла большим числом.
func TestLoadBinaryCorruptCountIsBounded(t *testing.T) {
	sqCfg := bm25TestConfig()
	sqCfg.UseSQ = true
	for _, fc := range []struct {
		name      string
		cfg       LeveledConfig
		dim, docs int
	}{
		{"frozen", bm25TestConfig(), 8, 24},
		{"sq8", sqCfg, 8, 24},
		// dim > 256 без SQ8 — сегмент замерзает в hnsw; документов меньше,
		// чтобы файл (а с ним и число прогонов) оставался небольшим.
		{"hnsw", bm25TestConfig(), 260, 6},
	} {
		t.Run(fc.name, func(t *testing.T) {
			cfg := fc.cfg
			cfg.PartitionAttr = "tenant"
			snap := corruptFixture(t, cfg, fc.dim, fc.docs)
			// Один стор на все прогоны: неудачная загрузка его не меняет
			// (LoadBinary — всё-или-ничего), а удачной здесь быть не должно.
			dst := NewLeveledVectorStore(cfg)
			defer dst.Close()
			// Потолок с запасом: честная загрузка такого файла выделяет
			// единицы мегабайт, порча счётчика без границы — гигабайты.
			const limit = 64 << 20
			var ms runtime.MemStats
			bad := make([]byte, len(snap))
			for off := 8; off+4 <= len(snap)-4; off++ {
				copy(bad, snap)
				binary.LittleEndian.PutUint32(bad[off:], 0xFFFFFFF0)

				runtime.ReadMemStats(&ms)
				before := ms.TotalAlloc
				err := dst.LoadBinary(bytes.NewReader(bad))
				runtime.ReadMemStats(&ms)

				if err == nil {
					t.Fatalf("смещение %d: испорченный снапшот загружен без ошибки", off)
				}
				if grew := ms.TotalAlloc - before; grew > limit {
					t.Fatalf("смещение %d: загрузка испорченного снапшота выделила %d МБ", off, grew>>20)
				}
			}
		})
	}
}

// TestLoadBinaryCorruptFile — тот же бюджет на пути сервера: снапшот читается
// из *os.File, остаток берётся из размера файла. Испорченное число векторов
// frozen-сегмента обязано дать ErrSnapshotCorrupt, а не выделение n×dim.
func TestLoadBinaryCorruptFile(t *testing.T) {
	cfg := bm25TestConfig()
	snap := corruptFixture(t, cfg, 8, 24)
	// Заголовок: magic 8, время 8, LSN 8, dim 4, nLevels 1, nSegs[0] 4,
	// segType 1 — дальше заголовок frozen-графа, первое поле n.
	const nOff = 8 + 8 + 8 + 4 + 1 + 4 + 1
	if snap[nOff-1] != segTypeFrozen {
		t.Fatalf("фикстура: на смещении %d не frozen-сегмент", nOff-1)
	}
	binary.LittleEndian.PutUint32(snap[nOff:], 0xFFFFFFF0)

	path := filepath.Join(t.TempDir(), "graph_leveled.bin")
	if err := os.WriteFile(path, snap, 0o644); err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()

	dst := NewLeveledVectorStore(cfg)
	defer dst.Close()
	if err := dst.LoadBinary(f); !errors.Is(err, ErrSnapshotCorrupt) {
		t.Fatalf("LoadBinary(испорченный файл) = %v, ожидалась ErrSnapshotCorrupt", err)
	}
}
