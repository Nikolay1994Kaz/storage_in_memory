package wal

import (
	"log"
	"os"
	"sync/atomic"
	"time"
)

const (
	MaxWALSize = 64 * 1024 * 1024 // 64MB
)

type Syncer struct {
	wal        *WAL
	interval   time.Duration
	stop       chan struct{}
	dir        string
	iterate    func(fn func(key string, value []byte))
	compacting atomic.Bool // ← atomic вместо обычного bool
}

func NewSyncer(w *WAL, interval time.Duration, dir string, iterate func(fn func(key string, value []byte))) *Syncer {
	s := &Syncer{
		wal:      w,
		interval: interval,
		stop:     make(chan struct{}),
		dir:      dir,
		iterate:  iterate,
	}
	go s.run()
	return s
}

func (s *Syncer) run() {
	ticker := time.NewTicker(s.interval)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			if err := s.wal.Sync(); err != nil {
				log.Printf("WAL sync error: %v", err)
			}

			if !s.compacting.Load() { // ← atomic чтение
				s.checkWALSize()
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

		s.compacting.Store(true) // ← atomic запись
		go func() {
			BackgroundCompact(s.wal, s.dir, s.iterate)
			s.compacting.Store(false) // ← atomic запись из другой горутины — БЕЗОПАСНО
		}()
	}
}

func (s *Syncer) Stop() {
	close(s.stop)
}
