package pubsub

import (
	"fmt"
	"log"
	"net"
	"sync"
	"sync/atomic"

	"kvstore/kvstore/internal/protocol"
	"kvstore/kvstore/vector"
)

// SemanticSub — подписчик семантического Pub/Sub.
// Получает сообщения, чей вектор ≤ threshold от его вектора интересов.
type SemanticSub struct {
	ch        chan protocol.Value
	conn      net.Conn
	done      chan struct{}
	key       string  // ключ в HNSW-индексе подписчиков
	threshold float32 // максимальная допустимая distance (0=exact, 1=orthogonal)
}

// SemanticHub — семантический Pub/Sub с маршрутизацией по векторной близости.
//
// Архитектура:
//
//	VSIM.SUBSCRIBE conn [vector] threshold → subscriberIndex.Add(key, vector)
//	VSIM.PUBLISH [vector] message          → subscriberIndex.Search(vector, K) → route
//	VSIM.UNSUBSCRIBE conn                  → subscriberIndex.Delete(key)
//
// Используется отдельный HNSW-индекс для хранения векторов интересов
// подписчиков. При публикации выполняется K-NN поиск по этому индексу,
// и сообщение доставляется только подписчикам с distance ≤ threshold.
type SemanticHub struct {
	mu    sync.RWMutex
	index *vector.VectorStore

	subs  map[string]*SemanticSub   // HNSW key → subscriber
	conns map[net.Conn]*SemanticSub // conn → subscriber

	nextID atomic.Uint64
}

// NewSemanticHub создаёт новый SemanticHub.
//
// index — VectorStore для хранения векторов интересов подписчиков.
// Рекомендуется NewVectorStoreCosine для семантической близости.
func NewSemanticHub(index *vector.VectorStore) *SemanticHub {
	return &SemanticHub{
		index: index,
		subs:  make(map[string]*SemanticSub),
		conns: make(map[net.Conn]*SemanticSub),
	}
}

// Subscribe регистрирует соединение с вектором интересов и порогом.
//
// conn      — сетевое соединение подписчика
// vec       — вектор интересов (будет добавлен в HNSW-индекс)
// threshold — максимальная distance для маршрутизации (0.0=exact, 1.0=любые)
//
// Если conn уже подписан — старая подписка заменяется.
// Подписчик получает подтверждение ["semantic-subscribe", "OK", 1].
func (sh *SemanticHub) Subscribe(conn net.Conn, vec []float32, threshold float32) (*SemanticSub, error) {
	sh.mu.Lock()
	defer sh.mu.Unlock()

	// Удаляем старую подписку если есть
	if old, exists := sh.conns[conn]; exists {
		sh.removeLocked(old)
	}

	id := sh.nextID.Add(1)
	key := fmt.Sprintf("__sem:%d", id)

	if err := sh.index.Add(key, vec); err != nil {
		return nil, err
	}

	sub := &SemanticSub{
		ch:        make(chan protocol.Value, maxBuffer),
		conn:      conn,
		done:      make(chan struct{}),
		key:       key,
		threshold: threshold,
	}

	sh.subs[key] = sub
	sh.conns[conn] = sub
	go sub.writePump()

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

// Unsubscribe удаляет семантическую подписку соединения.
// Возвращает true если подписка была найдена и удалена.
func (sh *SemanticHub) Unsubscribe(conn net.Conn) bool {
	sh.mu.Lock()
	defer sh.mu.Unlock()

	sub, exists := sh.conns[conn]
	if !exists {
		return false
	}

	sh.removeLocked(sub)
	return true
}

// Publish доставляет сообщение подписчикам, чьи вектора интересов
// находятся в пределах их threshold от вектора запроса.
//
// Алгоритм:
//  1. HNSW Search по индексу подписчиков → K ближайших
//  2. Фильтрация по threshold каждого подписчика
//  3. Non-blocking send в канал подписчика (slow → disconnect)
//
// Возвращает количество доставленных сообщений.
func (sh *SemanticHub) Publish(queryVec []float32, message string) int {
	sh.mu.RLock()
	subCount := len(sh.subs)
	if subCount == 0 {
		sh.mu.RUnlock()
		return 0
	}

	// HNSW search: найти ближайших подписчиков
	results, err := sh.index.Search(queryVec, subCount)
	if err != nil || len(results) == 0 {
		sh.mu.RUnlock()
		return 0
	}

	// Собрать matching подписчиков под RLock
	type target struct {
		sub *SemanticSub
	}
	targets := make([]target, 0, len(results))

	for _, r := range results {
		sub, exists := sh.subs[r.Key]
		if !exists {
			continue
		}
		if r.Distance <= sub.threshold {
			targets = append(targets, target{sub: sub})
		}
	}
	sh.mu.RUnlock()

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
			sh.disconnectSlow(t.sub)
		}
	}

	return delivered
}

// RemoveConn удаляет соединение (вызывается при отключении клиента).
func (sh *SemanticHub) RemoveConn(conn net.Conn) {
	sh.Unsubscribe(conn)
}

// SubscriberCount возвращает количество активных подписчиков.
func (sh *SemanticHub) SubscriberCount() int {
	sh.mu.RLock()
	n := len(sh.conns)
	sh.mu.RUnlock()
	return n
}

// IsSubscriber проверяет наличие семантической подписки у соединения.
func (sh *SemanticHub) IsSubscriber(conn net.Conn) bool {
	sh.mu.RLock()
	_, exists := sh.conns[conn]
	sh.mu.RUnlock()
	return exists
}

// --- Внутренние методы ---

func (sh *SemanticHub) removeLocked(sub *SemanticSub) {
	sh.index.Delete(sub.key)
	close(sub.done)
	delete(sh.subs, sub.key)
	delete(sh.conns, sub.conn)
}

// disconnectSlow отключает медленного подписчика.
// Вызывается вне RLock — берёт полный Lock для безопасного удаления.
func (sh *SemanticHub) disconnectSlow(sub *SemanticSub) {
	sh.mu.Lock()
	defer sh.mu.Unlock()

	// Idempotent: проверяем что подписчик ещё существует
	if _, exists := sh.conns[sub.conn]; !exists {
		return
	}

	sh.removeLocked(sub)
	sub.conn.Close()
}

// writePump — горутина подписчика. Отправляет RESP-сообщения в conn.
func (s *SemanticSub) writePump() {
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
