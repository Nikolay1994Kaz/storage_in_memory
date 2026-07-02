package tcmalloc

import (
	"fmt"
	"sync"
	"testing"
)

// ─── Handle: encode/decode ─────────────────────────────────
//
// Handle упаковывает (spanID, objIndex) в один uint64.
// Если Handle сломан — Get/Del будут читать/удалять ЧУЖИЕ данные.
// Это фундамент — тестируем первым.

func TestHandle_PackUnpack(t *testing.T) {
	tests := []struct {
		spanID   uint32
		objIndex int
	}{
		{0, 0},          // минимальные значения
		{1, 5},          // типичный случай
		{1000, 63},      // span #1000, объект #63
		{0xFFFFFFFF, 0}, // максимальный spanID
		{0, 0x7FFFFFFF}, // большой objIndex
	}

	for _, tt := range tests {
		name := fmt.Sprintf("span%d_obj%d", tt.spanID, tt.objIndex)
		t.Run(name, func(t *testing.T) {
			h := MakeHandle(tt.spanID, tt.objIndex)

			if h.SpanID() != tt.spanID {
				t.Fatalf("SpanID: got %d, want %d", h.SpanID(), tt.spanID)
			}
			if h.ObjIndex() != tt.objIndex {
				t.Fatalf("ObjIndex: got %d, want %d", h.ObjIndex(), tt.objIndex)
			}
		})
	}
}

// Два разных Handle не должны быть равны
func TestHandle_Uniqueness(t *testing.T) {
	h1 := MakeHandle(1, 0)
	h2 := MakeHandle(0, 1)
	h3 := MakeHandle(1, 1)

	if h1 == h2 {
		t.Fatal("different spanID/objIndex produced same Handle")
	}
	if h1 == h3 || h2 == h3 {
		t.Fatal("different Handles should not be equal")
	}
}

// ─── SizeClass ─────────────────────────────────────────────
//
// SizeClassForSize определяет в какой "ящик" попадёт объект.
// Ошибка здесь = данные не влезут в выделенный блок → corruption.

func TestSizeClassForSize(t *testing.T) {
	tests := []struct {
		size      int
		wantClass int
	}{
		{1, 0},  // 1B → class 0 (32B)
		{32, 0}, // ровно 32B → class 0
		{33, 1}, // 33B → class 1 (64B), не влезает в 32
		{64, 1}, // ровно 64
		{65, 2}, // → class 2 (128B)
		{128, 2},
		{4096, 7},   // максимальный span-класс
		{4097, -1},  // > 4096 → large object
		{10000, -1}, // large
	}

	for _, tt := range tests {
		t.Run(fmt.Sprintf("size%d", tt.size), func(t *testing.T) {
			got := SizeClassForSize(tt.size)
			if got != tt.wantClass {
				t.Fatalf("SizeClassForSize(%d) = %d, want %d", tt.size, got, tt.wantClass)
			}
		})
	}
}

// Проверяем что объект ВСЕГДА помещается в свой size class
func TestSizeClass_FitsData(t *testing.T) {
	for size := 1; size <= 4096; size++ {
		sc := SizeClassForSize(size)
		if sc < 0 {
			t.Fatalf("size %d should have a class, got -1", size)
		}
		if sizeClasses[sc] < size {
			t.Fatalf("size %d → class %d (capacity %d): doesn't fit!",
				size, sc, sizeClasses[sc])
		}
	}
}

// ─── Span: Alloc/Free ──────────────────────────────────────
//
// Span — ящик с N слотами. Два пути аллокации:
//   1. Bump pointer: allocIndex++ (первичное выделение)
//   2. FreeStack pop: переиспользование освобождённого слота
//
// Тестируем оба пути + переполнение.

func TestSpan_AllocBumpPointer(t *testing.T) {
	// Создаём span: 4 объекта по 32 байта = 128 байт
	data := make([]byte, 128)
	s := NewSpan(data, 32, 0, nil)

	// Аллоцируем 4 объекта — bump pointer path
	for i := 0; i < 4; i++ {
		buf, idx := s.Alloc()
		if buf == nil {
			t.Fatalf("Alloc #%d returned nil", i)
		}
		if idx != i {
			t.Fatalf("Alloc #%d: idx=%d, want %d", i, idx, i)
		}
		if len(buf) != 32 {
			t.Fatalf("Alloc #%d: len=%d, want 32", i, len(buf))
		}
	}

	// Пятый — span полон
	buf, idx := s.Alloc()
	if buf != nil {
		t.Fatal("Alloc on full span should return nil")
	}
	if idx != -1 {
		t.Fatalf("full span idx = %d, want -1", idx)
	}
}

func TestSpan_FreeAndReuse(t *testing.T) {
	data := make([]byte, 128)
	s := NewSpan(data, 32, 0, nil)
	// Ставим state = spanInCache чтобы Free был lock-free
	s.state = spanInCache

	// Заполняем полностью
	for i := 0; i < 4; i++ {
		s.Alloc()
	}
	if !s.IsFull() {
		t.Fatal("span should be full after 4 allocs")
	}

	// Освобождаем объект #2
	s.Free(2)

	if s.IsFull() {
		t.Fatal("span should not be full after Free")
	}

	// Следующий Alloc должен вернуть объект #2 (из freeStack)
	buf, idx := s.Alloc()
	if buf == nil {
		t.Fatal("Alloc after Free returned nil")
	}
	if idx != 2 {
		t.Fatalf("expected reuse of index 2, got %d", idx)
	}
}

func TestSpan_FreeCount(t *testing.T) {
	data := make([]byte, 128)
	s := NewSpan(data, 32, 0, nil)
	s.state = spanInCache

	// Изначально: 4 свободных (все через bump pointer)
	if s.FreeCount() != 4 {
		t.Fatalf("initial FreeCount = %d, want 4", s.FreeCount())
	}

	s.Alloc() // осталось 3
	if s.FreeCount() != 3 {
		t.Fatalf("after 1 alloc: FreeCount = %d, want 3", s.FreeCount())
	}

	s.Alloc()
	s.Alloc()
	s.Alloc() // осталось 0
	if s.FreeCount() != 0 {
		t.Fatalf("after 4 allocs: FreeCount = %d, want 0", s.FreeCount())
	}

	s.Free(1) // вернули 1
	if s.FreeCount() != 1 {
		t.Fatalf("after Free: FreeCount = %d, want 1", s.FreeCount())
	}
}

// ─── Store: интеграционный тест ────────────────────────────
//
// Проверяем полный цикл: Set → Get → Del → Get.
// workerID=0 для всех операций (однопоточный тест).

func TestStore_SetGetDel(t *testing.T) {
	store := NewTCMallocStore(1) // 1 worker
	defer store.Close()

	store.Set(0, "hello", []byte("world"))

	val, ok := store.Get("hello")
	if !ok {
		t.Fatal("Get returned false for existing key")
	}
	if string(val) != "world" {
		t.Fatalf("Get = %q, want world", val)
	}

	// Delete
	deleted := store.Del(0, "hello")
	if !deleted {
		t.Fatal("Del returned false")
	}

	// After delete — should not exist
	_, ok = store.Get("hello")
	if ok {
		t.Fatal("Get returned true after Del")
	}
}

func TestStore_GetMissing(t *testing.T) {
	store := NewTCMallocStore(1)
	defer store.Close()

	_, ok := store.Get("nonexistent")
	if ok {
		t.Fatal("Get should return false for missing key")
	}
}

func TestStore_Overwrite(t *testing.T) {
	store := NewTCMallocStore(1)
	defer store.Close()

	store.Set(0, "key", []byte("first"))
	store.Set(0, "key", []byte("second"))

	val, ok := store.Get("key")
	if !ok {
		t.Fatal("Get returned false")
	}
	if string(val) != "second" {
		t.Fatalf("after overwrite: %q, want second", val)
	}
}

func TestStore_OOM(t *testing.T) {
	store := NewTCMallocStore(1)
	defer store.Close()
	store.SetMaxMemory(1024 * 1024) // 1MB лимит

	if store.IsOOM() {
		t.Fatal("fresh store should not be OOM")
	}

	// Первый SET должен работать
	store.Set(0, "k", []byte("v"))
	if store.UsedMemory() <= 0 {
		t.Fatal("UsedMemory should be > 0 after Set")
	}
}

func TestStore_Len(t *testing.T) {
	store := NewTCMallocStore(1)
	defer store.Close()

	if store.Len() != 0 {
		t.Fatalf("empty store Len = %d", store.Len())
	}

	store.Set(0, "a", []byte("1"))
	store.Set(0, "b", []byte("2"))
	store.Set(0, "c", []byte("3"))

	if store.Len() != 3 {
		t.Fatalf("Len = %d, want 3", store.Len())
	}

	store.Del(0, "b")
	if store.Len() != 2 {
		t.Fatalf("after Del: Len = %d, want 2", store.Len())
	}
}

func TestStore_MultiWorker(t *testing.T) {
	const numWorkers = 4
	const keysPerWorker = 500
	store := NewTCMallocStore(numWorkers)
	defer store.Close()
	// Каждый worker пишет свои ключи: "w0:0", "w0:1", ... "w3:499"
	var wg sync.WaitGroup
	wg.Add(numWorkers)
	for w := 0; w < numWorkers; w++ {
		go func(workerID int) {
			defer wg.Done()
			for i := 0; i < keysPerWorker; i++ {
				key := fmt.Sprintf("w%d:%d", workerID, i)
				val := fmt.Sprintf("val-%d-%d", workerID, i)
				store.Set(workerID, key, []byte(val))
			}
		}(w)
	}
	wg.Wait()
	// Проверяем: все ключи на месте
	expectedTotal := numWorkers * keysPerWorker
	if store.Len() != expectedTotal {
		t.Fatalf("Len = %d, want %d", store.Len(), expectedTotal)
	}
	// Проверяем: значения корректны
	for w := 0; w < numWorkers; w++ {
		for i := 0; i < keysPerWorker; i++ {
			key := fmt.Sprintf("w%d:%d", w, i)
			wantVal := fmt.Sprintf("val-%d-%d", w, i)
			got, ok := store.Get(key)
			if !ok {
				t.Fatalf("key %q not found", key)
			}
			if string(got) != wantVal {
				t.Fatalf("key %q: got %q, want %q", key, got, wantVal)
			}
		}
	}
}

func TestStore_ConcurrentReadWrite(t *testing.T) {
	const numWorkers = 4
	store := NewTCMallocStore(numWorkers)
	defer store.Close()

	for i := 0; i < 100; i++ {
		store.Set(0, fmt.Sprintf("key%d", i), []byte("initial"))
	}
	var wg sync.WaitGroup
	wg.Add(numWorkers)

	for w := 0; w < 2; w++ {
		go func(wid int) {
			defer wg.Done()
			for i := 0; i < 1000; i++ {
				key := fmt.Sprintf("key%d", i%100)
				store.Set(wid, key, []byte(fmt.Sprintf("v%d", i)))
			}
		}(w)
	}
	for w := 2; w < 4; w++ {
		go func(wid int) {
			defer wg.Done()
			for i := 0; i < 1000; i++ {
				key := fmt.Sprintf("key:%d", i%100)
				store.Get(key)
			}
		}(w)
	}
	wg.Wait()
}

func TestStore_LargeObject(t *testing.T) {
	store := NewTCMallocStore(1)
	defer store.Close()

	bigVal := make([]byte, 8192)
	for i := range bigVal {
		bigVal[i] = byte(i % 256)
	}
	store.Set(0, "bigkey", bigVal)
	got, ok := store.Get("bigkey")
	if !ok {
		t.Fatal("large key not found")
	}
	if len(got) != 8192 {
		t.Fatalf("large value len = %d, want 8192", len(got))
	}
	if got[0] != 0 || got[100] != 100 || got[8191] != byte(8191%256) {
		t.Fatal("large value data corrupted")
	}
	if !store.Del(0, "bigkey") {
		t.Fatal("Del large key failed")
	}

}

func BenchmarkStore_Set(b *testing.B) {
	store := NewTCMallocStore(1)
	defer store.Close()
	val := []byte("benchmark-value-data")
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		store.Set(0, fmt.Sprintf("k-%d", i%10000), val)
	}
}

func BenchmarkStore_Get(b *testing.B) {
	store := NewTCMallocStore(1)
	defer store.Close()
	// Подготовка: заполняем 10K ключей
	for i := 0; i < 10000; i++ {
		store.Set(0, fmt.Sprintf("k:%d", i), []byte("value"))
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		store.Get(fmt.Sprintf("k:%d", i%10000))
	}
}

func BenchmarkStore_Get_Parallel(b *testing.B) {
	store := NewTCMallocStore(12)
	defer store.Close()
	for i := 0; i < 10000; i++ {
		store.Set(0, fmt.Sprintf("k:%d", i), []byte("value"))
	}
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		i := 0
		for pb.Next() {
			store.Get(fmt.Sprintf("k%d", i%10000))
			i++
		}
	})
}

// ─── HashTable: Grow при высоком load factor ───────────────

// ─── HashTable: Grow при высоком load factor ───────────────

func TestHashTable_Grow(t *testing.T) {
	ht := NewHashTable(8) // Реально создаст на 1024 слота

	// Вставляем 800 записей (800 / 1024 = 78% > 70%)
	for i := uint64(100); i < 900; i++ {
		ht.Put(i, i*10)
	}

	if !ht.NeedsGrow() {
		t.Fatal("800/1024 = 78%, должен триггерить Grow")
	}

	grown := ht.Grow()

	if grown.Len() != 800 {
		t.Fatalf("after Grow: Len=%d, want 800", grown.Len())
	}
	for i := uint64(100); i < 900; i++ {
		val, ok := grown.Get(i)
		if !ok || val != i*10 {
			t.Fatalf("key %d: got (%d, %v), want (%d, true)", i, val, ok, i*10)
		}
	}

	if grown.size <= ht.size {
		t.Fatalf("grown.size=%d should be > old.size=%d", grown.size, ht.size)
	}
}

// ─── HashTable: Rebuild (tombstone cleanup) ────────────────

func TestHashTable_Rebuild(t *testing.T) {
	ht := NewHashTable(8) // Реально создаст на 1024 слота

	// Вставляем 400 записей
	for i := uint64(10); i < 410; i++ {
		ht.Put(i, i*10)
	}

	// Удаляем 300 из них → 300 tombstones из 1024 слотов = 29% > 25% порог
	for i := uint64(10); i < 310; i++ {
		ht.Delete(i)
	}

	if !ht.NeedsRebuild() {
		t.Fatalf("tombstones=%d, size=%d → should need rebuild", ht.tombstones, ht.size)
	}

	rebuilt := ht.Rebuild()

	if rebuilt.Len() != 100 {
		t.Fatalf("after Rebuild: Len=%d, want 100", rebuilt.Len())
	}
	if rebuilt.tombstones != 0 {
		t.Fatalf("rebuilt should have 0 tombstones, got %d", rebuilt.tombstones)
	}

	// Оставшиеся ключи на месте (310...409)
	for i := uint64(310); i < 410; i++ {
		val, ok := rebuilt.Get(i)
		if !ok || val != i*10 {
			t.Fatalf("key %d lost after Rebuild", i)
		}
	}

	// Удалённые ключи (10...309) НЕ должны найтись
	for i := uint64(10); i < 310; i++ {
		if _, ok := rebuilt.Get(i); ok {
			t.Fatalf("deleted key %d found after Rebuild", i)
		}
	}
}

// ─── HashTable: Linear Probing (коллизии) ──────────────────

func TestHashTable_LinearProbing(t *testing.T) {
	ht := NewHashTable(8) // size=1024 (минимум), но мы бьём в один слот

	// Два РАЗНЫХ хеша, которые попадают в один слот: hash & mask = одно число.
	// Для size=1024, mask=1023: hash1=1024 и hash2=2048 дадут idx=0,
	// но sanitizeHash сдвинет 0→1. Используем хеши, которые точно коллидируют.
	hash1 := uint64(5)
	hash2 := uint64(5 + ht.size) // оба дают idx = 5 & mask = 5

	ht.Put(hash1, 100)
	ht.Put(hash2, 200)

	// Оба должны найтись
	v1, ok1 := ht.Get(hash1)
	v2, ok2 := ht.Get(hash2)

	if !ok1 || v1 != 100 {
		t.Fatalf("hash1: got (%d, %v), want (100, true)", v1, ok1)
	}
	if !ok2 || v2 != 200 {
		t.Fatalf("hash2: got (%d, %v), want (200, true)", v2, ok2)
	}

	// Удаляем первый — второй должен остаться (tombstone не ломает chain)
	ht.Delete(hash1)

	if _, ok := ht.Get(hash1); ok {
		t.Fatal("deleted hash1 still found")
	}
	v2after, ok := ht.Get(hash2)
	if !ok || v2after != 200 {
		t.Fatal("hash2 lost after deleting hash1 (tombstone broke probing chain)")
	}
}

// ─── Store: Grow через один шард ───────────────────────────

func TestStore_ForcedGrow(t *testing.T) {
	store := NewTCMallocStore(1)
	defer store.Close()

	// Находим ключи, которые все попадают в шард 0
	// hash(key) % 256 == 0
	var targetKeys []string
	for i := 0; len(targetKeys) < 800; i++ {
		key := fmt.Sprintf("grow-test-%d", i)
		h := hashStoreKey(key)
		if h%numStoreShards == 0 {
			targetKeys = append(targetKeys, key)
		}
	}

	// Записываем 800 ключей в один шард
	// defaultInitialCap=1024, loadFactor=70% → Grow при ~717 записях
	for _, key := range targetKeys {
		store.Set(0, key, []byte("v"))
	}

	// Все ключи на месте после Grow
	for _, key := range targetKeys {
		if _, ok := store.Get(key); !ok {
			t.Fatalf("key %q lost after forced Grow", key)
		}
	}

	t.Logf("wrote %d keys to shard 0 (forced Grow)", len(targetKeys))
}

// ─── Store: Forced Rebuild (tombstones) ────────────────────

func TestStore_ForcedRebuild(t *testing.T) {
	store := NewTCMallocStore(1)
	defer store.Close()

	// Находим 500 ключей в одном шарде
	var keys []string
	for i := 0; len(keys) < 500; i++ {
		key := fmt.Sprintf("rebuild-%d", i)
		if hashStoreKey(key)%numStoreShards == 1 {
			keys = append(keys, key)
		}
	}

	// Пишем 500, удаляем 400 → tombstones
	for _, key := range keys {
		store.Set(0, key, []byte("x"))
	}
	for _, key := range keys[:400] {
		store.Del(0, key)
	}

	// Пишем ещё 200 новых → Rebuild должен сработать
	var newKeys []string
	for i := 0; len(newKeys) < 200; i++ {
		key := fmt.Sprintf("rebuild-new-%d", i)
		if hashStoreKey(key)%numStoreShards == 1 {
			newKeys = append(newKeys, key)
			store.Set(0, key, []byte("y"))
		}
	}

	// Оставшиеся 100 старых + 200 новых = 300
	for _, key := range keys[400:] {
		if _, ok := store.Get(key); !ok {
			t.Fatalf("surviving key %q lost after Rebuild", key)
		}
	}
	for _, key := range newKeys {
		if _, ok := store.Get(key); !ok {
			t.Fatalf("new key %q lost after Rebuild", key)
		}
	}
}

// ─── MCentral: Full → Partial (ReturnToPartial) ────────────

func TestMCentral_ReturnToPartial(t *testing.T) {
	heap := NewMHeap()
	central := NewMCentral(0, heap) // class 0: 32B, 1024 obj/span

	// Берём span, заполняем полностью, возвращаем как full
	span := central.GetSpan()
	for i := 0; i < span.capacity; i++ {
		buf, _ := span.Alloc()
		if buf == nil {
			t.Fatalf("Alloc failed at %d/%d", i, span.capacity)
		}
	}
	if !span.IsFull() {
		t.Fatal("span should be full")
	}

	// Возвращаем в central как full
	central.ReturnSpan(span)

	_, fullCount, _ := central.Stats()
	if fullCount != 1 {
		t.Fatalf("full count = %d, want 1", fullCount)
	}

	// Освобождаем объект #0 → триггерит ReturnToPartial
	span.Free(0)

	// Проверяем: span переехал из full → partial
	partialCount, fullCount, _ := central.Stats()
	if fullCount != 0 {
		t.Fatalf("after Free: full=%d, want 0", fullCount)
	}
	if partialCount != 1 {
		t.Fatalf("after Free: partial=%d, want 1", partialCount)
	}

	// Берём span снова — должен вернуться тот же (с дыркой)
	reused := central.GetSpan()
	buf, idx := reused.Alloc()
	if buf == nil {
		t.Fatal("reused span Alloc failed")
	}
	if idx != 0 {
		t.Fatalf("expected reuse of slot 0, got %d", idx)
	}
}

// ─── Large Object: memory accounting ───────────────────────

func TestStore_LargeObject_MemoryAccounting(t *testing.T) {
	store := NewTCMallocStore(1)
	defer store.Close()

	memBefore := store.UsedMemory()

	bigVal := make([]byte, 8192)
	store.Set(0, "big", bigVal)

	memAfterSet := store.UsedMemory()
	if memAfterSet <= memBefore {
		t.Fatalf("memory didn't increase: before=%d, after=%d", memBefore, memAfterSet)
	}

	store.Del(0, "big")

	// С deferred free память НЕ освобождается сразу — handle в очереди.
	memAfterDel := store.UsedMemory()
	if memAfterDel < memAfterSet {
		t.Fatal("memory decreased immediately after Del (should be deferred)")
	}

	// FlushDeferred безусловно освобождает всё отложенное за один вызов
	// (force-drain, используется на shutdown и в тестах; штатный путь — QSBR).
	freed := store.FlushDeferred()

	if freed == 0 {
		t.Fatal("FlushDeferred should have freed at least 1 handle")
	}

	memAfterFlush := store.UsedMemory()
	if memAfterFlush >= memAfterSet {
		t.Fatalf("memory didn't decrease after FlushDeferred: set=%d, flush=%d (leak!)",
			memAfterSet, memAfterFlush)
	}

	t.Logf("memory: before=%d, afterSet=%d, afterFlush=%d (freed %d bytes, %d handles)",
		memBefore, memAfterSet, memAfterFlush, memAfterSet-memAfterFlush, freed)
}
