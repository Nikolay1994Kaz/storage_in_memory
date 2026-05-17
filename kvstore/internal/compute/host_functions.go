package compute

import (
	"context"
	"log"
	"math"
	"time"

	"github.com/tetratelabs/wazero"
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
	return e.buildHostModule(e.runtime).Instantiate(ctx)
}

// registerHostFunctionsOn регистрирует host-функции на указанном runtime.
// Используется WorkerLocalEngine для создания отдельного runtime.
func (e *Engine) registerHostFunctionsOn(rt wazero.Runtime) {
	ctx := context.Background()
	if _, err := e.buildHostModule(rt).Instantiate(ctx); err != nil {
		log.Fatalf("[wasm] Failed to register host functions on worker-local runtime: %v", err)
	}
}

// buildHostModule создаёт HostModuleBuilder с host-функциями.
func (e *Engine) buildHostModule(rt wazero.Runtime) wazero.HostModuleBuilder {
	return rt.NewHostModuleBuilder("env").

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
			// Используем фиксированный offset (ValueOffset) для результата
			m.Memory().Write(ValueOffset, val)
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

		// ─── vsim_search(query_ptr, dim, k) → result_count ───
		//
		// Семантический поиск из WASM-модуля.
		// WASM-модуль кладёт query-вектор ([]float32) в свою memory,
		// передаёт (ptr, dimension, K).
		//
		// Результаты записываются в WASM-memory начиная с offset 4096:
		// Формат: [count:u32] [key1_len:u32] [key1_bytes...] [dist1:f32] [key2_len:u32] ...
		//
		// Возвращает количество найденных результатов.
		NewFunctionBuilder().
		WithFunc(func(ctx context.Context, m api.Module, queryPtr, dim, k uint32) uint32 {
			if e.VSimSearch == nil {
				return 0
			}

			// 1. Читаем query вектор из WASM-памяти
			//    Каждый float32 = 4 байта
			queryBytes, ok := m.Memory().Read(queryPtr, dim*4)
			if !ok {
				return 0
			}

			// 2. Конвертируем []byte → []float32
			query := make([]float32, dim)
			for i := uint32(0); i < dim; i++ {
				bits := uint32(queryBytes[i*4]) |
					uint32(queryBytes[i*4+1])<<8 |
					uint32(queryBytes[i*4+2])<<16 |
					uint32(queryBytes[i*4+3])<<24
				query[i] = math.Float32frombits(bits)
			}

			// 3. Вызываем поиск
			results := e.VSimSearch(query, int(k))

			// 4. Записываем результаты в WASM-memory по offset 4096
			//    Формат: [key_len:u32][key_bytes...][dist:f32] для каждого результата
			offset := uint32(4096)
			count := uint32(0)

			for _, r := range results {
				keyBytes := []byte(r.Key)
				keyLen := uint32(len(keyBytes))

				// key length (4 bytes, little-endian)
				m.Memory().WriteUint32Le(offset, keyLen)
				offset += 4

				// key bytes
				m.Memory().Write(offset, keyBytes)
				offset += keyLen

				// distance (float32 as 4 bytes, little-endian)
				m.Memory().WriteFloat32Le(offset, r.Distance)
				offset += 4

				count++

				// Защита от переполнения памяти
				if offset > 60000 {
					break
				}
			}

			return count
		}).
		Export("vsim_search").

		// ─── ai_embed(text_ptr, text_len) → dim ───
		//
		// Генерирует embedding через Ollama из WASM-модуля.
		// WASM кладёт текст в memory → Go читает → Ollama → вектор.
		// Результат ([]float32) записывается по offset 8192.
		// Возвращает размерность вектора (0 = ошибка или AI недоступен).
		NewFunctionBuilder().
		WithFunc(func(ctx context.Context, m api.Module, textPtr, textLen uint32) uint32 {
			if e.AIEmbed == nil {
				return 0
			}

			text, ok := m.Memory().Read(textPtr, textLen)
			if !ok {
				return 0
			}

			// Отдельный контекст: AI занимает секунды, не 10ms как WASM
			aiCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()

			embedding, err := e.AIEmbed(aiCtx, string(text))
			if err != nil {
				log.Printf("[wasm] ai_embed error: %v", err)
				return 0
			}

			// Записываем []float32 в WASM-memory по offset 8192
			offset := uint32(8192)
			for _, v := range embedding {
				m.Memory().WriteFloat32Le(offset, v)
				offset += 4
			}

			return uint32(len(embedding))
		}).
		Export("ai_embed").

		// ─── ai_chat(prompt_ptr, prompt_len) → response_len ───
		//
		// Отправляет промпт в LLM (Gemma) из WASM-модуля.
		// Результат (строка ответа) записывается по offset 16384.
		// Возвращает длину ответа в байтах (0 = ошибка).
		NewFunctionBuilder().
		WithFunc(func(ctx context.Context, m api.Module, promptPtr, promptLen uint32) uint32 {
			if e.AIChat == nil {
				return 0
			}

			prompt, ok := m.Memory().Read(promptPtr, promptLen)
			if !ok {
				return 0
			}

			aiCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()

			response, err := e.AIChat(aiCtx, string(prompt))
			if err != nil {
				log.Printf("[wasm] ai_chat error: %v", err)
				return 0
			}

			respBytes := []byte(response)
			if !m.Memory().Write(16384, respBytes) {
				return 0
			}

			return uint32(len(respBytes))
		}).
		Export("ai_chat")
}
