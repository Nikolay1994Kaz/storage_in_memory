package tcmalloc

import (
	"fmt"
	"sync"
	"testing"
)

// TestT1_DeferredFreeRace — репро гонки T1 (порча данных под всем KV).
//
// FlushDeferred (фоновая горутина) вызывает caches[0].Free(h) → span.Free(idx).
// Если span сейчас в состоянии spanInCache и его АКТИВНО аллоцирует ДРУГОЙ
// воркер, то span.Free берёт lock-free путь `s.freeStack = append(...)`
// конкурентно с s.Alloc() владельца (pop/bump того же freeStack) →
//   - data race на freeStack (ловится -race),
//   - потеря обновлений: один objIndex может уйти двум ключам → тихая порча KV.
//
// Тест воспроизводит именно этот сценарий: overwrite/DEL одних и тех же ключей
// с РАЗНЫХ воркеров (каждый overwrite кладёт в deferred хэндл, аллоцированный
// другим воркером) + агрессивный FlushDeferred, который освобождает эти хэндлы,
// пока их спаны ещё spanInCache и аллоцируются владельцами.
//
// Запускать под `go test -race -run TestT1_DeferredFreeRace`.
func TestT1_DeferredFreeRace(t *testing.T) {
	const (
		numWorkers = 8
		numKeys    = 64
		iters      = 20000
	)
	store := NewTCMallocStore(numWorkers)
	defer store.Close()

	// 100 байт → size class 2 (128B, 256 obj/span): спаны живут долго,
	// оставаясь spanInCache во время множества аллокаций.
	val := make([]byte, 100)

	var wg sync.WaitGroup
	for w := 0; w < numWorkers; w++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			for i := 0; i < iters; i++ {
				key := fmt.Sprintf("k%d", i%numKeys)
				store.Set(id, key, val) // overwrite → DeferFree чужого хэндла
				if i%3 == 0 {
					store.Del(id, key) // DEL → DeferFree
				}
			}
		}(w)
	}

	// Фоновый агрессивный flush — освобождает deferred-хэндлы немедленно,
	// пока их спаны ещё активны у владельцев (максимизирует окно гонки).
	stop := make(chan struct{})
	var fwg sync.WaitGroup
	fwg.Add(1)
	go func() {
		defer fwg.Done()
		for {
			select {
			case <-stop:
				return
			default:
				store.FlushDeferred()
			}
		}
	}()

	wg.Wait()
	close(stop)
	fwg.Wait()
}

// TestT1_NoCrossKeyCorruption — прямой страж потери обновлений из T1.
//
// Если remote-free (FlushDeferred) и Alloc владельца гоняют freeStack, один
// objIndex может уйти двум ключам → блок одного ключа затирается значением
// другого. Здесь каждый воркер владеет НЕПЕРЕСЕКАЮЩИМСЯ набором ключей, а
// значение детерминировано выводится из ключа. После конкурентной фазы (с
// агрессивным flush) КАЖДЫЙ ключ обязан вернуть ровно своё значение.
func TestT1_NoCrossKeyCorruption(t *testing.T) {
	const (
		numWorkers  = 8
		keysPerWork = 128
		iters       = 4000
	)
	store := NewTCMallocStore(numWorkers)
	defer store.Close()

	valFor := func(w, k int) []byte {
		return []byte(fmt.Sprintf("w%d-k%d-payload-padding-to-classtwo", w, k))
	}

	var wg sync.WaitGroup
	for w := 0; w < numWorkers; w++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			for i := 0; i < iters; i++ {
				k := i % keysPerWork
				key := fmt.Sprintf("w%d/k%d", id, k)
				if i%4 == 0 {
					store.Del(id, key)
				}
				store.Set(id, key, valFor(id, k)) // финальное состояние: ключ есть
			}
		}(w)
	}

	stop := make(chan struct{})
	var fwg sync.WaitGroup
	fwg.Add(1)
	go func() {
		defer fwg.Done()
		for {
			select {
			case <-stop:
				return
			default:
				store.FlushDeferred()
			}
		}
	}()

	wg.Wait()
	close(stop)
	fwg.Wait()

	// Дренируем оставшиеся отложенные освобождения.
	store.FlushDeferred()
	store.FlushDeferred()

	// Каждый ключ обязан вернуть ровно своё значение — иначе один objIndex
	// был выдан двум ключам (порча) или блок затёрт.
	for w := 0; w < numWorkers; w++ {
		for k := 0; k < keysPerWork; k++ {
			key := fmt.Sprintf("w%d/k%d", w, k)
			got, ok := store.Get(key)
			if !ok {
				t.Fatalf("ключ %s пропал (потеря обновления/порча индекса)", key)
			}
			if want := valFor(w, k); string(got) != string(want) {
				t.Fatalf("ключ %s: got %q, want %q (cross-key порча)", key, got, want)
			}
		}
	}
}
