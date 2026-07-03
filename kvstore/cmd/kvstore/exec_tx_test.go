package main

import "testing"

// TestExecQueuedTx_PanicReleasesMutex — страж H1.
//
// EXEC брал globalTxMu.Lock без defer. Паника команды в очереди (её ловит
// recover в handleConn) пропускала Unlock → мьютекс залочен НАВСЕГДА → EXEC
// всех соединений виснут. Тест паникует в run внутри execQueuedTx и проверяет,
// что globalTxMu всё равно освобождён (TryLock успешен).
func TestExecQueuedTx_PanicReleasesMutex(t *testing.T) {
	queue := [][][]byte{
		{[]byte("SET"), []byte("k"), []byte("v")},
		{[]byte("BOOM")}, // на этой команде run паникует
	}

	func() {
		defer func() {
			if r := recover(); r == nil {
				t.Fatal("ожидали, что паника из run прорастёт наружу (как в handleConn)")
			}
		}()
		execQueuedTx(queue, func(int) {}, func(qCmd string, _ [][]byte) {
			if qCmd == "BOOM" {
				panic("boom in queued command")
			}
		})
	}()

	if !globalTxMu.TryLock() {
		t.Fatal("H1: globalTxMu остался залочен после паники в EXEC — EXEC всех соединений завис бы навсегда")
	}
	globalTxMu.Unlock()
}

// TestExecQueuedTx_NormalPath — счастливый путь: все команды выполнены,
// заголовок массива записан с длиной очереди, мьютекс освобождён.
func TestExecQueuedTx_NormalPath(t *testing.T) {
	queue := [][][]byte{
		{[]byte("set"), []byte("a"), []byte("1")},
		{[]byte("get"), []byte("a")},
	}

	var gotHeader int
	var ran []string
	execQueuedTx(queue, func(n int) { gotHeader = n }, func(qCmd string, _ [][]byte) {
		ran = append(ran, qCmd)
	})

	if gotHeader != 2 {
		t.Fatalf("writeHeader(%d), want 2", gotHeader)
	}
	if len(ran) != 2 || ran[0] != "SET" || ran[1] != "GET" {
		t.Fatalf("выполнено %v, want [SET GET] (в верхнем регистре)", ran)
	}
	if !globalTxMu.TryLock() {
		t.Fatal("globalTxMu не освобождён после нормального EXEC")
	}
	globalTxMu.Unlock()
}
