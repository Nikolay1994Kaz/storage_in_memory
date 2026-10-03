package main

import (
	"strconv"
	"testing"
)

// ZADD с NaN — ошибка, как в Redis. strconv.ParseFloat("nan") ошибки не даёт,
// поэтому NaN доходил до дерева, а в упорядоченном индексе он ломает сравнение.
func TestExecZAddRejectsNaN(t *testing.T) {
	e := newExecEnv(t)
	for _, bad := range []string{"nan", "NaN", "-nan"} {
		e.wantErrPrefix(e.do("ZADD", "z", bad, "m"), "ERR")
	}
	e.wantInt(e.do("ZCARD", "z"), 0)
	e.wantErrPrefix(e.do("ZRANGEBYSCORE", "z", "nan", "1"), "ERR")
	e.wantErrPrefix(e.do("ZRANGEBYSCORE", "z", "0", "nan"), "ERR")
	// Бесконечности — законные score.
	e.wantInt(e.do("ZADD", "z", "-inf", "low"), 1)
	e.wantInt(e.do("ZADD", "z", "+inf", "high"), 1)
	if v := e.do("ZRANGEBYSCORE", "z", "-inf", "+inf"); len(v.Array) != 2 {
		t.Fatalf("ZRANGEBYSCORE -inf +inf: %d элементов, ожидали 2", len(v.Array))
	}
}

// Равные score на нескольких листах дерева — на уровне команды. Прежде
// ZRANGEBYSCORE z 5 5 отдавал 1 элемент из 300.
func TestExecZRangeByScoreEqualScores(t *testing.T) {
	e := newExecEnv(t)
	for i := 0; i < 300; i++ {
		e.wantInt(e.do("ZADD", "z", "5", "m"+strconv.Itoa(i)), 1)
	}
	for i := 0; i < 50; i++ {
		e.do("ZADD", "z", "9", "high"+strconv.Itoa(i))
	}
	if v := e.do("ZRANGEBYSCORE", "z", "5", "5"); len(v.Array) != 300 {
		t.Fatalf("ZRANGEBYSCORE z 5 5: %d элементов, ожидали 300", len(v.Array))
	}
	if v := e.do("ZRANGEBYSCORE", "z", "5", "9", "WITHSCORES"); len(v.Array) != 2*350 {
		t.Fatalf("ZRANGEBYSCORE z 5 9 WITHSCORES: %d значений, ожидали 700", len(v.Array))
	}
}

// VSIM.SEARCHRANGE фильтрует кандидатов HNSW множеством member из диапазона
// score. Прежде то же обрезание дерева сужало фильтр до одного member, и поиск
// молча возвращал 1 результат вместо K.
func TestExecVSimSearchRangeEqualScores(t *testing.T) {
	e := newExecEnv(t)
	for i := 0; i < 300; i++ {
		key := "doc" + strconv.Itoa(i)
		x := strconv.Itoa(i % 17)
		y := strconv.Itoa(i % 13)
		e.wantSimple(e.do("VSIM.ADD", key, x, y, "1"), "OK")
		score := "5"
		if i%2 == 1 {
			score = "9" // половина документов вне диапазона
		}
		e.do("ZADD", "price", score, key)
	}
	v := e.do("VSIM.SEARCHRANGE", "40", "price", "5", "5", "3", "3", "1")
	if v.Typ != '*' {
		t.Fatalf("ответ %q: %s", v.Typ, v.Str)
	}
	if got := len(v.Array) / 2; got != 40 {
		t.Fatalf("VSIM.SEARCHRANGE вернул %d результатов, ожидали 40", got)
	}
	for i := 0; i < len(v.Array); i += 2 {
		n, _ := strconv.Atoi(v.Array[i].Str[len("doc"):])
		if n%2 == 1 {
			t.Fatalf("в выдаче %s со score 9 — фильтр диапазона не сработал", v.Array[i].Str)
		}
	}
}
