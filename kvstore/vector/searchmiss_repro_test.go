package vector

// Диагностический репро searchmiss из soak (run 28854588610: searchmiss=7/400
// на обоих чекпойнтах). Реплицирует seed-пайплайн soak_oracle in-process:
// те же векторы (NormFloat64 → FormatFloat 6 знаков → ParseFloat32), те же
// ключи/удаления, PartitionAttr=tenant, конфиг сервера (M=32/efC=400/ef=100,
// fp32). Затем фаза «нагрузки» (батчи+flush+merge), затем Save/Load-раундтрип.
// На каждой стадии — self-query всех живых оракул-ключей: точка с dist=0
// обязана быть в top-10.

import (
	"bytes"
	"fmt"
	"math/rand"
	"strconv"
	"testing"
)

// oracleVecRepro — копия soak_oracle.vecFor вплоть до float-раундтрипа через строку.
func oracleVecRepro(rngSeed int64, i, dim int) []float32 {
	rng := rand.New(rand.NewSource(rngSeed + int64(i)))
	out := make([]float32, dim)
	for d := 0; d < dim; d++ {
		s := strconv.FormatFloat(rng.NormFloat64(), 'f', 6, 32)
		f, _ := strconv.ParseFloat(s, 32)
		out[d] = float32(f)
	}
	return out
}

func oracleSelfQueryMisses(t *testing.T, lvs *LeveledVectorStore, n, dim int, stage string) []string {
	t.Helper()
	var missed []string
	for i := 0; i < n; i++ {
		if i%5 == 0 {
			continue // удалён при seed
		}
		key := fmt.Sprintf("oracle:vec:%d", i)
		q := oracleVecRepro(42, i, dim)
		res, err := lvs.Search(q, 10, nil)
		if err != nil {
			t.Fatalf("%s: Search(%s): %v", stage, key, err)
		}
		found := false
		for _, r := range res {
			if r.Key == key {
				found = true
				break
			}
		}
		if !found {
			missed = append(missed, key)
		}
	}
	return missed
}

func TestSearchmiss_OracleRepro(t *testing.T) {
	const (
		dim         = 128
		n           = 500
		tnTenants   = 2
		tnPerTenant = 40
	)

	builderVariants := []int{2, 6}
	trials := 5
	if testing.Short() {
		builderVariants = []int{2} // лёгкий прогон: одна конфигурация, два трайла (~2с)
		trials = 2
	}
	for _, builders := range builderVariants {
		for trial := 0; trial < trials; trial++ {
			t.Run(fmt.Sprintf("builders=%d/trial=%d", builders, trial), func(t *testing.T) {
				lvs := NewLeveledVectorStore(LeveledConfig{
					M:              32,
					EfConstruction: 400,
					EfSearch:       100,
					Distance:       EuclideanDistance,
					PartitionAttr:  "tenant",
					NumBuilders:    builders,
				})
				defer lvs.Close()

				// === seed (как soak_oracle) ===
				for i := 0; i < n; i++ {
					if err := lvs.Add(fmt.Sprintf("oracle:vec:%d", i), oracleVecRepro(42, i, dim)); err != nil {
						t.Fatalf("Add: %v", err)
					}
				}
				for i := 0; i < n; i += 5 {
					lvs.Delete(fmt.Sprintf("oracle:vec:%d", i))
				}
				for tn := 0; tn < tnTenants; tn++ {
					for i := 0; i < tnPerTenant; i++ {
						err := lvs.AddWithAttrs(
							fmt.Sprintf("oracle:tn:t%d:%d", tn, i),
							oracleVecRepro(42+int64(1000*(tn+1)), i, dim),
							Attributes{Cat: map[string]string{"tenant": fmt.Sprintf("t%d", tn)}},
						)
						if err != nil {
							t.Fatalf("AddWithAttrs: %v", err)
						}
					}
				}
				lvs.FlushDeltaSync() // VSIM.INFO в seed

				missSeed := oracleSelfQueryMisses(t, lvs, n, dim, "after-seed")

				// === фаза «нагрузки»: чужие векторы, флаши, merge в L1 ===
				// stress_test добавляет vec:* и зовёт VSIM.INFO (=flush); к чекпойнту
				// A были L0+L1 → оракул-сегмент прошёл merge. Воспроизводим: 5 батчей
				// по 300 с flush после каждого → fanout=4 гарантирует L0→L1 merge.
				//
				// КРИТИЧНО: векторы фазы — uniform [0,1), как rng.Float32() в
				// stress_test. Датасет становится двухоболочечным (gaussian |v|≈11.3
				// + uniform-шар вокруг (0.5,...)): все uniform-точки ближе к любому
				// гауссову запросу (~171²), чем его собственные соседи (~196²+), и
				// beam self-query засасывается в uniform-кластер. При блочном порядке
				// вставки merge (сначала все гауссианы, потом uniform) у слабых
				// гауссовых вершин нет in-рёбер из uniform-кластера → детерминированный
				// searchmiss (soak: 7/400). С гауссовой фазой не воспроизводится.
				phaseRng := rand.New(rand.NewSource(777))
				id := 0
				for batch := 0; batch < 5; batch++ {
					for j := 0; j < 300; j++ {
						vec := make([]float32, dim)
						for d := range vec {
							vec[d] = phaseRng.Float32()
						}
						if err := lvs.Add(fmt.Sprintf("vec:%d", id), vec); err != nil {
							t.Fatalf("Add(load): %v", err)
						}
						// как в стрессе: часть ключей удаляется → tombstones
						if id%7 == 0 {
							lvs.Delete(fmt.Sprintf("vec:%d", id))
						}
						id++
					}
					lvs.FlushDeltaSync()
				}

				missPhase := oracleSelfQueryMisses(t, lvs, n, dim, "after-phase")

				// === рестарт: Save → Load в свежий инстанс ===
				var buf bytes.Buffer
				if err := lvs.SaveBinary(&buf); err != nil {
					t.Fatalf("SaveBinary: %v", err)
				}
				fresh := NewLeveledVectorStore(LeveledConfig{
					M:              32,
					EfConstruction: 400,
					EfSearch:       100,
					Distance:       EuclideanDistance,
					PartitionAttr:  "tenant",
					NumBuilders:    builders,
				})
				defer fresh.Close()
				if err := fresh.LoadBinary(bytes.NewReader(buf.Bytes())); err != nil {
					t.Fatalf("LoadBinary: %v", err)
				}
				missReload := oracleSelfQueryMisses(t, fresh, n, dim, "after-reload")

				t.Logf("miss: seed=%d phase=%d reload=%d | seedKeys=%v phaseKeys=%v reloadKeys=%v",
					len(missSeed), len(missPhase), len(missReload), missSeed, missPhase, missReload)

				// Диагностика остаточных промахов: сегмент, K-лестница.
				for _, key := range missReload {
					var i int
					fmt.Sscanf(key, "oracle:vec:%d", &i)
					q := oracleVecRepro(42, i, dim)
					for _, K := range []int{10, 100, 500} {
						res, _ := fresh.Search(q, K, nil)
						rank := -1
						for r := range res {
							if res[r].Key == key {
								rank = r
								break
							}
						}
						t.Logf("  diag %s: K=%d rank=%d (of %d)", key, K, rank, len(res))
					}
					for lvl := range fresh.levels {
						for si, seg := range fresh.levels[lvl] {
							if seg.HasKey(key) {
								t.Logf("  diag %s: живёт в L%d[%d] type=%T n=%d", key, lvl, si, seg, seg.Len())
							}
						}
					}
				}

				if len(missSeed)+len(missPhase)+len(missReload) > 0 {
					t.Fatalf("self-query miss: seed=%d phase=%d reload=%d — регрессия достижимости "+
						"(шафл вставки в buildSegment / RepairReachability)", len(missSeed), len(missPhase), len(missReload))
				}
			})
		}
	}
}
