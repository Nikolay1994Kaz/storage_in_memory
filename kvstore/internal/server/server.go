package server

import (
	"crypto/tls"
	"fmt"
	"log"
	"net"
	"runtime"
	"sync"
	"sync/atomic"
	"time"

	"kvstore/kvstore/internal/monitoring"
)

const (
	// ReadTimeout — максимальное время ожидания данных от клиента.
	// Защита от Slowloris-атаки: если клиент прислал полкоманды
	// и замолчал — через 5 секунд соединение закроется.
	ReadTimeout = 5 * time.Second

	// drainTimeout — предел ожидания завершения воркеров в Stop(). Страховка на
	// случай, когда закрытие epoll-fd не разбудило заблокированный epoll_wait
	// (idle-timeout=0 → timeoutMs=-1): shutdown не должен зависать навсегда.
	drainTimeout = 5 * time.Second
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
	TxAborted     bool       // H2: в MULTI встретилась запрещённая команда → EXEC вернёт EXECABORT
	Authenticated bool       // true после успешной AUTH команды

	// LastActivity — unix-наносекунды последней активности (приём данных).
	// Реапер закрывает соединения, неактивные дольше Server.IdleTimeout. В этой
	// epoll-архитектуре дедлайн на сокете не реапит молчащие conn (EpollWait не
	// проснётся от Go-дедлайна), поэтому реапинг — по этому полю.
	LastActivity atomic.Int64
}

// WorkerReclaimer — хук QSBR-управления памятью аллокатора (см. tcmalloc).
//
// Воркер сообщает «тихое» состояние (ReportQuiescent) после пробуждения из
// epoll_wait — до обработки команд, и уходит offline (GoOffline) перед
// блокировкой в epoll_wait, когда гарантированно не держит ни одного хендла.
// Это позволяет аллокатору освобождать отложенные слоты не по таймеру, а по
// кворуму quiescence (фикс UAF T2). nil = не используется.
type WorkerReclaimer interface {
	ReportQuiescent(workerID int)
	GoOffline(workerID int)
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

	// IdleTimeout — макс. время без активности, после которого соединение
	// реапится (защита от Slowloris/брошенных conn). 0 = реапинг выключен.
	IdleTimeout time.Duration

	// WriteTimeout — макс. время на отправку ответа (Flush). Защита от застрявшего
	// reader, блокирующего горутину воркера. 0 = без дедлайна.
	WriteTimeout time.Duration

	// MaxConnections — потолок одновременных соединений (защита от исчерпания
	// fd/RAM). 0 = без лимита. Сверх лимита acceptLoop сразу закрывает новый conn.
	MaxConnections int

	// OnDisconnect — колбэк на закрытие любого соединения (единая точка в
	// Epoll.Remove). Обычно = hub.RemoveConn, чтобы отключение клиента чистило
	// его подписки Pub/Sub (иначе течёт семантический вектор в HNSW). nil = нет.
	OnDisconnect func(net.Conn)

	// Reclaimer — необязательный хук QSBR-освобождения памяти аллокатора.
	// Обычно = tcmalloc-store. nil = воркеры не рапортуют quiescence (тесты).
	Reclaimer WorkerReclaimer

	// activeConns — текущее число живых соединений (общий счётчик по всем epoll;
	// inc в Epoll.Add, dec в Epoll.Remove). Читается acceptLoop для проверки лимита.
	activeConns atomic.Int64

	// stopping — выставляется в Stop(); воркеры выходят из eventLoop, а не
	// крутятся в tight-loop на EpollWait-ошибке после закрытия epoll-fd.
	stopping atomic.Bool

	// wg отслеживает горутины воркеров для graceful drain: Stop() дожидается
	// завершения in-flight обработчиков, чтобы их WAL-записи попали в канал
	// ДО того, как вызывающий закроет WAL (иначе — send на закрытый канал).
	wg sync.WaitGroup
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
		ep.connCount = &s.activeConns // общий счётчик соединений на все воркеры
		ep.onRemove = s.OnDisconnect  // очистка подписок Pub/Sub при отключении
		s.workers[i] = &worker{id: i, epoll: ep}

		s.wg.Add(1)
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
			if s.stopping.Load() {
				return // штатная остановка: listener закрыт
			}
			log.Printf("Accept error: %v", err)
			return
		}

		// Потолок одновременных соединений: защита от исчерпания fd/RAM. Сразу
		// рвём лишнее соединение, не регистрируя его в epoll (не выделяя rbuf).
		// acceptLoop — единственная горутина-аксептор, поэтому проверка-перед-Add
		// без гонки: предыдущее соединение уже учтено (Add синхронный) к этому шагу.
		if s.MaxConnections > 0 && s.activeConns.Load() >= int64(s.MaxConnections) {
			conn.Close()
			monitoring.ConnRejected.Inc()
			continue
		}

		w := s.nextWorker()

		cs := &ConnState{
			Conn:     conn,
			Buf:      NewConnBuf(conn), // ← ConnBuf вместо Reader+Writer
			WorkerID: w.id,
		}
		cs.Buf.WriteTimeout = s.WriteTimeout
		cs.LastActivity.Store(time.Now().UnixNano())

		if err := w.epoll.Add(cs); err != nil {
			log.Printf("Epoll Add error (worker %d): %v", w.id, err)
			conn.Close()
			continue
		}
	}
}

// eventLoop — главный цикл воркера.
//
// Если IdleTimeout задан, EpollWait получает конечный таймаут (вместо -1), чтобы
// воркер периодически просыпался и реапил неактивные соединения — даже когда по
// ним нет событий (молчащий Slowloris-клиент epoll не разбудит).
func (s *Server) eventLoop(w *worker) {
	defer s.wg.Done()

	timeoutMs := -1 // по умолчанию блокируем бесконечно (реапинг выключен)
	if s.IdleTimeout > 0 {
		tick := s.IdleTimeout
		if tick > time.Second {
			tick = time.Second // подметаем не реже раза в секунду
		}
		timeoutMs = int(tick / time.Millisecond)
		if timeoutMs < 1 {
			timeoutMs = 1
		}
	}

	for {
		// Проверяем остановку и на таймаут-пробуждении, а не только на ошибке
		// Wait: закрытие epoll-fd из другой горутины не всегда будит уже
		// заблокированный epoll_wait, но при заданном idle-timeout воркер
		// просыпается по тику и выходит здесь чисто (graceful drain).
		if s.stopping.Load() {
			return
		}
		// QSBR: уходим offline перед блокировкой в epoll_wait. Здесь воркер
		// гарантированно не держит ни одного хендла (все команды обработаны,
		// значения скопированы), поэтому исключение из кворума безопасно и не
		// даёт спящему воркеру заморозить reclaim (см. tcmalloc/reclaim.go).
		if s.Reclaimer != nil {
			s.Reclaimer.GoOffline(w.id)
		}
		states, err := w.epoll.Wait(timeoutMs)
		// Проснулись → online: публикуем наблюдаемое поколение ДО обработки
		// команд (до любого lock-free Get).
		if s.Reclaimer != nil {
			s.Reclaimer.ReportQuiescent(w.id)
		}
		if err != nil {
			if s.stopping.Load() {
				return // сервер останавливается, epoll-fd закрыт — выходим чисто
			}
			log.Printf("Worker %d: epoll wait error: %v", w.id, err)
			continue
		}

		for _, cs := range states {
			s.handleConn(w, cs)
		}

		if s.IdleTimeout > 0 {
			w.epoll.reapIdle(s.IdleTimeout)
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
	// Defense-in-depth: паника в парсере или хендлере НЕ должна ронять воркер
	// (а с ним весь процесс). Закрываем только это соединение и живём дальше.
	defer func() {
		if r := recover(); r != nil {
			log.Printf("Worker %d: recovered panic on connection, closing it: %v", w.id, r)
			w.epoll.Remove(cs)
		}
	}()

	n, err := cs.Buf.ReadFromConn()
	if n > 0 {
		cs.LastActivity.Store(time.Now().UnixNano()) // сбрасываем счётчик idle
	}
	// ReadFromConn ставит ProtoErr, когда rbuf упёрся в потолок maxRbufSize
	// (клиент льёт данные без завершённого кадра → mem-DoS). Рвём с ошибкой.
	if cs.Buf.ProtoErr() {
		s.closeProtoErr(w, cs)
		return
	}
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

		// Нарушение протокола (невалидная/огромная bulk-длина, count > maxArgs):
		// битый кадр нельзя «дождать» — он застрянет в буфере. Рвём conn с ошибкой.
		if cs.Buf.ProtoErr() {
			s.closeProtoErr(w, cs)
			return
		}

		// Greedy drain: пробуем забрать ещё данные (non-blocking)
		if cs.Buf.TryRead() == 0 {
			break // нет данных — выходим
		}
		// Есть данные — продолжаем обработку
	}

	// TryRead в greedy-цикле тоже мог упереться в потолок rbuf.
	if cs.Buf.ProtoErr() {
		s.closeProtoErr(w, cs)
		return
	}

	// Один write() для ВСЕХ ответов (включая greedy drain). Ошибка (в т.ч.
	// write-таймаут на застрявшем reader) → закрываем соединение, чтобы залипший
	// клиент не держал воркера и буфер.
	if err := cs.Buf.Flush(); err != nil {
		w.epoll.Remove(cs)
	}
}

// closeProtoErr отвечает RESP-ошибкой и закрывает соединение при нарушении
// протокола или достижении потолка буфера. Битый/негодный кадр нельзя «дождать»,
// поэтому соединение рвётся (защита от wedge и mem-DoS).
func (s *Server) closeProtoErr(w *worker, cs *ConnState) {
	cs.Buf.WriteError("ERR Protocol error")
	cs.Buf.Flush()
	w.epoll.Remove(cs)
}

// CloseConn закрывает соединение по net.Conn через ЕДИНУЮ точку (Epoll.Remove):
// декремент connCount/ActiveConnections + EpollCtl DEL + удаление из карты epoll
// + очистка подписок (onRemove) + conn.Close().
//
// H3: Hub.disconnectSlow (эвикция медленного подписчика) обязан ходить сюда, а
// не звать conn.Close() напрямую — иначе учёт epoll не чистится и слот течёт
// навсегда → перманентный DoS при MaxConnections.
//
// Поиск по идентичности conn (а не по fd) — чтобы не звать socketFD на, возможно,
// уже закрытом соединении (паника) и быть устойчивым к гонке двойного закрытия.
// Путь редкий (эвикция) → O(N) допустим. Remove идемпотентен: если conn уже
// снят другим путём, EpollCtl DEL вернёт ошибку и Remove выйдет без двойного
// декремента.
func (s *Server) CloseConn(conn net.Conn) {
	for _, w := range s.workers {
		var target *ConnState
		w.epoll.mu.RLock()
		for _, cs := range w.epoll.connections {
			if cs.Conn == conn {
				target = cs
				break
			}
		}
		w.epoll.mu.RUnlock()
		if target != nil {
			w.epoll.Remove(target)
			return
		}
	}
	// Не найдено среди epoll (уже снято / не регистрировалось) — закрываем прямо.
	conn.Close()
}

// Stop останавливает сервер.
func (s *Server) Stop() error {
	s.stopping.Store(true) // воркеры выйдут из eventLoop, а не зациклятся на ошибке
	s.listener.Close()

	// Будим воркеров, стоящих в epoll_wait, ЧЕРЕЗ self-pipe — но пока НЕ закрываем
	// epoll-fd: закрытие под заблокированным epoll_wait его не разбудит. Разбуженный
	// воркер видит stopping=true и выходит из eventLoop сам.
	for _, w := range s.workers {
		w.epoll.Wake()
	}

	// Graceful drain: дожидаемся выхода воркеров — их in-flight обработчики
	// дозавершатся и отправят свои WAL-записи в канал ДО его закрытия вызывающим.
	// Ограничиваем drainTimeout, чтобы shutdown не завис при непредвиденной блокировке.
	done := make(chan struct{})
	go func() {
		s.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(drainTimeout):
		log.Println("Server.Stop: drain timeout — продолжаем остановку")
	}

	// Воркеры вышли (или таймаут) — теперь безопасно закрыть epoll-fd и соединения.
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
