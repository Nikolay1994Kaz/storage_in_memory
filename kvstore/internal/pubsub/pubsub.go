package pubsub

import (
	"fmt"
	"log"
	"net"
	"strconv"
	"sync"
	"sync/atomic"

	"kvstore/kvstore/internal/protocol"
	"kvstore/kvstore/vector"
)

const (
	maxBuffer = 256
)

// Subscriber — единый подписчик, поддерживающий оба типа маршрутизации.
//
// Один conn = один Subscriber = один writePump = один канал доставки.
// Classic Publish и Semantic Publish оба пишут в тот же ch.
type Subscriber struct {
	ch   chan protocol.Value
	conn net.Conn
	done chan struct{}

	// Classic routing: набор строковых каналов
	channels map[string]struct{}

	// Semantic routing: вектор интересов в HNSW-индексе
	vecKey    string  // ключ в HNSW ("" = нет семантической подписки)
	threshold float32 // максимальная distance
}

// Hub — единый диспетчер Pub/Sub с двумя типами маршрутизации:
//   - Classic: точное совпадение имени канала (SUBSCRIBE/PUBLISH)
//   - Semantic: K-NN поиск в HNSW + threshold (VSIM.SUBSCRIBE/VSIM.PUBLISH)
//
// Один conn может иметь ОБА типа подписок одновременно.
// Все сообщения (classic и semantic) доставляются через единый канал и writePump.
type Hub struct {
	mu          sync.RWMutex
	subscribers map[net.Conn]*Subscriber

	// Classic routing
	channels map[string]map[*Subscriber]struct{}

	// Semantic routing (опционально)
	semIndex  *vector.VectorStore    // HNSW-индекс интересов подписчиков
	semSubs   map[string]*Subscriber // HNSW key → subscriber
	nextSemID atomic.Uint64
}

// NewHub создаёт единый Hub.
//
// semIndex — VectorStore для семантической маршрутизации (nil = только classic).
// Рекомендуется vector.NewVectorStoreCosine для семантической близости.
func NewHub(semIndex *vector.VectorStore) *Hub {
	h := &Hub{
		subscribers: make(map[net.Conn]*Subscriber),
		channels:    make(map[string]map[*Subscriber]struct{}),
	}
	if semIndex != nil {
		h.semIndex = semIndex
		h.semSubs = make(map[string]*Subscriber)
	}
	return h
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

// ═══════════════════════════════════════════════════════════════
// Classic Pub/Sub
// ═══════════════════════════════════════════════════════════════

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
		// Отписка от всех classic каналов — напрямую, без лишней аллокации слайса
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

	// Закрываем подписчика только если НЕТ ни classic, ни semantic подписок
	if len(sub.channels) == 0 && sub.vecKey == "" {
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

// ═══════════════════════════════════════════════════════════════
// Semantic Pub/Sub (Vector-routed)
// ═══════════════════════════════════════════════════════════════

// SemanticSubscribe регистрирует вектор интересов для соединения.
//
// Если conn уже имеет classic подписки — они сохраняются.
// Если conn уже имеет semantic подписку — она заменяется.
func (h *Hub) SemanticSubscribe(conn net.Conn, vec []float32, threshold float32) (*Subscriber, error) {
	if h.semIndex == nil {
		return nil, fmt.Errorf("semantic pub/sub not configured")
	}

	h.mu.Lock()
	defer h.mu.Unlock()

	sub := h.getOrCreateSub(conn)

	// Удаляем старую semantic подписку если есть
	if sub.vecKey != "" {
		h.semIndex.Delete(sub.vecKey)
		delete(h.semSubs, sub.vecKey)
	}

	id := h.nextSemID.Add(1)
	key := fmt.Sprintf("__sem:%d", id)

	if err := h.semIndex.Add(key, vec); err != nil {
		return nil, err
	}

	sub.vecKey = key
	sub.threshold = threshold
	h.semSubs[key] = sub

	// Подтверждение подписки
	confirm := protocol.Value{
		Typ: '*',
		Array: []protocol.Value{
			{Typ: '$', Str: "semantic-subscribe"},
			{Typ: '$', Str: "OK"},
			{Typ: ':', Num: 1},
		},
	}
	select {
	case sub.ch <- confirm:
	default:
	}

	return sub, nil
}

// SemanticUnsubscribe удаляет семантическую подписку.
// Classic подписки сохраняются.
func (h *Hub) SemanticUnsubscribe(conn net.Conn) bool {
	if h.semIndex == nil {
		return false
	}

	h.mu.Lock()
	defer h.mu.Unlock()

	sub, exists := h.subscribers[conn]
	if !exists || sub.vecKey == "" {
		return false
	}

	h.semIndex.Delete(sub.vecKey)
	delete(h.semSubs, sub.vecKey)
	sub.vecKey = ""
	sub.threshold = 0

	// Закрываем подписчика если нет и classic подписок
	if len(sub.channels) == 0 {
		h.closeSub(sub)
	}

	return true
}

// SemanticPublish доставляет сообщение подписчикам по векторной близости.
func (h *Hub) SemanticPublish(queryVec []float32, message string) int {
	if h.semIndex == nil {
		return 0
	}

	h.mu.RLock()
	subCount := len(h.semSubs)
	if subCount == 0 {
		h.mu.RUnlock()
		return 0
	}

	// HNSW search: найти ближайших подписчиков
	results, err := h.semIndex.Search(queryVec, subCount, nil)
	if err != nil || len(results) == 0 {
		h.mu.RUnlock()
		return 0
	}

	// Собрать matching подписчиков под RLock
	type target struct {
		sub *Subscriber
	}
	targets := make([]target, 0, len(results))

	for _, r := range results {
		sub, exists := h.semSubs[r.Key]
		if !exists {
			continue
		}
		if r.Distance <= sub.threshold {
			targets = append(targets, target{sub: sub})
		}
	}
	h.mu.RUnlock()

	if len(targets) == 0 {
		return 0
	}

	// Формируем RESP-сообщение ВНЕ лока
	msg := protocol.Value{
		Typ: '*',
		Array: []protocol.Value{
			{Typ: '$', Str: "semantic-message"},
			{Typ: '$', Str: message},
		},
	}

	// Доставка ВНЕ лока (аналогично Hub.Publish)
	delivered := 0
	for _, t := range targets {
		select {
		case t.sub.ch <- msg:
			delivered++
		default:
			log.Printf("Semantic Pub/Sub: slow subscriber disconnected")
			h.disconnectSlow(t.sub)
		}
	}

	return delivered
}

// SemanticSubscriberCount возвращает количество семантических подписчиков.
func (h *Hub) SemanticSubscriberCount() int {
	if h.semIndex == nil {
		return 0
	}
	h.mu.RLock()
	n := len(h.semSubs)
	h.mu.RUnlock()
	return n
}

// ═══════════════════════════════════════════════════════════════
// Общие методы
// ═══════════════════════════════════════════════════════════════

// RemoveConn удаляет ВСЕ подписки соединения (classic + semantic).
// Вызывается при отключении клиента.
func (h *Hub) RemoveConn(conn net.Conn) {
	h.mu.Lock()
	defer h.mu.Unlock()

	sub, exists := h.subscribers[conn]
	if !exists {
		return
	}

	// Очистка classic каналов
	for channel := range sub.channels {
		if subs, ok := h.channels[channel]; ok {
			delete(subs, sub)
			if len(subs) == 0 {
				delete(h.channels, channel)
			}
		}
	}

	// Очистка semantic подписки
	if sub.vecKey != "" && h.semIndex != nil {
		h.semIndex.Delete(sub.vecKey)
		delete(h.semSubs, sub.vecKey)
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

	case "VSIM.SUBSCRIBE":
		vec, threshold, err := parseVecThreshold(args)
		if err != nil {
			h.Send(conn, errValue("ERR "+err.Error()))
			return
		}
		if _, err := h.SemanticSubscribe(conn, vec, threshold); err != nil {
			h.Send(conn, errValue("ERR "+err.Error())) // подтверждение — через writePump
		}

	case "VSIM.UNSUBSCRIBE":
		h.Send(conn, protocol.Value{Typ: '+', Str: "OK"})
		h.SemanticUnsubscribe(conn)

	case "VSIM.PUBLISH":
		vec, msg, err := parseMsgVec(args)
		if err != nil {
			h.Send(conn, errValue("ERR "+err.Error()))
			return
		}
		count := h.SemanticPublish(vec, msg)
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

// parseVecThreshold разбирает "VSIM.SUBSCRIBE <threshold> <v1..vN>".
func parseVecThreshold(args [][]byte) ([]float32, float32, error) {
	if len(args) < 2 {
		return nil, 0, fmt.Errorf("usage: VSIM.SUBSCRIBE <threshold> <v1> ... <vN>")
	}
	threshold, err := strconv.ParseFloat(string(args[0]), 32)
	if err != nil || threshold < 0 {
		return nil, 0, fmt.Errorf("invalid threshold (must be non-negative float)")
	}
	vec := make([]float32, len(args)-1)
	for i := 1; i < len(args); i++ {
		f, err := strconv.ParseFloat(string(args[i]), 32)
		if err != nil {
			return nil, 0, fmt.Errorf("invalid float at position %d", i)
		}
		vec[i-1] = float32(f)
	}
	return vec, float32(threshold), nil
}

// parseMsgVec разбирает "VSIM.PUBLISH <message> <v1..vN>".
func parseMsgVec(args [][]byte) ([]float32, string, error) {
	if len(args) < 2 {
		return nil, "", fmt.Errorf("usage: VSIM.PUBLISH <message> <v1> ... <vN>")
	}
	msg := string(args[0])
	vec := make([]float32, len(args)-1)
	for i := 1; i < len(args); i++ {
		f, err := strconv.ParseFloat(string(args[i]), 32)
		if err != nil {
			return nil, "", fmt.Errorf("invalid float at position %d", i)
		}
		vec[i-1] = float32(f)
	}
	return vec, msg, nil
}

// IsSubscriber проверяет наличие любой подписки (classic или semantic).
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
// Отправляет ВСЕ сообщения (classic + semantic) в TCP.
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
			log.Printf("Pub/Sub: writePump panic recovered: %v", r)
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

// disconnectSlow отключает медленного подписчика.
// Очищает ОБА типа подписок. Безопасен при вызове из нескольких горутин.
func (h *Hub) disconnectSlow(sub *Subscriber) {
	h.mu.Lock()
	defer h.mu.Unlock()

	// Проверяем: не отключили ли уже другой горутиной?
	if _, exists := h.subscribers[sub.conn]; !exists {
		return // Уже отключен — выходим без паники
	}

	// Очистка classic
	for channel := range sub.channels {
		if subs, ok := h.channels[channel]; ok {
			delete(subs, sub)
			if len(subs) == 0 {
				delete(h.channels, channel)
			}
		}
	}

	// Очистка semantic
	if sub.vecKey != "" && h.semIndex != nil {
		h.semIndex.Delete(sub.vecKey)
		delete(h.semSubs, sub.vecKey)
	}

	close(sub.done)
	sub.conn.Close()
	delete(h.subscribers, sub.conn)
}
