package vector

import (
	"bytes"
	"strings"
	"testing"
)

// TestSnapshot_CRCDetectsCorruption — репро/страж P0-3.
//
// Используемый формат снапшота (LeveledVectorStore.SaveBinary) раньше не имел
// чексуммы: тихий bit-rot в graph_leveled.bin грузился как валидный → битые
// данные попадали в индекс. v5 добавляет CRC32-трейлер, проверяемый на загрузке.
func TestSnapshot_CRCDetectsCorruption(t *testing.T) {
	const dim = 32
	mk := func(seed int) []float32 {
		v := make([]float32, dim)
		for i := range v {
			v[i] = float32(seed) + float32(i)*0.5
		}
		return v
	}

	cfg := LeveledConfig{
		Distance:       EuclideanDistance,
		EfConstruction: 64,
		EfSearch:       64,
	}

	lvs := NewLeveledVectorStore(cfg)
	defer lvs.Close()
	for i := 0; i < 300; i++ {
		if err := lvs.Add(itoa(i), mk(i)); err != nil {
			t.Fatal(err)
		}
	}
	lvs.FlushDeltaSync() // данные во frozen-сегменте → есть что портить

	var buf bytes.Buffer
	if err := lvs.SaveBinary(&buf); err != nil {
		t.Fatalf("SaveBinary: %v", err)
	}

	// Контроль: неповреждённый снапшот грузится без ошибок.
	clean := NewLeveledVectorStore(cfg)
	defer clean.Close()
	if err := clean.LoadBinary(bytes.NewReader(buf.Bytes())); err != nil {
		t.Fatalf("чистый снапшот не загрузился: %v", err)
	}

	// Портим байт в СЕРЕДИНЕ (вектор-данные, не структура и не CRC-трейлер).
	corrupt := append([]byte(nil), buf.Bytes()...)
	corrupt[len(corrupt)/2] ^= 0xFF

	restored := NewLeveledVectorStore(cfg)
	defer restored.Close()
	err := restored.LoadBinary(bytes.NewReader(corrupt))
	if err == nil {
		t.Fatal("РЕПРО P0-3: LoadBinary приняла повреждённый снапшот — CRC не проверяется")
	}
	if !strings.Contains(err.Error(), "CRC mismatch") {
		t.Fatalf("ожидали ошибку CRC mismatch, получили: %v", err)
	}
}
