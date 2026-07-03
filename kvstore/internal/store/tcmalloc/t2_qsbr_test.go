package tcmalloc

import (
	"fmt"
	"sync"
	"testing"
)

// TestT2_QSBRHoldsHandleUntilQuiescent — детерминированный страж фикса T2.
//
// Баг T2: старый deferred-free освобождал слот по таймеру (100ms). Если
// lock-free Get вытеснит планировщик/STW-GC между загрузкой хендла и
// Resolve+decode на время дольше grace-периода, а параллельный Del/overwrite
// тем временем освободит слот и Alloc переиспользует его — Get прочитает
// чужую/недописанную память (UAF/torn-read/ложный not-found).
//
// QSBR-фикс: слот освобождается не по времени, а по кворуму quiescence.
// Здесь моделируется читатель, вытесненный ПОСРЕДИ Get: воркер прошёл границу
// команды (online на поколении e), но НЕ дошёл до следующего тихого состояния.
// Пока это так, ретайрнутый хендл освобождать нельзя, СКОЛЬКО БЫ циклов
// reclaim ни прошло. Тест падает на старом (time-based/безусловном) поведении.
func TestT2_QSBRHoldsHandleUntilQuiescent(t *testing.T) {
	s := NewTCMallocStore(1)
	defer s.Close()

	// Читатель (воркер 0) прошёл границу команды и вот-вот/уже загрузил хендл.
	s.ReportQuiescent(0)

	_, h := s.Alloc(0, 40)
	s.DeferFree(h) // параллельный Del/overwrite ретайрит хендл

	// Reclaimer крутится много циклов. Воркер «застрял» (не рапортует новое
	// тихое состояние) — как вытесненный посреди Get. Освобождать НЕЛЬЗЯ.
	for i := 0; i < 100; i++ {
		s.reclaim()
	}
	if q := s.DeferredQueueLen(); q == 0 {
		t.Fatal("T2 regression: хендл освобождён, пока читатель ещё активен " +
			"(не прошёл quiescent-состояние) — окно use-after-free открыто")
	}

	// Читатель дошёл до границы команды (значение скопировано, хендла не держит).
	s.ReportQuiescent(0)
	s.reclaim()
	if q := s.DeferredQueueLen(); q != 0 {
		t.Fatalf("хендл не освобождён после quiescent-состояния: queue=%d", q)
	}
}

// TestT2_ReclaimProgressesWhenWorkerOffline — liveness-страж QSBR.
//
// Спящий в epoll_wait воркер (offline) не держит хендлов и обязан быть исключён
// из кворума — иначе idle-воркер заморозил бы своё поколение навсегда и reclaim
// встал бы (утечка pending). Проверяем, что при всех offline-воркерах слот
// освобождается без единого QS-репорта.
func TestT2_ReclaimProgressesWhenWorkerOffline(t *testing.T) {
	s := NewTCMallocStore(2)
	defer s.Close()

	s.GoOffline(0)
	s.GoOffline(1)

	_, h := s.Alloc(0, 40)
	s.DeferFree(h)

	s.reclaim()
	if q := s.DeferredQueueLen(); q != 0 {
		t.Fatalf("reclaim встал при всех offline-воркерах (утечка pending): queue=%d", q)
	}
}

// TestT2_OnlineWorkerBlocksThenOfflineReleases — граница online→offline.
//
// Онлайн-воркер на поколении ретайра держит хендл (min ≤ tag → не освобождаем).
// Как только он уходит offline (прошёл тихое состояние перед сном) — хендл
// становится освобождаемым.
func TestT2_OnlineWorkerBlocksThenOfflineReleases(t *testing.T) {
	s := NewTCMallocStore(1)
	defer s.Close()

	s.ReportQuiescent(0) // online на поколении 1
	_, h := s.Alloc(0, 40)
	s.DeferFree(h) // тег = 1

	s.reclaim()
	if q := s.DeferredQueueLen(); q == 0 {
		t.Fatal("хендл освобождён, пока воркер online на поколении ретайра")
	}

	s.GoOffline(0) // воркер уснул → вне кворума
	s.reclaim()
	if q := s.DeferredQueueLen(); q != 0 {
		t.Fatalf("хендл не освобождён после ухода воркера offline: queue=%d", q)
	}
}

// TestT2_CloseIdempotent — фикс T4: повторный Close не паникует.
//
// Старый Close делал голый close(s.stopCh); двойной вызов (или двойной defer)
// = паника «close of closed channel». sync.Once делает Close идемпотентным.
func TestT2_CloseIdempotent(t *testing.T) {
	s := NewTCMallocStore(1)
	s.Close()
	s.Close() // не должно паниковать
}

// TestT2_ConcurrentQSBR_NoRace — интеграционный страж под -race.
//
// Много «воркеров» рапортуют quiescence на границах команд, иногда «засыпают»
// (offline), а отдельная горутина агрессивно гоняет reclaim, освобождая слоты
// через caches[0].Free (тот же cross-worker путь, что защищён remote-free от
// T1). Проверяет, что новая QSBR-бухгалтерия (epoch/workerEpoch/pending) и
// освобождение не гоняются друг с другом. Запускать под `go test -race`.
func TestT2_ConcurrentQSBR_NoRace(t *testing.T) {
	const (
		numWorkers = 8
		numKeys    = 128
		iters      = 5000
	)
	s := NewTCMallocStore(numWorkers)
	defer s.Close()

	val := make([]byte, 100)

	var wg sync.WaitGroup
	for w := 0; w < numWorkers; w++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			for i := 0; i < iters; i++ {
				s.ReportQuiescent(id) // граница команды (как воркер после Wait)
				key := fmt.Sprintf("k%d", i%numKeys)
				s.Set(id, key, val)
				s.Get(key)
				if i%3 == 0 {
					s.Del(id, key)
				}
				if i%64 == 0 {
					s.GoOffline(id) // иногда «засыпаем» между командами
				}
			}
			s.GoOffline(id)
		}(w)
	}

	stop := make(chan struct{})
	var rwg sync.WaitGroup
	rwg.Add(1)
	go func() {
		defer rwg.Done()
		for {
			select {
			case <-stop:
				return
			default:
				s.reclaim()
			}
		}
	}()

	wg.Wait()
	close(stop)
	rwg.Wait()
}
