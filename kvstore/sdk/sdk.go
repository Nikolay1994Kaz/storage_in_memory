//go:build tinygo.wasm

package sdk

import (
	"unsafe"
)

// ─── WASM Host Imports ──────────────────────────────────────

//go:wasmimport env kv_get
func kvGet(keyPtr, keyLen uint32) uint32

//go:wasmimport env kv_set
func kvSet(keyPtr, keyLen, valPtr, valLen uint32) uint32

//go:wasmimport env kv_del
func kvDel(keyPtr, keyLen uint32) uint32

//go:wasmimport env publish
func kvPublish(chanPtr, chanLen, msgPtr, msgLen uint32) uint32

//go:wasmimport env tx_begin
func txBegin() uint32

//go:wasmimport env tx_commit
func txCommit() uint32

//go:wasmimport env log_info
func logInfo(msgPtr, msgLen uint32)

//go:wasmimport env log_error
func logError(msgPtr, msgLen uint32)

//go:wasmimport env current_time_ms
func currentTimeMs() int64

//go:wasmimport env vsim_search
func vsimSearch(queryPtr, dim, k uint32) uint32

//go:wasmimport env ai_embed
func aiEmbed(textPtr, textLen uint32) uint32

//go:wasmimport env ai_chat
func aiChat(promptPtr, promptLen uint32) uint32

// ─── Memory Layout Offsets ──────────────────────────────────

const (
	InputOffset  uint32 = 16384 // Входной буфер (Host -> Guest)
	ValueOffset  uint32 = 20480 // Буфер для результатов kv_get (Host -> Guest)
	OutputOffset uint32 = 24576 // Выходной буфер (Guest -> Host)

	vsimResultOffset uint32 = 4096  // Буфер результатов vsim_search
	aiEmbedOffset    uint32 = 8192  // Буфер результатов ai_embed
	aiChatOffset     uint32 = 16384 // Буфер результатов ai_chat
)

// VSimResult представляет один результат векторного поиска
type VSimResult struct {
	Key      string
	Distance float32
}

// ─── Helper Functions ───────────────────────────────────────

func ptr(b []byte) uint32 {
	if len(b) == 0 {
		return 0
	}
	return uint32(uintptr(unsafe.Pointer(&b[0])))
}

// ─── Public API ─────────────────────────────────────────────

// GetInputKey извлекает ключ запуска в виде строки из переданного хостом указателя
func GetInputKey(keyPtr, keyLen uint32) string {
	if keyLen == 0 {
		return ""
	}
	keyBytes := unsafe.Slice((*byte)(unsafe.Pointer(uintptr(keyPtr))), int(keyLen))
	return string(keyBytes)
}

// SliceAt возвращает view на слайс байт по указанному смещению и длине (zero-alloc)
func SliceAt(offset, length uint32) []byte {
	if length == 0 {
		return nil
	}
	return unsafe.Slice((*byte)(unsafe.Pointer(uintptr(offset))), int(length))
}



// Get возвращает значение по ключу из KV-хранилища (копирует данные в новый слайс)
func Get(key string) ([]byte, bool) {
	keyBytes := []byte(key)
	valLen := kvGet(ptr(keyBytes), uint32(len(keyBytes)))
	if valLen == 0 {
		return nil, false
	}
	rawVal := unsafe.Slice((*byte)(unsafe.Pointer(uintptr(ValueOffset))), int(valLen))
	
	res := make([]byte, valLen)
	copy(res, rawVal)
	return res, true
}

// GetView возвращает read-only view на значение в буфере хоста (zero-alloc).
// ВНИМАНИЕ: данные будут перезаписаны при следующем вызове чтения (Get/GetView).
func GetView(key string) ([]byte, bool) {
	keyBytes := []byte(key)
	valLen := kvGet(ptr(keyBytes), uint32(len(keyBytes)))
	if valLen == 0 {
		return nil, false
	}
	return unsafe.Slice((*byte)(unsafe.Pointer(uintptr(ValueOffset))), int(valLen)), true
}

// GetViewBytes — то же самое, но принимает ключ в виде слайса байт (абсолютный zero-alloc).
func GetViewBytes(key []byte) ([]byte, bool) {
	valLen := kvGet(ptr(key), uint32(len(key)))
	if valLen == 0 {
		return nil, false
	}
	return unsafe.Slice((*byte)(unsafe.Pointer(uintptr(ValueOffset))), int(valLen)), true
}


// Set записывает значение по ключу в KV-хранилище (с записью в WAL)
func Set(key string, val []byte) bool {
	keyBytes := []byte(key)
	res := kvSet(ptr(keyBytes), uint32(len(keyBytes)), ptr(val), uint32(len(val)))
	return res == 1
}

// SetBytes — то же самое, но принимает ключ в виде слайса байт (zero-alloc).
func SetBytes(key, val []byte) bool {
	res := kvSet(ptr(key), uint32(len(key)), ptr(val), uint32(len(val)))
	return res == 1
}

// Delete удаляет значение по ключу из KV-хранилища (с записью в WAL)
func Delete(key string) bool {
	keyBytes := []byte(key)
	res := kvDel(ptr(keyBytes), uint32(len(keyBytes)))
	return res == 1
}

// Publish публикует сообщение в Pub/Sub канал
func Publish(channel, message string) bool {
	chBytes := []byte(channel)
	msgBytes := []byte(message)
	res := kvPublish(ptr(chBytes), uint32(len(chBytes)), ptr(msgBytes), uint32(len(msgBytes)))
	return res == 1
}

// PublishBytes — то же самое, но принимает канал и сообщение в виде слайсов байт (zero-alloc).
func PublishBytes(channel, message []byte) bool {
	res := kvPublish(ptr(channel), uint32(len(channel)), ptr(message), uint32(len(message)))
	return res == 1
}


// TxBegin открывает транзакцию в контексте WASM (команды группируются)
func TxBegin() bool {
	return txBegin() == 1
}

// TxCommit фиксирует все изменения транзакции под глобальным Lock-ом хоста
func TxCommit() bool {
	return txCommit() == 1
}

// LogInfo отправляет лог уровня INFO на сторону сервера
func LogInfo(msg string) {
	msgBytes := []byte(msg)
	logInfo(ptr(msgBytes), uint32(len(msgBytes)))
}

// LogError отправляет лог уровня ERROR на сторону сервера
func LogError(msg string) {
	msgBytes := []byte(msg)
	logError(ptr(msgBytes), uint32(len(msgBytes)))
}

// CurrentTimeMs возвращает текущее Unix-время хоста в миллисекундах
func CurrentTimeMs() int64 {
	return currentTimeMs()
}

// VSimSearch производит векторный поиск по графу HNSW
func VSimSearch(query []float32, k int) []VSimResult {
	if len(query) == 0 {
		return nil
	}
	queryPtr := uint32(uintptr(unsafe.Pointer(&query[0])))
	count := vsimSearch(queryPtr, uint32(len(query)), uint32(k))
	if count == 0 {
		return nil
	}

	results := make([]VSimResult, count)
	offset := vsimResultOffset

	for i := uint32(0); i < count; i++ {
		// Читаем длину ключа (4 байта)
		keyLen := *(*uint32)(unsafe.Pointer(uintptr(offset)))
		offset += 4

		// Читаем сам ключ
		keyBytes := unsafe.Slice((*byte)(unsafe.Pointer(uintptr(offset))), int(keyLen))
		key := string(keyBytes)
		offset += keyLen

		// Читаем дистанцию (float32 - 4 байта)
		dist := *(*float32)(unsafe.Pointer(uintptr(offset)))
		offset += 4

		results[i] = VSimResult{
			Key:      key,
			Distance: dist,
		}
	}

	return results
}

// AIEmbed генерирует вектор эмбеддингов для указанного текста через Ollama
func AIEmbed(text string) []float32 {
	textBytes := []byte(text)
	dim := aiEmbed(ptr(textBytes), uint32(len(textBytes)))
	if dim == 0 {
		return nil
	}

	// Указываем на буфер результатов ai_embed
	resBytes := unsafe.Slice((*float32)(unsafe.Pointer(uintptr(aiEmbedOffset))), int(dim))
	
	// Возвращаем копию для безопасности
	res := make([]float32, dim)
	copy(res, resBytes)
	return res
}

// AIChat отправляет промпт в LLM через Ollama и возвращает текстовый ответ
func AIChat(prompt string) string {
	promptBytes := []byte(prompt)
	respLen := aiChat(ptr(promptBytes), uint32(len(promptBytes)))
	if respLen == 0 {
		return ""
	}

	// Указываем на буфер результатов ai_chat
	respBytes := unsafe.Slice((*byte)(unsafe.Pointer(uintptr(aiChatOffset))), int(respLen))
	return string(respBytes)
}

// WriteOutput записывает финальный результат работы модуля в выходной буфер хоста
func WriteOutput(data []byte) uint32 {
	if len(data) == 0 {
		return 0
	}
	outSlice := unsafe.Slice((*byte)(unsafe.Pointer(uintptr(OutputOffset))), len(data))
	copy(outSlice, data)
	return uint32(len(data))
}
