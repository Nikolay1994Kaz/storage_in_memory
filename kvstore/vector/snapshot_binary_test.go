package vector

import (
	"bytes"
	"math/rand"
	"strconv"
	"testing"

	"kvstore/kvstore/internal/store/tcmalloc"
)

// generateRandomVector генерирует случайный вектор заданной размерности.
func generateRandomVector(dim int) []float32 {
	vec := make([]float32, dim)
	for i := 0; i < dim; i++ {
		vec[i] = rand.Float32()*2.0 - 1.0 // диапазон [-1.0, 1.0]
	}
	return vec
}

func TestVectorStore_BinarySnapshotRoundTrip(t *testing.T) {
	for _, dim := range []int{128, 256} {
		t.Run("dim="+strconv.Itoa(dim), func(t *testing.T) {
			numVectors := 500
			rand.Seed(42) // Делаем генерацию детерминированной

			vsSource := NewVectorStore(EuclideanDistance, tcmalloc.NewTCMallocStore(1))

			// 1. Наполняем базу случайными векторами
			vectors := make(map[string][]float32)
			for i := 0; i < numVectors; i++ {
				key := "vec:" + strconv.Itoa(i)
				vec := generateRandomVector(dim)
				vectors[key] = vec
				if err := vsSource.Add(key, vec); err != nil {
					t.Fatalf("Failed to add vector %s: %v", key, err)
				}
			}

			// 2. Делаем несколько удалений для образования "дыр" (Tombstones, FreeIDs, FreeOffsets)
			deletedKeys := []string{"vec:10", "vec:50", "vec:100", "vec:250", "vec:499"}
			for _, key := range deletedKeys {
				if !vsSource.Delete(key) {
					t.Fatalf("Failed to delete key %s", key)
				}
				delete(vectors, key)
			}

			// 3. Выполняем проверочные поисковые запросы на исходном графе
			numQueries := 20
			K := 5
			queryVectors := make([][]float32, numQueries)
			expectedResults := make([][]VSearchResult, numQueries)

			for i := 0; i < numQueries; i++ {
				queryVectors[i] = generateRandomVector(dim)
				res, err := vsSource.Search(queryVectors[i], K, nil)
				if err != nil {
					t.Fatalf("Search failed during baseline: %v", err)
				}
				expectedResults[i] = res
			}

			// 4. Сериализуем граф в бинарный буфер
			var buf bytes.Buffer
			if err := vsSource.SaveBinary(&buf); err != nil {
				t.Fatalf("SaveBinary failed: %v", err)
			}
			t.Logf("Binary snapshot size: %d bytes (%.2f KB) for %d vectors",
				buf.Len(), float64(buf.Len())/1024, numVectors-len(deletedKeys))

			// 5. Десериализуем в новое чистое хранилище
			vsDest := NewVectorStore(EuclideanDistance, tcmalloc.NewTCMallocStore(1))
			if err := vsDest.LoadBinary(&buf); err != nil {
				t.Fatalf("LoadBinary failed: %v", err)
			}

			// 6. Сверяем параметры метаданных
			sCount, sDim, sMaxL := vsSource.Info()
			dCount, dDim, dMaxL := vsDest.Info()

			if sCount != dCount || sDim != dDim || sMaxL != dMaxL {
				t.Errorf("Metadata mismatch:\nSource: count=%d, dim=%d, maxL=%d\nDest  : count=%d, dim=%d, maxL=%d",
					sCount, sDim, sMaxL, dCount, dDim, dMaxL)
			}

			if vsSource.dim != vsDest.dim || vsSource.autoNormalize != vsDest.autoNormalize {
				t.Errorf("Store dimensions or normalization flags mismatch")
			}

			// 7. Сверяем результаты поиска (они должны совпадать абсолютно бит-в-бит)
			for i := 0; i < numQueries; i++ {
				actualRes, err := vsDest.Search(queryVectors[i], K, nil)
				if err != nil {
					t.Fatalf("Search failed on restored graph: %v", err)
				}

				expected := expectedResults[i]
				if len(expected) != len(actualRes) {
					t.Fatalf("Query %d: results length mismatch (expected %d, got %d)", i, len(expected), len(actualRes))
				}

				for j := range expected {
					if expected[j].Key != actualRes[j].Key {
						t.Errorf("Query %d, item %d: Key mismatch (expected %q, got %q)", i, j, expected[j].Key, actualRes[j].Key)
					}
					if expected[j].Distance != actualRes[j].Distance {
						t.Errorf("Query %d, item %d: Distance mismatch (expected %f, got %f)", i, j, expected[j].Distance, actualRes[j].Distance)
					}
				}
			}
			t.Log("Successfully verified identical search results on restored HNSW graph!")
		})
	}
}

func TestVectorStore_BinarySnapshotErrorHandling(t *testing.T) {
	dim := 128
	vs := NewVectorStore(EuclideanDistance, tcmalloc.NewTCMallocStore(1))
	
	// Вставляем пару векторов
	_ = vs.Add("v1", generateRandomVector(dim))
	_ = vs.Add("v2", generateRandomVector(dim))

	var buf bytes.Buffer
	if err := vs.SaveBinary(&buf); err != nil {
		t.Fatalf("SaveBinary failed: %v", err)
	}
	snapshotBytes := buf.Bytes()

	// 1. Тест на коррупцию Magic Header
	corruptedMagic := append([]byte("WRONG"), snapshotBytes[5:]...)
	vsDest := NewVectorStore(EuclideanDistance, tcmalloc.NewTCMallocStore(1))
	err := vsDest.LoadBinary(bytes.NewReader(corruptedMagic))
	if err == nil {
		t.Error("LoadBinary should fail on corrupted Magic Header, but got no error")
	} else {
		t.Logf("Magic Header corruption caught successfully: %v", err)
	}

	// 2. Тест на несовпадение CRC32 чексуммы
	// Меняем один случайный байт в полезной нагрузке (после заголовка 8+4+8 = 20 байт)
	corruptedPayload := make([]byte, len(snapshotBytes))
	copy(corruptedPayload, snapshotBytes)
	corruptedPayload[25] ^= 0xFF // Инвертируем байт

	err = vsDest.LoadBinary(bytes.NewReader(corruptedPayload))
	if err == nil {
		t.Error("LoadBinary should fail on CRC32 mismatch, but got no error")
	} else {
		t.Logf("CRC32 mismatch caught successfully: %v", err)
	}
}

func TestVectorStore_BinarySnapshotEmptyGraph(t *testing.T) {
	vsSource := NewVectorStore(EuclideanDistance, tcmalloc.NewTCMallocStore(1))
	
	var buf bytes.Buffer
	if err := vsSource.SaveBinary(&buf); err != nil {
		t.Fatalf("SaveBinary failed on empty store: %v", err)
	}

	vsDest := NewVectorStore(EuclideanDistance, tcmalloc.NewTCMallocStore(1))
	if err := vsDest.LoadBinary(&buf); err != nil {
		t.Fatalf("LoadBinary failed on empty store: %v", err)
	}

	sCount, _, _ := vsSource.Info()
	dCount, _, _ := vsDest.Info()
	if sCount != dCount || sCount != 0 {
		t.Errorf("Empty store mismatch: source count=%d, dest count=%d", sCount, dCount)
	}
	t.Log("Successfully verified binary snapshot round-trip on an empty vector store")
}
