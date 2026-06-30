package vector

import (
	"math/rand"
	"testing"
)

// TestSerializeVectorWithAttrs_RoundTrip — базовый round-trip формата WAL-операции
// OpVSimAddAttrs (P0-4): вектор и атрибуты переживают сериализацию без потерь.
func TestSerializeVectorWithAttrs_RoundTrip(t *testing.T) {
	vec := []float32{1.5, -2.25, 3, 42, 0, -0.001}
	attrs := Attributes{
		Cat: map[string]string{"tenant": "acme", "lang": "ru"},
		Num: map[string]float64{"price": 19.99, "year": 2026},
	}

	vec2, a2, err := DeserializeVectorWithAttrs(SerializeVectorWithAttrs(vec, attrs))
	if err != nil {
		t.Fatalf("DeserializeVectorWithAttrs: %v", err)
	}

	if len(vec2) != len(vec) {
		t.Fatalf("vec len: got %d, want %d", len(vec2), len(vec))
	}
	for i := range vec {
		if vec2[i] != vec[i] {
			t.Fatalf("vec[%d]: got %v, want %v", i, vec2[i], vec[i])
		}
	}
	for k, v := range attrs.Cat {
		if a2.Cat[k] != v {
			t.Fatalf("Cat[%q]: got %q, want %q", k, a2.Cat[k], v)
		}
	}
	for k, v := range attrs.Num {
		if a2.Num[k] != v {
			t.Fatalf("Num[%q]: got %v, want %v", k, a2.Num[k], v)
		}
	}
}

// TestWALAttrs_SurviveReplay — сквозной страж P0-4: атрибуты, проехавшие через
// сериализацию WAL (write-путь) и десериализацию (replay) → AddWithAttrs, остаются
// queryable через SearchFilter. До фикса replay звал голый Add и терял attrs/tenant.
func TestWALAttrs_SurviveReplay(t *testing.T) {
	const dim = 16
	rng := rand.New(rand.NewSource(20260630))
	mk := func() []float32 {
		v := make([]float32, dim)
		for i := range v {
			v[i] = rng.Float32()
		}
		return v
	}

	lvs := NewLeveledVectorStore(LeveledConfig{
		Distance:      EuclideanDistance,
		DeltaMax:      200, // мелкая дельта → flush+merge, проверяем выживание attrs
		Fanout:        4,
		EfSearch:      64,
		PartitionAttr: "tenant",
	})
	defer lvs.Close()

	acme := make(map[string]bool)
	// replay вектора через тот же путь сериализации, что и WAL (write→WAL→replay).
	replayAdd := func(key, tenant string) {
		blob := SerializeVectorWithAttrs(mk(), Attributes{Cat: map[string]string{"tenant": tenant}})
		v2, a2, err := DeserializeVectorWithAttrs(blob)
		if err != nil {
			t.Fatalf("replay decode %s: %v", key, err)
		}
		if err := lvs.AddWithAttrs(key, v2, a2); err != nil {
			t.Fatalf("AddWithAttrs %s: %v", key, err)
		}
	}

	for i := 0; i < 300; i++ {
		key := itoa(i)
		if i%2 == 0 {
			replayAdd(key, "acme")
			acme[key] = true
		} else {
			replayAdd(key, "globex")
		}
	}
	settle(lvs)

	q := mk()
	res, err := lvs.SearchFilter(q, 10, Filter{Eq: map[string]string{"tenant": "acme"}})
	if err != nil {
		t.Fatalf("SearchFilter: %v", err)
	}
	if len(res) == 0 {
		t.Fatal("P0-4: фильтр по восстановленному tenant=acme не вернул ничего — attrs потеряны при replay")
	}
	for _, r := range res {
		if !acme[r.Key] {
			t.Fatalf("P0-4: в результатах tenant=acme чужой ключ %q — атрибут не сохранился", r.Key)
		}
	}
}
