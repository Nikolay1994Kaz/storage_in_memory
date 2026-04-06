package cluster

import (
	"fmt"
	"net"
	"strconv"
	"strings"
	"time"

	"kvstore/kvstore/internal/protocol"
)

// clusterSetSlot обрабатывает CLUSTER SETSLOT <slot> <action> [node-id]
func (c *Cluster) clusterSetSlot(args []protocol.Value) protocol.Value {
	if len(args) < 2 {
		return protocol.Value{Typ: '-', Str: "ERR wrong number of arguments for 'cluster setslot'"}
	}

	slotNum, err := strconv.Atoi(args[0].Str)
	if err != nil || slotNum < 0 || slotNum >= TotalSlots {
		return protocol.Value{Typ: '-', Str: "ERR invalid slot number"}
	}
	slot := uint16(slotNum)

	action := strings.ToUpper(args[1].Str)

	switch action {
	case "MIGRATING":
		// "Я отдаю слот 5000 ноде e5f6g7h8"
		if len(args) < 3 {
			return protocol.Value{Typ: '-', Str: "ERR missing node-id for MIGRATING"}
		}
		targetID := args[2].Str

		c.State.mu.Lock()
		if c.State.SlotTable[slot] != c.State.Self {
			c.State.mu.Unlock()
			return protocol.Value{Typ: '-', Str: "ERR I'm not the owner of this slot"}
		}
		if _, ok := c.State.Nodes[targetID]; !ok {
			c.State.mu.Unlock()
			return protocol.Value{Typ: '-', Str: "ERR unknown node " + targetID}
		}
		c.State.Migrating[slot] = targetID
		c.State.mu.Unlock()
		return protocol.Value{Typ: '+', Str: "OK"}

	case "IMPORTING":
		// "Я принимаю слот 5000 от ноды a1b2c3d4"
		if len(args) < 3 {
			return protocol.Value{Typ: '-', Str: "ERR missing node-id for IMPORTING"}
		}
		sourceID := args[2].Str

		c.State.mu.Lock()
		if _, ok := c.State.Nodes[sourceID]; !ok {
			c.State.mu.Unlock()
			return protocol.Value{Typ: '-', Str: "ERR unknown node " + sourceID}
		}
		c.State.Importing[slot] = sourceID
		c.State.mu.Unlock()
		return protocol.Value{Typ: '+', Str: "OK"}

	case "NODE":
		// "Миграция завершена: слот 5000 теперь у ноды e5f6g7h8"
		if len(args) < 3 {
			return protocol.Value{Typ: '-', Str: "ERR missing node-id for NODE"}
		}
		nodeID := args[2].Str

		c.State.mu.Lock()
		node, ok := c.State.Nodes[nodeID]
		if !ok {
			c.State.mu.Unlock()
			return protocol.Value{Typ: '-', Str: "ERR unknown node " + nodeID}
		}

		// 1. Забираем слот у всех
		for _, n := range c.State.Nodes {
			n.Slots[slot] = false
		}
		// 2. Назначаем новому владельцу
		node.Slots[slot] = true
		c.State.SlotTable[slot] = node

		// 3. Очищаем состояние миграции
		delete(c.State.Migrating, slot)
		delete(c.State.Importing, slot)

		c.State.mu.Unlock()
		return protocol.Value{Typ: '+', Str: "OK"}

	default:
		return protocol.Value{Typ: '-', Str: "ERR invalid action: " + action}
	}
}

func (c *Cluster) clusterGetKeysInSlot(args []protocol.Value) protocol.Value {
	if len(args) < 2 {
		return protocol.Value{Typ: '-', Str: "ERR wrong number of arguments for 'cluster getkeysinslot'"}
	}
	slotNum, err := strconv.Atoi(args[0].Str)
	if err != nil || slotNum < 0 || slotNum >= TotalSlots {
		return protocol.Value{Typ: '-', Str: "ERR invalid slot number"}
	}
	count, err := strconv.Atoi(args[1].Str)
	if err != nil || count <= 0 {
		return protocol.Value{Typ: '-', Str: "ERR invalid count"}
	}
	if c.GetKeysInSlotFunc == nil {
		return protocol.Value{Typ: '-', Str: "ERR store not configured for migration"}
	}
	keys := c.GetKeysInSlotFunc(uint16(slotNum), count)
	result := make([]protocol.Value, len(keys))
	for i, key := range keys {
		result[i] = protocol.Value{Typ: '$', Str: key}
	}
	return protocol.Value{Typ: '*', Array: result}

}

// MigrateKey — переносит один ключ на другую ноду:
//  1. Читаем значение из локального Store
//  2. Отправляем SET на целевую ноду по TCP
//  3. Удаляем ключ из локального Store
func (c *Cluster) MigrateKey(host string, port int, key string) protocol.Value {
	if c.MigrateGetFunc == nil || c.MigrateDelFunc == nil || c.MigrateSetRemoteFunc == nil {
		return protocol.Value{Typ: '-', Str: "ERR migration functions not configured"}
	}

	value, ok := c.MigrateGetFunc(key)
	if !ok {
		return protocol.Value{Typ: '-', Str: "ERR key not found: " + key}
	}

	addr := fmt.Sprintf("%s:%d", host, port)
	if err := c.MigrateSetRemoteFunc(addr, key, value); err != nil {
		return protocol.Value{Typ: '-', Str: "ERR migration failed: " + err.Error()}
	}

	c.MigrateDelFunc(key)

	return protocol.Value{Typ: '+', Str: "OK"}
}

// SendSetToNode отправляет SET key value на удалённую ноду через TCP/RESP.
func SendSetToNode(addr, key string, value []byte) error {
	conn, err := net.DialTimeout("tcp", addr, 2*time.Second)
	if err != nil {
		return fmt.Errorf("connect to %s: %w", addr, err)
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(3 * time.Second))

	// Формируем RESP: *3\r\n$3\r\nSET\r\n$<keylen>\r\n<key>\r\n$<vallen>\r\n<value>\r\n
	cmd := fmt.Sprintf("*3\r\n$3\r\nSET\r\n$%d\r\n%s\r\n$%d\r\n%s\r\n",
		len(key), key, len(value), string(value))

	if _, err := conn.Write([]byte(cmd)); err != nil {
		return fmt.Errorf("write to %s: %w", addr, err)
	}

	buf := make([]byte, 64)
	n, err := conn.Read(buf)
	if err != nil {
		return fmt.Errorf("read from %s: %w", addr, err)
	}

	resp := string(buf[:n])
	if !strings.HasPrefix(resp, "+OK") {
		return fmt.Errorf("remote SET failed: %s", strings.TrimSpace(resp))
	}

	return nil
}
