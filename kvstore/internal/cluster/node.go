package cluster

import (
	"fmt"
	"strings"
	"sync"
	"time"
)

// ============================================================
// NodeState — состояние ноды (как мы её видим).
//
// Жизненный цикл:
//
//	ONLINE → (нет ответа 10 сек) → PFAIL → (большинство согласно) → FAIL
//	FAIL → (нода вернулась, отвечает на PING) → ONLINE
//
// ============================================================
type NodeState int

const (
	NodeOnline NodeState = iota // Нода работает, отвечает на PING
	NodePFail                   // Подозрение: одна нода не может до неё достучаться
	NodeFail                    // Факт: большинство нод подтвердили — нода мертва
)

// String — для красивого вывода в логах.
func (s NodeState) String() string {
	switch s {
	case NodeOnline:
		return "online"
	case NodePFail:
		return "pfail"
	case NodeFail:
		return "fail"
	default:
		return "unknown"
	}
}

type SlotMigration struct {
	Slot   uint16
	Target string
	Source string
}

// ============================================================
// Node — одна нода кластера.
//
// Конкретный пример на нашем сценарии:
//
//	node_алматы := &Node{
//	    ID:   "a1b2c3d4",
//	    Addr: "10.0.1.1:6380",
//	    GossipPort: 6381,
//	    Slots: [0, 1, 2, ..., 5460],     (bitmap: какие слоты мои)
//	    State: NodeOnline,
//	}
//
// ============================================================

type NodeRole int

const (
	RoleMaster  NodeRole = iota // 0 — мастер (по умолчанию)
	RoleReplica                 // 1 — реплика
)

func (r NodeRole) String() string {
	switch r {
	case RoleMaster:
		return "master"
	case RoleReplica:
		return "replica"
	default:
		return "unknown"
	}
}

type Node struct {
	ID         string    // Уникальный идентификатор (генерируется при первом запуске)
	Addr       string    // Адрес для клиентов: "10.0.1.1:6380"
	GossipPort int       // Порт для gossip: 6381
	State      NodeState // online / pfail / fail
	Slots      []bool    // Slots[i] = true → эта нода владеет слотом i
	LastPong   time.Time // когда последний раз получили PONG от этой ноды
	Role       NodeRole
	MasterID   string
}

// NewNode создаёт ноду с пустыми слотами.
func NewNode(id, addr string, gossipPort int) *Node {
	return &Node{
		ID:         id,
		Addr:       addr,
		GossipPort: gossipPort,
		State:      NodeOnline,
		Slots:      make([]bool, TotalSlots), // 16384 bool = все false
		LastPong:   time.Now(),
	}
}

// AssignSlots назначает диапазон слотов этой ноде.
//
// Пример:
//
//	node_алматы.AssignSlots(0, 5460)
//	→ Slots[0]=true, Slots[1]=true, ..., Slots[5460]=true
//
// После этого node_алматы «владеет» слотами 0–5460
// и будет обрабатывать ключи, попадающие в эти слоты.
func (n *Node) AssignSlots(start, end int) {
	for i := start; i <= end; i++ {
		n.Slots[i] = true
	}
}

// OwnsSlot проверяет, владеет ли нода данным слотом.
func (n *Node) OwnsSlot(slot uint16) bool {
	return n.Slots[slot]
}

// SlotCount возвращает количество слотов у ноды.
func (n *Node) SlotCount() int {
	count := 0
	for _, owned := range n.Slots {
		if owned {
			count++
		}
	}
	return count
}

// SlotPairs возвращает слоты как пары [start, end].
// Это БАЗОВЫЙ метод — его используют и SlotRanges, и nodeToInfo.
//
// Пример: Slots[0..5460]=true → [][2]int{{0, 5460}}
func (n *Node) SlotPairs() [][2]int {
	var pairs [][2]int
	start := -1

	for i := 0; i < TotalSlots; i++ {
		if n.Slots[i] {
			if start == -1 {
				start = i
			}
		} else {
			if start != -1 {
				pairs = append(pairs, [2]int{start, i - 1})
				start = -1
			}
		}
	}
	if start != -1 {
		pairs = append(pairs, [2]int{start, TotalSlots - 1})
	}

	return pairs
}

// SlotRanges — строковое представление для CLUSTER NODES.

func (n *Node) SlotRanges() string {
	pairs := n.SlotPairs()
	var ranges []string
	for _, p := range pairs {
		if p[0] == p[1] {
			ranges = append(ranges, fmt.Sprintf("%d", p[0]))
		} else {
			ranges = append(ranges, fmt.Sprintf("%d-%d", p[0], p[1]))

		}
	}
	return strings.Join(ranges, ",")
}

// ============================================================
// ClusterState — состояние всего кластера.
//
// Это то, что знает ТЕКУЩАЯ нода (node-A) обо всех остальных.
// У каждой ноды свой ClusterState — они синхронизируются через Gossip.
//
// Конкретный пример (глазами Node-A в Алматы):
//
//	ClusterState{
//	    Self: node_алматы,              ← это я
//	    Nodes: {
//	        "a1b2c3d4": node_алматы,    ← я сам
//	        "e5f6g7h8": node_астана,    ← сосед
//	        "i9j0k1l2": node_караганда, ← сосед
//	    }
//	    SlotTable: [0]=node_алматы, [1]=node_алматы, ...,
//	               [5461]=node_астана, ...,
//	               [10923]=node_караганда, ...
//	}
//
// ============================================================
type ClusterState struct {
	mu        sync.RWMutex
	Self      *Node             // Текущая нода (я)
	Nodes     map[string]*Node  // Все известные ноды (ID → Node)
	SlotTable [TotalSlots]*Node // Для каждого слота — какая нода владеет
	Migrating map[uint16]string // slot → targetNodeID (мы отдаём)
	Importing map[uint16]string // slot → sourceNodeID (мы принимаем)
}

// NewClusterState создаёт состояние кластера для текущей ноды.
func NewClusterState(self *Node) *ClusterState {
	cs := &ClusterState{
		Self:      self,
		Nodes:     make(map[string]*Node),
		Migrating: make(map[uint16]string),
		Importing: make(map[uint16]string),
	}
	cs.Nodes[self.ID] = self

	// Заполняем SlotTable на основе слотов текущей ноды
	cs.RebuildSlotTable()

	return cs
}

// RebuildSlotTable пересчитывает таблицу слот→нода.
// Вызывается после любого изменения слотов у нод.
func (cs *ClusterState) RebuildSlotTable() {
	cs.mu.Lock()
	defer cs.mu.Unlock()

	// Обнуляем таблицу
	for i := range cs.SlotTable {
		cs.SlotTable[i] = nil
	}

	// Проходим по всем нодам, заполняем слоты
	for _, node := range cs.Nodes {
		for slot := 0; slot < TotalSlots; slot++ {
			if node.Slots[slot] {
				cs.SlotTable[slot] = node
			}
		}
	}
}

// LookupSlot находит ноду, которая владеет данным слотом.
// Возвращает nil если слот никому не назначен.
//
// Пример:
//
//	cs.LookupSlot(9425)  → node_астана
//	cs.LookupSlot(2109)  → node_алматы
//	cs.LookupSlot(12048) → node_караганда
func (cs *ClusterState) LookupSlot(slot uint16) *Node {
	cs.mu.RLock()
	defer cs.mu.RUnlock()
	return cs.SlotTable[slot]
}

// IsMySlot проверяет: принадлежит ли слот текущей ноде?
func (cs *ClusterState) IsMySlot(slot uint16) bool {
	cs.mu.RLock()
	defer cs.mu.RUnlock()
	return cs.SlotTable[slot] == cs.Self
}

// AddNode добавляет ноду в кластер (при CLUSTER MEET или gossip).
func (cs *ClusterState) AddNode(node *Node) {
	cs.mu.Lock()
	cs.Nodes[node.ID] = node
	cs.mu.Unlock()

	cs.RebuildSlotTable()
}
