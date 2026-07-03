package main

import "testing"

// TTL-семейство диспетчера: SET ... EX, EXPIRE, TTL, PERSIST — включая
// граничные коды Redis (-2 нет ключа, -1 нет таймера, :0 EXPIRE на отсутствующем).

func TestExec_SetWithExAndTTL(t *testing.T) {
	e := newExecEnv(t)

	e.wantSimple(e.do("SET", "k", "v", "EX", "100"), "OK")

	v := e.do("TTL", "k")
	if v.Typ != ':' || v.Num <= 0 || v.Num > 100 {
		t.Fatalf("TTL после SET EX 100 должен быть ~100, got typ=%q num=%d", v.Typ, v.Num)
	}
}

func TestExec_SetExInvalid(t *testing.T) {
	e := newExecEnv(t)
	// EX с нечисловым/неположительным аргументом → явная ошибка.
	e.wantErrPrefix(e.do("SET", "k", "v", "EX", "notnum"), "ERR")
	e.wantErrPrefix(e.do("SET", "k", "v", "EX", "0"), "ERR")
}

func TestExec_TTLCodes(t *testing.T) {
	e := newExecEnv(t)

	// Нет ключа вовсе → -2.
	e.wantInt(e.do("TTL", "ghost"), -2)

	// Ключ есть, таймера нет → -1.
	e.do("SET", "k", "v")
	e.wantInt(e.do("TTL", "k"), -1)
}

func TestExec_Expire(t *testing.T) {
	e := newExecEnv(t)

	// EXPIRE на несуществующем ключе → 0 (нечего протухать).
	e.wantInt(e.do("EXPIRE", "ghost", "100"), 0)

	// EXPIRE на существующем → 1, затем TTL это видит.
	e.do("SET", "k", "v")
	e.wantInt(e.do("EXPIRE", "k", "100"), 1)
	if v := e.do("TTL", "k"); v.Num <= 0 || v.Num > 100 {
		t.Fatalf("после EXPIRE 100 ждали TTL ~100, got %d", v.Num)
	}

	// Некорректное время → ошибка.
	e.wantErrPrefix(e.do("EXPIRE", "k", "-5"), "ERR")
}

func TestExec_Persist(t *testing.T) {
	e := newExecEnv(t)

	e.do("SET", "k", "v", "EX", "100")
	// PERSIST снимает таймер → 1, повторный → 0.
	e.wantInt(e.do("PERSIST", "k"), 1)
	e.wantInt(e.do("PERSIST", "k"), 0)
	// После PERSIST TTL = -1 (ключ есть, таймера нет).
	e.wantInt(e.do("TTL", "k"), -1)
}
