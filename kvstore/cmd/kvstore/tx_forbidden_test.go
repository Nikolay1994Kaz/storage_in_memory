package main

import (
	"net"
	"testing"

	"kvstore/kvstore/internal/server"
)

// newTxConnState — ConnState в режиме транзакции с рабочим Buf.
// Записи идут в in-memory wbuf (без Flush → без I/O), поэтому pipe не читаем.
func newTxConnState(t *testing.T) *server.ConnState {
	t.Helper()
	_, srv := net.Pipe()
	t.Cleanup(func() { srv.Close() })
	return &server.ConnState{Conn: srv, Buf: server.NewConnBuf(srv), InTx: true}
}

// TestForbiddenInTx_List — страж списка запрещённых в MULTI команд (H2).
func TestForbiddenInTx_List(t *testing.T) {
	for _, c := range []string{"SUBSCRIBE", "UNSUBSCRIBE", "VSIM.SUBSCRIBE", "VSIM.UNSUBSCRIBE"} {
		if !forbiddenInTx(c) {
			t.Errorf("%q обязана быть запрещена в MULTI — subscriber-mode пишет мимо cs.Buf и рвёт EXEC-кадр", c)
		}
	}
	for _, c := range []string{"SET", "GET", "PUBLISH", "ZADD", "DEL", "TTL"} {
		if forbiddenInTx(c) {
			t.Errorf("%q не должна быть запрещена в MULTI", c)
		}
	}
}

// TestQueueTxCommand_RejectsSubscribe — страж H2.
//
// SUBSCRIBE в MULTI не должна попадать в очередь (иначе EXEC выполнит её,
// напишет 0 ответов при обещанном ArrayHeader(N) → RESP-десинк + writePump =
// второй писатель). Должна помечать транзакцию на отмену.
func TestQueueTxCommand_RejectsSubscribe(t *testing.T) {
	cs := newTxConnState(t)
	queueTxCommand(cs, [][]byte{[]byte("SUBSCRIBE"), []byte("news")}, "SUBSCRIBE")

	if !cs.TxAborted {
		t.Fatal("H2: SUBSCRIBE в MULTI обязана пометить tx на отмену (TxAborted) → EXEC вернёт EXECABORT")
	}
	if len(cs.TxQueue) != 0 {
		t.Fatalf("H2: SUBSCRIBE НЕ должна ставиться в очередь, got %d элементов", len(cs.TxQueue))
	}
}

// TestQueueTxCommand_QueuesNormal — обычная команда ставится в очередь с копией
// аргументов (ring buffer будет перезаписан).
func TestQueueTxCommand_QueuesNormal(t *testing.T) {
	cs := newTxConnState(t)
	args := [][]byte{[]byte("SET"), []byte("k"), []byte("v")}
	queueTxCommand(cs, args, "SET")

	if cs.TxAborted {
		t.Fatal("обычная команда не должна отменять транзакцию")
	}
	if len(cs.TxQueue) != 1 {
		t.Fatalf("SET должна попасть в очередь, got %d", len(cs.TxQueue))
	}
	args[1][0] = 'X' // имитируем перезапись ring buffer
	if string(cs.TxQueue[0][1]) != "k" {
		t.Fatal("H2/корректность: аргументы обязаны копироваться при постановке в очередь")
	}
}
