package server

import (
	"fmt"
	"net"
	"sync"
	"syscall"
)

// Epoll оборачивает Linux epoll для неблокирующего I/O.
// Позволяет одному потоку следить за тысячами соединений.
type Epoll struct {
	fd          int                 // файловый дескриптор epoll instance
	connections map[int]*ConnState  // fd → состояние соединения
	mu          sync.RWMutex
}

// NewEpoll создаёт новый epoll instance.
// epoll_create1(0) — системный вызов, создающий epoll-объект в ядре.
func NewEpoll() (*Epoll, error) {
	fd, err := syscall.EpollCreate1(0)
	if err != nil {
		return nil, fmt.Errorf("epoll_create1: %w", err)
	}
	return &Epoll{
		fd:          fd,
		connections: make(map[int]*ConnState),
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

	return cs.Conn.Close()
}

// Wait ждёт событий на зарегистрированных соединениях.
// Возвращает список ConnState, на которых есть данные для чтения.
// Это БЛОКИРУЮЩИЙ вызов — горутина спит, пока не появятся события.
func (e *Epoll) Wait() ([]*ConnState, error) {
	events := make([]syscall.EpollEvent, 100) // до 100 событий за раз

	// epoll_wait — ядро разбудит нас, когда на каком-то сокете появятся данные
	// -1 означает "ждать бесконечно"
	n, err := syscall.EpollWait(e.fd, events, -1)
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

// Close закрывает epoll instance.
func (e *Epoll) Close() error {
	e.mu.Lock()
	defer e.mu.Unlock()

	for _, cs := range e.connections {
		cs.Conn.Close()
	}
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
