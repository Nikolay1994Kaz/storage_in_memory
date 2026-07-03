package vector

import (
	"strconv"
	"testing"

	"kvstore/kvstore/internal/store/tcmalloc"
)

// TestSearchFilter_NoPartition_SurvivesFlush — страж #3.
//
// Репро дефекта: без PartitionAttr/TenantOf колоночный attr-слой строился только
// при партиции. AddWithAttrs честно клал attrs в дельту, SearchFilter на свежих
// данных работал — но после первого flush frozen-сегмент строился БЕЗ attrs, и
// SearchFilter молча возвращал пусто. Результат фильтра менялся после flush —
// тихий фугас.
//
// Контракт после фикса: колонки строятся всегда при наличии атрибутов; результат
// SearchFilter одинаков ДО и ПОСЛЕ flush.
func TestSearchFilter_NoPartition_SurvivesFlush(t *testing.T) {
	const dim = 8
	// ВАЖНО: без PartitionAttr и без TenantOf — прежде это роняло attrs на flush.
	lvs := NewLeveledVectorStore(LeveledConfig{
		Distance:       EuclideanDistance,
		Allocator:      tcmalloc.NewTCMallocStore(2),
		DeltaMax:       1000,
		Fanout:         8,
		EfSearch:       64,
		M:              16,
		EfConstruction: 100,
	})
	defer lvs.Clear()

	// 30 векторов, половина color=red, половина color=blue.
	const N = 30
	for i := 0; i < N; i++ {
		color := "red"
		if i%2 == 1 {
			color = "blue"
		}
		attrs := Attributes{
			Cat: map[string]string{"color": color},
			Num: map[string]float64{"price": float64(i)},
		}
		if err := lvs.AddWithAttrs("k"+strconv.Itoa(i), mkVecN(dim, float32(i)), attrs); err != nil {
			t.Fatalf("AddWithAttrs: %v", err)
		}
	}

	countReds := func(tag string) int {
		res, err := lvs.SearchFilter(mkVecN(dim, 0), N, Filter{Eq: map[string]string{"color": "red"}})
		if err != nil {
			t.Fatalf("%s SearchFilter: %v", tag, err)
		}
		for _, r := range res {
			// каждый вернувшийся ключ обязан быть red (чётный индекс)
			idx, _ := strconv.Atoi(r.Key[1:])
			if idx%2 != 0 {
				t.Fatalf("%s: SearchFilter вернул не-red ключ %q", tag, r.Key)
			}
		}
		return len(res)
	}

	before := countReds("до flush")
	if before == 0 {
		t.Fatal("до flush фильтр не вернул ни одного red — тест сломан")
	}

	lvs.FlushDeltaSync()

	after := countReds("после flush")
	if after != before {
		t.Fatalf("после flush фильтр вернул %d red вместо %d — attrs потеряны на flush", after, before)
	}

	// Числовой диапазон тоже должен пережить flush.
	rng, err := lvs.SearchFilter(mkVecN(dim, 0), N, Filter{Range: map[string][2]float64{"price": {0, 5}}})
	if err != nil {
		t.Fatalf("Range SearchFilter: %v", err)
	}
	if len(rng) == 0 {
		t.Fatal("после flush числовой фильтр price∈[0,5] вернул пусто — числовые attrs потеряны")
	}
	for _, r := range rng {
		idx, _ := strconv.Atoi(r.Key[1:])
		if idx < 0 || idx > 5 {
			t.Fatalf("Range вернул ключ %q вне [0,5]", r.Key)
		}
	}
}
