package pubsub

import (
	"log"
	"net"
	"sync"

	"kvstore/kvstore/internal/protocol"
)

const (
	maxBuffer = 256
)

// Subscriber — один подписчик.
type Subscriber struct {
	ch       chan protocol.Value
	conn     net.Conn
	done     chan struct{}
	channels map[string]struct{} // каналы этого подписчика — O(1) lookup
}

// Hub — центральный диспетчер Pub/Sub.
type Hub struct {
	mu          sync.RWMutex
	channels    map[string]map[*Subscriber]struct{}
	subscribers map[net.Conn]*Subscriber
}

func NewHub() *Hub {
	return &Hub{
		channels:    make(map[string]map[*Subscriber]struct{}),
		subscribers: make(map[net.Conn]*Subscriber),
	}
}

// Subscribe подписывает соединение на каналы.
func (h *Hub) Subscribe(conn net.Conn, channels []string) *Subscriber {
	h.mu.Lock()

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

	// Подготавливаем сообщения-подтверждения ДО снятия блокировки.
	// len(sub.channels) читается безопасно под локом.
	confirmations := make([]protocol.Value, 0, len(channels))

	for _, channel := range channels {
		if h.channels[channel] == nil {
			h.channels[channel] = make(map[*Subscriber]struct{})
		}
		h.channels[channel][sub] = struct{}{}
		sub.channels[channel] = struct{}{} // O(1) трекинг подписок

		// Собираем ответ, пока мапа безопасно заблокирована.
		// Инкремент счётчика корректен — как в настоящем Redis.
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
		// Отписка от всех — напрямую, без лишней аллокации слайса
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

	// Если больше нет подписок — закрываем подписчика
	if len(sub.channels) == 0 {
		close(sub.done)
		delete(h.subscribers, conn)
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
			log.Printf("Pub/Sub: slow subscriber disconnected")
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

// RemoveConn вызывается при отключении клиента.
func (h *Hub) RemoveConn(conn net.Conn) {
	h.Unsubscribe(conn, nil)
}

// IsSubscriber проверяет, является ли соединение подписчиком.
func (h *Hub) IsSubscriber(conn net.Conn) bool {
	h.mu.RLock()
	_, exists := h.subscribers[conn]
	h.mu.RUnlock()
	return exists
}

// --- Внутренние методы ---

// writePump — горутина подписчика.
func (s *Subscriber) writePump() {
	writer := protocol.NewWriter(s.conn)

	for {
		select {
		case msg := <-s.ch:
			if err := writer.Write(msg); err != nil {
				return
			}
		case <-s.done:
			return
		}
	}
}

// disconnectSlow отключает медленного подписчика.
// Безопасен при вызове из нескольких горутин одновременно.
func (h *Hub) disconnectSlow(sub *Subscriber) {
	h.mu.Lock()
	defer h.mu.Unlock()

	// Проверяем: не отключили ли уже другой горутиной?
	if _, exists := h.subscribers[sub.conn]; !exists {
		return // Уже отключен — выходим без паники
	}

	// Убираем из всех каналов
	for channel := range sub.channels {
		if subs, ok := h.channels[channel]; ok {
			delete(subs, sub)
			if len(subs) == 0 {
				delete(h.channels, channel)
			}
		}
	}

	close(sub.done)
	sub.conn.Close()
	delete(h.subscribers, sub.conn)
}
