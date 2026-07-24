package vmemmcp

import (
	"fmt"
	"net"
	"sync"
	"time"

	"kvstore/kvstore/internal/protocol"
)

// opTimeout — потолок одной RESP-операции. REMEMBER fsync-bound (~десятки мс
// под миксом), даём широкий запас: зависший вызов хуже медленного —
// у хоста свой таймаут tool-вызова.
const opTimeout = 30 * time.Second

// RESPClient — ленивое RESP-соединение с одним редиалом на вызов: сессии
// агентов живут часами, сервер за это время может перезапуститься.
type RESPClient struct {
	addr string
	auth string

	mu   sync.Mutex
	conn net.Conn
	w    *protocol.Writer
	r    *protocol.Reader
}

func NewRESPClient(addr, auth string) *RESPClient {
	return &RESPClient{addr: addr, auth: auth}
}

func (c *RESPClient) connectLocked() error {
	conn, err := net.DialTimeout("tcp", c.addr, 5*time.Second)
	if err != nil {
		return err
	}
	c.conn, c.w, c.r = conn, protocol.NewWriter(conn), protocol.NewReader(conn)
	if c.auth != "" {
		v, err := c.roundtripLocked([]protocol.Value{bulk("AUTH"), bulk(c.auth)})
		if err != nil {
			c.dropLocked()
			return fmt.Errorf("AUTH failed: %w", err)
		}
		if v.Typ == '-' {
			c.dropLocked()
			return fmt.Errorf("AUTH rejected: %s", v.Str)
		}
	}
	return nil
}

func (c *RESPClient) dropLocked() {
	if c.conn != nil {
		c.conn.Close()
		c.conn = nil
	}
}

func (c *RESPClient) roundtripLocked(args []protocol.Value) (protocol.Value, error) {
	c.conn.SetDeadline(time.Now().Add(opTimeout))
	if err := c.w.Write(protocol.Value{Typ: '*', Array: args}); err != nil {
		return protocol.Value{}, err
	}
	if err := c.w.Flush(); err != nil {
		return protocol.Value{}, err
	}
	return c.r.Read()
}

// Do выполняет одну команду. Транспортная ошибка → один редиал и повтор
// (безопасно: REMEMBER идемпотентен только с клиентским ID, но адаптер его не
// шлёт — поэтому повтор ТОЛЬКО если запись даже не ушла в сокет; после
// частичной записи возвращаем ошибку, хост решает сам).
func (c *RESPClient) Do(args []protocol.Value) (protocol.Value, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.conn == nil {
		if err := c.connectLocked(); err != nil {
			return protocol.Value{}, err
		}
	}
	v, err := c.roundtripLocked(args)
	if err != nil {
		// Соединение мертво (сервер перезапускался). Редиал и одна повторная
		// попытка целиком — прежний запрос гарантированно не был принят,
		// только если сокет умер на записи; RESP не даёт этого различить,
		// поэтому повторяем только чтения и явно неидемпотентное не шлём
		// повторно: RECALL/FORGET безопасны (FORGET идемпотентен по
		// контракту), REMEMBER без клиентского ID — нет.
		c.dropLocked()
		if string(args[0].Str) == "VMEM.REMEMBER" {
			return protocol.Value{}, fmt.Errorf("connection lost mid-call: %w", err)
		}
		if cerr := c.connectLocked(); cerr != nil {
			return protocol.Value{}, cerr
		}
		v, err = c.roundtripLocked(args)
		if err != nil {
			c.dropLocked()
			return protocol.Value{}, err
		}
	}
	if v.Typ == '-' {
		return v, RespError(v.Str)
	}
	return v, nil
}
