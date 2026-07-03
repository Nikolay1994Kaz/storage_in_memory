package vector

import (
	"strconv"
	"strings"
	"testing"

	"kvstore/kvstore/internal/store/tcmalloc"
)

// TestTenant_NoAttrSegmentLeak — страж изоляции тенантов при СМЕШАННОЙ истории
// ингеста. Если часть векторов вставлена БЕЗ атрибутов (голый Add), они образуют
// сегмент без колоночного слоя (sa==nil, buildSegmentAttrs→nil). SearchFilter по
// partitionAttr на таком сегменте молча делал полный обход: compile пропускал
// партиционный атрибут (skipAttr) ДО nil-чека → predIdx=always-true, matchable=true,
// а блок партиционного роутинга требовал sa!=nil и потому пропускался → сегмент
// возвращал ВСЕ свои векторы вместо пустоты. Утечка чужих данных в тенант-запрос.
//
// Корректно: партиционный фильтр задан, но сегмент не может его выполнить (нет
// каталога/атрибутов) → тенанта в этом сегменте нет → пусто.
//
// dim=8 бьёт путь frozenSegment (searchFilterFrozen), dim=300 — hnswSegment
// (собственный SearchFilter); оба имели идентичный баг.
func TestTenant_NoAttrSegmentLeak(t *testing.T) {
	for _, dim := range []int{8, 300} {
		t.Run("dim"+strconv.Itoa(dim), func(t *testing.T) {
			cfg := LeveledConfig{
				Distance:      EuclideanDistance,
				Allocator:     tcmalloc.NewTCMallocStore(4),
				DeltaMax:      10000, // большая дельта → один flush = один сегмент, без авто-merge
				Fanout:        4,
				EfSearch:      64,
				PartitionAttr: "tenant",
			}
			lvs := NewLeveledVectorStore(cfg)

			q := make([]float32, dim) // запрос = нули

			// Батч 1: БЕЗ атрибутов, идентичны запросу (дистанция 0 → заведомо топ,
			// если протекут). Формируют сегмент без атрибутов (sa==nil).
			const nLeak = 20
			for i := 0; i < nLeak; i++ {
				if err := lvs.Add("leak"+strconv.Itoa(i), make([]float32, dim)); err != nil {
					t.Fatalf("Add: %v", err)
				}
			}
			lvs.FlushDeltaSync()

			// Батч 2: тенант t1, чуть дальше от запроса.
			for i := 0; i < 20; i++ {
				v := make([]float32, dim)
				for d := range v {
					v[d] = 1
				}
				if err := lvs.AddWithAttrs("t1-"+strconv.Itoa(i), v, Attributes{
					Cat: map[string]string{"tenant": "t1"},
				}); err != nil {
					t.Fatalf("AddWithAttrs: %v", err)
				}
			}
			lvs.FlushDeltaSync()

			// Тенант-запрос обязан вернуть ТОЛЬКО t1-*, никаких leak-*.
			res, err := lvs.SearchFilter(q, nLeak, Filter{Eq: map[string]string{"tenant": "t1"}})
			if err != nil {
				t.Fatalf("SearchFilter: %v", err)
			}
			for _, r := range res {
				if strings.HasPrefix(r.Key, "leak") {
					t.Fatalf("УТЕЧКА ТЕНАНТА: запрос tenant=t1 вернул безатрибутный ключ %q "+
						"(сегмент без атрибутов протёк через полный обход)", r.Key)
				}
			}
		})
	}
}
