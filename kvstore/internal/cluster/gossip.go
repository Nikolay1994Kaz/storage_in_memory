package cluster

import (
	"encoding/json"
	"fmt"
	"log"
	"math/rand"
	"net"
	"time"
)

// ============================================================
// ФОРМАТ СООБЩЕНИЙ
//
// Ноды общаются JSON-сообщениями через TCP.
// В реальном Redis — бинарный формат (быстрее, компактнее).
// Мы используем JSON для читаемости и простоты отладки.
//
// Пример PING от Node-A:
//   {
//     "type": "PING",
//     "sender": {
//       "id": "8702f3e4",
//       "addr": "127.0.0.1:6380",
//       "gossip_port": 6381,
//       "state": "online",
//       "slots": [[0, 5460]]
//     },
//     "nodes": [
//       {"id":"955f38fb", "addr":"127.0.0.1:6381", "state":"online", "slots":[[5461,10922]]}
//     ]
//   }
// ============================================================

// NodeInfo — информация о ноде для передачи через gossip.
// Это «лёгкая» версия Node — без мьютексов и больших массивов.
type NodeInfo struct {
	ID         string   `json:"id"`
	Addr       string   `json:"addr"`
	GossipPort int      `json:"gossip_port"`
	State      string   `json:"state"`
	Slots      [][2]int `json:"slots"` // пары [start, end] вместо 16384 bool
}

// GossipMessage — пакет, который ноды отправляют друг другу.
type GossipMessage struct {
	Type   string     `json:"type"`   // "PING" или "PONG"
	Sender NodeInfo   `json:"sender"` // информация об отправителе
	Nodes  []NodeInfo `json:"nodes"`  // сплетни о других нодах
}

// ============================================================
// Конвертеры: Node ↔ NodeInfo
//
// Node — внутренняя структура (16384 bool для слотов).
// NodeInfo — компактная для передачи по сети.
//
// Пример:
//   Node.Slots = [true, true, ...(5461 раз)..., false, false, ...]
//   NodeInfo.Slots = [[0, 5460]]  ← компактно!
// ============================================================

// nodeToInfo конвертирует внутренний Node в компактный NodeInfo.
func nodeToInfo(n *Node) NodeInfo {
	return NodeInfo{
		ID:         n.ID,
		Addr:       n.Addr,
		GossipPort: n.GossipPort,
		State:      n.State.String(),
		Slots:      n.SlotPairs(),
	}
}

// applyNodeInfo обновляет или создаёт ноду из полученной NodeInfo.
// Возвращает true если нода была новой (добавлена).
func (cs *ClusterState) applyNodeInfo(info NodeInfo) bool {
	cs.mu.Lock()
	defer cs.mu.Unlock()

	// Не обновляем информацию о себе от чужих сплетен
	if info.ID == cs.Self.ID {
		return false
	}

	// Игнорируем ноды с нашим собственным адресом, но другим ID
	// (это фантомные tmp-ноды от CLUSTER MEET)
	if info.Addr == cs.Self.Addr {
		return false
	}

	node, exists := cs.Nodes[info.ID]
	if !exists {
		// Новая нода! Добавляем.
		node = NewNode(info.ID, info.Addr, info.GossipPort)
		cs.Nodes[info.ID] = node
		log.Printf("[gossip] Discovered new node: %s (%s)", info.ID, info.Addr)
	}

	// Обновляем адрес и порт (могли измениться)
	node.Addr = info.Addr
	node.GossipPort = info.GossipPort

	// Обновляем слоты
	for i := range node.Slots {
		node.Slots[i] = false
	}
	for _, pair := range info.Slots {
		for i := pair[0]; i <= pair[1]; i++ {
			node.Slots[i] = true
		}
	}

	// Обновляем SlotTable (без лока, мы уже под mu.Lock)
	for slot := 0; slot < TotalSlots; slot++ {
		if node.Slots[slot] {
			cs.SlotTable[slot] = node
		}
	}

	return !exists
}

// ============================================================
// GOSSIP SERVER
//
// Слушает на gossip-порту (например, 6381).
// Принимает PING от других нод, отвечает PONG.
//
// Алгоритм:
//   1. Accept TCP-соединение
//   2. Читаем JSON → GossipMessage (PING)
//   3. Обрабатываем: обновляем known_nodes из msg.Nodes
//   4. Формируем PONG с нашей информацией + сплетни
//   5. Отправляем PONG → закрываем соединение
// ============================================================

// StartGossip запускает gossip-сервер и тикер.
// Вызывается из main.go после создания кластера.
func (c *Cluster) StartGossip() error {
	// 1. Запускаем TCP-listener на gossip-порту
	addr := fmt.Sprintf(":%d", c.State.Self.GossipPort)
	listener, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("gossip listen: %w", err)
	}

	log.Printf("[gossip] Listening on %s", addr)

	// Сохраняем listener чтобы потом закрыть
	c.gossipListener = listener

	// 2. Горутина: принимаем входящие соединения
	c.wg.Add(1)
	go func() {
		defer c.wg.Done()
		for {
			conn, err := listener.Accept()
			if err != nil {
				// listener закрыт (StopGossip) → выходим
				return
			}
			// Каждое соединение обрабатываем в отдельной горутине
			go c.handleGossipConn(conn)
		}
	}()

	// 3. Горутина: тикер — каждые 2 секунды шлём PING случайной ноде
	c.wg.Add(1)
	go func() {
		defer c.wg.Done()
		c.gossipTicker()
	}()

	// 4. Горутина: проверка сбоев — каждые 5 секунд
	c.wg.Add(1)
	go func() {
		defer c.wg.Done()
		c.failureDetector()
	}()

	return nil
}

// StopGossip останавливает gossip.
func (c *Cluster) StopGossip() {
	// Сигнализируем всем горутинам: стоп
	close(c.stopCh)

	// Закрываем listener → Accept() вернёт ошибку → горутина выйдет
	if c.gossipListener != nil {
		c.gossipListener.Close()
	}

	// Ждём завершения всех горутин
	c.wg.Wait()
	log.Println("[gossip] Stopped")
}

// ============================================================
// handleGossipConn — обработка входящего PING.
//
// Другая нода подключилась к нашему gossip-порту и шлёт PING.
// Мы читаем его, обновляем свои данные и отвечаем PONG.
// ============================================================
func (c *Cluster) handleGossipConn(conn net.Conn) {
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(5 * time.Second))

	// Читаем PING
	decoder := json.NewDecoder(conn)
	var msg GossipMessage
	if err := decoder.Decode(&msg); err != nil {
		return // битый пакет — игнорируем
	}

	// Обновляем информацию об отправителе
	c.State.applyNodeInfo(msg.Sender)

	// Обновляем LastPong отправителя (он жив — мы его слышим)
	c.State.mu.Lock()
	if node, ok := c.State.Nodes[msg.Sender.ID]; ok {
		node.LastPong = time.Now()
		// Если нода была PFAIL/FAIL — восстанавливаем
		if node.State != NodeOnline {
			log.Printf("[gossip] Node %s (%s) is back ONLINE", node.ID, node.Addr)
			node.State = NodeOnline
		}
	}
	c.State.mu.Unlock()

	// Обрабатываем сплетни о других нодах
	for _, nodeInfo := range msg.Nodes {
		c.State.applyNodeInfo(nodeInfo)
	}

	// Формируем PONG
	pong := c.buildMessage("PONG")

	// Отправляем
	encoder := json.NewEncoder(conn)
	encoder.Encode(pong)
}

// ============================================================
// gossipTicker — фоновый тикер.
//
// Каждые 2 секунды:
//  1. Выбираем случайную ноду из known_nodes
//  2. Подключаемся к её gossip-порту
//  3. Отправляем PING
//  4. Читаем PONG
//  5. Обновляем данные
//
// Почему 2 секунды?
//
//	Баланс между скоростью распространения и трафиком.
//	1 сек = быстро, но больше трафика.
//	5 сек = экономно, но медленнее обнаруживает сбои.
//
// ============================================================
func (c *Cluster) gossipTicker() {
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-c.stopCh:
			return
		case <-ticker.C:
			c.pingRandomNode()
		}
	}
}

// pingRandomNode выбирает случайную ноду и шлёт ей PING.
func (c *Cluster) pingRandomNode() {
	// Собираем список нод (кроме себя)
	c.State.mu.RLock()
	var candidates []*Node
	for _, node := range c.State.Nodes {
		if node != c.State.Self {
			candidates = append(candidates, node)
		}
	}
	c.State.mu.RUnlock()

	if len(candidates) == 0 {
		return // мы одни в кластере
	}

	// Выбираем случайную
	target := candidates[rand.Intn(len(candidates))]

	// Подключаемся к gossip-порту
	addr := fmt.Sprintf("%s:%d", extractHost(target.Addr), target.GossipPort)
	conn, err := net.DialTimeout("tcp", addr, 2*time.Second)
	if err != nil {
		// Нода не отвечает — ничего не делаем.
		// failureDetector обнаружит это по LastPong.
		return
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(3 * time.Second))

	// Отправляем PING
	ping := c.buildMessage("PING")
	encoder := json.NewEncoder(conn)
	if err := encoder.Encode(ping); err != nil {
		return
	}

	// Читаем PONG
	decoder := json.NewDecoder(conn)
	var pong GossipMessage
	if err := decoder.Decode(&pong); err != nil {
		return
	}

	// Обновляем информацию об отправителе PONG
	c.State.applyNodeInfo(pong.Sender)

	// Обновляем LastPong
	c.State.mu.Lock()
	if node, ok := c.State.Nodes[pong.Sender.ID]; ok {
		node.LastPong = time.Now()
		if node.State != NodeOnline {
			log.Printf("[gossip] Node %s (%s) is back ONLINE", node.ID, node.Addr)
			node.State = NodeOnline
		}
	}
	c.State.mu.Unlock()

	// Обрабатываем сплетни
	for _, nodeInfo := range pong.Nodes {
		c.State.applyNodeInfo(nodeInfo)
	}
}

// ============================================================
// buildMessage формирует PING или PONG с текущей информацией.
//
// В сообщение включаем:
//  1. Sender — полная информация о нас (слоты, состояние)
//  2. Nodes — информация о ДРУГИХ нодах (сплетни)
//
// Это и есть механизм распространения:
//
//	Node-A знает про Node-C → включает NodeInfo(C) в PING → Node-B
//	Node-B получает → "О, Node-C существует!" → applyNodeInfo
//
// ============================================================
func (c *Cluster) buildMessage(msgType string) GossipMessage {
	c.State.mu.RLock()
	defer c.State.mu.RUnlock()

	msg := GossipMessage{
		Type:   msgType,
		Sender: nodeToInfo(c.State.Self),
	}

	// Добавляем информацию обо всех известных нодах (кроме себя)
	for _, node := range c.State.Nodes {
		if node != c.State.Self {
			msg.Nodes = append(msg.Nodes, nodeToInfo(node))
		}
	}

	return msg
}

// ============================================================
// failureDetector — обнаружение сбоев.
//
// Каждые 5 секунд проверяем LastPong каждой ноды.
//
//	LastPong < 10 сек назад → ONLINE (всё ок)
//	LastPong > 10 сек назад → PFAIL (подозрение)
//	LastPong > 30 сек назад → FAIL (мертва)
//
// Пороги:
//
//	10 сек для PFAIL — 5 тиков по 2 сек без ответа
//	30 сек для FAIL  — 15 тиков, точно мертва
//
// ============================================================
const (
	pfailTimeout = 10 * time.Second
	failTimeout  = 30 * time.Second
)

func (c *Cluster) failureDetector() {
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-c.stopCh:
			return
		case <-ticker.C:
			c.checkNodeHealth()
		}
	}
}

func (c *Cluster) checkNodeHealth() {
	c.State.mu.Lock()
	defer c.State.mu.Unlock()

	now := time.Now()

	for _, node := range c.State.Nodes {
		if node == c.State.Self {
			continue // себя не проверяем
		}

		since := now.Sub(node.LastPong)

		switch {
		case since > failTimeout && node.State != NodeFail:
			log.Printf("[gossip] Node %s (%s) → FAIL (no pong for %v)",
				node.ID, node.Addr, since.Round(time.Second))
			node.State = NodeFail

			// Leader Election: если мастер упал и я его реплика → промоутим
			if c.State.Self.Role == RoleReplica && c.State.Self.MasterID == node.ID {
				c.promoteToMaster(node)
			}

		case since > pfailTimeout && node.State == NodeOnline:
			log.Printf("[gossip] Node %s (%s) → PFAIL (no pong for %v)",
				node.ID, node.Addr, since.Round(time.Second))
			node.State = NodePFail
		}
	}
}

// promoteToMaster — реплика продвигает себя в мастер.
// Вызывается когда мастер в состоянии FAIL.
// ВАЖНО: вызывается под mu.Lock() из checkNodeHealth.
func (c *Cluster) promoteToMaster(deadMaster *Node) {
	log.Printf("[election] Master %s is FAIL — promoting self to master!", deadMaster.ID)

	// 1. Меняем роль
	c.State.Self.Role = RoleMaster
	c.State.Self.MasterID = ""

	// 2. Забираем слоты мёртвого мастера
	for slot := 0; slot < TotalSlots; slot++ {
		if deadMaster.Slots[slot] {
			deadMaster.Slots[slot] = false
			c.State.Self.Slots[slot] = true
			c.State.SlotTable[slot] = c.State.Self
		}
	}

	// 3. Считаем сколько слотов забрали
	count := 0
	for _, v := range c.State.Self.Slots {
		if v {
			count++
		}
	}

	log.Printf("[election] Promoted! Now master with %d slots", count)
}

// extractHost вытаскивает хост из "127.0.0.1:6380" → "127.0.0.1"
func extractHost(addr string) string {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return addr
	}
	return host
}
