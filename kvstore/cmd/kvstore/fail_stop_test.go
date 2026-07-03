package main

import "testing"

// TestIsWriteCmd — страж классификации durability fail-stop гейта.
//
// Регресс здесь = либо мутация проскочит мимо гейта (клиент получит OK на
// запись, потерянную из-за сломанного WAL), либо read-команда окажется
// заблокирована (нельзя будет снять данные из read-only режима).
//
// Ключевое отличие от OOM-гейта (isMemoryGrowingCmd): удаляющие команды здесь
// ДОЛЖНЫ блокироваться (на полном диске нельзя записать даже удаление), тогда
// как в OOM они разрешены для выхода из состояния нехватки памяти.
func TestIsWriteCmd(t *testing.T) {
	writes := []string{
		"SET", "DEL", "EXPIRE", "PERSIST",
		"VSIM.ADD", "VSIM.ADDBIN", "VSIM.DEL",
		"ZADD", "ZREM", "AI.INGEST",
	}
	for _, c := range writes {
		if !isWriteCmd(c) {
			t.Errorf("%q должна быть write-командой — иначе fail-stop её пропустит и клиент получит OK на потерянную запись", c)
		}
	}

	// Удаления обязаны попадать под fail-stop (в отличие от OOM-гейта).
	for _, c := range []string{"DEL", "VSIM.DEL", "ZREM", "EXPIRE", "PERSIST"} {
		if isMemoryGrowingCmd(c) {
			t.Errorf("%q не должна быть в OOM-гейте (удаления освобождают память)", c)
		}
	}

	reads := []string{"GET", "PING", "TTL", "ZSCORE", "ZCARD", "VSIM.SEARCH", "VSIM.INFO", "DBSIZE", "SUBSCRIBE"}
	for _, c := range reads {
		if isWriteCmd(c) {
			t.Errorf("%q не мутирует — fail-stop не должен её блокировать (нужен доступ к данным в read-only)", c)
		}
	}
}
