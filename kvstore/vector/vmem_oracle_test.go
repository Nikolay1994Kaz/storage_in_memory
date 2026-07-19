package vector

// Оракул корректности VMEM (шаг 1 спринта, docs/VMEM_DESIGN.md).
//
// Источник истины — testdata/vmem/scenarios.json, написанный руками. В отличие
// от BM25-оракула внешний эталон не нужен: семантика памяти (интервалы
// валидности, scope, erasure) вычислима — поэтому TestVMEMGoldenIntegrity
// содержит исполняемую модель-интерпретатор и ПЕРЕСЧИТЫВАЕТ ожидания каждого
// запроса; расхождение с записанным expect = баг сценария, а не движка.
// Лексический матч модели — тот же bm25Tokenize, что в проде (пересечение
// стемов >= 1); скоринг модель не считает — scoring-сценарии пинят только
// множество + expect_first, порядок включится в шаге 5.
//
// Контракт, который пинят сценарии (см. заголовок scenarios.json):
//   - валидность = полуинтервал valid_from <= t < valid_to, открытый = 2^53;
//   - RECALL-дефолт = валидное-сейчас; история через as_of / all;
//   - supersession (история живёт) != erasure (FORGET/TTL: невидим ВЕЗДЕ,
//     включая all и as_of; стирание среднего звена цепочки не воскрешает
//     закрытые интервалы соседей);
//   - upsert по клиентскому ID = полная замена; без ID ретрай = дубль;
//   - запросы исполняются после всей ленты операций (now >= последнего at).
//
// TestVMEMOracleParity включается, когда появится живой путь REMEMBER/RECALL
// (шаги 2–3): те же сценарии через движок x3 состояния LSM, как в BM25.

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"testing"
)

const vmemOpenValidTo = int64(1) << 53 // сентинел открытого интервала (дверь контракта)

type vmemOp struct {
	Op         string   `json:"op"` // remember | forget
	ID         string   `json:"id"`
	Scope      string   `json:"scope"`
	Text       string   `json:"text"`
	Type       string   `json:"type"`
	Importance *float64 `json:"importance"`
	At         int64    `json:"at"`
	TTL        int64    `json:"ttl"`
	Supersedes string   `json:"supersedes"`
}

type vmemQuery struct {
	ID          string   `json:"id"`
	Scope       string   `json:"scope"`
	Query       string   `json:"query"`
	Now         int64    `json:"now"`
	AsOf        *int64   `json:"as_of"`
	All         bool     `json:"all"`
	TypeEq      string   `json:"type_eq"`
	Expect      []string `json:"expect"`
	ExpectFirst string   `json:"expect_first"`
	Scoring     bool     `json:"scoring"`
}

type vmemScenario struct {
	Name    string      `json:"name"`
	Ops     []vmemOp    `json:"ops"`
	Queries []vmemQuery `json:"queries"`
}

type vmemScenarios struct {
	Scenarios []vmemScenario `json:"scenarios"`
}

func loadVMEMScenarios(t *testing.T) vmemScenarios {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", "vmem", "scenarios.json"))
	if err != nil {
		t.Fatalf("читаем scenarios.json: %v", err)
	}
	var s vmemScenarios
	if err := json.Unmarshal(raw, &s); err != nil {
		t.Fatalf("парсим scenarios.json: %v", err)
	}
	if len(s.Scenarios) == 0 {
		t.Fatal("пустой набор сценариев")
	}
	return s
}

// vmemFact — состояние одного факта в модели после проигрывания ленты операций.
type vmemFact struct {
	scope     string
	tokens    []string
	typ       string
	validFrom int64
	validTo   int64 // vmemOpenValidTo если не superseded
	expiresAt int64 // vmemOpenValidTo если без TTL
	forgotten bool
}

// vmemReplay проигрывает ленту операций сценария и возвращает финальное
// состояние фактов. Валидирует структурные инварианты (существование целей
// supersedes/forget, одинаковый scope, монотонность времени) через t.Fatalf.
func vmemReplay(t *testing.T, name string, ops []vmemOp) map[string]*vmemFact {
	t.Helper()
	facts := make(map[string]*vmemFact)
	prevAt := int64(0)
	for i, op := range ops {
		if op.At <= 0 || op.At < prevAt {
			t.Fatalf("%s: op[%d] at=%d — времена должны быть >0 и неубывающими", name, i, op.At)
		}
		prevAt = op.At
		switch op.Op {
		case "remember":
			if op.ID == "" || op.Scope == "" || op.Text == "" {
				t.Fatalf("%s: op[%d] remember без id/scope/text", name, i)
			}
			if op.Importance != nil && (*op.Importance < 0 || *op.Importance > 1) {
				t.Fatalf("%s: op[%d] importance=%v вне [0,1]", name, i, *op.Importance)
			}
			if old, ok := facts[op.ID]; ok && old.scope != op.Scope {
				t.Fatalf("%s: op[%d] upsert %s меняет scope %s->%s — запрещено контрактом", name, i, op.ID, old.scope, op.Scope)
			}
			f := &vmemFact{
				scope:     op.Scope,
				tokens:    bm25Tokenize(op.Text),
				typ:       op.Type,
				validFrom: op.At,
				validTo:   vmemOpenValidTo,
				expiresAt: vmemOpenValidTo,
			}
			if op.TTL > 0 {
				f.expiresAt = op.At + op.TTL // конвертация при ингесте (дверь 1: абсолютное время в WAL)
			}
			if op.Supersedes != "" {
				target, ok := facts[op.Supersedes]
				if !ok || op.Supersedes == op.ID {
					t.Fatalf("%s: op[%d] supersedes=%q — цель не существует или self", name, i, op.Supersedes)
				}
				if target.scope != op.Scope {
					t.Fatalf("%s: op[%d] supersedes через границу scope (%s vs %s) — запрещено контрактом", name, i, target.scope, op.Scope)
				}
				target.validTo = op.At
			}
			facts[op.ID] = f
		case "forget":
			target, ok := facts[op.ID]
			if !ok {
				t.Fatalf("%s: op[%d] forget %q — факт не существует", name, i, op.ID)
			}
			target.forgotten = true
		default:
			t.Fatalf("%s: op[%d] неизвестная операция %q", name, i, op.Op)
		}
	}
	return facts
}

// vmemModelRecall — эталонная семантика RECALL: множество id (без порядка).
func vmemModelRecall(facts map[string]*vmemFact, q vmemQuery) []string {
	qTokens := bm25Tokenize(q.Query)
	tEff := q.Now
	if q.AsOf != nil {
		tEff = *q.AsOf
	}
	var got []string
	for id, f := range facts {
		if f.forgotten || f.scope != q.Scope {
			continue
		}
		if q.Now >= f.expiresAt { // erasure: невидим во всех режимах
			continue
		}
		if !q.All && !(f.validFrom <= tEff && tEff < f.validTo) {
			continue
		}
		if q.TypeEq != "" && f.typ != q.TypeEq {
			continue
		}
		match := false
		for _, qt := range qTokens {
			if slices.Contains(f.tokens, qt) {
				match = true
				break
			}
		}
		if match {
			got = append(got, id)
		}
	}
	slices.Sort(got)
	return got
}

// TestVMEMGoldenIntegrity — ручные expect каждого запроса совпадают с
// пересчётом исполняемой моделью. Ловит и баги сценариев, и молчаливый дрейф
// контракта (сменили семантику — сценарии закричат).
func TestVMEMGoldenIntegrity(t *testing.T) {
	all := loadVMEMScenarios(t)
	seenNames := make(map[string]bool)
	for _, sc := range all.Scenarios {
		if seenNames[sc.Name] {
			t.Fatalf("дубль имени сценария %q", sc.Name)
		}
		seenNames[sc.Name] = true
		facts := vmemReplay(t, sc.Name, sc.Ops)

		lastAt := sc.Ops[len(sc.Ops)-1].At
		seenQ := make(map[string]bool)
		for _, q := range sc.Queries {
			label := sc.Name + "/" + q.ID
			if seenQ[q.ID] {
				t.Fatalf("%s: дубль id запроса", label)
			}
			seenQ[q.ID] = true
			if q.Now < lastAt {
				t.Fatalf("%s: now=%d раньше последней операции (%d) — запросы исполняются после ленты", label, q.Now, lastAt)
			}
			if q.AsOf != nil && q.All {
				t.Fatalf("%s: as_of и all одновременно не имеют смысла", label)
			}
			if q.Scoring && q.ExpectFirst == "" {
				t.Fatalf("%s: scoring-запрос без expect_first", label)
			}
			if q.ExpectFirst != "" && !slices.Contains(q.Expect, q.ExpectFirst) {
				t.Fatalf("%s: expect_first=%q отсутствует в expect", label, q.ExpectFirst)
			}

			want := append([]string(nil), q.Expect...)
			slices.Sort(want)
			if got := vmemModelRecall(facts, q); !slices.Equal(got, want) {
				t.Errorf("%s: модель даёт %v, в golden записано %v", label, got, want)
			}
		}
	}
}

// TestVMEMOracleParity — те же сценарии через живой путь REMEMBER/RECALL
// (по образцу BM25: x3 состояния LSM — дельта / после flush / смешанное).
// Включается в шагах 2–3, когда появится реализация.
func TestVMEMOracleParity(t *testing.T) {
	t.Skip("шаги 2–3 VMEM-спринта: путь REMEMBER/RECALL ещё не реализован")
}
