package vector

import (
	"math/rand"
	"runtime"
	"strconv"
	"testing"
)

// TestMergeReleasesGraphMemory — память кучи не растёт с числом слияний при
// постоянном наборе ключей.
//
// Регресс: связи временного графа, который слияние строит и замораживает в
// сегмент, жили в пуловых аллокаторах LeveledVectorStore (свой на каждый
// уровень), а их никто не очищал. Граф выброшен — блоки остались. Замер на
// исходном коде, тот же сценарий: heap 105,1 → 123,0 МБ между раундами 3 и 9
// (+18 МБ при неизменных 2000 векторах), рост линейный, без потолка. Теперь
// связи в арене самого графа и уходят вместе с ним: +1 МБ за то же окно.
func TestMergeReleasesGraphMemory(t *testing.T) {
	const dim, live, rounds = 32, 2000, 10
	lvs := NewLeveledVectorStore(LeveledConfig{
		DeltaMax:       500,
		Fanout:         2, // слияние на каждые два сегмента уровня
		M:              16,
		EfConstruction: 64,
		EfSearch:       64,
		Distance:       EuclideanDistance,
	})
	defer lvs.Close()

	rng := rand.New(rand.NewSource(1))
	vec := make([]float32, dim)
	heap := make([]uint64, rounds)
	for r := 0; r < rounds; r++ {
		// Upsert тех же ключей: живых векторов всегда live, а слияния идут.
		for i := 0; i < live; i++ {
			for j := range vec {
				vec[j] = rng.Float32()
			}
			if err := lvs.Add("k"+strconv.Itoa(i), vec); err != nil {
				t.Fatal(err)
			}
		}
		settle(lvs)
		runtime.GC()
		var ms runtime.MemStats
		runtime.ReadMemStats(&ms)
		heap[r] = ms.HeapAlloc
	}
	growth := int64(heap[9]) - int64(heap[3])
	t.Logf("heap: раунд 3 = %.1f МБ, раунд 9 = %.1f МБ, рост %.1f МБ",
		float64(heap[3])/1e6, float64(heap[9])/1e6, float64(growth)/1e6)
	if growth > 8<<20 {
		t.Fatalf("за 6 раундов слияний heap вырос на %.1f МБ при постоянных %d векторах — связи графов не освобождаются",
			float64(growth)/1e6, live)
	}
}
