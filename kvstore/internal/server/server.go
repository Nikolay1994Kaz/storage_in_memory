package server

import (
	"crypto/tls"
	"fmt"
	"log"
	"net"
	"runtime"
	"sync/atomic"
	"time"
)

const (
	// ReadTimeout — максимальное время ожидания данных от клиента.
	// Защита от Slowloris-атаки: если клиент прислал полкоманды
	// и замолчал — через 5 секунд соединение закроется.
	ReadTimeout = 5 * time.Second
)

// ConnState хранит состояние соединения.
//
// Buf — per-connection ring buffer (заменяет bufio.Reader + bufio.Writer).
// WorkerID — идентификатор epoll-воркера для TCMallocStore MCache.
type ConnState struct {
	Conn          net.Conn
	Buf           *ConnBuf // ← заменяет Reader + Writer
	WorkerID      int
	InTx          bool
	TxQueue       [][][]byte // ← was [][]protocol.Value
	Authenticated bool       // true после успешной AUTH команды
}

// Handler — функция обработки RESP-команды.
//
// Было:  func(cs, args []Value) Value   — создаёт Value, возвращает Value
// Стало: func(cs, args [][]byte)        — читает из ring buffer, пишет в ring buffer
//
// Handler НЕ возвращает значение. Вместо этого пишет ответ прямо в cs.Buf:
//
//	cs.Buf.WriteSimpleString("OK")
//	cs.Buf.WriteBulk(value)
//	cs.Buf.WriteError("ERR ...")
type Handler func(cs *ConnState, args [][]byte)

// worker — один воркер со своим epoll instance.
type worker struct {
	id    int
	epoll *Epoll
}

// Server — TCP-сервер на базе per-worker epoll.
type Server struct {
	addr      string
	handler   Handler
	listener  net.Listener
	workers   []*worker
	next      atomic.Uint64
	TLSConfig *tls.Config // nil = plain TCP, not-nil = TLS
}

func NewServer(addr string, handler Handler) *Server {
	return &Server{
		addr:    addr,
		handler: handler,
	}
}

// Start запускает сервер с per-worker epoll архитектурой.
func (s *Server) Start() error {
	var err error

	s.listener, err = net.Listen("tcp", s.addr)
	if err != nil {
		return fmt.Errorf("failed to listen on %s: %w", s.addr, err)
	}

	// TLS: оборачиваем listener, если TLSConfig задан.
	// Весь остальной код (epoll, ConnBuf) работает без изменений —
	// TLS прозрачно шифрует на уровне listener, conn.Read/Write те же.
	if s.TLSConfig != nil {
		s.listener = tls.NewListener(s.listener, s.TLSConfig)
		log.Println("TLS enabled")
	}

	numWorkers := runtime.NumCPU()
	s.workers = make([]*worker, numWorkers)

	for i := 0; i < numWorkers; i++ {
		ep, err := NewEpoll()
		if err != nil {
			return fmt.Errorf("failed to create epoll for worker %d: %w", i, err)
		}
		s.workers[i] = &worker{id: i, epoll: ep}

		go s.eventLoop(s.workers[i])
	}

	go s.acceptLoop()

	log.Printf("Server listening on %s (epoll mode, %d workers)", s.addr, numWorkers)
	return nil
}

// nextWorker выбирает следующего воркера по Round Robin.
func (s *Server) nextWorker() *worker {
	idx := s.next.Add(1)
	return s.workers[idx%uint64(len(s.workers))]
}

// acceptLoop принимает соединения и распределяет по воркерам.
func (s *Server) acceptLoop() {
	for {
		conn, err := s.listener.Accept()
		if err != nil {
			log.Printf("Accept error: %v", err)
			return
		}

		w := s.nextWorker()

		cs := &ConnState{
			Conn:     conn,
			Buf:      NewConnBuf(conn), // ← ConnBuf вместо Reader+Writer
			WorkerID: w.id,
		}

		if err := w.epoll.Add(cs); err != nil {
			log.Printf("Epoll Add error (worker %d): %v", w.id, err)
			conn.Close()
			continue
		}
	}
}

// eventLoop — главный цикл воркера.
func (s *Server) eventLoop(w *worker) {
	for {
		states, err := w.epoll.Wait()
		if err != nil {
			log.Printf("Worker %d: epoll wait error: %v", w.id, err)
			continue
		}

		for _, cs := range states {
			s.handleConn(w, cs)
		}
	}
}

// handleConn обрабатывает команды от клиента.
//
// ★ HYBRID: RING BUFFER + GREEDY DRAIN ★
//
// Цикл:
//  1. ReadFromConn() — основной read (epoll гарантирует данные)
//  2. ParseCommand() loop — разбираем ВСЕ команды из буфера
//  3. TryRead() — non-blocking raw syscall.Read:
//     пока мы обрабатывали команды (~100μs), клиент уже мог
//     отправить следующую. Ловим её БЕЗ нового epoll_wait.
//  4. Если TryRead вернул данные — обрабатываем и повторяем
//  5. Flush() — ONE write() для всех ответов
//
// Это то, что делает bufio.Reader "бесплатно" — жадно читает
// больше данных чем нужно. Мы делаем то же, но осознанно и
// без overhead bufio (без двойного буфера, без лишних memcpy).
func (s *Server) handleConn(w *worker, cs *ConnState) {
	n, err := cs.Buf.ReadFromConn()
	if n == 0 || err != nil {
		w.epoll.Remove(cs)
		return
	}

	for {
		// Разбираем и обрабатываем ВСЕ команды из буфера
		for {
			args := cs.Buf.ParseCommand()
			if args == nil {
				break
			}
			s.handler(cs, args)
		}

		// Greedy drain: пробуем забрать ещё данные (non-blocking)
		if cs.Buf.TryRead() == 0 {
			break // нет данных — выходим
		}
		// Есть данные — продолжаем обработку
	}

	// Один write() для ВСЕХ ответов (включая greedy drain)
	cs.Buf.Flush()
}

// Stop останавливает сервер.
func (s *Server) Stop() error {
	s.listener.Close()
	for _, w := range s.workers {
		w.epoll.Close()
	}
	return nil
}

// Stats — мониторинг распределения нагрузки по воркерам.
func (s *Server) Stats() map[int]int {
	stats := make(map[int]int, len(s.workers))
	for _, w := range s.workers {
		stats[w.id] = w.epoll.Count()
	}
	return stats
}
