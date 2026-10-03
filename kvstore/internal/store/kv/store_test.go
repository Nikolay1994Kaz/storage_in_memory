package kv

import (
	"bytes"
	"strconv"
	"sync"
	"testing"
	"time"
)

func TestSetGetDel(t *testing.T) {
	s := New()
	if _, ok := s.Get("a"); ok {
		t.Fatal("пустой стор вернул ключ")
	}
	s.Set("a", []byte("1"))
	s.Set("b", []byte("22"))
	if v, ok := s.Get("a"); !ok || string(v) != "1" {
		t.Fatalf("Get(a) = %q, %v", v, ok)
	}
	s.Set("a", []byte("333"))
	if v, _ := s.Get("a"); string(v) != "333" {
		t.Fatalf("перезапись: Get(a) = %q", v)
	}
	if s.Len() != 2 {
		t.Fatalf("Len = %d, ожидали 2", s.Len())
	}
	if !s.Del("a") || s.Del("a") {
		t.Fatal("Del: первый вызов должен вернуть true, второй false")
	}
	if _, ok := s.Get("a"); ok {
		t.Fatal("ключ жив после Del")
	}
	if s.Len() != 1 {
		t.Fatalf("Len = %d после Del, ожидали 1", s.Len())
	}
}

// Читатель, взявший значение до перезаписи, обязан видеть его целиком: стор
// кладёт НОВЫЙ срез, а не пишет поверх старого (замена QSBR из прежней арены).
func TestOverwriteDoesNotTouchReaderSlice(t *testing.T) {
	s := New()
	s.Set("k", []byte("old-value"))
	held, _ := s.Get("k")
	s.Set("k", []byte("NEW"))
	s.Del("k")
	if string(held) != "old-value" {
		t.Fatalf("срез читателя изменился: %q", held)
	}
}

// Учёт памяти возвращается в ноль, когда всё удалено. Регресс: учёт прежней
// арены не уменьшался для мелких объектов — после одного касания maxmemory
// сервер отказывал в записи до рестарта, даже если удалить все ключи.
func TestMemoryAccountingReturnsToZero(t *testing.T) {
	s := New()
	s.SetMaxMemory(4 << 20)
	val := make([]byte, 100)
	n := 0
	for !s.IsOOM() {
		s.Set("k"+strconv.Itoa(n), val)
		n++
	}
	if s.UsedMemory() < 4<<20 {
		t.Fatalf("OOM при used=%d < лимита", s.UsedMemory())
	}
	for i := 0; i < n; i++ {
		s.Del("k" + strconv.Itoa(i))
	}
	if s.Len() != 0 || s.UsedMemory() != 0 {
		t.Fatalf("после удаления всех: Len=%d used=%d, ожидали 0 и 0", s.Len(), s.UsedMemory())
	}
	if s.IsOOM() {
		t.Fatal("OOM залип после удаления всех ключей")
	}
}

func TestMemoryAccountingOverwrite(t *testing.T) {
	s := New()
	s.Set("k", make([]byte, 1000))
	big := s.UsedMemory()
	s.Set("k", make([]byte, 10))
	if got, want := big-s.UsedMemory(), int64(990); got != want {
		t.Fatalf("перезапись на меньшее значение освободила %d байт, ожидали %d", got, want)
	}
	s.Clear()
	if s.UsedMemory() != 0 || s.Len() != 0 {
		t.Fatalf("после Clear: used=%d Len=%d", s.UsedMemory(), s.Len())
	}
}

// Внешний учёт (sorted sets) идёт в тот же счётчик и тот же гейт.
func TestMemoryCounterSharedWithGate(t *testing.T) {
	s := New()
	s.SetMaxMemory(1000)
	s.MemoryCounter().Add(1000)
	if !s.IsOOM() {
		t.Fatal("внешний учёт не попал в гейт")
	}
	s.MemoryCounter().Add(-1000)
	if s.IsOOM() {
		t.Fatal("гейт не снялся после возврата внешнего учёта")
	}
}

// fn в ForEach вызывается без лока шарда: колбэк, который пишет в тот же стор,
// не должен самоблокироваться.
func TestForEachCallbackMayWrite(t *testing.T) {
	s := New()
	for i := 0; i < 1000; i++ {
		s.Set("k"+strconv.Itoa(i), []byte("v"))
	}
	done := make(chan int)
	go func() {
		seen := 0
		s.ForEach(func(key string, value []byte) {
			seen++
			s.Set(key, []byte("w")) // тот же шард
			s.Del("other-" + key)
		})
		done <- seen
	}()
	select {
	case seen := <-done:
		if seen != 1000 {
			t.Fatalf("ForEach увидел %d ключей, ожидали 1000", seen)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("ForEach завис: колбэк вызывается под локом шарда")
	}
}

func TestConcurrentMixed(t *testing.T) {
	s := New()
	const workers, ops = 8, 5000
	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			own := "own-" + strconv.Itoa(w)
			for i := 0; i < ops; i++ {
				k := "k" + strconv.Itoa(i%97)
				switch i % 3 {
				case 0:
					s.Set(k, []byte(strconv.Itoa(i)))
				case 1:
					if v, ok := s.Get(k); ok && len(v) == 0 {
						t.Errorf("пустое значение у %s", k)
					}
				case 2:
					s.Del(k)
				}
				s.Set(own, []byte(strconv.Itoa(i)))
			}
		}(w)
	}
	wg.Wait()
	for w := 0; w < workers; w++ {
		v, ok := s.Get("own-" + strconv.Itoa(w))
		if !ok || !bytes.Equal(v, []byte(strconv.Itoa(ops-1))) {
			t.Fatalf("own-%d = %q, %v", w, v, ok)
		}
	}
	// Учёт сходится с фактическим содержимым после гонок.
	var want int64
	s.ForEach(func(k string, v []byte) { want += entrySize(k, v) })
	if got := s.UsedMemory(); got != want {
		t.Fatalf("учёт разошёлся: used=%d, по содержимому %d", got, want)
	}
}

// GET на горячем пути не аллоцирует. Регресс-страж к замеру 22.09: прежний
// стор делал 2 аллокации на каждый GET (копия ключа и копия значения).
func TestGetDoesNotAllocate(t *testing.T) {
	s := New()
	s.Set("key:42", []byte("value-of-twenty-byte"))
	allocs := testing.AllocsPerRun(1000, func() {
		if _, ok := s.Get("key:42"); !ok {
			t.Fatal("ключ пропал")
		}
	})
	if allocs != 0 {
		t.Fatalf("Get аллоцирует: %.1f allocs/op", allocs)
	}
}
