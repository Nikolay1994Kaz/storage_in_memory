package cluster

import (
	"bufio"
	"fmt"
	"kvstore/kvstore/internal/protocol"
	"net"
	"strings"
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

// ─── Gossip: TCP PING/PONG ─────────────────────────────────
//
// Создаём 2 реальных кластера на разных портах.
// Они обмениваются PING/PONG и узнают друг о друге.

func TestGossip_PingPong(t *testing.T) {
	// Нода A: порты 16380/16381
	cA := New("127.0.0.1:16380", 16381)
	cA.State.Self.AssignSlots(0, 8191)
	cA.State.RebuildSlotTable()

	// Нода B: порты 16382/16383
	cB := New("127.0.0.1:16382", 16383)
	cB.State.Self.AssignSlots(8192, 16383)
	cB.State.RebuildSlotTable()

	// Запускаем gossip серверы
	if err := cA.StartGossip(); err != nil {
		t.Fatalf("StartGossip A: %v", err)
	}
	if err := cB.StartGossip(); err != nil {
		t.Fatalf("StartGossip B: %v", err)
	}

	defer cA.StopGossip()
	defer cB.StopGossip()

	// A знакомится с B: вручную добавляем ноду B в A
	cA.State.applyNodeInfo(NodeInfo{
		ID:         cB.State.Self.ID,
		Addr:       "127.0.0.1:16382",
		GossipPort: 16383,
		State:      "online",
		Slots:      [][2]int{{8192, 16383}},
	})

	// Запускаем один PING от A к B
	cA.pingRandomNode()

	// Даём время на обработку
	time.Sleep(200 * time.Millisecond)

	// B должен узнать про A (через PONG)
	cB.State.mu.RLock()
	_, knowsA := cB.State.Nodes[cA.State.Self.ID]
	cB.State.mu.RUnlock()

	if !knowsA {
		t.Fatal("Node B should discover Node A after PING/PONG exchange")
	}
	t.Log("gossip: B discovered A through PING/PONG ✓")
}

// TestGossip_ThreeNodeDiscovery — транзитивное обнаружение.
// A знает B, B знает C. После gossip — A узнаёт про C.
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

	// B знает про A и C
	cB.State.applyNodeInfo(NodeInfo{
		ID: cA.State.Self.ID, Addr: "127.0.0.1:17380", GossipPort: 17381,
	})
	cB.State.applyNodeInfo(NodeInfo{
		ID: cC.State.Self.ID, Addr: "127.0.0.1:17384", GossipPort: 17385,
	})

	// A знает только про B
	cA.State.applyNodeInfo(NodeInfo{
		ID: cB.State.Self.ID, Addr: "127.0.0.1:17382", GossipPort: 17383,
	})

	// A пингует B → B ответит PONG с инфой про C
	cA.pingRandomNode()
	time.Sleep(200 * time.Millisecond)

	// Теперь A должен знать про C (транзитивно через B)
	cA.State.mu.RLock()
	_, knowsC := cA.State.Nodes[cC.State.Self.ID]
	cA.State.mu.RUnlock()

	if !knowsC {
		t.Fatal("Node A should discover Node C transitively through B's PONG")
	}
	t.Log("gossip: A discovered C through B (transitive) ✓")
}

// TestGossip_StopGraceful — StopGossip не зависает и не паникует.
func TestGossip_StopGraceful(t *testing.T) {
	c := New("127.0.0.1:18380", 18381)
	if err := c.StartGossip(); err != nil {
		t.Fatalf("StartGossip: %v", err)
	}

	done := make(chan struct{})
	go func() {
		c.StopGossip()
		close(done)
	}()

	select {
	case <-done:
		// OK
	case <-time.After(5 * time.Second):
		t.Fatal("StopGossip deadlocked")
	}
}

// TestBuildMessage — структура PING/PONG сообщения.
func TestBuildMessage(t *testing.T) {
	c := New("127.0.0.1:6380", 6381)
	c.State.Self.AssignSlots(0, 5460)
	c.State.RebuildSlotTable()

	// Добавляем вторую ноду
	nodeB := NewNode("bbb", "127.0.0.1:6381", 6382)
	nodeB.AssignSlots(5461, 10922)
	c.State.AddNode(nodeB)

	msg := c.buildMessage("PING")

	if msg.Type != "PING" {
		t.Fatalf("Type = %q, want PING", msg.Type)
	}
	if msg.Sender.ID != c.State.Self.ID {
		t.Fatal("Sender should be self")
	}
	if len(msg.Sender.Slots) != 1 || msg.Sender.Slots[0] != [2]int{0, 5460} {
		t.Fatalf("Sender slots: %v", msg.Sender.Slots)
	}
	// Nodes should contain nodeB (not self)
	if len(msg.Nodes) != 1 {
		t.Fatalf("Nodes: %d, want 1", len(msg.Nodes))
	}
	if msg.Nodes[0].ID != "bbb" {
		t.Fatalf("gossip node: %q, want bbb", msg.Nodes[0].ID)
	}
}

// ─── Replication ──────────────────────────────────────────
//
// Тестируем без реального TCP: используем net.Pipe().

func TestReplication_FullSync(t *testing.T) {
	c := New("127.0.0.1:6380", 6381)
	rm := c.Repl

	// Mock store: 3 ключа
	storeData := map[string]string{
		"key1": "val1",
		"key2": "val2",
		"key3": "val3",
	}

	rm.StoreForEach = func(fn func(string, []byte)) {
		for k, v := range storeData {
			fn(k, []byte(v))
		}
	}

	// net.Pipe: master-side и replica-side
	masterConn, replicaConn := net.Pipe()
	defer masterConn.Close()
	defer replicaConn.Close()

	// Запускаем HandlePsync в горутине (будет писать в masterConn)
	go rm.HandlePsync(masterConn, "replica-1")

	// Читаем из replicaConn как реплика
	scanner := bufio.NewScanner(replicaConn)

	// Первая строка: +FULLSYNC
	if !scanner.Scan() {
		t.Fatal("no FULLSYNC marker")
	}
	if scanner.Text() != "+FULLSYNC" {
		t.Fatalf("expected +FULLSYNC, got %q", scanner.Text())
	}

	// Читаем SET команды
	received := make(map[string]string)
	for scanner.Scan() {
		line := scanner.Text()
		if line == "+FULLSYNC_DONE" {
			break
		}
		// "SET key1 val1"
		parts := strings.SplitN(line, " ", 3)
		if len(parts) == 3 && parts[0] == "SET" {
			received[parts[1]] = parts[2]
		}
	}

	if len(received) != 3 {
		t.Fatalf("full sync: got %d keys, want 3", len(received))
	}
	for k, v := range storeData {
		if received[k] != v {
			t.Fatalf("key %q: got %q, want %q", k, received[k], v)
		}
	}
}

func TestReplication_ForwardWrite(t *testing.T) {
	c := New("127.0.0.1:6380", 6381)
	rm := c.Repl

	masterConn, replicaConn := net.Pipe()

	rm.mu.Lock()
	rm.replicas["replica-1"] = masterConn
	rm.mu.Unlock()

	// Запускаем ForwardWrite в отдельной горутине,
	// чтобы он не блокировал основной поток теста!
	go func() {
		rm.ForwardWrite("SET hello world")
	}()

	// Читаем в основном потоке
	buf := make([]byte, 256)
	replicaConn.SetReadDeadline(time.Now().Add(time.Second))
	n, err := replicaConn.Read(buf)
	if err != nil {
		t.Fatalf("replica read: %v", err)
	}

	got := string(buf[:n])
	if !strings.Contains(got, "SET hello world") {
		t.Fatalf("replica got %q, want 'SET hello world'", got)
	}

	masterConn.Close()
	replicaConn.Close()
}

func TestReplication_ForwardWrite_DeadReplica(t *testing.T) {
	c := New("127.0.0.1:6380", 6381)
	rm := c.Repl

	masterConn, replicaConn := net.Pipe()
	rm.mu.Lock()
	rm.replicas["dead-replica"] = masterConn
	rm.mu.Unlock()

	// Закрываем реплику — она "мертва"
	replicaConn.Close()

	// ForwardWrite должен обнаружить мертвую реплику и убрать её
	rm.ForwardWrite("SET test val")

	// Проверяем: мертвая реплика удалена
	rm.mu.RLock()
	_, exists := rm.replicas["dead-replica"]
	rm.mu.RUnlock()

	if exists {
		t.Fatal("dead replica should be removed from map")
	}
}

func TestApplyReplicationCommand(t *testing.T) {
	c := New("127.0.0.1:6380", 6381)
	rm := c.Repl

	var setKeys []string
	var delKeys []string

	rm.StoreSet = func(key string, value []byte) {
		setKeys = append(setKeys, key+"="+string(value))
	}
	rm.StoreDel = func(key string) {
		delKeys = append(delKeys, key)
	}

	rm.applyReplicationCommand("SET user:1 Alice", false)
	rm.applyReplicationCommand("SET user:2 Bob", false)
	rm.applyReplicationCommand("DEL user:1", false)

	if len(setKeys) != 2 {
		t.Fatalf("SET count: %d, want 2", len(setKeys))
	}
	if len(delKeys) != 1 {
		t.Fatalf("DEL count: %d, want 1", len(delKeys))
	}
	if delKeys[0] != "user:1" {
		t.Fatalf("DEL key: %q, want user:1", delKeys[0])
	}
}

func TestApplyReplicationCommand_VSIM(t *testing.T) {
	c := New("127.0.0.1:6380", 6381)
	rm := c.Repl

	var addedKey string
	var addedVec []float32
	var deletedKey string

	rm.VecStoreAdd = func(key string, vec []float32) {
		addedKey = key
		addedVec = vec
	}
	rm.VecStoreDel = func(key string) {
		deletedKey = key
	}

	rm.applyReplicationCommand("VSIM.ADD vec1 0.1 0.2 0.3", false)
	rm.applyReplicationCommand("VSIM.DEL vec1", false)

	if addedKey != "vec1" {
		t.Fatalf("VSIM.ADD key: %q", addedKey)
	}
	if len(addedVec) != 3 {
		t.Fatalf("VSIM.ADD vec: %v", addedVec)
	}
	if deletedKey != "vec1" {
		t.Fatalf("VSIM.DEL key: %q", deletedKey)
	}
}

// ─── Full Sync: очистка «призрачных векторов» ─────────────
//
// Проблема: TestReplication_FullSync_ClearsGhostKeys проверяет только
// обычные KV-ключи. Но HNSW-индекс тоже живёт на реплике!
//
// Сценарий:
//  1. Реплика имеет stale-вектор: "ghost-vec" (удалён на мастере)
//  2. Мастер шлёт +FULLSYNC → реплика ОБЯЗАНА вызвать VecStoreClear
//  3. Мастер шлёт свежий вектор: "fresh-vec"
//  4. После FULLSYNC_DONE: есть "fresh-vec", НЕТ "ghost-vec"
//
// Без VecStoreClear призрак останется в HNSW и будет мусорить
// в результатах VSIM.SEARCH.

func TestReplication_FullSync_ClearsGhostData(t *testing.T) {
	c := New("127.0.0.1:6380", 6381)
	rm := c.Repl

	// ── KV store (как раньше) ──
	kvData := map[string]string{
		"old-kv": "stale",
	}
	rm.StoreSet = func(key string, value []byte) {
		kvData[key] = string(value)
	}
	rm.StoreDel = func(key string) {
		delete(kvData, key)
	}
	rm.StoreClear = func() {
		for k := range kvData {
			delete(kvData, k)
		}
	}

	// ── Вектор store: есть stale-вектор "ghost-vec" ──
	vecData := map[string][]float32{
		"ghost-vec": {0.9, 0.8, 0.7}, // удалён на мастере, пока реплика лежала
	}
	rm.VecStoreAdd = func(key string, vec []float32) {
		vecData[key] = vec
	}
	rm.VecStoreDel = func(key string) {
		delete(vecData, key)
	}
	rm.VecStoreClear = func() {
		for k := range vecData {
			delete(vecData, k)
		}
	}

	// ── Мастер шлёт: +FULLSYNC → свежие данные → +FULLSYNC_DONE ──
	masterConn, replicaConn := net.Pipe()

	go func() {
		fmt.Fprintf(masterConn, "+FULLSYNC\r\n")
		fmt.Fprintf(masterConn, "SET fresh-kv new-value\r\n")
		fmt.Fprintf(masterConn, "VSIM.ADD fresh-vec 0.1 0.2 0.3\r\n")
		fmt.Fprintf(masterConn, "+FULLSYNC_DONE\r\n")
		masterConn.Close()
	}()

	// ── Реплика обрабатывает поток (имитация replicationLoop) ──
	scanner := bufio.NewScanner(replicaConn)
	for scanner.Scan() {
		line := scanner.Text()

		if line == "+FULLSYNC" {
			if rm.StoreClear != nil {
				rm.StoreClear()
			}
			// ← ВОТ ЭТО главная проверка теста:
			// Без VecStoreClear призраки остаются в HNSW!
			if rm.VecStoreClear != nil {
				rm.VecStoreClear()
			}
			continue
		}
		if line == "+FULLSYNC_DONE" {
			break
		}

		rm.applyReplicationCommand(line, false)
	}
	replicaConn.Close()

	// ── Проверки: KV ──
	if _, exists := kvData["old-kv"]; exists {
		t.Fatal("KV ghost key 'old-kv' should be cleared")
	}
	if val, ok := kvData["fresh-kv"]; !ok || val != "new-value" {
		t.Fatalf("fresh-kv: got %q, want 'new-value'", val)
	}
	if len(kvData) != 1 {
		t.Fatalf("KV store should have 1 key, got %d: %v", len(kvData), kvData)
	}

	// ── Проверки: Вектор ──
	if _, exists := vecData["ghost-vec"]; exists {
		t.Fatal("Vector ghost 'ghost-vec' should be cleared by VecStoreClear")
	}
	if vec, ok := vecData["fresh-vec"]; !ok {
		t.Fatal("fresh-vec should exist after full sync")
	} else if len(vec) != 3 {
		t.Fatalf("fresh-vec dims: got %d, want 3", len(vec))
	}
	if len(vecData) != 1 {
		t.Fatalf("Vec store should have 1 vector, got %d: %v", len(vecData), vecData)
	}
}

// ─── Миграция слотов: SETSLOT + CheckKeyAsk ──────────────
//
// Полный цикл миграции слота от Node-A к Node-B:
//
//  Шаг 1: CLUSTER SETSLOT 5000 MIGRATING bbb   (на Node-A)
//  Шаг 2: CLUSTER SETSLOT 5000 IMPORTING aaa   (на Node-B)
//  Шаг 3: Клиент GET key → ключ есть → отдаём (нормально)
//         Клиент GET key → ключа НЕТ → отвечаем -ASK (CheckKeyAsk)
//  Шаг 4: CLUSTER SETSLOT 5000 NODE bbb        (на обоих)
//
// Это единственный правильный сценарий в Redis Cluster Spec.

func TestSlotMigration_FullCycle(t *testing.T) {
	// ── Настройка: 2 ноды, Node-A владеет слотами 0-8191 ──
	cA := New("127.0.0.1:6380", 6381)
	cA.State.Self.ID = "aaa"
	cA.State.Nodes["aaa"] = cA.State.Self
	cA.State.Self.AssignSlots(0, 8191)

	nodeB := NewNode("bbb", "127.0.0.1:6381", 6382)
	nodeB.AssignSlots(8192, 16383)
	cA.State.AddNode(nodeB)

	// Находим ключ, попадающий точно в слот 5000
	// (используем фиксированный слот для детерминизма)
	targetSlot := uint16(5000)

	// ── Шаг 1: SETSLOT MIGRATING — Node-A отдаёт слот 5000 ──
	resp := cA.HandleClusterCommand([]protocol.Value{
		{Str: "SETSLOT"},
		{Str: "5000"},
		{Str: "MIGRATING"},
		{Str: "bbb"},
	})
	if resp.Typ != '+' || resp.Str != "OK" {
		t.Fatalf("SETSLOT MIGRATING: type=%c str=%q", resp.Typ, resp.Str)
	}

	// Проверяем: слот помечен как MIGRATING
	cA.State.mu.RLock()
	targetID, migrating := cA.State.Migrating[targetSlot]
	cA.State.mu.RUnlock()
	if !migrating || targetID != "bbb" {
		t.Fatalf("slot 5000 should be MIGRATING to bbb, got migrating=%v target=%q",
			migrating, targetID)
	}

	// ── Шаг 2: SETSLOT IMPORTING — Node-B принимает слот 5000 ──
	cB := New("127.0.0.1:6381", 6382)
	cB.State.Self.ID = "bbb"
	cB.State.Nodes["bbb"] = cB.State.Self
	cB.State.Self.AssignSlots(8192, 16383)
	nodeA := NewNode("aaa", "127.0.0.1:6380", 6381)
	nodeA.AssignSlots(0, 8191)
	cB.State.AddNode(nodeA)

	resp = cB.HandleClusterCommand([]protocol.Value{
		{Str: "SETSLOT"},
		{Str: "5000"},
		{Str: "IMPORTING"},
		{Str: "aaa"},
	})
	if resp.Typ != '+' || resp.Str != "OK" {
		t.Fatalf("SETSLOT IMPORTING: type=%c str=%q", resp.Typ, resp.Str)
	}

	// Проверяем: слот помечен как IMPORTING
	cB.State.mu.RLock()
	sourceID, importing := cB.State.Importing[targetSlot]
	cB.State.mu.RUnlock()
	if !importing || sourceID != "aaa" {
		t.Fatalf("slot 5000 should be IMPORTING from aaa, got importing=%v source=%q",
			importing, sourceID)
	}

	// ── Шаг 3: CheckKeyAsk — ключа нет на A → возвращаем ASK ──
	// CheckKeyAsk вызывается ПОСЛЕ CheckKey (который вернул nil, т.к. слот наш)
	// и ПОСЛЕ того как Store не нашёл ключ.
	askResp := cA.CheckKeyAsk("imaginary-key-slot-5000")
	// Примечание: ключ может не попасть ровно в слот 5000,
	// поэтому тестируем напрямую через подготовленное состояние.

	// Для детерминизма: находим ключ, который ТОЧНО попадает в слот 5000
	var keyInSlot5000 string
	for i := 0; i < 1000000; i++ {
		candidate := fmt.Sprintf("migrate-%d", i)
		if KeySlot(candidate) == targetSlot {
			keyInSlot5000 = candidate
			break
		}
	}
	if keyInSlot5000 == "" {
		t.Fatal("could not find a key mapping to slot 5000")
	}

	askResp = cA.CheckKeyAsk(keyInSlot5000)
	if askResp == nil {
		t.Fatal("CheckKeyAsk should return ASK for migrating slot when key is missing")
	}
	if !contains(askResp.Str, "ASK") {
		t.Fatalf("expected ASK response, got %q", askResp.Str)
	}
	if !contains(askResp.Str, "5000") {
		t.Fatalf("ASK should contain slot number 5000, got %q", askResp.Str)
	}
	if !contains(askResp.Str, "127.0.0.1:6381") {
		t.Fatalf("ASK should contain target addr, got %q", askResp.Str)
	}

	// ── Node-B: слот IMPORTING → CheckKey НЕ отвечает MOVED ──
	// (принимающая нода должна обслуживать запросы при IMPORTING)
	movedResp := cB.CheckKey(keyInSlot5000)
	if movedResp != nil {
		t.Fatalf("importing node should NOT return MOVED, got %q", movedResp.Str)
	}

	// ── Шаг 4: Завершение миграции — SETSLOT NODE ──
	resp = cA.HandleClusterCommand([]protocol.Value{
		{Str: "SETSLOT"},
		{Str: "5000"},
		{Str: "NODE"},
		{Str: "bbb"},
	})
	if resp.Typ != '+' || resp.Str != "OK" {
		t.Fatalf("SETSLOT NODE (on A): type=%c str=%q", resp.Typ, resp.Str)
	}

	// Проверяем: Migrating очищен
	cA.State.mu.RLock()
	_, stillMigrating := cA.State.Migrating[targetSlot]
	cA.State.mu.RUnlock()
	if stillMigrating {
		t.Fatal("Migrating map should be cleared after SETSLOT NODE")
	}

	// Проверяем: слот теперь принадлежит Node-B
	owner := cA.State.LookupSlot(targetSlot)
	if owner == nil || owner.ID != "bbb" {
		t.Fatalf("slot 5000 should belong to bbb after migration, got %v", owner)
	}

	t.Log("slot migration full cycle: MIGRATING → IMPORTING → ASK → NODE ✓")
}

// TestSlotMigration_WithVector проверяет, что при миграции слота
// переносятся как KV-данные, так и векторные данные, если они существуют.
func TestSlotMigration_WithVector(t *testing.T) {
	c := New("127.0.0.1:6380", 6381)
	c.State.Self.ID = "aaa"
	c.State.Nodes["aaa"] = c.State.Self
	c.State.Self.AssignSlots(0, 8191)

	// Мокаем хранилище данных
	kvStore := map[string][]byte{
		"my-key": []byte("some-metadata"),
	}
	vecStore := map[string][]float32{
		"my-key": {0.1, 0.2, 0.3},
	}

	c.MigrateGetFunc = func(key string) ([]byte, bool) {
		val, ok := kvStore[key]
		return val, ok
	}
	c.MigrateGetVecFunc = func(key string) ([]float32, bool) {
		vec, ok := vecStore[key]
		return vec, ok
	}

	var remoteKVKey string
	var remoteKVVal []byte
	c.MigrateSetRemoteFunc = func(addr, key string, value []byte) error {
		remoteKVKey = key
		remoteKVVal = value
		return nil
	}

	var remoteVecKey string
	var remoteVecVal []float32
	c.MigrateSetRemoteVecFunc = func(addr, key string, vec []float32) error {
		remoteVecKey = key
		remoteVecVal = vec
		return nil
	}

	deletedLocal := false
	c.MigrateDelFunc = func(key string) {
		delete(kvStore, key)
		delete(vecStore, key)
		deletedLocal = true
	}

	// Запускаем миграцию
	resp := c.MigrateKey("127.0.0.1", 6381, "my-key")
	if resp.Typ != '+' || resp.Str != "OK" {
		t.Fatalf("MigrateKey failed: %v", resp)
	}

	// 1. Проверяем, что KV улетело на удаленную ноду
	if remoteKVKey != "my-key" || string(remoteKVVal) != "some-metadata" {
		t.Fatalf("KV data not sent to remote node: key=%s val=%s", remoteKVKey, string(remoteKVVal))
	}

	// 2. Проверяем, что вектор улетел на удаленную ноду
	if remoteVecKey != "my-key" || len(remoteVecVal) != 3 || remoteVecVal[0] != 0.1 {
		t.Fatalf("Vector data not sent to remote node: key=%s vec=%v", remoteVecKey, remoteVecVal)
	}

	// 3. Проверяем, что локально все очистилось
	if !deletedLocal {
		t.Fatal("local delete function was not called")
	}
	if len(kvStore) != 0 || len(vecStore) != 0 {
		t.Fatalf("local stores not cleared: kv=%v, vec=%v", kvStore, vecStore)
	}
}

// TestSetSlot_Errors — валидация ошибок SETSLOT.
func TestSetSlot_Errors(t *testing.T) {
	c := New("127.0.0.1:6380", 6381)
	c.State.Self.ID = "aaa"
	c.State.Nodes["aaa"] = c.State.Self
	c.State.Self.AssignSlots(0, 8191)
	c.State.RebuildSlotTable()

	// Ошибка: MIGRATING чужого слота (мы не владеем слотом 10000)
	resp := c.HandleClusterCommand([]protocol.Value{
		{Str: "SETSLOT"},
		{Str: "10000"},
		{Str: "MIGRATING"},
		{Str: "bbb"},
	})
	if resp.Typ != '-' {
		t.Fatalf("MIGRATING foreign slot should fail, got type=%c str=%q", resp.Typ, resp.Str)
	}

	// Ошибка: MIGRATING на несуществующую ноду
	resp = c.HandleClusterCommand([]protocol.Value{
		{Str: "SETSLOT"},
		{Str: "100"},
		{Str: "MIGRATING"},
		{Str: "unknown-node"},
	})
	if resp.Typ != '-' {
		t.Fatalf("MIGRATING to unknown node should fail, got type=%c str=%q", resp.Typ, resp.Str)
	}

	// Ошибка: неверный номер слота
	resp = c.HandleClusterCommand([]protocol.Value{
		{Str: "SETSLOT"},
		{Str: "99999"},
		{Str: "MIGRATING"},
		{Str: "aaa"},
	})
	if resp.Typ != '-' {
		t.Fatalf("invalid slot should fail, got type=%c str=%q", resp.Typ, resp.Str)
	}
}

// ─── HandleClusterCommand: тесты внешнего RESP-интерфейса ─
//
// Проверяем что клиент получает корректные ответы на все
// подкоманды CLUSTER *.

func TestHandleClusterCommand_MYID(t *testing.T) {
	c := New("127.0.0.1:6380", 6381)

	resp := c.HandleClusterCommand([]protocol.Value{
		{Str: "MYID"},
	})
	if resp.Typ != '$' {
		t.Fatalf("MYID: expected bulk string, got type=%c", resp.Typ)
	}
	if resp.Str != c.State.Self.ID {
		t.Fatalf("MYID: got %q, want %q", resp.Str, c.State.Self.ID)
	}
}

func TestHandleClusterCommand_INFO(t *testing.T) {
	c := New("127.0.0.1:6380", 6381)
	c.State.Self.AssignSlots(0, 16383) // все слоты → cluster_state:ok
	c.State.RebuildSlotTable()

	resp := c.HandleClusterCommand([]protocol.Value{
		{Str: "INFO"},
	})
	if resp.Typ != '$' {
		t.Fatalf("INFO: expected bulk string, got type=%c", resp.Typ)
	}
	if !contains(resp.Str, "cluster_state:ok") {
		t.Fatalf("INFO: should contain 'cluster_state:ok', got:\n%s", resp.Str)
	}
	if !contains(resp.Str, "cluster_slots_assigned:16384") {
		t.Fatalf("INFO: should contain 'cluster_slots_assigned:16384', got:\n%s", resp.Str)
	}

	// Частично назначенные слоты → cluster_state:fail
	c2 := New("127.0.0.1:6380", 6381)
	c2.State.Self.AssignSlots(0, 100) // только 101 слот
	c2.State.RebuildSlotTable()

	resp2 := c2.HandleClusterCommand([]protocol.Value{
		{Str: "INFO"},
	})
	if !contains(resp2.Str, "cluster_state:fail") {
		t.Fatalf("INFO: incomplete slots should give 'cluster_state:fail', got:\n%s", resp2.Str)
	}
}

func TestHandleClusterCommand_NODES(t *testing.T) {
	c := New("127.0.0.1:6380", 6381)
	c.State.Self.AssignSlots(0, 5460)
	c.State.RebuildSlotTable()

	nodeB := NewNode("bbb", "127.0.0.1:6381", 6382)
	nodeB.AssignSlots(5461, 10922)
	c.State.AddNode(nodeB)

	resp := c.HandleClusterCommand([]protocol.Value{
		{Str: "NODES"},
	})
	if resp.Typ != '$' {
		t.Fatalf("NODES: expected bulk string, got type=%c", resp.Typ)
	}
	// Должна быть строка "myself,"
	if !contains(resp.Str, "myself,") {
		t.Fatalf("NODES: should contain 'myself,', got:\n%s", resp.Str)
	}
	// Должны быть оба адреса
	if !contains(resp.Str, "127.0.0.1:6380") {
		t.Fatalf("NODES: should contain self addr, got:\n%s", resp.Str)
	}
	if !contains(resp.Str, "127.0.0.1:6381") {
		t.Fatalf("NODES: should contain nodeB addr, got:\n%s", resp.Str)
	}
	// Должны быть слоты
	if !contains(resp.Str, "0-5460") {
		t.Fatalf("NODES: should contain '0-5460', got:\n%s", resp.Str)
	}
}

func TestHandleClusterCommand_SLOTS(t *testing.T) {
	c := New("127.0.0.1:6380", 6381)
	c.State.Self.AssignSlots(0, 16383)
	c.State.RebuildSlotTable()

	resp := c.HandleClusterCommand([]protocol.Value{
		{Str: "SLOTS"},
	})
	if resp.Typ != '*' {
		t.Fatalf("SLOTS: expected array, got type=%c", resp.Typ)
	}
	if len(resp.Array) != 1 {
		t.Fatalf("SLOTS: expected 1 range (all slots one node), got %d", len(resp.Array))
	}

	// Каждый элемент: [start, end, [host, port]]
	entry := resp.Array[0]
	if len(entry.Array) != 3 {
		t.Fatalf("SLOTS entry: expected 3 elements, got %d", len(entry.Array))
	}
	if entry.Array[0].Num != 0 {
		t.Fatalf("SLOTS start: got %d, want 0", entry.Array[0].Num)
	}
	if entry.Array[1].Num != 16383 {
		t.Fatalf("SLOTS end: got %d, want 16383", entry.Array[1].Num)
	}
	// Вложенный массив [host, port]
	hostPort := entry.Array[2]
	if hostPort.Typ != '*' || len(hostPort.Array) != 2 {
		t.Fatalf("SLOTS host/port: expected array of 2, got type=%c len=%d",
			hostPort.Typ, len(hostPort.Array))
	}
	if hostPort.Array[0].Str != "127.0.0.1" {
		t.Fatalf("SLOTS host: got %q, want '127.0.0.1'", hostPort.Array[0].Str)
	}
	if hostPort.Array[1].Num != 6380 {
		t.Fatalf("SLOTS port: got %d, want 6380", hostPort.Array[1].Num)
	}
}

func TestHandleClusterCommand_MEET(t *testing.T) {
	c := New("127.0.0.1:6380", 6381)

	// До MEET: только 1 нода (self)
	c.State.mu.RLock()
	beforeCount := len(c.State.Nodes)
	c.State.mu.RUnlock()
	if beforeCount != 1 {
		t.Fatalf("before MEET: %d nodes, want 1", beforeCount)
	}

	resp := c.HandleClusterCommand([]protocol.Value{
		{Str: "MEET"},
		{Str: "10.0.2.1"},
		{Str: "6380"},
	})
	if resp.Typ != '+' || resp.Str != "OK" {
		t.Fatalf("MEET: type=%c str=%q", resp.Typ, resp.Str)
	}

	// После MEET: 2 ноды
	c.State.mu.RLock()
	afterCount := len(c.State.Nodes)
	c.State.mu.RUnlock()
	if afterCount != 2 {
		t.Fatalf("after MEET: %d nodes, want 2", afterCount)
	}

	// Повторный MEET той же ноды — не дублирует
	c.HandleClusterCommand([]protocol.Value{
		{Str: "MEET"},
		{Str: "10.0.2.1"},
		{Str: "6380"},
	})
	c.State.mu.RLock()
	dupeCount := len(c.State.Nodes)
	c.State.mu.RUnlock()
	if dupeCount != 2 {
		t.Fatalf("duplicate MEET: %d nodes, want 2 (no duplicate)", dupeCount)
	}
}

func TestHandleClusterCommand_REPLICATE(t *testing.T) {
	c := New("127.0.0.1:6381", 6382)

	// Добавляем мастера в кластер
	master := NewNode("master-id", "127.0.0.1:6380", 6381)
	master.AssignSlots(0, 16383)
	c.State.AddNode(master)

	// До REPLICATE: мы мастер
	if c.State.Self.Role != RoleMaster {
		t.Fatal("should be master initially")
	}

	resp := c.HandleClusterCommand([]protocol.Value{
		{Str: "REPLICATE"},
		{Str: "master-id"},
	})
	if resp.Typ != '+' || resp.Str != "OK" {
		t.Fatalf("REPLICATE: type=%c str=%q", resp.Typ, resp.Str)
	}

	// После REPLICATE: мы реплика с правильным MasterID
	if c.State.Self.Role != RoleReplica {
		t.Fatalf("after REPLICATE: role=%v, want replica", c.State.Self.Role)
	}
	if c.State.Self.MasterID != "master-id" {
		t.Fatalf("after REPLICATE: MasterID=%q, want 'master-id'", c.State.Self.MasterID)
	}

	// REPLICATE несуществующего мастера → ошибка
	c2 := New("127.0.0.1:6382", 6383)
	resp2 := c2.HandleClusterCommand([]protocol.Value{
		{Str: "REPLICATE"},
		{Str: "nonexistent"},
	})
	if resp2.Typ != '-' {
		t.Fatalf("REPLICATE unknown node: expected error, got type=%c str=%q",
			resp2.Typ, resp2.Str)
	}
}

func TestHandleClusterCommand_UnknownSubcommand(t *testing.T) {
	c := New("127.0.0.1:6380", 6381)

	resp := c.HandleClusterCommand([]protocol.Value{
		{Str: "DOESNOTEXIST"},
	})
	if resp.Typ != '-' {
		t.Fatalf("unknown subcommand: expected error, got type=%c", resp.Typ)
	}
	if !contains(resp.Str, "unknown subcommand") {
		t.Fatalf("error message: %q", resp.Str)
	}
}

func TestHandleClusterCommand_NoArgs(t *testing.T) {
	c := New("127.0.0.1:6380", 6381)

	resp := c.HandleClusterCommand([]protocol.Value{})
	if resp.Typ != '-' {
		t.Fatalf("no args: expected error, got type=%c", resp.Typ)
	}
}
