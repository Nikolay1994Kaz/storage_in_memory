package vector

// Тесты двух улучшений по итогам эксперимента 19.07
// (bm25_boost_experiment_test.go): буст заголовка и df-отсечение запроса.

import (
	"fmt"
	"strconv"
	"testing"
)

// Буст заголовка — чистая арифметика tf: терм заголовка получает tf×W,
// доклен растёт на W×len(title); текст без заголовка не меняется вовсе.
func TestTokenizeDocTitled_TFMath(t *testing.T) {
	got := map[string]uint16{}
	for _, tt := range TokenizeDocTitled("чай", "чай напиток") {
		got[tt.Term] = tt.TF
	}
	// «чай» = 3 повтора заголовка + 1 в тексте; стемы совпадают с TokenizeDoc.
	stem := TokenizeDoc("чай")[0].Term
	if got[stem] != bm25TitleWeight+1 {
		t.Fatalf("tf(чай)=%d, ждали %d", got[stem], bm25TitleWeight+1)
	}
	other := TokenizeDoc("напиток")[0].Term
	if got[other] != 1 {
		t.Fatalf("tf(напиток)=%d, ждали 1", got[other])
	}

	// Пустой заголовок — байт-в-байт эквивалент TokenizeDoc.
	a := TokenizeDocTitled("", "чай зелёный")
	b := TokenizeDoc("чай зелёный")
	if len(a) != len(b) {
		t.Fatalf("пустой title изменил термы: %+v vs %+v", a, b)
	}
	for i := range a {
		if a[i] != b[i] {
			t.Fatalf("пустой title изменил термы: %+v vs %+v", a, b)
		}
	}
}

// Буст сквозь живой путь: два дока, у одного терм в заголовке, у другого тот же
// терм только в тексте (и чаще!) — заголовочный побеждает за счёт tf×3.
func TestSearchText_TitleBoostRanks(t *testing.T) {
	lvs := NewLeveledVectorStore(bm25TestConfig())
	t.Cleanup(lvs.Clear)
	if err := lvs.AddDocTitled("titled", mkVecN(8, 1), Attributes{}, "чай", "напиток из листьев"); err != nil {
		t.Fatalf("AddDocTitled: %v", err)
	}
	if err := lvs.AddDoc("plain", mkVecN(8, 2), Attributes{}, "чай чай напиток из листьев"); err != nil {
		t.Fatalf("AddDoc: %v", err)
	}
	res, err := lvs.SearchText("чай", 10)
	if err != nil {
		t.Fatalf("SearchText: %v", err)
	}
	if len(res) != 2 || res[0].Key != "titled" {
		t.Fatalf("ждали titled первым (tf=3 против tf=2), got %+v", res)
	}
}

// df-отсечение: на корпусе ≥bm25PruneMinN терм с df>N/2 выпадает из скоринга —
// выдача и скоры идентичны запросу без него; запрос целиком из общих термов
// не отсекается (fallback) и продолжает матчиться.
func TestSearchText_PrunesCommonQueryTerms(t *testing.T) {
	lvs := NewLeveledVectorStore(bm25TestConfig())
	t.Cleanup(lvs.Clear)
	// 1200 доков: «спам» в каждом (df=N), «сигнал» — в каждом двенадцатом
	// (df=100). Всё живёт в дельте — SearchText это законный путь.
	n := bm25PruneMinN + 200
	for i := range n {
		text := "спам"
		if i%12 == 0 {
			text = "спам сигнал"
		}
		if err := lvs.AddDoc(strconv.Itoa(i), mkVecN(4, float32(i)), Attributes{}, text); err != nil {
			t.Fatalf("AddDoc %d: %v", i, err)
		}
	}

	mixed, err := lvs.SearchText("спам сигнал", 10)
	if err != nil {
		t.Fatalf("SearchText mixed: %v", err)
	}
	clean, err := lvs.SearchText("сигнал", 10)
	if err != nil {
		t.Fatalf("SearchText clean: %v", err)
	}
	if len(mixed) == 0 || len(mixed) != len(clean) {
		t.Fatalf("отсечённый запрос обязан быть эквивалентен чистому: mixed=%d clean=%d", len(mixed), len(clean))
	}
	for i := range mixed {
		if mixed[i] != clean[i] {
			t.Fatalf("расхождение с чистым запросом на позиции %d: %+v vs %+v", i, mixed[i], clean[i])
		}
	}
	for _, h := range mixed {
		id, _ := strconv.Atoi(h.Key)
		if id%12 != 0 {
			t.Fatalf("в выдаче док без «сигнал»: %s — терм «спам» не отсечён", h.Key)
		}
	}

	// Fallback: запрос только из общего терма ищется как есть.
	only, err := lvs.SearchText("спам", 5)
	if err != nil {
		t.Fatalf("SearchText fallback: %v", err)
	}
	if len(only) != 5 {
		t.Fatalf("fallback сломан: запрос из общего терма вернул %d хитов", len(only))
	}
}

// Ниже порога bm25PruneMinN отсечение выключено: общий терм продолжает
// скориться (пин против случайного сдвига golden-оракула).
func TestSearchText_NoPruneBelowMinN(t *testing.T) {
	lvs := NewLeveledVectorStore(bm25TestConfig())
	t.Cleanup(lvs.Clear)
	for i := range 10 {
		if err := lvs.AddDoc(fmt.Sprintf("d%d", i), mkVecN(4, float32(i)), Attributes{}, "спам"); err != nil {
			t.Fatalf("AddDoc: %v", err)
		}
	}
	res, err := lvs.SearchText("спам", 3)
	if err != nil {
		t.Fatalf("SearchText: %v", err)
	}
	if len(res) != 3 {
		t.Fatalf("df=N на малом корпусе не должен отсекаться: got %d хитов", len(res))
	}
}
