package main

import (
	"testing"

	"kvstore/kvstore/internal/protocol"
	"kvstore/kvstore/vector"
)

// bin — сериализация вектора в бинарный формат провода (LittleEndian float32),
// как его шлёт клиент в VSIM.ADDBIN/VSIM.SEARCHBIN.
func bin(vec ...float32) string {
	return string(vector.SerializeVector(vec))
}

// VSIM бинарные варианты: ADDBIN/SEARCHBIN — тот же индекс, что текстовые,
// но вектор приходит сырыми байтами (zero-copy путь).
func TestExec_VsimBinary(t *testing.T) {
	e := newExecEnv(t)

	e.wantSimple(e.do("VSIM.ADDBIN", "a", bin(1, 0, 0)), "OK")
	e.wantSimple(e.do("VSIM.ADDBIN", "b", bin(0, 1, 0)), "OK")

	// Ближайший к [1,0,0] — "a".
	v := e.do("VSIM.SEARCHBIN", "1", bin(1, 0, 0))
	if v.Typ != '*' || len(v.Array) != 2 {
		t.Fatalf("VSIM.SEARCHBIN K=1 ждали массив из 2, got typ=%q len=%d", v.Typ, len(v.Array))
	}
	if v.Array[0].Str != "a" {
		t.Fatalf("ближайший к [1,0,0] должен быть 'a', got %q", v.Array[0].Str)
	}
}

func TestExec_VsimBinaryErrors(t *testing.T) {
	e := newExecEnv(t)
	// Длина байт не кратна 4 → ошибка (не паника zero-copy каста).
	e.wantErrPrefix(e.do("VSIM.ADDBIN", "a", "xyz"), "ERR invalid binary vector")
	e.wantErrPrefix(e.do("VSIM.SEARCHBIN", "1", "xyz"), "ERR invalid binary vector")
	// Неверная арность.
	e.wantErrPrefix(e.do("VSIM.ADDBIN", "a"), "ERR")
}

// VSIM.SEARCHFILTER — режим PREFIX: фильтр по префиксу ключа вектора.
func TestExec_VsimSearchFilterPrefix(t *testing.T) {
	e := newExecEnv(t)

	e.do("VSIM.ADD", "product:x", "1", "0", "0")
	e.do("VSIM.ADD", "other:y", "1", "0", "0") // тот же вектор, но иной префикс

	v := e.do("VSIM.SEARCHFILTER", "5", "PREFIX", "product:", "1", "0", "0")
	if v.Typ != '*' {
		t.Fatalf("ждали массив, got typ=%q", v.Typ)
	}
	// Должен вернуться только product:x, other:y отфильтрован.
	if len(v.Array) != 2 || v.Array[0].Str != "product:x" {
		t.Fatalf("PREFIX-фильтр должен вернуть только 'product:x', got %v", respKeys(v))
	}
}

// VSIM.SEARCHFILTER — режим KV: фильтр по метаданным field:key в KV Store.
func TestExec_VsimSearchFilterKV(t *testing.T) {
	e := newExecEnv(t)

	e.do("VSIM.ADD", "v1", "1", "0", "0")
	e.do("SET", "cat:v1", "electronics") // метаданные вектора

	// Совпадающий фильтр → v1 найден.
	v := e.do("VSIM.SEARCHFILTER", "5", "cat", "electronics", "1", "0", "0")
	if len(v.Array) != 2 || v.Array[0].Str != "v1" {
		t.Fatalf("KV-фильтр должен вернуть 'v1', got %v", respKeys(v))
	}

	// Несовпадающий фильтр → пусто.
	v = e.do("VSIM.SEARCHFILTER", "5", "cat", "books", "1", "0", "0")
	if v.Typ != '*' || len(v.Array) != 0 {
		t.Fatalf("несовпадающий фильтр должен дать пустой результат, got len=%d", len(v.Array))
	}
}

// VSIM.SEARCHRANGE — комбо: score-диапазон из zset + векторный поиск.
func TestExec_VsimSearchRange(t *testing.T) {
	e := newExecEnv(t)

	// Векторы, чьи ключи = члены sorted set.
	e.do("VSIM.ADD", "doc1", "1", "0", "0")
	e.do("VSIM.ADD", "doc2", "1", "0", "0")
	e.do("ZADD", "myset", "10", "doc1")
	e.do("ZADD", "myset", "20", "doc2")

	// Диапазон [5,15] пропускает только doc1 (score 10); doc2 (20) вне диапазона.
	v := e.do("VSIM.SEARCHRANGE", "5", "myset", "5", "15", "1", "0", "0")
	if len(v.Array) != 2 || v.Array[0].Str != "doc1" {
		t.Fatalf("SEARCHRANGE [5,15] должен вернуть только 'doc1', got %v", respKeys(v))
	}

	// Диапазон вне всех скоров → пусто.
	v = e.do("VSIM.SEARCHRANGE", "5", "myset", "100", "200", "1", "0", "0")
	if v.Typ != '*' || len(v.Array) != 0 {
		t.Fatalf("пустой диапазон должен дать 0 результатов, got len=%d", len(v.Array))
	}

	// Арность.
	e.wantErrPrefix(e.do("VSIM.SEARCHRANGE", "5", "myset", "5"), "ERR")
}

// respKeys вытаскивает ключи (чётные позиции) из ответа key+distance пар — для
// читаемых сообщений об ошибке.
func respKeys(v protocol.Value) []string {
	var out []string
	for i := 0; i+1 < len(v.Array); i += 2 {
		out = append(out, v.Array[i].Str)
	}
	return out
}
