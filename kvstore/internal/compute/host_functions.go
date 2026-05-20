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
		WithGoModuleFunction(api.GoModuleFunc(func(ctx context.Context, m api.Module, stack []uint64) {
			keyPtr := uint32(stack[0])
			keyLen := uint32(stack[1])

			// 1. Читаем ключ из WASM-памяти
			key, ok := m.Memory().Read(keyPtr, keyLen)
			if !ok {
				stack[0] = 0
				return
			}

			// 2. Получаем значение из Store
			val, found := e.StoreGet(string(key))
			if !found {
				stack[0] = 0
				return
			}

			// 3. Записываем значение обратно в WASM-память
			// Используем фиксированный offset (ValueOffset) для результата
			m.Memory().Write(ValueOffset, val)
			stack[0] = uint64(uint32(len(val)))
		}), []api.ValueType{api.ValueTypeI32, api.ValueTypeI32}, []api.ValueType{api.ValueTypeI32}).
		Export("kv_get").

		// ─── kv_set(key_ptr, key_len, val_ptr, val_len) → 0/1 ───
		// Записывает ключ-значение в Store + WAL.
		// Возвращает 1 при успехе, 0 при ошибке.
		NewFunctionBuilder().
		WithGoModuleFunction(api.GoModuleFunc(func(ctx context.Context, m api.Module, stack []uint64) {
			keyPtr := uint32(stack[0])
			keyLen := uint32(stack[1])
			valPtr := uint32(stack[2])
			valLen := uint32(stack[3])

			key, ok := m.Memory().Read(keyPtr, keyLen)
			if !ok {
				stack[0] = 0
				return
			}
			val, ok := m.Memory().Read(valPtr, valLen)
			if !ok {
				stack[0] = 0
				return
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
				stack[0] = 1
				return
			}
			writeFunc()
			stack[0] = 1
		}), []api.ValueType{api.ValueTypeI32, api.ValueTypeI32, api.ValueTypeI32, api.ValueTypeI32}, []api.ValueType{api.ValueTypeI32}).
		Export("kv_set").

		// ─── kv_del(key_ptr, key_len) → 0/1 ───
		// Удаляет ключ из Store + WAL.
		NewFunctionBuilder().
		WithGoModuleFunction(api.GoModuleFunc(func(ctx context.Context, m api.Module, stack []uint64) {
			keyPtr := uint32(stack[0])
			keyLen := uint32(stack[1])

			key, ok := m.Memory().Read(keyPtr, keyLen)
			if !ok {
				stack[0] = 0
				return
			}

			// BUG FIX: Используем StoreDelWithWAL для durability.
			if e.StoreDelWithWAL != nil {
				if err := e.StoreDelWithWAL(string(key)); err != nil {
					log.Printf("[wasm] kv_del WAL error: %v", err)
					stack[0] = 0
					return
				}
			} else {
				e.StoreDel(string(key))
			}
			stack[0] = 1
		}), []api.ValueType{api.ValueTypeI32, api.ValueTypeI32}, []api.ValueType{api.ValueTypeI32}).
		Export("kv_del").

		// ─── tx_begin() → 1 ───
		// Включает режим накопления команд в очередь для текущей WASM-сессии.
		NewFunctionBuilder().
		WithGoModuleFunction(api.GoModuleFunc(func(ctx context.Context, m api.Module, stack []uint64) {
			tx, ok := ctx.Value(execTxKey{}).(*WasmTxCtx)
			if ok {
				tx.InTx = true
				tx.Queue = make([]func(), 0, 10) // выделим заранее место
			}
			stack[0] = 1
		}), []api.ValueType{}, []api.ValueType{api.ValueTypeI32}).
		Export("tx_begin").

		// ─── tx_commit() → 0/1 ───
		// Выполняет всё накопленное под глобальным Lock-ом.
		NewFunctionBuilder().
		WithGoModuleFunction(api.GoModuleFunc(func(ctx context.Context, m api.Module, stack []uint64) {
			tx, ok := ctx.Value(execTxKey{}).(*WasmTxCtx)
			if !ok || !tx.InTx {
				stack[0] = 0 // Ошибка: коммит без tx_begin
				return
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
			stack[0] = 1
		}), []api.ValueType{}, []api.ValueType{api.ValueTypeI32}).
		Export("tx_commit").

		// ─── publish(chan_ptr, chan_len, msg_ptr, msg_len) → 0/1 ───
		// Публикует сообщение в Pub/Sub канал.
		NewFunctionBuilder().
		WithGoModuleFunction(api.GoModuleFunc(func(ctx context.Context, m api.Module, stack []uint64) {
			chanPtr := uint32(stack[0])
			chanLen := uint32(stack[1])
			msgPtr := uint32(stack[2])
			msgLen := uint32(stack[3])

			ch, ok := m.Memory().Read(chanPtr, chanLen)
			if !ok {
				stack[0] = 0
				return
			}
			msg, ok := m.Memory().Read(msgPtr, msgLen)
			if !ok {
				stack[0] = 0
				return
			}

			if e.Publish != nil {
				e.Publish(string(ch), string(msg))
			}
			stack[0] = 1
		}), []api.ValueType{api.ValueTypeI32, api.ValueTypeI32, api.ValueTypeI32, api.ValueTypeI32}, []api.ValueType{api.ValueTypeI32}).
		Export("publish").

		// ─── log_info(msg_ptr, msg_len) ───
		// Логирование из WASM-модуля (уровень INFO).
		NewFunctionBuilder().
		WithGoModuleFunction(api.GoModuleFunc(func(ctx context.Context, m api.Module, stack []uint64) {
			msgPtr := uint32(stack[0])
			msgLen := uint32(stack[1])
			msg, ok := m.Memory().Read(msgPtr, msgLen)
			if !ok {
				return
			}
			log.Printf("[wasm] %s", string(msg))
		}), []api.ValueType{api.ValueTypeI32, api.ValueTypeI32}, []api.ValueType{}).
		Export("log_info").

		// ─── log_error(msg_ptr, msg_len) ───
		// Логирование из WASM-модуля (уровень ERROR).
		NewFunctionBuilder().
		WithGoModuleFunction(api.GoModuleFunc(func(ctx context.Context, m api.Module, stack []uint64) {
			msgPtr := uint32(stack[0])
			msgLen := uint32(stack[1])
			msg, ok := m.Memory().Read(msgPtr, msgLen)
			if !ok {
				return
			}
			log.Printf("[wasm:ERROR] %s", string(msg))
		}), []api.ValueType{api.ValueTypeI32, api.ValueTypeI32}, []api.ValueType{}).
		Export("log_error").

		// ─── current_time_ms() → i64 ───
		// Возвращает текущее время в миллисекундах (Unix epoch).
		// WASM-модуль не имеет доступа к системным часам,
		// поэтому предоставляем время через host-функцию.
		NewFunctionBuilder().
		WithGoModuleFunction(api.GoModuleFunc(func(ctx context.Context, m api.Module, stack []uint64) {
			stack[0] = uint64(time.Now().UnixMilli())
		}), []api.ValueType{}, []api.ValueType{api.ValueTypeI64}).
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
		WithGoModuleFunction(api.GoModuleFunc(func(ctx context.Context, m api.Module, stack []uint64) {
			queryPtr := uint32(stack[0])
			dim := uint32(stack[1])
			k := uint32(stack[2])

			if e.VSimSearch == nil {
				stack[0] = 0
				return
			}

			// 1. Читаем query вектор из WASM-памяти
			//    Каждый float32 = 4 байта
			queryBytes, ok := m.Memory().Read(queryPtr, dim*4)
			if !ok {
				stack[0] = 0
				return
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

			stack[0] = uint64(count)
		}), []api.ValueType{api.ValueTypeI32, api.ValueTypeI32, api.ValueTypeI32}, []api.ValueType{api.ValueTypeI32}).
		Export("vsim_search").

		// ─── ai_embed(text_ptr, text_len) → dim ───
		//
		// Генерирует embedding через Ollama из WASM-модуля.
		// WASM кладёт текст в memory → Go читает → Ollama → вектор.
		// Результат ([]float32) записывается по offset 8192.
		// Возвращает размерность вектора (0 = ошибка или AI недоступен).
		NewFunctionBuilder().
		WithGoModuleFunction(api.GoModuleFunc(func(ctx context.Context, m api.Module, stack []uint64) {
			textPtr := uint32(stack[0])
			textLen := uint32(stack[1])

			if e.AIEmbed == nil {
				stack[0] = 0
				return
			}

			text, ok := m.Memory().Read(textPtr, textLen)
			if !ok {
				stack[0] = 0
				return
			}

			// Отдельный контекст: AI занимает секунды, не 10ms как WASM
			aiCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()

			embedding, err := e.AIEmbed(aiCtx, string(text))
			if err != nil {
				log.Printf("[wasm] ai_embed error: %v", err)
				stack[0] = 0
				return
			}

			// Записываем []float32 в WASM-memory по offset 8192
			offset := uint32(8192)
			for _, v := range embedding {
				m.Memory().WriteFloat32Le(offset, v)
				offset += 4
			}

			stack[0] = uint64(len(embedding))
		}), []api.ValueType{api.ValueTypeI32, api.ValueTypeI32}, []api.ValueType{api.ValueTypeI32}).
		Export("ai_embed").

		// ─── ai_chat(prompt_ptr, prompt_len) → response_len ───
		//
		// Отправляет промпт в LLM (Gemma) из WASM-модуля.
		// Результат (строка ответа) записывается по offset 16384.
		// Возвращает длину ответа в байтах (0 = ошибка).
		NewFunctionBuilder().
		WithGoModuleFunction(api.GoModuleFunc(func(ctx context.Context, m api.Module, stack []uint64) {
			promptPtr := uint32(stack[0])
			promptLen := uint32(stack[1])

			if e.AIChat == nil {
				stack[0] = 0
				return
			}

			prompt, ok := m.Memory().Read(promptPtr, promptLen)
			if !ok {
				stack[0] = 0
				return
			}

			aiCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()

			response, err := e.AIChat(aiCtx, string(prompt))
			if err != nil {
				log.Printf("[wasm] ai_chat error: %v", err)
				stack[0] = 0
				return
			}

			respBytes := []byte(response)
			if !m.Memory().Write(16384, respBytes) {
				stack[0] = 0
				return
			}

			stack[0] = uint64(len(respBytes))
		}), []api.ValueType{api.ValueTypeI32, api.ValueTypeI32}, []api.ValueType{api.ValueTypeI32}).
		Export("ai_chat")
}
