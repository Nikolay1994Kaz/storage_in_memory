package server

import (
	"fmt"
	"log"
	"net"
	"runtime"
	"sync/atomic"
	"time"

	"kvstore/kvstore/internal/protocol"
)

const (
	// ReadTimeout — максимальное время ожидания данных от клиента.
	// Защита от Slowloris-атаки: если клиент прислал полкоманды
	// и замолчал — через 5 секунд соединение закроется.
	ReadTimeout = 5 * time.Second
)

// ConnState хранит состояние соединения: Reader и Writer переиспользуются
// между командами, чтобы избежать аллокации bufio.Reader (4KB) на каждый запрос.
type ConnState struct {
	Conn   net.Conn
	Reader *protocol.Reader
	Writer *protocol.Writer
}

// Handler — функция обработки RESP-команды.
type Handler func(conn net.Conn, args []protocol.Value) protocol.Value

// worker — один воркер со своим epoll instance.
// Каждый воркер независим: свой epoll, своя очередь событий.
type worker struct {
	id    int
	epoll *Epoll
}

// Server — TCP-сервер на базе per-worker epoll.
type Server struct {
	addr     string
	handler  Handler
	listener net.Listener
	workers  []*worker
	next     atomic.Uint64 // счётчик для Round Robin
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

	// Создаём по одному воркеру на каждый CPU
	numWorkers := runtime.NumCPU()
	s.workers = make([]*worker, numWorkers)

	for i := 0; i < numWorkers; i++ {
		ep, err := NewEpoll()
		if err != nil {
			return fmt.Errorf("failed to create epoll for worker %d: %w", i, err)
		}
		s.workers[i] = &worker{id: i, epoll: ep}

		// Каждый воркер крутит свой event loop на своём epoll
		go s.eventLoop(s.workers[i])
	}

	go s.acceptLoop()

	log.Printf("Server listening on %s (epoll mode, %d workers)", s.addr, numWorkers)
	return nil
}

// nextWorker выбирает следующего воркера по Round Robin.
// atomic.Uint64 — lock-free, zero contention.
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

		// Создаём ConnState с Reader/Writer один раз при подключении
		cs := &ConnState{
			Conn:   conn,
			Reader: protocol.NewReader(conn),
			Writer: protocol.NewWriter(conn),
		}

		// Round Robin: каждое новое соединение → следующий воркер
		w := s.nextWorker()
		if err := w.epoll.Add(cs); err != nil {
			log.Printf("Epoll Add error (worker %d): %v", w.id, err)
			conn.Close()
			continue
		}
	}
}

// eventLoop — главный цикл воркера.
// Каждый воркер ждёт событий ТОЛЬКО на своих соединениях.
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

// handleConn обрабатывает одну команду от клиента.
func (s *Server) handleConn(w *worker, cs *ConnState) {
	// Защита от Slowloris / partial read
	cs.Conn.SetReadDeadline(time.Now().Add(ReadTimeout))

	value, err := cs.Reader.Read()
	if err != nil {
		w.epoll.Remove(cs)
		return
	}

	cs.Conn.SetReadDeadline(time.Time{}) // сброс дедлайна

	if value.Typ != '*' || len(value.Array) == 0 {
		cs.Writer.Write(protocol.Value{Typ: '-', Str: "ERR invalid command"})
		return
	}

	result := s.handler(cs.Conn, value.Array)
	cs.Writer.Write(result)
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
