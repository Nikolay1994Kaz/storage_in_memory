package vector

// Тесты шага 7 BM25-спринта: RRF-слияние SearchHybrid. Оба входных пути
// (SearchText, Search) покрыты своими сьютами — здесь пиннится только
// механика слияния: консенсус, детерминизм tie-break, деградация без термов.

import (
	"math"
	"testing"
)

// hybridEnv — три дока с управляемыми рангами (dim 8, euclidean, запрос-вектор
// нулевой → векторный ранг = |значение| заливки).
//
//	текстовый список по «чай»: [y (tf=3), x (tf=1)]; z не матчится
//	векторный список:          [z (1.0), x (2.0), y (3.0)]
func hybridEnv(t *testing.T) *LeveledVectorStore {
	t.Helper()
	lvs := NewLeveledVectorStore(bm25TestConfig())
	t.Cleanup(lvs.Clear)
	for _, d := range []struct {
		key, text string
		fill      float32
	}{
		{"x", "чай напиток", 2},
		{"y", "чай чай чай", 3},
		{"z", "молоко", 1},
	} {
		if err := lvs.AddDoc(d.key, mkVecN(8, d.fill), Attributes{}, d.text); err != nil {
			t.Fatalf("AddDoc %s: %v", d.key, err)
		}
	}
	return lvs
}

// Консенсус бьёт уверенность одного: z — победитель векторного списка, но без
// текстового матча проигрывает обоим докам, найденным двумя путями. Полный
// порядок детерминирован: y{1,3} > x{2,2} (выпуклость 1/61+1/63 > 2/62) > z{1}.
func TestHybrid_ConsensusBeatsSingleList(t *testing.T) {
	lvs := hybridEnv(t)
	res, err := lvs.SearchHybrid("чай", mkVecN(8, 0), 10)
	if err != nil {
		t.Fatalf("SearchHybrid: %v", err)
	}
	wantOrder := []string{"y", "x", "z"}
	if len(res) != len(wantOrder) {
		t.Fatalf("len=%d, ждали %d: %+v", len(res), len(wantOrder), res)
	}
	for i, w := range wantOrder {
		if res[i].Key != w {
			t.Fatalf("порядок[%d]=%s, ждали %s (весь: %+v)", i, res[i].Key, w, res)
		}
	}
	// Скоры — точные RRF-суммы (ранги целые, арифметика воспроизводима бит-в-бит).
	wantScores := []float64{1.0/61 + 1.0/63, 2.0 / 62, 1.0 / 61}
	for i, w := range wantScores {
		if math.Abs(res[i].Score-w) > 1e-15 {
			t.Fatalf("score[%s]=%v, ждали %v", res[i].Key, res[i].Score, w)
		}
	}
}

// Симметричные ранги {1,2} и {2,1} дают равные RRF-суммы — порядок обязан
// решаться tie-break'ом по ключу, не случайностью обхода map.
func TestHybrid_TieBreakByKey(t *testing.T) {
	lvs := NewLeveledVectorStore(bm25TestConfig())
	t.Cleanup(lvs.Clear)
	// текст: b (tf=2) ранг 1, a ранг 2; вектор: a (1.0) ранг 1, b (2.0) ранг 2.
	if err := lvs.AddDoc("b", mkVecN(8, 2), Attributes{}, "чай чай"); err != nil {
		t.Fatalf("AddDoc b: %v", err)
	}
	if err := lvs.AddDoc("a", mkVecN(8, 1), Attributes{}, "чай"); err != nil {
		t.Fatalf("AddDoc a: %v", err)
	}
	for range 5 { // map-обход рандомизирован — гоняем слияние несколько раз
		res, err := lvs.SearchHybrid("чай", mkVecN(8, 0), 10)
		if err != nil {
			t.Fatalf("SearchHybrid: %v", err)
		}
		if len(res) != 2 || res[0].Key != "a" || res[1].Key != "b" {
			t.Fatalf("ждали [a b] по tie-break, got %+v", res)
		}
		if res[0].Score != res[1].Score {
			t.Fatalf("суммы обязаны быть равны: %v vs %v", res[0].Score, res[1].Score)
		}
	}
}

// Пустой текстовый запрос (нет термов) → деградация до векторного порядка
// в RRF-шкале; K режет выдачу.
func TestHybrid_DegradesToVectorAndTruncates(t *testing.T) {
	lvs := hybridEnv(t)
	res, err := lvs.SearchHybrid("", mkVecN(8, 0), 10)
	if err != nil {
		t.Fatalf("SearchHybrid: %v", err)
	}
	wantOrder := []string{"z", "x", "y"} // чисто векторные ранги
	if len(res) != 3 {
		t.Fatalf("len=%d: %+v", len(res), res)
	}
	for i, w := range wantOrder {
		if res[i].Key != w {
			t.Fatalf("порядок[%d]=%s, ждали %s", i, res[i].Key, w)
		}
		if want := 1.0 / float64(61+i); math.Abs(res[i].Score-want) > 1e-15 {
			t.Fatalf("score[%d]=%v, ждали %v", i, res[i].Score, want)
		}
	}
	top1, err := lvs.SearchHybrid("чай", mkVecN(8, 0), 1)
	if err != nil {
		t.Fatalf("SearchHybrid K=1: %v", err)
	}
	if len(top1) != 1 || top1[0].Key != "y" {
		t.Fatalf("K=1: ждали [y], got %+v", top1)
	}
}
