package pubsub

import (
	"fmt"
	"log/slog"
	"net"
	"sync"

	"kvstore/kvstore/internal/protocol"
)

const (
	maxBuffer = 256
)

// Subscriber — подписчик одного соединения.
//
// Один conn = один Subscriber = один writePump = один канал доставки.
type Subscriber struct {
	ch   chan protocol.Value
	conn net.Conn
	done chan struct{}

	// channels — набор каналов, на которые подписано соединение.
	channels map[string]struct{}
}

// Hub — диспетчер Pub/Sub: маршрутизация по точному имени канала
// (SUBSCRIBE/PUBLISH). Все сообщения подписчику идут через его writePump.
type Hub struct {
	mu          sync.RWMutex
	subscribers map[net.Conn]*Subscriber

	// channels — канал → его подписчики.
	channels map[string]map[*Subscriber]struct{}

	// onSlowClose — маршрут закрытия соединения через ЕДИНУЮ точку сервера
	// (Server.CloseConn): снятие с учёта activeConns/ActiveConnections и из
	// карты соединений. Без него disconnectSlow закрывал бы conn напрямую,
	// минуя учёт сервера → slot течёт → DoS при MaxConnections (H3). nil в юнит-
	// тестах без сервера → fallback на conn.Close(). Вызывается ВНЕ h.mu.
	onSlowClose func(net.Conn)
}

// SetOnSlowClose задаёт маршрут закрытия соединения для disconnectSlow.
// Сервер связывает его со Server.CloseConn (единая точка учёта соединений).
func (h *Hub) SetOnSlowClose(fn func(net.Conn)) {
	h.mu.Lock()
	h.onSlowClose = fn
	h.mu.Unlock()
}

// NewHub создаёт Hub.
func NewHub() *Hub {
	return &Hub{
		subscribers: make(map[net.Conn]*Subscriber),
		channels:    make(map[string]map[*Subscriber]struct{}),
	}
}

// getOrCreateSub возвращает существующего подписчика или создаёт нового.
// Вызывается под h.mu.Lock().
func (h *Hub) getOrCreateSub(conn net.Conn) *Subscriber {
	sub, exists := h.subscribers[conn]
	if !exists {
		sub = &Subscriber{
			ch:       make(chan protocol.Value, maxBuffer),
			conn:     conn,
			done:     make(chan struct{}),
			channels: make(map[string]struct{}),
		}
		h.subscribers[conn] = sub
		go sub.writePump()
	}
	return sub
}

// Subscribe подписывает соединение на строковые каналы.
func (h *Hub) Subscribe(conn net.Conn, channels []string) *Subscriber {
	h.mu.Lock()

	sub := h.getOrCreateSub(conn)

	// Подготавливаем сообщения-подтверждения ДО снятия блокировки.
	confirmations := make([]protocol.Value, 0, len(channels))

	for _, channel := range channels {
		if h.channels[channel] == nil {
			h.channels[channel] = make(map[*Subscriber]struct{})
		}
		h.channels[channel][sub] = struct{}{}
		sub.channels[channel] = struct{}{} // O(1) трекинг подписок

		confirmations = append(confirmations, protocol.Value{
			Typ: '*',
			Array: []protocol.Value{
				{Typ: '$', Str: "subscribe"},
				{Typ: '$', Str: channel},
				{Typ: ':', Num: len(sub.channels)},
			},
		})
	}

	h.mu.Unlock() // Теперь безопасно отпускаем лок

	// Отправляем сформированные подтверждения ВНЕ лока
	for _, msg := range confirmations {
		select {
		case sub.ch <- msg:
		default:
			// Клиент не читает даже подтверждения → отключаем
			h.disconnectSlow(sub)
			return sub
		}
	}

	return sub
}

// Unsubscribe отписывает соединение от каналов.
func (h *Hub) Unsubscribe(conn net.Conn, channels []string) {
	h.mu.Lock()
	defer h.mu.Unlock()

	sub, exists := h.subscribers[conn]
	if !exists {
		return
	}

	if len(channels) == 0 {
		// Отписка от всех каналов — напрямую, без лишней аллокации слайса
		for channel := range sub.channels {
			if subs, ok := h.channels[channel]; ok {
				delete(subs, sub)
				if len(subs) == 0 {
					delete(h.channels, channel)
				}
			}
		}
		clear(sub.channels) // Go 1.21+ — zero-alloc очистка мапы
	} else {
		// Отписка от конкретных каналов
		for _, channel := range channels {
			if subs, ok := h.channels[channel]; ok {
				delete(subs, sub)
				if len(subs) == 0 {
					delete(h.channels, channel)
				}
			}
			delete(sub.channels, channel)
		}
	}

	// Подписок не осталось — соединение выходит из режима подписки.
	if len(sub.channels) == 0 {
		h.closeSub(sub)
	}
}

var subscriberSlicePool = sync.Pool{
	New: func() any {
		s := make([]*Subscriber, 0, 64)
		return &s
	},
}

// Publish отправляет сообщение всем подписчикам канала.
func (h *Hub) Publish(channel string, message string) int {
	h.mu.RLock()
	subs, exists := h.channels[channel]
	if !exists {
		h.mu.RUnlock()
		return 0
	}

	ptr := subscriberSlicePool.Get().(*[]*Subscriber)
	recipients := *ptr

	for sub := range subs {
		recipients = append(recipients, sub)
	}
	h.mu.RUnlock()

	msg := protocol.Value{
		Typ: '*',
		Array: []protocol.Value{
			{Typ: '$', Str: "message"},
			{Typ: '$', Str: channel},
			{Typ: '$', Str: message},
		},
	}

	delivered := 0
	for _, sub := range recipients {
		select {
		case sub.ch <- msg:
			delivered++
		default:
			slog.Warn("pub/sub: slow subscriber disconnected")
			h.disconnectSlow(sub)
		}
	}

	// Очистка и возврат в Pool
	for i := range recipients {
		recipients[i] = nil
	}
	recipients = recipients[:0]

	if cap(recipients) <= 1024 {
		*ptr = recipients
		subscriberSlicePool.Put(ptr)
	}

	return delivered
}

// RemoveConn удаляет все подписки соединения. Вызывается при отключении клиента.
func (h *Hub) RemoveConn(conn net.Conn) {
	h.mu.Lock()
	defer h.mu.Unlock()

	sub, exists := h.subscribers[conn]
	if !exists {
		return
	}

	for channel := range sub.channels {
		if subs, ok := h.channels[channel]; ok {
			delete(subs, sub)
			if len(subs) == 0 {
				delete(h.channels, channel)
			}
		}
	}

	h.closeSub(sub)
}

// Send доставляет один RESP-кадр подписчику через его writePump — единственного
// писателя для conn в режиме подписки. Возвращает false, если conn не подписчик
// (тогда писать через writePump нельзя — его нет). Медленного подписчика отключает,
// та же дисциплина, что в Publish.
func (h *Hub) Send(conn net.Conn, v protocol.Value) bool {
	h.mu.RLock()
	sub, ok := h.subscribers[conn]
	h.mu.RUnlock()
	if !ok {
		return false
	}
	select {
	case sub.ch <- v:
		return true
	default:
		h.disconnectSlow(sub)
		return false
	}
}

// HandleSubscriberCommand обслуживает команду от соединения, УЖЕ находящегося в
// режиме подписки (IsSubscriber == true). Ключевой инвариант: ВСЕ ответы уходят
// через writePump (h.Send / подтверждения Subscribe), а НЕ через cs.Buf основного
// обработчика. Иначе два писателя (writePump + cs.Buf) параллельно пишут в один
// сокет — перемешанные кадры; плюс клиент-подписчик не ждёт обычных ответов
// (как в Redis subscriber-mode).
//
// Разрешены только команды pub/sub-семейства и PING; остальные отклоняются
// ошибкой (тоже через writePump).
func (h *Hub) HandleSubscriberCommand(conn net.Conn, cmd string, args [][]byte) {
	switch cmd {
	case "SUBSCRIBE":
		h.Subscribe(conn, toStrings(args)) // подтверждения — через writePump

	case "UNSUBSCRIBE":
		// ack ДО снятия: если это последняя подписка, Unsubscribe закроет writePump,
		// но тот дренирует канал на done — ack успеет уйти.
		h.Send(conn, protocol.Value{Typ: '+', Str: "OK"})
		h.Unsubscribe(conn, toStrings(args))

	case "PING":
		h.Send(conn, protocol.Value{Typ: '+', Str: "PONG"})

	case "PUBLISH":
		if len(args) < 2 {
			h.Send(conn, errValue("ERR wrong number of arguments for 'PUBLISH'"))
			return
		}
		count := h.Publish(string(args[0]), string(args[1]))
		h.Send(conn, protocol.Value{Typ: ':', Num: count})

	default:
		h.Send(conn, errValue(fmt.Sprintf(
			"ERR '%s' not allowed in subscriber mode (only pub/sub commands and PING)", cmd)))
	}
}

func errValue(msg string) protocol.Value { return protocol.Value{Typ: '-', Str: msg} }

func toStrings(args [][]byte) []string {
	out := make([]string, len(args))
	for i, a := range args {
		out[i] = string(a)
	}
	return out
}

// IsSubscriber сообщает, находится ли соединение в режиме подписки.
func (h *Hub) IsSubscriber(conn net.Conn) bool {
	h.mu.RLock()
	_, exists := h.subscribers[conn]
	h.mu.RUnlock()
	return exists
}

// ═══════════════════════════════════════════════════════════════
// Внутренние методы
// ═══════════════════════════════════════════════════════════════

// closeSub закрывает подписчика и удаляет из реестра.
// Вызывается под h.mu.Lock().
func (h *Hub) closeSub(sub *Subscriber) {
	close(sub.done)
	delete(h.subscribers, sub.conn)
}

// writePump — единственная горутина подписчика.
// Отправляет все сообщения подписчика в TCP.
//
// КРИТИЧНО: protocol.Writer буферизирован (bufio, 4KB). Write() лишь копирует
// в буфер — данные не уходят клиенту, пока буфер не переполнится ИЛИ пока не
// вызван Flush(). Поэтому после каждой пачки записей ОБЯЗАТЕЛЕН Flush(), иначе
// мелкие pub/sub-сообщения оседают в буфере и доставки фактически нет.
//
// Под нагрузкой сначала неблокирующе дренируем всё, что уже готово в канале
// (coalescing), и флашим один раз на пачку — так сохраняется выигрыш bufio
// (1 syscall на пачку), но без потери доставки на редких одиночных сообщениях.
func (s *Subscriber) writePump() {
	// recover: паника в Marshal/Write (напр. битый Value) не должна валить весь
	// процесс — падает только доставка этому одному подписчику.
	defer func() {
		if r := recover(); r != nil {
			slog.Error("pub/sub: writePump panic recovered", "panic", r)
		}
	}()

	writer := protocol.NewWriter(s.conn)

	for {
		select {
		case msg := <-s.ch:
			if err := writer.Write(msg); err != nil {
				return
			}
			// Coalescing: забираем без блокировки всё, что уже накопилось.
			for drained := false; !drained; {
				select {
				case msg := <-s.ch:
					if err := writer.Write(msg); err != nil {
						return
					}
				default:
					drained = true
				}
			}
			// Флаш: без него сообщения не доходят до клиента.
			if err := writer.Flush(); err != nil {
				return
			}
		case <-s.done:
			// Дренируем остаток перед выходом: финальные кадры (напр. ack на
			// UNSUBSCRIBE, снявший последнюю подписку) должны уйти клиенту, даже
			// если done уже закрыт. select при готовности обоих каналов случаен —
			// поэтому дренаж на done обязателен, иначе ack терялся бы.
			for {
				select {
				case msg := <-s.ch:
					if err := writer.Write(msg); err != nil {
						return
					}
				default:
					_ = writer.Flush()
					return
				}
			}
		}
	}
}

// disconnectSlow отключает медленного подписчика. Безопасен при вызове из
// нескольких горутин.
//
// H3: само закрытие соединения идёт через ЕДИНУЮ точку (onSlowClose →
// Server.CloseConn), а НЕ через прямой conn.Close(). Иначе учёт сервера
// (activeConns/ActiveConnections, карта соединений) не чистится → каждый
// медленный подписчик навсегда съедает слот → перманентный DoS при
// MaxConnections.
//
// Порядок: очистка подписок + удаление sub из карты — под h.mu; закрытие
// соединения — ВНЕ h.mu (Server.CloseConn зовёт OnDisconnect=RemoveConn,
// который снова берёт h.mu; sub уже удалён → ранний return, дедлока нет).
// Идемпотентно: параллельные вызовы находят !exists и выходят — закрывает
// ровно один.
func (h *Hub) disconnectSlow(sub *Subscriber) {
	h.mu.Lock()

	// Проверяем: не отключили ли уже другой горутиной?
	if _, exists := h.subscribers[sub.conn]; !exists {
		h.mu.Unlock()
		return // Уже отключен — выходим без паники
	}

	for channel := range sub.channels {
		if subs, ok := h.channels[channel]; ok {
			delete(subs, sub)
			if len(subs) == 0 {
				delete(h.channels, channel)
			}
		}
	}

	close(sub.done)
	delete(h.subscribers, sub.conn)
	closer := h.onSlowClose
	conn := sub.conn
	h.mu.Unlock()

	// Закрытие — ВНЕ лока, через единую точку учёта сервера (H3).
	if closer != nil {
		closer(conn)
	} else {
		conn.Close() // fallback: юнит-тесты Hub без сервера
	}
}
