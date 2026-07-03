package vector

import (
	"bytes"
	"testing"

	"kvstore/kvstore/internal/store/tcmalloc"
)

// TestSnapshot_WatermarkRoundTrip — страж #2 (идемпотентность recovery векторов).
//
// graph_leveled.bin (v6) хранит LSN-watermark: «снапшот покрывает векторные
// операции до этого LSN». Recovery пропускает реплей WAL-записей с LSN ≤ него,
// иначе операции из окна «ротация WAL → запись снапшота» дублируют векторы при
// каждом рестарте. Здесь проверяем, что watermark переживает SaveBinary→LoadBinary.
func TestSnapshot_WatermarkRoundTrip(t *testing.T) {
	const dim = 8
	src := NewLeveledVectorStore(LeveledConfig{
		Distance:       EuclideanDistance,
		Allocator:      tcmalloc.NewTCMallocStore(2),
		DeltaMax:       1000,
		Fanout:         8,
		EfSearch:       64,
		M:              16,
		EfConstruction: 100,
	})
	defer src.Clear()

	for i := 0; i < 20; i++ {
		if err := src.Add("k"+string(rune('a'+i)), mkVecN(dim, float32(i))); err != nil {
			t.Fatalf("Add: %v", err)
		}
	}
	src.FlushDeltaSync()

	const watermark = uint64(4242)
	src.SetSnapshotWatermark(watermark)

	var buf bytes.Buffer
	if err := src.SaveBinary(&buf); err != nil {
		t.Fatalf("SaveBinary: %v", err)
	}

	dst := NewLeveledVectorStore(LeveledConfig{
		Distance:       EuclideanDistance,
		Allocator:      tcmalloc.NewTCMallocStore(2),
		DeltaMax:       1000,
		Fanout:         8,
		EfSearch:       64,
		M:              16,
		EfConstruction: 100,
	})
	defer dst.Clear()

	if err := dst.LoadBinary(&buf); err != nil {
		t.Fatalf("LoadBinary: %v", err)
	}

	if got := dst.SnapshotLSN(); got != watermark {
		t.Fatalf("SnapshotLSN() после загрузки = %d, ожидали %d — watermark не переживает round-trip", got, watermark)
	}

	// Данные тоже на месте (снапшот валиден).
	res, err := dst.Search(mkVecN(dim, 0), 1, nil)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(res) != 1 || res[0].Key != "ka" {
		t.Fatalf("после загрузки ближайший к 0 = %+v, ожидали \"ka\"", res)
	}
}

// TestSnapshot_WatermarkDefaultZero — свежий store без загрузки имеет watermark 0
// (recovery реплеит все векторные WAL-записи, как до v6).
func TestSnapshot_WatermarkDefaultZero(t *testing.T) {
	lvs := NewLeveledVectorStore(LeveledConfig{
		Distance:  EuclideanDistance,
		Allocator: tcmalloc.NewTCMallocStore(1),
		DeltaMax:  1000,
		Fanout:    8,
	})
	defer lvs.Clear()
	if got := lvs.SnapshotLSN(); got != 0 {
		t.Fatalf("свежий store: SnapshotLSN()=%d, ожидали 0", got)
	}
}
