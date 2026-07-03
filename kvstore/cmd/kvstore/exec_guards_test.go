package main

import "testing"

// Общие ветки диспетчера: неизвестная команда, WASM-заглушка, pub/sub счётчик,
// кластер выключен.

func TestExec_UnknownCommand(t *testing.T) {
	e := newExecEnv(t)
	// Неизвестная команда → явная ошибка, а не тишина/паника.
	e.wantErrPrefix(e.do("NOSUCHCMD", "x"), "ERR unknown command")
}

func TestExec_WasmDisabledInProdBuild(t *testing.T) {
	e := newExecEnv(t)
	// Прод-сборка (без -tags experimental): WASM.* обслуживает nopCompute →
	// явная ошибка «выключено», а не выполнение произвольного модуля (ACE-фенс).
	e.wantErrPrefix(e.do("WASM.EXEC", "module"), "ERR WASM support is disabled")
}

func TestExec_PublishNoSubscribers(t *testing.T) {
	e := newExecEnv(t)
	// PUBLISH без подписчиков → 0 доставок.
	e.wantInt(e.do("PUBLISH", "chan", "hello"), 0)
	// Неверная арность.
	e.wantErrPrefix(e.do("PUBLISH", "chan"), "ERR")
}

func TestExec_ClusterDisabled(t *testing.T) {
	e := newExecEnv(t)
	// cl=nil в харнессе → cluster-команды отвечают «not enabled», не паникуют.
	e.wantErrPrefix(e.do("CLUSTER", "INFO"), "ERR cluster mode is not enabled")
	e.wantErrPrefix(e.do("MIGRATE", "h", "p", "k"), "ERR cluster mode is not enabled")
}

// TestExec_OomGateBlocksGrowingWrites — единый OOM-гейт в начале executeCommand.
//
// Растущие в памяти команды (SET/ZADD/VSIM.ADD) отклоняются при used>=maxmemory,
// а удаляющие/читающие — НЕТ (иначе из OOM не выйти освобождением). Это как раз
// класс багов, ради которого гейт вынесли из одного лишь SET (ZADD/VSIM.ADD
// раньше шли мимо и уводили процесс в OOM).
func TestExec_OomGateBlocksGrowingWrites(t *testing.T) {
	e := newExecEnv(t)

	e.do("SET", "seed", "allocate-some-memory") // поднять usedBytes > 0
	e.s.SetMaxMemory(1)                         // теперь IsOOM() = true

	// Растущие команды отклоняются с OOM.
	e.wantErrPrefix(e.do("SET", "k", "v"), "OOM")
	e.wantErrPrefix(e.do("ZADD", "z", "1", "m"), "OOM")
	e.wantErrPrefix(e.do("VSIM.ADD", "vec", "1", "2", "3"), "OOM")

	// Читающие/удаляющие проходят мимо гейта (нужны для выхода из OOM).
	if v := e.do("GET", "seed"); v.Typ == '-' {
		t.Fatalf("GET не должен блокироваться OOM-гейтом, got ошибку %q", v.Str)
	}
	e.wantInt(e.do("DEL", "seed"), 1)
}

// TestExec_FailStopBlocksWritesAllowsReads — durability fail-stop гейт.
//
// Когда WAL перестал durable-писать (ENOSPC/I/O), сервер не может честно
// подтверждать мутации → ВСЕ пишущие команды (включая удаляющие, в отличие от
// OOM) отклоняются, чтение остаётся доступным для снятия данных.
func TestExec_FailStopBlocksWritesAllowsReads(t *testing.T) {
	e := newExecEnv(t)

	// Предусловие: положим данные ДО поломки WAL, чтобы было что читать.
	e.wantSimple(e.do("SET", "k", "v"), "OK")

	// Симулируем отказ диска: закрываем fd под WAL и форсим Sync → латч Failed().
	raw := e.bw.RawWAL()
	_ = raw.Close()
	_ = raw.Sync() // fsync на закрытом fd → w.fail(err) взводит fail-stop
	if e.bw.Failed() == nil {
		t.Fatal("предусловие: не удалось перевести WAL в fail-stop")
	}

	// Пишущие команды отклоняются (в т.ч. удаляющая DEL — на полном диске
	// нельзя записать даже удаление).
	e.wantErrPrefix(e.do("SET", "k2", "v2"), "WAL persistence failed")
	e.wantErrPrefix(e.do("DEL", "k"), "WAL persistence failed")

	// Чтение по-прежнему обслуживается.
	e.wantBulk(e.do("GET", "k"), "v")
}
