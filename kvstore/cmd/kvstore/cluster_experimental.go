//go:build experimental

package main

import (
	"fmt"
	"log/slog"
	"net"
	"strconv"
	"strings"
	"time"

	"kvstore/kvstore/internal/cluster"
	"kvstore/kvstore/internal/protocol"
	"kvstore/kvstore/internal/store"
	"kvstore/kvstore/internal/store/kv"
	"kvstore/kvstore/vector"
)

// clusterAdapter приводит *cluster.Cluster к узкому интерфейсу clusterNode.
// Компилируется только в сборке с тегом `experimental`.
type clusterAdapter struct{ c *cluster.Cluster }

func (a clusterAdapter) CheckKey(key string) *protocol.Value    { return a.c.CheckKey(key) }
func (a clusterAdapter) CheckKeyAsk(key string) *protocol.Value { return a.c.CheckKeyAsk(key) }
func (a clusterAdapter) IsReplica() bool                        { return a.c.State.Self.Role == cluster.RoleReplica }

func (a clusterAdapter) ForwardWrite(cmd string) {
	if a.c.Repl != nil {
		a.c.Repl.ForwardWrite(cmd)
	}
}

func (a clusterAdapter) HandleClusterCommand(args []protocol.Value) protocol.Value {
	return a.c.HandleClusterCommand(args)
}

func (a clusterAdapter) MigrateKey(host string, port int, key string) protocol.Value {
	return a.c.MigrateKey(host, port, key)
}

func (a clusterAdapter) HandlePsync(conn net.Conn, replicaID string) {
	a.c.Repl.HandlePsync(conn, replicaID)
}

func (a clusterAdapter) StopGossip() { a.c.StopGossip() }

// newClusterRouter поднимает кластерный узел и связывает его с локальными KV/vector/TTL.
// Вся обвязка репликации/миграции живёт здесь, под тегом `experimental`.
func newClusterRouter(addr string, gossipPort, slotStart, slotEnd int,
	s *kv.Store, vecStore vector.VectorIndex, ttl *store.TTLManager) (clusterNode, error) {

	cl := cluster.New(addr, gossipPort)
	cl.State.Self.AssignSlots(slotStart, slotEnd)
	cl.State.RebuildSlotTable()
	slog.Info("cluster mode (experimental)", "node", cl.State.Self.ID, "slotStart", slotStart, "slotEnd", slotEnd)

	cl.GetKeysInSlotFunc = func(slot uint16, count int) []string {
		var keys []string
		s.ForEach(func(key string, _ []byte) {
			if len(keys) < count && cluster.KeySlot(key) == slot {
				keys = append(keys, key)
			}
		})
		return keys
	}
	cl.MigrateGetFunc = func(key string) ([]byte, bool) {
		return s.Get(key)
	}
	cl.MigrateDelFunc = func(key string) {
		s.Del(key)
		vecStore.Delete(key)
		ttl.OnDelete(key)
	}
	cl.MigrateGetVecFunc = func(key string) ([]float32, bool) {
		return vecStore.Get(key)
	}
	cl.MigrateSetRemoteVecFunc = func(addr, key string, vec []float32) error {
		return SendVectorToNode(addr, key, vec)
	}

	cl.Repl.StoreForEach = func(fn func(key string, value []byte)) { s.ForEach(fn) }
	cl.Repl.VecStoreAdd = func(key string, vec []float32) { vecStore.Add(key, vec) }
	cl.Repl.VecStoreDel = func(key string) { vecStore.Delete(key) }
	cl.Repl.VecStoreForEach = func(fn func(key string, vec []float32)) { vecStore.ForEach(fn) }
	cl.Repl.StoreSet = func(key string, value []byte) { s.Set(key, value) }
	cl.Repl.StoreDel = func(key string) { s.Del(key) }
	cl.Repl.StoreClear = func() { s.Clear() }
	cl.Repl.VecStoreClear = func() { vecStore.Clear() }

	if err := cl.StartGossip(); err != nil {
		return nil, fmt.Errorf("start gossip: %w", err)
	}
	return clusterAdapter{cl}, nil
}

// SendVectorToNode отправляет VSIM.ADD key v1 v2 ... vN на удалённую ноду через TCP.
func SendVectorToNode(addr, key string, vec []float32) error {
	conn, err := net.DialTimeout("tcp", addr, 2*time.Second)
	if err != nil {
		return fmt.Errorf("connect to %s: %w", addr, err)
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(3 * time.Second))

	// Формируем массив RESP аргументов для команды VSIM.ADD
	var sb strings.Builder
	// Число аргументов: 2 (VSIM.ADD + key) + len(vec)
	fmt.Fprintf(&sb, "*%d\r\n$8\r\nVSIM.ADD\r\n$%d\r\n%s\r\n", 2+len(vec), len(key), key)
	for _, v := range vec {
		vStr := strconv.FormatFloat(float64(v), 'f', -1, 32)
		fmt.Fprintf(&sb, "$%d\r\n%s\r\n", len(vStr), vStr)
	}

	if _, err := conn.Write([]byte(sb.String())); err != nil {
		return fmt.Errorf("write to %s: %w", addr, err)
	}

	buf := make([]byte, 64)
	n, err := conn.Read(buf)
	if err != nil {
		return fmt.Errorf("read from %s: %w", addr, err)
	}

	resp := string(buf[:n])
	if !strings.HasPrefix(resp, "+OK") {
		return fmt.Errorf("remote VSIM.ADD failed: %s", strings.TrimSpace(resp))
	}

	return nil
}
