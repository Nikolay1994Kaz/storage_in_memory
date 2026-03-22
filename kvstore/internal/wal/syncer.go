package wal

import (
	"log"
	"time"
)

type Syncer struct {
	wal      *WAL
	interval time.Duration
	stop     chan struct{}
}

func NewSyncer(w *WAL, internal time.Duration) *Syncer {
	s := &Syncer{
		wal:      w,
		interval: internal,
		stop:     make(chan struct{}),
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
		case <-s.stop:
			s.wal.Sync()
			return
		}
	}
}

func (s *Syncer) Stop() {
	close(s.stop)
}
