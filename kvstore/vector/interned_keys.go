package vector

import "unsafe"

// =============================================================================
// internedKeys — N пользовательских ключей в ОДНОМ байтовом блобе + оффсетах.
//
// Зачем: []string на N векторов — это N заголовков-строк, и каждый содержит
// указатель. GC обходит эти N указателей КАЖДЫЙ цикл сборки. Для in-memory БД,
// держащей 1М+ векторов годами, это миллионы указателей на скан за цикл →
// GC-CPU и хвост p99-латентности (это НЕ видно в allocs/op бенча — это
// steady-state цена постоянно живущей структуры).
//
// internedKeys заменяет []string на []byte (blob) + []uint32 (offs). Ни один из
// них не содержит внутренних указателей → GC полностью пропускает содержимое
// (сканирует лишь два заголовка слайса вместо N). Ключ i = blob[offs[i]:offs[i+1]].
//
// Пустой ключ (offs[i] == offs[i+1]) трактуется как tombstone (эквивалент "").
// Структура иммутабельна после построения → безопасна для конкурентного чтения.
// =============================================================================
type internedKeys struct {
	blob []byte
	offs []uint32 // len = n+1; offs[0]=0; offs[i+1] >= offs[i]
}

// count возвращает число ключей.
func (k internedKeys) count() int {
	if len(k.offs) == 0 {
		return 0
	}
	return len(k.offs) - 1
}

// view возвращает ZERO-COPY представление ключа i поверх blob.
//
// ВАЖНО: результат валиден, ТОЛЬКО пока жива таблица (т.е. жив сегмент).
// Использовать исключительно для чтения: сравнения, map-lookup, передачи в
// filterFn. НЕЛЬЗЯ сохранять/возвращать наружу за пределы времени жизни
// сегмента (иначе use-after-free после compaction) — для этого clone.
func (k internedKeys) view(i int) string {
	s, e := k.offs[i], k.offs[i+1]
	if s == e {
		return ""
	}
	return unsafe.String(&k.blob[s], int(e-s))
}

// clone возвращает БЕЗОПАСНУЮ (скопированную) строку ключа i — для возврата
// наружу или хранения в структурах, переживающих сегмент.
func (k internedKeys) clone(i int) string {
	s, e := k.offs[i], k.offs[i+1]
	if s == e {
		return ""
	}
	return string(k.blob[s:e])
}

// buildInternedKeys упаковывает []string → blob+offs (используется при freeze).
func buildInternedKeys(keys []string) internedKeys {
	total := 0
	for _, s := range keys {
		total += len(s)
	}
	blob := make([]byte, 0, total)
	offs := make([]uint32, len(keys)+1)
	for i, s := range keys {
		blob = append(blob, s...)
		offs[i+1] = uint32(len(blob))
	}
	return internedKeys{blob: blob, offs: offs}
}
