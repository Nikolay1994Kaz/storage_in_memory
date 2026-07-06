package vector

import (
	"bytes"
	"testing"
)

// TestSnapshot_LoadBinaryAtomicOnError — репро/страж неатомарного LoadBinary.
//
// Баг: LoadBinary мутировала стор по мере чтения (dim, levels, tombstones,
// snapshotLSN), а CRC сверялся в самом конце. При ошибке (bit-rot, усечённый
// файл) возвращалась error, но стор оставался наполовину загруженным. Вызывающая
// сторона (main.go recovery) на ошибку отвечает «rebuild from WAL» и реплеит WAL
// ПОВЕРХ этого мусора → тихо-неконсистентная база вместо честного отказа.
//
// Контракт после фикса: LoadBinary всё-или-ничего — при любой ошибке стор
// остаётся ровно в том состоянии, в котором был до вызова.
func TestSnapshot_LoadBinaryAtomicOnError(t *testing.T) {
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

	// Источник: сегменты + tombstone + ненулевой LSN-watermark — чтобы у частичной
	// загрузки было что оставить после себя по каждому из мутируемых полей.
	src := NewLeveledVectorStore(cfg)
	defer src.Close()
	for i := 0; i < 300; i++ {
		if err := src.Add(itoa(i), mk(i)); err != nil {
			t.Fatal(err)
		}
	}
	src.FlushDeltaSync()
	if !src.Delete(itoa(42)) { // ключ во frozen-сегменте → персистентный tombstone
		t.Fatal("Delete(42) не удалил ключ из сегмента")
	}
	src.SetSnapshotWatermark(777)
	var buf bytes.Buffer
	if err := src.SaveBinary(&buf); err != nil {
		t.Fatalf("SaveBinary: %v", err)
	}

	corruptCRC := append([]byte(nil), buf.Bytes()...)
	corruptCRC[len(corruptCRC)/2] ^= 0xFF // payload парсится целиком, CRC в конце не сойдётся

	truncated := append([]byte(nil), buf.Bytes()...)
	truncated = truncated[:len(truncated)*3/5] // обрыв посреди чтения сегментов

	cases := []struct {
		name string
		data []byte
	}{
		{"CRCMismatch", corruptCRC},
		{"Truncated", truncated},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// Приёмник с собственным состоянием, которое обязано пережить неудачную загрузку.
			dst := NewLeveledVectorStore(cfg)
			defer dst.Close()
			if err := dst.Add("survivor", mk(9000)); err != nil {
				t.Fatal(err)
			}
			dst.FlushDeltaSync() // survivor в levels — сброс уровней его бы стёр

			if err := dst.LoadBinary(bytes.NewReader(tc.data)); err == nil {
				t.Fatal("LoadBinary приняла повреждённый снапшот")
			}

			// Всё-или-ничего: ни одно поле не должно быть тронуто.
			if got := dst.Len(); got != 1 {
				t.Errorf("РЕПРО: Len=%d после неудачной загрузки, ожидали 1 (только survivor)", got)
			}
			if _, ok := dst.Get("survivor"); !ok {
				t.Error("РЕПРО: survivor исчез — levels сброшены частичной загрузкой")
			}
			if _, ok := dst.Get(itoa(0)); ok {
				t.Error("РЕПРО: ключ из битого снапшота виден в сторе — частичная загрузка осталась")
			}
			if got := dst.SnapshotLSN(); got != 0 {
				t.Errorf("РЕПРО: SnapshotLSN=%d после неудачной загрузки, ожидали 0", got)
			}
			if got := dst.Stats().Tombstones; got != 0 {
				t.Errorf("РЕПРО: Tombstones=%d после неудачной загрузки, ожидали 0", got)
			}

			// Стор остаётся рабочим: добавление и чтение после отказа.
			if err := dst.Add("post-failure", mk(9001)); err != nil {
				t.Fatalf("Add после неудачной загрузки: %v", err)
			}
			if _, ok := dst.Get("post-failure"); !ok {
				t.Error("Get(post-failure) не видит вектор, добавленный после неудачной загрузки")
			}
		})
	}
}
