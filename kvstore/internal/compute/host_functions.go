package compute

import (
	"context"
	"log"
	"time"

	"github.com/tetratelabs/wazero/api"
)

// registerHostFunctions регистрирует Go-функции,
// которые WASM-модуль может вызывать.
//
// Проблема: WASM работает только с числами (i32, i64).
// Строки передаются через линейную память WASM:
//   - WASM-модуль кладёт строку в свою memory по offset
//   - Передаёт (offset, length) как i32
//   - Go-код читает байты из memory по этому offset
func (e *Engine) registerHostFunctions(ctx context.Context) (api.Module, error) {
	return e.runtime.NewHostModuleBuilder("env").

		// ─── kv_get(key_ptr, key_len) → val_len ───
		// Читает значение из Store по ключу.
		// Результат записывается в WASM-memory начиная с offset 1024.
		// Возвращает длину значения (0 = не найден).
		NewFunctionBuilder().
		WithFunc(func(ctx context.Context, m api.Module, keyPtr, keyLen uint32) uint32 {
			// 1. Читаем ключ из WASM-памяти
			key, ok := m.Memory().Read(keyPtr, keyLen)
			if !ok {
				return 0
			}

			// 2. Получаем значение из Store
			val, found := e.StoreGet(string(key))
			if !found {
				return 0
			}

			// 3. Записываем значение обратно в WASM-память
			// Используем фиксированный offset (1024) для результата
			m.Memory().Write(1024, val)
			return uint32(len(val))
		}).
		Export("kv_get").

		// ─── kv_set(key_ptr, key_len, val_ptr, val_len) → 0/1 ───
		// Записывает ключ-значение в Store + WAL.
		// Возвращает 1 при успехе, 0 при ошибке.
		NewFunctionBuilder().
		WithFunc(func(ctx context.Context, m api.Module, keyPtr, keyLen, valPtr, valLen uint32) uint32 {
			key, ok := m.Memory().Read(keyPtr, keyLen)
			if !ok {
				return 0
			}
			val, ok := m.Memory().Read(valPtr, valLen)
			if !ok {
				return 0
			}

			// Копируем — WASM-memory может быть переиспользована
			keyCopy := make([]byte, len(key))
			valCopy := make([]byte, len(val))
			copy(keyCopy, key)
			copy(valCopy, val)

			// BUG FIX: Используем StoreSetWithWAL для durability.
			// Раньше данные из WASM терялись при рестарте.
			tx, ok := ctx.Value(execTxKey{}).(*WasmTxCtx)
			writeFunc := func() {
				if e.StoreSetWithWAL != nil {
					if err := e.StoreSetWithWAL(string(keyCopy), valCopy); err != nil {
						log.Printf("[wasm] kv_set WAL error: %v", err)
					}
				} else {
					e.StoreSet(string(keyCopy), valCopy)
				}
			}
			if ok && tx.InTx {
				tx.Queue = append(tx.Queue, writeFunc)
				return 1
			}
			writeFunc()
			return 1
		}).
		Export("kv_set").

		// ─── kv_del(key_ptr, key_len) → 0/1 ───
		// Удаляет ключ из Store + WAL.
		NewFunctionBuilder().
		WithFunc(func(ctx context.Context, m api.Module, keyPtr, keyLen uint32) uint32 {
			key, ok := m.Memory().Read(keyPtr, keyLen)
			if !ok {
				return 0
			}

			// BUG FIX: Используем StoreDelWithWAL для durability.
			if e.StoreDelWithWAL != nil {
				if err := e.StoreDelWithWAL(string(key)); err != nil {
					log.Printf("[wasm] kv_del WAL error: %v", err)
					return 0
				}
			} else {
				e.StoreDel(string(key))
			}
			return 1
		}).
		Export("kv_del").
		// ─── tx_begin() → 1 ───
		// Включает режим накопления команд в очередь для текущей WASM-сессии.
		NewFunctionBuilder().
		WithFunc(func(ctx context.Context, m api.Module) uint32 {
			tx, ok := ctx.Value(execTxKey{}).(*WasmTxCtx)
			if ok {
				tx.InTx = true
				tx.Queue = make([]func(), 0, 10) // выделим заранее место
			}
			return 1
		}).
		Export("tx_begin").

		// ─── tx_commit() → 0/1 ───
		// Выполняет всё накопленное под глобальным Lock-ом.
		NewFunctionBuilder().
		WithFunc(func(ctx context.Context, m api.Module) uint32 {
			tx, ok := ctx.Value(execTxKey{}).(*WasmTxCtx)
			if !ok || !tx.InTx {
				return 0 // Ошибка: коммит без tx_begin
			}

			// 1. БЕРЕМ ГЛОБАЛЬНЫЙ ЛОК (Останавливаем мир!)
			if e.GlobalLock != nil {
				e.GlobalLock()
			}

			// 2. ВЫПОЛНЯЕМ ВСЮ ОЧЕРЕДЬ
			for _, op := range tx.Queue {
				op()
			}

			// 3. ОТПУСКАЕМ ЛОК (Мир продолжает работу)
			if e.GlobalUnlock != nil {
				e.GlobalUnlock()
			}

			// Очищаем состояние
			tx.InTx = false
			tx.Queue = nil
			return 1
		}).
		Export("tx_commit").

		// ─── publish(chan_ptr, chan_len, msg_ptr, msg_len) → 0/1 ───
		// Публикует сообщение в Pub/Sub канал.
		NewFunctionBuilder().
		WithFunc(func(ctx context.Context, m api.Module, chanPtr, chanLen, msgPtr, msgLen uint32) uint32 {
			ch, ok := m.Memory().Read(chanPtr, chanLen)
			if !ok {
				return 0
			}
			msg, ok := m.Memory().Read(msgPtr, msgLen)
			if !ok {
				return 0
			}

			if e.Publish != nil {
				e.Publish(string(ch), string(msg))
			}
			return 1
		}).
		Export("publish").

		// ─── log_info(msg_ptr, msg_len) ───
		// Логирование из WASM-модуля (уровень INFO).
		NewFunctionBuilder().
		WithFunc(func(ctx context.Context, m api.Module, msgPtr, msgLen uint32) {
			msg, ok := m.Memory().Read(msgPtr, msgLen)
			if !ok {
				return
			}
			log.Printf("[wasm] %s", string(msg))
		}).
		Export("log_info").

		// ─── log_error(msg_ptr, msg_len) ───
		// Логирование из WASM-модуля (уровень ERROR).
		NewFunctionBuilder().
		WithFunc(func(ctx context.Context, m api.Module, msgPtr, msgLen uint32) {
			msg, ok := m.Memory().Read(msgPtr, msgLen)
			if !ok {
				return
			}
			log.Printf("[wasm:ERROR] %s", string(msg))
		}).
		Export("log_error").

		// ─── current_time_ms() → i64 ───
		// Возвращает текущее время в миллисекундах (Unix epoch).
		// WASM-модуль не имеет доступа к системным часам,
		// поэтому предоставляем время через host-функцию.
		NewFunctionBuilder().
		WithFunc(func(ctx context.Context) uint64 {
			return uint64(time.Now().UnixMilli())
		}).
		Export("current_time_ms").

		// Компилируем и создаём host-модуль
		Instantiate(ctx)
}
