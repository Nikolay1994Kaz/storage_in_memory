package main

import "testing"

// zset-семейство диспетчера: ZADD/ZSCORE/ZREM/ZRANGEBYSCORE/ZCARD.

func TestExec_ZAddScoreRem(t *testing.T) {
	e := newExecEnv(t)

	// Новый member → 1; повторный ZADD того же member → 0 (обновление, не вставка).
	e.wantInt(e.do("ZADD", "s", "1.5", "alice"), 1)
	e.wantInt(e.do("ZADD", "s", "2.5", "alice"), 0)

	// ZSCORE отдаёт обновлённый скор как bulk-строку.
	e.wantBulk(e.do("ZSCORE", "s", "alice"), "2.5")

	// ZSCORE отсутствующего member → null.
	if v := e.do("ZSCORE", "s", "bob"); v.Typ != '$' || v.Str != "" {
		t.Fatalf("ZSCORE несуществующего member должен быть null, got typ=%q str=%q", v.Typ, v.Str)
	}

	// ZREM: удалить есть → 1, повторно → 0.
	e.wantInt(e.do("ZREM", "s", "alice"), 1)
	e.wantInt(e.do("ZREM", "s", "alice"), 0)
}

func TestExec_ZCardAndRange(t *testing.T) {
	e := newExecEnv(t)

	e.do("ZADD", "s", "1", "a")
	e.do("ZADD", "s", "2", "b")
	e.do("ZADD", "s", "3", "c")

	e.wantInt(e.do("ZCARD", "s"), 3)

	// ZRANGEBYSCORE [2,3] → {b,c} по возрастанию скора.
	v := e.do("ZRANGEBYSCORE", "s", "2", "3")
	if v.Typ != '*' || len(v.Array) != 2 {
		t.Fatalf("ZRANGEBYSCORE ждали массив из 2, got typ=%q len=%d", v.Typ, len(v.Array))
	}
	if v.Array[0].Str != "b" || v.Array[1].Str != "c" {
		t.Fatalf("ZRANGEBYSCORE порядок [b c], got [%s %s]", v.Array[0].Str, v.Array[1].Str)
	}
}

func TestExec_ZAddArity(t *testing.T) {
	e := newExecEnv(t)
	e.wantErrPrefix(e.do("ZADD", "s", "1"), "ERR")      // нет member
	e.wantErrPrefix(e.do("ZADD", "s", "x", "m"), "ERR") // нечисловой score
}
