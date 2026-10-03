package server

import (
	"crypto/tls"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"kvstore/kvstore/internal/monitoring"
)

// drainTimeout — предел ожидания завершения обработчиков в Stop(). Страховка на
// случай непредвиденной блокировки: shutdown не должен зависать навсегда.
const drainTimeout = 5 * time.Second

// ConnState хранит состояние соединения.
//
// Buf — per-connection буфер чтения/записи (заменяет bufio.Reader + bufio.Writer).
type ConnState struct {
	Conn          net.Conn
	Buf           *ConnBuf
	InTx          bool
	TxQueue       [][][]byte
	TxAborted     bool // H2: в MULTI встретилась запрещённая команда → EXEC вернёт EXECABORT
	Authenticated bool // true после успешной AUTH команды

	closeOnce sync.Once
}

// Handler — функция обработки RESP-команды.
//
// Handler НЕ возвращает значение. Вместо этого пишет ответ прямо в cs.Buf:
//
//	cs.Buf.WriteSimpleString("OK")
//	cs.Buf.WriteBulk(value)
//	cs.Buf.WriteError("ERR ...")
type Handler func(cs *ConnState, args [][]byte)

// Server — TCP-сервер: горутина на соединение поверх netpoller Go.
//
// Прежде здесь был свой слой epoll: net.Listener отдавал соединение, его fd
// регистрировался во втором epoll-инстансе на воркер (в первом его уже держит
// netpoller рантайма), а читалось всё равно через conn.Read Go. Свой epoll
// нёс одну функцию — стабильный WorkerID для MCache самописного аллокатора;
// аллокатора больше нет, и вместе с ним ушли:
//   - TLS, падавший паникой на первом соединении (socketFD приводил conn к
//     *net.TCPConn, а TLS-листенер отдаёт *tls.Conn); «минимальный фикс» через
//     NetConn() не спасал — tls.Conn вычитывает из сокета несколько записей в
//     свой буфер, epoll больше не просыпается, и конвейер команд висит;
//   - собственные wake-pipe, реапер и учёт fd, на которые пришлась половина
//     фиксов этого пакета; Linux-only.
//
// Горячий путь прежний: ConnBuf (разбор без аллокаций), жадный дочит готовых
// байт без ожидания (TryRead), один Flush на пачку ответов.
type Server struct {
	addr      string
	handler   Handler
	listener  net.Listener
	TLSConfig *tls.Config // nil = plain TCP, not-nil = TLS

	// IdleTimeout — макс. время без приёма данных, после которого соединение
	// закрывается (Slowloris, брошенные соединения). Реализовано дедлайном
	// чтения: каждое чтение ждёт не дольше IdleTimeout. 0 = не закрывать.
	IdleTimeout time.Duration

	// WriteTimeout — макс. время на отправку ответа (Flush). Защита от
	// застрявшего читателя на той стороне. 0 = без дедлайна.
	WriteTimeout time.Duration

	// MaxConnections — потолок одновременных соединений (защита от исчерпания
	// fd/RAM). 0 = без лимита. Сверх лимита acceptLoop сразу закрывает новое.
	MaxConnections int

	// OnDisconnect — колбэк на закрытие любого соединения (единая точка
	// closeConn). Обычно = hub.RemoveConn, чтобы отключение клиента чистило его
	// подписки Pub/Sub (иначе течёт семантический вектор в HNSW). nil = нет.
	OnDisconnect func(net.Conn)

	// IdleExempt — предикат освобождения соединения от закрытия по IdleTimeout.
	// Обычно = hub.IsSubscriber: подписчик после SUBSCRIBE легитимно молчит
	// (получает только push'и), Redis тоже не применяет timeout к CLIENT_PUBSUB.
	// Мёртвых peer'ов среди них ловят TCP keepalive и ошибка записи writePump.
	// nil = закрываются все молчащие.
	IdleExempt func(net.Conn) bool

	// activeConns — текущее число живых соединений. Inc в acceptLoop (до старта
	// горутины соединения), dec в closeConn — ровно один раз на соединение.
	activeConns atomic.Int64

	// stopping — выставляется в Stop(): acceptLoop и горутины соединений
	// выходят на ближайшей границе, а не продолжают работу.
	stopping atomic.Bool

	// wg отслеживает горутины соединений для graceful drain: Stop() дожидается
	// in-flight обработчиков, чтобы их WAL-записи попали в канал ДО того, как
	// вызывающий закроет WAL (иначе — send на закрытый канал).
	wg sync.WaitGroup

	mu    sync.Mutex
	conns map[net.Conn]*ConnState // живые соединения — для CloseConn и Stop
}

func NewServer(addr string, handler Handler) *Server {
	return &Server{
		addr:    addr,
		handler: handler,
		conns:   make(map[net.Conn]*ConnState),
	}
}

// Start открывает листенер и начинает принимать соединения.
func (s *Server) Start() error {
	ln, err := net.Listen("tcp", s.addr)
	if err != nil {
		return fmt.Errorf("failed to listen on %s: %w", s.addr, err)
	}
	if s.TLSConfig != nil {
		ln = tls.NewListener(ln, s.TLSConfig)
		slog.Info("TLS enabled")
	}
	s.startOn(ln)
	slog.Info("server listening", "addr", s.addr, "mode", "goroutine-per-conn")
	return nil
}

// startOn запускает приём на готовом листенере (тесты подставляют свой).
func (s *Server) startOn(ln net.Listener) {
	s.listener = ln
	go s.acceptLoop()
}

// acceptLoop принимает соединения и запускает на каждое свою горутину.
//
// Временная ошибка Accept (EMFILE/ENFILE — кончились дескрипторы,
// ECONNABORTED) не останавливает приём: пауза с ростом до секунды и повтор,
// как в net/http. Прежде любая ошибка завершала цикл навсегда — процесс жил,
// но новых клиентов больше не принимал.
func (s *Server) acceptLoop() {
	var backoff time.Duration
	for {
		conn, err := s.listener.Accept()
		if err != nil {
			if s.stopping.Load() || errors.Is(err, net.ErrClosed) {
				return
			}
			var ne net.Error
			//nolint:staticcheck // Temporary() — единственный признак EMFILE/ENFILE у ошибки Accept.
			if errors.As(err, &ne) && ne.Temporary() {
				if backoff == 0 {
					backoff = 5 * time.Millisecond
				} else if backoff *= 2; backoff > time.Second {
					backoff = time.Second
				}
				slog.Error("accept error, retrying", "err", err, "backoff", backoff)
				time.Sleep(backoff)
				continue
			}
			slog.Error("accept error, listener stopped", "err", err)
			return
		}
		backoff = 0

		// Потолок одновременных соединений: acceptLoop — единственный аксептор,
		// счётчик увеличивается здесь же, поэтому проверка-перед-Add без гонки.
		if s.MaxConnections > 0 && s.activeConns.Load() >= int64(s.MaxConnections) {
			conn.Close()
			monitoring.ConnRejected.Inc()
			continue
		}

		cs := &ConnState{Conn: conn, Buf: NewConnBuf(conn)}
		cs.Buf.WriteTimeout = s.WriteTimeout

		s.mu.Lock()
		if s.stopping.Load() {
			// Stop уже обошёл карту соединений — это соединение он не увидит.
			s.mu.Unlock()
			conn.Close()
			return
		}
		s.conns[conn] = cs
		s.mu.Unlock()
		s.activeConns.Add(1)
		monitoring.ActiveConnections.Inc()

		s.wg.Add(1)
		go s.serve(cs)
	}
}

// serve — жизнь одного соединения: чтение → разбор всех готовых команд →
// жадный дочит → один Flush на пачку ответов.
func (s *Server) serve(cs *ConnState) {
	defer s.wg.Done()
	defer s.closeConn(cs)
	// Паника в парсере или обработчике закрывает только это соединение.
	defer func() {
		if r := recover(); r != nil {
			slog.Error("recovered panic on connection, closing it", "panic", r)
		}
	}()

	for {
		if s.IdleTimeout > 0 {
			_ = cs.Conn.SetReadDeadline(time.Now().Add(s.IdleTimeout))
		}
		// Проверка ПОСЛЕ взвода дедлайна: Stop выставляет stopping, затем
		// дедлайн «сейчас»; если мы взвели свой позже его, то увидим stopping
		// здесь, а не уснём в чтении на весь IdleTimeout.
		if s.stopping.Load() {
			return
		}
		n, err := cs.Buf.ReadFromConn()
		// ReadFromConn ставит ProtoErr, когда rbuf упёрся в потолок maxRbufSize
		// (клиент льёт данные без завершённого кадра → mem-DoS).
		if cs.Buf.ProtoErr() {
			s.writeProtoErr(cs)
			return
		}
		if err != nil {
			if errors.Is(err, os.ErrDeadlineExceeded) && !s.stopping.Load() {
				if s.IdleExempt != nil && s.IdleExempt(cs.Conn) {
					continue
				}
				monitoring.IdleReaped.Inc()
			}
			return
		}
		if n == 0 {
			continue
		}

		for {
			for {
				args := cs.Buf.ParseCommand()
				if args == nil {
					break
				}
				s.handler(cs, args)
			}
			// Битый кадр (невалидная/огромная bulk-длина, count > maxArgs) нельзя
			// «дождать» — он застрянет в буфере. Рвём соединение с ошибкой.
			if cs.Buf.ProtoErr() {
				s.writeProtoErr(cs)
				return
			}
			// Жадный дочит: пока разбирали, клиент мог прислать ещё —
			// забираем без ожидания и отвечаем на всё одним Flush.
			if cs.Buf.TryRead() == 0 {
				break
			}
		}
		if cs.Buf.ProtoErr() {
			s.writeProtoErr(cs)
			return
		}
		// Ошибка записи (в т.ч. WriteTimeout на застрявшем читателе) — закрываем.
		if err := cs.Buf.Flush(); err != nil {
			return
		}
	}
}

// writeProtoErr отвечает RESP-ошибкой перед закрытием соединения при нарушении
// протокола или достижении потолка буфера.
func (s *Server) writeProtoErr(cs *ConnState) {
	cs.Buf.WriteError("ERR Protocol error")
	_ = cs.Buf.Flush()
}

// closeConn — ЕДИНАЯ точка закрытия соединения: снять с учёта, вызвать
// OnDisconnect, закрыть. Ровно один раз на соединение, сколько бы путей ни
// пришли сюда одновременно (выход горутины, CloseConn, Stop) — иначе
// задвоенный декремент или утёкший слот (ложный отказ по MaxConnections) и
// неочищенная подписка Pub/Sub.
func (s *Server) closeConn(cs *ConnState) {
	cs.closeOnce.Do(func() {
		s.mu.Lock()
		delete(s.conns, cs.Conn)
		s.mu.Unlock()
		s.activeConns.Add(-1)
		monitoring.ActiveConnections.Dec()
		// Вне s.mu: RemoveConn берёт свой лок в Hub, пересечения блокировок нет.
		if s.OnDisconnect != nil {
			s.OnDisconnect(cs.Conn)
		}
		cs.Conn.Close()
	})
}

// CloseConn закрывает соединение по net.Conn через единую точку closeConn.
//
// H3: Hub.disconnectSlow (эвикция медленного подписчика) обязан ходить сюда, а
// не звать conn.Close() напрямую — иначе учёт не чистится и слот течёт навсегда
// → перманентный DoS при MaxConnections. Горутина этого соединения получит
// ошибку чтения и выйдет; её собственный вызов closeConn будет no-op.
func (s *Server) CloseConn(conn net.Conn) {
	s.mu.Lock()
	cs := s.conns[conn]
	s.mu.Unlock()
	if cs != nil {
		s.closeConn(cs)
		return
	}
	// Не найдено (уже снято или не регистрировалось) — закрываем прямо.
	conn.Close()
}

// Stop останавливает сервер.
//
// Порядок: перестать принимать → разбудить горутины, ждущие данных (дедлайн
// чтения «сейчас»), не трогая тех, кто в обработчике, → дождаться их выхода
// (их WAL-записи уходят в канал ДО того, как вызывающий закроет WAL) → закрыть
// то, что осталось после drainTimeout.
func (s *Server) Stop() error {
	s.stopping.Store(true)
	if s.listener != nil {
		s.listener.Close()
	}

	now := time.Now()
	s.mu.Lock()
	for conn := range s.conns {
		_ = conn.SetReadDeadline(now)
	}
	s.mu.Unlock()

	done := make(chan struct{})
	go func() {
		s.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(drainTimeout):
		slog.Warn("server stop: drain timeout — продолжаем остановку")
	}

	s.mu.Lock()
	rest := make([]*ConnState, 0, len(s.conns))
	for _, cs := range s.conns {
		rest = append(rest, cs)
	}
	s.mu.Unlock()
	for _, cs := range rest {
		s.closeConn(cs)
	}
	return nil
}
