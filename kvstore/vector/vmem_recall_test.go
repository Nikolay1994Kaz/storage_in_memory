package vector

// Тесты шага 3 VMEM-спринта: white-box кухни фильтра RECALL (три режима
// валидности = один Filter), вырождение в BM25-only без вектора запроса и
// регресс filter-then-fuse — маленький scope не голодает рядом с большим.
// Паритет с моделью — TestVMEMOracleParity (vmem_oracle_test.go).

import (
	"fmt"
	"reflect"
	"testing"
)

func TestRecallFilterModes(t *testing.T) {
	const now = int64(1_753_000_000)
	base := RecallRequest{Scope: "user:dana", Query: "алматы", K: 10}
	sent := float64(vmemOpenValidTo)

	t.Run("дефолт = валидное-сейчас", func(t *testing.T) {
		f, err := recallFilter(base, now)
		if err != nil {
			t.Fatalf("recallFilter: %v", err)
		}
		wantEq := map[string]string{vmemAttrScope: "user:dana"}
		wantRange := map[string][2]float64{
			vmemAttrExpiresAt: {float64(now + 1), sent}, // expires_at > now
			vmemAttrValidFrom: {0, float64(now)},        // valid_from <= now
			vmemAttrValidTo:   {float64(now + 1), sent}, // now < valid_to
		}
		if !reflect.DeepEqual(f.Eq, wantEq) || !reflect.DeepEqual(f.Range, wantRange) {
			t.Errorf("фильтр = %+v, ожидалось Eq=%v Range=%v", f, wantEq, wantRange)
		}
	})

	t.Run("as_of подменяет только ось валидности", func(t *testing.T) {
		ts := int64(500)
		req := base
		req.AsOf = &ts
		f, err := recallFilter(req, now)
		if err != nil {
			t.Fatalf("recallFilter: %v", err)
		}
		if got := f.Range[vmemAttrValidFrom]; got != [2]float64{0, 500} {
			t.Errorf("valid_from = %v", got)
		}
		if got := f.Range[vmemAttrValidTo]; got != [2]float64{501, sent} {
			t.Errorf("valid_to = %v", got)
		}
		// Erasure судится против now, не against ts: право быть забытым
		// побеждает машину времени.
		if got := f.Range[vmemAttrExpiresAt]; got != [2]float64{float64(now + 1), sent} {
			t.Errorf("expires_at = %v, ожидалась граница от now", got)
		}
	})

	t.Run("all снимает интервал, но не erasure", func(t *testing.T) {
		req := base
		req.All = true
		f, err := recallFilter(req, now)
		if err != nil {
			t.Fatalf("recallFilter: %v", err)
		}
		if _, ok := f.Range[vmemAttrValidFrom]; ok {
			t.Error("all оставил Range по valid_from")
		}
		if _, ok := f.Range[vmemAttrValidTo]; ok {
			t.Error("all оставил Range по valid_to")
		}
		if got := f.Range[vmemAttrExpiresAt]; got != [2]float64{float64(now + 1), sent} {
			t.Errorf("expires_at = %v — erasure обязан фильтроваться и в all", got)
		}
	})

	t.Run("type_eq добавляет Eq", func(t *testing.T) {
		req := base
		req.TypeEq = "preference"
		f, err := recallFilter(req, now)
		if err != nil {
			t.Fatalf("recallFilter: %v", err)
		}
		if f.Eq[vmemAttrType] != "preference" {
			t.Errorf("Eq = %v", f.Eq)
		}
	})

	t.Run("валидация", func(t *testing.T) {
		ts := int64(500)
		bad := int64(-1)
		cases := []struct {
			name string
			req  RecallRequest
			now  int64
			want error
		}{
			{"пустой scope", RecallRequest{Query: "q", K: 1}, now, ErrVMEMScope},
			{"пустой query", RecallRequest{Scope: "s", K: 1}, now, ErrVMEMQuery},
			{"K=0", RecallRequest{Scope: "s", Query: "q"}, now, ErrVMEMK},
			{"as_of+all", RecallRequest{Scope: "s", Query: "q", K: 1, AsOf: &ts, All: true}, now, ErrVMEMAsOf},
			{"as_of<=0", RecallRequest{Scope: "s", Query: "q", K: 1, AsOf: &bad}, now, ErrVMEMAsOf},
			{"now=0", RecallRequest{Scope: "s", Query: "q", K: 1}, 0, ErrVMEMNow},
		}
		for _, tc := range cases {
			if _, err := recallFilter(tc.req, tc.now); err != tc.want {
				t.Errorf("%s: err = %v, ожидалось %v", tc.name, err, tc.want)
			}
		}
	})
}

// TestRecallSmallScopeNoStarvation — регресс filter-then-fuse: рядом с большим
// чужим scope (300 доков, все матчатся запросу лексически и лежат вплотную к
// запросному вектору) маленький scope из 2 фактов обязан вернуть ОБА. При
// пост-фьюжн отсеве top-100 каждого плеча был бы занят чужими доками, и
// маленькому scope достался бы огрызок или пусто.
func TestRecallSmallScopeNoStarvation(t *testing.T) {
	const dim = 8
	lvs := NewLeveledVectorStore(bm25TestConfig())
	defer lvs.Close()

	qvec := mkVecN(dim, 500)
	// Большой чужой scope: лексически и векторно доминирует оба плеча.
	for i := 0; i < 300; i++ {
		req := RememberRequest{
			ID:     fmt.Sprintf("big:%d", i),
			Scope:  "user:erlan",
			Text:   "командировка в алматы отменена",
			Vector: mkVecN(dim, 500+float32(i)*0.001),
		}
		if _, err := lvs.Remember(req, 100); err != nil {
			t.Fatalf("Remember big:%d: %v", i, err)
		}
	}
	// Маленький целевой scope: 2 матчащихся факта с далёкими векторами.
	for i := 0; i < 2; i++ {
		req := RememberRequest{
			ID:     fmt.Sprintf("small:%d", i),
			Scope:  "user:dana",
			Text:   "переезд в алматы состоялся",
			Vector: mkVecN(dim, float32(i)),
		}
		if _, err := lvs.Remember(req, 100); err != nil {
			t.Fatalf("Remember small:%d: %v", i, err)
		}
	}

	for _, state := range []string{"delta", "flushed"} {
		if state == "flushed" {
			lvs.FlushDeltaSync()
		}
		res, err := lvs.Recall(RecallRequest{
			Scope: "user:dana", Query: "алматы", Vector: qvec, K: 10,
		}, 200)
		if err != nil {
			t.Fatalf("%s: Recall: %v", state, err)
		}
		found := map[string]bool{}
		for _, r := range res {
			if r.Key != "small:0" && r.Key != "small:1" {
				t.Errorf("%s: чужой факт %q в выдаче Даны", state, r.Key)
			}
			found[r.Key] = true
		}
		if !found["small:0"] || !found["small:1"] {
			t.Errorf("%s: маленький scope голодает: выдача %v", state, res)
		}
	}
}

// TestRecallDegradesToBM25 — без вектора запроса Recall идёт BM25-only:
// шумовое плечо из placeholder-векторов не голосует в RRF.
func TestRecallDegradesToBM25(t *testing.T) {
	lvs := NewLeveledVectorStore(bm25TestConfig())
	defer lvs.Close()
	// Ступень 0: факты без векторов (placeholder из id).
	if _, err := lvs.Remember(RememberRequest{ID: "f1", Scope: "s", Text: "кофе без сахара"}, 100); err != nil {
		t.Fatalf("Remember: %v", err)
	}
	if _, err := lvs.Remember(RememberRequest{ID: "f2", Scope: "s", Text: "переезд в астану"}, 100); err != nil {
		t.Fatalf("Remember: %v", err)
	}
	res, err := lvs.Recall(RecallRequest{Scope: "s", Query: "кофе", K: 10}, 200)
	if err != nil {
		t.Fatalf("Recall: %v", err)
	}
	if len(res) != 1 || res[0].Key != "f1" {
		t.Fatalf("BM25-only выдача: %+v, ожидался только f1", res)
	}
}
