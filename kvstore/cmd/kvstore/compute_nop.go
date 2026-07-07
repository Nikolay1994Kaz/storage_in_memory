package main

import (
	"context"

	"kvstore/kvstore/internal/server"
)

// nopCompute — заглушка WASM-движка. В прод-сборке (без тега experimental)
// newComputeEngine возвращает её вместо реального адаптера, поэтому пакет
// internal/compute и вся wazero-зависимость НЕ линкуются в бинарь: ACE-поверхность
// (WASM.EXEC произвольного модуля) физически отсутствует. Все методы — no-op, а
// WASM.* команды отвечают явной ошибкой «выключено в этой сборке».
//
// Файл намеренно БЕЗ build-тега: тесты диспетчера (exec_harness_test.go)
// используют nopCompute как seam в обеих сборках — иначе vet/test с
// -tags experimental не компилируются.
type nopCompute struct{}

func (nopCompute) FireSet(string, int) {}
func (nopCompute) FireDel(string, int) {}

func (nopCompute) HandleCommand(cmd string, args [][]byte, buf *server.ConnBuf) {
	buf.WriteError("ERR WASM support is disabled in this build (rebuild with -tags experimental)")
}

func (nopCompute) SetAI(func(context.Context, string) ([]float32, error), func(context.Context, string) (string, error)) {
}

func (nopCompute) Close() {}
