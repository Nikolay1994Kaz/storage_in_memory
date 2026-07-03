package vector

import (
	"math/rand"
	"sync"
	"testing"

	"kvstore/kvstore/internal/store/tcmalloc"
)

// TestLSH_ConcurrentSearchNoRace — страж гонки на LSH-буфере кандидатов.
// SearchWithLSH берёт vs.mu.RLock (конкурентно) и звал FindCandidates, который
// мутировал ОБЩИЙ idx.candidateBuf → параллельные поиски топтали чужой буфер
// (data race + порча выдачи). Теперь буфер локальный per-call. Тест ловит регресс
// под `go test -race`.
func TestLSH_ConcurrentSearchNoRace(t *testing.T) {
	const dim = 256 // LSH инициализируется только при dim >= 256
	vs := NewVectorStoreCosine(tcmalloc.NewTCMallocStore(1))
	vs.SetUseLSH(true)

	rng := rand.New(rand.NewSource(7))
	mkVec := func() []float32 {
		v := make([]float32, dim)
		for i := range v {
			v[i] = rng.Float32()
		}
		return v
	}

	queries := make([][]float32, 8)
	for i := range queries {
		queries[i] = mkVec()
	}
	for i := 0; i < 500; i++ {
		if err := vs.Add("k"+string(rune('A'+i%26))+string(rune('0'+i/26)), mkVec()); err != nil {
			t.Fatalf("Add: %v", err)
		}
	}

	var wg sync.WaitGroup
	for g := 0; g < 8; g++ {
		wg.Add(1)
		go func(q []float32) {
			defer wg.Done()
			for i := 0; i < 200; i++ {
				if _, err := vs.SearchWithLSH(q, 10, 4, nil); err != nil {
					t.Errorf("SearchWithLSH: %v", err)
					return
				}
			}
		}(queries[g])
	}
	wg.Wait()
}
