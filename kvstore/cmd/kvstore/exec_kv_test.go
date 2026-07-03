package main

import (
	"testing"
)

// Покрытие KV-семейства диспетчера executeCommand: PING/SET/GET/DEL/EXISTS-нет,
// плюс контрактные детали (arity-ошибки, отсутствующий ключ, перезапись).

func TestExec_Ping(t *testing.T) {
	e := newExecEnv(t)
	e.wantSimple(e.do("PING"), "PONG")
}

func TestExec_SetGet(t *testing.T) {
	e := newExecEnv(t)

	e.wantSimple(e.do("SET", "k", "hello"), "OK")
	e.wantBulk(e.do("GET", "k"), "hello")

	// Перезапись значения.
	e.wantSimple(e.do("SET", "k", "world"), "OK")
	e.wantBulk(e.do("GET", "k"), "world")
}

func TestExec_GetMissing(t *testing.T) {
	e := newExecEnv(t)
	// Отсутствующий ключ → RESP null bulk ($-1), не пустая строка.
	v := e.do("GET", "nope")
	if v.Typ != '$' || v.Str != "" {
		t.Fatalf("GET несуществующего должен быть null bulk, got typ=%q str=%q", v.Typ, v.Str)
	}
}

func TestExec_Del(t *testing.T) {
	e := newExecEnv(t)
	e.do("SET", "k", "v")

	// DEL существующего → 1 удалён.
	e.wantInt(e.do("DEL", "k"), 1)
	// Повторный DEL → 0 (уже нет).
	e.wantInt(e.do("DEL", "k"), 0)
	// GET после DEL → null.
	if v := e.do("GET", "k"); v.Typ != '$' || v.Str != "" {
		t.Fatalf("после DEL ключ должен исчезнуть, got typ=%q str=%q", v.Typ, v.Str)
	}
}

func TestExec_SetArityError(t *testing.T) {
	e := newExecEnv(t)
	// SET без значения → arity-ошибка, а не паника/тихий OK.
	e.wantErrPrefix(e.do("SET", "onlykey"), "ERR")
	// GET без ключа → arity-ошибка.
	e.wantErrPrefix(e.do("GET"), "ERR")
}

func TestExec_DbsizeCountsKeys(t *testing.T) {
	e := newExecEnv(t)
	e.wantInt(e.do("DBSIZE"), 0)
	e.do("SET", "a", "1")
	e.do("SET", "b", "2")
	e.wantInt(e.do("DBSIZE"), 2)
	e.do("DEL", "a")
	e.wantInt(e.do("DBSIZE"), 1)
}
