package main

import (
	"strings"
	"testing"
)

// vector-семейство диспетчера: VSIM.ADD/SEARCH/DEL/INFO + ошибки парсинга.

func TestExec_VsimAddSearch(t *testing.T) {
	e := newExecEnv(t)

	e.wantSimple(e.do("VSIM.ADD", "a", "1", "0", "0"), "OK")
	e.wantSimple(e.do("VSIM.ADD", "b", "0", "1", "0"), "OK")
	e.wantSimple(e.do("VSIM.ADD", "c", "0", "0", "1"), "OK")

	// Ближайший к [1,0,0] — сам вектор "a" (расстояние ~0).
	v := e.do("VSIM.SEARCH", "1", "1", "0", "0")
	if v.Typ != '*' || len(v.Array) != 2 { // K=1 → [key, distance]
		t.Fatalf("VSIM.SEARCH K=1 ждали массив из 2 (key+dist), got typ=%q len=%d", v.Typ, len(v.Array))
	}
	if v.Array[0].Str != "a" {
		t.Fatalf("ближайший к [1,0,0] должен быть 'a', got %q", v.Array[0].Str)
	}
}

func TestExec_VsimInfo(t *testing.T) {
	e := newExecEnv(t)
	e.do("VSIM.ADD", "a", "1", "2", "3")
	e.do("VSIM.ADD", "b", "4", "5", "6")

	v := e.do("VSIM.INFO")
	if v.Typ != '$' {
		t.Fatalf("VSIM.INFO ждали bulk, got typ=%q", v.Typ)
	}
	// Формат "vectors:N dimension:D max_level:L" — проверяем count и dim.
	if !strings.HasPrefix(v.Str, "vectors:2 dimension:3 ") {
		t.Fatalf("VSIM.INFO неожиданный вывод: %q", v.Str)
	}
}

func TestExec_VsimDel(t *testing.T) {
	e := newExecEnv(t)
	e.do("VSIM.ADD", "a", "1", "0", "0")

	e.wantInt(e.do("VSIM.DEL", "a"), 1) // удалён
	e.wantInt(e.do("VSIM.DEL", "a"), 0) // уже нет
}

// DEL ключа должен подчищать и вектор (composite-удаление в SET/DEL пути).
func TestExec_DelAlsoRemovesVector(t *testing.T) {
	e := newExecEnv(t)
	e.do("SET", "k", "meta")
	e.do("VSIM.ADD", "k", "1", "0", "0")

	e.do("DEL", "k")

	// Вектора больше нет — VSIM.DEL вернёт 0.
	e.wantInt(e.do("VSIM.DEL", "k"), 0)
}

// VSIM.EXISTS — прямой existence-чек мимо ANN (для durability-оракула).
func TestExec_VsimExists(t *testing.T) {
	e := newExecEnv(t)
	e.do("VSIM.ADD", "a", "1", "0", "0")

	e.wantInt(e.do("VSIM.EXISTS", "a"), 1)     // есть
	e.wantInt(e.do("VSIM.EXISTS", "ghost"), 0) // не было
	e.do("VSIM.DEL", "a")
	e.wantInt(e.do("VSIM.EXISTS", "a"), 0)      // удалён — не воскрес
	e.wantErrPrefix(e.do("VSIM.EXISTS"), "ERR") // без ключа
}

func TestExec_VsimErrors(t *testing.T) {
	e := newExecEnv(t)
	e.wantErrPrefix(e.do("VSIM.ADD", "onlykey"), "ERR")        // нет компонент
	e.wantErrPrefix(e.do("VSIM.ADD", "k", "1", "bad"), "ERR")  // нечисловая компонента
	e.wantErrPrefix(e.do("VSIM.SEARCH", "0", "1", "2"), "ERR") // K<=0
	e.wantErrPrefix(e.do("VSIM.SEARCH", "x", "1", "2"), "ERR") // K не число
}
