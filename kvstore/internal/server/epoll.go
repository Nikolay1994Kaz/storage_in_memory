package server

import (
	"fmt"
	"net"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"kvstore/kvstore/internal/monitoring"
)

// Epoll оборачивает Linux epoll для неблокирующего I/O.
// Позволяет одному потоку следить за тысячами соединений.
type Epoll struct {
	fd          int                // файловый дескриптор epoll instance
	connections map[int]*ConnState // fd → состояние соединения
	mu          sync.RWMutex

	// connCount — общий на все воркеры счётчик живых соединений (указатель на
	// Server.activeConns). Inc в Add, dec в Remove. nil = не считаем (legacy).
	connCount *atomic.Int64

	// onRemove — колбэк на удаление соединения (единая точка). Нужен, чтобы при
	// отключении клиента почистить его подписки Pub/Sub: иначе семантический
	// вектор навсегда остаётся в HNSW-индексе, а горутина writePump висит.
	// Идемпотентен (для неподписанного conn — no-op). nil = не вызываем.
	onRemove func(net.Conn)

	// wakeR/wakeW — self-pipe для пробуждения заблокированного epoll_wait при
	// остановке. Закрытие epoll-fd из другой горутины НЕ гарантирует возврат из
	// epoll_wait(-1); Close() пишет байт в wakeW → read-конец готов к чтению →
	// epoll_wait просыпается, и воркер выходит по флагу stopping (graceful drain).
	wakeR int
	wakeW int
}

// NewEpoll создаёт новый epoll instance.
// epoll_create1(0) — системный вызов, создающий epoll-объект в ядре.
func NewEpoll() (*Epoll, error) {
	fd, err := syscall.EpollCreate1(0)
	if err != nil {
		return nil, fmt.Errorf("epoll_create1: %w", err)
	}

	// Self-pipe: read-конец регистрируем в epoll, в write-конец пишет Close().
	// O_NONBLOCK — чтобы Write в Close() не заблокировался, если буфер полон;
	// O_CLOEXEC — не наследуется дочерними процессами.
	var p [2]int
	if err := syscall.Pipe2(p[:], syscall.O_NONBLOCK|syscall.O_CLOEXEC); err != nil {
		syscall.Close(fd)
		return nil, fmt.Errorf("wake pipe: %w", err)
	}
	if err := syscall.EpollCtl(fd, syscall.EPOLL_CTL_ADD, p[0], &syscall.EpollEvent{
		Events: syscall.EPOLLIN,
		Fd:     int32(p[0]),
	}); err != nil {
		syscall.Close(p[0])
		syscall.Close(p[1])
		syscall.Close(fd)
		return nil, fmt.Errorf("epoll_ctl wake pipe: %w", err)
	}

	return &Epoll{
		fd:          fd,
		connections: make(map[int]*ConnState),
		wakeR:       p[0],
		wakeW:       p[1],
	}, nil
}

// Add регистрирует соединение в epoll.
// После этого epoll будет уведомлять нас, когда на этом сокете появятся данные.
func (e *Epoll) Add(cs *ConnState) error {
	// Извлекаем "сырой" файловый дескриптор из net.Conn
	fd := socketFD(cs.Conn)

	// Говорим ядру: "следи за этим fd, сообщи когда можно читать"
	// EPOLLIN — событие "данные готовы для чтения"
	// EPOLLHUP — событие "клиент отключился"
	err := syscall.EpollCtl(e.fd, syscall.EPOLL_CTL_ADD, fd, &syscall.EpollEvent{
		Events: syscall.EPOLLIN | syscall.EPOLLHUP,
		Fd:     int32(fd),
	})
	if err != nil {
		return fmt.Errorf("epoll_ctl ADD: %w", err)
	}

	e.mu.Lock()
	e.connections[fd] = cs
	e.mu.Unlock()

	if e.connCount != nil {
		e.connCount.Add(1)
	}
	monitoring.ActiveConnections.Inc()

	return nil
}

// Remove убирает соединение из epoll и закрывает его.
func (e *Epoll) Remove(cs *ConnState) error {
	fd := socketFD(cs.Conn)

	err := syscall.EpollCtl(e.fd, syscall.EPOLL_CTL_DEL, fd, nil)
	if err != nil {
		return fmt.Errorf("epoll_ctl DEL: %w", err)
	}

	e.mu.Lock()
	delete(e.connections, fd)
	e.mu.Unlock()

	if e.connCount != nil {
		e.connCount.Add(-1)
	}
	monitoring.ActiveConnections.Dec()

	// Единая точка очистки подписок Pub/Sub при любом закрытии соединения
	// (ошибка, EOF, idle-реап, shutdown). Вызывается вне e.mu — RemoveConn берёт
	// свой лок в Hub, пересечения блокировок нет.
	if e.onRemove != nil {
		e.onRemove(cs.Conn)
	}

	return cs.Conn.Close()
}

// Wait ждёт событий на зарегистрированных соединениях.
// Возвращает список ConnState, на которых есть данные для чтения.
//
// timeoutMs — макс. время ожидания в миллисекундах: -1 = блокировать бесконечно,
// >0 = проснуться по таймауту даже без событий (чтобы реапер мог подмести idle).
func (e *Epoll) Wait(timeoutMs int) ([]*ConnState, error) {
	events := make([]syscall.EpollEvent, 100) // до 100 событий за раз

	n, err := syscall.EpollWait(e.fd, events, timeoutMs)
	if err != nil {
		// EINTR — нас прервал сигнал, это нормально, просто повторяем
		if err == syscall.EINTR {
			return nil, nil
		}
		return nil, fmt.Errorf("epoll_wait: %w", err)
	}

	e.mu.RLock()
	defer e.mu.RUnlock()

	var states []*ConnState
	for i := 0; i < n; i++ {
		cs, ok := e.connections[int(events[i].Fd)]
		if ok {
			states = append(states, cs)
		}
	}
	return states, nil
}

// reapIdle закрывает соединения, неактивные дольше timeout. Вызывается воркером
// после каждого Wait. Сбор stale идёт под RLock, само закрытие (Remove берёт
// Lock) — вне него, чтобы не словить рекурсивный лок.
func (e *Epoll) reapIdle(timeout time.Duration) {
	cutoff := time.Now().Add(-timeout).UnixNano()

	e.mu.RLock()
	var stale []*ConnState
	for _, cs := range e.connections {
		if cs.LastActivity.Load() < cutoff {
			stale = append(stale, cs)
		}
	}
	e.mu.RUnlock()

	for _, cs := range stale {
		e.Remove(cs)
		monitoring.IdleReaped.Inc()
	}
}

// Wake будит заблокированный epoll_wait, записывая байт в self-pipe.
//
// Вызывается ДО Close(): нельзя закрывать epoll-fd, пока воркер стоит в
// epoll_wait — закрытие fd под ним не гарантирует возврат. Правильный порядок
// (см. Server.Stop): Wake() всем воркерам → дождаться их выхода → Close() всем.
// Best-effort: ошибку Write игнорируем (non-blocking pipe, воркер мог уже выйти).
func (e *Epoll) Wake() {
	syscall.Write(e.wakeW, []byte{1})
}

// Close закрывает epoll instance и self-pipe. Соединения тоже закрываются.
// Вызывать только ПОСЛЕ выхода воркера из eventLoop (см. Wake).
func (e *Epoll) Close() error {
	e.mu.Lock()
	defer e.mu.Unlock()

	for _, cs := range e.connections {
		cs.Conn.Close()
	}
	syscall.Close(e.wakeR)
	syscall.Close(e.wakeW)
	return syscall.Close(e.fd)
}

// socketFD извлекает числовой файловый дескриптор из net.Conn.
// Используем SyscallConn().Control() — это даёт прямой доступ к fd
// БЕЗ вызова dup(), который ломал бы сокет для epoll.
func socketFD(conn net.Conn) int {
	tcpConn := conn.(*net.TCPConn)
	rawConn, err := tcpConn.SyscallConn()
	if err != nil {
		panic(fmt.Sprintf("SyscallConn failed: %v", err))
	}
	var fd int
	rawConn.Control(func(f uintptr) {
		fd = int(f)
	})
	return fd
}

func (e *Epoll) Count() int {
	e.mu.RLock()
	n := len(e.connections)
	e.mu.RUnlock()
	return n
}
