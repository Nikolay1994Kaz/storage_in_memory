package cluster

import (
	"fmt"
	"testing"
	"time"
)

// ─── CRC16 + KeySlot ──────────────────────────────────────
//
// CRC16-CCITT — стандарт Redis для маршрутизации ключей.
// Если наш CRC16 не совпадает с Redis — ключи полетят на чужие ноды.

func TestCRC16_KnownValues(t *testing.T) {
	// Эталоны из Redis documentation / redis-cli CLUSTER KEYSLOT
	tests := []struct {
		key      string
		wantSlot uint16
	}{
		{"foo", 12182},
		{"bar", 5061},
		{"hello", 866},
		{"", 0}, // пустой ключ
	}

	for _, tt := range tests {
		t.Run(tt.key, func(t *testing.T) {
			got := KeySlot(tt.key)
			if got != tt.wantSlot {
				t.Fatalf("KeySlot(%q) = %d, want %d", tt.key, got, tt.wantSlot)
			}
		})
	}
}

// Все слоты в диапазоне 0..16383
func TestKeySlot_Range(t *testing.T) {
	for i := 0; i < 10000; i++ {
		key := fmt.Sprintf("test-key-%d", i)
		slot := KeySlot(key)
		if slot >= TotalSlots {
			t.Fatalf("KeySlot(%q) = %d, out of range [0, %d)", key, slot, TotalSlots)
		}
	}
}

// Распределение: проверяем что ключи не кучкуются в одном слоте.
// 10K ключей должны занять > 3000 уникальных слотов (из 16384).
func TestKeySlot_Distribution(t *testing.T) {
	seen := make(map[uint16]bool)
	for i := 0; i < 10000; i++ {
		slot := KeySlot(fmt.Sprintf("user:%d", i))
		seen[slot] = true
	}
	if len(seen) < 3000 {
		t.Fatalf("poor distribution: only %d unique slots from 10K keys", len(seen))
	}
	t.Logf("distribution: %d unique slots from 10K keys", len(seen))
}

// ─── Node: слоты ──────────────────────────────────────────

func TestNode_AssignSlots(t *testing.T) {
	n := NewNode("test-id", "127.0.0.1:6380", 6381)

	n.AssignSlots(0, 5460)

	if n.SlotCount() != 5461 {
		t.Fatalf("SlotCount = %d, want 5461", n.SlotCount())
	}
	if !n.OwnsSlot(0) || !n.OwnsSlot(5460) {
		t.Fatal("should own slots 0 and 5460")
	}
	if n.OwnsSlot(5461) {
		t.Fatal("should NOT own slot 5461")
	}
}

func TestNode_SlotPairs(t *testing.T) {
	n := NewNode("test", "127.0.0.1:6380", 6381)

	// Два непрерывных диапазона: 0-100, 200-300
	n.AssignSlots(0, 100)
	n.AssignSlots(200, 300)

	pairs := n.SlotPairs()
	if len(pairs) != 2 {
		t.Fatalf("SlotPairs: %d pairs, want 2", len(pairs))
	}
	if pairs[0] != [2]int{0, 100} {
		t.Fatalf("pair[0] = %v, want [0, 100]", pairs[0])
	}
	if pairs[1] != [2]int{200, 300} {
		t.Fatalf("pair[1] = %v, want [200, 300]", pairs[1])
	}
}

func TestNode_SlotRanges(t *testing.T) {
	n := NewNode("test", "127.0.0.1:6380", 6381)
	n.AssignSlots(0, 5460)

	ranges := n.SlotRanges()
	if ranges != "0-5460" {
		t.Fatalf("SlotRanges = %q, want \"0-5460\"", ranges)
	}
}

func TestNode_SlotRanges_Single(t *testing.T) {
	n := NewNode("test", "127.0.0.1:6380", 6381)
	n.Slots[42] = true // одиночный слот

	ranges := n.SlotRanges()
	if ranges != "42" {
		t.Fatalf("single slot: %q, want \"42\"", ranges)
	}
}

// ─── ClusterState ─────────────────────────────────────────

func TestClusterState_ThreeNodes(t *testing.T) {
	// Создаём 3-нодовый кластер (как Алматы-Астана-Караганда)
	nodeA := NewNode("aaa", "10.0.1.1:6380", 6381)
	nodeA.AssignSlots(0, 5460)

	nodeB := NewNode("bbb", "10.0.2.1:6380", 6381)
	nodeB.AssignSlots(5461, 10922)

	nodeC := NewNode("ccc", "10.0.3.1:6380", 6381)
	nodeC.AssignSlots(10923, 16383)

	cs := NewClusterState(nodeA)
	cs.AddNode(nodeB)
	cs.AddNode(nodeC)

	// Все 16384 слота назначены
	for slot := uint16(0); slot < TotalSlots; slot++ {
		owner := cs.LookupSlot(slot)
		if owner == nil {
			t.Fatalf("slot %d has no owner", slot)
		}
	}

	// Проверяем маршрутизацию
	if !cs.IsMySlot(0) {
		t.Fatal("slot 0 should be mine (nodeA)")
	}
	if cs.IsMySlot(5461) {
		t.Fatal("slot 5461 should NOT be mine (nodeB)")
	}

	// LookupSlot возвращает правильных владельцев
	if cs.LookupSlot(0).ID != "aaa" {
		t.Fatal("slot 0 owner should be aaa")
	}
	if cs.LookupSlot(5461).ID != "bbb" {
		t.Fatal("slot 5461 owner should be bbb")
	}
	if cs.LookupSlot(10923).ID != "ccc" {
		t.Fatal("slot 10923 owner should be ccc")
	}
}

// ─── CheckKey (MOVED) ─────────────────────────────────────

func TestCheckKey_MySlot(t *testing.T) {
	c := New("127.0.0.1:6380", 6381)
	c.State.Self.AssignSlots(0, 16383) // владеем всеми слотами
	c.State.RebuildSlotTable()

	// Любой ключ → наш слот → nil (выполняем команду)
	result := c.CheckKey("any-key")
	if result != nil {
		t.Fatalf("expected nil for my slot, got: %+v", result)
	}
}

func TestCheckKey_MOVED(t *testing.T) {
	// Создаём 2 ноды
	c := New("127.0.0.1:6380", 6381)
	c.State.Self.AssignSlots(0, 8191)
	c.State.RebuildSlotTable()

	nodeB := NewNode("bbb", "127.0.0.1:6381", 6382)
	nodeB.AssignSlots(8192, 16383)
	c.State.AddNode(nodeB)

	// Находим ключ, попадающий в слот nodeB
	var foreignKey string
	for i := 0; i < 100000; i++ {
		key := fmt.Sprintf("test-%d", i)
		if KeySlot(key) >= 8192 {
			foreignKey = key
			break
		}
	}

	result := c.CheckKey(foreignKey)
	if result == nil {
		t.Fatal("expected MOVED for foreign slot")
	}
	if result.Typ != '-' {
		t.Fatalf("expected error type '-', got %c", result.Typ)
	}
	// Должно содержать "MOVED" и адрес nodeB
	if !contains(result.Str, "MOVED") || !contains(result.Str, "127.0.0.1:6381") {
		t.Fatalf("wrong MOVED: %q", result.Str)
	}
}

// ─── Gossip: applyNodeInfo ────────────────────────────────

func TestApplyNodeInfo_NewNode(t *testing.T) {
	self := NewNode("aaa", "127.0.0.1:6380", 6381)
	cs := NewClusterState(self)

	info := NodeInfo{
		ID:         "bbb",
		Addr:       "127.0.0.1:6381",
		GossipPort: 6382,
		State:      "online",
		Slots:      [][2]int{{5461, 10922}},
	}

	isNew := cs.applyNodeInfo(info)
	if !isNew {
		t.Fatal("should return true for new node")
	}

	// Нода добавлена
	if _, ok := cs.Nodes["bbb"]; !ok {
		t.Fatal("node bbb not in Nodes map")
	}

	// Слоты установлены
	if !cs.Nodes["bbb"].OwnsSlot(5461) {
		t.Fatal("node bbb should own slot 5461")
	}
}

func TestApplyNodeInfo_IgnoreSelf(t *testing.T) {
	self := NewNode("aaa", "127.0.0.1:6380", 6381)
	cs := NewClusterState(self)

	info := NodeInfo{
		ID:   "aaa", // наш ID
		Addr: "127.0.0.1:6380",
	}

	isNew := cs.applyNodeInfo(info)
	if isNew {
		t.Fatal("should not add self")
	}
}

func TestApplyNodeInfo_UpdateExisting(t *testing.T) {
	self := NewNode("aaa", "127.0.0.1:6380", 6381)
	cs := NewClusterState(self)

	// Первое применение
	cs.applyNodeInfo(NodeInfo{
		ID:         "bbb",
		Addr:       "127.0.0.1:6381",
		GossipPort: 6382,
		Slots:      [][2]int{{0, 100}},
	})

	// Обновление: другие слоты
	cs.applyNodeInfo(NodeInfo{
		ID:         "bbb",
		Addr:       "127.0.0.1:6381",
		GossipPort: 6382,
		Slots:      [][2]int{{200, 300}},
	})

	node := cs.Nodes["bbb"]
	// Старые слоты должны быть сброшены
	if node.OwnsSlot(50) {
		t.Fatal("old slot 50 should be cleared")
	}
	// Новые слоты установлены
	if !node.OwnsSlot(200) {
		t.Fatal("new slot 200 should be set")
	}
}

// ─── Failure Detection ────────────────────────────────────

func TestFailureDetection(t *testing.T) {
	c := New("127.0.0.1:6380", 6381)

	// Добавляем ноду с устаревшим LastPong
	nodeB := NewNode("bbb", "127.0.0.1:6381", 6382)
	nodeB.LastPong = time.Now().Add(-15 * time.Second) // 15 сек назад
	c.State.AddNode(nodeB)

	// checkNodeHealth должен пометить как PFAIL (>10 сек)
	c.checkNodeHealth()

	if c.State.Nodes["bbb"].State != NodePFail {
		t.Fatalf("expected PFAIL, got %s", c.State.Nodes["bbb"].State)
	}

	// Ставим LastPong ещё старше — FAIL (>30 сек)
	c.State.Nodes["bbb"].LastPong = time.Now().Add(-35 * time.Second)
	c.checkNodeHealth()

	if c.State.Nodes["bbb"].State != NodeFail {
		t.Fatalf("expected FAIL, got %s", c.State.Nodes["bbb"].State)
	}
}

// ─── Leader Election (promote) ────────────────────────────

func TestPromoteToMaster(t *testing.T) {
	c := New("127.0.0.1:6381", 6382)
	// Мы — реплика
	c.State.Self.Role = RoleReplica
	c.State.Self.MasterID = "master-1"

	// Мастер с его слотами
	master := NewNode("master-1", "127.0.0.1:6380", 6381)
	master.AssignSlots(0, 5460)
	c.State.AddNode(master)

	// Промоутим
	c.State.mu.Lock()
	c.promoteToMaster(master)
	c.State.mu.Unlock()

	// Проверяем: мы теперь мастер
	if c.State.Self.Role != RoleMaster {
		t.Fatal("should be master after promote")
	}
	if c.State.Self.MasterID != "" {
		t.Fatal("MasterID should be empty after promote")
	}

	// Слоты перешли к нам
	if c.State.Self.SlotCount() != 5461 {
		t.Fatalf("should have 5461 slots, got %d", c.State.Self.SlotCount())
	}

	// У мертвого мастера слотов нет
	if master.SlotCount() != 0 {
		t.Fatalf("dead master should have 0 slots, got %d", master.SlotCount())
	}
}

// ─── nodeToInfo / round-trip ──────────────────────────────

func TestNodeToInfo_Roundtrip(t *testing.T) {
	n := NewNode("abc", "10.0.1.1:6380", 6381)
	n.AssignSlots(0, 5460)

	info := nodeToInfo(n)

	if info.ID != "abc" {
		t.Fatalf("ID: %q", info.ID)
	}
	if info.Addr != "10.0.1.1:6380" {
		t.Fatalf("Addr: %q", info.Addr)
	}
	if len(info.Slots) != 1 || info.Slots[0] != [2]int{0, 5460} {
		t.Fatalf("Slots: %v", info.Slots)
	}
}

// ─── Helper ───────────────────────────────────────────────

func contains(s, substr string) bool {
	return len(s) >= len(substr) && searchString(s, substr)
}

func searchString(s, sub string) bool {
	for i := 0; i <= len(s)-len(sub); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}

func TestGossip_PinPong(t *testing.T) {
	cA := New("127.0.0.1:16380", 16381)
	cA.State.Self.AssignSlots(0, 8191)
	cA.State.RebuildSlotTable()

	cB := New("127.0.0.1:16382", 16383)
	cB.State.Self.AssignSlots(8192, 16383)
	cB.State.RebuildSlotTable()

	if err := cA.StartGossip(); err != nil {
		t.Fatalf("StartGossip A: %v", err)
	}
	if err := cB.StartGossip(); err != nil {
		t.Fatalf("StartGossip B: %v", err)
	}
	defer cA.StopGossip()
	defer cB.StopGossip()

	cA.State.applyNodeInfo(NodeInfo{
		ID:         cB.State.Self.ID,
		Addr:       "127.0.0.1:16382",
		GossipPort: 16383,
		State:      "online",
		Slots:      [][2]int{{8192, 16383}},
	})
	cA.pingRandomNode()
	time.Sleep(200 * time.Millisecond)
	cB.State.mu.RLock()
	_, knowsA := cB.State.Nodes[cA.State.Self.ID]
	cB.State.mu.RUnlock()
	if !knowsA {
		t.Fatal("Node B shoud discover Node A fater PING/PONG exchange")
	}
	t.Log("gossip: B discovered A throungh PING/PONG")
}

func TestGossip_ThreeNodeDiscovery(t *testing.T) {
	cA := New("127.0.0.1:17380", 17381)
	cB := New("127.0.0.1:17382", 17383)
	cC := New("127.0.0.1:17384", 17385)

	if err := cA.StartGossip(); err != nil {
		t.Fatalf("StartGossip A: %v", err)
	}
	if err := cB.StartGossip(); err != nil {
		t.Fatalf("StartGossip B: %v", err)
	}
	if err := cC.StartGossip(); err != nil {
		t.Fatalf("StartGossip C: %v", err)
	}
	defer cA.StopGossip()
	defer cB.StopGossip()
	defer cC.StopGossip()

	cB.State.applyNodeInfo(NodeInfo{
		ID: cA.State.Self.ID, Addr: " 127.0.0.1:17380", GossipPort: 17381,
	})
	cB.State.applyNodeInfo(NodeInfo{
		ID: cC.State.Self.ID, Addr: "127.0.0.1:17384", GossipPort: 17385,
	})
	cA.State.applyNodeInfo(NodeInfo{
		ID: cB.State.Self.ID, Addr: "127.0.0.1:17382", GossipPort: 17383,
	})
	cA.pingRandomNode()
	time.Sleep(200 * time.Millisecond)
	cA.State.mu.RLock()
	_, knowsC := cA.State.Nodes[cC.State.Self.ID]
	cA.State.mu.RUnlock()
	if !knowsC {
		t.Fatal("Node A shoud discover Node C fater PING/PONG exchange")
	}
	t.Log("gossip: A discovered C throungh PING/PONG")
}
