package vector

// Оракул корректности BM25 (шаг 1 спринта, docs/BM25_HYBRID_DESIGN.md).
//
// Источник истины — testdata/bm25/golden.json, сгенерированный
// scripts/bm25_oracle.py (Python rank_bm25 как внешний эталон + целевая
// Lucene-формула). Контракт токенизации и скоринга описан в заголовке скрипта.
//
// Сейчас активна только проверка целостности golden-файла; паритет-тест
// включается, когда появятся токенайзер (шаг 2) и скоринг (шаг 3).

import (
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"testing"
)

const bm25ScoreEps = 1e-6 // допуск ничьих: скоры в golden округлены до 6 знаков

type bm25GoldenDoc struct {
	ID     string   `json:"id"`
	Tokens []string `json:"tokens"`
}

type bm25GoldenHit struct {
	Doc   string  `json:"doc"`
	Score float64 `json:"score"`
}

type bm25GoldenQuery struct {
	ID      string          `json:"id"`
	Text    string          `json:"text"`
	Tokens  []string        `json:"tokens"`
	Ranking []bm25GoldenHit `json:"ranking"`
}

type bm25Golden struct {
	Params struct {
		K1 float64 `json:"k1"`
		B  float64 `json:"b"`
	} `json:"params"`
	AvgDL   float64           `json:"avgdl"`
	Docs    []bm25GoldenDoc   `json:"docs"`
	Queries []bm25GoldenQuery `json:"queries"`
}

type bm25Corpus struct {
	Docs []struct {
		ID   string `json:"id"`
		Text string `json:"text"`
	} `json:"docs"`
	Queries []struct {
		ID   string `json:"id"`
		Text string `json:"text"`
	} `json:"queries"`
}

func loadBM25Golden(t *testing.T) (bm25Corpus, bm25Golden) {
	t.Helper()
	var c bm25Corpus
	var g bm25Golden
	for path, dst := range map[string]any{
		filepath.Join("testdata", "bm25", "corpus.json"): &c,
		filepath.Join("testdata", "bm25", "golden.json"): &g,
	} {
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("читаем %s: %v (перегенерация: scripts/bm25_oracle.py)", path, err)
		}
		if err := json.Unmarshal(raw, dst); err != nil {
			t.Fatalf("парсим %s: %v", path, err)
		}
	}
	return c, g
}

// TestBM25GoldenIntegrity — golden согласован с корпусом и внутренне валиден.
// Ловит забытую перегенерацию после правки corpus.json.
func TestBM25GoldenIntegrity(t *testing.T) {
	corpus, golden := loadBM25Golden(t)

	if golden.Params.K1 != 1.2 || golden.Params.B != 0.75 {
		t.Fatalf("параметры golden k1=%v b=%v, ждём 1.2/0.75", golden.Params.K1, golden.Params.B)
	}

	docIDs := make(map[string]bool, len(golden.Docs))
	totalLen := 0
	for _, d := range golden.Docs {
		if len(d.Tokens) == 0 {
			t.Errorf("док %s: пустые токены", d.ID)
		}
		docIDs[d.ID] = true
		totalLen += len(d.Tokens)
	}
	if len(golden.Docs) != len(corpus.Docs) {
		t.Fatalf("golden %d доков, corpus %d — перегенерируй golden", len(golden.Docs), len(corpus.Docs))
	}
	for _, d := range corpus.Docs {
		if !docIDs[d.ID] {
			t.Errorf("док %s есть в corpus, нет в golden", d.ID)
		}
	}
	if got := float64(totalLen) / float64(len(golden.Docs)); math.Abs(got-golden.AvgDL) > 1e-4 {
		t.Errorf("avgdl из токенов %.6f != записанного %.6f", got, golden.AvgDL)
	}

	if len(golden.Queries) != len(corpus.Queries) {
		t.Fatalf("golden %d запросов, corpus %d — перегенерируй golden", len(golden.Queries), len(corpus.Queries))
	}
	for _, q := range golden.Queries {
		prev := math.Inf(1)
		for _, h := range q.Ranking {
			if !docIDs[h.Doc] {
				t.Errorf("%s: ранжирован неизвестный док %s", q.ID, h.Doc)
			}
			if h.Score <= 0 {
				t.Errorf("%s: неположительный скор %v у %s", q.ID, h.Score, h.Doc)
			}
			if h.Score > prev+bm25ScoreEps {
				t.Errorf("%s: скоры не по убыванию (%v после %v)", q.ID, h.Score, prev)
			}
			prev = h.Score
		}
	}
}

// assertBM25Ranking сверяет выдачу с golden: порядок обязан совпадать,
// ничьи (скоры в пределах bm25ScoreEps) могут идти в любом порядке внутри
// своей группы. Скоры сверяются с тем же допуском.
func assertBM25Ranking(t *testing.T, queryID string, want []bm25GoldenHit, got []bm25GoldenHit) {
	t.Helper()
	if len(got) != len(want) {
		t.Errorf("%s: длина выдачи %d, ждём %d (want=%v got=%v)", queryID, len(got), len(want), want, got)
		return
	}
	groupOf := func(hits []bm25GoldenHit) map[string]int {
		g, id := map[string]int{}, 0
		for i, h := range hits {
			if i > 0 && math.Abs(h.Score-hits[i-1].Score) > bm25ScoreEps {
				id++
			}
			g[h.Doc] = id
		}
		return g
	}
	wantG := groupOf(want)
	for i, h := range got {
		if math.Abs(h.Score-want[i].Score) > bm25ScoreEps {
			t.Errorf("%s: позиция %d — скор %.6f, ждём %.6f", queryID, i, h.Score, want[i].Score)
		}
		wg, ok := wantG[h.Doc]
		if !ok {
			t.Errorf("%s: позиция %d — неожиданный док %s", queryID, i, h.Doc)
			continue
		}
		if gotGroup := groupOf(got)[h.Doc]; gotGroup != wg {
			t.Errorf("%s: док %s в группе ничьих %d, ждём %d", queryID, h.Doc, gotGroup, wg)
		}
	}
}

// TestBM25OracleParity — паритет Go-реализации с golden.
// Включить после шагов 2–3 (токенайзер + segmentText/скоринг): токенизировать
// corpus.docs, посчитать скоры по каждому запросу и прогнать через
// assertBM25Ranking; токены доков/запросов сверить с golden побуквенно.
func TestBM25OracleParity(t *testing.T) {
	t.Skip("BM25 ещё не реализован — оракул-харнесс готов, ждёт шагов 2–3 плана")
}
