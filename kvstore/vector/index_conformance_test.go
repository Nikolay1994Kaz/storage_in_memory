package vector

// Конформанс-тест интерфейса VectorIndex: один сценарий, обе реализации
// (VectorStore и LeveledVectorStore). Страховка от тихой дивергенции —
// багфикс семантики интерфейса в одной реализации обязан проходить и во
// второй (прецедент: delta post-filter чинился только в Leveled-путях).
//
// Тест проверяет КОНТРАКТ (что видно пользователю интерфейса), а не
// внутренности: recall здесь не меряется, только точная семантика
// Add/Get/Delete/Search/ForEach/Save/Load/Clear на малом N.

import (
	"bytes"
	"fmt"
	"testing"

	"kvstore/kvstore/internal/store/tcmalloc"
)

// indexImpl описывает одну реализацию VectorIndex для конформанс-прогона.
type indexImpl struct {
	name string
	// make создаёт свежий пустой индекс (Euclidean, fp32).
	make func(t *testing.T) VectorIndex
	// syncForSave приводит индекс к состоянию, покрываемому SaveBinary.
	// Часть реального контракта: прод зовёт FlushDeltaSync перед снапшотом
	// Leveled (cmd/kvstore/main.go, saveVectors) — дельта в снапшот не входит.
	syncForSave func(idx VectorIndex)
}

func conformanceImpls() []indexImpl {
	return []indexImpl{
		{
			name: "VectorStore",
			make: func(t *testing.T) VectorIndex {
				return NewVectorStore(EuclideanDistance, tcmalloc.NewTCMallocStore(1))
			},
			syncForSave: func(VectorIndex) {},
		},
		{
			name: "LeveledVectorStore",
			make: func(t *testing.T) VectorIndex {
				lvs := NewLeveledVectorStore(LeveledConfig{
					// Маленькая дельта: сценарий на N=300 проходит и delta-,
					// и frozen-путь (flush срабатывает несколько раз).
					DeltaMax: 64,
					Distance: EuclideanDistance,
				})
				t.Cleanup(lvs.Close)
				return lvs
			},
			syncForSave: func(idx VectorIndex) {
				idx.(*LeveledVectorStore).FlushDeltaSync()
			},
		},
		{
			// Шардированная дельта: flush идёт через freeze-per-shard (каждый
			// шард → свой fp32-мини-сегмент в L0). Контракт обязан совпадать
			// с одношардовым Leveled: та же семантика Add/Get/Delete/Search/
			// Save/Load при другом внутреннем пути публикации. fp32 (не SQ):
			// конформанс сверяет Get побайтно, квантование бы шумело.
			name: "LeveledVectorStore/sharded-freeze",
			make: func(t *testing.T) VectorIndex {
				lvs := NewLeveledVectorStore(LeveledConfig{
					DeltaMax:    64,
					DeltaShards: 4,
					Distance:    EuclideanDistance,
				})
				t.Cleanup(lvs.Close)
				return lvs
			},
			syncForSave: func(idx VectorIndex) {
				idx.(*LeveledVectorStore).FlushDeltaSync()
			},
		},
	}
}

func confKey(i int) string { return fmt.Sprintf("key:%04d", i) }

// assertSorted проверяет, что результаты отсортированы по возрастанию дистанции.
func assertSorted(t *testing.T, res []VSearchResult) {
	t.Helper()
	for i := 1; i < len(res); i++ {
		if res[i].Distance < res[i-1].Distance {
			t.Fatalf("результаты не отсортированы: [%d]=%v < [%d]=%v",
				i, res[i].Distance, i-1, res[i-1].Distance)
		}
	}
}

func TestVectorIndex_Conformance(t *testing.T) {
	const (
		n   = 300
		dim = 8
		k   = 10
	)
	vecs := makeRandVecs(n, dim, 42)

	for _, impl := range conformanceImpls() {
		t.Run(impl.name, func(t *testing.T) {
			idx := impl.make(t)

			// ── Пустой индекс ────────────────────────────────────────────
			res, err := idx.Search(vecs[0], k, nil)
			if err != nil || len(res) != 0 {
				t.Fatalf("Search по пустому индексу: res=%v err=%v, ожидали пусто без ошибки", res, err)
			}
			if got, _ := idx.Get(confKey(0)); got != nil {
				t.Fatalf("Get по пустому индексу вернул вектор")
			}

			// ── Вставка ──────────────────────────────────────────────────
			for i := 0; i < n; i++ {
				if err := idx.Add(confKey(i), vecs[i]); err != nil {
					t.Fatalf("Add(%s): %v", confKey(i), err)
				}
			}
			if err := idx.Add("bad-dim", make([]float32, dim+1)); err == nil {
				t.Fatalf("Add с неверной размерностью обязан вернуть ошибку")
			}
			if count, d, _ := idx.Info(); count != n || d != dim {
				t.Fatalf("Info: count=%d dim=%d, ожидали %d/%d", count, d, n, dim)
			}

			// ── Get ──────────────────────────────────────────────────────
			got, ok := idx.Get(confKey(7))
			if !ok {
				t.Fatalf("Get(%s): не найден", confKey(7))
			}
			for j := range got {
				if got[j] != vecs[7][j] {
					t.Fatalf("Get(%s): вектор искажён на [%d]: %v != %v", confKey(7), j, got[j], vecs[7][j])
				}
			}
			if _, ok := idx.Get("no-such-key"); ok {
				t.Fatalf("Get несуществующего ключа вернул ok")
			}

			// ── Self-search: свой вектор обязан находиться с dist≈0 ─────
			// Ошибка размерности запроса — тоже контракт.
			if _, err := idx.Search(make([]float32, dim+1), k, nil); err == nil {
				t.Fatalf("Search с неверной размерностью обязан вернуть ошибку")
			}
			for i := 0; i < n; i += 13 {
				res, err := idx.Search(vecs[i], k, nil)
				if err != nil {
					t.Fatalf("Search(self %d): %v", i, err)
				}
				assertSorted(t, res)
				found := false
				for _, r := range res {
					if r.Key == confKey(i) {
						found = true
						if r.Distance > 1e-5 {
							t.Fatalf("self-query %s: dist=%v, ожидали ~0", confKey(i), r.Distance)
						}
					}
				}
				if !found {
					t.Fatalf("self-query %s: собственный вектор не найден в top-%d", confKey(i), k)
				}
			}

			// ── SearchFiltered: фильтр обязан соблюдаться ───────────────
			even := func(key string) bool {
				var i int
				fmt.Sscanf(key, "key:%d", &i)
				return i%2 == 0
			}
			fres, err := idx.SearchFiltered(vecs[2], k, even, nil)
			if err != nil {
				t.Fatalf("SearchFiltered: %v", err)
			}
			if len(fres) == 0 {
				t.Fatalf("SearchFiltered: пустой результат")
			}
			assertSorted(t, fres)
			for _, r := range fres {
				if !even(r.Key) {
					t.Fatalf("SearchFiltered: ключ %s не проходит фильтр", r.Key)
				}
			}
			// Self-query проходит фильтр → обязан быть первым с dist≈0.
			if fres[0].Key != confKey(2) || fres[0].Distance > 1e-5 {
				t.Fatalf("SearchFiltered self-query: top1=%s dist=%v, ожидали %s/~0",
					fres[0].Key, fres[0].Distance, confKey(2))
			}

			// ── Delete ───────────────────────────────────────────────────
			if !idx.Delete(confKey(0)) {
				t.Fatalf("Delete(%s): вернул false для живого ключа", confKey(0))
			}
			if idx.Delete(confKey(0)) {
				t.Fatalf("повторный Delete(%s): вернул true", confKey(0))
			}
			if _, ok := idx.Get(confKey(0)); ok {
				t.Fatalf("Get(%s) после Delete вернул ok", confKey(0))
			}
			res, err = idx.Search(vecs[0], k, nil)
			if err != nil {
				t.Fatalf("Search после Delete: %v", err)
			}
			for _, r := range res {
				if r.Key == confKey(0) {
					t.Fatalf("удалённый ключ %s вернулся из Search", confKey(0))
				}
			}
			if count, _, _ := idx.Info(); count != n-1 {
				t.Fatalf("Info после Delete: count=%d, ожидали %d", count, n-1)
			}

			// ── Update: повторный Add того же ключа = замена ────────────
			updated := make([]float32, dim)
			for j := range updated {
				updated[j] = float32(100 + j)
			}
			if err := idx.Add(confKey(1), updated); err != nil {
				t.Fatalf("Add(update %s): %v", confKey(1), err)
			}
			// Контракт Info (см. api.go): update не увеличивает число живых
			// векторов, но LSM-реализация до компакции может временно завышать
			// count на затенённую frozen-копию. Занижаться count не должен.
			if count, _, _ := idx.Info(); count < n-1 || count > n {
				t.Fatalf("Info после update: count=%d, ожидали %d..%d", count, n-1, n)
			}
			got, ok = idx.Get(confKey(1))
			if !ok {
				t.Fatalf("Get(%s) после update: не найден", confKey(1))
			}
			for j := range got {
				if got[j] != updated[j] {
					t.Fatalf("Get(%s) после update: вернулась старая версия ([%d]=%v)", confKey(1), j, got[j])
				}
			}
			// Search по новому вектору обязан найти новую версию с dist≈0.
			res, err = idx.Search(updated, k, nil)
			if err != nil {
				t.Fatalf("Search(updated): %v", err)
			}
			if len(res) == 0 || res[0].Key != confKey(1) || res[0].Distance > 1e-5 {
				t.Fatalf("Search(updated): top1=%v, ожидали %s/~0", res, confKey(1))
			}

			// ── ForEach ──────────────────────────────────────────────────
			seen := make(map[string]bool)
			idx.ForEach(func(key string, vec []float32) {
				if seen[key] {
					t.Fatalf("ForEach: ключ %s посещён дважды", key)
				}
				seen[key] = true
				if len(vec) != dim {
					t.Fatalf("ForEach: len(vec)=%d у %s, ожидали %d", len(vec), key, dim)
				}
			})
			if len(seen) != n-1 {
				t.Fatalf("ForEach: посещено %d ключей, ожидали %d", len(seen), n-1)
			}
			if seen[confKey(0)] {
				t.Fatalf("ForEach: посетил удалённый ключ %s", confKey(0))
			}

			// ── SaveBinary → LoadBinary в свежий инстанс ────────────────
			impl.syncForSave(idx)
			var buf bytes.Buffer
			if err := idx.SaveBinary(&buf); err != nil {
				t.Fatalf("SaveBinary: %v", err)
			}
			fresh := impl.make(t)
			if err := fresh.LoadBinary(bytes.NewReader(buf.Bytes())); err != nil {
				t.Fatalf("LoadBinary: %v", err)
			}
			// Тот же допуск, что после update: затенённая upsert-копия могла
			// уехать в снапшот отдельным сегментом (компакция её схлопнет).
			if count, d, _ := fresh.Info(); count < n-1 || count > n || d != dim {
				t.Fatalf("Info после Load: count=%d dim=%d, ожидали %d..%d/%d", count, d, n-1, n, dim)
			}
			if _, ok := fresh.Get(confKey(0)); ok {
				t.Fatalf("после Load: удалённый ключ %s воскрес", confKey(0))
			}
			got, ok = fresh.Get(confKey(1))
			if !ok {
				t.Fatalf("после Load: Get(%s) не найден", confKey(1))
			}
			for j := range got {
				if got[j] != updated[j] {
					t.Fatalf("после Load: Get(%s) вернул старую версию ([%d]=%v)", confKey(1), j, got[j])
				}
			}
			res, err = fresh.Search(vecs[5], k, nil)
			if err != nil || len(res) == 0 || res[0].Key != confKey(5) || res[0].Distance > 1e-5 {
				t.Fatalf("после Load: self-search %s: res=%v err=%v", confKey(5), res, err)
			}

			// ── Clear: полный сброс, включая размерность ────────────────
			idx.Clear()
			if count, _, _ := idx.Info(); count != 0 {
				t.Fatalf("Info после Clear: count=%d", count)
			}
			res, err = idx.Search(vecs[0], k, nil)
			if err != nil || len(res) != 0 {
				t.Fatalf("Search после Clear: res=%v err=%v", res, err)
			}
			// Clear сбрасывает dim → индекс принимает другую размерность.
			if err := idx.Add("re:0", make([]float32, dim/2)); err != nil {
				t.Fatalf("Add после Clear с новой размерностью: %v", err)
			}
			if count, d, _ := idx.Info(); count != 1 || d != dim/2 {
				t.Fatalf("Info после Clear+Add: count=%d dim=%d, ожидали 1/%d", count, d, dim/2)
			}
		})
	}
}
