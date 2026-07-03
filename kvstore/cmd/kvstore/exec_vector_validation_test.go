package main

import (
	"math"
	"strconv"
	"strings"
	"testing"

	"kvstore/kvstore/vector"
)

// Стражи входной валидации VSIM-команд. Недоверенный вход не должен ни ронять
// процесс (crash-loop через отравленный WAL), ни молча портить состояние
// (SQ8-калибровку, снапшот, дельту). Все проверки обязаны отсекать ДО записи
// в WAL — иначе отрава переживёт рестарт.

// Пустой вектор в VSIM.ADDBIN проходил %4-чек (0%4==0) и доходил до
// deltaMax(dim=0) → деление на ноль → паника. А так как bw.Write шёл ДО Add,
// запись уже в WAL → на рестарте реплей повторяет панику = crash-loop.
// Страж: команда обязана вернуть ERR, а не уронить диспетчер.
func TestExec_VsimEmptyVectorNoCrashLoop(t *testing.T) {
	e := newExecEnv(t)
	// Пустые байты — валидны по кратности 4, но вектор нулевой длины.
	e.wantErrPrefix(e.do("VSIM.ADDBIN", "a", ""), "ERR")
	// Сервер жив: обычная вставка после этого работает.
	e.wantSimple(e.do("VSIM.ADDBIN", "a", bin(1, 0, 0)), "OK")
}

// NaN/Inf отравляют калибровку SQ8 целого сегмента и портят HNSW-расстояния.
// strconv.ParseFloat("NaN"/"Inf") успешен → текстовый путь их пропускал;
// бинарный путь кодирует битпаттерн напрямую.
func TestExec_VsimNaNInfRejected(t *testing.T) {
	e := newExecEnv(t)
	e.wantErrPrefix(e.do("VSIM.ADD", "a", "NaN", "0", "0"), "ERR")
	e.wantErrPrefix(e.do("VSIM.ADD", "b", "Inf", "0", "0"), "ERR")
	e.wantErrPrefix(e.do("VSIM.ADD", "c", "0", "-Inf", "0"), "ERR")
	// Бинарный путь.
	e.wantErrPrefix(e.do("VSIM.ADDBIN", "d", bin(float32(math.NaN()), 0, 0)), "ERR")
	e.wantErrPrefix(e.do("VSIM.ADDBIN", "e", bin(float32(math.Inf(1)), 0, 0)), "ERR")
}

// Пустой ключ в дельте — сентинел tombstone (delta.go: keys[i]=="" → удалён).
// Вектор с пустым ключом молча считался бы удалённым (тихая потеря данных).
func TestExec_VsimEmptyKeyRejected(t *testing.T) {
	e := newExecEnv(t)
	e.wantErrPrefix(e.do("VSIM.ADD", "", "1", "0", "0"), "ERR")
	e.wantErrPrefix(e.do("VSIM.ADDBIN", "", bin(1, 0, 0)), "ERR")
}

// Ключ длиннее uint16 усекается в бинарном снапшоте (5 мест PutUint16) →
// порча формата. Отсекаем на входе.
func TestExec_VsimKeyTooLongRejected(t *testing.T) {
	e := newExecEnv(t)
	longKey := strings.Repeat("k", vector.MaxKeyLen+1)
	e.wantErrPrefix(e.do("VSIM.ADD", longKey, "1", "0", "0"), "ERR")
}

// K без верхнего капа → efSearch=K раздувает кандидатные кучи → OOM (DoS
// одним маленьким запросом). Верхний кап отсекает до аллокации.
func TestExec_VsimSearchKCap(t *testing.T) {
	e := newExecEnv(t)
	e.wantSimple(e.do("VSIM.ADD", "a", "1", "0", "0"), "OK")
	e.wantErrPrefix(e.do("VSIM.SEARCH", strconv.Itoa(vector.MaxSearchK+1), "1", "0", "0"), "ERR")
	e.wantErrPrefix(e.do("VSIM.SEARCHBIN", strconv.Itoa(vector.MaxSearchK+1), bin(1, 0, 0)), "ERR")
	// В пределах капа — обычный поиск работает.
	v := e.do("VSIM.SEARCH", "1", "1", "0", "0")
	if v.Typ != '*' {
		t.Fatalf("VSIM.SEARCH в пределах капа: ждали массив, got typ=%q", v.Typ)
	}
}
