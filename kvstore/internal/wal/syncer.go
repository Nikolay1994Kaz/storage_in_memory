package wal

import (
	"log"
	"os"
	"sync"
	"sync/atomic"
	"time"
)

const (
	MaxWALSize = 64 * 1024 * 1024 // 64MB

	// sizeCheckEvery — проверять размер WAL каждые N тиков Syncer'а.
	// При syncInterval=100ms → проверка раз в 5 секунд.
	// os.Stat — syscall, нет смысла дёргать его каждые 100ms.
	sizeCheckEvery = 50
)

type Syncer struct {
	wal        *WAL
	interval   time.Duration
	stop       chan struct{}
	done       chan struct{} // закрывается когда run() завершается
	dir        string
	iterate    func(fn func(key string, value []byte))
	compacting atomic.Bool
	wg         sync.WaitGroup
}

func NewSyncer(w *WAL, interval time.Duration, dir string, iterate func(fn func(key string, value []byte))) *Syncer {
	s := &Syncer{
		wal:      w,
		interval: interval,
		stop:     make(chan struct{}),
		done:     make(chan struct{}),
		dir:      dir,
		iterate:  iterate,
	}
	s.wg.Add(1)
	go s.run()
	return s
}

func (s *Syncer) run() {
	defer s.wg.Done()
	defer close(s.done)

	ticker := time.NewTicker(s.interval)
	defer ticker.Stop()

	sizeCheckCounter := 0

	for {
		select {
		case <-ticker.C:
			if err := s.wal.Sync(); err != nil {
				log.Printf("WAL sync error: %v", err)
			}

			// Проверяем размер WAL реже — os.Stat каждые 100ms избыточен.
			sizeCheckCounter++
			if sizeCheckCounter >= sizeCheckEvery {
				sizeCheckCounter = 0
				if !s.compacting.Load() {
					s.checkWALSize()
				}
			}

		case <-s.stop:
			s.wal.Sync()
			return
		}
	}
}

func (s *Syncer) checkWALSize() {
	path := s.wal.Path()
	info, err := os.Stat(path)
	if err != nil {
		return
	}

	if info.Size() > MaxWALSize {
		log.Printf("WAL size %.1f MB > %.1f MB limit — starting auto-compact",
			float64(info.Size())/(1024*1024),
			float64(MaxWALSize)/(1024*1024))

		s.compacting.Store(true)
		go func() {
			// defer гарантирует сброс флага даже при panic —
			// без этого auto-compact навсегда отключится.
			defer s.compacting.Store(false)
			defer func() {
				if r := recover(); r != nil {
					log.Printf("WAL compact panic: %v", r)
				}
			}()
			BackgroundCompact(s.wal, s.dir, s.iterate)
		}()
	}
}

// Stop останавливает Syncer и ждёт завершения run-горутины.
// Гарантирует, что последний Sync() выполнен перед возвратом.
func (s *Syncer) Stop() {
	close(s.stop)
	s.wg.Wait()
}
