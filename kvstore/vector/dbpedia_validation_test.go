//go:build datasets

// Замер на внешнем датасете. Тег `datasets` намеренно держит его ВНЕ обычной
// сборки тестов: без файла в /tmp такой тест скипался молча, и «ok» пакета
// означал в том числе «тридцать функций даже не пытались запуститься».
// Запуск и получение данных — docs/BENCHMARKS.md, раздел Reproducing:
//
//	make test-datasets

package vector

import (
	"encoding/binary"
	"fmt"
	"os"
	"strconv"
	"testing"
	"time"
	"unsafe"

	"kvstore/kvstore/internal/store/tcmalloc"
)

// =============================================================================
// Валидация движка на РЕАЛЬНЫХ трансформерных эмбеддингах — dbpedia-openai
// (OpenAI ada-002, 1536-dim, angular/cosine, вектора L2-нормализованы). Это
// продакшн-геометрия (анизотропия/конус), которую SIFT/MNIST (euclidean,
// image-признаки) не воспроизводят — см. [[vector-direction-20260629]].
//
// Цель: держатся ли наши пороги/калибровка (SQ8, efSearch) на cosine + high-dim?
// Меряем recall@10 vs готовый ground-truth + QPS_12 для float32 и SQ8.
//
// Вектора уже нормализованы → cosine-ранжирование == dot-ранжирование, берём
// DotProductDistance (1-dot). Параллельно проверяем риск isDotDistance (pointer-
// сравнение): если SQ8 уходит в euclidean-ADC, recall просядет vs float32.
//
//   go test ./kvstore/vector -run TestDBpedia_RealEmbeddingValidation -v -timeout 1800s
//
// Требует /tmp/dbpedia100k.bin (scripts/convert_dbpedia.py).
// =============================================================================

// loadDBpediaRaw читает формат convert_dbpedia.py: train/test (как SIFT) + GT.
func loadDBpediaRaw(path string) (train, test [][]float32, gt [][]int32, err error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, nil, nil, err
	}
	if len(data) < 8 {
		return nil, nil, nil, fmt.Errorf("file too small")
	}
	n := int(binary.LittleEndian.Uint32(data[0:4]))
	dim := int(binary.LittleEndian.Uint32(data[4:8]))
	off := 8

	// ⚠Границы проверяются ДО unsafe.Slice: без этого файл, собранный не тем
	// скриптом (convert_annbench.py пишет тот же формат, но БЕЗ хвоста ground
	// truth), давал чтение за пределами буфера — не ошибку, а UB. Формат
	// собирает scripts/convert_dbpedia.py.
	need := 8 + n*dim*4 + 4
	if n <= 0 || dim <= 0 || len(data) < need {
		return nil, nil, nil, fmt.Errorf("файл оборван: n=%d dim=%d, нужно ≥%d байт, есть %d", n, dim, need, len(data))
	}

	trainFlat := unsafe.Slice((*float32)(unsafe.Pointer(&data[off])), n*dim)
	train = make([][]float32, n)
	for i := 0; i < n; i++ {
		train[i] = trainFlat[i*dim : (i+1)*dim]
	}
	off += n * dim * 4

	nTest := int(binary.LittleEndian.Uint32(data[off : off+4]))
	off += 4
	if nTest <= 0 || len(data) < off+nTest*dim*4 {
		return nil, nil, nil, fmt.Errorf("тестовая секция оборвана: nTest=%d, нужно ещё %d байт, есть %d",
			nTest, nTest*dim*4, len(data)-off)
	}
	testFlat := unsafe.Slice((*float32)(unsafe.Pointer(&data[off])), nTest*dim)
	test = make([][]float32, nTest)
	for i := 0; i < nTest; i++ {
		test[i] = testFlat[i*dim : (i+1)*dim]
	}
	off += nTest * dim * 4

	if len(data) < off+4 {
		return nil, nil, nil, fmt.Errorf("нет секции ground truth: файл собран не scripts/convert_dbpedia.py?")
	}
	kgt := int(binary.LittleEndian.Uint32(data[off : off+4]))
	off += 4
	if kgt <= 0 || len(data) < off+nTest*kgt*4 {
		return nil, nil, nil, fmt.Errorf("ground truth оборван: K=%d, нужно ещё %d байт, есть %d", kgt, nTest*kgt*4, len(data)-off)
	}
	gtFlat := unsafe.Slice((*int32)(unsafe.Pointer(&data[off])), nTest*kgt)
	gt = make([][]int32, nTest)
	for i := 0; i < nTest; i++ {
		gt[i] = gtFlat[i*kgt : (i+1)*kgt]
	}
	return train, test, gt, nil
}

func TestDBpedia_RealEmbeddingValidation(t *testing.T) {
	if testing.Short() {
		t.Skip("long: строит 100k×1536 HNSW (float32 и SQ8)")
	}
	train, test, gt, err := loadDBpediaRaw("/tmp/dbpedia100k.bin")
	if err != nil {
		t.Skipf("нет данных: %v (scripts/convert_dbpedia.py)", err)
	}
	dim := len(train[0])
	const (
		K   = 10
		ef  = 128
		W   = 12
		dur = 3 * time.Second
	)

	// recall@K результата (ключи = индексы train) против GT top-K.
	recallAt := func(res []VSearchResult, gtRow []int32) float64 {
		want := make(map[int]struct{}, K)
		for i := 0; i < K && i < len(gtRow); i++ {
			want[int(gtRow[i])] = struct{}{}
		}
		hit := 0
		for i := 0; i < K && i < len(res); i++ {
			id, _ := strconv.Atoi(res[i].Key)
			if _, ok := want[id]; ok {
				hit++
			}
		}
		return float64(hit) / float64(K)
	}

	run := func(name string, useSQ bool) {
		cfg := LeveledConfig{
			Distance:       DotProductDistance, // нормализованные → cosine == dot
			Allocator:      tcmalloc.NewTCMallocStore(8),
			DeltaMax:       len(train) + 1, // один консолидированный сегмент
			Fanout:         4,
			EfConstruction: arEfConstruction,
			EfSearch:       ef,
			UseSQ:          useSQ,
		}
		lvs := NewLeveledVectorStore(cfg)
		t0 := time.Now()
		for i, v := range train {
			if err := lvs.Add(strconv.Itoa(i), v); err != nil {
				t.Fatalf("[%s] Add: %v", name, err)
			}
		}
		settle(lvs)
		buildDur := time.Since(t0)

		st := lvs.Stats()
		nseg := 0
		for _, n := range st.SegmentsByLevel {
			nseg += n
		}

		var recall float64
		for qi, q := range test {
			res, _ := lvs.Search(q, K, nil)
			recall += recallAt(res, gt[qi])
		}
		recall /= float64(len(test))

		qps := runQPS(W, dur, test, func(q []float32) { _, _ = lvs.Search(q, K, nil) })

		fmt.Printf("%-10s recall@%d=%.4f  QPS_%d=%.0f  segs=%d  build=%.1fs  mem=%.0fMB\n",
			name, K, recall, W, qps, nseg, buildDur.Seconds(),
			float64(st.DataBytes+st.AllocatorBytes)/1e6)
	}

	fmt.Println()
	fmt.Printf("DBPEDIA-OPENAI VALIDATION — ada-002 %d-dim, cosine(norm.dot), N=%d, q=%d, ef=%d, K=%d\n",
		dim, len(train), len(test), ef, K)
	fmt.Println("Search() без фильтра; recall vs готовый angular GT. float32 (hnsw flat, dim>256) vs SQ8.")
	fmt.Println("---------------------------------------------------------------------------------------")
	run("float32", false)
	run("SQ8", true)
	fmt.Println("---------------------------------------------------------------------------------------")
	fmt.Println("Ожидание: float32 recall высокий (~0.97+@ef128). SQ8 близко если cosine-ADC корректен;")
	fmt.Println("сильный провал SQ8 = риск isDotDistance (euclidean-ADC на cosine) или предел SQ@dim1536.")
}
